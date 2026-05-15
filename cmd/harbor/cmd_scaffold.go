// cmd/harbor/cmd_scaffold.go — `harbor scaffold` stub.
//
// Phase 67 (per docs/plans/README.md) populates this — generate a new
// agent skeleton from a template (default "minimal-react").

package main

import "github.com/spf13/cobra"

func newScaffoldCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scaffold [name]",
		Short: "generate a new agent skeleton from a template (Phase 67)",
		Long: `Generate a new Harbor agent project skeleton from a template (default
"minimal-react"). The output project is buildable and passes
` + "`harbor validate`" + `.

This subcommand is registered but not yet implemented; the body lands
in phase 67.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return emitCLIError(cmd, CLIError{
				Subcommand: "scaffold",
				Message:    "not yet implemented",
				Code:       CodeNotImplemented,
				Hint:       "see phase 67 — harbor scaffold",
			})
		},
	}
	return cmd
}
