// cmd/harbor/cmd_inspect_topology.go — `harbor inspect-topology` stub.
//
// Phase 70 (per docs/plans/README.md) populates this — render a run's
// node graph as ASCII.

package main

import "github.com/spf13/cobra"

func newInspectTopologyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect-topology",
		Short: "render a run's node graph as ASCII (Phase 70)",
		Long: `Render the topology of a Harbor run as an ASCII node graph, using the
` + "`topology.snapshot`" + ` events emitted by the Runtime.

This subcommand is registered but not yet implemented; the body lands
in phase 70.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return emitCLIError(cmd, CLIError{
				Subcommand: "inspect-topology",
				Message:    "not yet implemented",
				Code:       CodeNotImplemented,
				Hint:       "see phase 70 — harbor inspect-topology",
			})
		},
	}
	return cmd
}
