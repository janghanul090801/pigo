package cmd

// TODO: pip 에서도 remove 하기
import (
	"bufio"
	"encoding/json"
	"fmt"
	_const "github.com/janghanul090801/pigo/cmd/const"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/spf13/cobra"
)

// --- [구조체 정의] ---
type ImportItem struct {
	Type   string
	Module string
	Names  []string
}

// 개발용 도구 등은 코드에서 import 하지 않아도 지우면 안 되므로 예외 리스트 정의
var defaultIgnoreList = map[string]bool{
	"pytest": true, "black": true, "flake8": true, "mypy": true,
	"pylint": true, "ipython": true, "gunicorn": true, "uvicorn": true,
	"wheel": true, "setuptools": true, "pip": true, "tox": true,
}

// --- [Python 메타데이터 조회 스크립트] ---
// Go에서 실행할 파이썬 코드입니다. 표준 입력으로 패키지 리스트를 받고, JSON으로 매핑을 반환합니다.
const pythonMapperScript = `
import sys
import json
import importlib.metadata

def get_import_names(package_names):
    mapping = {}
    for pkg in package_names:
        try:
            # 패키지 메타데이터에서 top-level 모듈 이름들을 가져옴
            dist = importlib.metadata.distribution(pkg)
            # top_level.txt가 있는 경우 (대부분의 패키지)
            if dist.read_text('top_level.txt'):
                top_levels = dist.read_text('top_level.txt').split()
                # 윈도우/리눅스 개행 문자 처리 및 공백 제거
                mapping[pkg] = [t.strip() for t in top_levels if t.strip()]
            else:
                # 메타데이터는 있지만 top_level이 명시되지 않은 경우 패키지 이름 그대로 사용 (fallback)
                mapping[pkg] = [pkg.lower().replace('-', '_')]
        except importlib.metadata.PackageNotFoundError:
            # 설치되지 않은 패키지는 추측 (이름 그대로 or 소문자)
            mapping[pkg] = [pkg, pkg.lower().replace('-', '_')]
        except Exception:
            mapping[pkg] = [pkg.lower()]
            
    return mapping

if __name__ == "__main__":
    # Stdin에서 패키지 리스트 읽기 (JSON array strings)
    input_data = sys.stdin.read()
    if not input_data:
        print("{}")
        sys.exit(0)
    
    packages = json.loads(input_data)
    result = get_import_names(packages)
    print(json.dumps(result))
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

// --- [패키지 이름 파싱] ---
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
	return strings.TrimSpace(parts[0])
}

func getRootModule(moduleName string) string {
	parts := strings.Split(moduleName, ".")
	return parts[0]
}

// --- [Python 실행 헬퍼 함수] ---
func fetchPackageMappings(packageNames []string) (map[string][]string, error) {
	// 1. 패키지 리스트를 JSON으로 변환
	inputJSON, err := json.Marshal(packageNames)
	if err != nil {
		return nil, err
	}

	// 2. Python 프로세스 실행
	cmd := exec.Command(_const.PYTHONPATHWINDOW, "-c", pythonMapperScript) // 혹은 "python3"
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	// 3. Stdin으로 데이터 전달
	go func() {
		defer stdin.Close()
		io.WriteString(stdin, string(inputJSON))
	}()

	// 4. Stdout 결과 읽기
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run python script: %v (make sure packages are installed in current env)", err)
	}

	// 5. 결과 파싱
	var result map[string][]string
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}

	return result, nil
}

// --- [Main Logic] ---

var tidyCmd = &cobra.Command{
	Use:   "tidy [path]",
	Short: "Automatically remove unused packages",
	Long:  `Scans python code and uses installed package metadata to accurately identify and remove unused dependencies from requirements.txt.`,
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

		pkgMapping, err := fetchPackageMappings(reqPackages)
		if err != nil {
			log.Printf("Warning: Could not fetch metadata automatically (%v). Fallback to name matching.", err)
			pkgMapping = make(map[string][]string)
		}

		importedSet := make(map[string]bool)

		files := []string{}
		_ = filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && filepath.Ext(path) == ".py" && !strings.Contains(path, ".venv") {
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

		var newLines []string
		var removedCount int

		for _, line := range originalLines {
			pkgName := parsePackageName(line)

			if pkgName == "" || defaultIgnoreList[strings.ToLower(pkgName)] {
				newLines = append(newLines, line)
				continue
			}

			isUsed := false

			if mappedNames, ok := pkgMapping[pkgName]; ok {
				for _, mappedName := range mappedNames {
					if importedSet[mappedName] || importedSet[getRootModule(mappedName)] {
						isUsed = true
						break
					}
				}
			}

			if !isUsed {
				if importedSet[pkgName] {
					isUsed = true
				}
				if !isUsed {
					for imported := range importedSet {
						if strings.EqualFold(imported, pkgName) {
							isUsed = true
							break
						}
					}
				}
			}

			if isUsed {
				newLines = append(newLines, line)
			} else {
				fmt.Printf("Removing: %s (Not imported)\n", pkgName)
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
