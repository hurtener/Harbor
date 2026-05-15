// cmd/harbor/cmd_validate.go — `harbor validate` stub.
//
// Phase 68 (per docs/plans/README.md) populates this — validate
// config / skills / agent definitions without booting; errors include
// file:line.

package main

import "github.com/spf13/cobra"

func newValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate [path]",
		Short: "validate config / skills / agent definitions without booting (Phase 68)",
		Long: `Validate a Harbor project's config, skills, and agent definitions
without booting the Runtime. Errors carry file:line. Suitable as a
CI pre-flight check.

This subcommand is registered but not yet implemented; the body lands
in phase 68.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return emitCLIError(cmd, CLIError{
				Subcommand: "validate",
				Message:    "not yet implemented",
				Code:       CodeNotImplemented,
				Hint:       "see phase 68 — harbor validate",
			})
		},
	}
	return cmd
}
