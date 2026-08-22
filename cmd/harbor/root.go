// cmd/harbor/root.go — the cobra root command + global flag wiring.
//
// The CLI skeleton (RFC §8) makes `harbor` a cobra-rooted CLI. The root
// command owns the two global flags `--quiet` and `--json`; every
// subcommand inherits them. Subcommands return their CLIError via cobra
// RunE; the root's RunE / PersistentPostRunE wiring funnels every error
// through PrintCLIError so JSON-mode round-trips with the wire shape
// pinned in.
//
// NOTE: cmd/harbor does not import internal/protocol/errors — the CLI
// structured-error surface is distinct from the Protocol wire-error
// surface (operator-facing exit codes vs. Protocol-client responses).
// See CLAUDE.md §8 and errors.go.

package main

import (
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/cmd/harbor/scaffold"
)

// HarborVersion is the harbor binary's product release version — its
// own semver, distinct from the Harbor Protocol version
// (`internal/protocol/types.ProtocolVersion`, RFC §5.3): the release
// version tracks the shipped binary, the Protocol version tracks the
// Runtime↔Console wire contract, and the two move independently.
//
// It is a `var`, not a `const`, so the release build can
// stamp the real git-derived semver into it at link time via
//
//	go build -ldflags="-X 'main.HarborVersion=v1.0.0-rc.1'"
//
// An un-stamped build (`go build`, `go run`, `go test`, a plain
// `make build` on a working tree) keeps the "v0.0.0-dev" default — the
// load-bearing operator signal that "this is not a release artifact",
// the same sentinel discipline `buildHash()` follows (CLAUDE.md §5
// "fail loudly"). The release workflow's version stamp comes from
// `git describe --tags`; `scripts/release-build.sh` is the single
// home of the stamping logic. The CLI surface is
// forward-compatible — `harbor version` always prints whatever value
// this variable carries.
var HarborVersion = "v0.0.0-dev"

// HarborCommit is the immutable Harbor source revision carried by a release
// artifact. scripts/release-build.sh stamps it from the checkout's full HEAD
// commit alongside HarborVersion. An un-stamped build keeps "unknown" and
// deliberately omits framework provenance from runtime.info rather than
// pairing a guessed revision with a product version.
var HarborCommit = "unknown"

// displayVersion resolves the version shown on operator-facing surfaces
// (RuntimeInfo.BuildVersion, the TUI banner). The DISPLAY contract
// differs deliberately from the scaffold's MODULE resolution
// (cmd/harbor/scaffold/version.go): a release artifact's own identity
// is its two-component GitHub tag (`v1.28`), so display accepts it;
// the scaffold's generated `go.mod` requires a proxy-resolvable
// three-component module version, so it does not. Priority: a
// link-stamped release tag wins; a `go install @vX.Y[.Z]` build
// carries its module version in build info; an un-stamped source build
// reports the last published module release with a "-dev" suffix —
// honest ("this source is v1.28.0 plus local changes") instead of the
// meaningless v0.0.0.
func displayVersion() string {
	if releaseDisplayRE.MatchString(HarborVersion) {
		return HarborVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info != nil && releaseDisplayRE.MatchString(info.Main.Version) {
		return info.Main.Version
	}
	return scaffold.FallbackModuleVersion + "-dev"
}

// frameworkIdentity returns the explicitly link-stamped Harbor product
// provenance a runtime may expose separately from its hosting binary's build
// metadata. Both values are either present or absent: proxy installs and
// source builds can know a version while lacking the immutable source commit,
// so they must not manufacture a pair. It returns the accepted stamp verbatim
// rather than displayVersion(): presentation fallback must never turn a
// requested release identity (including release-dryrun's synthetic stamp)
// into a different framework_version.
func frameworkIdentity() (version, commit string) {
	if HarborVersion == "" || HarborVersion == "v0.0.0-dev" || HarborCommit == "" || HarborCommit == "unknown" {
		return "", ""
	}
	return HarborVersion, HarborCommit
}

// releaseDisplayRE matches the version strings a binary may legitimately
// carry as its own RELEASE identity: Harbor's canonical two-component GA
// tags (`v1.28`), the older three-component patch form (`v1.28.0`), and
// both with a conventional pre-release suffix (`-rc.1`, `-beta.2`). It
// deliberately REJECTS the shapes that mean "not a release" — the
// "v0.0.0-dev" un-stamped sentinel and the `git describe` derivatives
// (`v1.13.0-4-gdeadbee`, `...-dirty`) — so those report the honest
// `FallbackModuleVersion + "-dev"` rather than masquerading as a
// release. This regex is for DISPLAY ONLY: two-component tags are
// artifact-release identities, not proxy-resolvable module versions,
// and the scaffold resolver (cmd/harbor/scaffold/version.go) requires
// three components for a module pin.
var releaseDisplayRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+(\.[0-9]+)?(-(alpha|beta|rc)[.0-9A-Za-z-]*)?$`)

// flagJSON is the global `--json` flag name; declared as a constant so
// subcommands and tests reference one canonical spelling.
const flagJSON = "json"

// flagQuiet is the global `--quiet` flag name; ditto.
const flagQuiet = "quiet"

// NewRootCmd constructs and returns a fresh root command tree. It is
// invoked once by main() per process, but tests call it per-test to
// exercise the command tree in isolation (cobra commands carry mutable
// flag state through Execute(), so sharing a root across tests is a
// bug). The returned tree includes every subcommand.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "harbor",
		Short: "Harbor — Go-native agent runtime SDK CLI",
		Long: `harbor is the command-line entry point for the Harbor agent runtime SDK.

Subcommands fall into three groups:

  Local dev loop      init, dev, console, scaffold, validate, skill
  Production          serve, token, postgres
  Run inspection      inspect-events, inspect-runs, inspect-topology,
                      composition-preview, tui
  Build information   version

Subcommands without a real implementation yet stub-fail with a
structured error pointing to the phase that will implement them. See
RFC-001-Harbor.md §8 for the settled subcommand surface and
docs/plans/README.md for the implementation schedule.`,
		// SilenceUsage / SilenceErrors hand error printing to the
		// PersistentPostRunE hook below so the structured-error
		// shape goes through PrintCLIError (CLAUDE.md §5 "fail
		// loudly").
		SilenceUsage:  true,
		SilenceErrors: true,
		// Disable cobra's default completion subcommand to keep the
		// help golden compact and the surface intentional. A later phase
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
	root.AddCommand(newServeCmd())
	root.AddCommand(newTokenCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newScaffoldCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newSkillCmd())
	root.AddCommand(newInspectEventsCmd())
	root.AddCommand(newInspectRunsCmd())
	root.AddCommand(newInspectTopologyCmd())
	root.AddCommand(newCompositionPreviewCmd())
	root.AddCommand(newTUICmd())
	root.AddCommand(newPostgresCmd())

	return root
}

// resolveJSONMode reads the inherited --json flag value off the
// command. Returns false if the flag is not registered on this command
// tree (defensive — every command Harbor ships inherits it).
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
// informational output (none in the stub bodies). The hook
// observes the flag so the contract is wired through end-to-end and
// future subcommand bodies inherit it.
func emitCLIError(cmd *cobra.Command, cliErr CLIError) error {
	_ = resolveQuietMode(cmd) // flag is wired through; no info output in a later phase
	if writeErr := PrintCLIError(cmd.ErrOrStderr(), resolveJSONMode(cmd), cliErr); writeErr != nil {
		// Fail loudly — a write error on stderr is a system-level
		// problem, surface it. This will reach cobra's
		// (silenced) error path but at least propagates a non-nil
		// up the stack so Execute returns non-zero.
		return fmt.Errorf("emit cli error: %w (original: %w)", writeErr, cliErr)
	}
	return cliErr
}
