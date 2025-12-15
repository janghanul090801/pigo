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

// 패키지 메타데이터 구조체
type PkgMeta struct {
	ImportNames []string `json:"imports"`  // 실제 import 모듈명 (예: cv2)
	Requires    []string `json:"requires"` // 의존성 패키지명 (예: numpy)
}

// 기본적으로 보호할 패키지 (개발 도구 등)
var defaultIgnoreList = map[string]bool{
	"pytest": true, "black": true, "flake8": true, "mypy": true,
	"pylint": true, "ipython": true, "gunicorn": true, "uvicorn": true,
	"wheel": true, "setuptools": true, "pip": true, "tox": true,
	"pre-commit": true, "poetry": true,
}

// --- [Python 메타데이터 조회 스크립트 (의존성 조회 기능 추가)] ---
const pythonMapperScript = `
import sys
import json
import importlib.metadata
import re

def parse_req_name(req_str):
    # "requests (>=2.0)" -> "requests"
    # "email-validator; extra == 'email'" -> "email-validator"
    if not req_str: return ""
    name = req_str.split('(')[0].split(';')[0].split('<')[0].split('>')[0].split('=')[0]
    return name.strip().lower()

def get_package_info(package_names):
    result = {}
    for pkg_raw in package_names:
        # 안전장치: pydantic[email] -> pydantic
        pkg = pkg_raw.split('[')[0].strip()
        
        info = {"imports": [], "requires": []}
        
        try:
            dist = importlib.metadata.distribution(pkg)
            
            # 1. Import Names (top_level.txt)
            if dist.read_text('top_level.txt'):
                top_levels = dist.read_text('top_level.txt').split()
                info["imports"] = [t.strip() for t in top_levels if t.strip()]
            else:
                info["imports"] = [pkg.lower().replace('-', '_')]
            
            # 2. Dependencies (requires.txt / METADATA)
            requires = dist.requires
            if requires:
                deps = []
                for req in requires:
                    # 의존성 이름 파싱
                    dep_name = parse_req_name(req)
                    if dep_name:
                        deps.append(dep_name)
                info["requires"] = deps
                
        except Exception:
            # 패키지 미설치 시 Fallback
            fallback = pkg.lower().replace('-', '_')
            info["imports"] = [fallback, pkg]
            
        result[pkg_raw] = info # Key는 원본 이름(pydantic[email]) 유지

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
		return nil, fmt.Errorf("python script failed: %v", err)
	}
	var result map[string]PkgMeta
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}
	return result, nil
}

var tidyCmd = &cobra.Command{
	Use:   "tidy [path]",
	Short: "Automatically remove unused packages",
	Long:  `Scans python code and uses installed package metadata (including dependencies) to identify unused dependencies.`,
	Run: func(cmd *cobra.Command, args []string) {
		searchPath := "."
		if len(args) > 0 {
			searchPath = args[0]
		}
		absSearchPath, _ := filepath.Abs(searchPath)
		reqPath := filepath.Join(searchPath, "requirements.txt")

		if _, err := os.Stat(reqPath); os.IsNotExist(err) {
			log.Fatalf("requirements.txt not found in %s", searchPath)
		}

		fmt.Println("Reading requirements.txt...")
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

		fmt.Println("Querying python environment for metadata & dependencies...")
		pkgInfoMap, err := fetchPackageInfo(reqPackages)
		if err != nil {
			log.Printf("Warning: Metadata fetch failed. Dependency protection disabled.")
			pkgInfoMap = make(map[string]PkgMeta)
		}

		fmt.Println("Scanning python files for imports...")
		importedSet := make(map[string]bool)

		files := []string{}
		_ = filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
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

		fmt.Println("Building dependency protection list...")
		protectedDeps := make(map[string]bool)

		for _, meta := range pkgInfoMap {
			isDirectlyUsed := false
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

		fmt.Println("Analyzing dependencies...")
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

			if meta, ok := pkgInfoMap[pkgName]; ok {
				for _, importName := range meta.ImportNames {
					if importedSet[importName] || importedSet[getRootModule(importName)] {
						isUsed = true
						break
					}
				}
			} else {
				if importedSet[pkgName] {
					isUsed = true
				}
			}

			if !isUsed {
				if protectedDeps[pkgLower] {
					isUsed = true
				}
			}

			if !isUsed {
				for imported := range importedSet {
					if strings.EqualFold(imported, pkgName) {
						isUsed = true
						break
					}
				}
			}

			if isUsed {
				newLines = append(newLines, line)
			} else {
				fmt.Printf("Removing: %s (Unused)\n", parsePackageName(line))
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
			fmt.Printf("\nDone! Removed %d packages.\n", removedCount)
		} else {
			fmt.Println("\nEverything looks clean.")
		}
	},
}

func init() {
	rootCmd.AddCommand(tidyCmd)
}
