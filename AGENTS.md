# Pigo AGENTS.md

**pigo** is a Go CLI tool that manages Python virtual environments and synchronizes dependencies.
It provides `init`, `get`, `remove`, `run`, and `tidy` subcommands.

---

## Quick commands

| Command | Description |
|---|---|
| `go run main.go` | Entry point — runs the pigo CLI |
| `pigo init` | Creates `.venv` (Python venv) and `requirements.txt` |
| `pigo get <pkg>` | Installs `<pkg>` via pip and appends to `requirements.txt` |
| `pigo remove <pkg>` | Uninstalls `<pkg>` via pip and removes from `requirements.txt` |
| `pigo run <file>` | Executes `<file>` using the `.venv` Python interpreter |
| `pigo tidy [path]` | **Full sync**: scans imports, removes unused deps, installs missing ones, updates `requirements.txt` |

---

## Environment & platform quirks

- **Virtual env detection**: pigo looks for `.venv`, `venv`, or `env` directories. It first checks `VIRTUAL_ENV` / `CONDA_PREFIX` env vars, then searches upward from the cwd.
- **Windows vs POSIX paths**: `GetVenvExecPath` in `utills/env.go` adds `.exe` suffix on Windows (`PathSeparator == '\\`) and uses `Scripts/` vs `bin/` directory naming.
- If no venv is found, subcommands that need Python (`init`, `get`, `run`, `tidy`) will fall back to the system `python` executable — but results may differ from an isolated venv.

---

## Import/package name mapping (critical for `tidy` and `get`)

The `importPackageAliases` map in `cmd/tidy.go` maps Python `import` names to their actual pip package names. This is why `cv2` → `opencv-python`, `PIL` → `Pillow`, `sklearn` → `scikit-learn`, etc. Without this mapping, `tidy` and `get` would resolve the wrong package.

- `defaultIgnoreList` also in `cmd/tidy.go` lists packages always skipped during cleanup: `pytest`, `black`, `flake8`, `mypy`, `pylint`, `ipython`, `gunicorn`, `uvicorn`, `wheel`, `setuptools`, `pip`, `tox`, `pre-commit`, `poetry`.

---

## Protect packages from deletion

The `pigo tidy` command can remove packages from `requirements.txt` that are not used in your code. To prevent specific packages from being removed, use the `--ignore` (or `-i`) flag:

```bash
pigo tidy --ignore numpy,pandas,requests
```

or with short flag:

```bash
pigo tidy -i numpy,pandas,requests
```

This is useful for protecting:
- Frequently used packages that you always want in `requirements.txt`
- Packages with special installation requirements
- Packages you manage separately

The ignored packages are preserved in `requirements.txt` even if they are not directly imported in your code. They are also excluded from the "remove unused" analysis.

**Combined with `defaultIgnoreList`**: The built-in `defaultIgnoreList` (pytest, black, flake8, mypy, etc.) is always applied first, and then `--ignore` adds additional packages to protect.

---

## `tidy` command flow (what actually happens)

1. Scans all `.py` files in the given path (skips `.venv`, `venv`, `env`, `.git`, `.idea`, `__pycache__`).
2. Uses **tree-sitter** (`github.com/smacker/go-tree-sitter/python`) to extract `import` and `import from` statements.
3. Resolves each imported module to a pip package name via:
   - `importPackageAliases` map
   - `packages_distributions()` metadata lookup
   - Fallback: `import_name.lower().replace('-', '_')`
4. Compares discovered imports against `requirements.txt`:
   - **Removes** packages not imported anywhere.
   - **Adds** packages imported in code but missing from `requirements.txt`.
   - **Installs** newly added packages into the venv.
   - **Syncs** the venv: uninstalls packages from `requirements.txt` that are absent from the current venv.
5. Writes the updated `requirements.txt`.

**Order matters**: `tidy` does cleanup → discovery → installation → sync. Running `lint -> typecheck -> test` is not applicable here (this is a Go tool managing Python deps), but if you chain commands, always run `tidy` before `get` or `remove` to keep `requirements.txt` consistent.

---

## Go tooling

- `go.mod` declares `go 1.25.2` — verify your local Go version compatibility.
- CI uses Go 1.22 (`actions/setup-go@v5`).
- Project uses **cobra** (`github.com/spf13/cobra`) for CLI scaffolding.
- Third-party parsing depends on **go-tree-sitter** (`github.com/smacker/go-tree-sitter/python`).
- To add a new subcommand: add a `*cobra.Command` in `cmd/` and register it via `rootCmd.AddCommand(...)` in `cmd/init.go`'s `init()` (or the respective command's `init()`).

---

## Release

- GitHub Actions (`.github/workflows/release.yml`) builds Windows and Linux binaries using **goreleaser**.
- Windows: `goreleaser release --clean --config .goreleaser.windows.yml`
- Linux: `goreleaser release --clean --config .goreleaser.linux.yml` (also installs `gcc-aarch64-linux-gnu` cross-compiler on ubuntu-latest).