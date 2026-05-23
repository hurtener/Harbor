package scaffold

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
)

// DefaultTemplate is the template `harbor scaffold` selects when the
// operator omits --template. Phase 67 ships only minimal-react.
const DefaultTemplate = "minimal-react"

// Sentinel errors. Callers (cmd/harbor/cmd_scaffold.go) compare via
// errors.Is and map onto CLIError{Code} values.
var (
	// ErrInvalidName signals Options.Name failed validateName. The
	// wrapped message names the offending name + the rule that
	// rejected it.
	ErrInvalidName = errors.New("scaffold: invalid project name")
	// ErrOutputDirExists signals Options.OutputDir already exists.
	// Scaffold refuses to overwrite — operators delete the directory
	// or pick a fresh path. This is the §13 fail-loud posture for an
	// operator-facing seam.
	ErrOutputDirExists = errors.New("scaffold: output directory already exists")
	// ErrUnknownTemplate signals Options.Template is not in Templates().
	// The wrapped message lists every known template.
	ErrUnknownTemplate = errors.New("scaffold: unknown template")
	// ErrRender signals a template execution or filesystem write
	// failed. The wrapped message names the offending file path.
	ErrRender = errors.New("scaffold: render failed")
	// ErrUpstreamConfigInvalid signals the operator-supplied yaml at
	// FromConfigPath failed to load or validate (Phase 83o / D-154).
	// Wraps the underlying `internal/config.ErrConfigInvalid`.
	ErrUpstreamConfigInvalid = errors.New("scaffold: upstream harbor.yaml is invalid")
)

// Options is the input to Scaffold.
type Options struct {
	// Name is the project name. Required. Used as the rendered
	// `go.mod` module name's last component, the `harbor.yaml`
	// service-name, and the README title.
	Name string
	// Template selects which embedded template tree to render. Empty
	// defaults to DefaultTemplate. The set of allowed values comes
	// from Templates().
	Template string
	// OutputDir is the directory Scaffold creates and writes the
	// rendered project into. Required. Without `Patch`, the path is
	// resolved with `filepath.Abs` and MUST NOT exist on disk; with
	// `Patch`, an existing directory is accepted and existing files
	// are skipped.
	OutputDir string
	// FromConfigPath is the optional path to an operator-edited
	// `harbor.yaml` (Phase 83o / D-154). When set, Scaffold loads +
	// validates it and uses its `tools.custom[]` entries to generate
	// per-tool Go stubs under `OutputDir/tools/`. Empty + no
	// `./harbor.yaml` in the cwd ⇒ Scaffold falls back to the
	// template-only behavior (the existing scaffold-without-init
	// path stays valid).
	FromConfigPath string
	// Patch relaxes the refuse-overwrite default (Phase 83o / D-154).
	// When true, an existing OutputDir is accepted and Scaffold writes
	// only files that do NOT already exist; existing files are
	// skipped and listed under `Result.Skipped`. The operator-edit
	// survival invariant.
	Patch bool
}

// Result reports what Scaffold wrote. Files are paths RELATIVE to
// OutputDir, in deterministic (lexicographic) order — a smoke script
// or scripted consumer can rely on the ordering.
//
// Phase 83o / D-154: `Skipped` lists files that were already on disk
// at scaffold time and were therefore NOT overwritten. Only populated
// when `Options.Patch == true`; non-patch runs reject existing output
// dirs outright.
type Result struct {
	Name      string   `json:"name"`
	OutputDir string   `json:"output_dir"`
	Files     []string `json:"files"`
	Skipped   []string `json:"skipped,omitempty"`
}

// templatesFS bundles the embedded template tree into the binary at
// compile time. The embed glob captures every file under templates/.
//
//go:embed templates/*
var templatesFS embed.FS

// projectNameRE pins the validateName regex: a single lowercase
// alphanumeric / dash / underscore identifier, 1–64 chars, starting
// with a letter or digit. Operators who want richer names should
// rename after scaffolding.
var projectNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// validateName enforces the name shape. It rejects: empty strings,
// names with path separators (`/`, `\`), parent-dir tokens (`..`),
// names starting with `-` or `_`, and names containing whitespace or
// uppercase. The rejection message wraps ErrInvalidName so callers can
// errors.Is.
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

// Templates returns the deterministic-order list of registered
// template names. Phase 67 ships exactly one entry.
func Templates() []string {
	entries, err := fs.ReadDir(templatesFS, "templates")
	if err != nil {
		// embed guarantees the directory exists at compile time; a
		// runtime read failure is "impossible by construction" per
		// CLAUDE.md §5. Return an empty slice rather than panic — the
		// caller (Scaffold) will produce ErrUnknownTemplate when an
		// operator-supplied template doesn't match.
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

// Scaffold materialises the named template at opts.OutputDir.
//
// Validation order (fail-loud, no partial writes):
//
//  1. opts.Name is validated via validateName.
//  2. opts.Template (empty → DefaultTemplate) must be a registered
//     template.
//  3. opts.OutputDir is resolved via filepath.Abs and (in default
//     mode) MUST NOT exist. With `opts.Patch == true`, an existing
//     directory is accepted; existing files are skipped.
//
// Upstream config (Phase 83o / D-154): when `opts.FromConfigPath` is
// set OR `./harbor.yaml` exists in the cwd, Scaffold loads + validates
// it via `internal/config.Load`. Its `tools.custom[]` entries drive
// per-tool stub generation; the yaml file itself is copied verbatim
// into OutputDir/harbor.yaml (overriding the template-rendered yaml).
//
// On success, every template file is rendered through text/template
// and written to OutputDir/<rel-path>. The returned Result.Files is
// the lexicographic-order list of relative paths written;
// Result.Skipped lists files left untouched in patch mode.
//
// On any failure during a NON-patch run after the output dir has
// been created, Scaffold removes the dir before returning so the
// operator never sees a half-scaffolded project. Patch runs do not
// roll back — the operator's pre-existing files are sacred.
func Scaffold(opts Options) (Result, error) {
	if err := validateName(opts.Name); err != nil {
		return Result{}, err
	}
	tmpl := opts.Template
	if tmpl == "" {
		tmpl = DefaultTemplate
	}
	if !templateExists(tmpl) {
		return Result{}, fmt.Errorf("%w: %q (known: %s)",
			ErrUnknownTemplate, tmpl, strings.Join(Templates(), ","))
	}
	if opts.OutputDir == "" {
		return Result{}, fmt.Errorf("%w: output_dir must not be empty", ErrRender)
	}
	absOut, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return Result{}, fmt.Errorf("%w: resolve output_dir: %w", ErrRender, err)
	}
	// Output-dir existence check. Without `Patch`, an existing dir
	// fails closed (operator deletes it / picks a fresh path).
	// With `Patch`, an existing dir is OK; a missing dir is also OK
	// (Scaffold creates it).
	if !opts.Patch {
		if _, statErr := os.Stat(absOut); statErr == nil {
			return Result{}, fmt.Errorf("%w: %s", ErrOutputDirExists, absOut)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("%w: stat output_dir: %w", ErrRender, statErr)
		}
	}
	// Resolve the upstream config (Phase 83o / D-154). An explicit
	// path is honoured verbatim; an empty path auto-detects
	// `./harbor.yaml` and silently falls through to template-only
	// when neither resolves.
	upstreamPath, upstreamCfg, loadErr := loadUpstreamConfig(opts.FromConfigPath)
	if loadErr != nil {
		return Result{}, loadErr
	}
	files, skipped, err := renderProject(tmpl, opts, absOut, upstreamPath, upstreamCfg)
	if err != nil {
		// Best-effort cleanup so the operator never sees a half-
		// scaffolded tree. Cleanup ONLY runs on non-patch runs (a
		// patch run touched only NEW files; rolling back would also
		// nuke operator-edited siblings that pre-existed).
		if !opts.Patch {
			if rmErr := removeAll(absOut); rmErr != nil {
				return Result{}, fmt.Errorf("%w (also failed to clean up partial output: %w)", err, rmErr)
			}
		}
		return Result{}, err
	}
	return Result{
		Name:      opts.Name,
		OutputDir: absOut,
		Files:     files,
		Skipped:   skipped,
	}, nil
}
