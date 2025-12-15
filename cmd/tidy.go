package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	python "github.com/smacker/go-tree-sitter/python"
	"github.com/spf13/cobra"
)

// --- [구조체 정의] ---
type ImportItem struct {
	Type   string
	Module string
	Names  []string
}

type PkgMeta struct {
	ImportNames []string `json:"imports"`
	Requires    []string `json:"requires"`
}

// 코드에 import는 없지만 지워지면 안 되는 개발/배포 도구들
// (이건 어쩔 수 없이 유지해야 합니다. 코드에 안 나오니까요.)
var defaultIgnoreList = map[string]bool{
	"pytest": true, "black": true, "flake8": true, "mypy": true,
	"pylint": true, "ipython": true, "gunicorn": true, "uvicorn": true,
	"wheel": true, "setuptools": true, "pip": true, "tox": true,
	"pre-commit": true, "poetry": true,
}

// --- [강력해진 Python 메타데이터 분석 스크립트] ---
const pythonMapperScript = `
import sys
import json
import importlib.metadata
import os

def parse_req_name(req_str):
    if not req_str: return ""
    name = req_str.split('(')[0].split(';')[0].split('<')[0].split('>')[0].split('=')[0]
    return name.strip().lower()

def get_import_names_from_files(dist):
    """
    top_level.txt가 없을 때, 실제 설치된 파일 경로를 분석하여 import 이름을 추출
    예: python-jose -> site-packages/jose/__init__.py -> 'jose' 추출
    """
    modules = set()
    if not dist.files:
        return []

    for path in dist.files:
        # 경로는 보통 'jose/__init__.py' 또는 'six.py' 형태임
        parts = str(path).split(os.sep)
        
        # 최상위 경로가 .dist-info나 .egg-info면 무시
        if len(parts) > 0:
            top = parts[0]
            if top.endswith('.dist-info') or top.endswith('.egg-info') or top == '__pycache__':
                continue
            
            # .py 파일인 경우 (예: six.py)
            if top.endswith('.py'):
                modules.add(top[:-3])
            # 폴더인 경우 (예: jose/)
            else:
                modules.add(top)
    
    return list(modules)

def get_package_info(package_names):
    result = {}
    for pkg_raw in package_names:
        # 안전장치: pydantic[email] -> pydantic
        pkg = pkg_raw.split('[')[0].strip()
        
        info = {"imports": [], "requires": []}
        try:
            dist = importlib.metadata.distribution(pkg)
            
            # 1. Imports 찾기 (top_level.txt 우선, 없으면 파일 분석)
            if dist.read_text('top_level.txt'):
                top_levels = dist.read_text('top_level.txt').split()
                info["imports"] = [t.strip() for t in top_levels if t.strip()]
            else:
                # top_level.txt가 없으면 설치된 파일 리스트를 뒤진다 (여기가 핵심)
                detected = get_import_names_from_files(dist)
                if detected:
                    info["imports"] = detected
                else:
                    # 최후의 수단: 이름 변환
                    info["imports"] = [pkg.lower().replace('-', '_')]
            
            # 2. Dependencies 찾기 (pydantic -> email-validator 보호용)
            requires = dist.requires
            if requires:
                deps = []
                for req in requires:
                    dep_name = parse_req_name(req)
                    if dep_name:
                        deps.append(dep_name)
                info["requires"] = deps

        except Exception:
            # 패키지 미설치 시 Fallback
            info["imports"] = [pkg.lower().replace('-', '_'), pkg]
            
        result[pkg_raw] = info 
    return result

if __name__ == "__main__":
    input_data = sys.stdin.read()
    if not input_data:
        print("{}")
        sys.exit(0)
    try:
        packages = json.loads(input_data)
        result = get_package_info(packages)
        print(json.dumps(result))
    except Exception:
        print("{}")
`

// --- [Tree-sitter 함수들 (변경 없음)] ---
func extractImports(root *sitter.Node, src []byte) []ImportItem {
	var res []ImportItem
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		switch n.Type() {
		case "import_statement":
			for i := 0; i < int(n.NamedChildCount()); i++ {
				child := n.NamedChild(i)
				moduleName := resolveModuleName(child, src)
				if moduleName != "" {
					res = append(res, ImportItem{Type: "import", Module: moduleName})
				}
			}
		case "import_from_statement":
			modNode := n.ChildByFieldName("module_name")
			module := ""
			if modNode != nil {
				module = modNode.Content(src)
			} else {
				module = "."
			}
			names := []string{}
			for i := 0; i < int(n.NamedChildCount()); i++ {
				child := n.NamedChild(i)
				if (child.Type() == "dotted_name" || child.Type() == "aliased_import") && child != modNode {
					names = append(names, resolveModuleName(child, src))
				}
			}
			if len(names) == 0 {
				namesNode := n.ChildByFieldName("names")
				names = getImportNames(namesNode, src)
			}
			res = append(res, ImportItem{Type: "from", Module: module, Names: names})
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return res
}

func resolveModuleName(n *sitter.Node, src []byte) string {
	if n.Type() == "aliased_import" {
		orig := n.ChildByFieldName("name")
		return orig.Content(src)
	}
	return n.Content(src)
}

func getImportNames(n *sitter.Node, src []byte) []string {
	if n == nil {
		return nil
	}
	var names []string
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		names = append(names, resolveModuleName(c, src))
	}
	return names
}

func isLocalModule(rootPath, moduleName string) bool {
	if strings.HasPrefix(moduleName, ".") {
		return true
	}
	relPath := strings.ReplaceAll(moduleName, ".", string(os.PathSeparator))
	absPath := filepath.Join(rootPath, relPath)
	if _, err := os.Stat(absPath + ".py"); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(absPath, "__init__.py")); err == nil {
		return true
	}
	return false
}

// pydantic[email] -> pydantic
func parsePackageName(line string) string {
	if idx := strings.Index(line, "#"); idx != -1 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	re := regexp.MustCompile(`([<>=~;]+)`)
	parts := re.Split(line, 2)
	pkgName := strings.TrimSpace(parts[0])

	if idx := strings.Index(pkgName, "["); idx != -1 {
		pkgName = strings.TrimSpace(pkgName[:idx])
	}
	return pkgName
}

func getRootModule(moduleName string) string {
	parts := strings.Split(moduleName, ".")
	return parts[0]
}

func fetchPackageInfo(packageNames []string) (map[string]PkgMeta, error) {
	inputJSON, err := json.Marshal(packageNames)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("python", "-c", pythonMapperScript)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	go func() {
		defer stdin.Close()
		io.WriteString(stdin, string(inputJSON))
	}()
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("python script failed")
	}
	var result map[string]PkgMeta
	json.Unmarshal(output, &result)
	return result, nil
}

// --- [Main Logic] ---

var tidyCmd = &cobra.Command{
	Use:   "tidy [path]",
	Short: "Automatically remove unused packages",
	Long:  `Analyzes dependencies by inspecting installed package files to accurately map PyPI names to import names without hardcoded lists.`,
	Run: func(cmd *cobra.Command, args []string) {
		searchPath := "."
		if len(args) > 0 {
			searchPath = args[0]
		}
		absSearchPath, _ := filepath.Abs(searchPath)
		reqPath := filepath.Join(searchPath, "requirements.txt")

		if _, err := os.Stat(reqPath); os.IsNotExist(err) {
			log.Fatalf("requirements.txt not found")
		}

		fmt.Println("📜 Reading requirements.txt...")
		reqFile, err := os.Open(reqPath)
		if err != nil {
			log.Fatal(err)
		}

		var originalLines []string
		var reqPackages []string
		scanner := bufio.NewScanner(reqFile)
		for scanner.Scan() {
			line := scanner.Text()
			originalLines = append(originalLines, line)
			pkgName := parsePackageName(line)
			if pkgName != "" {
				reqPackages = append(reqPackages, pkgName)
			}
		}
		reqFile.Close()

		fmt.Println("🤖 Analyzing python environment (Smart Mode)...")
		pkgInfoMap, _ := fetchPackageInfo(reqPackages)

		fmt.Println("🔍 Scanning code imports...")
		importedSet := make(map[string]bool)

		files := []string{}
		filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && filepath.Ext(path) == ".py" {
				files = append(files, path)
			}
			return nil
		})

		parser := sitter.NewParser()
		parser.SetLanguage(python.GetLanguage())

		for _, filename := range files {
			func() {
				f, err := os.Open(filename)
				if err != nil {
					return
				}
				defer f.Close()
				src, _ := io.ReadAll(f)
				tree := parser.Parse(nil, src)
				imports := extractImports(tree.RootNode(), src)
				for _, imp := range imports {
					if !isLocalModule(absSearchPath, imp.Module) {
						importedSet[getRootModule(imp.Module)] = true
						importedSet[imp.Module] = true
					}
				}
			}()
		}

		// 의존성 보호 목록 생성
		protectedDeps := make(map[string]bool)
		for _, meta := range pkgInfoMap {
			isDirectlyUsed := false

			// 메타데이터(설치된 파일 분석 결과)로 확인
			for _, importName := range meta.ImportNames {
				if importedSet[importName] || importedSet[getRootModule(importName)] {
					isDirectlyUsed = true
					break
				}
			}

			if isDirectlyUsed {
				for _, dep := range meta.Requires {
					protectedDeps[strings.ToLower(dep)] = true
				}
			}
		}

		fmt.Println("🧹 Cleaning up...")
		var newLines []string
		var removedCount int

		for _, line := range originalLines {
			pkgName := parsePackageName(line)
			pkgLower := strings.ToLower(pkgName)

			if pkgName == "" || defaultIgnoreList[pkgLower] {
				newLines = append(newLines, line)
				continue
			}

			isUsed := false

			// 1. 메타데이터 매핑 확인
			if meta, ok := pkgInfoMap[line]; ok { // 키값 주의
				for _, importName := range meta.ImportNames {
					if importedSet[importName] || importedSet[getRootModule(importName)] {
						isUsed = true
						break
					}
				}
			}
			// line 키로 못 찾으면 pkgName 키로 재시도
			if !isUsed {
				if meta, ok := pkgInfoMap[pkgName]; ok {
					for _, importName := range meta.ImportNames {
						if importedSet[importName] || importedSet[getRootModule(importName)] {
							isUsed = true
							break
						}
					}
				}
			}

			// 2. 단순 이름 일치 (Fallback)
			if !isUsed {
				if importedSet[pkgName] {
					isUsed = true
				}
				if !isUsed {
					for imp := range importedSet {
						if strings.EqualFold(imp, pkgName) {
							isUsed = true
							break
						}
					}
				}
			}

			// 3. 의존성 보호 (pydantic -> email-validator 등)
			if !isUsed && protectedDeps[pkgLower] {
				isUsed = true
			}

			if isUsed {
				newLines = append(newLines, line)
			} else {
				fmt.Printf("❌ Removing: %s\n", pkgName)
				removedCount++
			}
		}

		if removedCount > 0 {
			outFile, err := os.Create(reqPath)
			if err != nil {
				log.Fatal(err)
			}
			defer outFile.Close()
			w := bufio.NewWriter(outFile)
			for _, l := range newLines {
				fmt.Fprintln(w, l)
			}
			w.Flush()
			fmt.Printf("\n✅ Removed %d packages.\n", removedCount)
		} else {
			fmt.Println("\n✨ Clean.")
		}
	},
}

func init() {
	rootCmd.AddCommand(tidyCmd)
}
