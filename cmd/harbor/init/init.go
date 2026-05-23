// Package harborinit ships the `harbor init` engine.
//
// `harbor init` materialises a tiered, commented `harbor.yaml` plus
// three companion files (`AGENTS.md`, `CLAUDE.md`, `README.md`) into
// a target directory. The operator then edits the yaml — uncomments
// one of four LLM-provider example blocks, opts into built-in tools,
// adds MCP servers, tunes memory — and proceeds with `harbor
// validate` and `harbor scaffold`.
//
// Phase 83n / D-153. The shape mirrors `cmd/harbor/scaffold`:
//
//   - Templates live under `cmd/harbor/init/templates/<name>/`.
//   - The default template is named `default` (V1.1 ships exactly
//     one — a second template earns its keep when a real second use
//     case arrives, not before).
//   - Files are rendered through `text/template` with `{.Name}` as
//     the only variable.
//
// The engine refuses to overwrite an existing file. A target
// directory that holds even one of the four files is rejected with
// `ErrFileExists` naming the offending path — the operator deletes
// the file (or picks a fresh `--target`) before retrying. This is the
// §13 fail-loud posture for an operator-facing seam.
package harborinit

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

// DefaultTemplate is the template `harbor init` selects when the
// operator omits --template. V1.1 ships only `default`.
const DefaultTemplate = "default"

// Sentinel errors. Callers (`cmd/harbor/cmd_init.go::runInit`)
// compare via errors.Is and map onto the CLIError code surface.
var (
	// ErrInvalidName signals Options.Name failed validateName.
	ErrInvalidName = errors.New("init: invalid project name")
	// ErrFileExists signals one of the four files Init would write
	// already exists in the target directory. Operators delete the
	// file or pick a fresh target — Init refuses to overwrite.
	ErrFileExists = errors.New("init: target file already exists")
	// ErrUnknownTemplate signals the requested template does not
	// exist in the embedded tree.
	ErrUnknownTemplate = errors.New("init: unknown template")
	// ErrInitFailed wraps any rendering / write failure not covered
	// by the more specific sentinels above.
	ErrInitFailed = errors.New("init: render failed")
)

// Options is the input to Init.
type Options struct {
	// Name is the agent name. Used as `.Name` inside templates so the
	// rendered files refer to the operator's intended agent. When
	// empty, Init derives the name from `filepath.Base(TargetDir)`;
	// when the basename does not satisfy `validateName`, Init falls
	// back to `"agent"` (the operator can rename freely after).
	Name string
	// Template selects which embedded template tree to render. Empty
	// defaults to DefaultTemplate. The set of allowed values comes
	// from Templates().
	Template string
	// TargetDir is the directory the four files are written into.
	// Empty defaults to the current working directory. The directory
	// is created (`os.MkdirAll`) if it does not exist; an existing
	// directory is OK as long as none of the four target files
	// already exist within it.
	TargetDir string
}

// Result reports what Init wrote. Files are absolute paths in
// deterministic (lexicographic) order.
type Result struct {
	Name      string   `json:"name"`
	TargetDir string   `json:"target_dir"`
	Files     []string `json:"files"`
}

// templatesFS bundles the embedded template tree.
//
//go:embed templates/*
var templatesFS embed.FS

// projectNameRE pins the validateName regex — same shape as
// `cmd/harbor/scaffold` for cross-tool consistency: a single
// lowercase alphanumeric / dash / underscore identifier.
var projectNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// validateName enforces the name shape. Mirrors
// `cmd/harbor/scaffold/scaffold.go::validateName`.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalidName)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%w: name must not contain path separators (got %q)", ErrInvalidName, name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: name must not contain parent-directory tokens (got %q)", ErrInvalidName, name)
	}
	if !projectNameRE.MatchString(name) {
		return fmt.Errorf("%w: name must match %s (got %q)", ErrInvalidName, projectNameRE.String(), name)
	}
	return nil
}

// Templates returns the sorted list of registered template names.
func Templates() []string {
	entries, err := fs.ReadDir(templatesFS, "templates")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// templateExists reports whether name is a registered template.
func templateExists(name string) bool {
	for _, t := range Templates() {
		if t == name {
			return true
		}
	}
	return false
}

// Init materialises the four templated files into opts.TargetDir.
//
// Validation order (fail-loud, no partial writes):
//
//  1. opts.Template (empty → DefaultTemplate) must be a registered
//     template.
//  2. opts.TargetDir is resolved via filepath.Abs and created if
//     absent.
//  3. opts.Name is normalised: explicit value validates via
//     validateName; empty value falls back to the basename, and a
//     non-validating basename falls back to the literal "agent".
//  4. Each of the four target files MUST NOT already exist; the
//     first collision returns ErrFileExists naming the path.
//
// On success Result.Files is the sorted list of ABSOLUTE paths
// written.
func Init(opts Options) (Result, error) {
	tmpl := opts.Template
	if tmpl == "" {
		tmpl = DefaultTemplate
	}
	if !templateExists(tmpl) {
		return Result{}, fmt.Errorf("%w: %q (known: %s)",
			ErrUnknownTemplate, tmpl, strings.Join(Templates(), ","))
	}
	target := opts.TargetDir
	if target == "" {
		target = "."
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return Result{}, fmt.Errorf("%w: resolve target_dir: %w", ErrInitFailed, err)
	}
	if mkErr := os.MkdirAll(absTarget, 0o755); mkErr != nil {
		return Result{}, fmt.Errorf("%w: mkdir %s: %w", ErrInitFailed, absTarget, mkErr)
	}

	name := opts.Name
	if name == "" {
		base := filepath.Base(absTarget)
		if err := validateName(base); err == nil {
			name = base
		} else {
			name = "agent"
		}
	} else if err := validateName(name); err != nil {
		return Result{}, err
	}

	// Enumerate the template directory deterministically so the
	// fail-loud collision check fires on the lexicographically-first
	// file the operator already has — the error message is then
	// reproducible across runs.
	templateRoot := filepath.Join("templates", tmpl)
	templatedFiles, err := listTemplateFiles(templateRoot)
	if err != nil {
		return Result{}, fmt.Errorf("%w: enumerate template: %w", ErrInitFailed, err)
	}

	// Pre-flight collision check — refuse to overwrite ANY of the
	// four files. Better to fail before writing the first byte than
	// to leave a half-initialised tree behind.
	plannedTargets := make(map[string]string, len(templatedFiles))
	rootWithSlash := templateRoot + "/"
	for _, tf := range templatedFiles {
		// Strip the `templates/<name>/` embed prefix so the output
		// path is rooted at the operator's target, not at the embed
		// tree's notion of "where the file lives in the binary".
		rel := strings.TrimPrefix(tf, rootWithSlash)
		outRel := strings.TrimSuffix(rel, ".tmpl")
		outAbs := filepath.Join(absTarget, outRel)
		if _, statErr := os.Stat(outAbs); statErr == nil {
			return Result{}, fmt.Errorf("%w: %s", ErrFileExists, outAbs)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("%w: stat %s: %w", ErrInitFailed, outAbs, statErr)
		}
		plannedTargets[tf] = outAbs
	}

	// Render + write. Each template is a standalone text/template;
	// the only variable is {{.Name}}.
	written := make([]string, 0, len(templatedFiles))
	for _, tf := range templatedFiles {
		raw, readErr := fs.ReadFile(templatesFS, tf)
		if readErr != nil {
			return Result{}, fmt.Errorf("%w: read embedded %s: %w", ErrInitFailed, tf, readErr)
		}
		tpl, parseErr := template.New(tf).Parse(string(raw))
		if parseErr != nil {
			return Result{}, fmt.Errorf("%w: parse %s: %w", ErrInitFailed, tf, parseErr)
		}
		outAbs := plannedTargets[tf]
		f, openErr := os.OpenFile(outAbs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if openErr != nil {
			return Result{}, fmt.Errorf("%w: open %s: %w", ErrInitFailed, outAbs, openErr)
		}
		execErr := tpl.Execute(f, struct{ Name string }{Name: name})
		closeErr := f.Close()
		if execErr != nil {
			return Result{}, fmt.Errorf("%w: execute %s: %w", ErrInitFailed, tf, execErr)
		}
		if closeErr != nil {
			return Result{}, fmt.Errorf("%w: close %s: %w", ErrInitFailed, outAbs, closeErr)
		}
		written = append(written, outAbs)
	}
	sort.Strings(written)
	return Result{Name: name, TargetDir: absTarget, Files: written}, nil
}

// listTemplateFiles walks the embedded `templates/<name>/`
// directory and returns every file path (sorted) so Render iterates
// deterministically.
func listTemplateFiles(root string) ([]string, error) {
	var out []string
	walkErr := fs.WalkDir(templatesFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(out)
	return out, nil
}
