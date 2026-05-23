package scaffold

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/hurtener/Harbor/internal/config"
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
	// BuiltIns is the operator's `tools.built_in` list, projected onto
	// the scaffold templates (Phase 83o / D-154). Empty when no
	// upstream yaml was loaded. The generated `agent.go` reads this
	// to emit `builtin.Register(cat, [...])`.
	BuiltIns []string
	// CustomTools is the projected `tools.custom[]` list with Go-
	// friendly type names (Phase 83o / D-154). One entry per generated
	// `tools/<name>.go` stub + matching test.
	CustomTools []customToolView
}

// customToolView is the per-tool projection the `tools/*.go.tmpl`
// templates consume. Field names are chosen for template readability,
// not for direct yaml mirroring.
type customToolView struct {
	// Name is the operator-facing tool name (e.g. `weather.lookup`).
	// Surfaces in the catalog + in the generated tool's
	// `WithDescription` Option.
	Name string
	// FileName is `Name` sanitised for filesystem use — dots, dashes
	// and other illegal-on-Windows chars become underscores. The
	// generated stub lives at `tools/<FileName>.go`.
	FileName string
	// GoIdent is the Go identifier the generated function uses
	// (e.g. `WeatherLookup`). Capitalised + non-letter chars
	// collapsed to underscores.
	GoIdent string
	// Description is the operator's one-line summary.
	Description string
	// Input + Output are the typed field lists, sorted by JSON name.
	Input  []customToolField
	Output []customToolField
}

// customToolField is one entry in `Input` / `Output`.
type customToolField struct {
	// JSONName is the field name as written in the yaml + the
	// `json:"..."` tag on the generated Go struct.
	JSONName string
	// GoIdent is the JSONName lifted into a Go-exported identifier
	// (CamelCase, leading char uppercased).
	GoIdent string
	// GoType is the resolved Go primitive (`string`, `int`,
	// `float64`, `bool`, `[]string`).
	GoType string
}

// renderProject is the Phase 83o (D-154) replacement for the
// pre-83o `renderTemplate`. It walks the embedded template tree, but
// also threads the upstream config through so per-tool templates can
// fan out, the operator's yaml can be copied verbatim, and existing
// files are skipped under `Options.Patch`.
//
// Filesystem semantics:
//   - outDir is created with mode 0o755.
//   - Subdirectories are created on demand with mode 0o755.
//   - File mode is 0o644 (the templates ship no executables).
//   - Path traversal: each rendered file's destination is verified to
//     remain UNDER outDir via filepath.Abs + strings.HasPrefix.
//
// Returns:
//   - `written` — relative paths Scaffold wrote.
//   - `skipped` — relative paths skipped in patch mode (empty in
//     non-patch runs).
func renderProject(name string, opts Options, absOut, upstreamPath string, upstreamCfg *config.Config) ([]string, []string, error) {
	root := filepath.Join("templates", name)
	if err := os.MkdirAll(absOut, 0o755); err != nil {
		return nil, nil, fmt.Errorf("%w: mkdir output dir %q: %w", ErrRender, absOut, err)
	}
	vars := templateVars{
		Name:          opts.Name,
		GoPackageName: strings.ReplaceAll(opts.Name, "-", "_"),
		Template:      name,
	}
	if upstreamCfg != nil {
		vars.BuiltIns = append([]string(nil), upstreamCfg.Tools.BuiltIn...)
		vars.CustomTools = projectCustomTools(upstreamCfg.Tools.Custom)
	}
	var written, skipped []string

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
		// The per-tool stubs (tool.go.tmpl + tool_test.go.tmpl) are
		// "fan-out" templates — one input file produces N output
		// files. Skip them in the generic walk; they render below.
		if isFanOutTemplate(rel) {
			return nil
		}
		outRel := strings.TrimSuffix(rel, ".tmpl")
		w, s, writeErr := renderOneTemplate(path, rel, outRel, absOut, vars, opts.Patch)
		if writeErr != nil {
			return writeErr
		}
		if w != "" {
			written = append(written, w)
		}
		if s != "" {
			skipped = append(skipped, s)
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrRender, walkErr)
	}

	// Phase 83o / D-154 — overlay the operator's yaml on top of the
	// template-rendered one. We do this AFTER the walk so the
	// rendered placeholder is replaced verbatim by the file the
	// operator actually edited. Patch mode skips when the destination
	// already exists.
	if upstreamPath != "" {
		copied, skippedYaml, copyErr := copyUpstreamYAML(upstreamPath, absOut, opts.Patch)
		if copyErr != nil {
			return nil, nil, copyErr
		}
		if copied != "" {
			// Replace the template-rendered harbor.yaml entry with the
			// copy if it was already in `written` (de-dupe).
			written = replaceOrAppend(written, copied)
		}
		if skippedYaml != "" {
			skipped = append(skipped, skippedYaml)
		}
	}

	// Phase 83o / D-154 — fan-out per-tool stubs.
	if len(vars.CustomTools) > 0 {
		toolWritten, toolSkipped, toolErr := renderCustomTools(root, vars, absOut, opts.Patch)
		if toolErr != nil {
			return nil, nil, toolErr
		}
		written = append(written, toolWritten...)
		skipped = append(skipped, toolSkipped...)
	}

	sort.Strings(written)
	sort.Strings(skipped)
	return written, skipped, nil
}

// isFanOutTemplate reports whether the embedded template at `rel`
// (relative to `templates/<name>/`) is a per-custom-tool fan-out
// template that the generic walk should skip. The fan-out renderer
// (renderCustomTools) handles these.
func isFanOutTemplate(rel string) bool {
	switch rel {
	case "tool.go.tmpl", "tool_test.go.tmpl":
		return true
	}
	return false
}

// renderOneTemplate parses + executes a single template and writes
// it to `outRel` under `absOut`. Returns `(written, skipped, err)`
// where exactly one of `written` / `skipped` is non-empty on success.
func renderOneTemplate(embedPath, parseName, outRel, absOut string, vars any, patch bool) (string, string, error) {
	dest := filepath.Join(absOut, outRel)
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return "", "", fmt.Errorf("abs dest %s: %w", dest, err)
	}
	if !strings.HasPrefix(destAbs, absOut+string(os.PathSeparator)) && destAbs != absOut {
		return "", "", fmt.Errorf("dest %s escapes output dir %s", destAbs, absOut)
	}
	if patch {
		if _, statErr := os.Stat(destAbs); statErr == nil {
			return "", outRel, nil
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return "", "", fmt.Errorf("stat %s: %w", destAbs, statErr)
		}
	}
	if err := os.MkdirAll(filepath.Dir(destAbs), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", filepath.Dir(destAbs), err)
	}
	raw, err := fs.ReadFile(templatesFS, embedPath)
	if err != nil {
		return "", "", fmt.Errorf("read embedded %s: %w", embedPath, err)
	}
	t, err := template.New(parseName).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return "", "", fmt.Errorf("parse %s: %w", parseName, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", "", fmt.Errorf("execute %s: %w", parseName, err)
	}
	//nolint:gosec // scaffolded project source files are intended to be world-readable (0o644); they carry no secrets
	if err := os.WriteFile(destAbs, buf.Bytes(), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", destAbs, err)
	}
	return outRel, "", nil
}

// renderCustomTools fans out the per-tool stubs (Phase 83o / D-154).
// One pair of files per `vars.CustomTools` entry: `tools/<name>.go`
// + `tools/<name>_test.go`.
func renderCustomTools(root string, vars templateVars, absOut string, patch bool) ([]string, []string, error) {
	const goTpl = "tool.go.tmpl"
	const testTpl = "tool_test.go.tmpl"
	goEmbed := filepath.Join(root, goTpl)
	testEmbed := filepath.Join(root, testTpl)
	var written, skipped []string
	for _, tool := range vars.CustomTools {
		// Build a per-tool vars view so the template can reference
		// {{.Tool}} alongside the project-scoped fields.
		perTool := struct {
			templateVars
			Tool customToolView
		}{
			templateVars: vars,
			Tool:         tool,
		}
		w, s, err := renderOneTemplate(goEmbed, goTpl,
			filepath.Join("tools", tool.FileName+".go"), absOut, perTool, patch)
		if err != nil {
			return nil, nil, err
		}
		if w != "" {
			written = append(written, w)
		}
		if s != "" {
			skipped = append(skipped, s)
		}
		w, s, err = renderOneTemplate(testEmbed, testTpl,
			filepath.Join("tools", tool.FileName+"_test.go"), absOut, perTool, patch)
		if err != nil {
			return nil, nil, err
		}
		if w != "" {
			written = append(written, w)
		}
		if s != "" {
			skipped = append(skipped, s)
		}
	}
	return written, skipped, nil
}

// loadUpstreamConfig resolves the operator-supplied yaml. Returns
// `(path, cfg, error)`: when both `path` is empty and `cfg` is nil
// the caller falls back to template-only rendering.
//
// Resolution order:
//  1. `fromPath` non-empty → load it; missing file is an error.
//  2. `fromPath` empty + `./harbor.yaml` in cwd → load it.
//  3. Neither → return (empty, nil, nil).
//
// A loaded yaml is validated via `internal/config.Load`. Validation
// errors are wrapped with `ErrUpstreamConfigInvalid` so the CLI maps
// them to the structured code.
func loadUpstreamConfig(fromPath string) (string, *config.Config, error) {
	path := fromPath
	if path == "" {
		const auto = "harbor.yaml"
		if _, err := os.Stat(auto); err == nil {
			path = auto
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("%w: stat ./harbor.yaml: %w", ErrRender, err)
		}
	}
	if path == "" {
		return "", nil, nil
	}
	cfg, err := config.Load(context.Background(), path)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %s: %w", ErrUpstreamConfigInvalid, path, err)
	}
	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		return "", nil, fmt.Errorf("%w: abs %s: %w", ErrRender, path, absErr)
	}
	return abs, cfg, nil
}

// copyUpstreamYAML copies the operator-edited yaml verbatim into
// `OutputDir/harbor.yaml`, preserving comments. In patch mode, an
// existing destination is skipped; without patch, a destination
// collision (the templated yaml that the walk just rendered) is
// overwritten — the operator's file is the source of truth.
func copyUpstreamYAML(srcPath, absOut string, patch bool) (string, string, error) {
	const dstRel = "harbor.yaml"
	dst := filepath.Join(absOut, dstRel)
	if patch {
		if _, statErr := os.Stat(dst); statErr == nil {
			return "", dstRel, nil
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return "", "", fmt.Errorf("stat %s: %w", dst, statErr)
		}
	}
	// In non-patch mode the template-walk just wrote a placeholder
	// yaml; overwrite it with the operator's edited version.
	src, err := os.Open(srcPath)
	if err != nil {
		return "", "", fmt.Errorf("%w: open upstream yaml %s: %w", ErrRender, srcPath, err)
	}
	defer src.Close()
	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", "", fmt.Errorf("%w: open dest yaml %s: %w", ErrRender, dst, err)
	}
	defer dstFile.Close()
	if _, err := io.Copy(dstFile, src); err != nil {
		return "", "", fmt.Errorf("%w: copy yaml: %w", ErrRender, err)
	}
	return dstRel, "", nil
}

// replaceOrAppend keeps a sorted-unique invariant on the `written`
// slice: if `path` already appears, no-op; otherwise append.
func replaceOrAppend(written []string, path string) []string {
	for _, w := range written {
		if w == path {
			return written
		}
	}
	return append(written, path)
}

// projectCustomTools maps a slice of `CustomToolConfig` to the
// template-friendly `customToolView` shape (Phase 83o / D-154). The
// returned slice is sorted by tool name so renders are deterministic.
func projectCustomTools(in []config.CustomToolConfig) []customToolView {
	if len(in) == 0 {
		return nil
	}
	out := make([]customToolView, 0, len(in))
	for _, t := range in {
		out = append(out, customToolView{
			Name:        t.Name,
			FileName:    fileNameFromToolName(t.Name),
			GoIdent:     goIdentFromToolName(t.Name),
			Description: t.Description,
			Input:       projectFields(t.Input),
			Output:      projectFields(t.Output),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// projectFields maps a `field:type` map to a sorted slice of
// `customToolField` so the generated struct fields render in a
// stable order.
func projectFields(m map[string]string) []customToolField {
	if len(m) == 0 {
		return nil
	}
	out := make([]customToolField, 0, len(m))
	for k, v := range m {
		out = append(out, customToolField{
			JSONName: k,
			GoIdent:  goIdentFromFieldName(k),
			GoType:   goTypeFromYAMLType(v),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JSONName < out[j].JSONName })
	return out
}

// fileNameFromToolName turns a tool name like `weather.lookup` into a
// filesystem-safe basename like `weather_lookup`. Dots and dashes
// collapse to underscores; other chars pass through unchanged (the
// validator already enforces the broader char set).
func fileNameFromToolName(name string) string {
	r := strings.NewReplacer(".", "_", "-", "_")
	return r.Replace(name)
}

// goIdentFromToolName lifts a tool name into a Go-exported identifier.
// `weather.lookup` → `WeatherLookup`; `get_temp` → `GetTemp`.
func goIdentFromToolName(name string) string {
	parts := splitIdentParts(name)
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		if len(p) > 1 {
			b.WriteString(p[1:])
		}
	}
	return b.String()
}

// goIdentFromFieldName lifts a json field name into a Go-exported
// identifier. `temp_c` → `TempC`; `summary` → `Summary`.
func goIdentFromFieldName(name string) string {
	return goIdentFromToolName(name)
}

// splitIdentParts splits a tool/field name on each non-identifier
// char so `goIdentFromToolName` can capitalise the segments.
func splitIdentParts(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	})
}

// goTypeFromYAMLType maps a yaml-shorthand type onto its Go primitive.
// V1.1 supports the closed set `string` / `integer` / `number` /
// `boolean` / `[]string`. The validator already rejects anything
// else; this function returns `any` as a defensive fallback so a
// future addition that misses the mapping fails LOUD when the
// generated Go fails to compile rather than silently dropping the
// field.
func goTypeFromYAMLType(t string) string {
	switch t {
	case "string":
		return "string"
	case "integer":
		return "int"
	case "number":
		return "float64"
	case "boolean":
		return "bool"
	case "[]string":
		return "[]string"
	default:
		return "any"
	}
}

// removeAll is a tiny wrapper around os.RemoveAll so render-time
// cleanup is a single named call. The helper exists so a future test
// can substitute a no-op cleanup; it has no other purpose.
func removeAll(path string) error {
	return os.RemoveAll(path)
}
