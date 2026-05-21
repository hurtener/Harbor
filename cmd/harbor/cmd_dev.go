// cmd/harbor/cmd_dev.go — `harbor dev` v1 (Phase 64, D-089).
//
// `harbor dev` boots an embedded Harbor Runtime + opens the Phase 60
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
// 11. The Phase 61 JWT auth.Validator (mandatory at the edge) +
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
// escape hatch (D-089).
//
// # The dev-only escape hatch
//
// `HARBOR_DEV_ALLOW_MOCK=1` (env var, not a CLI flag — pinned in
// D-089) tells the dev subcommand to:
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
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/devdraft"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	llmsummarizer "github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/search"
	searchartifacts "github.com/hurtener/Harbor/internal/search/artifacts"
	searchevents "github.com/hurtener/Harbor/internal/search/events"
	searchsessions "github.com/hurtener/Harbor/internal/search/sessions"
	searchtasks "github.com/hurtener/Harbor/internal/search/tasks"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	toolcatalog "github.com/hurtener/Harbor/internal/tools/catalog"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
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
// LLM path. Pinned in D-089. The choice between an env var and a CLI
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
// flag — see D-089.
func newDevCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "boot the local Runtime + Protocol server",
		Long: `Boot a local Harbor Runtime, open the Phase 60 Protocol transports
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
	// Phase 65 (D-099) — operator-facing escape hatch for hot-reload.
	// The default boot enables the watcher per cfg.CLI.DevHotReload.Enabled
	// (which defaults to true via the loader); passing --no-hot-reload
	// forces the watcher off regardless of config. The flag is the §13
	// "dev-only escape hatch — explicit, never the default" surface
	// applied in reverse: operators OPT OUT of a sensible default.
	cmd.Flags().Bool(flagDevNoHotReload, false, "disable the fsnotify-driven hot-reload watcher (overrides cli.dev_hot_reload.enabled)")
	return cmd
}

// runDev is the cobra RunE entry. It assembles the boot stack, mounts
// the Protocol mux, serves until a termination signal, then drains.
// Every failure path returns a CLIError so the structured-error
// surface routes through the root.
func runDev(cmd *cobra.Command, _ []string) error {
	cfgPath, _ := cmd.Flags().GetString(flagDevConfig)
	port, _ := cmd.Flags().GetInt(flagDevPort)
	noHotReload, _ := cmd.Flags().GetBool(flagDevNoHotReload)
	bindAddrOverride := os.Getenv("HARBOR_BIND")
	if bindAddrOverride != "" {
		// HARBOR_BIND=host:port overrides --port (used by preflight,
		// D-104 in particular — `HARBOR_BIND=127.0.0.1:0` requests an
		// ephemeral port). The override is a single env var so an
		// operator who needs to bind beyond 127.0.0.1 can drive both
		// host AND port from the same surface. We parse the port out
		// for the bind addr but keep the full host:port as the listen
		// string. parsePortFromBind rejects port 0 (the sentinel),
		// which is correct — port 0 stays in `bindAddrOverride` so the
		// listener sees `host:0` and the OS hands back a real port.
		if p, ok := parsePortFromBind(bindAddrOverride); ok {
			port = p
		}
	}
	allowMock := os.Getenv(EnvDevAllowMock) == "1"

	// Boot logger — text handler on stderr so a dev operator's terminal
	// shows readable lines. JSON-handler under a future production
	// `harbor server` subcommand.
	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	bootOpts := devBootOptions{
		cfgPath:   cfgPath,
		port:      port,
		bindAddr:  bindAddrOverride,
		allowMock: allowMock,
		logger:    logger,
		stderr:    cmd.ErrOrStderr(),
	}
	stack, err := bootDevStack(ctx, bootOpts)
	if err != nil {
		return emitCLIError(cmd, bootErrorToCLIError(err))
	}

	// Phase 65 (D-099) — hot-reload supervisor. The supervisor owns the
	// active devStack lifecycle from this point: on a file change it
	// drains the current stack per `cli.dev_hot_reload.policy`, calls
	// `bootDevStack` again with the same opts, and swaps the result in.
	// The supervisor exits cleanly on ctx-cancel (SIGINT/SIGTERM) and
	// runs to completion alongside the stack's serve loop.
	//
	// The supervisor is OPTIONAL — disabled when:
	//   - The operator passes `--no-hot-reload`.
	//   - The config sets `cli.dev_hot_reload.enabled: false`.
	//   - The config sets `cli.dev_hot_reload.policy: disabled`.
	//
	// In the disabled case, runDev falls back to the pre-Phase-65
	// behaviour: serve the stack directly, drain on ctx-cancel.
	hotReloadEnabled := !noHotReload
	hrCfg := stack.cfg.CLI.DevHotReload
	if hotReloadEnabled && hrCfg.Enabled != nil && !*hrCfg.Enabled {
		hotReloadEnabled = false
	}
	if hotReloadEnabled && hrCfg.Policy == config.DevHotReloadPolicyDisabled {
		hotReloadEnabled = false
	}

	if !hotReloadEnabled {
		defer stack.close(context.Background())
		if err := stack.serve(ctx); err != nil {
			return emitCLIError(cmd, CLIError{
				Subcommand: "dev",
				Message:    fmt.Sprintf("dev server stopped: %v", err),
				Code:       CodeBootInternal,
				Hint:       "check the server log lines above for the originating subsystem",
			})
		}
		return nil
	}

	// Construct the supervisor with the initial stack. The supervisor
	// takes ownership of the stack — we drain via supervisor.CurrentStack()
	// on the deferred shutdown so signal-driven AND rebuild-driven
	// shutdowns share one drain path.
	watchRoots := resolveHotReloadWatchRoots(hrCfg, cfgPath)
	supervisor, err := newHotReloadSupervisor(logger, bootOpts, stack, hrCfg, watchRoots)
	if err != nil {
		stack.close(context.Background())
		return emitCLIError(cmd, CLIError{
			Subcommand: "dev",
			Message:    fmt.Sprintf("hot-reload supervisor: %v", err),
			Code:       CodeBootInternal,
			Hint:       "check cli.dev_hot_reload in harbor.yaml; pass --no-hot-reload to bypass",
		})
	}
	defer func() {
		// Drain the supervisor's current stack on shutdown. After
		// supervisor.Run returns, CurrentStack() is the last
		// successfully-booted stack — either the initial one (no
		// rebuild ever fired) or the latest reboot.
		current := supervisor.CurrentStack()
		if current != nil {
			current.close(context.Background())
		}
	}()

	// The supervisor owns both the serve loop and the rebuild loop.
	// Run blocks until ctx cancels OR a rebuild fails fatally OR the
	// active serve goroutine exits with a non-nil error.
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

// devBootOptions bundles the inputs `bootDevStack` consumes. Kept as
// a struct so tests can drive the boot in isolation (Phase 64
// integration test) without re-creating cobra wiring.
//
// `bindAddr` is the operator override path for the listener address.
// It's read by `runDev` from the `HARBOR_BIND` env var (D-104 — the
// preflight harness sets `HARBOR_BIND=127.0.0.1:0` so a non-zero
// ephemeral port is OS-assigned) and threaded explicitly here so
// `bootDevStack` does NOT read the env var directly. Tests that
// construct `devBootOptions` with `port: 0` and an empty `bindAddr`
// get an ephemeral port regardless of whatever HARBOR_BIND the
// surrounding process inherits — a leak that previously caused
// cmd/harbor tests to bind the preflight server's port under load.
type devBootOptions struct {
	cfgPath   string
	port      int
	bindAddr  string
	allowMock bool
	logger    *slog.Logger
	stderr    interface{ Write(p []byte) (int, error) }
	// serveConsole, when true, mounts the embedded SvelteKit Console
	// build (cmd/harbor/console_embed.go) onto the boot router at `/`
	// with SPA fallback. Set ONLY by the `harbor console` subcommand
	// (Phase 73m / D-129) — `harbor dev` leaves it false so the Console
	// build is NEVER embedded into the dev-loop surface (D-091 binding
	// rule). The Protocol surface (`/v1/*`) is identical either way; the
	// flag only adds the static-asset routes.
	serveConsole bool
	// subcommandLabel is the subcommand name stamped into the `/healthz`
	// JSON body + the boot log lines ("dev" or "console"). Defaults to
	// "dev" when empty.
	subcommandLabel string
}

// bootDevStack does the heavy lifting: it reads the config, opens
// every subsystem, composes the Protocol surface, and returns a
// `devStack` whose `serve` method binds the listener and runs until
// ctx cancels.
//
// On any error, every successfully-opened subsystem is Close'd
// before returning. The caller MUST call `stack.close` after a
// successful boot to drain the listen loop + close every dep.
func bootDevStack(ctx context.Context, opts devBootOptions) (*devStack, error) {
	cfg, err := config.Load(ctx, opts.cfgPath, config.WithLogger(opts.logger))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// Construct subsystems in dependency order. Every "close everything
	// we've opened so far" path is funneled through `closers`.
	var closers []func(context.Context) error
	closeAll := func(ctx context.Context) {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i](ctx)
		}
	}

	red, err := audit.Open(ctx, cfg.Audit)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	// `audit.Redactor` has no Close in the current interface — nothing
	// to register here.

	bus, err := events.Open(ctx, cfg.Events, red)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	closers = append(closers, bus.Close)

	// Phase 56 / 72f: the MetricsRegistry — metrics are a derivation of
	// the event bus (D-082). The registry is constructed once at boot;
	// BridgeBusToMetrics fans every bus event into the registry's
	// counter so `metrics.snapshot` (wired into PostureDeps.Metrics
	// below) projects live numbers — never an empty stub (§17.6 F3).
	// An Admin-scope filter feeds the fleet-wide bridge; the bridge
	// does not widen the isolation boundary (the wire scope is the
	// caller's responsibility — here a runtime-internal infra consumer).
	metricsReg, metricsShutdown, err := telemetry.NewMetricsRegistry(cfg.Telemetry)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("telemetry metrics: %w", err)
	}
	closers = append(closers, metricsShutdown)
	metricsBridgeStop, err := telemetry.BridgeBusToMetrics(ctx, bus, metricsReg, events.Filter{Admin: true})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("telemetry metrics bridge: %w", err)
	}
	closers = append(closers, func(context.Context) error { metricsBridgeStop(); return nil })

	stateStore, err := state.Open(ctx, cfg.State)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("state: %w", err)
	}
	closers = append(closers, stateStore.Close)

	artStore, err := artifacts.Open(ctx, cfg.Artifacts)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("artifacts: %w", err)
	}
	closers = append(closers, artStore.Close)

	// LLM seam — fail loud per §13 when no provider configured AND
	// the operator did not explicitly opt into the mock.
	if err := validateLLMProvider(cfg, opts.allowMock); err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("llm: %w", err)
	}
	// The dev binary blank-imports the mock LLM driver via
	// `cmd/harbor/devmock.go` so its init() seats the registration in
	// the llm.factories map. The conditional import is NOT in main.go
	// (that's the §13 "unreachable from main.go's blank-import block"
	// surface); it lives at the dev-cmd boundary. The runtime gate —
	// `validateLLMProvider` above — refuses to start the runtime
	// against `driver: mock` UNLESS allowMock is true. The
	// unconditional stderr banner emit on every boot when the env var
	// fires is the "do not use in production" surfacing the §13
	// amendment mandates.
	registerMockIfDevAllowMock(opts.allowMock, opts.stderr)

	// Build the LLM ConfigSnapshot. When the dev-only escape hatch
	// fired, override the driver to "mock" regardless of what
	// harbor.yaml said — the operator's intent ("give me the mock")
	// is explicit via the env var, and bypassing the bifrost knobs
	// avoids the operator having to maintain two separate yaml files
	// (one for prod, one for dev). The override is local to the
	// snapshot; the original config.Config is unchanged.
	driverName := cfg.LLM.Driver
	apiKey := cfg.LLM.APIKey
	if opts.allowMock {
		driverName = "mock"
		apiKey = ""
	}
	llmCfg := llm.ConfigSnapshot{
		Driver:               driverName,
		Provider:             cfg.LLM.Provider,
		Model:                cfg.LLM.Model,
		APIKey:               apiKey,
		BaseURL:              cfg.LLM.BaseURL,
		Timeout:              cfg.LLM.Timeout,
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles:        copyModelProfiles(cfg.LLM.ModelProfiles),
	}
	llmClient, err := llm.Open(ctx, llmCfg, llm.Deps{
		Artifacts: artStore,
		Bus:       bus,
	})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("llm: %w", err)
	}
	closers = append(closers, llmClient.Close)

	// Memory subsystem. When the operator picked rolling_summary, wire
	// the LLM-backed default Summarizer (constraint #3 — Phase 64 / D-089).
	// The memory open path is configured even when strategy=none so the
	// runtime has a memory store for future per-session reads.
	//
	// The Phase 23 registry path (`memory.Open`) does NOT accept a
	// Summarizer injection; only strategy=none + strategy=truncation
	// resolve through it. For strategy=rolling_summary we MUST call
	// the driver's `inmem.New(...)` directly with `Options{Summarizer:
	// llmsummarizer.New(client)}`. SQLite + Postgres memory drivers
	// (Phase 25) have not yet been audited for this same shape — for
	// now, rolling_summary on those drivers is rejected at boot with a
	// clear "not yet wired" error.
	var memStore memory.MemoryStore
	if cfg.Memory.Driver != "" {
		memCfg := memory.ConfigSnapshot{
			Driver:             cfg.Memory.Driver,
			DSN:                cfg.Memory.DSN,
			Strategy:           memory.Strategy(cfg.Memory.Strategy),
			BudgetTokens:       cfg.Memory.BudgetTokens,
			RecoveryBacklogMax: cfg.Memory.RecoveryBacklogMax,
		}
		if cfg.Memory.Strategy == "rolling_summary" {
			if cfg.Memory.Driver != "inmem" {
				closeAll(ctx)
				return nil, fmt.Errorf("memory: rolling_summary is only wired against driver=inmem at Phase 64 (got driver=%q); see docs/plans/phase-25-memory-drivers.md for the SQLite/Postgres Summarizer-injection follow-up", cfg.Memory.Driver)
			}
			s, sErr := llmsummarizer.New(llmClient)
			if sErr != nil {
				closeAll(ctx)
				return nil, fmt.Errorf("summarizer: %w", sErr)
			}
			ms, openErr := memoryinmem.New(memCfg, memory.Deps{
				State: stateStore,
				Bus:   bus,
			}, memoryinmem.Options{
				Summarizer: s,
			})
			if openErr != nil {
				closeAll(ctx)
				return nil, fmt.Errorf("memory: %w", openErr)
			}
			closers = append(closers, ms.Close)
			memStore = ms
		} else {
			ms, openErr := memory.Open(ctx, memCfg, memory.Deps{
				State: stateStore,
				Bus:   bus,
			})
			if openErr != nil {
				closeAll(ctx)
				return nil, fmt.Errorf("memory: %w", openErr)
			}
			closers = append(closers, ms.Close)
			memStore = ms
		}
	}
	// memStore is consumed by the Phase 73j (D-118) Console Memory page
	// `memory.*` read routes — wired into transports.NewMux below via
	// transports.WithMemory.

	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store:    stateStore,
		Bus:      bus,
		Redactor: red,
		Cfg:      cfg.Tasks,
	})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("tasks: %w", err)
	}
	closers = append(closers, taskReg.Close)

	// Tool catalog + Phase 64a catalog wiring (D-090). The dev cmd
	// constructs an empty catalog; operators register tools either via
	// in-process Go code (their own embedding harness) or — once
	// Phase 27/28/29 manifests are loaded — via the configured tool
	// sources. The Phase 64a wiring step is applied LAST so any
	// `tools.entries[]` declared in `harbor.yaml` auto-wraps its
	// matching descriptors with the declared approval / OAuth
	// middleware. An entry naming a tool that is not registered fails
	// the boot loud (CLAUDE.md §13 amendment).
	//
	// The shared `pauseresume.Coordinator` is the unified pause/resume
	// primitive (Phase 50 / D-067). Future phases that need to pause
	// for OAuth or approval read this same Coordinator from
	// `devStack.coordinator` — there is NEVER a second Coordinator
	// instance (CLAUDE.md §13).
	toolCat := tools.NewCatalog()
	// WithBus(bus) is mandatory in production: it is what makes
	// pause.requested / pause.resumed land on the canonical event
	// stream so wire consumers (Console, third-party Protocol clients,
	// integration tests) observe D-096's typed Decision marker. Bare
	// pauseresume.New() short-circuits emit when bus == nil — the same
	// shape the Wave 11.5 §17.5 closeout audit flagged as F1.
	// harbortest/devstack.Assemble carries the matching wiring per
	// D-094's "helper tracks production" rule.
	coord := pauseresume.New(pauseresume.WithBus(bus))
	appliedGates, oauthProviders, applyErr := applyToolCatalogWiring(ctx, cfg, toolCat, coord, bus, red, stateStore)
	if applyErr != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("tools/catalog: %w", applyErr)
	}
	// Approval gates close cleanly when the dev stack drains; close in
	// reverse-dependency order with the rest of the subsystems.
	for _, g := range appliedGates {
		gate := g
		closers = append(closers, func(closeCtx context.Context) error { return gate.Close(closeCtx) })
	}
	// OAuth providers also close cleanly.
	for _, p := range oauthProviders {
		prov := p
		closers = append(closers, func(closeCtx context.Context) error { return prov.Close(closeCtx) })
	}

	steeringReg := steering.NewRegistry()

	// Planner — the swappable reasoning policy the RunLoop drives.
	// D-103 (closes issue #126) — the planner concrete is resolved via
	// the `internal/planner` driver registry (the §4.4 seam pattern
	// that D-095 uses for OAuth providers). `cmd/harbor/main.go`
	// blank-imports each driver so its init() registration fires; the
	// `cfg.Planner.Driver` allowlist in `internal/config/validate.go`
	// pre-boots an unknown driver name. The V1 reference driver
	// (`react`) backs the no-config-needed default; future concretes
	// (Plan-Execute, Workflow, Graph, Deterministic, Supervisor,
	// MultiAgent, HumanApproval per RFC §6.2) opt in via
	// `planner.driver: <name>` in `harbor.yaml`. The planner is reusable
	// across concurrent runs (D-025); one instance backs every spawned
	// task's RunLoop.
	plannerCfg := plannerConfigFromConfig(cfg.Planner)
	plnr, err := planner.Resolve(ctx, plannerCfg, planner.FactoryDeps{LLM: llmClient})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("planner: %w", err)
	}

	// RunLoop — the per-run planner-step loop (Phase 53 / D-071) that
	// drives the planner to a terminal Finish, draining the steering
	// inbox between steps and routing pause decisions through the
	// unified Coordinator. ONE RunLoop instance is constructed at
	// boot and shared across N concurrent goroutines (D-025); each
	// goroutine's per-run state lives on its own stack + ctx. The
	// WithApprovalGates option wires the D-097 steering→gate bridge:
	// a wire-side APPROVE / REJECT control that reaches the run's
	// steering inbox is routed to the matching gate's pending map,
	// unblocking the wrapped tool's Invoke.
	runLoop, err := steering.NewRunLoop(steeringReg, coord,
		steering.WithRunLoopBus(bus),
		steering.WithTaskRegistry(taskReg),
		steering.WithApprovalGates(appliedGates),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("steering.RunLoop: %w", err)
	}

	// Phase 74 (D-114): the ControlSurface is built WITHOUT a
	// topology accessor. `harbor dev`'s runtime is planner/RunLoop-
	// shaped — it hosts no `engine.Engine` node-graph — so there is no
	// topology to project. A `topology.snapshot` call against this
	// surface therefore returns CodeUnknownMethod (HTTP 404), which
	// the smoke script's 404 → SKIP convention picks up cleanly. This
	// is the documented nil-safe path, NOT a wiring gap (CLAUDE.md
	// §17.6): a Runtime that hosts an engine wires it via
	// protocol.WithTopologyAccessor; the Phase 74 integration test
	// does exactly that through harbortest/devstack.AssembleOpts.
	surface, err := protocol.NewControlSurface(taskReg, steeringReg)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("protocol: %w", err)
	}

	// Per-task RunLoop driver — subscribes to `task.spawned` events
	// across every tenant/user/session (the dev binary serves them
	// all) and launches a goroutine per spawned foreground task that
	// calls `runLoop.Run` with the task's identity quadruple. This is
	// the wiring that closes issue #114 (Phase 53's RunLoop had zero
	// production consumers before D-097). The driver shuts down with
	// the rest of the stack — its closer cancels the subscription's
	// ctx and waits for every in-flight goroutine to drain.
	//
	// Subscription is admin-scoped via §6 rule 5's elevated-subscription
	// path — the driver listens across every (tenant, user, session)
	// because task.spawned filtering by triple would force per-session
	// subscriptions and a registry-side hook the V1 design hasn't
	// introduced. The rule authorizes this for runtime-internal fan-in
	// subscribers; the bus emits `audit.admin_scope_used` for the
	// trail.
	runLoopDriver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		logger:   opts.logger,
		bus:      bus,
		runLoop:  runLoop,
		planner:  plnr,
		tasks:    taskReg, // D-098: the FSM the driver advances on RunLoop exit (closes #123)
		taskKind: tasks.KindForeground,
	})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("steering.RunLoop driver: %w", err)
	}
	if err := runLoopDriver.Start(ctx); err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("steering.RunLoop driver start: %w", err)
	}
	closers = append(closers, runLoopDriver.Close)

	devSigner, err := newDevSigner()
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("dev signer: %w", err)
	}
	validator, err := auth.NewValidator(devSigner.KeySet(),
		auth.WithRedactor(red),
		auth.WithLogger(opts.logger),
		auth.WithEventBus(bus),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("auth: %w", err)
	}

	// Phase 73m (D-129): the `auth.rotate_token` surface — the single
	// net-new Console Settings-page Protocol method. The dev signer is
	// the V1 auth.TokenIssuer (it re-mints ephemeral ES256 tokens for
	// the caller's verified identity); a real OIDC-backed deployment
	// wires an RFC 8693 token-exchange issuer behind the same seam.
	// The redactor + bus are wired so every accepted rotation emits a
	// redacted `audit.admin_scope_used` event (CLAUDE.md §7 rule 6).
	rotateSurface, err := auth.NewRotateSurface(devSigner, red,
		auth.WithRotateBus(bus),
		auth.WithRotateLogger(opts.logger),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("auth: rotate surface: %w", err)
	}

	// Phase 73l (D-120): the artifacts surface backing the Console
	// Artifacts page — `artifacts.list` (catalog), `artifacts.put`
	// (upload pipeline per Brief 11 §PG-2), `artifacts.get_ref`
	// (presigned-URL resolver per D-022 / D-026). Wired with the live
	// artifact store, the audit redactor, and the bus so an operator
	// gets a working artifacts surface out of the box (no seam to wire —
	// CLAUDE.md §13). The dev artifact store defaults to the `inmem`
	// driver, which does not implement `Presigner`; an `artifacts.get_ref`
	// against it fails loud with `CodePresignUnsupported` (the Console
	// renders the fallback Download link).
	artDriverName := cfg.Artifacts.Driver
	if artDriverName == "" {
		artDriverName = "inmem"
	}
	artifactsSurface, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store:        artStore,
		Redactor:     red,
		Bus:          bus,
		Clock:        time.Now,
		DriverName:   artDriverName,
		MaxBodyBytes: cfg.Protocol.ResolvedMaxRequestBytes(),
	})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("protocol artifacts surface: %w", err)
	}

	// Phase 73f: the Console Tools-page Protocol service. The catalog
	// projector is built over the same tool catalog the runtime
	// dispatches against, so the Console renders the live registered
	// set. The bus + redactor are wired so the two admin methods'
	// `audit.admin_scope_used` events reach the bus (CLAUDE.md §13 —
	// the admin path is never a silent no-op).
	toolsProjector, err := toolsprotocol.NewCatalogProjector(toolCat)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("tools/protocol projector: %w", err)
	}
	toolsService, err := toolsprotocol.NewService(toolsProjector,
		toolsprotocol.WithBus(bus),
		toolsprotocol.WithRedactor(red),
		toolsprotocol.WithLogger(opts.logger),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("tools/protocol service: %w", err)
	}

	// Phase 73e (D-124): the Console Agents-page Protocol service. The
	// Agent Registry is the per-runtime-instance subsystem owning agent
	// registration identity (D-059 / D-060); it is constructed over the
	// same StateStore the rest of the runtime persists through, so the
	// Console renders the live registered set. A fresh dev stack with no
	// agents registered correctly serves an empty catalog ("no agents
	// yet" is the right empty state, not a missing surface). The eight
	// `agents.*` methods are READ-ONLY projections; the five
	// agent-control verbs the page exposes are the EXISTING shipped
	// `registry.*` control verbs (D-066), not new methods (CLAUDE.md
	// §13).
	agentRegistry, err := registry.New(registry.Deps{
		Store:    stateStore,
		Bus:      bus,
		Redactor: red,
	})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("agent registry: %w", err)
	}
	agentsProjector, err := agentsprotocol.NewRegistryProjector(agentRegistry)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("registry/protocol projector: %w", err)
	}
	agentsService, err := agentsprotocol.NewService(agentsProjector,
		agentsprotocol.WithLogger(opts.logger),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("registry/protocol service: %w", err)
	}

	// Phase 73i (D-117): the Console Flows-page surface. The dev stack
	// boots an empty flow.Registry — flows register into it at
	// agent-definition time, so a fresh dev stack with no graph-family
	// agents correctly serves an empty catalog ("no flows registered"
	// is the right empty state, not a missing surface). The Catalog +
	// Invoker are wired with the real artifact store + the configured
	// heavy-content threshold so the Console Flows page works out of
	// the box (no seam for the operator to wire — CLAUDE.md §13). The
	// FuncInvoker's launcher delegates to the task registry's
	// `SpawnTool` path — running an existing flow is permitted at V1
	// (D-063).
	flowRegistry := flow.NewRegistry()
	flowCatalog, err := flowprotocol.NewRegistryCatalog(flowRegistry, artStore, cfg.Artifacts.HeavyOutputThresholdBytes)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("flow protocol catalog: %w", err)
	}
	flowInvoker, err := flowprotocol.NewFuncInvoker(
		func(launchCtx context.Context, id identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
			runCtx, rerr := identity.WithRun(launchCtx, id, "flow-run-"+flowID)
			if rerr != nil {
				return "", time.Time{}, fmt.Errorf("flows.run: identity scope incomplete: %w", rerr)
			}
			handle, serr := taskReg.SpawnTool(runCtx, tasks.SpawnToolRequest{
				Identity:    identity.Quadruple{Identity: id},
				ToolName:    flowID,
				Description: "Console flows.run invocation of " + flowID,
			})
			if serr != nil {
				return "", time.Time{}, fmt.Errorf("flows.run: spawn failed: %w", serr)
			}
			return string(handle.ID), time.Now(), nil
		}, flowRegistry)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("flow protocol invoker: %w", err)
	}
	flowsSurface, err := flowprotocol.NewSurface(flowCatalog, flowInvoker)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("flow protocol surface: %w", err)
	}

	// Phase 73d (D-123): the Console Tasks-page Protocol service. The
	// registry projector is built over the same TaskRegistry the
	// runtime drives, so the Console renders the live task set. The bus
	// + redactor are wired so a cross-tenant `tasks.list` fan-in's
	// `audit.admin_scope_used` event reaches the bus (CLAUDE.md §13 —
	// the admin path is never a silent no-op).
	tasksProjector, err := tasksprotocol.NewRegistryProjector(taskReg)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("tasks/protocol projector: %w", err)
	}
	tasksService, err := tasksprotocol.NewService(tasksProjector,
		tasksprotocol.WithBus(bus),
		tasksprotocol.WithRedactor(red),
		tasksprotocol.WithLogger(opts.logger),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("tasks/protocol service: %w", err)
	}

	// Phase 73c (D-122): the Console Sessions-page Protocol service.
	// The registry is StateStore-backed (Phase 08) — the same store
	// the task registry uses, so the Console Sessions page renders the
	// live session set. The ListerProjector wraps the registry's
	// SessionLister surface; the service's bus + redactor are wired so
	// a cross-tenant `sessions.list` query emits its
	// `audit.admin_scope_used` event (CLAUDE.md §13 — the admin path is
	// never a silent no-op).
	sessionRegistry, err := sessions.New(stateStore, cfg.Sessions, bus)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("sessions registry: %w", err)
	}
	closers = append(closers, sessionRegistry.CloseRegistry)
	sessionsProjector, err := sessionsprotocol.NewListerProjector(sessionRegistry)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("sessions/protocol projector: %w", err)
	}
	sessionsService, err := sessionsprotocol.NewService(sessionsProjector,
		sessionsprotocol.WithBus(bus),
		sessionsprotocol.WithRedactor(red),
		sessionsprotocol.WithLogger(opts.logger),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("sessions/protocol service: %w", err)
	}

	// Phase 73n (D-130): the Console Playground-page Runs service. The
	// override Store is an in-process artifact (the next-message
	// override slot is ephemeral per-runtime state — there is no
	// persistence-shaped seam). Wired unconditionally so the Console
	// Playground page can record next-message overrides out of the box.
	runsService, err := runsprotocol.NewService(runsprotocol.NewStore(),
		runsprotocol.WithBus(bus),
		runsprotocol.WithRedactor(red),
		runsprotocol.WithLogger(opts.logger),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("runs/protocol service: %w", err)
	}

	// Phase 72f / 72g (D-111 / D-112): the single posture surface — the
	// seven read-only posture Protocol methods. The five `runtime.*` /
	// `metrics.*` reads (72f) project the runtime's build identity,
	// per-subsystem health, live counters, configured drivers, and a
	// metrics snapshot; the two `governance.posture` / `llm.posture`
	// reads (72g) project the operator-configured `IdentityTiers`
	// (latent by default) and the bound LLM provider/model/region + the
	// boot-captured MockMode flag. All seven are read-only; a
	// cross-tenant read gates on `auth.ScopeAdmin` (D-079) and the
	// governance / llm cross-tenant reads emit a `*.posture_read_admin`
	// audit event. There is exactly ONE NewPostureSurface call site per
	// binary.
	//
	// §17.6 F3: Counters + Metrics are wired to LIVE runtime state — the
	// task registry's per-identity running/background counts + the
	// session registry's active-session count for Counters, and the
	// MetricsRegistry's bus-fed counter snapshot for Metrics. They are
	// NEVER an empty stub.
	postureSurface, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Build: types.RuntimeInfo{
			BuildVersion:   HarborVersion,
			BuildCommit:    "dev",
			BuildGoVersion: goruntime.Version(),
		},
		Clock:    time.Now,
		BootedAt: time.Now(),
		Health: func(_ context.Context) []types.SubsystemHealth {
			return devPostureHealth(cfg)
		},
		Counters: runtimeposture.CountersProvider(taskReg, sessionRegistry),
		Drivers: func() []types.SubsystemDriver {
			return devPostureDrivers(cfg)
		},
		Metrics:     runtimeposture.MetricsProvider(metricsReg, opts.logger),
		Governance:  governance.NewPostureProvider(governanceConfigFromConfig(cfg.Governance)),
		LLM:         llm.NewPostureProvider(llmCfg),
		Redactor:    red,
		Bus:         bus,
		DisplayName: "harbor dev",
		InstanceID:  devInstanceID(),
	})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("protocol posture surface: %w", err)
	}

	// Phase 72c (D-108): the `search.*` cluster surface — the five
	// search methods backed by the live sessions / tasks / events /
	// artifacts subsystems. Wired so an operator gets a working search
	// surface out of the box (CLAUDE.md §13 — no seam to hand-wire).
	// The events Searcher needs the bus's Replayer capability; the
	// inmem / replay-buffered bus implements it, so a fresh dev stack
	// serves `search.events` too.
	searchDeps := search.Deps{Redactor: red, AdminScope: search.AdminScopeFromAuth}
	searchSessions, err := searchsessions.New(sessionRegistry, searchDeps)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("search sessions: %w", err)
	}
	searchTasks, err := searchtasks.New(sessionRegistry, taskReg, searchDeps)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("search tasks: %w", err)
	}
	searchArtifacts, err := searchartifacts.New(artStore, searchDeps)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("search artifacts: %w", err)
	}
	searchers := []search.Searcher{searchSessions, searchTasks, searchArtifacts}
	if replayer, ok := bus.(events.Replayer); ok {
		searchEvents, eserr := searchevents.New(replayer, searchDeps)
		if eserr != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("search events: %w", eserr)
		}
		searchers = append(searchers, searchEvents)
	}
	searchRegistry, err := search.NewRegistry(searchers...)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("search registry: %w", err)
	}
	searchSurface, err := protocol.NewSearchSurface(searchRegistry, search.AdminScopeFromAuth)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("search surface: %w", err)
	}

	// Phase 72d (D-109): the long-lived `notification.*` Subscriber —
	// the bus consumer that synthesises `notification.*` events from the
	// V1 trigger taxonomy (task.failed, tool.approval_requested, …).
	// §17.6 F2: the Subscriber MUST be constructed and Run() here — a
	// blank import alone only registers the event types; without a live
	// Run the `notification.*` topic has no producer. The goroutine is
	// cancelled + joined on shutdown via the closer below.
	notifSubscriber := notifications.NewSubscriber(bus, opts.logger)
	notifCtx, notifCancel := context.WithCancel(context.Background())
	notifDone := make(chan struct{})
	go func() {
		defer close(notifDone)
		if rerr := notifSubscriber.Run(notifCtx); rerr != nil && !errors.Is(rerr, context.Canceled) {
			opts.logger.ErrorContext(notifCtx, "notifications subscriber exited", slog.Any("error", rerr))
		}
	}()
	closers = append(closers, func(context.Context) error {
		notifCancel()
		<-notifDone
		return nil
	})

	mux, err := transports.NewMux(surface, bus,
		transports.WithLogger(opts.logger),
		transports.WithValidator(validator),
		transports.WithPostureSurface(postureSurface),
		// Phase 72c (D-108): mount the five `search.*` methods.
		transports.WithSearch(searchSurface),
		transports.WithArtifactsSurface(artifactsSurface),
		// Phase 72e: mount the `pause.list` snapshot route. The
		// production path always wires the unified Coordinator + the
		// artifact store + the configured heavy-content threshold so
		// the Console intervention queue works out of the box (no seam
		// for the operator to wire — CLAUDE.md §13).
		transports.WithPauseList(coord, artStore, cfg.Artifacts.HeavyOutputThresholdBytes),
		// Phase 73j (D-118): mount the three `memory.*` read routes for
		// the Console Memory page. The handler reuses the artifact
		// store + heavy-content threshold supplied to WithPauseList for
		// the D-026 heavy-value bypass. When no memory driver is
		// configured (memStore is nil) the routes are left un-mounted.
		transports.WithMemory(memStore, cfg.Memory.Driver),
		// Phase 73f: mount the `tools.*` route family so the Console
		// Tools page has a live Protocol surface.
		transports.WithToolsService(toolsService),
		// Phase 73i: mount the six Console Flows-page routes.
		transports.WithFlows(flowsSurface),
		// Phase 73d: mount the two Console Tasks-page read routes.
		transports.WithTasksService(tasksService),
		// Phase 73e: mount the eight `agents.*` read routes so the
		// Console Agents page has a live Protocol surface.
		transports.WithAgentsService(agentsService),
		// Phase 73c (D-122): mount the two Console Sessions-page routes
		// (`sessions.list` / `sessions.inspect`) so the Console Sessions
		// page has a live Protocol surface.
		transports.WithSessionsService(sessionsService),
		// Phase 73n (D-130): mount the Console Playground-page route
		// (`runs.set_overrides`) so the Playground can record
		// next-message reasoning-effort / temperature / max-tokens /
		// system-prompt overrides.
		transports.WithRunsService(runsService),
		// Phase 73m (D-129): mount the `auth.rotate_token` route so the
		// Console Settings page "Rotate token" action has a live
		// Protocol surface.
		transports.WithAuthSurface(rotateSurface),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("transports: %w", err)
	}

	// Health / readiness — small in-process surface that the preflight
	// gate hits to confirm boot. /healthz returns 200 with a JSON
	// body once the server starts serving; /readyz is reserved for a
	// later phase that gates "ready" on dep health (state migration
	// applied, LLM provider reachable, etc.).
	subcommandLabel := opts.subcommandLabel
	if subcommandLabel == "" {
		subcommandLabel = "dev"
	}
	router := http.NewServeMux()
	healthzBody := fmt.Sprintf(`{"status":"ok","subcommand":%q}`, subcommandLabel)
	router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(healthzBody))
	})
	router.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	// Phase 66 / D-100 — `harbor dev` draft-save scaffolding. The
	// draft store materialises agent skeletons under `.harbor/drafts/
	// <tenant>/<user>/<session>/<draft_id>/` (operator's working dir;
	// scoped by identity to keep concurrent operators isolated per §6).
	// The handler is wrapped in the same auth.Middleware as the Phase
	// 60 transports so every draft endpoint inherits the JWT
	// validator + identity-in-ctx invariant. Mounted on
	// `/v1/dev/drafts/` — Go's http.ServeMux longest-prefix-match
	// resolves this BEFORE the `/v1/` Protocol catch-all below.
	draftRoot := filepath.Join(".", ".harbor", "drafts")
	draftStore, err := devdraft.NewStore(devdraft.Options{
		Root:   draftRoot,
		Bus:    bus,
		Logger: opts.logger,
	})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("devdraft: %w", err)
	}
	draftHandler, err := devdraft.NewHandler(draftStore, opts.logger)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("devdraft: handler: %w", err)
	}
	// Auth-wrap so the draft handler reads identity from ctx via the
	// same path the Protocol transports do — keeps the §6 identity-
	// is-mandatory invariant uniform across every authenticated
	// surface mounted on the dev mux.
	draftMW := auth.Middleware(validator, auth.MWLogger(opts.logger))
	router.Handle(devdraft.RoutePrefix+"/", draftMW(draftHandler))

	// Forward every other Protocol-prefixed path to the Phase 60 mux.
	// The draft handler is registered above; this catch-all picks up
	// everything else under /v1/.
	router.Handle("/v1/", mux)

	// Phase 73m (D-129): when booted via `harbor console`, mount the
	// embedded SvelteKit Console build at `/` with SPA fallback. The
	// Protocol surface (`/v1/*`, `/healthz`) registered above takes
	// precedence — Go's http.ServeMux longest-prefix-match resolves a
	// `/v1/...` request to the Protocol mux and only a non-`/v1/`
	// non-`/healthz` path falls through to the Console asset handler.
	// `harbor dev` leaves serveConsole false — the Console build is
	// NEVER embedded into the dev-loop surface (D-091 binding rule).
	if opts.serveConsole {
		consoleHandler, err := newConsoleAssetHandler(opts.logger)
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("console assets: %w", err)
		}
		router.Handle("/", consoleHandler)
	}

	bindAddr := fmt.Sprintf("127.0.0.1:%d", opts.port)
	if opts.bindAddr != "" {
		// runDev's `HARBOR_BIND` parse threads the override here.
		// bootDevStack itself does NOT read HARBOR_BIND from env —
		// the read happens once in `runDev` and propagates via the
		// explicit opts field (D-104). Reading the env directly here
		// caused cmd/harbor tests that construct `devBootOptions`
		// with `port: 0` to leak-inherit the preflight harness's
		// HARBOR_BIND value and try to bind the preflight server's
		// port under parallel-batch load.
		bindAddr = opts.bindAddr
	}

	server := &http.Server{
		Addr:              bindAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		// Long read/write timeouts because SSE streams hold the conn
		// open. The dev server is not production-tuned — operators
		// who need different limits can run their own Protocol server
		// behind the runtime.
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
	}

	// Mint and print a default-identity dev token so an operator can
	// curl the protocol surface without writing JWT-signing code.
	token, err := devSigner.SignDevToken(time.Now(), DevTenant, DevUser, DevSession, []string{"admin", "console:fleet"})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("dev token: %w", err)
	}

	// Phase 75a (D-131): the dev-only runtime-entity fixture seeder.
	// When `HARBOR_DEV_SEED_FIXTURES=1`, populate the registries with
	// a deterministic fixture set so the Console e2e Playwright suite
	// (which boots `harbor console`) renders real rows instead of
	// SKIPping every data-shaped per-page assertion. Gated behind an
	// explicit env var, never the default — a production runtime boots
	// empty (the §13 dev-only escape-hatch posture; see devseed.go).
	if os.Getenv(EnvDevSeedFixtures) == "1" {
		_, _ = fmt.Fprintln(opts.stderr, DevSeedBanner)
		if seedErr := seedDevFixtures(ctx, devSeedDeps{
			sessions:  sessionRegistry,
			agents:    agentRegistry,
			tasks:     taskReg,
			artifacts: artStore,
			memory:    memStore,
			tools:     toolCat,
			flows:     flowRegistry,
			bus:       bus,
			logger:    opts.logger,
		}); seedErr != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("devseed: %w", seedErr)
		}
	}

	return &devStack{
		cfg:             cfg,
		logger:          opts.logger,
		stderr:          opts.stderr,
		server:          server,
		bindAddr:        bindAddr,
		devToken:        token,
		allowMock:       opts.allowMock,
		effectiveDriver: driverName,
		label:           subcommandLabel,
		closeFns:        closers,
		bus:             bus,
		toolCatalog:     toolCat,
		coordinator:     coord,
		appliedGates:    appliedGates,
		runLoop:         runLoop,
		runLoopDriver:   runLoopDriver,
		draftStore:      draftStore,
	}, nil
}

// devStack is the runtime bundle a successful bootDevStack returns.
// `serve` binds the listener and runs until ctx cancels. `close`
// runs every subsystem's Close in reverse dependency order.
type devStack struct {
	cfg             *config.Config
	logger          *slog.Logger
	stderr          interface{ Write(p []byte) (int, error) }
	server          *http.Server
	bindAddr        string
	devToken        string
	allowMock       bool
	effectiveDriver string
	// label is the subcommand name ("dev" or "console") used in the
	// boot log lines. The stderr `HARBOR_DEV_TOKEN=` / `HARBOR_DEV_BOUND=`
	// contract lines keep the `DEV_` prefix regardless of label — the
	// preflight + e2e harnesses parse those exact prefixes (D-104).
	label    string
	closeFns []func(context.Context) error
	// bus is the canonical event bus. Exposed so regression tests
	// can assert wire-side invariants — F1 from the Wave 11.5 §17.5
	// audit (pauseresume.New must be bus-wired in production so
	// D-096's typed Decision marker reaches subscribers).
	bus events.EventBus
	// Phase 64a (D-090) surfaces — the tool catalog + Coordinator are
	// constructed by bootDevStack; future phases that grow per-tool
	// dispatch logic read these from the stack.
	toolCatalog  tools.ToolCatalog
	coordinator  pauseresume.Coordinator
	appliedGates map[string]*toolapproval.ApprovalGate
	// D-097 surfaces — the shared `steering.RunLoop` and its per-task
	// driver. The driver's lifecycle is tied to the stack via its
	// Close func registered in closers; tests inspect the RunLoop
	// directly for the wire-side APPROVE/REJECT bridge invariants.
	runLoop       *steering.RunLoop
	runLoopDriver *perTaskRunLoopDriver

	// Phase 66 / D-100 — the draft-save scaffolding store. Exposed
	// so tests + the devstack helper can inspect / probe the on-disk
	// state without going through the HTTP surface. Production code
	// reaches the store ONLY via the HTTP handler the dev mux mounts
	// at devdraft.RoutePrefix.
	draftStore *devdraft.Store
}

// serve binds the listener and runs the http.Server until ctx
// cancels. On graceful-shutdown, the server gets
// `cfg.Server.ShutdownGracePeriod` to drain.
func (s *devStack) serve(ctx context.Context) error {
	// Mock-LLM banner was printed at boot (registerMockIfDevAllowMock);
	// we DO NOT repeat it here to avoid double-emission. The
	// boot-time banner is the §13 amendment surface — every boot
	// prints it exactly once on stderr.
	label := s.label
	if label == "" {
		label = "dev"
	}
	s.logger.InfoContext(ctx, "harbor "+label+": starting Protocol server",
		slog.String("bind", s.bindAddr),
		slog.String("driver_llm", s.effectiveDriver),
		slog.String("driver_state", s.cfg.State.Driver),
		slog.String("driver_events", s.cfg.Events.Driver),
		slog.String("driver_memory", s.cfg.Memory.Driver),
		slog.String("memory_strategy", s.cfg.Memory.Strategy),
		slog.Bool("dev_allow_mock", s.allowMock),
	)
	s.logger.InfoContext(ctx, "harbor "+label+": dev token minted",
		slog.String("kid", DevKID),
		slog.String("tenant", DevTenant),
		slog.String("user", DevUser),
		slog.String("session", DevSession),
		slog.Duration("ttl", DevTokenTTL),
	)
	// Print the dev token to stderr so operators can `export
	// HARBOR_DEV_TOKEN=$(harbor dev 2>&1 | grep ...` — wait, simpler:
	// emit a single named-prefix line.
	_, _ = fmt.Fprintf(s.stderr, "HARBOR_DEV_TOKEN=%s\n", s.devToken)

	// Bind the listener up front (rather than using ListenAndServe) so
	// (a) we can support HARBOR_BIND=host:0 ephemeral-port allocation
	// — `scripts/preflight.sh` uses this so sibling worktrees can run
	// the gate concurrently without colliding on a hardcoded
	// `:18080` — and (b) we can emit a parseable
	// `HARBOR_DEV_BOUND=<host:port>` line on stderr that the preflight
	// harness reads to discover the actual port the OS handed us.
	// D-104 pins this contract; the line MUST appear exactly once per
	// boot, on stderr, with that exact prefix.
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.server.Addr, err)
	}
	boundAddr := listener.Addr().String()
	// Refresh s.bindAddr so any subsequent log / observability surface
	// reflects the actual bound addr (matters when Addr was host:0).
	s.bindAddr = boundAddr
	_, _ = fmt.Fprintf(s.stderr, "HARBOR_DEV_BOUND=%s\n", boundAddr)
	s.logger.InfoContext(ctx, "harbor "+label+": listener bound", slog.String("bind", boundAddr))

	listenErr := make(chan error, 1)
	go func() {
		err := s.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			return
		}
		listenErr <- nil
	}()

	select {
	case err := <-listenErr:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-ctx.Done():
		grace := s.cfg.Server.ShutdownGracePeriod
		if grace <= 0 {
			grace = 30 * time.Second
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		s.logger.Info("harbor "+label+": draining", slog.Duration("grace", grace))
		_ = s.server.Shutdown(shutdownCtx)
		return nil
	}
}

// close runs every subsystem's Close in reverse dependency order.
// Idempotent — safe to call after `serve` returned normally.
func (s *devStack) close(ctx context.Context) {
	for i := len(s.closeFns) - 1; i >= 0; i-- {
		_ = s.closeFns[i](ctx)
	}
}

// applyToolCatalogWiring is the Phase 64a (D-090) integration point:
// reads `cfg.Tools.Entries` and applies the catalog wiring builder
// against `cat`. Returns the set of applied gates + OAuth providers
// so the dev stack can register their Close functions.
//
// When `cfg.Tools.Entries` is empty, this is a no-op — the catalog
// stays as the operator registered it (via in-process Go code,
// Phase 27/28/29 manifests, etc.).
//
// Fail-loud semantics: every error path returns a wrapped error;
// missing/unknown tool/policy/provider crashes boot. CLAUDE.md §13
// amendment.
//
// OAuth provider construction lands here per D-095 (closes issue
// #116 and D-090's "OAuth provider construction deferred" deferral).
// The function walks `cfg.Tools.OAuthProviders[]`, dispatches each
// entry to the `internal/tools/auth` driver registry by `Driver`
// name, and populates the catalog Builder's `Deps.OAuthProviders`
// map keyed by `Name`. Credentials enter via env-var indirection
// (§7 rule 2 — never hardcoded, never logged) — `os.Getenv` resolves
// `ClientIDEnv` / `ClientSecretEnv` at this boundary and the dev
// stack reads the KEK from the env var named in
// `cfg.Tools.OAuthTokenKEKEnv` (32 hex bytes; the Sealer enforces
// length). Every failure is loud: empty / wrong-length KEK, missing
// env-var contents, unknown driver, or factory errors all crash
// boot with a wrapped error naming the offending field.
func applyToolCatalogWiring(
	ctx context.Context,
	cfg *config.Config,
	cat tools.ToolCatalog,
	coord pauseresume.Coordinator,
	bus events.EventBus,
	red audit.Redactor,
	stateStore state.StateStore,
) (map[string]*toolapproval.ApprovalGate, map[string]toolauth.OAuthProvider, error) {
	gates := make(map[string]*toolapproval.ApprovalGate)
	providers := make(map[string]toolauth.OAuthProvider)

	// D-095 — construct OAuth providers BEFORE the catalog Builder
	// runs so the Builder's `Deps.OAuthProviders` lookup resolves.
	// The dev stack constructs one TokenStore + Sealer (shared across
	// every provider, single operator-supplied KEK) and passes them
	// into every driver factory. An empty `OAuthProviders` list is a
	// no-op — the binary still boots cleanly when no operator
	// declares OAuth bindings.
	if len(cfg.Tools.OAuthProviders) > 0 {
		kek, err := resolveOAuthTokenKEK(cfg.Tools.OAuthTokenKEKEnv)
		if err != nil {
			return nil, nil, err
		}
		sealer, err := toolauth.NewAESGCMSealer(kek)
		if err != nil {
			return nil, nil, fmt.Errorf("tools/oauth: sealer: %w", err)
		}
		tokenStore, err := toolauth.NewTokenStore(stateStore, sealer)
		if err != nil {
			return nil, nil, fmt.Errorf("tools/oauth: token store: %w", err)
		}
		deps := toolauth.FactoryDeps{
			Store:       tokenStore,
			Bus:         bus,
			Redactor:    red,
			Coordinator: coord,
		}
		for i, p := range cfg.Tools.OAuthProviders {
			clientID := os.Getenv(p.ClientIDEnv)
			if clientID == "" {
				return nil, nil, fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d]): env var %q (named by client_id_env) is unset or empty",
					p.Name, i, p.ClientIDEnv)
			}
			clientSecret := os.Getenv(p.ClientSecretEnv)
			if clientSecret == "" {
				return nil, nil, fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d]): env var %q (named by client_secret_env) is unset or empty",
					p.Name, i, p.ClientSecretEnv)
			}
			pcfg := toolauth.ProviderConfig{
				Name:         p.Name,
				ClientID:     clientID,
				ClientSecret: clientSecret,
				Scopes:       append([]string(nil), p.Scopes...),
				AuthURL:      p.AuthURL,
				TokenURL:     p.TokenURL,
				RedirectURL:  p.RedirectURL,
				Extra:        p.Extra,
			}
			prov, err := toolauth.Resolve(ctx, p.Driver, pcfg, deps)
			if err != nil {
				return nil, nil, fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d], driver=%q): %w",
					p.Name, i, p.Driver, err)
			}
			providers[p.Name] = prov
		}
	}

	if len(cfg.Tools.Entries) == 0 {
		return gates, providers, nil
	}
	b := toolcatalog.New(cfg.Tools.Entries, toolcatalog.Deps{
		Catalog:        cat,
		Coordinator:    coord,
		Bus:            bus,
		Redactor:       red,
		OAuthProviders: providers,
		AppliedGates:   gates,
	})
	if err := b.Apply(ctx); err != nil {
		return nil, nil, err
	}
	return gates, providers, nil
}

// plannerConfigFromConfig maps the operator-facing `config.PlannerConfig`
// onto the registry-facing `planner.PlannerConfig` boundary. D-103
// (closes issue #126). Empty Driver defaults to "react" — the V1
// reference planner — so a config that omits the planner block boots
// unchanged from the pre-D-103 hardcoded path. The boundary copy
// matches the D-095 OAuth-provider precedent (`internal/config` keeps
// its own struct shape so it doesn't import `internal/planner`).
func plannerConfigFromConfig(cfg config.PlannerConfig) planner.PlannerConfig {
	driver := cfg.Driver
	if driver == "" {
		driver = "react"
	}
	var extra map[string]string
	if len(cfg.Extra) > 0 {
		extra = make(map[string]string, len(cfg.Extra))
		for k, v := range cfg.Extra {
			extra[k] = v
		}
	}
	return planner.PlannerConfig{
		Driver:   driver,
		MaxSteps: cfg.MaxSteps,
		Extra:    extra,
	}
}

// resolveOAuthTokenKEK reads the named env var and decodes its value
// as a 32-byte hex-encoded key-encryption key for AES-256-GCM token
// encryption at rest. Fail-loud per §13 amendment: empty env or
// wrong-length decoded key crashes boot with a wrapped error naming
// the env var.
func resolveOAuthTokenKEK(envName string) ([]byte, error) {
	if envName == "" {
		return nil, fmt.Errorf("tools/oauth: tools.oauth_token_kek_env must be set (validated upstream — this is a sanity check)")
	}
	raw := os.Getenv(envName)
	if raw == "" {
		return nil, fmt.Errorf("tools/oauth: env var %q (named by tools.oauth_token_kek_env) is unset or empty — operator must populate a 32-byte hex-encoded KEK",
			envName)
	}
	kek, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: env var %q is not valid hex: %w", envName, err)
	}
	if len(kek) != toolauth.KEKSizeBytes {
		return nil, fmt.Errorf("tools/oauth: env var %q decoded to %d bytes, want %d (AES-256-GCM)",
			envName, len(kek), toolauth.KEKSizeBytes)
	}
	return kek, nil
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
// to the sentinel.
func bootErrorToCLIError(err error) CLIError {
	switch {
	case errors.Is(err, ErrLLMRequired):
		return CLIError{
			Subcommand: "dev",
			Message:    err.Error(),
			Code:       CodeBootLLMRequired,
			Hint:       "see examples/dev.yaml for the canonical shape; set " + EnvDevAllowMock + "=1 for the dev-only mock escape hatch",
		}
	case errors.Is(err, config.ErrConfigNotFound):
		return CLIError{
			Subcommand: "dev",
			Message:    err.Error(),
			Code:       CodeBootConfigInvalid,
			Hint:       "pass --config or create harbor.yaml in the working directory (try `harbor scaffold --name my-agent`)",
		}
	case errors.Is(err, config.ErrConfigInvalid):
		return CLIError{
			Subcommand: "dev",
			Message:    err.Error(),
			Code:       CodeBootConfigInvalid,
			Hint:       "run `harbor validate` for file:line precision on the failing field",
		}
	default:
		return CLIError{
			Subcommand: "dev",
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

// copyModelProfiles converts the config-package map shape into the
// llm-package ModelProfile map. Each profile field is copied by
// value — both packages own their own struct types so a copy keeps
// the seam decoupled.
// governanceConfigFromConfig projects the operator-supplied
// `config.GovernanceConfig` onto the `governance.Config` shape the
// Phase 72g posture surface reads. Only the read-only posture-relevant
// fields are projected — `DefaultTier` + `IdentityTiers` (each tier's
// `BudgetCeilingUSD` / `RateLimit` / `MaxTokens`). The enforcement
// fields (`Resolver` / `Clock`) stay zero — the posture surface is a
// read-only projection, not the enforcement seam, so a nil resolver is
// correct (every caller's `ResolvedTier` falls back to `DefaultTier`).
//
// An empty `IdentityTiers` in the YAML (the latent default) yields a
// `governance.Config` with an empty tier map — the posture surface
// reports it verbatim and the Console renders the explicit "No tiers
// configured" state.
func governanceConfigFromConfig(in config.GovernanceConfig) governance.Config {
	tiers := make(map[string]governance.TierConfig, len(in.IdentityTiers))
	for name, tc := range in.IdentityTiers {
		tiers[name] = governance.TierConfig{
			BudgetCeilingUSD: tc.BudgetCeilingUSD,
			RateLimit: governance.RateLimitConfig{
				Capacity:       tc.RateLimit.Capacity,
				RefillTokens:   tc.RateLimit.RefillTokens,
				RefillInterval: tc.RateLimit.RefillInterval,
			},
			MaxTokens: tc.MaxTokens,
		}
	}
	return governance.Config{
		DefaultTier:   in.DefaultTier,
		IdentityTiers: tiers,
	}
}

// devPostureHealth builds the Phase 72f `runtime.health` seam for the
// dev boot. The dev stack is in-process and fully assembled by the time
// the posture surface is constructed, so every persistence-shaped
// subsystem reports `ready`.
func devPostureHealth(cfg *config.Config) []types.SubsystemHealth {
	subs := []string{"state", "events"}
	if cfg.Artifacts.Driver != "" {
		subs = append(subs, "artifacts")
	}
	if cfg.Memory.Driver != "" {
		subs = append(subs, "memory")
	}
	out := make([]types.SubsystemHealth, 0, len(subs))
	for _, s := range subs {
		out = append(out, types.SubsystemHealth{Subsystem: s, Status: types.HealthStatusReady})
	}
	return out
}

// devPostureDrivers builds the Phase 72f `runtime.drivers` seam — the
// configured driver name per persistence-shaped subsystem. Never the
// DSN (CLAUDE.md §7) — the driver name only.
func devPostureDrivers(cfg *config.Config) []types.SubsystemDriver {
	out := []types.SubsystemDriver{
		{Subsystem: "state", Driver: cfg.State.Driver},
	}
	if cfg.Artifacts.Driver != "" {
		out = append(out, types.SubsystemDriver{Subsystem: "artifacts", Driver: cfg.Artifacts.Driver})
	}
	if cfg.Memory.Driver != "" {
		out = append(out, types.SubsystemDriver{Subsystem: "memory", Driver: cfg.Memory.Driver})
	}
	if cfg.Events.Driver != "" {
		out = append(out, types.SubsystemDriver{Subsystem: "events", Driver: cfg.Events.Driver})
	}
	return out
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

func copyModelProfiles(in map[string]config.LLMModelProfileConfig) map[string]llm.ModelProfile {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]llm.ModelProfile, len(in))
	for name, p := range in {
		mp := llm.ModelProfile{
			ContextWindowTokens: p.ContextWindowTokens,
			TokenEstimator:      p.TokenEstimator,
			JSONSchemaMode:      p.JSONSchemaMode,
			ReasoningEffort:     llm.ReasoningEffort(p.ReasoningEffort),
			MaxRetries:          p.MaxRetries,
		}
		if p.DefaultMaxTokens != nil {
			v := *p.DefaultMaxTokens
			mp.DefaultMaxTokens = &v
		}
		out[name] = mp
	}
	return out
}

// Compile-time ensure identity is imported (boot wiring reads
// identity.Quadruple under the dev token's claims; the import is also
// used by the dev-cmd integration test via the SignDevToken helper).
var _ identity.Identity
