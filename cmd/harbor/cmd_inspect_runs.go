// cmd/harbor/cmd_inspect_runs.go — `harbor inspect-runs` stub.
//
// Phase 69 (per docs/plans/README.md) populates this — list recent
// runs; show a run's trajectory.

package main

import "github.com/spf13/cobra"

func newInspectRunsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect-runs",
		Short: "list recent runs and inspect a run's trajectory (Phase 69)",
		Long: `List recent runs visible on the connected Harbor Runtime and inspect
an individual run's trajectory (the Phase 43 ReAct trajectory shape).

This subcommand is registered but not yet implemented; the body lands
in phase 69.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return emitCLIError(cmd, CLIError{
				Subcommand: "inspect-runs",
				Message:    "not yet implemented",
				Code:       CodeNotImplemented,
				Hint:       "see phase 69 — harbor inspect-events / inspect-runs",
			})
		},
	}
	return cmd
}
