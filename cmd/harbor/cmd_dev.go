// cmd/harbor/cmd_dev.go — `harbor dev` stub.
//
// Phase 63 ships the subcommand registration; Phase 64 (per the
// pre-plan note in docs/plans/README.md) populates the body — boot
// the embedded Runtime, open the Phase 60 Protocol transports onto a
// real net.Listener, and serve `/healthz`. The stub exits non-zero
// with CLIError{Code: CodeNotImplemented} so scripts get the §13
// fail-loud signal.

package main

import "github.com/spf13/cobra"

func newDevCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "boot the local Runtime + Protocol server (Phase 64)",
		Long: `Boot a local Harbor Runtime, open the Phase 60 Protocol transports
onto a 127.0.0.1 listener, and (later) embed the Console.

This subcommand is registered but not yet implemented. The behaviour
above lands in phase 64 — see docs/plans/README.md and the Phase 64
pre-plan note for the binding constraints (LLM-default flip, dev-only
escape hatch, identity injection, /healthz).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return emitCLIError(cmd, CLIError{
				Subcommand: "dev",
				Message:    "not yet implemented",
				Code:       CodeNotImplemented,
				Hint:       "see phase 64 — harbor dev v1",
			})
		},
	}
	return cmd
}
