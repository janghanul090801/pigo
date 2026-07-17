package utills

import (
	"os"
	"path/filepath"
)

// GetVenvExecPath finds the executable (e.g. "pip", "python") inside virtual environments.
func GetVenvExecPath(searchPath string, binaryName string) string {
	exeName := binaryName
	if os.PathSeparator == '\\' {
		exeName += ".exe"
	}

	// 1. Check activated virtualenv (VIRTUAL_ENV)
	if venv := os.Getenv("VIRTUAL_ENV"); venv != "" {
		var path string
		if os.PathSeparator == '\\' {
			path = filepath.Join(venv, "Scripts", exeName)
		} else {
			path = filepath.Join(venv, "bin", exeName)
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	// 2. Check conda prefix (CONDA_PREFIX)
	if conda := os.Getenv("CONDA_PREFIX"); conda != "" {
		var path string
		if os.PathSeparator == '\\' {
			if binaryName == "python" {
				path = filepath.Join(conda, exeName)
			} else {
				path = filepath.Join(conda, "Scripts", exeName)
			}
		} else {
			path = filepath.Join(conda, "bin", exeName)
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	// 3. Search starting from searchPath
	if searchPath == "" {
		if wd, err := os.Getwd(); err == nil {
			searchPath = wd
		} else {
			searchPath = "."
		}
	}
	absSearchPath, _ := filepath.Abs(searchPath)

	candidates := []string{".venv", "venv", "env"}
	for _, c := range candidates {
		venvPath := filepath.Join(absSearchPath, c)
		var path string
		if os.PathSeparator == '\\' {
			path = filepath.Join(venvPath, "Scripts", exeName)
		} else {
			path = filepath.Join(venvPath, "bin", exeName)
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}

		parentVenvPath := filepath.Join(filepath.Dir(absSearchPath), c)
		if os.PathSeparator == '\\' {
			path = filepath.Join(parentVenvPath, "Scripts", exeName)
		} else {
			path = filepath.Join(parentVenvPath, "bin", exeName)
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return binaryName
}
