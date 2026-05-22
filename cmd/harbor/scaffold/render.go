package scaffold

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// templateVars is the data passed to every .tmpl file's text/template
// execution. Add new fields here when templates need them; keep the
// shape small and predictable so future templates inherit a stable
// vocabulary.
type templateVars struct {
	// Name is the project name passed via Options.Name. Templates
	// reference it as {{.Name}}.
	Name string
	// GoPackageName is Name with `-` replaced by `_` so it can be
	// used verbatim as a Go package identifier (dashes are not legal
	// in Go package names). Templates reference it as
	// {{.GoPackageName}}.
	GoPackageName string
	// Template is the template name selected. Templates reference it
	// as {{.Template}}; used in the rendered README to identify the
	// origin.
	Template string
}

// renderTemplate walks templates/<name>/, renders every .tmpl file
// against vars, and writes the rendered output to outDir/<rel-path>
// (stripping the trailing `.tmpl` suffix). The returned slice is the
// deterministic-order list of relative paths written.
//
// Filesystem semantics:
//   - outDir is created with mode 0o755.
//   - Subdirectories are created on demand with mode 0o755.
//   - File mode is 0o644 (the templates ship no executables).
//   - Path traversal: each rendered file's destination is verified to
//     remain UNDER outDir via filepath.Abs + strings.HasPrefix; an
//     escape attempt fails loud with ErrRender (defence-in-depth — the
//     embedded templates are trusted, but template names with `..`
//     would be a regression).
func renderTemplate(name, projectName, outDir string) ([]string, error) {
	root := filepath.Join("templates", name)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: mkdir output dir %q: %w", ErrRender, outDir, err)
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return nil, fmt.Errorf("%w: abs output dir %q: %w", ErrRender, outDir, err)
	}
	vars := templateVars{
		Name:          projectName,
		GoPackageName: strings.ReplaceAll(projectName, "-", "_"),
		Template:      name,
	}
	var written []string
	walkErr := fs.WalkDir(templatesFS, root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		// Strip the trailing `.tmpl` so the on-disk filename is the
		// expected one (e.g. `go.mod.tmpl` → `go.mod`).
		outRel := strings.TrimSuffix(rel, ".tmpl")
		dest := filepath.Join(absOut, outRel)
		// Path-traversal defence: the destination MUST live under
		// absOut.
		destAbs, err := filepath.Abs(dest)
		if err != nil {
			return fmt.Errorf("abs dest %s: %w", dest, err)
		}
		if !strings.HasPrefix(destAbs, absOut+string(os.PathSeparator)) && destAbs != absOut {
			return fmt.Errorf("dest %s escapes output dir %s", destAbs, absOut)
		}
		if err := os.MkdirAll(filepath.Dir(destAbs), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(destAbs), err)
		}
		raw, err := fs.ReadFile(templatesFS, path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		// Use a unique template name per file so error messages name
		// the source. Disable auto-trim so the rendered output is
		// byte-stable.
		t, err := template.New(rel).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, vars); err != nil {
			return fmt.Errorf("execute %s: %w", rel, err)
		}
		//nolint:gosec // scaffolded project source files are intended to be world-readable (0o644); they carry no secrets
		if err := os.WriteFile(destAbs, buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", destAbs, err)
		}
		written = append(written, outRel)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrRender, walkErr)
	}
	sort.Strings(written)
	return written, nil
}

// removeAll is a tiny wrapper around os.RemoveAll so render-time
// cleanup is a single named call. The helper exists so a future test
// can substitute a no-op cleanup; it has no other purpose.
func removeAll(path string) error {
	return os.RemoveAll(path)
}
