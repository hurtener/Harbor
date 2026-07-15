package main

import "github.com/spf13/cobra"

// newTUICmd exposes an honest preview help surface without enabling attach.
func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "preview the native terminal client availability",
		Long: `The Harbor native terminal client's visual and lifecycle foundation is installed.

Interactive Runtime attachment is not available in this release. The first
operational surface will explicitly provide --attach after conversation and
session workflows are complete. This command currently documents availability
only; it does not connect to a Runtime or expose a private Protocol surface.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return emitCLIError(cmd, CLIError{Subcommand: "tui", Message: "interactive terminal attachment is not available yet", Code: CodeNotImplemented, Hint: "use `harbor tui --help` to inspect the preview status"})
		},
	}
}
