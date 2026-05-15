// cmd/harbor/cmd_inspect_events.go — `harbor inspect-events` stub.
//
// Phase 69 (per docs/plans/README.md) populates this — tail or filter
// the event bus of a running Runtime.

package main

import "github.com/spf13/cobra"

func newInspectEventsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect-events",
		Short: "tail or filter the event bus of a running Runtime (Phase 69)",
		Long: `Open the Phase 60 SSE event stream against a running Harbor Runtime
and emit each frame. Supports identity-scoped filtering and replay
from a cursor.

This subcommand is registered but not yet implemented; the body lands
in phase 69.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return emitCLIError(cmd, CLIError{
				Subcommand: "inspect-events",
				Message:    "not yet implemented",
				Code:       CodeNotImplemented,
				Hint:       "see phase 69 — harbor inspect-events / inspect-runs",
			})
		},
	}
	return cmd
}
