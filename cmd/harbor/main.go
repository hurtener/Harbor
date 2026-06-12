// Command harbor is the Harbor binary entry point.
//
// The CLI skeleton (RFC §8) turns this from a driver-registration stub
// into a cobra-rooted CLI. The production driver set self-registers
// via the `internal/drivers/prod` aggregator blank import below (so
// the audit / events / state / artifacts / memory / skills / tools /
// telemetry / llm / governance / distributed factories resolve when
// something Opens them — the §4.4 seam pattern; the per-driver imports
// were collapsed into the aggregator by). The cobra
// command tree lives in root.go (NewRootCmd) and is executed here.
package main

import (
	"fmt"
	"os"

	// Production driver aggregator — the single
	// sanctioned home of the driver blank-import block (§4.4). Its
	// init chain seats every production driver factory, the LLM
	// wrapper hooks (corrections / downgrade / retry / governance),
	// and the notifications event-type registration. New drivers are
	// added THERE, not here. The dev-only mock LLM driver is NOT part
	// of the set — `harbor dev` conditionally imports it behind
	// HARBOR_DEV_ALLOW_MOCK=1 (see cmd/harbor/devmock.go).
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
)

func main() {
	root := NewRootCmd()
	err := root.Execute()
	if err == nil {
		return
	}
	// Cobra has already routed a CLIError through the subcommand's
	// RunE → emitCLIError → PrintCLIError chain (SilenceErrors on the
	// root suppresses cobra's own printing). Defensive fallback: if
	// the error is NOT a CLIError (e.g. flag-parse failure before any
	// RunE ran), surface a structured error here so the operator
	// never sees a bare cobra trace. Fail loudly per CLAUDE.md §5.
	cli, ok := asCLIError(err)
	if !ok {
		fallback := CLIError{
			Message: fmt.Sprintf("invocation error: %s", err.Error()),
			Code:    "invocation_error",
		}
		//nolint:errcheck // last-ditch error print before exit; if stderr is unwritable there is nothing left to do
		_ = PrintCLIError(os.Stderr, false, fallback)
		os.Exit(1)
	}
	os.Exit(exitCodeFor(cli))
}

// exitCodeFor maps a CLIError.Code to the binary's exit code. An earlier phase
// introduced the distinction between "validation found issues" (exit 1)
// and "unexpected / internal error" (exit 2). All other codes
// (`not_implemented`, `invocation_error`, ...) collapse to exit 1.
// The mapping is centralised here so future codes pick a deliberate
// exit slot rather than inheriting "1" by accident.
func exitCodeFor(cli CLIError) int {
	switch cli.Code {
	case CodeValidationInternal:
		return 2
	default:
		return 1
	}
}
