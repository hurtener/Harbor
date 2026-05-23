// cmd/harbor/cmd_init.go — `harbor init` subcommand.
//
// Phase 83n / D-153. Drops a tiered, commented `harbor.yaml` plus
// `AGENTS.md`, `CLAUDE.md`, and `README.md` into the operator's
// chosen target directory. The companion files explain the next
// steps (edit yaml → validate → scaffold → dev).
//
// Every failure path emits a structured CLIError (D-084) — no
// hand-rolled JSON, no silent partial writes.

package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	harborinit "github.com/hurtener/Harbor/cmd/harbor/init"
)

// Stable CLI error codes for `harbor init`. Each value is a fixed
// string a smoke test asserts against (D-084 wire contract).
const (
	// CodeInitFileExists fires when one of the four target files
	// already exists in the target dir. Operators delete the file or
	// pick a fresh --target; init refuses to overwrite (§13 fail-loud
	// posture).
	CodeInitFileExists = "init_file_exists"
	// CodeInitInvalidProjectName fires when --name fails validateName.
	CodeInitInvalidProjectName = "init_invalid_project_name"
	// CodeInitUnknownTemplate fires when --template is not in
	// harborinit.Templates().
	CodeInitUnknownTemplate = "init_unknown_template"
	// CodeInitFailed is the catch-all for rendering / write failures.
	CodeInitFailed = "init_failed"
)

// Flag names declared as constants so subcommand body, tests, and the
// help golden reference one spelling.
const (
	flagInitName     = "name"
	flagInitTemplate = "template"
	flagInitTarget   = "target"
)

// initJSONResult is the wire shape `harbor init --json` emits on
// success. Pinned by D-153.
type initJSONResult struct {
	Name      string   `json:"name"`
	TargetDir string   `json:"target_dir"`
	Files     []string `json:"files"`
}

// newInitCmd builds the `init` cobra subcommand. Flags:
//
//	--name (optional)     — agent name (defaults to target-dir basename)
//	--template (optional) — defaults to harborinit.DefaultTemplate
//	--target (optional)   — target directory (defaults to ".")
func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "drop a tiered harbor.yaml + AGENTS.md/CLAUDE.md/README.md into a directory",
		Long: `Drop the four operator-editable files that bootstrap a Harbor agent:

  harbor.yaml    Tiered configuration (REQUIRED / COMMON / ADVANCED).
  AGENTS.md      Binding rules for AI assistants editing the agent.
  CLAUDE.md      Verbatim mirror of AGENTS.md (Claude Code auto-loads it).
  README.md      Operator-facing walkthrough.

The yaml ships with placeholder identity values, four commented LLM-provider
example blocks (OpenRouter / Anthropic / OpenAI / NVIDIA NIM), and pointers
to docs/CONFIG.md for the full knob reference. Edit, validate, then run
` + "`harbor scaffold`" + ` to materialise the Go project.

Init refuses to overwrite existing files; delete the file or pick a fresh
--target.

Examples:
  harbor init
  harbor init --name my-agent
  harbor init --name my-agent --target ./projects/my-agent --json`,
		Args: cobra.NoArgs,
		RunE: runInit,
	}
	cmd.Flags().String(flagInitName, "",
		"agent name (optional; defaults to the target directory's basename or \"agent\")")
	cmd.Flags().String(flagInitTemplate, harborinit.DefaultTemplate,
		"init template to render (V1.1 ships only \""+harborinit.DefaultTemplate+"\")")
	cmd.Flags().String(flagInitTarget, "",
		"target directory (defaults to the current working directory; created if absent)")
	return cmd
}

// runInit is the cobra RunE for `harbor init`.
func runInit(cmd *cobra.Command, _ []string) error {
	// Every flag below is statically registered on this command, so the
	// GetString lookups cannot fail; the blank-error discards are intentional.
	name, _ := cmd.Flags().GetString(flagInitName)     //nolint:errcheck // flag statically registered; lookup cannot fail
	tmpl, _ := cmd.Flags().GetString(flagInitTemplate) //nolint:errcheck // flag statically registered; lookup cannot fail
	target, _ := cmd.Flags().GetString(flagInitTarget) //nolint:errcheck // flag statically registered; lookup cannot fail
	result, err := harborinit.Init(harborinit.Options{
		Name:      name,
		Template:  tmpl,
		TargetDir: target,
	})
	if err != nil {
		return emitCLIError(cmd, initErrorToCLIError(err))
	}
	jsonMode := resolveJSONMode(cmd)
	quietMode := resolveQuietMode(cmd)
	if jsonMode {
		return writeInitJSON(cmd, result)
	}
	if !quietMode {
		writeInitHuman(cmd, result)
	}
	return nil
}

// initErrorToCLIError maps a harborinit sentinel onto the matching
// CLIError code.
func initErrorToCLIError(err error) CLIError {
	switch {
	case errors.Is(err, harborinit.ErrFileExists):
		return CLIError{
			Subcommand: "init",
			Message:    err.Error(),
			Code:       CodeInitFileExists,
			Hint:       "delete the existing file (or pick a different --target) and re-run",
		}
	case errors.Is(err, harborinit.ErrInvalidName):
		return CLIError{
			Subcommand: "init",
			Message:    err.Error(),
			Code:       CodeInitInvalidProjectName,
			Hint:       "name must match [a-z0-9][a-z0-9_-]{0,63}",
		}
	case errors.Is(err, harborinit.ErrUnknownTemplate):
		return CLIError{
			Subcommand: "init",
			Message:    err.Error(),
			Code:       CodeInitUnknownTemplate,
			Hint:       "see harbor init --help for the list of templates (V1.1 ships only \"" + harborinit.DefaultTemplate + "\")",
		}
	default:
		return CLIError{
			Subcommand: "init",
			Message:    err.Error(),
			Code:       CodeInitFailed,
			Hint:       "see the wrapped error chain for the failing file path",
		}
	}
}

// writeInitJSON emits the single-line success JSON on stdout.
func writeInitJSON(cmd *cobra.Command, r harborinit.Result) error {
	buf, err := json.Marshal(initJSONResult{
		Name:      r.Name,
		TargetDir: r.TargetDir,
		Files:     r.Files,
	})
	if err != nil {
		// Marshal of three string fields + a []string is "impossible
		// by construction" per CLAUDE.md §5 — but fail loudly anyway.
		return fmt.Errorf("init: marshal result: %w", err)
	}
	buf = append(buf, '\n')
	if _, err := cmd.OutOrStdout().Write(buf); err != nil {
		return fmt.Errorf("init: write result: %w", err)
	}
	return nil
}

// writeInitHuman emits the multi-line success summary on stdout. The
// format is stable; smoke scripts grep for line prefixes.
func writeInitHuman(cmd *cobra.Command, r harborinit.Result) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "initialised %q at %s\n", r.Name, r.TargetDir)
	_, _ = fmt.Fprintln(out, "files:")
	for _, f := range r.Files {
		_, _ = fmt.Fprintf(out, "  %s\n", f)
	}
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out, "Next steps:")
	_, _ = fmt.Fprintln(out, "  1. Edit harbor.yaml — uncomment one LLM provider block + set its API key env var.")
	_, _ = fmt.Fprintln(out, "  2. harbor validate ./harbor.yaml")
	_, _ = fmt.Fprintf(out, "  3. harbor scaffold --name %s\n", r.Name)
	_, _ = fmt.Fprintln(out, "  4. harbor dev --config ./harbor.yaml")
}
