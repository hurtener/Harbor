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
	"strings"
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
	credentialhttp "github.com/hurtener/Harbor/internal/llm/credentials/httptransport"
	llmbifrost "github.com/hurtener/Harbor/internal/llm/drivers/bifrost"
	llmprovider "github.com/hurtener/Harbor/internal/llm/provider"
	providerroutehttp "github.com/hurtener/Harbor/internal/llm/providerroute/httptransport"
	llmreceipts "github.com/hurtener/Harbor/internal/llm/receipts"
	receipthttp "github.com/hurtener/Harbor/internal/llm/receipts/httptransport"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/cors"
	protocoltypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	agentregistry "github.com/hurtener/Harbor/internal/runtime/registry"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/server"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/skills/importer"
	"github.com/hurtener/Harbor/internal/skills/publication"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// ErrAuthValidatorFactoryRequired is returned by Boot when Options carries a
// nil AuthValidatorFactory. Identity is mandatory: the serve band never
// stands up an unauthenticated listener. Callers compare via errors.Is.
var ErrAuthValidatorFactoryRequired = errors.New("serve: auth-validator factory is required (identity is mandatory; nil is never an unauthenticated listener)")

// ErrAuthValidatorRequired is returned by Boot when the configured
// AuthValidatorFactory returns a nil Validator with a nil error. A nil
// Validator is the test-kit auth opt-out that BuildMux reads as
// transports.WithoutValidator(); the serve band never takes that path, so
// Boot fails construction here instead. Callers compare via errors.Is.
var ErrAuthValidatorRequired = errors.New("serve: auth-validator factory returned a nil validator (identity is mandatory; nil is never an unauthenticated listener)")

// AuthValidatorFactory builds the Protocol auth validator from the loaded
// config plus the assembled redactor / bus / logger. The production caller
// injects a JWKS-backed factory; the dev caller injects one built from its
// ephemeral dev signer. It is REQUIRED — a nil factory fails Boot loud.
type AuthValidatorFactory func(ctx context.Context, cfg *config.Config, red audit.Redactor, bus events.EventBus, logger *slog.Logger) (auth.Validator, error)

// NewJWKSAuthValidatorFactory returns THE production auth-validator
// factory: it projects the operator's identity config onto a JWKS-backed
// Validator (URL or file source), wiring the assembled redactor / bus /
// logger. The initial JWKS fetch runs synchronously inside the
// projection, so a bad source fails the boot loud. Every production
// serve caller (the serve subcommand and the external-serving facade)
// injects this one factory — a second hand-rolled copy is the drift this
// shared constructor exists to prevent.
func NewJWKSAuthValidatorFactory() AuthValidatorFactory {
	return func(ctx context.Context, cfg *config.Config, red audit.Redactor, bus events.EventBus, logger *slog.Logger) (auth.Validator, error) {
		return auth.NewJWKSValidator(ctx, cfg.Identity, auth.ValidatorDeps{
			Redactor: red,
			Logger:   logger,
			Bus:      bus,
		})
	}
}

// InstanceID mints a stable-per-process instance identifier of the form
// "<prefix>-<hostname>" (bare prefix when the hostname is unavailable).
// A Console attached to multiple Runtimes keys each attachment by it.
// Shared by every production serve caller so the id shape stays uniform.
func InstanceID(prefix string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return prefix + "-" + h
	}
	return prefix
}

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
	// FrameworkVersion / FrameworkCommit optionally stamp the Harbor framework
	// provenance in runtime.info for an external host whose own build identity
	// is independent of the framework it embeds.
	FrameworkVersion string
	FrameworkCommit  string
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
	// ExternalGrant carries host-owned grant dependencies into the real
	// runtime assembly. The non-secret verifier posture is read from
	// cfg.LLM.ExternalGrant; credentials and receipt delivery are never read
	// from a Protocol request or caller-provided config field.
	ExternalGrant          llm.ExternalGrantConfig
	ExternalGrantDelivery  llmreceipts.Delivery
	ExternalGrantPending   llmreceipts.PendingReceiptSource
	ExternalGrantMaxBatch  int
	ExternalGrantReconcile time.Duration
	// ProviderRoute injects an optional host-owned route resolver. It conflicts
	// with a configured stock provider_route transport.
	ProviderRoute    llm.ProviderRouteConfig
	BuildAuthSurface AuthSurfaceBuilder
	ExtraRoutes      ExtraRoutesFunc
	PostBoot         PostBootFunc
}

func configureStockProviderRoute(cfg config.LLMProviderRouteConfig, opts *Options, getenv func(string) (string, bool)) (*providerroutehttp.Client, error) {
	configured := strings.TrimSpace(cfg.ResolverURL) != "" || strings.TrimSpace(cfg.AuthTokenEnv) != "" ||
		strings.TrimSpace(cfg.RuntimeID) != "" || cfg.Timeout != 0
	if !configured {
		return nil, nil
	}
	if opts.ProviderRoute.Resolver != nil {
		return nil, fmt.Errorf("provider route: configured stock resolver conflicts with injected resolver")
	}
	token, ok := getenv(cfg.AuthTokenEnv)
	if !ok || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("provider route: auth token env %q is unset", cfg.AuthTokenEnv)
	}
	client, err := providerroutehttp.New(providerroutehttp.Config{ResolverURL: cfg.ResolverURL, AuthToken: token, Timeout: cfg.Timeout})
	if err != nil {
		return nil, fmt.Errorf("provider route: construct stock resolver: %w", err)
	}
	opts.ProviderRoute = llm.ProviderRouteConfig{Resolver: client, RuntimeID: cfg.RuntimeID}
	return client, nil
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

	// readyOnce + readyCh implement race-safe one-shot readiness. Serve
	// signals exactly once — on successful bind (carrying the OS-assigned
	// address) or on bind/cancellation failure (carrying the error). The
	// channel is CLOSED (broadcast) so every waiter — early or late —
	// unblocks; the stored `ready` value carries the outcome. Close also
	// fires a "closed before bind" signal so a waiter on a handle that
	// never served returns promptly.
	readyOnce sync.Once
	readyCh   chan struct{}
	ready     readiness
}

// readiness carries the one-shot bind outcome: either the actual bound
// address (success) or the bind/cancellation error (failure). Exactly one
// field is populated. Written exactly once (under mu) before readyCh is
// closed; safe for any number of concurrent reads after the close barrier.
type readiness struct {
	addr string
	err  error
}

// signalReady delivers the one-shot readiness outcome. Safe to call from
// multiple paths (Serve success, Serve failure, Close) — sync.Once ensures
// the first caller wins. The channel is closed (broadcast) so every
// waiter — early or late — unblocks; the stored value carries the outcome.
func (h *Handle) signalReady(r readiness) {
	h.readyOnce.Do(func() {
		h.mu.Lock()
		h.ready = r
		h.mu.Unlock()
		close(h.readyCh)
	})
}

// Boot reads the config, opens every subsystem, composes the Protocol
// surface, and returns a Handle whose Serve binds the listener. On any error
// every already-opened subsystem is Closed before returning.
// resolveMCPAttachIdentity returns the transport-event fallback identity for
// the runtime-added MCP connection attacher. An empty configured value — the
// state of every SDK-facade embedder, since the facade exposes no knob to set
// it — is filled with assemble.DefaultMCPIdentity, mirroring the boot-time
// fallback assemble already applies to config-declared servers. Without this,
// a runtime-added MCP connection is rejected at construction ("DefaultIdentity
// must be fully populated"), so a compiled SDK server can never accept one.
// The default only stamps transport-side events that arrive without an
// inflight call; per-call isolation rides the inflight caller identity, never
// this value.
func resolveMCPAttachIdentity(configured identity.Identity) identity.Identity {
	if configured == (identity.Identity{}) {
		return assemble.DefaultMCPIdentity
	}
	return configured
}

func externalGrantReadinessProvider(settings config.LLMExternalGrantConfig, provided llm.ExternalGrantConfig, delivery llmreceipts.Delivery, stock *stockExternalGrantClients) func() protocoltypes.ExternalGrantReadiness {
	mode := strings.TrimSpace(settings.Mode)
	if mode == "" {
		mode = strings.TrimSpace(string(provided.Mode))
	}
	if mode == "" {
		mode = string(llm.ExternalGrantDisabled)
	}
	routeMode := strings.TrimSpace(settings.RouteMode)
	if routeMode == "" {
		routeMode = strings.TrimSpace(string(provided.RouteMode))
	}
	return func() protocoltypes.ExternalGrantReadiness {
		out := protocoltypes.ExternalGrantReadiness{
			Supported: true,
			Mode:      mode,
			SupportedGrantVersions: []int{
				llm.ExternalGrantVersionLegacy,
				llm.ExternalGrantVersionAgentBound,
			},
			AgentBinding:         "required_v2",
			ReceiptParser:        "strict_canonical_v1",
			ReceiptTransportKind: "none",
			ReceiptTransport:     "disabled",
			TopUpTransport:       "unsupported",
			TopUpState:           "absent",
		}
		if mode == string(llm.ExternalGrantDisabled) {
			return out
		}
		out.Configured = true
		if routeMode == "" {
			out.AcceptedRouteModes = []string{string(llm.ExternalGrantRouteRuntimeDefault), string(llm.ExternalGrantRouteCoordinatorBound)}
		} else {
			out.AcceptedRouteModes = []string{routeMode}
		}
		out.VerifierConfigured = provided.Verifier != nil || len(settings.PublicKeys) > 0
		// Successful serve assembly always supplies the StateStore-backed
		// reservation manager when an external-grant mode is enabled.
		out.ReservationsWired = true
		out.CredentialResolverWired = provided.Credentials != nil
		if provided.TopUpper != nil {
			out.TopUpTransport = "host_injected"
			out.TopUpState = "wired"
		}
		switch {
		case stock != nil && stock.receipts != nil:
			ready := stock.receipts.Readiness()
			out.ReceiptTransportKind = "stock_authenticated_http"
			out.ReceiptTransport = ready.Receipt
			if ready.TopUp != "absent" {
				out.TopUpTransport = "stock_authenticated_http"
				out.TopUpState = ready.TopUp
			}
		case delivery != nil:
			out.ReceiptTransportKind = "host_injected_delivery"
			out.ReceiptTransport = "wired"
		case provided.ReceiptSink != nil:
			out.ReceiptTransportKind = "host_injected_sink"
			out.ReceiptTransport = "wired"
		default:
			out.ReceiptTransport = "absent"
		}
		baseReady := out.VerifierConfigured && out.ReservationsWired && out.ReceiptTransport == "wired"
		if baseReady && (routeMode == "" || routeMode == string(llm.ExternalGrantRouteRuntimeDefault)) {
			out.ReadyRouteModes = append(out.ReadyRouteModes, string(llm.ExternalGrantRouteRuntimeDefault))
		}
		if baseReady && out.CredentialResolverWired && (routeMode == "" || routeMode == string(llm.ExternalGrantRouteCoordinatorBound)) {
			out.ReadyRouteModes = append(out.ReadyRouteModes, string(llm.ExternalGrantRouteCoordinatorBound))
		}
		out.StrictReady = len(out.ReadyRouteModes) == len(out.AcceptedRouteModes) && out.TopUpState != "degraded"
		return out
	}
}

type stockExternalGrantClients struct {
	receipts    *receipthttp.Client
	credentials *credentialhttp.Client
}

func (c *stockExternalGrantClients) Deliver(ctx context.Context, receipt llm.AttemptUsageReceipt) error {
	return c.receipts.Deliver(ctx, receipt)
}

func (c *stockExternalGrantClients) DeliverBatch(ctx context.Context, batch []llm.AttemptUsageReceipt) ([]llmreceipts.DeliveryAck, error) {
	return c.receipts.DeliverBatch(ctx, batch)
}

func (c *stockExternalGrantClients) TopUp(ctx context.Context, grant llm.ExternalGrant, units int64) (llm.ExternalGrant, error) {
	return c.receipts.TopUp(ctx, grant, units)
}

func (c *stockExternalGrantClients) Renew(ctx context.Context, grant llm.ExternalGrant, units int64, reason llm.ExternalGrantRenewalReason) (llm.ExternalGrant, error) {
	return c.receipts.Renew(ctx, grant, units, reason)
}

func (c *stockExternalGrantClients) Readiness() receipthttp.Readiness {
	return c.receipts.Readiness()
}

func (c *stockExternalGrantClients) SetOutboxHealth(healthy bool) {
	c.receipts.SetOutboxHealth(healthy)
}

func (c *stockExternalGrantClients) Close(context.Context) error {
	if c == nil || c.credentials == nil {
		return nil
	}
	return c.credentials.Close()
}

func configureStockExternalGrant(coordinatorCfg config.ExternalGrantCoordinatorConfig, opts *Options, getenv func(string) (string, bool)) (*stockExternalGrantClients, error) {
	if coordinatorCfg.ReceiptURL == "" && coordinatorCfg.CredentialURL == "" {
		return nil, nil
	}
	if coordinatorCfg.ReceiptURL != "" && (opts.ExternalGrantDelivery != nil || opts.ExternalGrant.ReceiptSink != nil) {
		return nil, fmt.Errorf("external grant coordinator: configured receipt transport conflicts with injected receipt handling")
	}
	if coordinatorCfg.TopUpURL != "" && opts.ExternalGrant.TopUpper != nil {
		return nil, fmt.Errorf("external grant coordinator: configured top-up transport conflicts with injected top-upper")
	}
	if coordinatorCfg.CredentialURL != "" && opts.ExternalGrant.Credentials != nil {
		return nil, fmt.Errorf("external grant coordinator: configured credential transport conflicts with injected credential resolver")
	}
	token, ok := getenv(coordinatorCfg.AuthTokenEnv)
	if !ok || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("external grant coordinator: env var %q (auth_token_env) is unset or empty", coordinatorCfg.AuthTokenEnv)
	}
	clients := &stockExternalGrantClients{}
	if coordinatorCfg.ReceiptURL != "" {
		client, err := receipthttp.New(receipthttp.Config{
			ReceiptURL: coordinatorCfg.ReceiptURL,
			TopUpURL:   coordinatorCfg.TopUpURL,
			AuthToken:  token,
			Timeout:    coordinatorCfg.Timeout,
			MaxBatch:   coordinatorCfg.MaxBatch,
		})
		if err != nil {
			return nil, fmt.Errorf("external grant coordinator: %w", err)
		}
		clients.receipts = client
		opts.ExternalGrantDelivery = clients
		if coordinatorCfg.TopUpURL != "" {
			opts.ExternalGrant.TopUpper = clients
		}
	}
	if coordinatorCfg.CredentialURL != "" {
		client, err := credentialhttp.New(credentialhttp.Config{
			CredentialURL: coordinatorCfg.CredentialURL,
			AuthToken:     token,
			Timeout:       coordinatorCfg.Timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("external grant coordinator: %w", err)
		}
		clients.credentials = client
		opts.ExternalGrant.Credentials = client
	}
	if opts.ExternalGrantMaxBatch == 0 {
		opts.ExternalGrantMaxBatch = coordinatorCfg.MaxBatch
	}
	if opts.ExternalGrantReconcile == 0 {
		opts.ExternalGrantReconcile = coordinatorCfg.ReconcileInterval
	}
	return clients, nil
}

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

	// The stock coordinator transport is constructed only when its explicit
	// config block is present. Disabled/default installations reach neither an
	// environment lookup nor a goroutine, timer, store read, or network call.
	stockCoordinator, stockErr := configureStockExternalGrant(cfg.LLM.ExternalGrant.Coordinator, &opts, os.LookupEnv)
	if stockErr != nil {
		return nil, stockErr
	}
	stockProviderRoute, routeErr := configureStockProviderRoute(cfg.LLM.ProviderRoute, &opts, os.LookupEnv)
	if routeErr != nil {
		if stockCoordinator != nil {
			_ = stockCoordinator.Close(ctx)
		}
		return nil, routeErr
	}
	finalGrantMode := opts.ExternalGrant.Mode
	if finalGrantMode == "" {
		finalGrantMode = llm.ExternalGrantMode(strings.TrimSpace(cfg.LLM.ExternalGrant.Mode))
	}
	finalGrantRoute := opts.ExternalGrant.RouteMode
	if finalGrantRoute == "" {
		finalGrantRoute = llm.ExternalGrantRouteMode(strings.TrimSpace(cfg.LLM.ExternalGrant.RouteMode))
	}
	if finalGrantMode == llm.ExternalGrantRequired &&
		finalGrantRoute == llm.ExternalGrantRouteCoordinatorBound &&
		opts.ExternalGrant.Credentials == nil {
		if stockCoordinator != nil {
			_ = stockCoordinator.Close(ctx)
		}
		return nil, fmt.Errorf("external grant: required coordinator_bound mode needs an injected or stock credential resolver")
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
		Logger:                 opts.Logger,
		LLMSnapshot:            &llmCfg,
		ExternalGrant:          opts.ExternalGrant,
		ExternalGrantDelivery:  opts.ExternalGrantDelivery,
		ExternalGrantPending:   opts.ExternalGrantPending,
		ExternalGrantMaxBatch:  opts.ExternalGrantMaxBatch,
		ExternalGrantReconcile: opts.ExternalGrantReconcile,
		ProviderRoute:          opts.ProviderRoute,
		MCPDefaultIdentity:     opts.MCPDefaultIdentity,
		ApprovalAuthorizer:     server.NewProtocolScopeAuthorizer(toolapproval.NewIdentityAuthorizer()),
		RegisterCatalog:        opts.RegisterCatalog,
	})
	closers := make([]func(context.Context) error, 0, 8)
	if stockCoordinator != nil {
		closers = append(closers, stockCoordinator.Close)
	}
	if stockProviderRoute != nil {
		closers = append(closers, func(context.Context) error { return stockProviderRoute.Close() })
	}
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
	// Provider catalog operations execute from the booted runtime, using the
	// same resolved LLM configuration as ordinary execution. This separate
	// bounded Bifrost client never receives a caller key or raw provider body.
	var providerCatalog llmprovider.CatalogSurface
	if stack != nil && stack.LLM != nil && llmCfg.Driver == "bifrost" && llmCfg.Provider != "" {
		catalog, catalogErr := llmbifrost.NewProviderCatalogWithDeps(llmCfg, llm.Deps{
			LiveKey:       stack.LLMLiveKey,
			ProviderRoute: opts.ProviderRoute,
		})
		if catalogErr != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("llm provider catalog: %w", catalogErr)
		}
		providerCatalog = catalog
		closers = append(closers, catalog.Close)
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
	if lifecycleErr := EnsureBootAgentLifecycle(ctx, stack.State, agentConfigRegistry,
		resolveMCPAttachIdentity(opts.MCPDefaultIdentity), devAgentConfigID); lifecycleErr != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("boot agent lifecycle: %w", lifecycleErr)
	}
	bootLifecycleEnsurer := agentcfg.BootLifecycleEnsurer(func(runCtx context.Context, id identity.Identity, agentID string) error {
		return EnsureBootAgentLifecycle(runCtx, stack.State, agentConfigRegistry, id, agentID)
	})
	runSnapshots := runsnapshot.NewGate()

	// The session-scoped safe-subset overlay store (the non-admin lower tier).
	sessionOverlayStore, err := sessionoverlay.NewStore(stack.State, nil)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("agent-config session-overlay store: %w", err)
	}
	closers = append(closers, sessionOverlayStore.Close)
	var sessionPersonalAuthority *SessionPersonalSkillAuthority
	if skillStore != nil {
		sessionPersonalAuthority, err = NewSessionPersonalSkillAuthority(
			ctx,
			stack.State,
			skillStore,
			cfg.Skills.SessionPersonalCutover.Tenants,
		)
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("agent-config session-personal skill authority: %w", err)
		}
	}
	var sessionPersonalStore *sessionoverlay.DurableStore
	var sessionPersonalCutover sessionoverlay.CutoverModeReader
	var sessionPersonalController agentcfgprotocol.SessionPersonalSkillController
	if sessionPersonalAuthority != nil {
		sessionPersonalStore = sessionPersonalAuthority.Personal
		sessionPersonalCutover = sessionPersonalAuthority.Cutover
		sessionPersonalController = sessionPersonalAuthority.Controller
	}

	// The Protocol ControlSurface. A start on a not-yet-existing session
	// materialises its registry row via the create-on-first-use ensurer.
	// A `start` naming an agent is validated against the SAME agent-config
	// registry + boot agent id the run-loop driver projects from, so the
	// edge cannot accept an agent the run loop would not honour.
	agentReach := auth.NewAgentReachAuthorizer()
	var agentReachAdmissions *tasks.AgentReachAdmissionAuthority
	if stack.OAuthProviderBuilder != nil && stack.OAuthProviderBuilder.AdmissionSealer() != nil {
		agentReachAdmissions, err = tasks.NewAgentReachAdmissionAuthority(stack.OAuthProviderBuilder.AdmissionSealer())
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("agent reach admission authority: %w", err)
		}
	}
	agentResolver := NewAgentResolverAdapter(agentConfigRegistry, devAgentConfigID, WithBootLifecycleEnsurer(bootLifecycleEnsurer))

	// ── v1.28 projection + admission + boot-baseline band ─────────────
	// The HA-56 render-admission surface, HA-64 turns projection, HA-65
	// rollups projection, and HA-66 boot baseline all derive from ONE
	// shared restart-stable KEK-backed sealer and ONE eager immutable
	// boot-pack index — resolved HERE, before readiness / listeners, so a
	// missing authority fails the boot loud.

	// The ONE shared KEK-backed sealer (never a second instance over the
	// same key): the ProviderBuilder's broker sealer when one exists,
	// else exactly one instance from an EXPLICITLY configured
	// `tools.oauth_token_kek_env` — resolved consumer-independently, so
	// HA-61 import keeps its sealer even when render admission is
	// disabled and no broker is present. When
	// `tools.mcp_app_render_admission.enabled` is set, an empty env
	// name / unset-invalid KEK / construction failure fails readiness
	// LOUD even with no OAuth provider or credential broker declared;
	// an explicitly configured but unresolvable env fails loud likewise.
	sharedSealer, sealerErr := ResolveSharedKEKSealer(cfg, stack.OAuthProviderBuilder)
	if sealerErr != nil {
		closeAll(ctx)
		return nil, sealerErr
	}
	// The shared sealer also authenticates durable control.start reach
	// admissions when no broker declared one: the SAME immutable
	// authority serves OAuth, signed admissions, HA-61 proposal tokens
	// and HA-56 render admissions.
	if agentReachAdmissions == nil && sharedSealer != nil {
		agentReachAdmissions, err = tasks.NewAgentReachAdmissionAuthority(sharedSealer)
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("agent reach admission authority: %w", err)
		}
	}
	// Construct the control surface only after the shared-sealer fallback has
	// resolved. Otherwise a runtime without an OAuth provider would capture a
	// nil admission authority here even though the same boot later wires the
	// run-loop authority, making every route-bearing start unverifiable.
	surface, err := protocol.NewControlSurface(taskReg, steeringReg,
		protocol.WithSessionEnsurer(NewSessionEnsurerAdapter(sessionRegistry)),
		protocol.WithAgentResolver(agentResolver),
		protocol.WithAgentReachAuthorizer(agentReach),
		protocol.WithAgentReachAdmissionAuthority(agentReachAdmissions),
	)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("protocol: %w", err)
	}
	// The publication store is mounted only when the same restart-stable
	// admission authority that authenticates control.start is available. This
	// keeps Protocol and run-start composition on one wrapped StateStore store;
	// a stack without that authority must not advertise a body-bearing surface
	// it cannot compose with verified reach after restart.
	var publicationStore publication.Store
	publicationRuntimeID := ""
	if agentReachAdmissions != nil && skillStore != nil && sessionPersonalStore != nil && sessionPersonalCutover != nil {
		publicationRuntimeID = publication.NewRuntimeID(opts.InstanceID)
		publicationStore, err = NewSkillPublicationStore(stack.State, publicationRuntimeID, agentReach)
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("skill publication store: %w", err)
		}
		closers = append(closers, publicationStore.Close)
	}

	// The HA-66 boot baseline: the eager immutable index loaded from
	// `skills.boot_agent_packs` (all files read before readiness), every
	// declared agent id validated against the one resolved boot/default
	// agent, static required_tools validation against the WRAPPED
	// catalog under the configured `tools.granted_scopes` ceiling
	// (required_tools is metadata only and grants nothing), and the
	// pre-read durable collision check (same hash dedupes both, differing
	// hash fails loud). Loader/composer never invokes admin pack verbs,
	// lifecycle, SkillStore/ArtifactStore writes, or AgentConfig
	// revisions.
	// The agent-config registry's retirement/read seam is asserted ONCE
	// here: the enabled v1.28 surfaces (boot baseline, render admission,
	// import, preview) all require it, so a registry that cannot serve it
	// fails the boot loud rather than degrading a surface.
	retirementRegistry, retirementOK := agentConfigRegistry.(agentcfg.RetirementRegistry)
	if !retirementOK && (len(cfg.Skills.BootAgentPacks) > 0 || cfg.Tools.MCPAppRenderAdmission.Enabled) {
		closeAll(ctx)
		return nil, fmt.Errorf("agent-config registry does not implement the retirement/read seam required by the enabled v1.28 surface")
	}
	var bootIndex *bootpacks.Index
	if len(cfg.Skills.BootAgentPacks) > 0 {
		bootIndex, err = OpenBootPackIndex(ctx, cfg, toolCat, artStore)
		if err != nil {
			closeAll(ctx)
			return nil, err
		}
		if vErr := ValidateBootAgentPacksForAgent(cfg, devAgentConfigID); vErr != nil {
			closeAll(ctx)
			return nil, vErr
		}
		if pErr := PreReadBootPackCollisions(ctx, bootIndex, retirementRegistry); pErr != nil {
			closeAll(ctx)
			return nil, pErr
		}
	}

	// The HA-64 turns projection + HA-65 rollups projection: opened over
	// the operator's config blocks with inmem / SQLite / Postgres parity,
	// each running its own cancelable wake-driven loop.
	var turnsProj *turns.Projector
	var turnsStore turns.Store
	var turnsCloser func(context.Context) error
	var rollupsStore rollups.Store
	var rollupsWorker *rollupWorker
	var rollupsCloser func(context.Context) error
	var turnsSvc *turnsService
	turnsProj, turnsSvc, turnsCloser, err = OpenTurnsProjection(ctx, cfg, TurnsProjectionDeps{
		Bus:           bus,
		Sessions:      sessionRegistry,
		Tasks:         taskReg,
		Artifacts:     artStore,
		Logger:        opts.Logger,
		PostgresPools: stack.PostgresPools,
	})
	if err != nil {
		closeAll(ctx)
		return nil, err
	}
	if turnsCloser != nil {
		closers = append(closers, turnsCloser)
	}
	if turnsSvc != nil {
		turnsStore = turnsSvc.store
	}
	rollupsStore, rollupsWorker, rollupsCloser, err = OpenRollupsProjection(ctx, cfg, RollupsProjectionDeps{
		Bus:           bus,
		Logger:        opts.Logger,
		PostgresPools: stack.PostgresPools,
	})
	if err != nil {
		closeAll(ctx)
		return nil, err
	}
	if rollupsCloser != nil {
		closers = append(closers, rollupsCloser)
	}

	// The HA-56 render-admission authority + gate pair. The pair is
	// wired ONLY when the operator explicitly opted in
	// (`tools.mcp_app_render_admission.enabled`); sealer availability is
	// NOT feature enablement, so an OAuth broker sealer alone never
	// wires the surface. Both halves are wired together (neither wired
	// keeps the compatible disabled surface). The gate re-runs verified
	// identity / the request's reach-admitted effective agent /
	// erasure / retirement / current exposure / exact server+resource /
	// paused-disabled before every mint and every callback
	// verification, and binds the exact current provider/catalog
	// generation.
	var sourceAuthorizer *mcpconsole.SourceAuthorizer
	if mcpRegistry != nil {
		sourceAuthorizer = mcpconsole.NewSourceAuthorizer(mcpRegistry, retirementRegistry, agentReach)
	}
	admissionAuthority, admissionGate, admErr := WireRenderAdmission(RenderAdmissionAuthorityDeps{
		Enabled:          cfg.Tools.MCPAppRenderAdmission.Enabled,
		Sessions:         sessionRegistry,
		AgentConfig:      retirementRegistry,
		SessionOverlay:   sessionOverlayStore,
		Registry:         mcpRegistry,
		SourceAuthorizer: sourceAuthorizer,
		Sealer:           sharedSealer,
	})
	if admErr != nil {
		closeAll(ctx)
		return nil, admErr
	}

	// The HA-61 two-phase import service, built from the SAME
	// already-composed seams the rest of the v1.28 band uses: the
	// production importer over the caller-owned artifact store, the ONE
	// shared restart-stable sealer, the runtime StateStore commit
	// ledger, the configured SkillStore, the agent-config registry's
	// retirement/read seam, and the capability-policy adapter projected
	// over the wrapped catalog under the caller's verified identity.
	// The signed effective-agent and session-reach gates ride the same
	// authorizers the ControlSurface and the AppsSurface use. The
	// service is nil (routes stay 501) only when a mandatory seam is
	// genuinely absent — no SkillStore, no shared sealer, or a registry
	// without the retirement/read seam — never a stub capability, never
	// a silent empty surface.
	var userSkillImportService *agentcfgprotocol.UserSkillImportService
	if skillStore != nil && sharedSealer != nil && retirementOK {
		imp, impErr := importer.New(importer.Deps{Store: artStore})
		if impErr != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("user skill import service: %w", impErr)
		}
		userSkillImportService, err = agentcfgprotocol.NewUserSkillImportService(
			imp, artStore, sharedSealer, stack.State, skillStore,
			retirementRegistry,
			agentcfgprotocol.NewUserSkillImportCapabilityPolicy(agentConfigRegistry,
				sessionOverlayStore, toolCat, cfg.Tools.GrantedScopes),
			agentcfgprotocol.WithImportAgentReach(agentReach),
			agentcfgprotocol.WithImportSessionReach(auth.NewSessionReachAuthorizer()),
			agentcfgprotocol.WithImportLogger(opts.Logger),
		)
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("user skill import service: %w", err)
		}
	}

	// The HA-66 read-only composition preview: the frozen boot index as
	// the BootPackReader — or the EMPTY immutable reader when no baseline
	// is declared, so boot config removal never 501s the preview and an
	// independently persisted active revision still appears as provenance
	// "revision" — plus the agent-config reader + the signed gates. Nil
	// only when the registry genuinely lacks the retirement/read seam (a
	// mandatory seam is absent — never a stub surface).
	var compositionPreviewService *agentcfgprotocol.CompositionPreviewService
	if retirementOK {
		compositionPreviewService, err = agentcfgprotocol.NewCompositionPreviewService(
			retirementRegistry,
			PreviewBootReader(bootIndex),
			agentcfgprotocol.WithPreviewAgentReach(agentReach),
			agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
			agentcfgprotocol.WithPreviewBus(bus),
			agentcfgprotocol.WithPreviewRedactor(red),
			agentcfgprotocol.WithPreviewLogger(opts.Logger),
		)
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("composition preview service: %w", err)
		}
	}
	// ── end v1.28 band ────────────────────────────────────────────────
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

	// The identity-tier policy WRITE policy (governance.set_posture) — the
	// StateStore-backed effective-policy record layered over the
	// config-declared defaults; the posture read reflects the same record.
	setPosturePolicy, err := governance.NewSetPosturePolicy(stack.State, bus,
		governance.ConfigFromOperator(cfg.Governance), nil, stack.GovernanceTierSource,
		stack.GovernanceEnforcementActive)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("governance set-posture policy: %w", err)
	}
	closers = append(closers, setPosturePolicy.Close)

	// The MCP attach/detach concretes — the attacher backs the runtime
	// add-connection verb AND the run-start ATTACH pass (one attach
	// implementation, never two); the detacher drives the run-start detach pass.
	var mcpAttacher agentcfgprotocol.ConnectionAttacher
	var mcpDetacher projection.ConnectionDetacher
	var mcpReattacher projection.ConnectionReattacher
	if toolCat != nil && mcpRegistry != nil {
		// Thread the runtime's MCP Apps tool-context store into the attacher so
		// a RUNTIME-ADDED connection captures an app-declaring tool call's
		// context exactly as a boot-config one does. The explicit nil check
		// matters: handing a typed-nil *ToolContextStore to the
		// mcpdrv.ToolContextCapturer interface would produce a non-nil
		// interface holding a nil pointer, and the driver's "is a capturer
		// wired?" test would answer yes for a store that cannot capture.
		var mcpToolCtxCapturer mcpdrv.ToolContextCapturer
		if mcpToolContext != nil {
			mcpToolCtxCapturer = mcpToolContext
		}
		// Thread the CURRENT boot policy the run-start ATTACH pass re-applies: the
		// fail-closed stdio command allowlist and the effective per-user
		// credential-injection opt-in (the config flag OR the boot env captured
		// once at process start). Both are re-evaluated at every re-attach against
		// the policy in force NOW, never the policy in force when the revision was
		// written — the reconcile kill-switch shape the provider installer already
		// ships, so a descriptor persisted while an opt-in was on is not rebuilt
		// after a restart with it off.
		attacher := NewMCPConnectionAttacher(toolCat, mcpRegistry, bus, opts.Logger,
			resolveMCPAttachIdentity(opts.MCPDefaultIdentity), oauthProviders, stack.OAuthProviderSet,
			mcpToolCtxCapturer,
			WithArtifactStore(artStore),
			// The runtime-add path carries the SAME egress ceiling the boot
			// attach path does, so a server attached over the control plane is
			// bounded identically to one declared in YAML.
			WithArtifactEgressMaxBytes(cfg.Tools.ResolvedMCPArtifactEgressMaxBytes()),
			WithReattachGates(MCPAddStdioAllowlist(cfg),
				cfg.Tools.AllowWireInjection || toolauth.AllowWireInjectionCaptured()))
		closers = append(closers, attacher.Close)
		mcpAttacher = attacher
		mcpReattacher = attacher
		mcpDetacher = NewMCPConnectionDetacher(toolCat, mcpRegistry, opts.Logger)
	}

	// The Protocol-installed OAuth provider installer (set_oauth_provider /
	// remove_oauth_provider + the run-start provider reconcile). Nil when no
	// provider set / builder is wired (no catalog), leaving the verbs at 501.
	// Effective DEV-ONLY wire-descriptor opt-in: the config flag OR the boot env
	// captured once at process start (cmd/harbor). Default false / fail-closed.
	// Threaded into BOTH the agent-config gate (the write path) AND the installer
	// (the reconcile kill-switch, so a wire provider persisted while it was on is
	// not rebuilt after a restart with it off).
	allowWireOAuthDescriptor := cfg.Tools.AllowWireOAuthDescriptor || toolauth.AllowWireOAuthDescriptorCaptured()
	var oauthProviderInstaller agentcfgprotocol.ProviderInstaller
	var oauthProviderReconciler projection.OAuthProviderReconciler
	if stack.OAuthProviderSet != nil && stack.OAuthProviderBuilder != nil {
		concrete := NewOAuthProviderInstaller(stack.OAuthProviderBuilder, stack.OAuthProviderSet, allowWireOAuthDescriptor, opts.Logger)
		if concrete != nil {
			oauthProviderInstaller = concrete
			oauthProviderReconciler = concrete
		}
	}
	// The verifier map is built at boot from the explicit broker trust
	// anchors. A configured JWKS fetch failure aborts boot rather than leaving a
	// partially enabled registration surface.
	signedOAuthMCPCapabilityAuthorities, err := SignedOAuthMCPCapabilityAuthoritiesFromConfig(ctx, cfg, opts.Logger)
	if err != nil {
		closeAll(ctx)
		return nil, err
	}
	// Signed-capability recovery is deliberately one bounded reconciler shared by boot and
	// every run start. It can enumerate only the configured bootstrap identity
	// here; tenant/user/session-specific recovery happens when that exact run
	// starts, never through a cross-tenant maintenance sweep.
	var signedOAuthMCPReconciler interface {
		ReconcileSignedOAuthMCPCapability(context.Context, identity.Quadruple, string) error
	}
	if len(signedOAuthMCPCapabilityAuthorities) > 0 {
		preparer, prepared := mcpAttacher.(agentcfgprotocol.ConnectionPreparer)
		// Signed-pair teardown is coupled to private preparation and needs the
		// exact generation/fence receipt carried by the attacher. The separate
		// mcpDetacher deliberately implements only the run-loop projection seam
		// (AttachedSources/Detach), so asking it for this Protocol seam leaves a
		// correctly composed runtime unable to boot whenever an authority is on.
		detacher, detachable := mcpAttacher.(agentcfgprotocol.ConnectionDetacher)
		providers, providerReady := oauthProviderInstaller.(agentcfgprotocol.SignedCapabilityProviderPreparer)
		if !prepared || !detachable || !providerReady {
			closeAll(ctx)
			return nil, fmt.Errorf("signed oauth mcp capability recovery: %w", agentcfgprotocol.ErrSignedCapabilityUnavailable)
		}
		concreteReconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(agentConfigRegistry, stack.State, preparer, detacher, providers)
		if err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("signed oauth mcp capability recovery: %w", err)
		}
		signedOAuthMCPReconciler = concreteReconciler
		bootIdentity := resolveMCPAttachIdentity(opts.MCPDefaultIdentity)
		if err := signedOAuthMCPReconciler.ReconcileSignedOAuthMCPCapability(ctx, identity.Quadruple{Identity: bootIdentity}, devAgentConfigID); err != nil {
			closeAll(ctx)
			return nil, fmt.Errorf("signed oauth mcp capability boot recovery: %w", err)
		}
	}

	// The Protocol-installed inference provider installer (set_llm_provider),
	// plus the boot-connect of a config-declared brokered primary. Wired
	// whenever an LLM driver is opened (the shared LiveKey is present); nil
	// otherwise, leaving the verb at 501. A brokered primary
	// (`llm.credential_source: remote`) that cannot be wired (or cannot
	// connect at boot) fails Boot LOUD — never a silent empty LiveKey.
	var llmProviderInstaller agentcfgprotocol.LLMProviderInstaller
	var inferenceBrokerNames []string
	if stack.LLMLiveKey != nil {
		concrete := NewLLMProviderInstaller(stack.LLMLiveKey, stack.LLMSnapshot.Provider,
			cfg.LLM.InferenceBrokers, bus, red, opts.InstanceID, opts.Logger)
		if concrete != nil {
			llmProviderInstaller = concrete
			inferenceBrokerNames = concrete.BrokerNames()
			closers = append(closers, concrete.Close)
			if cfg.LLM.CredentialSource == "remote" {
				if cErr := concrete.BootConnectPrimary(ctx, cfg.LLM.InferenceBroker); cErr != nil {
					closeAll(ctx)
					return nil, fmt.Errorf("llm brokered primary: %w", cErr)
				}
			}
		}
	} else if cfg.LLM.CredentialSource == "remote" {
		// The config declared a brokered primary but no LLM driver / LiveKey was
		// wired — fail loud rather than accept a config that can never source a
		// key (CLAUDE.md §13 "fail loudly at boot").
		closeAll(ctx)
		return nil, fmt.Errorf("llm.credential_source is \"remote\" but no LLM driver is wired to source the brokered key — set llm.driver or use llm.credential_source: local")
	}

	virtualProfiles, virtualProfilesErr := cfg.VirtualAgents.ToMap()
	if virtualProfilesErr != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("virtual profiles: %w", virtualProfilesErr)
	}
	// The HA-66 P0 composition at the run-loop seam: the frozen index
	// reaches the reader interface ONLY when a baseline is bound. A nil
	// `*bootpacks.Index` assigned directly into the interface field would
	// produce a NON-NIL interface holding a nil pointer, whose Lookup
	// panics at the first default-config run start. The run loop is
	// deliberately NOT PreviewBootReader — absence must preserve the
	// compatible no-baseline (nil) shape.
	var runLoopBootReader agentcfgprotocol.BootPackReader
	if bootIndex != nil {
		runLoopBootReader = bootIndex
	}
	runLoopDriver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Logger:                   opts.Logger,
		Bus:                      bus,
		RunLoop:                  runLoop,
		Planner:                  plnr,
		Tasks:                    taskReg,
		TaskKind:                 tasks.KindForeground,
		DriveBackground:          true,
		Memory:                   memStore,
		MemoryRecall:             memory.RecallFromConfig(cfg.Memory),
		SkillsDirectory:          skillsDir,
		PlanningHints:            planner.HintsFromConfig(cfg.Planner.PlanningHints),
		SkillStore:               skillStore,
		SessionPersonalSkills:    sessionPersonalStore,
		SessionSkillCutover:      sessionPersonalCutover,
		Catalog:                  toolCat,
		Executor:                 stack.Executor,
		MaxStepsRunLoop:          cfg.Planner.MaxSteps,
		TrancheSteps:             steering.EffectiveTrancheSteps(cfg.Planner.MaxSteps),
		GrantedScopes:            append([]string(nil), cfg.Tools.GrantedScopes...),
		ArtifactStore:            artStore,
		TokenBudget:              cfg.Planner.TokenBudget,
		Compression:              stack.Compression,
		DispositionPolicy:        dispositionPolicy,
		TenantOverrides:          tenantOverridePolicy,
		SessionOverrides:         runsStore,
		AgentConfig:              agentConfigRegistry,
		AgentConfigID:            devAgentConfigID,
		VirtualProfiles:          virtualProfiles,
		EnsureBootAgentLifecycle: bootLifecycleEnsurer,
		RunSnapshots:             runSnapshots,
		AgentReachAdmissions:     agentReachAdmissions,
		ProviderRouteRuntimeID:   opts.ProviderRoute.RuntimeID,
		PublicationStore:         publicationStore,
		PublicationRuntimeID:     publicationRuntimeID,
		SessionOverlay:           sessionOverlayStore,
		BootPackReader:           runLoopBootReader,
		RunCompletionHook:        projection.RunCompletionHookFromConfig(cfg.Runtime.Hooks.RunCompletion),
		ConnectionDetacher:       mcpDetacher,
		ConnectionReattacher:     mcpReattacher,
		BootDeclaredMCP:          BootDeclaredMCPServerSet(cfg),
		OAuthProviderReconciler:  oauthProviderReconciler,
		SignedOAuthMCPReconciler: signedOAuthMCPReconciler,
		NamingDefault:            cfg.Runtime.Naming,
		SessionTitler:            sessionRegistry,
		NamingLLM:                stack.LLM,
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

	// Auth validator — the REQUIRED production path. A factory error, or a
	// factory that hands back a nil Validator, fails Boot loud (never a
	// silent unauthenticated fallback). The nil check keeps that guarantee
	// real: BuildMux reads a nil Validator as the test-kit
	// transports.WithoutValidator() opt-out, and the serve band never
	// takes that path. Identity is mandatory — CLAUDE.md §6 rule 9.
	validator, err := opts.AuthValidatorFactory(ctx, cfg, red, bus, opts.Logger)
	if err != nil {
		closeAll(ctx)
		return nil, fmt.Errorf("auth: %w", err)
	}
	if validator == nil {
		closeAll(ctx)
		return nil, ErrAuthValidatorRequired
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

	// The HA-66 P0 composition at the mux seam: a SEPARATE actual-nil
	// BootOwnership interface, populated only when the index is non-nil.
	// The pre-fix typed-nil assignment made the first pack mutation call
	// OwnsName on a nil receiver. The read-only composition preview keeps
	// using PreviewBootReader(bootIndex) so no-config revision-only
	// preview stays available.
	var bootOwnership agentcfgprotocol.BootOwnership
	if bootIndex != nil {
		bootOwnership = bootIndex
	}
	muxInput := MuxInput{
		Cfg:                            cfg,
		Surface:                        surface,
		Bus:                            bus,
		Redactor:                       red,
		Logger:                         opts.Logger,
		Metrics:                        metricsReg,
		LLMSnapshot:                    llmCfg,
		ProviderCatalog:                providerCatalog,
		ExternalGrantReadiness:         externalGrantReadinessProvider(cfg.LLM.ExternalGrant, opts.ExternalGrant, opts.ExternalGrantDelivery, stockCoordinator),
		Tasks:                          taskReg,
		Sessions:                       sessionRegistry,
		Agents:                         agentRegistry,
		Artifacts:                      artStore,
		Memory:                         memStore,
		Catalog:                        toolCat,
		Coordinator:                    coord,
		MCPRegistry:                    mcpRegistry,
		MCPToolContext:                 mcpToolContext,
		SourceAuthorizer:               sourceAuthorizer,
		State:                          stack.State,
		Skills:                         skillStore,
		AgentPackLLM:                   stack.LLM,
		AgentConfig:                    agentConfigRegistry,
		AgentConfigID:                  devAgentConfigID,
		AgentResolver:                  agentResolver,
		BootLifecycleEnsurer:           bootLifecycleEnsurer,
		RunSnapshots:                   runSnapshots,
		SessionOverlay:                 sessionOverlayStore,
		SessionPersonalSkillController: sessionPersonalController,
		RunsStore:                      runsStore,
		RunLoopDriver:                  runLoopDriver,
		OAuthProviders:                 oauthProviders,
		TenantOverridePolicy:           tenantOverridePolicy,
		SetPosturePolicy:               setPosturePolicy,
		KeyRotator:                     stack.KeyRotator,
		ValidModels:                    validModels,
		MCPAttacher:                    mcpAttacher,
		MCPStdioAllowlist:              MCPAddStdioAllowlist(cfg),
		BootDeclaredMCP:                BootDeclaredMCPServerNames(cfg),
		BootDeclaredOAuth:              BootDeclaredOAuthProviderNames(cfg),
		AllowWireOAuthDescriptor:       allowWireOAuthDescriptor,
		AllowWireInjection:             cfg.Tools.AllowWireInjection || toolauth.AllowWireInjectionCaptured(),
		OAuthProviderInstaller:         oauthProviderInstaller,
		LLMProviderInstaller:           llmProviderInstaller,
		InferenceBrokers:               inferenceBrokerNames,
		Validator:                      validator,
		AuthSurface:                    authSurface,
		AgentReach:                     agentReach,
		ProviderRouteRuntimeID:         opts.ProviderRoute.RuntimeID,
		PublicationStore:               publicationStore,
		PublicationRuntimeID:           publicationRuntimeID,
		DisplayName:                    opts.DisplayName,
		InstanceID:                     opts.InstanceID,
		BuildVersion:                   opts.BuildVersion,
		BuildCommit:                    opts.BuildCommit,
		FrameworkVersion:               opts.FrameworkVersion,
		FrameworkCommit:                opts.FrameworkCommit,
		TopologyAvailable:              false,
		RenderAdmissionAuthority:       admissionAuthority,
		RenderAdmissionGate:            admissionGate,
		TurnsProjector:                 turnsProj,
		TurnsStore:                     turnsStore,
		RollupsStore:                   rollupsStore,
		RollupsQuality:                 rollupsWorker,
		UserSkillImportService:         userSkillImportService,
		CompositionPreviewService:      compositionPreviewService,
		BootOwnership:                  bootOwnership,
	}
	muxInput.SignedOAuthMCPCapabilityAuthorities = signedOAuthMCPCapabilityAuthorities
	built, err := BuildMux(muxInput)
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
		readyCh:         make(chan struct{}),
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
		h.signalReady(readiness{err: fmt.Errorf("listen %s: %w", h.server.Addr, err)})
		return fmt.Errorf("listen %s: %w", h.server.Addr, err)
	}
	boundAddr := listener.Addr().String()
	h.mu.Lock()
	h.bindAddr = boundAddr
	h.mu.Unlock()
	// Signal readiness exactly once at the bind site — the race-safe
	// one-shot contract a co-launched TUI (or any external caller) waits
	// on through WaitReady. No polling, no second listener lifecycle.
	h.signalReady(readiness{addr: boundAddr})
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
// Close also fires a "closed before bind" readiness signal so a WaitReady
// waiter on a handle whose Serve never bound (or was cancelled before
// binding) returns promptly instead of blocking until its own ctx expires.
func (h *Handle) Close(ctx context.Context) {
	h.mu.Lock()
	closers := h.closeFns
	h.closeFns = nil
	h.mu.Unlock()
	h.signalReady(readiness{err: errors.New("serve: handle closed")})
	for i := len(closers) - 1; i >= 0; i-- {
		if cErr := closers[i](ctx); cErr != nil && h.logger != nil {
			h.logger.Warn("serve: error closing subsystem during drain",
				slog.String("error", cErr.Error()))
		}
	}
}

// WaitReady blocks until the listener binds (returning the actual
// OS-assigned address) or until the bind fails / ctx cancels (returning
// the error). It is the race-safe one-shot readiness contract a
// co-launched client (the TUI, an operator script, an embedder) waits on
// before dialing the server — no polling, no second listener lifecycle.
//
// Safe to call from multiple goroutines before, during, or after Serve
// binds: the first successful bind delivers the address to every waiter
// through the buffered channel; a late caller after a successful bind
// returns immediately. If Serve was never called or was cancelled before
// binding, WaitReady blocks until ctx cancels (callers SHOULD scope their
// wait with a deadline). Close fires a "closed before bind" signal so a
// waiter on an abandoned handle returns promptly.
func (h *Handle) WaitReady(ctx context.Context) (string, error) {
	select {
	case <-h.readyCh:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.ready.addr, h.ready.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
