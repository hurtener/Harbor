// cmd/harbor/cmd_dev.go — `harbor dev` v1.
//
// `harbor dev` boots an embedded Harbor Runtime + opens the
// Protocol transports on `127.0.0.1:<port>`. This is the moment the
// binary stops being a driver-registration stub and starts running a
// real LLM-backed runtime — the §13 "test stubs as production
// defaults on operator-facing seams" amendment closure for the LLM
// seam.
//
// # The boot stack
//
// The subcommand assembles, in dependency order:
//
//  1. The config (default `harbor.yaml`, overridable via `--config`).
//  2. The audit Redactor (`audit/drivers/patterns`).
//  3. The event bus (`events/drivers/inmem` or `events/drivers/durable`
//     per config).
//  4. The state store (`state/drivers/{inmem,sqlite,postgres}`).
//  5. The artifact store (`artifacts/drivers/{inmem,fs,sqlite,postgres,s3}`).
//  6. The LLM client (`llm/drivers/bifrost` by default; the mock
//     blank-import is conditionally pulled in by `HARBOR_DEV_ALLOW_MOCK=1`).
//  7. The memory store (`memory/drivers/{inmem,sqlite,postgres}`) +
//     when `memory.strategy: rolling_summary`, an `llm/summarizer.New`
//     Summarizer.
//  8. The task registry (`tasks/drivers/inprocess`).
//  9. The steering registry (process-wide).
// 10. The Protocol ControlSurface + the SSE/REST mux from
//     `internal/protocol/transports`.
// 11. The JWT auth.Validator (mandatory at the edge) +
//     the dev-only ephemeral ES256 KeySet + a default-identity dev
//     token printed at startup.
// 12. An http.Server bound to `127.0.0.1:<port>` with /healthz +
//     /readyz + the mounted Protocol mux.
//
// # Fail-loud at boot
//
// CLAUDE.md §13 "fail loudly at boot": missing LLM provider, missing
// API key, missing required config field → the boot prints a
// one-line error naming the field and points to `examples/dev.yaml`,
// then exits non-zero. No silent fallback to the mock; the only path
// to the mock at runtime is the explicit `HARBOR_DEV_ALLOW_MOCK=1`
// escape hatch.
//
// # The dev-only escape hatch
//
// `HARBOR_DEV_ALLOW_MOCK=1` (env var, not a CLI flag — pinned in)
// tells the dev subcommand to:
//   - blank-import the mock LLM driver so its init() registration
//     fires and `llm.Open(cfg{Driver:"mock"})` resolves;
//   - skip the bifrost-knobs validation gate that would otherwise
//     reject a config with `driver: mock`;
//   - print a stderr banner `[DEV-ONLY MOCK LLM — DO NOT USE IN
//     PRODUCTION]` on every boot.
// The banner is unconditional when the env var is set — no operator
// can "quiet" it; the §13 amendment is explicit about that.
//
// # Graceful shutdown
//
// SIGINT / SIGTERM trigger a graceful drain: the http.Server stops
// accepting new connections, in-flight requests get
// `Server.ShutdownGracePeriod` to complete (default 30s), then the
// subsystems Close in reverse dependency order. A second signal
// during shutdown forces an immediate exit (operators stuck in a
// deadlocked drain can ctrl-C twice).

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/serve"
)

// Stable CLI error codes for `harbor dev`. New codes ADD entries to
// this block; existing codes are wire contracts.
const (
	// CodeBootConfigInvalid fires when the config file fails to load or
	// validate (parse error, missing required, bad enum). Exit 1.
	CodeBootConfigInvalid = "boot_config_invalid"
	// CodeBootLLMRequired fires when the LLM seam cannot be opened
	// because no provider is configured. Exit 1. Surfaced as a
	// one-line message naming the missing knob.
	CodeBootLLMRequired = "boot_llm_required"
	// CodeBootInternal is the catch-all for unexpected wiring failures
	// (a driver Open returning error, a listen failure). Exit 2.
	CodeBootInternal = "boot_internal_error"
)

// Flag names declared as constants so the dev cmd body, tests, and the
// help golden reference one spelling.
const (
	flagDevConfig      = "config"
	flagDevPort        = "port"
	flagDevNoHotReload = "no-hot-reload"
)

// EnvDevAllowMock is the env var name that unlocks the dev-only mock
// LLM path. The choice between an env var and a CLI
// flag was settled on the env var because preflight invokes
// `./bin/harbor dev` without arguments — an env var lets the smoke
// flow without changing the preflight harness.
const EnvDevAllowMock = "HARBOR_DEV_ALLOW_MOCK"

// MockBanner is the unconditional stderr banner printed on every
// `harbor dev` boot when the mock-LLM escape hatch is active. §13
// amendment: "every boot prints a stderr banner".
const MockBanner = "[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]"

// DefaultDevPort is the loopback port `harbor dev` listens on when
// the operator does not override via `--port` or env. Matches the
// preflight harness default.
const DefaultDevPort = 18080

// DefaultDevConfig is the config path `harbor dev` resolves when the
// operator does not pass `--config`. Mirrors `harbor validate`.
const DefaultDevConfig = "harbor.yaml"

// newDevCmd builds the `dev` cobra subcommand. Flags:
//
//	--config <path>  default `harbor.yaml`
//	--port <int>     default 18080 (also overridable via HARBOR_BIND env).
//
// The escape hatch is an env var (`HARBOR_DEV_ALLOW_MOCK=1`), not a
// flag —
func newDevCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "boot the local Runtime + Protocol server",
		Long: `Boot a local Harbor Runtime, open the Protocol transports
onto a 127.0.0.1 listener, and serve until SIGINT / SIGTERM.

The default port is ` + fmt.Sprintf("%d", DefaultDevPort) + `; override via --port or
HARBOR_BIND=host:port.

Identity injection is via an ephemeral ES256 dev-token printed to
stderr at boot. The token carries (tenant=` + DevTenant + `,user=` + DevUser + `,session=` + DevSession + `)
plus admin scope and lives for 24h. Operators wiring a real OIDC
provider should set identity.jwks_url in harbor.yaml (production
wiring lands in a later release-engineering phase).

The LLM seam fails closed: a missing provider exits non-zero with a
named-field error. Operators MUST configure llm.driver=bifrost +
llm.api_key (or env.NAME) in production. The dev-only escape hatch
` + EnvDevAllowMock + `=1 unlocks the mock LLM driver for first-clone
convenience; every boot prints a stderr banner when it fires.

Examples:
  harbor dev
  harbor dev --config ./my-agent/harbor.yaml --port 8080
  HARBOR_DEV_ALLOW_MOCK=1 harbor dev   # dev shortcut; not for production`,
		Args: cobra.NoArgs,
		RunE: runDev,
	}
	cmd.Flags().String(flagDevConfig, DefaultDevConfig, "path to harbor.yaml")
	cmd.Flags().Int(flagDevPort, DefaultDevPort, "loopback port for the Protocol server")
	// operator-facing escape hatch for hot-reload.
	// The default boot enables the watcher per cfg.CLI.DevHotReload.Enabled
	// (which defaults to true via the loader); passing --no-hot-reload
	// forces the watcher off regardless of config. The flag is the §13
	// "dev-only escape hatch — explicit, never the default" surface
	// applied in reverse: operators OPT OUT of a sensible default.
	cmd.Flags().Bool(flagDevNoHotReload, false, "disable the fsnotify-driven hot-reload watcher (overrides cli.dev_hot_reload.enabled)")
	return cmd
}

// runDev is the cobra RunE entry. It composes the dev-only serve policy
// (ephemeral signer, mock-LLM gate, drafts + bootstrap routes, fixture
// seeding) onto the promoted serve band, boots it, mints + prints the dev
// token, and serves until a termination signal — optionally under the
// hot-reload supervisor. Every failure path returns a CLIError so the
// structured-error surface routes through the root.
func runDev(cmd *cobra.Command, _ []string) error {
	// Every flag below is statically registered on this command, so the
	// GetX lookups cannot fail; the blank-error discards are intentional.
	cfgPath, _ := cmd.Flags().GetString(flagDevConfig)        //nolint:errcheck // flag statically registered; lookup cannot fail
	port, _ := cmd.Flags().GetInt(flagDevPort)                //nolint:errcheck // flag statically registered; lookup cannot fail
	noHotReload, _ := cmd.Flags().GetBool(flagDevNoHotReload) //nolint:errcheck // flag statically registered; lookup cannot fail
	bindAddrOverride := os.Getenv("HARBOR_BIND")
	if bindAddrOverride != "" {
		if p, ok := parsePortFromBind(bindAddrOverride); ok {
			port = p
		}
	}
	allowMock := os.Getenv(EnvDevAllowMock) == "1"

	// Boot logger — text handler on stderr so a dev operator's terminal
	// shows readable lines.
	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The mock-LLM escape hatch is dev policy: print the unconditional banner
	// (when the env var fired) and capture the boot-time mock flag for the
	// llm.posture surface. The mock driver itself is blank-imported at
	// compile time via devmock.go; this is the runtime surfacing.
	registerMockIfDevAllowMock(allowMock, cmd.ErrOrStderr())

	comp, err := newDevComposition(devCompositionOptions{
		allowMock:    allowMock,
		serveConsole: false,
	})
	if err != nil {
		return emitCLIError(cmd, bootErrorToCLIError("dev", err))
	}
	bootOpts := comp.serveOptions(cfgPath, port, bindAddrOverride, "dev", logger, cmd.ErrOrStderr())

	stack, err := serve.Boot(ctx, bootOpts)
	if err != nil {
		return emitCLIError(cmd, bootErrorToCLIError("dev", err))
	}

	// Mint + print the ephemeral dev token so an operator can curl the
	// Protocol surface without writing JWT-signing code. Production
	// (`harbor serve`) mints nothing. The same signer backs every hot-reload
	// reboot (so the validator keeps accepting old tokens), and the
	// supervisor's onReboot hook below re-mints + re-prints a fresh token per
	// reboot so a long-lived dev session never outlives the printed expiry.
	if tErr := comp.printDevToken("dev", logger, cmd.ErrOrStderr()); tErr != nil {
		stack.Close(context.Background())
		return emitCLIError(cmd, CLIError{
			Subcommand: "dev",
			Message:    fmt.Sprintf("dev token: %v", tErr),
			Code:       CodeBootInternal,
			Hint:       "this is an internal signing failure; re-run `harbor dev`",
		})
	}

	// hot-reload supervisor. Disabled by --no-hot-reload or config.
	hotReloadEnabled := !noHotReload
	hrCfg := stack.Cfg.CLI.DevHotReload
	if hotReloadEnabled && hrCfg.Enabled != nil && !*hrCfg.Enabled {
		hotReloadEnabled = false
	}
	if hotReloadEnabled && hrCfg.Policy == config.DevHotReloadPolicyDisabled {
		hotReloadEnabled = false
	}

	if !hotReloadEnabled {
		defer stack.Close(context.Background())
		if err := stack.Serve(ctx); err != nil {
			return emitCLIError(cmd, CLIError{
				Subcommand: "dev",
				Message:    fmt.Sprintf("dev server stopped: %v", err),
				Code:       CodeBootInternal,
				Hint:       "check the server log lines above for the originating subsystem",
			})
		}
		return nil
	}

	watchRoots := resolveHotReloadWatchRoots(hrCfg, cfgPath)
	supervisor, err := newHotReloadSupervisor(logger, bootOpts, stack, hrCfg, watchRoots)
	if err != nil {
		stack.Close(context.Background())
		return emitCLIError(cmd, CLIError{
			Subcommand: "dev",
			Message:    fmt.Sprintf("hot-reload supervisor: %v", err),
			Code:       CodeBootInternal,
			Hint:       "check cli.dev_hot_reload in harbor.yaml; pass --no-hot-reload to bypass",
		})
	}
	// Every reboot re-announces the dev posture: the mock banner (when the
	// escape hatch fired) prints on EVERY boot, and a fresh dev token is
	// re-minted + re-printed so its 24h expiry restarts per reboot. A token
	// re-mint failure is logged loud but does not kill the reboot — the
	// previously printed token (same signer) keeps validating.
	stderr := cmd.ErrOrStderr()
	supervisor.onReboot = func(_ *serve.Handle) {
		registerMockIfDevAllowMock(allowMock, stderr)
		if tErr := comp.printDevToken("dev", logger, stderr); tErr != nil {
			logger.Error("harbor dev: dev-token re-mint after hot-reload failed (the previously printed token remains valid)",
				slog.String("error", tErr.Error()))
		}
	}
	defer func() {
		current := supervisor.CurrentStack()
		if current != nil {
			current.Close(context.Background())
		}
	}()

	if err := supervisor.Run(ctx); err != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "dev",
			Message:    fmt.Sprintf("dev server stopped: %v", err),
			Code:       CodeBootInternal,
			Hint:       "check the server log lines above for the originating subsystem; pass --no-hot-reload to disable the watcher",
		})
	}
	return nil
}

// validateLLMProvider enforces constraint #2: missing provider, missing
// API key, or empty `llm:` block (driver=bifrost without provider/model/
// api_key) fails closed with a one-line error naming the missing key
// and pointing to `examples/dev.yaml`.
//
// When `allowMock` is true (HARBOR_DEV_ALLOW_MOCK=1), the function
// short-circuits success — the mock driver ignores provider knobs.
func validateLLMProvider(cfg *config.Config, allowMock bool) error {
	if allowMock {
		// Operator opted in. The escape hatch is the explicit signal;
		// no validation runs on the bifrost knobs.
		return nil
	}
	if cfg.LLM.Driver == "" || cfg.LLM.Driver == "mock" {
		return fmt.Errorf("%w: config.llm.driver: must be %q (or set %s=1 for the dev-only mock; see examples/dev.yaml)",
			ErrLLMRequired, "bifrost", EnvDevAllowMock)
	}
	if cfg.LLM.Driver == "bifrost" {
		if cfg.LLM.Provider == "" {
			return fmt.Errorf("%w: config.llm.provider: required when driver=bifrost (see examples/dev.yaml)", ErrLLMRequired)
		}
		if cfg.LLM.Model == "" {
			return fmt.Errorf("%w: config.llm.model: required when driver=bifrost (see examples/dev.yaml)", ErrLLMRequired)
		}
		// API key — the bifrost driver resolves `env.NAME` references
		// at construction time, so we accept ANY non-empty string at
		// this layer (the driver will fail loud if the env var is
		// unset). An EMPTY api_key is the boot-fail-loud case.
		if cfg.LLM.APIKey == "" {
			return fmt.Errorf("%w: config.llm.api_key: required when driver=bifrost (use env.NAME for env-var indirection; see examples/dev.yaml)", ErrLLMRequired)
		}
	}
	return nil
}

// ErrLLMRequired is the typed sentinel constraint #2's fail-loud
// surfaces. Tests compare via `errors.Is`.
var ErrLLMRequired = errors.New("dev: LLM provider not configured")

// bootErrorToCLIError maps a boot error onto a CLIError. The mapping
// preserves the underlying error chain so callers can errors.Is back
// to the sentinel. subcommand labels which command's boot failed ("dev"
// / "console" / "serve") so the structured-error surface attributes the
// failure correctly.
func bootErrorToCLIError(subcommand string, err error) CLIError {
	switch {
	case errors.Is(err, ErrLLMRequired):
		return CLIError{
			Subcommand: subcommand,
			Message:    err.Error(),
			Code:       CodeBootLLMRequired,
			Hint:       "see examples/dev.yaml for the canonical shape; set " + EnvDevAllowMock + "=1 for the dev-only mock escape hatch",
		}
	case errors.Is(err, config.ErrConfigNotFound):
		return CLIError{
			Subcommand: subcommand,
			Message:    err.Error(),
			Code:       CodeBootConfigInvalid,
			Hint:       "pass --config or create harbor.yaml in the working directory (try `harbor scaffold --name my-agent`)",
		}
	case errors.Is(err, config.ErrConfigInvalid):
		return CLIError{
			Subcommand: subcommand,
			Message:    err.Error(),
			Code:       CodeBootConfigInvalid,
			Hint:       "run `harbor validate` for file:line precision on the failing field",
		}
	default:
		return CLIError{
			Subcommand: subcommand,
			Message:    fmt.Sprintf("boot failed: %v", err),
			Code:       CodeBootInternal,
			Hint:       "check the server log lines above for the originating subsystem",
		}
	}
}

// parsePortFromBind extracts the port from a host:port bind string.
// Used so HARBOR_BIND=host:port overrides --port consistently. Returns
// (0, false) on malformed input — the caller keeps the supplied port.
func parsePortFromBind(bind string) (int, bool) {
	// Look for the LAST ':' so IPv6-shaped binds (`[::1]:18080`) parse.
	i := strings.LastIndex(bind, ":")
	if i < 0 || i == len(bind)-1 {
		return 0, false
	}
	tail := bind[i+1:]
	n := 0
	for _, c := range tail {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// devInstanceID mints a stable-per-process instance identifier for the
// dev Runtime. A Console attached to multiple Runtimes keys each
// attachment by it.
func devInstanceID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return "harbor-dev-" + h
	}
	return "harbor-dev"
}

// Compile-time ensure identity is imported (boot wiring reads
// identity.Quadruple under the dev token's claims; the import is also
// used by the dev-cmd integration test via the SignDevToken helper).
var _ identity.Identity
