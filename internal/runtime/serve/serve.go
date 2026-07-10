// Package serve is the importable serve band: the config→listener
// composition that turns a loaded configuration into a running Harbor
// Protocol server.
//
// # Distinct from internal/server
//
// internal/server is the Protocol server's request-handling core (scope
// authorizers, bootstrap handler, search-scope adapters). This package sits
// ABOVE it: it assembles the runtime (via internal/runtime/assemble), mounts
// the Protocol surfaces (via BuildMux), binds the listener, and owns the
// serve/close lifecycle. In short — internal/server answers requests; this
// package composes the process that listens for them.
//
// # One constructor, production-shaped by construction
//
// Boot REQUIRES a non-nil auth-validator factory: identity is mandatory, so a
// nil factory is a loud construction error, never an unauthenticated
// listener. Boot mounts ONLY the surfaces every caller shares. Dev-only
// surfaces (the bootstrap-token endpoint, draft scaffolding, the token-rotate
// surface, the Console static build, fixture seeding, the mock-LLM snapshot
// override) are composed CALLER-SIDE through the injection seams on Options —
// the extra pre-CORS routes, the auth-surface builder, the LLM-snapshot
// builder, and the post-boot hook. The dev signer never reaches this package.
//
// # Lifecycle
//
// Boot returns a Handle whose Serve binds the listener and runs until ctx
// cancels, and whose Close drains every subsystem in reverse dependency
// order. On any boot error every already-opened subsystem is Closed before
// returning.
package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	nhpprof "net/http/pprof"
	"os"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/cors"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	agentregistry "github.com/hurtener/Harbor/internal/runtime/registry"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/server"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// ErrAuthValidatorFactoryRequired is returned by Boot when Options carries a
// nil AuthValidatorFactory. Identity is mandatory: the serve band never
// stands up an unauthenticated listener. Callers compare via errors.Is.
var ErrAuthValidatorFactoryRequired = errors.New("serve: auth-validator factory is required (identity is mandatory; nil is never an unauthenticated listener)")

// AuthValidatorFactory builds the Protocol auth validator from the loaded
// config plus the assembled redactor / bus / logger. The production caller
// injects a JWKS-backed factory; the dev caller injects one built from its
// ephemeral dev signer. It is REQUIRED — a nil factory fails Boot loud.
type AuthValidatorFactory func(ctx context.Context, cfg *config.Config, red audit.Redactor, bus events.EventBus, logger *slog.Logger) (auth.Validator, error)

// AuthSurfaceBuilder builds the optional token-rotate surface after the
// runtime is assembled (it needs the redactor + bus the assembly produces).
// The dev caller injects a builder that closes over its dev signer; the
// production caller leaves it nil (no in-runtime token issuer).
type AuthSurfaceBuilder func(red audit.Redactor, bus events.EventBus, logger *slog.Logger) (*auth.RotateSurface, error)

// LLMSnapshotBuilder projects the loaded config onto the LLM ConfigSnapshot
// the runtime opens the client with. The dev caller injects a builder that
// runs the mock-LLM gate and overrides the driver when the escape hatch
// fired; a nil builder uses the default llm.SnapshotFromConfig projection.
type LLMSnapshotBuilder func(cfg *config.Config) (*llm.ConfigSnapshot, error)

// RouteMount is handed to the ExtraRoutes seam so a caller can mount pre-CORS
// routes (draft scaffolding, the bootstrap-token endpoint, the Console static
// build) with access to the boot's validator, bus, and bound address. The
// caller returns any closers its routes registered (a draft store's Close).
type RouteMount struct {
	Router    *http.ServeMux
	Validator auth.Validator
	Bus       events.EventBus
	Redactor  audit.Redactor
	Logger    *slog.Logger
	// BindAddr is the resolved listen address (the bootstrap handler puts it
	// in its connection envelope).
	BindAddr string
}

// ExtraRoutesFunc mounts caller-side pre-CORS routes and returns any closers
// they registered. Called after the boot's validator and bind address are
// known and before the CORS wrap. On error, the closers accumulated up to the
// failing step MUST still be returned — Boot appends them to its rollback
// chain before inspecting the error, so a partially-mounted seam never leaks
// an already-constructed subsystem.
type ExtraRoutesFunc func(ctx context.Context, m RouteMount) ([]func(context.Context) error, error)

// PostBootHandles carries the assembled subsystem handles the post-boot hook
// receives (the dev caller's fixture seeder writes through them).
type PostBootHandles struct {
	Sessions  *sessions.Registry
	Agents    *agentregistry.Registry
	Tasks     tasks.TaskRegistry
	Artifacts artifacts.ArtifactStore
	Memory    memory.MemoryStore
	Tools     tools.ToolCatalog
	Flows     *flow.Registry
	Bus       events.EventBus
	Logger    *slog.Logger
}

// PostBootFunc runs after the listener + surfaces are composed but before
// Serve. The dev caller uses it to seed fixture entities.
type PostBootFunc func(ctx context.Context, h PostBootHandles) error

// Options bundles the inputs Boot consumes. ConfigPath (not a pre-loaded
// config) is carried so a hot-reload supervisor re-reads the file on each
// reboot by re-calling Boot with the same Options.
type Options struct {
	ConfigPath string

	// Config, when non-nil, is used directly instead of loading and
	// validating ConfigPath — the entry point for a caller that already
	// holds a validated (or programmatically-built) configuration.
	// Boot re-runs the full-binary Validate on it so a hand-built config
	// cannot bypass validation. When both Config and ConfigPath are set
	// Config wins; when Config is nil ConfigPath is loaded.
	Config *config.Config

	Port            int
	BindAddr        string
	Logger          *slog.Logger
	Stderr          io.Writer
	SubcommandLabel string

	// PreferConfigBindAddr opts into honoring the operator-configured
	// `server.bind_addr` (which may name a non-loopback interface) when no
	// explicit BindAddr override is given. ONLY the production serve caller
	// sets it. The dev/console callers leave it false so they stay
	// loopback-only on 127.0.0.1:<Port> — a dev boot against a serve-shaped
	// yaml must never expose the dev-token stack off-box.
	PreferConfigBindAddr bool

	// AuthValidatorFactory is REQUIRED (nil fails Boot loud).
	AuthValidatorFactory AuthValidatorFactory

	// MCPDefaultIdentity is the identity a boot-time / runtime-added MCP
	// connection dials under.
	MCPDefaultIdentity identity.Identity

	// DisplayName / InstanceID stamp the posture surface's runtime.info.
	DisplayName string
	InstanceID  string
	// BuildVersion / BuildCommit stamp runtime.info's build identity.
	BuildVersion string
	BuildCommit  string
	// DevAllowMock stamps the Serve start log's dev_allow_mock attribute —
	// the dev caller sets it when the mock escape hatch fired so an operator
	// reading the boot line sees the dev-only posture. A stamp only; the mock
	// gate itself is caller policy inside BuildLLMSnapshot.
	DevAllowMock bool

	// RegisterCatalog forwards a compiled-tool registrar onto the
	// assembly's pre-policy catalog seam (assemble.Options.RegisterCatalog):
	// a tool it registers is wrapped with its declared approval / OAuth /
	// policy shell before the run loop can dispatch it. Nil is a no-op —
	// the stock serve caller passes nil; an external serving binary passes
	// its agent's RegisterTools.
	RegisterCatalog func(catalog tools.ToolCatalog) error

	// Caller-side injection seams (all optional).
	BuildLLMSnapshot LLMSnapshotBuilder
	BuildAuthSurface AuthSurfaceBuilder
	ExtraRoutes      ExtraRoutesFunc
	PostBoot         PostBootFunc
}

// Handle is the running serve band a successful Boot returns. Serve binds the
// listener and runs until ctx cancels; Close drains every subsystem in
// reverse dependency order.
type Handle struct {
	// Cfg / Bus are exported so a supervising caller (the dev hot-reload
	// loop) can read the reload policy and publish reload lifecycle events.
	Cfg *config.Config
	Bus events.EventBus

	logger          *slog.Logger
	stderr          io.Writer
	server          *http.Server
	debugServer     *http.Server
	effectiveDriver string
	label           string
	devAllowMock    bool

	// mu guards bindAddr (written by Serve once the listener binds, read by
	// BindAddr) and closeFns (drained exactly once by Close). The Handle is
	// a compiled artifact shared across goroutines — internally synchronized.
	mu       sync.Mutex
	bindAddr string
	closeFns []func(context.Context) error
}

// Boot reads the config, opens every subsystem, composes the Protocol
// surface, and returns a Handle whose Serve binds the listener. On any error
// every already-opened subsystem is Closed before returning.
func Boot(ctx context.Context, opts Options) (*Handle, error) {
	if opts.AuthValidatorFactory == nil {
		return nil, ErrAuthValidatorFactoryRequired
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	var cfg *config.Config
	if opts.Config != nil {
		// A caller-supplied config is re-validated with the full-binary
		// profile so a programmatically-built config cannot bypass
		// validation (the identity/JWKS ceremony a Protocol server MUST
		// carry is enforced here, loud, before any subsystem opens).
		if vErr := opts.Config.Validate(); vErr != nil {
			return nil, fmt.Errorf("config: %w", vErr)
		}
		cfg = opts.Config
	} else {
		loaded, err := config.Load(ctx, opts.ConfigPath, config.WithLogger(opts.Logger))
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		cfg = loaded
	}

	// LLM snapshot — the caller-side builder runs any dev gate + override;
	// the default projection is the production path.
	var llmCfg llm.ConfigSnapshot
	if opts.BuildLLMSnapshot != nil {
		snap, sErr := opts.BuildLLMSnapshot(cfg)
		if sErr != nil {
			return nil, fmt.Errorf("llm: %w", sErr)
		}
		llmCfg = *snap
	} else {
		llmCfg = llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts)
	}

	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{
		Logger:             opts.Logger,
		LLMSnapshot:        &llmCfg,
		MCPDefaultIdentity: opts.MCPDefaultIdentity,
		ApprovalAuthorizer: server.NewProtocolScopeAuthorizer(toolapproval.NewIdentityAuthorizer()),
		RegisterCatalog:    opts.RegisterCatalog,
	})
	closers := make([]func(context.Context) error, 0, 8)
	if stack != nil {
		closers = append(closers, func(closeCtx context.Context) error { return stack.Close(closeCtx) })
	}
	closeAll := func(ctx context.Context) {
		for i := len(closers) - 1; i >= 0; i-- {
			if cErr := closers[i](ctx); cErr != nil && opts.Logger != nil {
				opts.Logger.Warn("serve: error closing subsystem during boot rollback",
					slog.String("error", cErr.Error()))
			}
		}
	}
	if err != nil {
		closeAll(ctx)
		return nil, err
	}

	var (
		red             = stack.Redactor
		bus             = stack.Bus
		metricsReg      = stack.Metrics
		artStore        = stack.Artifacts
		memStore        = stack.Memory
		skillStore      = stack.Skills
		taskReg         = stack.Tasks
		toolCat         = stack.Catalog
		coord           = stack.Coordinator
		mcpRegistry     = stack.MCPRegistry
		mcpToolContext  = stack.MCPToolContext
		oauthProviders  = stack.OAuthProviders
		sessionRegistry = stack.Sessions
		agentRegistry   = stack.Agents
		steeringReg     = stack.Steering
		plnr            = stack.Planner
		runLoop         = stack.RunLoop
	)

	// The skills Directory — the run loop's `<skills_context>` producer.
	var skillsDir *skills.Directory
	if skillStore != nil {
		skillsDir, err = skills.NewDirectory(skillStore, skills.Deps{Bus: bus},
			skills.DirectoryFromConfig(cfg.Skills, cfg.Planner.SkillsContextMaxResolved()))
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("skills directory: %w", err)
		}
	}

	// The agent-config control plane registry, keyed by the dev agent's
	// registration id, over the runtime StateStore.
	const devAgentConfigID = "harbor-dev-agent"
	agentConfigRegistry, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: stack.State, Bus: bus})
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("agent-config registry: %w", err)
	}
	closers = append(closers, agentConfigRegistry.Close)

	// The session-scoped safe-subset overlay store (the non-admin lower tier).
	sessionOverlayStore, err := sessionoverlay.NewStore(stack.State, nil)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("agent-config session-overlay store: %w", err)
	}
	closers = append(closers, sessionOverlayStore.Close)

	// The Protocol ControlSurface. A start on a not-yet-existing session
	// materialises its registry row via the create-on-first-use ensurer.
	surface, err := protocol.NewControlSurface(taskReg, steeringReg,
		protocol.WithSessionEnsurer(NewSessionEnsurerAdapter(sessionRegistry)),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("protocol: %w", err)
	}

	dispositionPolicy, err := planner.DispositionPolicyFromConfig(cfg.Multimodal)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("multimodal disposition policy: %w", err)
	}

	validModels := make([]string, 0, len(llmCfg.ModelProfiles))
	for m := range llmCfg.ModelProfiles {
		validModels = append(validModels, m)
	}

	// The session-level pending-override Store, shared by the run-loop driver
	// (consume at run start) and the runs Service (set via runs.set_overrides).
	runsStore := runsprotocol.NewStore()
	tenantOverridePolicy, err := governance.NewTenantOverridePolicy(stack.State, bus, validModels, nil)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("governance tenant-override policy: %w", err)
	}
	closers = append(closers, tenantOverridePolicy.Close)

	// The MCP attach/detach concretes — the attacher backs the runtime
	// add-connection verb; the detacher drives the run-start reconcile.
	var mcpAttacher agentcfgprotocol.ConnectionAttacher
	var mcpDetacher projection.ConnectionDetacher
	if toolCat != nil && mcpRegistry != nil {
		attacher := NewMCPConnectionAttacher(toolCat, mcpRegistry, bus, opts.Logger,
			opts.MCPDefaultIdentity, oauthProviders)
		closers = append(closers, attacher.Close)
		mcpAttacher = attacher
		mcpDetacher = NewMCPConnectionDetacher(toolCat, mcpRegistry, opts.Logger)
	}

	runLoopDriver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Logger:             opts.Logger,
		Bus:                bus,
		RunLoop:            runLoop,
		Planner:            plnr,
		Tasks:              taskReg,
		TaskKind:           tasks.KindForeground,
		DriveBackground:    true,
		Memory:             memStore,
		MemoryRecall:       memory.RecallFromConfig(cfg.Memory),
		SkillsDirectory:    skillsDir,
		PlanningHints:      planner.HintsFromConfig(cfg.Planner.PlanningHints),
		Catalog:            toolCat,
		Executor:           stack.Executor,
		MaxStepsRunLoop:    cfg.Planner.MaxSteps,
		GrantedScopes:      append([]string(nil), cfg.Tools.GrantedScopes...),
		ArtifactStore:      artStore,
		TokenBudget:        cfg.Planner.TokenBudget,
		Compression:        stack.Compression,
		DispositionPolicy:  dispositionPolicy,
		TenantOverrides:    tenantOverridePolicy,
		SessionOverrides:   runsStore,
		AgentConfig:        agentConfigRegistry,
		AgentConfigID:      devAgentConfigID,
		SessionOverlay:     sessionOverlayStore,
		RunCompletionHook:  projection.RunCompletionHookFromConfig(cfg.Runtime.Hooks.RunCompletion),
		ConnectionDetacher: mcpDetacher,
		BootDeclaredMCP:    BootDeclaredMCPServerSet(cfg),
		NamingDefault:      cfg.Runtime.Naming,
		SessionTitler:      sessionRegistry,
		NamingLLM:          stack.LLM,
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

	// Auth validator — the REQUIRED production path. A factory error fails
	// Boot loud (never a silent unauthenticated fallback).
	validator, err := opts.AuthValidatorFactory(ctx, cfg, red, bus, opts.Logger)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("auth: %w", err)
	}

	// Optional token-rotate surface (dev-only; nil in production).
	var authSurface *auth.RotateSurface
	if opts.BuildAuthSurface != nil {
		authSurface, err = opts.BuildAuthSurface(red, bus, opts.Logger)
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("auth: rotate surface: %w", err)
		}
	}

	built, err := BuildMux(MuxInput{
		Cfg:                  cfg,
		Surface:              surface,
		Bus:                  bus,
		Redactor:             red,
		Logger:               opts.Logger,
		Metrics:              metricsReg,
		LLMSnapshot:          llmCfg,
		Tasks:                taskReg,
		Sessions:             sessionRegistry,
		Agents:               agentRegistry,
		Artifacts:            artStore,
		Memory:               memStore,
		Catalog:              toolCat,
		Coordinator:          coord,
		MCPRegistry:          mcpRegistry,
		MCPToolContext:       mcpToolContext,
		State:                stack.State,
		Skills:               skillStore,
		AgentConfig:          agentConfigRegistry,
		AgentConfigID:        devAgentConfigID,
		SessionOverlay:       sessionOverlayStore,
		RunsStore:            runsStore,
		RunLoopDriver:        runLoopDriver,
		OAuthProviders:       oauthProviders,
		TenantOverridePolicy: tenantOverridePolicy,
		KeyRotator:           stack.KeyRotator,
		ValidModels:          validModels,
		MCPAttacher:          mcpAttacher,
		MCPStdioAllowlist:    MCPAddStdioAllowlist(cfg),
		BootDeclaredMCP:      BootDeclaredMCPServerNames(cfg),
		Validator:            validator,
		AuthSurface:          authSurface,
		DisplayName:          opts.DisplayName,
		InstanceID:           opts.InstanceID,
		BuildVersion:         opts.BuildVersion,
		BuildCommit:          opts.BuildCommit,
		TopologyAvailable:    false,
	})
	if err != nil {
		closeAll(ctx)
		return nil, err
	}

	subcommandLabel := opts.SubcommandLabel
	if subcommandLabel == "" {
		subcommandLabel = "dev"
	}
	router := http.NewServeMux()
	healthzBody := fmt.Sprintf(`{"status":"ok","subcommand":%q}`, subcommandLabel)
	router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // health-probe response write; a failure is non-actionable and the probe just retries
		_, _ = w.Write([]byte(healthzBody))
	})
	router.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // readiness-probe response write; a failure is non-actionable and the probe just retries
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	if metricsHandler, mherr := telemetry.PrometheusHandler(metricsReg); mherr == nil {
		router.Handle("/metrics", metricsHandler)
	} else if opts.Logger != nil {
		opts.Logger.InfoContext(ctx, "serve: /metrics endpoint not mounted (registry has no Prometheus pull surface)",
			slog.String("reason", mherr.Error()))
	}

	// Resolve the bind address before the extra-routes seam runs (the
	// bootstrap handler puts it in its connection envelope). The
	// operator-configured `server.bind_addr` — which may name a non-loopback
	// interface and is always non-empty after config.Load's defaulting — is
	// honored ONLY when the caller opted in via PreferConfigBindAddr (the
	// production serve posture). Without the opt-in the caller-supplied
	// loopback port wins, so dev/console boots never expose the dev-token
	// stack off-box even against a serve-shaped yaml.
	bindAddr := fmt.Sprintf("127.0.0.1:%d", opts.Port)
	if opts.BindAddr != "" {
		bindAddr = opts.BindAddr
	} else if opts.PreferConfigBindAddr && cfg.Server.BindAddr != "" {
		bindAddr = cfg.Server.BindAddr
	}

	// Caller-side pre-CORS routes (draft scaffolding, bootstrap endpoint,
	// Console static build). The tool-OAuth callback is a SHARED surface and
	// stays mounted here regardless of caller.
	router.Handle(toolauth.CallbackRoutePattern,
		toolauth.CallbackHandler(oauthProviders, toolauth.WithCallbackLogger(opts.Logger)))

	if opts.ExtraRoutes != nil {
		extra, xErr := opts.ExtraRoutes(ctx, RouteMount{
			Router:    router,
			Validator: validator,
			Bus:       bus,
			Redactor:  red,
			Logger:    opts.Logger,
			BindAddr:  bindAddr,
		})
		// Append the returned closers BEFORE checking the error: the seam's
		// contract lets a failing mount hand back the closers it accumulated
		// so far, so the rollback drains a subsystem constructed before the
		// failing step (never a leaked handle on a partial mount).
		closers = append(closers, extra...)
		if xErr != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("extra routes: %w", xErr)
		}
	}

	router.Handle("/v1/", built.Mux)

	if cfg.Server.CORSDevAllowAny {
		_, _ = fmt.Fprintln(opts.Stderr,
			"[DEV-ONLY CORS WILDCARD — server.cors_dev_allow_any=true; DO NOT USE IN PRODUCTION]")
	}
	corsHandler := cors.Wrap(router, cors.Config{
		AllowedOrigins: append([]string(nil), cfg.Server.AllowedOrigins...),
		DevAllowAny:    cfg.Server.CORSDevAllowAny,
	})

	httpServer := &http.Server{
		Addr:              bindAddr,
		Handler:           corsHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	// pprof debug listener — off by default, loopback-only, on its own server.
	debugAddr := cfg.Server.DebugAddr
	if env := os.Getenv("HARBOR_DEBUG_ADDR"); env != "" {
		if vErr := config.ValidateLoopbackAddr(env); vErr != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("HARBOR_DEBUG_ADDR: %w", vErr)
		}
		debugAddr = env
	}
	var debugServer *http.Server
	if debugAddr != "" {
		debugMux := http.NewServeMux()
		debugMux.HandleFunc("/debug/pprof/", nhpprof.Index)
		debugMux.HandleFunc("/debug/pprof/cmdline", nhpprof.Cmdline)
		debugMux.HandleFunc("/debug/pprof/profile", nhpprof.Profile)
		debugMux.HandleFunc("/debug/pprof/symbol", nhpprof.Symbol)
		debugMux.HandleFunc("/debug/pprof/trace", nhpprof.Trace)
		debugServer = &http.Server{
			Addr:              debugAddr,
			Handler:           debugMux,
			ReadHeaderTimeout: 10 * time.Second,
			BaseContext:       func(_ net.Listener) context.Context { return ctx },
		}
		_, _ = fmt.Fprintf(opts.Stderr,
			"[DEV-ONLY pprof debug listener on %s — DO NOT EXPOSE]\n", debugAddr)
	}

	// Post-boot hook (fixture seeding). Runs before Serve so seeded entities
	// are present the moment the listener answers.
	if opts.PostBoot != nil {
		if pErr := opts.PostBoot(ctx, PostBootHandles{
			Sessions:  sessionRegistry,
			Agents:    agentRegistry,
			Tasks:     taskReg,
			Artifacts: artStore,
			Memory:    memStore,
			Tools:     toolCat,
			Flows:     built.FlowRegistry,
			Bus:       bus,
			Logger:    opts.Logger,
		}); pErr != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("post-boot: %w", pErr)
		}
	}

	return &Handle{
		Cfg:             cfg,
		Bus:             bus,
		logger:          opts.Logger,
		stderr:          opts.Stderr,
		server:          httpServer,
		debugServer:     debugServer,
		bindAddr:        bindAddr,
		effectiveDriver: llmCfg.Driver,
		label:           subcommandLabel,
		devAllowMock:    opts.DevAllowMock,
		closeFns:        closers,
	}, nil
}

// Serve binds the listener and runs the http.Server until ctx cancels.
func (h *Handle) Serve(ctx context.Context) error {
	label := h.label
	if label == "" {
		label = "dev"
	}
	h.logger.InfoContext(ctx, "harbor "+label+": starting Protocol server",
		slog.String("bind", h.BindAddr()),
		slog.String("driver_llm", h.effectiveDriver),
		slog.String("driver_state", h.Cfg.State.Driver),
		slog.String("driver_events", h.Cfg.Events.Driver),
		slog.String("driver_memory", h.Cfg.Memory.Driver),
		slog.String("memory_strategy", h.Cfg.Memory.Strategy),
		slog.Bool("dev_allow_mock", h.devAllowMock),
	)

	listener, err := net.Listen("tcp", h.server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", h.server.Addr, err)
	}
	boundAddr := listener.Addr().String()
	h.mu.Lock()
	h.bindAddr = boundAddr
	h.mu.Unlock()
	_, _ = fmt.Fprintf(h.stderr, "HARBOR_DEV_BOUND=%s\n", boundAddr)
	h.logger.InfoContext(ctx, "harbor "+label+": listener bound", slog.String("bind", boundAddr))

	if h.debugServer != nil {
		debugListener, derr := net.Listen("tcp", h.debugServer.Addr)
		if derr != nil {
			return fmt.Errorf("listen pprof debug %s: %w", h.debugServer.Addr, derr)
		}
		h.logger.InfoContext(ctx, "harbor "+label+": pprof debug listener bound",
			slog.String("bind", debugListener.Addr().String()))
		go func() {
			if derr := h.debugServer.Serve(debugListener); derr != nil && !errors.Is(derr, http.ErrServerClosed) {
				h.logger.Warn("harbor "+label+": pprof debug listener stopped with error",
					slog.String("error", derr.Error()))
			}
		}()
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			//nolint:errcheck // best-effort drain of the debug listener on teardown
			_ = h.debugServer.Shutdown(shutCtx)
		}()
	}

	listenErr := make(chan error, 1)
	go func() {
		err := h.server.Serve(listener)
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
		grace := h.Cfg.Server.ShutdownGracePeriod
		if grace <= 0 {
			grace = 30 * time.Second
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		h.logger.Info("harbor "+label+": draining", slog.Duration("grace", grace))
		if err := h.server.Shutdown(shutdownCtx); err != nil {
			h.logger.Warn("harbor "+label+": graceful shutdown did not complete within the grace period",
				slog.String("error", err.Error()))
		}
		return nil
	}
}

// BindAddr reports the address the listener is (or will be) bound to. After
// Serve binds an ephemeral port it reflects the OS-assigned address.
// Internally synchronized — safe to call while Serve runs.
func (h *Handle) BindAddr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bindAddr
}

// Handler returns the composed CORS-wrapped router the listener serves. It
// lets a caller drive the Protocol surface (and any caller-mounted pre-CORS
// route) through httptest without binding a port — the posture split between
// the dev and serve compositions is asserted this way.
func (h *Handle) Handler() http.Handler { return h.server.Handler }

// Close runs every subsystem's Close in reverse dependency order. Idempotent:
// the closer slice is drained exactly once; a second Close is a no-op.
func (h *Handle) Close(ctx context.Context) {
	h.mu.Lock()
	closers := h.closeFns
	h.closeFns = nil
	h.mu.Unlock()
	for i := len(closers) - 1; i >= 0; i-- {
		if cErr := closers[i](ctx); cErr != nil && h.logger != nil {
			h.logger.Warn("serve: error closing subsystem during drain",
				slog.String("error", cErr.Error()))
		}
	}
}
