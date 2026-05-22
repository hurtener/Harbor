// cmd/harbor/cmd_version.go — the `harbor version` subcommand.
//
// `harbor version` prints three distinct version facts:
//
//   - HarborVersion   — the product RELEASE version of the binary,
//     stamped at link time by the Phase 81 release build (D-139). An
//     un-stamped build reports the "v0.0.0-dev" sentinel.
//   - types.ProtocolVersion — the Harbor PROTOCOL version (Phase 59,
//     RFC §5.3, D-077): the Runtime↔Console wire contract. It is
//     DISTINCT from HarborVersion and versioned independently — a
//     Runtime refactor that bumps the release version need not bump
//     the Protocol version, and vice versa.
//   - the VCS build hash from runtime/debug.ReadBuildInfo().
//
// Human-mode output (default), pinned by D-084:
//
//	harbor <semver>
//	protocol <protocol_version>
//	build <hash>
//
// JSON-mode output (--json), pinned by D-084:
//
//	{"harbor":"<semver>","protocol":"<protocol_version>","build_hash":"<hash>"}

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// versionInfo is the wire shape `harbor version --json` emits. Pinned
// by D-084; smoke scripts and Console / IDE clients depend on the
// field names.
type versionInfo struct {
	Harbor    string `json:"harbor"`
	Protocol  string `json:"protocol"`
	BuildHash string `json:"build_hash"`
}

// buildHash extracts the VCS revision from runtime/debug.ReadBuildInfo
// when available. Returns "unknown" when the binary was not built with
// VCS info (e.g. `go run` or a test binary). The "unknown" sentinel is
// the load-bearing operator signal that "this is not a release build"
// — silent degradation to an empty string is forbidden per CLAUDE.md
// §5 / §13.
func buildHash() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value
		}
	}
	return "unknown"
}

// currentVersionInfo assembles the versionInfo struct from the
// package constants + runtime/debug. Factored out so tests can pin the
// shape without spawning a process.
func currentVersionInfo() versionInfo {
	return versionInfo{
		Harbor:    HarborVersion,
		Protocol:  types.ProtocolVersion,
		BuildHash: buildHash(),
	}
}

// renderVersionHuman writes the human-mode rendering. Three lines —
// one label per line, label first, then value. Format is stable; smoke
// scripts grep for the line prefixes.
func renderVersionHuman(w io.Writer, info versionInfo) error {
	if _, err := fmt.Fprintf(w, "harbor %s\nprotocol %s\nbuild %s\n", info.Harbor, info.Protocol, info.BuildHash); err != nil {
		return fmt.Errorf("write version (human): %w", err)
	}
	return nil
}

// renderVersionJSON writes the --json rendering. Single-line JSON
// object followed by a newline. Field names pinned by versionInfo
// struct tags (D-084).
func renderVersionJSON(w io.Writer, info versionInfo) error {
	buf, err := json.Marshal(info)
	if err != nil {
		// Marshal of three string fields with no unusual chars is
		// "impossible by construction" per CLAUDE.md §5 — but
		// fail loudly anyway rather than write a partial line.
		return fmt.Errorf("marshal version (json): %w", err)
	}
	buf = append(buf, '\n')
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write version (json): %w", err)
	}
	return nil
}

// newVersionCmd builds the `version` subcommand. The cobra command
// reads --json off its inherited flag set and dispatches to the
// matching renderer.
func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "print harbor semver, protocol version, and build hash",
		Long: `Print the harbor CLI's version metadata.

Human-mode (default):
  harbor v0.0.0-dev
  protocol 0.1.0
  build <vcs-revision-or-"unknown">

JSON-mode (--json):
  {"harbor":"v0.0.0-dev","protocol":"0.1.0","build_hash":"..."}`,
		// Args: no positional args.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := currentVersionInfo()
			if resolveJSONMode(cmd) {
				return renderVersionJSON(cmd.OutOrStdout(), info)
			}
			return renderVersionHuman(cmd.OutOrStdout(), info)
		},
	}
	return cmd
}
