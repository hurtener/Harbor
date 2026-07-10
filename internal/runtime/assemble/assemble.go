// Package assemble is Harbor's assembly entry point:
// the ONE exported, error-returning config→stack fan-out that
// turns a validated *config.Config into a running runtime stack.
//
// # What this package replaces
//
// Previously the dependency-ordered composition — stores → bus → llm
// → memory → skills → tasks → catalog (builtins + OAuth + approval +
// MCP attach) → sessions → planner → run loop, with reverse-order
// closers and partial-failure cleanup — existed in exactly two places:
// `cmd/harbor/cmd_dev.go::bootDevStack` (package main) and
// `harbortest/devstack`'s unexported `tryAssemble` (gated behind a
// `*testing.T` wrapper). The two copies had already drifted (the
// devstack MCP attach silently dropped the ToolPolicy
// projection; the devstack assembly never constructed cfg-declared
// OAuth providers). Both callers are now thin wrappers over
// `Assemble`; there is no second ordering left to drift.
// one model, no legacy "before" mode).
//
// # Scope
//
// Assemble ends where the network surface begins. Protocol surfaces,
// transports, auth validators, listeners, CORS, the Console, draft
// stores, and the per-task run-loop *driver* (the `task.spawned`
// subscriber) stay with the caller — `cmd/harbor` keeps its
// production driver, `harbortest/devstack` its test-kit mirror, and a
// headless embedder drives `Stack.RunLoop.Run` directly (see
// docs/recipes/embed-harbor-headless.md).
//
// # Lifecycle contract
//
// On error, Assemble returns the PARTIAL *Stack alongside the error so
// the caller's deferred Close drains every subsystem that opened
// before the failure. On success the caller owns the stack and MUST
// call Close (reverse dependency order, idempotent).
//
// # Concurrent reuse
//
// The returned *Stack is a compiled artifact: every field is set once
// during Assemble and never mutated afterwards (Close flips an
// internal once). The composed subsystems carry their own concurrent-reuse
// concurrent-reuse guarantees; per-run state lives in ctx +
// planner.RunContext, never on the stack.
package assemble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	llmsummarizer "github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	agentregistry "github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
	televentbus "github.com/hurtener/Harbor/internal/telemetry/eventbus"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/builtin"
	toolcatalog "github.com/hurtener/Harbor/internal/tools/catalog"
	httpdrv "github.com/hurtener/Harbor/internal/tools/drivers/http"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
	"github.com/hurtener/Harbor/internal/tools/drivers/searchcache"
)

// DefaultMCPIdentity is the fallback identity stamped on MCP
// server-pushed events that arrive without an inflight call when the
// caller supplies no Options.MCPDefaultIdentity. It matches the
// `harbor dev` single-operator triple.
var DefaultMCPIdentity = identity.Identity{TenantID: "dev", UserID: "dev", SessionID: "dev"}

// Options carries the injection points the two existing callers need
// (CLAUDE.md §4.3 / the assembly plan's options-surface rule: the union of
// what cmd/harbor and harbortest/devstack consume today, not a
// speculative embedder wishlist).
type Options struct {
	// Logger receives the pre-telemetry bootstrap warnings (the window
	// before the redactor + bus exist). Once the telemetry Logger is
	// constructed, the assembly threads its Slog() bridge into every
	// subsequently-built subsystem, so subsystem log lines flow
	// through the canonical pipeline.
	// Defaults to slog.Default().
	Logger *slog.Logger

	// LLMSnapshot, when non-nil, overrides the snapshot Assemble would
	// otherwise project from cfg.LLM via llm.SnapshotFromConfig (
	// ). `harbor dev` uses this for the mock-LLM
	// escape hatch; tests flip the driver without rewriting yaml.
	LLMSnapshot *llm.ConfigSnapshot

	// PlannerOverride, when non-nil, replaces the registry-resolved
	// planner concrete. Tests inject stub / scripted / pausing
	// planners; production never sets it.
	PlannerOverride planner.Planner

	// SkillStore, when non-nil, wins over the cfg-opened store. The
	// caller owns its lifecycle (it is NOT added to the closer chain).
	SkillStore skills.SkillStore

	// Embedder, when non-nil, wins over the cfg-opened embedding
	// client. The caller owns its lifecycle (it is NOT added to the
	// closer chain). Tests inject deterministic embedders; headless
	// embedders inject a pre-built client.
	Embedder embeddings.Embedder

	// OAuthProviders pre-populates / overrides entries in the provider
	// map the catalog Builder consults. Entries here win over
	// same-named cfg-built providers; the caller owns their lifecycle.
	OAuthProviders map[string]toolauth.OAuthProvider

	// PreRegisterTools is registered on the catalog BEFORE the builtin
	// registration and the Builder apply, so operator config
	// in cfg.Tools.Entries can wrap in-process fixtures.
	PreRegisterTools []tools.ToolDescriptor

	// RegisterCatalog is an optional callback invoked at the same
	// pre-policy point as PreRegisterTools — on the freshly-constructed
	// catalog, BEFORE builtin registration and BEFORE the catalog
	// Builder wraps every entry with its declared reliability shell,
	// approval gate, and OAuth binding (cfg.Tools.Entries). A compiled
	// in-process tool registered here therefore receives the IDENTICAL
	// wrapping an operator's YAML-declared tool gets. It is an adapter
	// over the one registration seam, never a second registration path:
	// registering the same tool after Assemble returns (a post-assembly
	// Catalog.Register) skips the reliability/approval/OAuth shell.
	// A non-nil error from the callback fails Assemble loud (the partial
	// stack is returned for the caller to drain).
	RegisterCatalog func(catalog tools.ToolCatalog) error

	// MCPDefaultIdentity is the transport-event fallback identity for
	// attached MCP servers. Zero value falls
	// back to DefaultMCPIdentity.
	MCPDefaultIdentity identity.Identity

	// MetricsOptions is threaded into telemetry.NewMetricsRegistry.
	// The devstack injects an in-process manual reader so the kit does
	// not require a metrics-exporter driver per test binary.
	MetricsOptions []telemetry.MetricsOption

	// TelemetryOptions is threaded into telemetry.New.
	// Tests inject telemetry.WithWriter to observe emitted
	// records; production callers pass nothing (stdout handler).
	TelemetryOptions []telemetry.Option

	// TracerOptions is threaded into telemetry.NewTracer.
	// Tests inject telemetry.WithSpanExporter with an
	// in-memory recorder so derived spans are observable without a
	// collector; production callers pass nothing (exporter selection
	// follows cfg.Telemetry.OTelEndpoint — noop without a collector).
	TracerOptions []telemetry.TracerOption

	// ApprovalAuthorizer overrides the resolve-privilege seam threaded
	// into every catalog-built ApprovalGate. Nil
	// defaults to the runtime-vocabulary
	// `approval.NewIdentityAuthorizer()` (originating identity /
	// control scope). The serving binary and the devstack inject the
	// Protocol-side `server.NewProtocolScopeAuthorizer` so wire-driven
	// resolution keeps today\'s admin / console:fleet acceptance.
	ApprovalAuthorizer toolapproval.ResolveAuthorizer

	// SkipCatalog disables the tool-catalog band (search cache,
	// builtins, pause Coordinator, OAuth providers, approval gates, MCP
	// attach, executor). Catalog / Coordinator / Gates / OAuthProviders
	// / MCPRegistry / Executor are nil. Test-kit convenience; the
	// production binary never sets it.
	SkipCatalog bool

	// SkipSteering disables the steering Registry + planner + RunLoop
	// band. Steering / Planner / RunLoop are nil. Test-kit convenience.
	SkipSteering bool

	// SkipRunLoop disables only the RunLoop construction (the steering
	// Registry and planner still build). Test-kit convenience.
	SkipRunLoop bool
}

// Stack is the composed runtime bundle Assemble returns. Fields are
// nil when the corresponding layer was skipped via Options or not
// implied by the cfg (LLM / Memory / Skills follow their cfg blocks).
type Stack struct {
	// Cfg is the *config.Config the stack was assembled from.
	Cfg *config.Config

	// Redactor / State / Bus / Metrics / Artifacts / Tasks / Sessions /
	// Agents are always non-nil after a successful Assemble — the
	// runtime's load-bearing core.
	Redactor  audit.Redactor
	State     state.StateStore
	Bus       events.EventBus
	Metrics   *telemetry.MetricsRegistry
	Artifacts artifacts.ArtifactStore
	Tasks     tasks.TaskRegistry
	Sessions  *sessions.Registry
	Agents    *agentregistry.Registry

	// Telemetry is the canonical redactor-mandatory structured Logger
	// (RFC §6.14), constructed with the bus-paired emitter so
	// Logger.Error emits the slog record AND a `runtime.error` bus
	// event. Always non-nil after a successful Assemble.
	// The Options.Logger slog logger remains the BOOT logger
	// for the pre-redactor bootstrap window only; the subsystems the
	// assembly constructs after this Logger exists receive its Slog()
	// bridge, and request-scoped emission
	// goes through this Logger directly.
	Telemetry *telemetry.Logger

	// Tracer is the canonical OTel tracer (RFC §6.14). Spans are a
	// derivation of the event bus: the assembly starts
	// telemetry.BridgeBusToTracer alongside the metrics bridge.
	// Always non-nil after a successful Assemble; with no collector
	// configured the noop exporter drops the spans (in-process
	// propagation still works).
	Tracer *telemetry.Tracer

	// RunErrorHandler is the production engine run-error handler:
	// it routes the structured engine.RunError
	// through Telemetry.Error so a terminal node failure emits the
	// paired `runtime.error` bus event. Flow composition forwards it
	// via `flow.WithRunErrorHandler(stack.RunErrorHandler)`.
	RunErrorHandler engine.RunErrorHandler

	// LLM / LLMSnapshot are populated when cfg.LLM.Driver (or
	// Options.LLMSnapshot) names a driver. LLMSnapshot is the resolved
	// snapshot the client was opened with — posture surfaces project it.
	LLM         llm.LLMClient
	LLMSnapshot llm.ConfigSnapshot
	// KeyRotator is the admin-write seam for Console-driven LLM key
	// rotation — it swaps the SAME atomic key holder the opened driver
	// reads per call. Populated alongside LLM (nil when no LLM driver is
	// configured). The serving binary wires it into the
	// `governance.rotate_key` service.
	KeyRotator *llm.ProviderKeyRotator

	// Embedder is populated when the cfg carries a non-zero
	// `embeddings` block (or Options.Embedder is set — caller-owned
	// lifecycle). The semantic retrieval modes in Memory / Skills
	// consume it via their Deps; it is also directly usable for
	// à-la-carte embedding (docs/recipes/embed-and-retrieve.md).
	Embedder embeddings.Embedder

	// Memory / Skills follow cfg.Memory.Driver / cfg.Skills.Driver
	// (Options.SkillStore wins for Skills).
	Memory memory.MemoryStore
	Skills skills.SkillStore

	// Catalog band (nil under SkipCatalog).
	Catalog        tools.ToolCatalog
	Coordinator    pauseresume.Coordinator
	Gates          map[string]*toolapproval.ApprovalGate
	OAuthProviders map[string]toolauth.OAuthProvider
	MCPRegistry    *mcpdrv.Registry
	Executor       steering.ToolExecutor

	// MCPToolContext is the MCP Apps tool-context store the MCP providers
	// capture through (input + lowered result behind a declared `ui://`
	// app) and the host reads back from for `mcp.apps.tool_context`. Nil
	// under SkipCatalog (no MCP providers attach). Wired into every
	// Provider so a planner-path app tool call's context is captured.
	MCPToolContext *mcpconsole.ToolContextStore

	// Steering band (nil under SkipSteering; RunLoop additionally nil
	// under SkipRunLoop or when no planner could be resolved).
	Steering *steering.Registry
	Planner  planner.Planner
	RunLoop  *steering.RunLoop

	// Compression is the trajectory-compression runner:
	// planner.NewCompressionRunner over the LLM-backed
	// summarizer.NewTrajectorySummariser. Non-nil only when
	// cfg.Planner.TokenBudget > 0 (and the steering band ran) — the
	// per-task run-loop drivers project it onto RunSpec.Compression
	// alongside Base.Budget.TokenBudget. Nil = compression off.
	Compression *planner.CompressionRunner

	closeOnce sync.Once
	closers   []func(context.Context) error
}

// Close runs every subsystem's Close in reverse dependency order and
// joins the errors. Idempotent: a second Close is a no-op. Safe on a
// partial stack returned alongside an Assemble error.
func (s *Stack) Close(ctx context.Context) error {
	var errs []error
	s.closeOnce.Do(func() {
		for i := len(s.closers) - 1; i >= 0; i-- {
			if err := s.closers[i](ctx); err != nil {
				errs = append(errs, err)
			}
		}
		s.closers = nil
	})
	return errors.Join(errs...)
}

// Assemble composes the runtime stack from a validated *config.Config.
// See the package doc for scope, the lifecycle contract (partial stack
// on error), and what stays with the caller.
func Assemble(ctx context.Context, cfg *config.Config, opts Options) (*Stack, error) {
	if cfg == nil {
		return nil, fmt.Errorf("assemble: cfg is required (call config.Load, or config.Defaults + ValidateCore for headless embedding)")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	stack := &Stack{Cfg: cfg}

	// Audit. The redactor has no Close in the current interface —
	// nothing to register.
	red, err := audit.Open(ctx, cfg.Audit)
	if err != nil {
		return stack, fmt.Errorf("audit: %w", err)
	}
	stack.Redactor = red

	// State BEFORE events (reconciliation): the durable event-log
	// driver shares the runtime's StateStore through events.OpenWith,
	// so the store must outlive the bus — open first, close last.
	stateStore, err := state.Open(ctx, cfg.State)
	if err != nil {
		return stack, fmt.Errorf("state: %w", err)
	}
	stack.State = stateStore
	stack.closers = append(stack.closers, stateStore.Close)

	bus, err := events.OpenWith(ctx, cfg.Events, red, events.Deps{State: stateStore})
	if err != nil {
		return stack, fmt.Errorf("events: %w", err)
	}
	stack.Bus = bus
	stack.closers = append(stack.closers, bus.Close)

	// the canonical telemetry Logger — redactor-
	// mandatory, identity-attributed, bus-paired (RFC §6.14:
	// "Logger.Error emits both an slog record AND a paired
	// runtime.error bus event"). Constructed the moment the redactor +
	// bus exist; the bare Options.Logger remains only the bootstrap /
	// wiring logger.
	tlog, err := telemetry.New(cfg.Telemetry, red,
		append([]telemetry.Option{telemetry.WithBusEmitter(televentbus.New(bus))}, opts.TelemetryOptions...)...)
	if err != nil {
		return stack, fmt.Errorf("telemetry logger: %w", err)
	}
	stack.Telemetry = tlog

	// checkpoint audit: from this point on, every subsystem the
	// assembly constructs with a *slog.Logger (the notifications
	// subscriber, the pause sweeper, the dispatch executor, the MCP
	// attach loop, the search-cache warn path, the run loop) logs
	// through the telemetry pipeline — ctx identity stamping,
	// mandatory redaction, bus-paired errors. The bare Options.Logger
	// served only the pre-telemetry bootstrap window above, which
	// makes the "bootstrap-only" posture true inside the
	// assembly.
	logger = tlog.Slog()

	// the production engine run-error handler —
	// `engine.WithRunErrorHandler`\'s godoc made true. Flow composition
	// forwards it via `flow.WithRunErrorHandler(stack.RunErrorHandler)`
	// so a terminal node failure reaches Telemetry.Error and the
	// paired runtime.error bus event.
	stack.RunErrorHandler = func(hctx context.Context, re *engine.RunError) {
		if re == nil {
			return
		}
		tlog.Error(hctx, "engine: run failed",
			slog.String("node", re.NodeName),
			slog.String("code", string(re.Code)),
			slog.String("error", re.Message),
		)
	}

	// the MetricsRegistry — metrics are a derivation of
	// the event bus. BridgeBusToMetrics fans every bus event
	// into the registry's counter so `metrics.snapshot` projects live
	// numbers — never an empty stub (§17.6 F3). The Admin-scope filter
	// feeds the fleet-wide bridge; it does not widen the isolation
	// boundary (a runtime-internal infra consumer).
	metricsReg, metricsShutdown, err := telemetry.NewMetricsRegistry(cfg.Telemetry, opts.MetricsOptions...)
	if err != nil {
		return stack, fmt.Errorf("telemetry metrics: %w", err)
	}
	stack.Metrics = metricsReg
	stack.closers = append(stack.closers, metricsShutdown)
	metricsBridgeStop, err := telemetry.BridgeBusToMetrics(ctx, bus, metricsReg, events.Filter{Admin: true})
	if err != nil {
		return stack, fmt.Errorf("telemetry metrics bridge: %w", err)
	}
	stack.closers = append(stack.closers, func(context.Context) error { metricsBridgeStop(); return nil })

	// the OTel tracer + the bus→tracer bridge
	// — traces become a first-class derivation of the event bus,
	// symmetric with metrics. Exporter selection follows
	// cfg.Telemetry.OTelEndpoint (noop without a collector); the
	// production filter scopes the bridge to the canonical lifecycle
	// pairs so a chatty bus never becomes span flood.
	tracer, tracerShutdown, err := telemetry.NewTracer(cfg.Telemetry, opts.TracerOptions...)
	if err != nil {
		return stack, fmt.Errorf("telemetry tracer: %w", err)
	}
	stack.Tracer = tracer
	stack.closers = append(stack.closers, tracerShutdown)
	traceBridgeStop, err := telemetry.BridgeBusToTracer(ctx, bus, tracer, telemetry.DefaultTraceBridgeFilter())
	if err != nil {
		return stack, fmt.Errorf("telemetry trace bridge: %w", err)
	}
	stack.closers = append(stack.closers, func(context.Context) error { traceBridgeStop(); return nil })

	artStore, err := artifacts.Open(ctx, cfg.Artifacts)
	if err != nil {
		return stack, fmt.Errorf("artifacts: %w", err)
	}
	stack.Artifacts = artStore
	stack.closers = append(stack.closers, artStore.Close)

	// ── governance enforcement assembly ─────────
	// Populated `governance.identity_tiers` ENFORCE: the Subsystem
	// (MaxTokens → RateLimiter → CostAccumulator) is built EAGERLY here
	// — a construction failure fails the boot loud (§13) — and
	// installed via governance.SetFactory BEFORE llm.Open composes the
	// wrapper chain, so PreCall gates every Complete on the assembled
	// client. Empty tiers CLEAR the factory: the latent default
	// must hold for THIS stack even when an earlier stack in the same
	// process installed a factory. The seam is process-global (second
	// SetFactory wins) — the binary assembles exactly one stack;
	// multi-runtime embedders compose governance.Wrap per stack instead
	// (see governance.SetFactory's godoc).
	// govSub is captured for the runtime governance-cache gauge below; it
	// stays nil in the latent no-enforcement default (empty tiers), in
	// which case the gauge reports zero via governance.CacheLen(nil).
	var govSub governance.Subsystem
	if len(cfg.Governance.IdentityTiers) > 0 {
		sub, govErr := governance.NewSubsystemFromConfig(
			governance.ConfigFromOperator(cfg.Governance), stateStore, bus)
		if govErr != nil {
			return stack, fmt.Errorf("governance: %w", govErr)
		}
		govSub = sub
		governance.SetFactory(func(llm.ConfigSnapshot, llm.Deps) (governance.Subsystem, error) {
			return govSub, nil
		})
		stack.closers = append(stack.closers, func(context.Context) error {
			governance.ClearFactory()
			return nil
		})
	} else {
		governance.ClearFactory()
	}
	// ── end governance band ───────────────────────────────

	// Runtime observability gauges. Register the low-cardinality
	// internal-size gauges on the MetricsRegistry now that the bus + the
	// governance enforcement subsystem (when configured) exist. The
	// readings flow to /metrics, OTLP, and metrics.snapshot alike.
	//
	// This is the SINGLE wiring site for both the binary (cmd/harbor) and
	// the test kit (harbortest/devstack) — both assemble through this
	// function, so the two boot paths cannot diverge (the parity failure
	// mode CLAUDE.md §17.6 warns about). The bus drop gauge is sourced via
	// the optional events.DroppedCounter capability; the governance gauge
	// via governance.CacheLen. The engine gauges (active runs, capacity
	// entries) are left nil here: this planner/RunLoop-shaped stack hosts
	// no engine.Engine node-graph, and the gauge seam skips a nil
	// callback. A future engine-graph runtime sources them from its
	// engine's ActiveRunCount / CapacityEntryCount accessors.
	var eventsDropped func() int64
	if dc, ok := bus.(events.DroppedCounter); ok {
		eventsDropped = dc.DroppedTotal
	}
	if err := metricsReg.RegisterRuntimeGauges(telemetry.RuntimeGaugeSource{
		GovernanceCacheEntries: func() int64 { return int64(governance.CacheLen(govSub)) },
		EventsDropped:          eventsDropped,
	}); err != nil {
		return stack, fmt.Errorf("telemetry runtime gauges: %w", err)
	}

	// LLM — opened when the cfg names a driver (or the caller pinned a
	// snapshot). Fail-loud LLM *requirement* policy is the binary's
	// concern (`harbor dev` validates before assembling); a headless
	// embedder that wants no LLM gets a stack without planner/RunLoop.
	if cfg.LLM.Driver != "" || opts.LLMSnapshot != nil {
		llmCfg := llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts)
		if opts.LLMSnapshot != nil {
			llmCfg = *opts.LLMSnapshot
		}
		stack.LLMSnapshot = llmCfg
		// The shared, atomically-swappable primary-key holder for
		// Console-driven key rotation. Created here so the SAME holder is
		// both injected into the driver (the per-call read path) and
		// exposed on the Stack as a KeyRotator (the admin write path).
		liveKey := llm.NewLiveKey()
		llmClient, llmErr := llm.Open(ctx, llmCfg, llm.Deps{
			Artifacts: artStore,
			Bus:       bus,
			LiveKey:   liveKey,
		})
		if llmErr != nil {
			return stack, fmt.Errorf("llm: %w", llmErr)
		}
		stack.LLM = llmClient
		stack.KeyRotator = llm.NewProviderKeyRotator(llmCfg.Provider, liveKey)
		stack.closers = append(stack.closers, llmClient.Close)
	}

	// Embeddings: opened when the operator configured the block (or
	// the caller injected one). The semantic retrieval modes consume
	// it below; misconfiguration (semantic mode + zero block) is
	// already rejected by the config validator, and the memory /
	// skills registries fail loudly again if an embedder is missing.
	stack.Embedder = opts.Embedder
	if stack.Embedder == nil && !cfg.Embeddings.IsZero() {
		emb, embErr := embeddings.Open(ctx, embeddings.SnapshotFromConfig(cfg.Embeddings), embeddings.Deps{})
		if embErr != nil {
			return stack, fmt.Errorf("embeddings: %w", embErr)
		}
		stack.Embedder = emb
		stack.closers = append(stack.closers, emb.Close)
	}

	// Memory: ONE memory.Open serves every driver ×
	// strategy; the Summarizer threads through Deps. For
	// rolling_summary the Summarizer defaults to the configured LLM —
	// no separate summariser model, no stub fallback (CLAUDE.md §13);
	// rolling_summary without an LLM fails loud.
	if cfg.Memory.Driver != "" {
		memCfg := memory.SnapshotFromConfig(cfg.Memory)
		var summarizer memory.Summarizer
		if memCfg.Strategy == memory.StrategyRollingSummary {
			if stack.LLM == nil {
				return stack, fmt.Errorf("memory: strategy=rolling_summary requires an LLM (configure llm) so the default Summarizer can be built")
			}
			s, sErr := llmsummarizer.New(stack.LLM,
				llmsummarizer.WithModel(cfg.Memory.Summarizer.Model),
				llmsummarizer.WithSystemPromptExtension(cfg.Memory.Summarizer.Prompt))
			if sErr != nil {
				return stack, fmt.Errorf("summarizer: %w", sErr)
			}
			summarizer = s
		}
		ms, openErr := memory.Open(ctx, memCfg, memory.Deps{
			State:      stateStore,
			Bus:        bus,
			Summarizer: summarizer,
			Embedder:   stack.Embedder,
		})
		if openErr != nil {
			return stack, fmt.Errorf("memory: %w", openErr)
		}
		stack.Memory = ms
		stack.closers = append(stack.closers, ms.Close)
	}

	// Skills: optional. An explicit Options
	// SkillStore wins (caller-owned lifecycle); otherwise the cfg block
	// opens via the exported projection.
	stack.Skills = opts.SkillStore
	if stack.Skills == nil && cfg.Skills.Driver != "" {
		ss, openErr := skills.Open(ctx, skills.SnapshotFromConfig(cfg.Skills), skills.Deps{Bus: bus, Embedder: stack.Embedder})
		if openErr != nil {
			return stack, fmt.Errorf("skills: %w", openErr)
		}
		stack.Skills = ss
		stack.closers = append(stack.closers, ss.Close)
	}

	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store:    stateStore,
		Bus:      bus,
		Redactor: red,
		Cfg:      cfg.Tasks,
	})
	if err != nil {
		return stack, fmt.Errorf("tasks: %w", err)
	}
	stack.Tasks = taskReg
	stack.closers = append(stack.closers, taskReg.Close)

	if !opts.SkipCatalog {
		if err := assembleCatalogBand(ctx, cfg, opts, stack, logger); err != nil {
			return stack, err
		}
	}

	// the StateStore-backed SessionRegistry. Sessions are
	// per-request + create-on-first-use (no boot-time Open of a fixed
	// session); the persistent catalog re-discovers sessions across
	// restarts. RFC §6.9: GC never reaps a session with a RUNNING task
	// — the TaskRegistry-backed probe is the enforcement (SDK friction
	// audit B2).
	sessionRegistry, err := sessions.New(stateStore, cfg.Sessions, bus,
		sessions.WithGCPolicy(sessions.GCPolicy{
			IdleTTL:       cfg.Sessions.IdleTTL,
			HardCap:       cfg.Sessions.HardCap,
			SweepInterval: cfg.Sessions.SweepInterval,
			RunningProbe:  sessions.TaskRunningProbe(taskReg),
		}))
	if err != nil {
		return stack, fmt.Errorf("sessions registry: %w", err)
	}
	stack.Sessions = sessionRegistry
	stack.closers = append(stack.closers, sessionRegistry.CloseRegistry)

	// the Agent Registry — the per-runtime-instance
	// subsystem owning agent registration identity,
	// persisted through the same StateStore as the rest of the runtime.
	agentRegistry, err := agentregistry.New(agentregistry.Deps{
		Store:    stateStore,
		Bus:      bus,
		Redactor: red,
	})
	if err != nil {
		return stack, fmt.Errorf("agent registry: %w", err)
	}
	stack.Agents = agentRegistry
	// every constructed subsystem registers
	// its Close — the V1 Registry flips a closed flag, but a future
	// driver that owns goroutines must not surface as a leak.
	stack.closers = append(stack.closers, agentRegistry.Close)

	if !opts.SkipSteering {
		if err := assembleSteeringBand(ctx, cfg, opts, stack, logger); err != nil {
			return stack, err
		}
	}

	// the long-lived `notification.*` Subscriber —
	// the bus consumer that synthesises notification.* events from the
	// V1 trigger taxonomy. The subscription must be established before
	// Assemble returns: without a subscribed-and-consuming Subscriber
	// the notification.* topic has no producer, and any trigger event
	// published in the window between Assemble returning and the
	// goroutine reaching bus.Subscribe is silently lost (the inmem bus
	// does not back-fill a plain Subscribe with pre-subscription
	// events). Start establishes the subscription synchronously and
	// returns the blocking consume closure; the closure runs in a
	// goroutine that is cancelled + joined on Close.
	notifSubscriber := notifications.NewSubscriber(bus, logger)
	// context.Background is deliberate: the subscriber's lifetime is the
	// stack's (bounded by the closer below), not the assembly ctx's.
	notifCtx, notifCancel := context.WithCancel(context.Background())
	// Start subscribes synchronously — the notification.* topic is
	// guaranteed to have a live consumer before Assemble returns. A
	// Subscribe failure means the bus is not accepting subscriptions;
	// fail the assembly loudly rather than silently degrading.
	notifRun, err := notifSubscriber.Start(notifCtx)
	if err != nil {
		notifCancel()
		return stack, fmt.Errorf("notifications subscriber: %w", err)
	}
	notifDone := make(chan struct{})
	go func() {
		defer close(notifDone)
		if rerr := notifRun(); rerr != nil && !errors.Is(rerr, context.Canceled) {
			logger.ErrorContext(notifCtx, "notifications subscriber exited", slog.Any("error", rerr))
		}
	}()
	stack.closers = append(stack.closers, func(context.Context) error {
		notifCancel()
		<-notifDone
		return nil
	})

	return stack, nil
}

// assembleCatalogBand builds the tool catalog + its middleware chain:
// search cache, builtins, the unified pause Coordinator, OAuth
// providers, the catalog Builder (approval gates), the MCP
// attach loop, and the promoted dispatch executor.
func assembleCatalogBand(ctx context.Context, cfg *config.Config, opts Options, stack *Stack, logger *slog.Logger) error {
	// the tool SearchCache backs deferred-tool
	// discovery + the `tool_search` / `tool_get` meta-tools. In-memory
	// DSN by default; a failure here is non-fatal: discovery degrades
	// to empty (the meta-tools return "no results") rather than wedging
	// the boot — and the degradation is logged loud.
	searchCacheDSN := ":memory:"
	if cfg.Tools.SearchCacheDSN != "" {
		searchCacheDSN = cfg.Tools.SearchCacheDSN
	}
	searchCache, scErr := searchcache.New(searchcache.Config{DSN: searchCacheDSN})
	if scErr != nil {
		logger.Warn("tools/searchcache: disabled — discovery meta-tools will return empty",
			"err", scErr.Error(),
			"dsn", searchCacheDSN)
		searchCache = nil
	} else {
		stack.closers = append(stack.closers, func(context.Context) error { return searchCache.Close() })
	}
	var catOpts []tools.CatalogOption
	if searchCache != nil {
		catOpts = append(catOpts, tools.WithSearchCache(searchCache))
	}
	toolCat := tools.NewCatalog(catOpts...)

	for _, d := range opts.PreRegisterTools {
		if regErr := toolCat.Register(d); regErr != nil {
			return fmt.Errorf("tools/preregister[%q]: %w", d.Tool.Name, regErr)
		}
	}

	// The compiled-tool registrar rides the same pre-policy seam as
	// PreRegisterTools: it runs on the bare catalog, ahead of builtin
	// registration and the Builder's tools.entries wrapping, so a tool
	// it registers is wrapped with its declared approval/OAuth/policy
	// shell exactly like an operator's YAML tool. A callback error is
	// loud (never a silent skip).
	if opts.RegisterCatalog != nil {
		if regErr := opts.RegisterCatalog(toolCat); regErr != nil {
			return fmt.Errorf("tools/register-catalog: %w", regErr)
		}
	}

	// opt-in built-in tools BEFORE
	// the catalog-wiring step so `tools.entries[]` middleware naming a
	// built-in resolves cleanly. Empty list = no-op; unknown name fails
	// loud. SkillStore + ArtifactStore are threaded so the skill_* set
	// / artifact_fetch reach their backing stores. Note:
	// Bus + Redactor + GrantedScopes feed the skill_* delegations to
	// the Phase-38 handlers + Phase-41 generator — the capability
	// filter, redaction, budgeter, and the generator's audit-mandatory
	// emit all run on this production path.
	if err := builtin.RegisterWith(builtin.RegistryContext{
		Catalog:       toolCat,
		SkillStore:    stack.Skills,
		ArtifactStore: stack.Artifacts,
		Bus:           stack.Bus,
		Redactor:      stack.Redactor,
		GrantedScopes: append([]string(nil), cfg.Tools.GrantedScopes...),
	}, cfg.Tools.BuiltIn); err != nil {
		return fmt.Errorf("tools/builtin: %w", err)
	}

	// The HTTP-manifest boot loader: each declared
	// tools.http_manifests[] file is loaded and its tools registered on
	// the catalog by name — AFTER built-ins (so a manifest tool can
	// never silently shadow one) and BEFORE the catalog wiring below
	// applies tools.entries[] (so an entry naming a manifest tool
	// resolves cleanly). A missing/unparseable/invalid manifest, or a
	// tool-name collision against an already-registered tool, fails
	// the boot loud — naming both the manifest file and the config key
	// (CLAUDE.md §13: never a silent skip).
	for i, path := range cfg.Tools.HTTPManifests {
		manifest, loadErr := httpdrv.LoadManifest(path)
		if loadErr != nil {
			return fmt.Errorf("tools.http_manifests[%d] (%s): %w", i, path, loadErr)
		}
		if regErr := httpdrv.RegisterManifest(toolCat, manifest); regErr != nil {
			return fmt.Errorf("tools.http_manifests[%d] (%s): %w", i, path, regErr)
		}
	}

	// The shared pauseresume.Coordinator is the unified pause/resume
	// primitive — there is NEVER a second
	// Coordinator instance (CLAUDE.md §13). WithBus(bus) is mandatory:
	// it is what makes pause.requested / pause.resumed land on the
	// canonical event stream (.5 §17.5 F1).
	// WithCheckpointStore(stack.State) is mandatory too (the
	// the §13 primitive-with-consumer closure): every pause is
	// checkpointed through the runtime's own StateStore, so pauses
	// survive a Runtime restart on any durable state driver. Pre-111c
	// both assemblies constructed the Coordinator storeless with the
	// store in scope — every pause was process-memory-only.
	// WithMaxParkDuration threads the pause-lifecycle ceiling
	// (`pauseresume.max_park_duration`; 0 = never expire, the default).
	coord := pauseresume.New(
		pauseresume.WithBus(stack.Bus),
		pauseresume.WithCheckpointStore(stack.State),
		pauseresume.WithMaxParkDuration(cfg.PauseResume.MaxParkDuration),
	)

	// the pause sweeper: DecisionTimeout's first
	// producer. Config-gated on max_park_duration > 0 (the validated
	// sweep_interval is consumed only here). The goroutine is
	// cancellable + joined on Close (CLAUDE.md §5): the closer cancels
	// the sweep ctx and blocks on the done channel.
	if cfg.PauseResume.MaxParkDuration > 0 {
		// context.Background is deliberate: the sweeper's lifetime is
		// the stack's (bounded by the closer below), not the assembly
		// ctx's — same documented bridge as the notifications
		// subscriber.
		sweepCtx, sweepCancel := context.WithCancel(context.Background())
		sweepDone := make(chan struct{})
		go func() {
			defer close(sweepDone)
			if serr := pauseresume.RunSweeper(sweepCtx, coord,
				pauseresume.WithSweepInterval(cfg.PauseResume.SweepInterval),
				pauseresume.WithSweeperLogger(logger),
			); serr != nil && !errors.Is(serr, context.Canceled) {
				logger.ErrorContext(sweepCtx, "pause sweeper exited", slog.Any("error", serr))
			}
		}()
		stack.closers = append(stack.closers, func(context.Context) error {
			sweepCancel()
			<-sweepDone
			return nil
		})
	}

	// OAuth providers BEFORE the catalog Builder runs so the
	// Builder's Deps.OAuthProviders lookup resolves. Caller-supplied
	// entries (Options.OAuthProviders) win over same-named cfg
	// declarations: an overridden cfg entry is NOT constructed at all
	// (so a test stack that injects every declared provider never
	// touches the KEK env), and the caller owns the lifecycle of
	// injected providers. Cfg-built providers register their Close.
	buildCfg := cfg.Tools
	if len(opts.OAuthProviders) > 0 && len(buildCfg.OAuthProviders) > 0 {
		remaining := make([]config.ToolOAuthProviderConfig, 0, len(buildCfg.OAuthProviders))
		for _, p := range buildCfg.OAuthProviders {
			if _, injected := opts.OAuthProviders[p.Name]; injected {
				continue
			}
			remaining = append(remaining, p)
		}
		buildCfg.OAuthProviders = remaining
	}
	providers, err := toolauth.BuildProviders(ctx, buildCfg, toolauth.BuildDeps{
		State:       stack.State,
		Bus:         stack.Bus,
		Redactor:    stack.Redactor,
		Coordinator: coord,
	})
	if err != nil {
		return fmt.Errorf("tools/catalog: %w", err)
	}
	for _, p := range providers {
		prov := p
		stack.closers = append(stack.closers, func(closeCtx context.Context) error { return prov.Close(closeCtx) })
	}
	for name, p := range opts.OAuthProviders {
		providers[name] = p
	}

	// the catalog wiring Builder: `tools.entries[]`
	// auto-wraps matching descriptors with declared approval / OAuth
	// middleware. An entry naming an unregistered tool fails the boot
	// loud (CLAUDE.md §13 amendment). Empty entries = no-op.
	gates := make(map[string]*toolapproval.ApprovalGate)
	if len(cfg.Tools.Entries) > 0 {
		// the injected resolve-privilege seam. The
		// runtime-vocabulary default admits the steering bridge (the
		// run\'s own identity) and control-scoped operators; the
		// serving binary injects the Protocol-side adapter on top so
		// wire-scoped resolvers keep today\'s acceptance.
		authorizer := opts.ApprovalAuthorizer
		if authorizer == nil {
			authorizer = toolapproval.NewIdentityAuthorizer()
		}
		b := toolcatalog.New(cfg.Tools.Entries, toolcatalog.Deps{
			Catalog:        toolCat,
			Coordinator:    coord,
			Bus:            stack.Bus,
			Redactor:       stack.Redactor,
			Authorizer:     authorizer,
			OAuthProviders: providers,
			AppliedGates:   gates,
		})
		if applyErr := b.Apply(ctx); applyErr != nil {
			return fmt.Errorf("tools/catalog: %w", applyErr)
		}
		for _, g := range gates {
			gate := g
			stack.closers = append(stack.closers, func(closeCtx context.Context) error { return gate.Close(closeCtx) })
		}
	}

	// the MCP southbound consumer: per configured
	// server, spawn the transport, discover tools, register descriptors
	// on the catalog, surface the Provider on the Registry. Fail-loud
	// at boot if a server cannot connect or discover. The promoted
	// mcpdrv.Attach carries the ToolPolicy projection.
	mcpDefault := opts.MCPDefaultIdentity
	if mcpDefault == (identity.Identity{}) {
		mcpDefault = DefaultMCPIdentity
	}
	mcpRegistry := mcpdrv.NewRegistry()
	// Resolve the host's renderable MCP App display modes once; every
	// attached server advertises the same host capability during its
	// initialize handshake (the host's rendering ability does not vary per
	// server). Defaults to the inline-only baseline.
	hostDisplayModes := cfg.Tools.MCPAppHostDisplayModes()
	// The MCP Apps tool-context store: MCP providers capture the input +
	// lowered result behind a declared `ui://` app through it, and the host
	// reads it back for `mcp.apps.tool_context`. Built over the runtime's
	// own StateStore + ArtifactStore so identity isolation + the persistence
	// triad come free; wired into every Provider below so a planner-path
	// app tool call's context is captured at the invocation site.
	toolCtxStore, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{
		State:     stack.State,
		Store:     stack.Artifacts,
		Bus:       stack.Bus,
		Threshold: cfg.Artifacts.HeavyOutputThresholdBytes,
	})
	if err != nil {
		return fmt.Errorf("mcp tool-context store: %w", err)
	}
	stack.MCPToolContext = toolCtxStore
	for _, ms := range cfg.Tools.MCPServers {
		if err := mcpdrv.Attach(ctx, ms, mcpdrv.AttachDeps{
			Catalog:          toolCat,
			Registry:         mcpRegistry,
			Bus:              stack.Bus,
			Logger:           logger,
			DefaultIdentity:  mcpDefault,
			Closers:          &stack.closers,
			HostDisplayModes: hostDisplayModes,
			ToolContext:      toolCtxStore,
			// per-identity southbound OAuth binding resolution
			// (`oauth_provider` names a declared provider; fail loud on an
			// unknown name).
			OAuthProviders: providers,
		}); err != nil {
			return fmt.Errorf("mcp[%s]: %w", ms.Name, err)
		}
	}

	stack.Catalog = toolCat
	stack.Coordinator = coord
	stack.Gates = gates
	stack.OAuthProviders = providers
	stack.MCPRegistry = mcpRegistry

	// the promoted dispatch executor —
	// the ONE tool-dispatch concrete every caller wires.
	stack.Executor = dispatch.NewToolExecutor(toolCat, stack.Artifacts, stack.Tasks,
		dispatch.WithHeavyThreshold(cfg.Artifacts.HeavyOutputThresholdBytes),
		dispatch.WithMaxSpawnDepth(cfg.Planner.SpawnDepthCap()),
		dispatch.WithLogger(logger))
	return nil
}

// assembleSteeringBand builds the steering Registry, resolves the
// planner concrete via the §4.4 driver registry, and
// constructs the shared RunLoop when a planner and
// the catalog band are available. The logger is the telemetry-backed
// *slog.Logger Assemble threads post-bootstrap.
func assembleSteeringBand(ctx context.Context, cfg *config.Config, opts Options, stack *Stack, logger *slog.Logger) error {
	stack.Steering = steering.NewRegistry()

	switch {
	case opts.PlannerOverride != nil:
		stack.Planner = opts.PlannerOverride
	case stack.LLM != nil:
		plannerCfg := planner.ConfigFromOperator(cfg.Planner)
		plnr, err := planner.Resolve(ctx, plannerCfg, planner.FactoryDeps{LLM: stack.LLM})
		if err != nil {
			return fmt.Errorf("planner: %w", err)
		}
		stack.Planner = plnr
	}

	// the trajectory-compression runner — built
	// when the operator set a non-zero `planner.token_budget`. The
	// summariser needs a real LLM client; a budget without an LLM is a
	// misconfiguration surfaced loudly at boot (CLAUDE.md §13 — no
	// silent "compression configured but inert" path).
	if cfg.Planner.TokenBudget > 0 {
		if stack.LLM == nil {
			return fmt.Errorf("planner: token_budget=%d requires an LLM (configure llm) so the trajectory summariser can be built — see examples/harbor.yaml", cfg.Planner.TokenBudget)
		}
		// The operator's heavy-output threshold is threaded so the
		// summariser's aggregate payload budget tracks the SAME limit
		// the LLM-edge safety pass enforces (checkpoint audit
		// — a compaction payload must never trip ErrContextLeak on
		// the run it exists to save). Zero falls back to the canonical
		// default inside the option.
		trajSumm, err := llmsummarizer.NewTrajectorySummariser(stack.LLM,
			llmsummarizer.WithTrajectoryHeavyOutputThreshold(cfg.Artifacts.HeavyOutputThresholdBytes))
		if err != nil {
			return fmt.Errorf("trajectory summariser: %w", err)
		}
		stack.Compression = planner.NewCompressionRunner(trajSumm)
	}

	// The RunLoop needs the planner plus the catalog band's Coordinator
	// + gates map (the §13 primitive-with-consumer rule applied to the
	// V1 wiring). One RunLoop instance backs every spawned task;
	// the steering→gate bridge routes wire-side
	// APPROVE / REJECT controls to the matching gate's pending map.
	if opts.SkipRunLoop || stack.Planner == nil || stack.Coordinator == nil {
		return nil
	}
	runLoop, err := steering.NewRunLoop(stack.Steering, stack.Coordinator,
		steering.WithRunLoopBus(stack.Bus),
		steering.WithTaskRegistry(stack.Tasks),
		steering.WithApprovalGates(stack.Gates),
		steering.WithRunLoopLogger(logger),
	)
	if err != nil {
		return fmt.Errorf("steering.RunLoop: %w", err)
	}
	stack.RunLoop = runLoop
	return nil
}
