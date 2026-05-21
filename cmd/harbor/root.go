// cmd/harbor/root.go — the cobra root command + global flag wiring.
//
// Phase 63 (RFC §8, D-084) makes `harbor` a cobra-rooted CLI. The root
// command owns the two global flags `--quiet` and `--json`; every
// subcommand inherits them. Subcommands return their CLIError via cobra
// RunE; the root's RunE / PersistentPostRunE wiring funnels every error
// through PrintCLIError so JSON-mode round-trips with the wire shape
// pinned in D-084.
//
// NOTE: cmd/harbor does not import internal/protocol/errors — the CLI
// structured-error surface is distinct from the Protocol wire-error
// surface (operator-facing exit codes vs. Protocol-client responses).
// See CLAUDE.md §8 and errors.go.

package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// HarborVersion is the binary's own semver. Pinned at "v0.0.0-dev" at
// Phase 63; a later release-engineering phase (likely Phase 78) injects
// a real semver via `-ldflags`. The CLI surface is forward-compatible —
// `harbor version` always prints the value of this constant.
const HarborVersion = "v0.0.0-dev"

// flagJSON is the global `--json` flag name; declared as a constant so
// subcommands and tests reference one canonical spelling.
const flagJSON = "json"

// flagQuiet is the global `--quiet` flag name; ditto.
const flagQuiet = "quiet"

// NewRootCmd constructs and returns a fresh root command tree. It is
// invoked once by main() per process, but tests call it per-test to
// exercise the command tree in isolation (cobra commands carry mutable
// flag state through Execute(), so sharing a root across tests is a
// bug). The returned tree includes every Phase 63 subcommand.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "harbor",
		Short: "Harbor — Go-native agent runtime SDK CLI",
		Long: `harbor is the command-line entry point for the Harbor agent runtime SDK.

Subcommands fall into three groups:

  Local dev loop      dev, console, scaffold, validate
  Run inspection      inspect-events, inspect-runs, inspect-topology
  Build information   version

Subcommands without a real implementation yet stub-fail with a
structured error pointing to the phase that will implement them. See
RFC-001-Harbor.md §8 for the settled subcommand surface and
docs/plans/README.md for the implementation schedule.`,
		// SilenceUsage / SilenceErrors hand error printing to the
		// PersistentPostRunE hook below so the structured-error
		// shape goes through PrintCLIError (CLAUDE.md §5 "fail
		// loudly" + D-084).
		SilenceUsage:  true,
		SilenceErrors: true,
		// Disable cobra's default completion subcommand to keep the
		// help golden compact and the surface intentional. Phase 78
		// (release engineering) can re-enable when it ships shell
		// completion docs.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}

	// Global flags — every subcommand inherits these.
	root.PersistentFlags().Bool(flagQuiet, false, "suppress informational output (errors still emit)")
	root.PersistentFlags().Bool(flagJSON, false, "emit machine-readable JSON output instead of human-readable text")

	// Bind subcommands. One per file for readability + git-history
	// locality when later phases populate the stub bodies.
	root.AddCommand(newVersionCmd())
	root.AddCommand(newDevCmd())
	root.AddCommand(newConsoleCmd())
	root.AddCommand(newScaffoldCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newInspectEventsCmd())
	root.AddCommand(newInspectRunsCmd())
	root.AddCommand(newInspectTopologyCmd())

	return root
}

// resolveJSONMode reads the inherited --json flag value off the
// command. Returns false if the flag is not registered on this command
// tree (defensive — every command Phase 63 ships inherits it).
func resolveJSONMode(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool(flagJSON)
	if err != nil {
		return false
	}
	return v
}

// resolveQuietMode reads the inherited --quiet flag value off the
// command. Returns false if the flag is not registered.
func resolveQuietMode(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool(flagQuiet)
	if err != nil {
		return false
	}
	return v
}

// asCLIError unwraps err into a CLIError, returning ok=false if err is
// not a CLIError. A subcommand that returns a non-CLIError from its
// RunE is wrapped here so the structured-error surface stays uniform.
func asCLIError(err error) (CLIError, bool) {
	var cli CLIError
	if errors.As(err, &cli) {
		return cli, true
	}
	return CLIError{}, false
}

// emitCLIError is the hook every subcommand's stub body calls. It
// resolves the json/quiet flags, writes the structured error to
// cmd.ErrOrStderr(), and returns a sentinel error so cobra's Execute()
// reports a non-zero exit code without printing anything else (we set
// SilenceErrors on the root). The returned error wraps the CLIError so
// callers / tests can still errors.As() back to it.
//
// The quietMode flag is currently a no-op for error output — errors
// always emit. quietMode reaching here would only suppress
// informational output (none in Phase 63's stub bodies). The hook
// observes the flag so the contract is wired through end-to-end and
// future subcommand bodies inherit it.
func emitCLIError(cmd *cobra.Command, cliErr CLIError) error {
	_ = resolveQuietMode(cmd) // flag is wired through; no info output in Phase 63
	if writeErr := PrintCLIError(cmd.ErrOrStderr(), resolveJSONMode(cmd), cliErr); writeErr != nil {
		// Fail loudly — a write error on stderr is a system-level
		// problem, surface it. This will reach cobra's
		// (silenced) error path but at least propagates a non-nil
		// up the stack so Execute returns non-zero.
		return fmt.Errorf("emit cli error: %w (original: %w)", writeErr, cliErr)
	}
	return cliErr
}
