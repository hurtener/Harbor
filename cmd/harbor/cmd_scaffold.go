// cmd/harbor/cmd_scaffold.go — `harbor scaffold` subcommand.
//
// Phase 67 (RFC §8, D-087) replaces the Phase 63 stub with the real
// scaffold implementation. The subcommand materialises a new Harbor
// agent project skeleton from an embedded template (default
// "minimal-react") into the operator-supplied output directory.
//
// The rendered project ships a production-shaped harbor.yaml that
// passes `internal/config.Load + Validate` with zero further edits —
// the in-PR stand-in for `harbor validate` (sibling-shipping in
// Phase 68; the CLI integration lands in Phase 68's PR per
// CLAUDE.md §17.6).
//
// Every failure path emits a structured CLIError (D-084) — no
// hand-rolled JSON, no silent partial writes.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/cmd/harbor/scaffold"
)

// Stable CLI error codes for `harbor scaffold`. Each value is a fixed
// string a smoke test asserts against (D-084 wire contract). New
// subcommands ADD codes to errors.go; subcommand-local codes (these)
// live next to the subcommand body for grep locality.
const (
	// CodeOutputDirExists fires when --output names an existing
	// directory. Operators delete the directory or pick a fresh path;
	// scaffold refuses to overwrite (§13 fail-loud posture).
	CodeOutputDirExists = "output_dir_exists"
	// CodeInvalidProjectName fires when --name fails validateName
	// (empty, path separators, parent-dir tokens, leading dash, or
	// non-`[a-z0-9_-]` chars).
	CodeInvalidProjectName = "invalid_project_name"
	// CodeUnknownTemplate fires when --template is not in
	// scaffold.Templates(). The hint lists the known templates.
	CodeUnknownTemplate = "unknown_template"
	// CodeScaffoldFailed is the catch-all for rendering / write
	// failures. The hint carries the wrapped engine error so an
	// operator can grep the offending file path.
	CodeScaffoldFailed = "scaffold_failed"
)

// Flag names declared as constants so subcommand body, tests, and the
// help golden reference one spelling.
const (
	flagScaffoldName     = "name"
	flagScaffoldTemplate = "template"
	flagScaffoldOutput   = "output"
)

// scaffoldJSONResult is the wire shape `harbor scaffold --json` emits
// on success. Field names are pinned by D-087.
type scaffoldJSONResult struct {
	Name      string   `json:"name"`
	OutputDir string   `json:"output_dir"`
	Files     []string `json:"files"`
}

// newScaffoldCmd builds the `scaffold` cobra subcommand. Flags:
//
//	--name (required)     — the project name
//	--template (optional) — defaults to scaffold.DefaultTemplate
//	--output (optional)   — defaults to `./<name>` (resolved at run time)
func newScaffoldCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scaffold",
		Short: "generate a new agent skeleton from a template",
		Long: `Materialise a new Harbor agent project skeleton from an embedded
template. The default template (` + scaffold.DefaultTemplate + `) ships a
production-shaped harbor.yaml, a go.mod, a worked example Agent, and a
harbortest-driven test.

The rendered config passes Harbor's config validator (` + "`harbor validate`" + `
when Phase 68 ships; ` + "`internal/config.Load + Validate`" + ` directly today)
with zero further edits.

Examples:
  harbor scaffold --name my-agent
  harbor scaffold --name my-agent --output ./projects/my-agent
  harbor scaffold --name my-agent --template ` + scaffold.DefaultTemplate + ` --json`,
		Args: cobra.NoArgs,
		RunE: runScaffold,
	}
	cmd.Flags().String(flagScaffoldName, "", "project name (required; lowercase alphanumeric / dash / underscore)")
	cmd.Flags().String(flagScaffoldTemplate, scaffold.DefaultTemplate,
		"template to render (known: "+strings.Join(scaffold.Templates(), ",")+")")
	cmd.Flags().String(flagScaffoldOutput, "",
		"output directory (defaults to ./<name>); MUST NOT already exist")
	return cmd
}

// runScaffold is the cobra RunE for `harbor scaffold`. It reads the
// three flags, calls scaffold.Scaffold, and routes any error through
// emitCLIError so the structured-error shape is uniform.
func runScaffold(cmd *cobra.Command, _ []string) error {
	// Every flag below is statically registered on this command, so the
	// GetString lookups cannot fail; the blank-error discards are intentional.
	name, _ := cmd.Flags().GetString(flagScaffoldName)     //nolint:errcheck // flag statically registered; lookup cannot fail
	tmpl, _ := cmd.Flags().GetString(flagScaffoldTemplate) //nolint:errcheck // flag statically registered; lookup cannot fail
	outDir, _ := cmd.Flags().GetString(flagScaffoldOutput) //nolint:errcheck // flag statically registered; lookup cannot fail
	if outDir == "" {
		// Default to ./<name>. validateName (inside Scaffold) will
		// reject an empty/invalid name with ErrInvalidName, which
		// maps to CodeInvalidProjectName — so an empty outDir
		// downstream of an empty name is fine; the name error fires
		// first.
		outDir = name
	}
	result, err := scaffold.Scaffold(scaffold.Options{
		Name:      name,
		Template:  tmpl,
		OutputDir: outDir,
	})
	if err != nil {
		return emitCLIError(cmd, scaffoldErrorToCLIError(err))
	}
	jsonMode := resolveJSONMode(cmd)
	quietMode := resolveQuietMode(cmd)
	if jsonMode {
		return writeScaffoldJSON(cmd, result)
	}
	if !quietMode {
		writeScaffoldHuman(cmd, result)
	}
	return nil
}

// scaffoldErrorToCLIError maps a scaffold engine sentinel onto the
// matching CLIError. An unknown error wraps under CodeScaffoldFailed
// with the engine error chain surfaced in the hint (operators get an
// actionable filename when a render fails).
func scaffoldErrorToCLIError(err error) CLIError {
	switch {
	case errors.Is(err, scaffold.ErrInvalidName):
		return CLIError{
			Subcommand: "scaffold",
			Message:    err.Error(),
			Code:       CodeInvalidProjectName,
			Hint:       "name must match [a-z0-9][a-z0-9_-]{0,63}",
		}
	case errors.Is(err, scaffold.ErrOutputDirExists):
		return CLIError{
			Subcommand: "scaffold",
			Message:    err.Error(),
			Code:       CodeOutputDirExists,
			Hint:       "delete the existing directory or pick a different --output",
		}
	case errors.Is(err, scaffold.ErrUnknownTemplate):
		return CLIError{
			Subcommand: "scaffold",
			Message:    err.Error(),
			Code:       CodeUnknownTemplate,
			Hint:       "known templates: " + strings.Join(scaffold.Templates(), ","),
		}
	default:
		return CLIError{
			Subcommand: "scaffold",
			Message:    err.Error(),
			Code:       CodeScaffoldFailed,
			Hint:       "see the wrapped error chain for the failing file path",
		}
	}
}

// writeScaffoldJSON emits the single-line success JSON object on stdout.
func writeScaffoldJSON(cmd *cobra.Command, r scaffold.Result) error {
	buf, err := json.Marshal(scaffoldJSONResult{
		Name:      r.Name,
		OutputDir: r.OutputDir,
		Files:     r.Files,
	})
	if err != nil {
		// Marshal of three string fields + a []string is "impossible
		// by construction" per CLAUDE.md §5 — but fail loudly anyway.
		return fmt.Errorf("scaffold: marshal result: %w", err)
	}
	buf = append(buf, '\n')
	if _, err := cmd.OutOrStdout().Write(buf); err != nil {
		return fmt.Errorf("scaffold: write result: %w", err)
	}
	return nil
}

// writeScaffoldHuman emits the multi-line success summary on stdout.
// The format is stable; smoke scripts grep for the line prefixes.
func writeScaffoldHuman(cmd *cobra.Command, r scaffold.Result) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "scaffolded %q at %s\n", r.Name, r.OutputDir)
	_, _ = fmt.Fprintln(out, "files:")
	for _, f := range r.Files {
		_, _ = fmt.Fprintf(out, "  %s\n", f)
	}
}
