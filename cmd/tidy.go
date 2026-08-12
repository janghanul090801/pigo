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
	"sort"
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

// import 이름과 pip package 이름이 다른 경우
var importPackageAliases = map[string]string{
	"cv2":       "opencv-python",
	"PIL":       "Pillow",
	"sklearn":   "scikit-learn",
	"yaml":      "PyYAML",
	"bs4":       "beautifulsoup4",
	"dotenv":    "python-dotenv",
	"jose":      "python-jose",
	"jwt":       "PyJWT",
	"Crypto":    "pycryptodome",
	"dateutil":  "python-dateutil",
	"google":    "google",
	"serial":    "pyserial",
	"fitz":      "PyMuPDF",
	"cv":        "opencv-python",
	"multipart": "python-multipart",
}

// --- [Python 메타데이터 분석 스크립트] ---

const pythonMapperScript = `
import sys
import json
import importlib.metadata


def parse_req_name(req_str):
    if not req_str:
        return ""

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
    modules = set()

    if not dist.files:
        return []

    for path in dist.files:
        parts = path.parts

        if len(parts) == 0:
            continue

        top = parts[0]

        if (
            top.endswith('.dist-info')
            or top.endswith('.egg-info')
            or top == '__pycache__'
        ):
            continue

        if top.endswith('.py'):
            modules.add(top[:-3])
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
            info["imports"] = [
                pkg.lower().replace('-', '_'),
                pkg,
            ]

        result[pkg_raw] = info

    return result


def get_distribution_for_import(import_name):
    """
    importlib.metadata.packages_distributions()를 사용해서
    import 이름 -> pip distribution 이름을 찾는다.

    예:
        requests -> requests
        PIL -> Pillow
        cv2 -> opencv-python
        sklearn -> scikit-learn
    """

    try:
        mapping = importlib.metadata.packages_distributions()

        distributions = mapping.get(import_name, [])

        if distributions:
            return distributions[0]

    except Exception:
        pass

    return ""


def is_stdlib(module_name):
    try:
        return module_name in sys.stdlib_module_names
    except AttributeError:
        return False


if __name__ == "__main__":
    input_data = sys.stdin.read()

    if not input_data:
        print("{}")
        sys.exit(0)

    try:
        data = json.loads(input_data)

        if data.get("mode") == "package_info":
            packages = data.get("packages", [])

            result = get_package_info(packages)

            print(json.dumps(result))

        elif data.get("mode") == "distribution_for_import":
            import_name = data.get("import", "")

            if is_stdlib(import_name):
                print(json.dumps({
                    "distribution": "",
                    "stdlib": True,
                }))
                sys.exit(0)

            distribution = get_distribution_for_import(import_name)

            print(json.dumps({
                "distribution": distribution,
                "stdlib": False,
            }))

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
					child.Type() == "aliased_import") &&
					child != modNode {

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

	// 주석
	if strings.HasPrefix(line, "#") {
		return ""
	}

	// requirements.txt 옵션
	if strings.HasPrefix(line, "-") {
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

	inputJSON, err := json.Marshal(map[string]interface{}{
		"mode":     "package_info",
		"packages": packageNames,
	})

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

// import 이름을 실제 pip package 이름으로 변환한다.
func resolvePackageName(
	pythonExec string,
	importName string,
) (string, bool, error) {

	root := getRootModule(importName)

	// 흔히 import 이름과 pip 이름이 다른 패키지
	if pkg, ok := importPackageAliases[root]; ok {
		return pkg, false, nil
	}

	inputJSON, err := json.Marshal(map[string]interface{}{
		"mode":   "distribution_for_import",
		"import": root,
	})

	if err != nil {
		return "", false, err
	}

	cmd := exec.Command(
		pythonExec,
		"-c",
		pythonMapperScript,
	)

	stdin, err := cmd.StdinPipe()

	if err != nil {
		return "", false, err
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
		return "", false, err
	}

	var result struct {
		Distribution string `json:"distribution"`
		Stdlib       bool   `json:"stdlib"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return "", false, err
	}

	if result.Stdlib {
		return "", true, nil
	}

	if result.Distribution != "" {
		return result.Distribution, false, nil
	}

	// metadata로 찾을 수 없는 경우
	// 가장 일반적인 경우에는 import 이름 == pip 이름
	return root, false, nil
}

func requirementExists(
	requirementLines []string,
	packageName string,
) bool {

	target := strings.ToLower(packageName)

	for _, line := range requirementLines {
		name := parsePackageName(line)

		if name == "" {
			continue
		}

		if strings.ToLower(name) == target {
			return true
		}
	}

	return false
}

func appendRequirement(
	requirementLines *[]string,
	packageName string,
) {

	if requirementExists(
		*requirementLines,
		packageName,
	) {
		return
	}

	*requirementLines = append(
		*requirementLines,
		packageName,
	)

	fmt.Printf(
		"Adding to requirements.txt: %s\n",
		packageName,
	)
}

func writeRequirements(
	reqPath string,
	lines []string,
) error {

	outFile, err := os.Create(reqPath)

	if err != nil {
		return err
	}

	defer outFile.Close()

	w := bufio.NewWriter(outFile)

	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}

	return w.Flush()
}

// requirements.txt에 없지만 코드에서 import된 패키지를 찾는다.
func findMissingRequirements(
	pythonExec string,
	importedSet map[string]bool,
	requirementLines *[]string,
	ignoreList map[string]bool,
) ([]string, error) {

	var packagesToInstall []string

	fmt.Println("\nChecking code imports against requirements.txt...")

	var imports []string

	for importName := range importedSet {
		// root module만 처리
		if importName != getRootModule(importName) {
			continue
		}

		imports = append(
			imports,
			importName,
		)
	}

	sort.Strings(imports)

	for _, importName := range imports {

		if importName == "" {
			continue
		}

		// requirements에 이미 있으면 끝
		if requirementMatchesImport(
			importName,
			*requirementLines,
			pythonExec,
		) {
			continue
		}

		pkgName, isStdlib, err := resolvePackageName(
			pythonExec,
			importName,
		)

		if err != nil {
			return nil, fmt.Errorf(
				"failed to resolve package for import %q: %w",
				importName,
				err,
			)
		}

		// Python 표준 라이브러리
		if isStdlib {
			continue
		}

		if pkgName == "" {
			continue
		}

		pkgLower := strings.ToLower(
			parsePackageName(pkgName),
		)

		if ignoreList[pkgLower] {
			continue
		}

		if requirementExists(
			*requirementLines,
			pkgName,
		) {
			continue
		}

		fmt.Printf(
			"Missing requirement: import %s -> %s\n",
			importName,
			pkgName,
		)

		appendRequirement(
			requirementLines,
			pkgName,
		)

		packagesToInstall = append(
			packagesToInstall,
			pkgName,
		)
	}

	return packagesToInstall, nil
}

// import가 requirements.txt에 포함되는지 검사한다.
//
// 예:
// requests -> requests
// cv2 -> opencv-python
// sklearn -> scikit-learn
func requirementMatchesImport(
	importName string,
	requirementLines []string,
	pythonExec string,
) bool {

	root := getRootModule(importName)

	// 직접 이름 비교
	for _, line := range requirementLines {

		pkgName := parsePackageName(line)

		if pkgName == "" {
			continue
		}

		if strings.EqualFold(
			pkgName,
			root,
		) {
			return true
		}
	}

	// alias 비교
	if alias, ok := importPackageAliases[root]; ok {

		for _, line := range requirementLines {

			pkgName := parsePackageName(line)

			if pkgName == "" {
				continue
			}

			if strings.EqualFold(
				pkgName,
				alias,
			) {
				return true
			}
		}

		return false
	}

	// 설치된 distribution의 import 이름과 비교
	pkgInfo, err := fetchPackageInfo(
		pythonExec,
		extractRequirementNames(requirementLines),
	)

	if err != nil {
		return false
	}

	for pkgName, meta := range pkgInfo {

		for _, importNameFromPackage := range meta.ImportNames {

			if strings.EqualFold(
				getRootModule(importNameFromPackage),
				root,
			) {
				_ = pkgName
				return true
			}
		}
	}

	return false
}

func extractRequirementNames(
	requirementLines []string,
) []string {

	var names []string

	for _, line := range requirementLines {

		name := parsePackageName(line)

		if name == "" {
			continue
		}

		names = append(
			names,
			name,
		)
	}

	return names
}

// requirements.txt에 존재하지만 venv에 설치되지 않은 package 설치
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

		if ignoreList[pkgLower] {
			continue
		}

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

	if len(missingRequirements) == 0 {
		fmt.Println(
			"All requirements are already installed.",
		)

		return nil
	}

	fmt.Println("\nMissing packages:")

	for _, requirement := range missingRequirements {
		fmt.Printf(
			"  - %s\n",
			requirement,
		)
	}

	fmt.Println(
		"\nInstalling missing packages...",
	)

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

// 코드에서 발견한 package를 pip로 설치
func installPackages(
	pythonExec string,
	packages []string,
) error {

	if len(packages) == 0 {
		return nil
	}

	// 중복 제거
	unique := make(map[string]bool)

	var targets []string

	for _, pkg := range packages {

		pkg = strings.TrimSpace(pkg)

		if pkg == "" {
			continue
		}

		key := strings.ToLower(pkg)

		if unique[key] {
			continue
		}

		unique[key] = true
		targets = append(targets, pkg)
	}

	if len(targets) == 0 {
		return nil
	}

	fmt.Println(
		"\nInstalling packages discovered from source code...",
	)

	for _, pkg := range targets {
		fmt.Printf(
			"  - %s\n",
			pkg,
		)
	}

	args := []string{
		"-m",
		"pip",
		"install",
	}

	args = append(
		args,
		targets...,
	)

	cmd := exec.Command(
		pythonExec,
		args...,
	)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return fmt.Errorf(
			"failed to install source dependencies: %w",
			err,
		)
	}

	return nil
}

var ignoreFlag []string

var tidyCmd = &cobra.Command{
	Use:   "tidy [path]",
	Short: "Automatically remove unused packages and synchronize dependencies",
	Long: `Analyzes Python dependencies by inspecting installed
package metadata and source-code imports.

The command:

1. Removes unused packages from requirements.txt.
2. Finds packages imported by source code but missing from requirements.txt.
3. Installs missing packages into the virtual environment.
4. Adds newly discovered packages to requirements.txt.
5. Installs packages that exist in requirements.txt but are missing
   from the current virtual environment.`,
	Run: func(cmd *cobra.Command, args []string) {

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

		// --------------------------------------------------
		// Ignore List 구성
		// --------------------------------------------------

		ignoreList := make(map[string]bool)

		for k, v := range defaultIgnoreList {
			ignoreList[k] = v
		}

		for _, val := range ignoreFlag {
			ignoreList[strings.ToLower(
				strings.TrimSpace(val),
			)] = true
		}

		// --------------------------------------------------
		// requirements.txt 읽기
		// --------------------------------------------------

		fmt.Println(
			"Reading requirements.txt...",
		)

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

		// --------------------------------------------------
		// Venv Python
		// --------------------------------------------------

		pythonExec := utills.GetVenvExecPath(
			absSearchPath,
			"python",
		)

		fmt.Printf(
			"Analyzing python environment (Smart Mode) using: %s\n",
			pythonExec,
		)

		// --------------------------------------------------
		// 기존 package metadata
		// --------------------------------------------------

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

		// --------------------------------------------------
		// 전체 Python 코드 순회
		// --------------------------------------------------

		fmt.Println(
			"Scanning code imports...",
		)

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
					files = append(
						files,
						path,
					)
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

				tree := parser.Parse(
					nil,
					src,
				)

				if tree == nil {
					return
				}

				defer tree.Close()

				imports := extractImports(
					tree.RootNode(),
					src,
				)

				for _, imp := range imports {

					if isLocalModule(
						absSearchPath,
						absFilename,
						imp.Module,
					) {
						continue
					}

					rootModule := getRootModule(
						imp.Module,
					)

					if rootModule == "" ||
						rootModule == "." {
						continue
					}

					importedSet[rootModule] = true
				}
			}()
		}

		// --------------------------------------------------
		// requirements에 없는 dependency 찾기
		// --------------------------------------------------

		finalRequirementLines := make(
			[]string,
			len(originalLines),
		)

		copy(
			finalRequirementLines,
			originalLines,
		)

		missingFromRequirements, err :=
			findMissingRequirements(
				pythonExec,
				importedSet,
				&finalRequirementLines,
				ignoreList,
			)

		if err != nil {
			log.Fatalf(
				"failed to find missing requirements: %v",
				err,
			)
		}

		// --------------------------------------------------
		// 기존 dependency의 transitive dependency 보호
		// --------------------------------------------------

		protectedDeps := make(map[string]bool)

		for _, meta := range pkgInfoMap {

			isDirectlyUsed := false

			for _, importName := range meta.ImportNames {

				if importedSet[getRootModule(importName)] {
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

		// --------------------------------------------------
		// 사용하지 않는 requirements 제거
		// --------------------------------------------------

		fmt.Println(
			"Cleaning up...",
		)

		var newLines []string

		var removedCount int

		for _, line := range finalRequirementLines {

			pkgName := parsePackageName(line)

			pkgLower := strings.ToLower(pkgName)

			// 빈 줄 / 주석 / 옵션 / ignore
			if pkgName == "" ||
				ignoreList[pkgLower] {

				newLines = append(
					newLines,
					line,
				)

				continue
			}

			isUsed := false

			if meta, ok := pkgInfoMap[pkgName]; ok {

				for _, importName := range meta.ImportNames {

					if importedSet[getRootModule(importName)] {
						isUsed = true
						break
					}
				}
			}

			// package name == import name
			if !isUsed {

				if importedSet[pkgName] {
					isUsed = true
				}
			}

			// case insensitive 비교
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

			// transitive dependency
			if !isUsed &&
				protectedDeps[pkgLower] {

				isUsed = true
			}

			// alias package
			if !isUsed {

				for importName, packageName := range importPackageAliases {

					if strings.EqualFold(
						packageName,
						pkgName,
					) &&
						importedSet[importName] {

						isUsed = true
						break
					}
				}
			}

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

		// --------------------------------------------------
		// requirements.txt 저장
		// --------------------------------------------------

		if removedCount > 0 ||
			len(missingFromRequirements) > 0 {

			if err := writeRequirements(
				reqPath,
				newLines,
			); err != nil {

				log.Fatalf(
					"failed to write requirements.txt: %v",
					err,
				)
			}

			fmt.Printf(
				"\nUpdated requirements.txt.\n",
			)

			if removedCount > 0 {
				fmt.Printf(
					"Removed %d package(s).\n",
					removedCount,
				)
			}

			if len(missingFromRequirements) > 0 {
				fmt.Printf(
					"Added %d package(s).\n",
					len(missingFromRequirements),
				)
			}

		} else {

			// 제거/추가가 없어도 newLines를 유지
			if len(newLines) == 0 {
				newLines = finalRequirementLines
			}

			fmt.Println(
				"\nClean. No requirements changes found.",
			)
		}

		// --------------------------------------------------
		// 새로 발견된 source dependency 설치
		// --------------------------------------------------

		if err := installPackages(
			pythonExec,
			missingFromRequirements,
		); err != nil {

			log.Fatalf(
				"\nFailed to install source dependencies: %v",
				err,
			)
		}

		// --------------------------------------------------
		// requirements.txt 기준 venv 동기화
		// --------------------------------------------------

		if err := installMissingPackages(
			pythonExec,
			newLines,
			ignoreList,
		); err != nil {

			log.Fatalf(
				"\nFailed to synchronize venv: %v",
				err,
			)
		}

		// --------------------------------------------------
		// 최종 완료
		// --------------------------------------------------

		fmt.Println(
			"\nTidy completed successfully.",
		)
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
