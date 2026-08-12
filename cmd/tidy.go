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

	"github.com/janghanul090801/pigo/utills"
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
var defaultIgnoreList = map[string]bool{
	"pytest":     true,
	"black":      true,
	"flake8":     true,
	"mypy":       true,
	"pylint":     true,
	"ipython":    true,
	"gunicorn":   true,
	"uvicorn":    true,
	"wheel":      true,
	"setuptools": true,
	"pip":        true,
	"tox":        true,
	"pre-commit": true,
	"poetry":     true,
}

// --- [Python 메타데이터 분석 스크립트] ---

const pythonMapperScript = `
import sys
import json
import importlib.metadata


def parse_req_name(req_str):
    if not req_str:
        return ""

    # extra 조건이 명시된 선택적 의존성은 런타임에 필요 없으므로 제외
    if ';' in req_str:
        condition = req_str.split(';', 1)[1]
        if 'extra' in condition:
            return ""

    name = (
        req_str
        .split('(')[0]
        .split(';')[0]
        .split('<')[0]
        .split('>')[0]
        .split('=')[0]
        .split('[')[0]
    )

    return name.strip().lower()


def get_import_names_from_files(dist):
    """
    top_level.txt가 없을 때 실제 설치된 파일 경로를 분석하여
    import 이름을 추출한다.

    예:
        python-jose
        -> site-packages/jose/__init__.py
        -> jose
    """

    modules = set()

    if not dist.files:
        return []

    for path in dist.files:
        parts = path.parts

        if len(parts) == 0:
            continue

        top = parts[0]

        # 메타데이터 디렉터리 제외
        if (
            top.endswith('.dist-info')
            or top.endswith('.egg-info')
            or top == '__pycache__'
        ):
            continue

        # .py 파일
        if top.endswith('.py'):
            modules.add(top[:-3])

        # package directory
        else:
            modules.add(top)

    return list(modules)


def get_package_info(package_names):
    result = {}

    for pkg_raw in package_names:
        pkg = pkg_raw.split('[')[0].strip()

        info = {
            "imports": [],
            "requires": [],
        }

        try:
            dist = importlib.metadata.distribution(pkg)

            # --------------------------------------------------
            # 1. Import 이름 찾기
            # --------------------------------------------------

            top_level_content = None

            try:
                top_level_content = dist.read_text('top_level.txt')
            except Exception:
                pass

            if top_level_content:
                top_levels = top_level_content.split()

                info["imports"] = [
                    t.strip()
                    for t in top_levels
                    if t.strip()
                ]

            else:
                detected = get_import_names_from_files(dist)

                if detected:
                    info["imports"] = detected
                else:
                    info["imports"] = [
                        pkg.lower().replace('-', '_')
                    ]

            # --------------------------------------------------
            # 2. Dependencies 찾기
            # --------------------------------------------------

            requires = dist.requires

            if requires:
                deps = []

                for req in requires:
                    dep_name = parse_req_name(req)

                    if dep_name:
                        deps.append(dep_name)

                info["requires"] = deps

        except Exception:
            # 패키지가 설치되어 있지 않은 경우 fallback
            info["imports"] = [
                pkg.lower().replace('-', '_'),
                pkg,
            ]

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

// --- [Tree-sitter 함수들] ---

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
					res = append(res, ImportItem{
						Type:   "import",
						Module: moduleName,
					})
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

				if (child.Type() == "dotted_name" ||
					child.Type() == "aliased_import") && child != modNode {
					names = append(
						names,
						resolveModuleName(child, src),
					)
				}
			}

			if len(names) == 0 {
				namesNode := n.ChildByFieldName("names")
				names = getImportNames(namesNode, src)
			}

			res = append(res, ImportItem{
				Type:   "from",
				Module: module,
				Names:  names,
			})
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

		if orig == nil {
			return ""
		}

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

		names = append(
			names,
			resolveModuleName(c, src),
		)
	}

	return names
}

func isLocalModule(
	rootPath,
	currentFilePath,
	moduleName string,
) bool {

	if strings.HasPrefix(moduleName, ".") {
		return true
	}

	relPath := strings.ReplaceAll(
		moduleName,
		".",
		string(os.PathSeparator),
	)

	dirsToSearch := []string{
		filepath.Dir(currentFilePath),
		rootPath,
		filepath.Join(rootPath, "src"),
	}

	for _, dir := range dirsToSearch {
		absPath := filepath.Join(
			dir,
			relPath,
		)

		if _, err := os.Stat(absPath + ".py"); err == nil {
			return true
		}

		if _, err := os.Stat(
			filepath.Join(absPath, "__init__.py"),
		); err == nil {
			return true
		}
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
		pkgName = strings.TrimSpace(
			pkgName[:idx],
		)
	}

	return pkgName
}

func getRootModule(moduleName string) string {
	parts := strings.Split(moduleName, ".")

	return parts[0]
}

func fetchPackageInfo(
	pythonExec string,
	packageNames []string,
) (map[string]PkgMeta, error) {

	inputJSON, err := json.Marshal(packageNames)

	if err != nil {
		return nil, err
	}

	cmd := exec.Command(
		pythonExec,
		"-c",
		pythonMapperScript,
	)

	stdin, err := cmd.StdinPipe()

	if err != nil {
		return nil, err
	}

	go func() {
		defer stdin.Close()

		_, _ = io.WriteString(
			stdin,
			string(inputJSON),
		)
	}()

	output, err := cmd.Output()

	if err != nil {
		return nil, fmt.Errorf(
			"python script failed",
		)
	}

	var result map[string]PkgMeta

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}

	return result, nil
}

// ------------------------------------------------------------
// venv에 없는 requirements 설치
// ------------------------------------------------------------

func installMissingPackages(
	pythonExec string,
	requirementLines []string,
	ignoreList map[string]bool,
) error {

	fmt.Println("\nChecking missing packages in venv...")

	var missingRequirements []string

	for _, line := range requirementLines {
		pkgName := parsePackageName(line)

		if pkgName == "" {
			continue
		}

		pkgLower := strings.ToLower(pkgName)

		// ignore 목록은 설치 검사에서도 제외
		if ignoreList[pkgLower] {
			continue
		}

		// 현재 venv의 Python으로 패키지 설치 여부 확인
		checkCmd := exec.Command(
			pythonExec,
			"-c",
			`
import importlib.metadata
import sys

package_name = sys.argv[1]

try:
    importlib.metadata.version(package_name)
except importlib.metadata.PackageNotFoundError:
    sys.exit(1)
`,
			pkgName,
		)

		if err := checkCmd.Run(); err != nil {
			missingRequirements = append(
				missingRequirements,
				line,
			)
		}
	}

	// --------------------------------------------------------
	// 설치할 패키지가 없음
	// --------------------------------------------------------

	if len(missingRequirements) == 0 {
		fmt.Println("All requirements are already installed.")
		return nil
	}

	// --------------------------------------------------------
	// 설치할 패키지 출력
	// --------------------------------------------------------

	fmt.Println("\nMissing packages:")

	for _, requirement := range missingRequirements {
		fmt.Printf("  - %s\n", requirement)
	}

	fmt.Println("\nInstalling missing packages...")

	// --------------------------------------------------------
	// requirements.txt의 원본 requirement를 그대로 전달
	//
	// 예:
	// fastapi==0.116.1
	// uvicorn>=0.35.0
	// pydantic[email]
	//
	// 버전 조건 및 extras를 유지한다.
	// --------------------------------------------------------

	args := []string{
		"-m",
		"pip",
		"install",
	}

	args = append(
		args,
		missingRequirements...,
	)

	installCmd := exec.Command(
		pythonExec,
		args...,
	)

	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	installCmd.Stdin = os.Stdin

	if err := installCmd.Run(); err != nil {
		return fmt.Errorf(
			"failed to install missing packages: %w",
			err,
		)
	}

	fmt.Printf(
		"\nSuccessfully installed %d missing package(s).\n",
		len(missingRequirements),
	)

	return nil
}

var ignoreFlag []string

var tidyCmd = &cobra.Command{
	Use:   "tidy [path]",
	Short: "Automatically remove unused packages",
	Long: `Analyzes Python dependencies by inspecting installed
package metadata and source-code imports.

After cleaning requirements.txt, packages that are missing
from the current virtual environment are automatically installed.`,
	Run: func(cmd *cobra.Command, args []string) {

		// --------------------------------------------------------
		// 1. 프로젝트 경로
		// --------------------------------------------------------

		searchPath := "."

		if len(args) > 0 {
			searchPath = args[0]
		}

		absSearchPath, err := filepath.Abs(searchPath)

		if err != nil {
			log.Fatalf(
				"failed to resolve path: %v",
				err,
			)
		}

		reqPath := filepath.Join(
			searchPath,
			"requirements.txt",
		)

		if _, err := os.Stat(reqPath); os.IsNotExist(err) {
			log.Fatalf(
				"requirements.txt not found: %s",
				reqPath,
			)
		}

		// --------------------------------------------------------
		// 2. Ignore 목록
		// --------------------------------------------------------

		ignoreList := make(map[string]bool)

		for k, v := range defaultIgnoreList {
			ignoreList[k] = v
		}

		for _, val := range ignoreFlag {
			ignoreList[strings.ToLower(
				strings.TrimSpace(val),
			)] = true
		}

		// --------------------------------------------------------
		// 3. requirements.txt 읽기
		// --------------------------------------------------------

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

			originalLines = append(
				originalLines,
				line,
			)

			pkgName := parsePackageName(line)

			if pkgName != "" {
				reqPackages = append(
					reqPackages,
					pkgName,
				)
			}
		}

		if err := scanner.Err(); err != nil {
			reqFile.Close()

			log.Fatalf(
				"failed to read requirements.txt: %v",
				err,
			)
		}

		reqFile.Close()

		// --------------------------------------------------------
		// 4. venv Python 찾기
		// --------------------------------------------------------

		pythonExec := utills.GetVenvExecPath(
			absSearchPath,
			"python",
		)

		fmt.Printf(
			"Analyzing python environment (Smart Mode) using: %s\n",
			pythonExec,
		)

		// --------------------------------------------------------
		// 5. 패키지 메타데이터 분석
		// --------------------------------------------------------

		pkgInfoMap, err := fetchPackageInfo(
			pythonExec,
			reqPackages,
		)

		if err != nil {
			fmt.Printf(
				"Warning: Failed to fetch metadata using Python (%v). "+
					"Falling back to static analysis.\n",
				err,
			)

			pkgInfoMap = make(map[string]PkgMeta)
		}

		// --------------------------------------------------------
		// 6. Python 코드 import 분석
		// --------------------------------------------------------

		fmt.Println("Scanning code imports...")

		importedSet := make(map[string]bool)

		var files []string

		err = filepath.Walk(
			searchPath,
			func(
				path string,
				info os.FileInfo,
				err error,
			) error {

				if err != nil {
					return nil
				}

				if info.IsDir() {
					name := info.Name()

					if name == ".venv" ||
						name == "venv" ||
						name == "env" ||
						name == ".git" ||
						name == ".idea" ||
						name == "__pycache__" {

						return filepath.SkipDir
					}

					return nil
				}

				if filepath.Ext(path) == ".py" {
					files = append(files, path)
				}

				return nil
			},
		)

		if err != nil {
			log.Fatalf(
				"failed to scan project: %v",
				err,
			)
		}

		parser := sitter.NewParser()

		defer parser.Close()

		parser.SetLanguage(
			python.GetLanguage(),
		)

		for _, filename := range files {

			absFilename, err := filepath.Abs(filename)

			if err != nil {
				continue
			}

			func() {

				f, err := os.Open(filename)

				if err != nil {
					return
				}

				defer f.Close()

				src, err := io.ReadAll(f)

				if err != nil {
					return
				}

				tree := parser.Parse(nil, src)

				if tree == nil {
					return
				}

				defer tree.Close()

				imports := extractImports(
					tree.RootNode(),
					src,
				)

				for _, imp := range imports {

					if !isLocalModule(
						absSearchPath,
						absFilename,
						imp.Module,
					) {

						importedSet[getRootModule(imp.Module)] = true

						importedSet[imp.Module] = true
					}
				}
			}()
		}

		// --------------------------------------------------------
		// 7. 의존성 보호 목록 생성
		// --------------------------------------------------------

		protectedDeps := make(map[string]bool)

		for _, meta := range pkgInfoMap {

			isDirectlyUsed := false

			for _, importName := range meta.ImportNames {

				if importedSet[importName] ||
					importedSet[getRootModule(importName)] {

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

		// --------------------------------------------------------
		// 8. requirements.txt 정리
		// --------------------------------------------------------

		fmt.Println("Cleaning up...")

		var newLines []string

		var removedCount int

		for _, line := range originalLines {

			pkgName := parsePackageName(line)

			pkgLower := strings.ToLower(pkgName)

			// 빈 줄 / 주석 / ignore
			if pkgName == "" ||
				ignoreList[pkgLower] {

				newLines = append(
					newLines,
					line,
				)

				continue
			}

			isUsed := false

			// ----------------------------------------------------
			// 8-1. 메타데이터 기반 매핑
			// ----------------------------------------------------

			if meta, ok := pkgInfoMap[pkgName]; ok {

				for _, importName := range meta.ImportNames {

					if importedSet[importName] ||
						importedSet[getRootModule(importName)] {

						isUsed = true
						break
					}
				}
			}

			// ----------------------------------------------------
			// 8-2. 단순 이름 일치 fallback
			// ----------------------------------------------------

			if !isUsed {

				if importedSet[pkgName] {
					isUsed = true
				}

				if !isUsed {

					for imp := range importedSet {

						if strings.EqualFold(
							imp,
							pkgName,
						) {

							isUsed = true
							break
						}
					}
				}
			}

			// ----------------------------------------------------
			// 8-3. 의존성 보호
			// ----------------------------------------------------

			if !isUsed &&
				protectedDeps[pkgLower] {

				isUsed = true
			}

			// ----------------------------------------------------
			// 8-4. 결과 처리
			// ----------------------------------------------------

			if isUsed {

				newLines = append(
					newLines,
					line,
				)

			} else {

				fmt.Printf(
					"Removing: %s\n",
					pkgName,
				)

				removedCount++
			}
		}

		// --------------------------------------------------------
		// 9. requirements.txt 저장
		// --------------------------------------------------------

		if removedCount > 0 {

			outFile, err := os.Create(reqPath)

			if err != nil {
				log.Fatal(err)
			}

			w := bufio.NewWriter(outFile)

			for _, line := range newLines {
				fmt.Fprintln(w, line)
			}

			if err := w.Flush(); err != nil {
				outFile.Close()

				log.Fatalf(
					"failed to write requirements.txt: %v",
					err,
				)
			}

			if err := outFile.Close(); err != nil {
				log.Fatalf(
					"failed to close requirements.txt: %v",
					err,
				)
			}

			fmt.Printf(
				"\nRemoved %d packages.\n",
				removedCount,
			)

		} else {

			fmt.Println(
				"\nClean. No unused packages found.",
			)
		}

		// --------------------------------------------------------
		// 10. requirements.txt 기준으로 venv 동기화
		// --------------------------------------------------------

		// 중요:
		// 여기서는 originalLines가 아니라 newLines를 사용한다.
		//
		// 즉 tidy가 제거한 패키지는 다시 설치하지 않는다.
		//
		// removedCount == 0이면 originalLines == newLines와
		// 동일한 효과를 가진다.

		finalRequirementLines := newLines

		if err := installMissingPackages(
			pythonExec,
			finalRequirementLines,
			ignoreList,
		); err != nil {

			log.Fatalf(
				"\nFailed to synchronize venv: %v",
				err,
			)
		}

		// --------------------------------------------------------
		// 11. 완료
		// --------------------------------------------------------

		fmt.Println("\nTidy completed successfully.")
	},
}

func init() {
	tidyCmd.Flags().StringSliceVarP(
		&ignoreFlag,
		"ignore",
		"i",
		nil,
		"dependencies to ignore (comma separated)",
	)

	rootCmd.AddCommand(tidyCmd)
}
