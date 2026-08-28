// mux.go — the single-homed Protocol surface construction + transport
// fan-out for the serve band.
//
// Both the promoted serve constructor (cmd/harbor's serve / dev / console)
// and the operator test kit (harbortest/devstack) mount the SAME Protocol
// surface set. Before this was single-homed the two compositions had already
// drifted on which surfaces they mounted (the kit's mux omitted the agents,
// auth-rotate, governance-override, and governance-key-rotate surfaces). This
// file is the ONE place that maps the assembled subsystem handles onto
// transport options, so the two callers can never diverge again.
//
// BuildMux is pure construction: it reads the handles in MuxInput, builds the
// Protocol surfaces the non-nil handles imply, and returns the mounted mux
// plus the flow registry it created (the caller's post-boot fixture seeding
// needs the same registry the flows surface serves). It owns no long-lived
// goroutines and registers no closers — every closer-bearing collaborator
// (the MCP attacher, the tenant-override policy, the key rotator) is built by
// the caller and its Close registered there.

package serve

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	goruntime "runtime"
	"strings"
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
	"github.com/hurtener/Harbor/internal/llm/provider"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/memory"
	observabilityprotocol "github.com/hurtener/Harbor/internal/observability/protocol"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/packproposer"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	governanceprotocol "github.com/hurtener/Harbor/internal/runtime/governance/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	agentregistry "github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/search"
	searchartifacts "github.com/hurtener/Harbor/internal/search/artifacts"
	searchevents "github.com/hurtener/Harbor/internal/search/events"
	searchsessions "github.com/hurtener/Harbor/internal/search/sessions"
	searchtasks "github.com/hurtener/Harbor/internal/search/tasks"
	"github.com/hurtener/Harbor/internal/server"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	turnsprotocol "github.com/hurtener/Harbor/internal/sessions/turns/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/publication"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/annotate"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// MuxInput carries the assembled subsystem handles and the caller-side
// posture stamps BuildMux mounts the Protocol surface over. Every handle is
// optional except the ControlSurface and the bus: a nil handle leaves the
// corresponding surface un-mounted (the route then 404s), exactly as the two
// callers already behave under their skip/absence paths.
type MuxInput struct {
	Cfg     *config.Config
	Surface *protocol.ControlSurface
	Bus     events.EventBus
	// Redactor / Logger / Metrics feed every surface's audit + posture legs.
	Redactor audit.Redactor
	Logger   *slog.Logger
	Metrics  *telemetry.MetricsRegistry
	// LLMSnapshot is the resolved snapshot the posture provider projects.
	LLMSnapshot            llm.ConfigSnapshot
	ProviderCatalog        provider.CatalogSurface
	ExternalGrantReadiness func() types.ExternalGrantReadiness

	// Core subsystem handles.
	Tasks          tasks.TaskRegistry
	Sessions       *sessions.Registry
	Agents         *agentregistry.Registry
	Artifacts      artifacts.ArtifactStore
	Memory         memory.MemoryStore
	Catalog        tools.ToolCatalog
	Coordinator    pauseresume.Coordinator
	MCPRegistry    *mcpdrv.Registry
	MCPToolContext *mcpconsole.ToolContextStore
	// SourceAuthorizer is the shared effective-source projection for
	// user-owned MCP attachments. Nil lets BuildMux construct it from the
	// registry and AgentConfig for compatibility with direct callers.
	SourceAuthorizer *mcpconsole.SourceAuthorizer
	State            state.StateStore
	Skills           skills.SkillStore
	// AgentPackLLM is the configured model client used by governed pack
	// authoring. BuildMux wraps it in the production proposer seam.
	AgentPackLLM llm.LLMClient

	// Control-plane handles.
	AgentConfig          agentcfg.Registry
	AgentConfigID        string
	AgentResolver        protocol.AgentResolver
	BootLifecycleEnsurer agentcfg.BootLifecycleEnsurer
	RunSnapshots         *runsnapshot.Gate
	SessionOverlay       sessionoverlay.Store
	// SessionPersonalSkillController is the single durable authority for the
	// session-personal skill Protocol tier and its dynamic overlay projection.
	// Nil keeps those methods fail-loud/unavailable; BuildMux never falls back
	// to SkillStore or the schema-1 Overlay.PersonalSkills field.
	SessionPersonalSkillController agentcfgprotocol.SessionPersonalSkillController
	RunsStore                      *runsprotocol.Store
	// RunLoopDriver backs the tasks.get trajectory enricher. Nil leaves
	// tasks reads un-enriched (a stack without a run loop).
	RunLoopDriver  *RunLoopDriver
	OAuthProviders map[string]toolauth.OAuthProvider

	// Governance handles. TenantOverridePolicy backs governance.set/get
	// tenant overrides; KeyRotator backs governance.rotate_key. Both are
	// built caller-side (the policy is shared with the run-loop driver; the
	// rotator's lifecycle is the assembly's).
	TenantOverridePolicy *governance.TenantOverridePolicy
	// SetPosturePolicy backs governance.set_posture — the admin identity-tier
	// policy WRITE surface (the StateStore-backed effective-policy record the
	// posture read layers over). Built caller-side; nil leaves the
	// set_posture route at 501 (the partial-build convention).
	SetPosturePolicy *governance.SetPosturePolicy
	KeyRotator       *llm.ProviderKeyRotator
	ValidModels      []string

	// MCPAttacher backs agent_config.add_mcp_connection. Built caller-side so
	// its Close joins the caller's closer chain; nil leaves the add verb
	// unwired.
	MCPAttacher       agentcfgprotocol.ConnectionAttacher
	MCPStdioAllowlist []string
	BootDeclaredMCP   []string

	// OAuthProviderInstaller backs agent_config.set_oauth_provider /
	// remove_oauth_provider (the Protocol-installed, zero-URL broker-pull
	// provider). Built caller-side; nil leaves the install verbs unwired (→ 501).
	OAuthProviderInstaller agentcfgprotocol.ProviderInstaller
	// SignedOAuthMCPCapabilityAuthorities are boot-built verifier
	// anchors, keyed by broker name. Empty leaves signed registration disabled.
	SignedOAuthMCPCapabilityAuthorities map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority
	// SignedOAuthMCPUserReconciler is the single durable live-profile recovery
	// artifact shared by run start and the user-tier reconcile operation.
	SignedOAuthMCPUserReconciler agentcfgprotocol.SignedOAuthMCPUserReconciler
	// BootDeclaredOAuth is the set of boot-declared OAuth provider names
	// (tools.oauth_providers[].name); an install/uninstall of one of these is
	// refused (boot wins).
	BootDeclaredOAuth []string
	// AllowWireOAuthDescriptor is the effective DEV-ONLY, fail-closed opt-in that
	// permits set_oauth_provider / add_mcp_connection to carry a FULL OAuth
	// provider binding over the wire (token_url / audience / scopes / remote{}).
	// The caller passes (tools.allow_wire_oauth_descriptor) OR (the captured
	// HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR boot env). Default false / fail-closed.
	AllowWireOAuthDescriptor bool
	// AllowWireInjection is the effective DEV-ONLY, fail-closed opt-in that
	// permits add_mcp_connection to carry a per-user credential-INJECTION mapping
	// (the `injection` object) for a receiver-style MCP server over the wire. The
	// caller passes (tools.allow_wire_injection) OR (the captured
	// HARBOR_ALLOW_WIRE_INJECTION boot env). INDEPENDENT of
	// AllowWireOAuthDescriptor. Default false / fail-closed.
	AllowWireInjection bool

	// LLMProviderInstaller backs agent_config.set_llm_provider (the
	// Protocol-installed, zero-URL broker-pull inference provider). Built
	// caller-side; nil leaves the verb unwired (→ 501).
	LLMProviderInstaller agentcfgprotocol.LLMProviderInstaller
	// InferenceBrokers is the set of boot-declared inference-broker names
	// (llm.inference_brokers[].name); a set_llm_provider naming a broker outside
	// this set is refused (→ 400 — no admin-writable field determines a sink).
	InferenceBrokers []string

	// Auth. Validator nil mounts the transports WithoutValidator (the
	// explicit test-kit opt-out); AuthSurface nil leaves auth.rotate_token
	// un-mounted (the production posture — no in-runtime token issuer).
	Validator   auth.Validator
	AuthSurface *auth.RotateSurface
	// AgentReach is the gate shared with the control surface by runtime
	// assembly. Nil still builds fail-closed stream projections.
	AgentReach             auth.AgentReachAuthorizer
	ProviderRouteRuntimeID string
	// PublicationStore is the one authorized StateStore-backed store shared by
	// the Protocol surface and the run-loop driver's exact-reference resolver.
	// Nil leaves the publication surface unmounted.
	PublicationStore     publication.Store
	PublicationRuntimeID string

	// Posture stamps.
	DisplayName      string
	InstanceID       string
	BuildVersion     string
	BuildCommit      string
	FrameworkVersion string
	FrameworkCommit  string
	// TopologyAvailable advertises the topology.snapshot capability (true
	// only when the caller wired a topology accessor onto the surface).
	TopologyAvailable bool

	// v1.28 composition handles (all optional — nil leaves the
	// corresponding surface unwired at 501 / the compatible disabled
	// posture).

	// RenderAdmissionAuthority / RenderAdmissionGate are the HA-56
	// fresh render-admission pair the AppsSurface mounts. Both must be
	// wired together (the surface rejects a half-wired pair); neither
	// wired keeps the compatible disabled surface.
	RenderAdmissionAuthority protocol.RenderAdmissionAuthority
	RenderAdmissionGate      protocol.RenderAdmissionGate

	// TurnsProjection is the HA-64 conversation-turn read seam
	// (turns/protocol.Projector) plus its store (the erasure-cascade
	// fencer). TurnsStore is nil when the projection is unwired.
	TurnsProjector *turns.Projector
	TurnsStore     turns.Store

	// RollupsStore / RollupsQuality are the HA-65 observability rollup
	// read seams (the Querier + the freshness QualitySource).
	RollupsStore   rollups.Store
	RollupsQuality observabilityprotocol.QualitySource

	// UserSkillImportService is the HA-61 two-phase import service;
	// CompositionPreviewService is the HA-66 read-only composition
	// preview. BootOwnership is the immutable boot-pack index the serve
	// band binds into every `/v1/agent_config/*` request context at the
	// mux boundary, so the agent-pack mutation guards fire on every
	// write path (nil = no boot baseline, guards inert).
	UserSkillImportService    *agentcfgprotocol.UserSkillImportService
	CompositionPreviewService *agentcfgprotocol.CompositionPreviewService
	BootOwnership             agentcfgprotocol.BootOwnership
}

// BuiltMux is the result of BuildMux: the mounted Protocol mux plus the flow
// registry the flows surface serves (the caller seeds the same registry).
type BuiltMux struct {
	// Mux is the mounted Protocol handler the caller mounts under /v1/.
	// It is an http.Handler (not a concrete *http.ServeMux) because the
	// HA-66 boot-ownership reader may wrap the transports mux in a thin
	// request-context middleware; callers only ever route /v1/ to it.
	Mux http.Handler
	// FlowRegistry is the empty registry the flows surface serves. Nil when
	// the flows surface was not mounted (no artifact store / task registry).
	FlowRegistry *flow.Registry
}

// sessionWindowFunc resolves a session's lifetime window from the
// sessions Registry snapshot (OpenedAt / LastSeen) — the coverage proof
// the projection-backed counter enricher requires before serving exact
// projection values.
func sessionWindowFunc(reg *sessions.Registry) sessionsprotocol.SessionWindowFunc {
	return func(ctx context.Context, id identity.Identity, sessionID string) (time.Time, time.Time, bool, error) {
		if reg == nil {
			return time.Time{}, time.Time{}, false, nil
		}
		snap, err := reg.Inspect(ctx, sessionID)
		if err != nil {
			return time.Time{}, time.Time{}, false, err
		}
		if snap == nil || snap.OpenedAt.IsZero() {
			return time.Time{}, time.Time{}, false, nil
		}
		last := snap.LastSeen
		if last.IsZero() {
			last = snap.OpenedAt
		}
		return snap.OpenedAt, last, true, nil
	}
}

// BuildMux constructs every Protocol surface the non-nil handles in MuxInput
// imply and returns the mounted transports mux. It is the single source of
// the handle→transport-option mapping shared by cmd/harbor and the test kit.
func BuildMux(in MuxInput) (*BuiltMux, error) {
	cfg := in.Cfg
	bus := in.Bus
	red := in.Redactor
	logger := in.Logger

	muxOpts := []transports.Option{}
	if logger != nil {
		// Thread the caller's logger into the transports so serve-side
		// request logs flow through the production handler (JSON under
		// `harbor serve`), never a default text fallback.
		muxOpts = append(muxOpts, transports.WithLogger(logger))
	}
	if in.Validator != nil {
		muxOpts = append(muxOpts, transports.WithValidator(in.Validator))
	} else {
		// Auth explicitly opted out (the test-kit WithoutValidator path).
		muxOpts = append(muxOpts, transports.WithoutValidator())
	}
	muxOpts = append(muxOpts, transports.WithAgentReachAuthorizer(in.AgentReach))

	// The session-erasure cascade is available only when every scoped store
	// the cascade deletes is present; the same condition gates the capability
	// advertisement so it is honest about the route.
	sessionLifecycleAvailable := in.Sessions != nil && in.State != nil &&
		in.Memory != nil && in.Artifacts != nil

	// The per-tool annotator (OAuth / approval / metrics / content-stats /
	// last-used) is wired when the catalog + its state-backed approval-policy
	// store are present — the same condition that gates the capability
	// advertisement so it is honest about the surface. A catalog without a
	// StateStore (a headless read-only stack) leaves the annotator unwired and
	// the tool-annotation facets loud-reject (the honest degradation), never a
	// false-empty page.
	toolAnnotationsAvailable := in.Catalog != nil && in.State != nil
	stateSnapshotsAvailable := in.Tasks != nil && in.Sessions != nil &&
		in.Coordinator != nil && in.Artifacts != nil
	publicationAvailable := in.PublicationStore != nil
	if publicationAvailable {
		if in.AgentReach == nil {
			return nil, wrapErr("skill publications", fmt.Errorf("%w: signed Agent-reach authorizer is nil", ErrPublicationWiringMisconfigured))
		}
		if strings.TrimSpace(in.PublicationRuntimeID) == "" {
			return nil, wrapErr("skill publications", fmt.Errorf("%w: runtime/deployment ID is empty", ErrPublicationWiringMisconfigured))
		}
		skillPublications, pErr := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{
			Store:      in.PublicationStore,
			AgentReach: in.AgentReach,
			RuntimeID:  in.PublicationRuntimeID,
		})
		if pErr != nil {
			return nil, wrapErr("skill publications surface", pErr)
		}
		muxOpts = append(muxOpts, transports.WithSkillPublicationsSurface(skillPublications))
	}

	postureSurface, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Build: types.RuntimeInfo{
			BuildVersion:       in.BuildVersion,
			BuildCommit:        in.BuildCommit,
			BuildGoVersion:     goruntime.Version(),
			FrameworkVersion:   in.FrameworkVersion,
			FrameworkCommit:    in.FrameworkCommit,
			MCPAppDisplayModes: cfg.Tools.MCPAppHostDisplayModes(),
		},
		Clock:    time.Now,
		BootedAt: time.Now(),
		Health: func(_ context.Context) []types.SubsystemHealth {
			return runtimeposture.HealthFromConfig(cfg)
		},
		Retention: runtimeposture.RetentionProvider(bus, in.Tasks, in.Sessions),
		Counters:  runtimeposture.CountersProvider(in.Tasks, in.Sessions, in.MCPRegistry),
		Drivers: func() []types.SubsystemDriver {
			return runtimeposture.DriversFromConfig(cfg)
		},
		Metrics:                    runtimeposture.MetricsProvider(in.Metrics, logger),
		Governance:                 governance.NewPostureProviderWithState(governance.ConfigFromOperator(cfg.Governance), in.State),
		LLM:                        llm.NewPostureProvider(in.LLMSnapshot),
		ProviderCatalog:            in.ProviderCatalog,
		AgentReach:                 in.AgentReach,
		ProviderRouteRuntimeID:     in.ProviderRouteRuntimeID,
		Redactor:                   red,
		Bus:                        bus,
		DisplayName:                in.DisplayName,
		InstanceID:                 in.InstanceID,
		ExternalGrant:              in.ExternalGrantReadiness,
		TopologyAvailable:          in.TopologyAvailable,
		AgentConfigAvailable:       in.AgentConfig != nil,
		StateSnapshotsAvailable:    stateSnapshotsAvailable,
		SessionLifecycleAvailable:  sessionLifecycleAvailable,
		ToolAnnotationsAvailable:   toolAnnotationsAvailable,
		SkillPublicationsAvailable: publicationAvailable,
		ProviderCatalogAvailable:   in.ProviderCatalog != nil,
	})
	if err != nil {
		return nil, wrapErr("posture surface", err)
	}
	muxOpts = append(muxOpts, transports.WithPostureSurface(postureSurface))

	// MCP servers + Apps host surfaces (the catalog band).
	if in.MCPRegistry != nil {
		sourceAuthorizer := in.SourceAuthorizer
		if sourceAuthorizer == nil {
			sourceAuthorizer = mcpconsole.NewSourceAuthorizer(in.MCPRegistry, in.AgentConfig, in.AgentReach)
		}
		// Wire the on-demand OAuth-requirement discovery walker into
		// the probe path: a probe against a server that answered a
		// `WWW-Authenticate` OAuth step-up triggers the RFC 9728 → RFC 8414
		// chain walk, whose verbatim result the operator reads via
		// mcp.servers.get. Harbor never runs the flow or holds a token.
		mcpRegAccessor, aErr := mcpconsole.NewRegistryAccessor(
			in.MCPRegistry,
			mcpconsole.WithSourceAuthorizer(sourceAuthorizer),
			mcpconsole.WithOAuthDiscoverer(toolauth.NewDiscoverer()),
		)
		if aErr != nil {
			return nil, wrapErr("mcp accessor", aErr)
		}
		mcpSurface, sErr := protocol.NewMCPSurface(protocol.MCPDeps{
			MCP:           mcpRegAccessor,
			OAuth:         mcpconsole.NewNoOAuthAccessor(),
			Redactor:      red,
			Bus:           bus,
			AgentResolver: in.AgentResolver,
			AgentReach:    in.AgentReach,
		})
		if sErr != nil {
			return nil, wrapErr("mcp surface", sErr)
		}
		muxOpts = append(muxOpts, transports.WithMCPSurface(mcpSurface))

		appsAccessor, aaErr := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
			Registry:         in.MCPRegistry,
			Catalog:          in.Catalog,
			Store:            in.Artifacts,
			Bus:              bus,
			ToolContext:      in.MCPToolContext,
			AgentConfig:      in.AgentConfig,
			AgentID:          in.AgentConfigID,
			SourceAuthorizer: sourceAuthorizer,
			SessionOverlay:   in.SessionOverlay,
			// PINNED, not threaded: the MCP Apps reads are
			// browser-rendered Protocol replies, so they select
			// inline-versus-reference at the Console inline-payload
			// bound rather than tracking the operator's LLM-context
			// heavy-output threshold.
			Threshold: config.DefaultConsoleInlinePayloadBytes,
		})
		if aaErr != nil {
			return nil, wrapErr("mcp apps accessor", aaErr)
		}
		appsSurface, asErr := protocol.NewAppsSurface(protocol.AppsDeps{
			Resource:                 appsAccessor,
			Invoker:                  appsAccessor,
			ToolContext:              appsAccessor,
			AgentResolver:            in.AgentResolver,
			AgentReach:               in.AgentReach,
			RenderAdmissionAuthority: in.RenderAdmissionAuthority,
			RenderAdmissionGate:      in.RenderAdmissionGate,
		})
		if asErr != nil {
			return nil, wrapErr("mcp apps surface", asErr)
		}
		muxOpts = append(muxOpts, transports.WithAppsSurface(appsSurface))
	}

	if in.Coordinator != nil && in.Artifacts != nil {
		// PINNED, not threaded: `pause.list` is a Console/TUI read whose
		// inline-versus-reference selection is Protocol-visible, so it
		// uses the Console inline-payload bound rather than the
		// operator's LLM-context heavy-output threshold.
		muxOpts = append(muxOpts, transports.WithPauseList(
			in.Coordinator, in.Artifacts, config.DefaultConsoleInlinePayloadBytes))
	}
	if in.Memory != nil {
		muxOpts = append(muxOpts, transports.WithMemory(in.Memory, cfg.Memory.Driver))
	}
	if in.Artifacts != nil {
		muxOpts = append(muxOpts, transports.WithStateHistory(bus, in.Artifacts))
		muxOpts = append(muxOpts, transports.WithEventsList(in.Artifacts))
	}

	if in.Catalog != nil {
		var toolsProjectorOpts []toolsprotocol.CatalogProjectorOption
		if in.AgentConfig != nil {
			toolsProjectorOpts = append(toolsProjectorOpts,
				toolsprotocol.WithCatalogViewResolver(projection.CatalogViewResolver{
					Registry:       in.AgentConfig,
					SessionOverlay: in.SessionOverlay,
					Catalog:        in.Catalog,
					OwnerResolver:  in.MCPRegistry,
				}),
				toolsprotocol.WithLoadingResolver(projection.LoadingResolverAdapter{
					Registry:      in.AgentConfig,
					OwnerResolver: in.MCPRegistry,
				}))
		}
		// Assemble + wire the production per-tool Annotator (the one-line seam;
		// the weight is the annotator's aggregation, not the wiring). It reads
		// OAuth status from tools/auth, the approval posture from the
		// state-backed approval-policy store, and metrics / last-used /
		// content-stats read-time from the events stream. Wiring it flips
		// AnnotationsAvailable() on, so the OAuth / approval facets, the version
		// search axis, and the catalog aggregates operate over real data instead
		// of loud-rejecting. The projection-completeness gate's Half-B prod-wiring
		// test drives BuildMux and proves a dropped WithAnnotator ships false
		// absence (the projection-completeness gate).
		if toolAnnotationsAvailable {
			policyStore, psErr := toolapproval.NewStatePolicyStore(in.State)
			if psErr != nil {
				return nil, wrapErr("tools/protocol approval-policy store", psErr)
			}
			annotator, aErr := annotate.NewAnnotator(annotate.Deps{
				Catalog:  in.Catalog,
				Approval: policyStore,
				Events:   bus,
				OAuth:    annotate.NewProviderOAuthReader(in.OAuthProviders),
				// Threaded deliberately: `tools.content_stats` REPORTS the
				// offload threshold and counts offload events against it,
				// so reporting the pinned Console bound here would make
				// the field lie.
				HeavyThresholdBytes: int64(cfg.Artifacts.HeavyOutputThresholdBytes),
			})
			if aErr != nil {
				return nil, wrapErr("tools/protocol annotator", aErr)
			}
			toolsProjectorOpts = append(toolsProjectorOpts, toolsprotocol.WithAnnotator(annotator))
		}
		toolsProjector, pErr := toolsprotocol.NewCatalogProjector(in.Catalog, toolsProjectorOpts...)
		if pErr != nil {
			return nil, wrapErr("tools/protocol projector", pErr)
		}
		toolsService, sErr := toolsprotocol.NewService(toolsProjector,
			toolsprotocol.WithBus(bus),
			toolsprotocol.WithRedactor(red),
			toolsprotocol.WithLogger(logger),
		)
		if sErr != nil {
			return nil, wrapErr("tools/protocol service", sErr)
		}
		muxOpts = append(muxOpts, transports.WithToolsService(toolsService))
	}

	if in.Agents != nil {
		var agentsProjectorOpts []agentsprotocol.ProjectorOption
		// Surface the runtime's synthetic default agent — the boot-configured
		// agent every process serves through but never registers as a fleet
		// entity — as a first-class, IsDefault-marked catalog row. Wired off
		// the boot agent id already threaded through the assembly. An empty
		// id leaves the projector byte-identical to an unwired one.
		if in.AgentConfigID != "" {
			agentsProjectorOpts = append(agentsProjectorOpts,
				agentsprotocol.WithDefaultAgent(agentsprotocol.DefaultAgentDescriptor{
					ID:          in.AgentConfigID,
					DisplayName: in.AgentConfigID,
					BootedAt:    time.Now(),
				}))
		}
		agentsProjector, pErr := agentsprotocol.NewRegistryProjector(in.Agents, agentsProjectorOpts...)
		if pErr != nil {
			return nil, wrapErr("registry/protocol projector", pErr)
		}
		agentsService, sErr := agentsprotocol.NewService(agentsProjector,
			agentsprotocol.WithLogger(logger),
			agentsprotocol.WithController(in.Agents),
			agentsprotocol.WithBus(bus),
			agentsprotocol.WithRedactor(red),
		)
		if sErr != nil {
			return nil, wrapErr("registry/protocol service", sErr)
		}
		muxOpts = append(muxOpts, transports.WithAgentsService(agentsService))
	}

	var flowRegistry *flow.Registry
	if in.Artifacts != nil && in.Tasks != nil {
		flowRegistry = flow.NewRegistry()
		// PINNED, not threaded — same Console-read reason as `pause.list`
		// and the `memory.*` handlers above.
		flowCatalog, fcErr := flowprotocol.NewRegistryCatalog(flowRegistry, in.Artifacts, config.DefaultConsoleInlinePayloadBytes)
		if fcErr != nil {
			return nil, wrapErr("flow protocol catalog", fcErr)
		}
		taskReg := in.Tasks
		flowInvoker, fiErr := flowprotocol.NewFuncInvoker(
			func(launchCtx context.Context, id identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
				runCtx, rerr := identity.WithRun(launchCtx, id, "flow-run-"+flowID)
				if rerr != nil {
					return "", time.Time{}, wrapErr("flows.run: identity scope incomplete", rerr)
				}
				handle, serr := taskReg.SpawnTool(runCtx, tasks.SpawnToolRequest{
					Identity:    identity.Quadruple{Identity: id},
					ToolName:    flowID,
					Description: "Console flows.run invocation of " + flowID,
				})
				if serr != nil {
					return "", time.Time{}, wrapErr("flows.run: spawn failed", serr)
				}
				return string(handle.ID), time.Now(), nil
			}, flowRegistry)
		if fiErr != nil {
			return nil, wrapErr("flow protocol invoker", fiErr)
		}
		flowsSurface, fsErr := flowprotocol.NewSurface(flowCatalog, flowInvoker)
		if fsErr != nil {
			return nil, wrapErr("flow protocol surface", fsErr)
		}
		muxOpts = append(muxOpts, transports.WithFlows(flowsSurface))
	}

	if in.Tasks != nil {
		var projectorOpts []tasksprotocol.RegistryProjectorOption
		if in.RunLoopDriver != nil {
			// The tasks.get parent-session card reads the session's status +
			// timestamps from the session lister (the parent-session un-stub); a nil lister
			// leaves the SessionID-only baseline.
			var enricherOpts []EnricherOption
			if in.Sessions != nil {
				enricherOpts = append(enricherOpts, WithSessionLister(in.Sessions))
			}
			projectorOpts = append(projectorOpts,
				tasksprotocol.WithEnricher(NewEnricher(in.RunLoopDriver.TrajectoryByTaskID, enricherOpts...)))
		}
		// Wire the list-time approval-gate seam so `has_pending_approval`
		// narrows to real open gates (the projection-completeness gate). Production ALWAYS wires it when
		// a pause coordinator is present; the projection gate's Half-B
		// prod-wiring test proves a forgotten WithApprovalChecker would ship a
		// permanently-false facet.
		if in.Coordinator != nil {
			projectorOpts = append(projectorOpts,
				tasksprotocol.WithApprovalChecker(NewApprovalChecker(in.Coordinator)))
		}
		tasksProjector, pErr := tasksprotocol.NewRegistryProjector(in.Tasks, projectorOpts...)
		if pErr != nil {
			return nil, wrapErr("tasks/protocol projector", pErr)
		}
		tasksService, sErr := tasksprotocol.NewService(tasksProjector,
			tasksprotocol.WithBus(bus),
			tasksprotocol.WithRedactor(red),
			tasksprotocol.WithLogger(logger),
		)
		if sErr != nil {
			return nil, wrapErr("tasks/protocol service", sErr)
		}
		muxOpts = append(muxOpts, transports.WithTasksService(tasksService))
	}

	if in.Sessions != nil {
		var sessionProjectorOpts []sessionsprotocol.ListerProjectorOption
		// Wire the read-time counter enricher whenever the aggregation deps
		// are present (they are, in every non-headless assembly): the event
		// substrate (cost / tokens / events), the task registry (tasks_count
		// / has_failed_task), and the pause coordinator
		// (has_pending_intervention). This is the WARN-3 "production ALWAYS
		// wires it" path: with the enricher wired the cost / failed /
		// intervention facets and the cost_desc sort operate on TRUTHFUL
		// data. A build missing any dep leaves the enricher unwired, and the
		// Service loud-rejects a counter facet/sort rather than lying.
		if bus != nil && in.Tasks != nil && in.Coordinator != nil {
			var sessionEnricher sessionsprotocol.Enricher
			sessionEnricher, eErr := sessionsprotocol.NewCounterEnricher(sessionsprotocol.CounterEnricherDeps{
				Bus:    bus,
				Tasks:  in.Tasks,
				Pauses: in.Coordinator,
				Logger: logger,
			})
			if eErr != nil {
				return nil, wrapErr("sessions/protocol counter enricher", eErr)
			}
			// HA-65: when the observability rollup projection is wired, the
			// session counters ride the projection-backed enricher — the
			// authoritative durable rollup rows for cost / tokens /
			// failed-task outcomes, with the raw bounded scan as the honest
			// partial fallback (a catching-up / unavailable projection, an
			// unresolvable session window, or a retention gap delegates
			// verbatim to the raw scan; the projection owns exactly the
			// dimensions it is authoritative for, never a subset mapped
			// onto a broader public counter).
			if in.RollupsStore != nil && in.RollupsQuality != nil && in.Sessions != nil {
				projectionEnricher, pErr := sessionsprotocol.NewProjectionEnricher(sessionsprotocol.ProjectionEnricherDeps{
					Store:    in.RollupsStore,
					Quality:  in.RollupsQuality,
					Fallback: sessionEnricher,
					Window:   sessionWindowFunc(in.Sessions),
					Logger:   logger,
				})
				if pErr != nil {
					return nil, wrapErr("sessions/protocol projection enricher", pErr)
				}
				sessionEnricher = projectionEnricher
			}
			sessionProjectorOpts = append(sessionProjectorOpts, sessionsprotocol.WithEnricher(sessionEnricher))
		}
		sessionsProjector, pErr := sessionsprotocol.NewListerProjector(in.Sessions, sessionProjectorOpts...)
		if pErr != nil {
			return nil, wrapErr("sessions/protocol projector", pErr)
		}
		sessionsOpts := []sessionsprotocol.Option{
			sessionsprotocol.WithBus(bus),
			sessionsprotocol.WithRedactor(red),
			sessionsprotocol.WithLogger(logger),
			sessionsprotocol.WithTitleSetter(in.Sessions),
		}
		if sessionLifecycleAvailable {
			eraser, eErr := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
				Registry:  in.Sessions,
				State:     in.State,
				Memory:    in.Memory,
				Artifacts: in.Artifacts,
				Skills:    in.Skills,
				Bus:       bus,
				Redactor:  red,
				Logger:    logger,
				// The HA-64 / HA-65 durable projection fences: the
				// cascade erases + permanently fences their rows BEFORE
				// any destructive step, so no late event can resurrect an
				// erased session's projection rows (no replay
				// resurrection; the fences stay durable).
				TurnsProjection:   in.TurnsStore,
				RollupsProjection: in.RollupsStore,
			})
			if eErr != nil {
				return nil, wrapErr("sessions/protocol eraser", eErr)
			}
			sessionsOpts = append(sessionsOpts, sessionsprotocol.WithEraser(eraser))
		}
		sessionsService, sErr := sessionsprotocol.NewService(sessionsProjector, sessionsOpts...)
		if sErr != nil {
			return nil, wrapErr("sessions/protocol service", sErr)
		}
		muxOpts = append(muxOpts, transports.WithSessionsService(sessionsService))
	}

	if in.Artifacts != nil {
		artDriverName := cfg.Artifacts.Driver
		if artDriverName == "" {
			artDriverName = "inmem"
		}
		artifactsSurface, asErr := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
			Store:        in.Artifacts,
			Redactor:     red,
			Bus:          bus,
			Clock:        time.Now,
			DriverName:   artDriverName,
			MaxBodyBytes: cfg.Protocol.ResolvedMaxRequestBytes(),
			// The read-back bound is the operator's, resolved here so a
			// configuration written before the keys existed gets the
			// documented defaults rather than a zero the surface would
			// have to reinterpret.
			FetchDefaultMaxBytes: cfg.Artifacts.ResolvedFetchDefaultMaxBytes(),
			FetchHardMaxBytes:    cfg.Artifacts.ResolvedFetchHardMaxBytes(),
		})
		if asErr != nil {
			return nil, wrapErr("protocol artifacts surface", asErr)
		}
		muxOpts = append(muxOpts, transports.WithArtifactsSurface(artifactsSurface))
	}

	if in.Sessions != nil && in.Tasks != nil && in.Artifacts != nil {
		searchDeps := search.Deps{
			Redactor:   red,
			AdminScope: server.SearchAdminScopeFromAuth,
			Audit:      bus.Publish,
		}
		searchSessions, seErr := searchsessions.New(in.Sessions, searchDeps)
		if seErr != nil {
			return nil, wrapErr("search sessions", seErr)
		}
		searchTasks, stErr := searchtasks.New(in.Sessions, in.Tasks, searchDeps)
		if stErr != nil {
			return nil, wrapErr("search tasks", stErr)
		}
		searchArtifacts, saErr := searchartifacts.New(in.Artifacts, searchDeps)
		if saErr != nil {
			return nil, wrapErr("search artifacts", saErr)
		}
		searchers := []search.Searcher{searchSessions, searchTasks, searchArtifacts}
		if replayer, ok := bus.(events.Replayer); ok {
			searchEvents, seErr2 := searchevents.New(replayer, searchDeps)
			if seErr2 != nil {
				return nil, wrapErr("search events", seErr2)
			}
			searchers = append(searchers, searchEvents)
		}
		searchRegistry, srErr := search.NewRegistry(searchers...)
		if srErr != nil {
			return nil, wrapErr("search registry", srErr)
		}
		searchSurface, ssErr := protocol.NewSearchSurface(searchRegistry, server.SearchAdminScopeFromAuth)
		if ssErr != nil {
			return nil, wrapErr("search surface", ssErr)
		}
		muxOpts = append(muxOpts, transports.WithSearch(searchSurface))
	}

	if in.RunsStore != nil {
		runsService, rsErr := runsprotocol.NewService(in.RunsStore,
			runsprotocol.WithBus(bus),
			runsprotocol.WithRedactor(red),
			runsprotocol.WithLogger(logger),
			runsprotocol.WithValidModels(in.ValidModels),
		)
		if rsErr != nil {
			return nil, wrapErr("runs/protocol service", rsErr)
		}
		muxOpts = append(muxOpts, transports.WithRunsService(runsService))
	}

	if in.AuthSurface != nil {
		muxOpts = append(muxOpts, transports.WithAuthSurface(in.AuthSurface))
	}

	if in.TenantOverridePolicy != nil {
		governanceService, gErr := governanceprotocol.NewService(in.TenantOverridePolicy,
			governanceprotocol.WithLogger(logger))
		if gErr != nil {
			return nil, wrapErr("governance/protocol service", gErr)
		}
		muxOpts = append(muxOpts, transports.WithGovernanceService(governanceService))
	}

	if in.SetPosturePolicy != nil {
		postureWriteService, pErr := governanceprotocol.NewPostureWriteService(in.SetPosturePolicy,
			governanceprotocol.WithPostureWriteLogger(logger))
		if pErr != nil {
			return nil, wrapErr("governance/protocol set-posture service", pErr)
		}
		muxOpts = append(muxOpts, transports.WithGovernancePostureWrite(postureWriteService))
	}

	if in.KeyRotator != nil {
		keyRotateService, kErr := governanceprotocol.NewKeyRotateService(in.KeyRotator,
			governanceprotocol.WithKeyRotateBus(bus),
			governanceprotocol.WithKeyRotateRedactor(red),
			governanceprotocol.WithKeyRotateLogger(logger))
		if kErr != nil {
			return nil, wrapErr("governance/protocol key-rotate service", kErr)
		}
		muxOpts = append(muxOpts, transports.WithGovernanceKeyRotate(keyRotateService))
	}

	if in.AgentConfig != nil {
		agentConfigOpts := []agentcfgprotocol.Option{
			agentcfgprotocol.WithLogger(logger),
			agentcfgprotocol.WithSkillStore(in.Skills),
			agentcfgprotocol.WithBus(bus),
			agentcfgprotocol.WithCoordinator(in.Coordinator),
			agentcfgprotocol.WithStdioAllowlist(append([]string(nil), in.MCPStdioAllowlist...)),
			agentcfgprotocol.WithSessionOverlay(in.SessionOverlay),
			agentcfgprotocol.WithSessionPersonalSkillController(in.SessionPersonalSkillController),
			agentcfgprotocol.WithBootLifecycleEnsurer(in.AgentConfigID, in.BootLifecycleEnsurer),
			agentcfgprotocol.WithRunSnapshotGate(in.RunSnapshots),
			agentcfgprotocol.WithValidModels(in.ValidModels),
			agentcfgprotocol.WithBootDeclaredMCPServers(append([]string(nil), in.BootDeclaredMCP...)),
			agentcfgprotocol.WithBootDeclaredOAuthProviders(append([]string(nil), in.BootDeclaredOAuth...)),
			agentcfgprotocol.WithAllowWireOAuthDescriptor(in.AllowWireOAuthDescriptor),
			agentcfgprotocol.WithAllowWireInjection(in.AllowWireInjection),
			agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(in.SignedOAuthMCPCapabilityAuthorities),
			agentcfgprotocol.WithSignedOAuthMCPOperationState(in.State),
			agentcfgprotocol.WithSignedOAuthMCPUserReconciler(in.SignedOAuthMCPUserReconciler),
			agentcfgprotocol.WithAgentPackProposalState(in.State),
			agentcfgprotocol.WithAgentPackCatalog(in.Catalog),
			agentcfgprotocol.WithAgentPackGrantedScopes(append([]string(nil), cfg.Tools.GrantedScopes...)),
		}
		if in.AgentPackLLM != nil {
			proposer, proposerErr := packproposer.New(in.AgentPackLLM)
			if proposerErr != nil {
				return nil, wrapErr("agent-config/pack proposer", proposerErr)
			}
			agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithAgentPackProposer(proposer))
		}
		if in.MCPAttacher != nil {
			if preparer, ok := in.MCPAttacher.(agentcfgprotocol.ConnectionPreparer); ok {
				agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithConnectionPreparer(preparer))
			} else {
				agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithConnectionAttacher(in.MCPAttacher))
			}
			// The production attacher concrete also applies the OAuth-discovery
			// allow-list live (the set_mcp_discovery_origins write path); wire it as
			// the applier when the attacher satisfies the seam.
			if applier, ok := in.MCPAttacher.(agentcfgprotocol.DiscoveryOriginApplier); ok {
				agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithDiscoveryOriginApplier(applier))
			}
			// The same concrete also tears a just-attached server back down when
			// the add's revision write fails after it (the expected-revision
			// conflict). Binding the compensation to the object that attached
			// guarantees it detaches through the same registry + catalog.
			if detacher, ok := in.MCPAttacher.(agentcfgprotocol.ConnectionDetacher); ok {
				agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithConnectionDetacher(detacher))
			}
		}
		if in.OAuthProviderInstaller != nil {
			agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithProviderInstaller(in.OAuthProviderInstaller))
		}
		if in.LLMProviderInstaller != nil {
			agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithLLMProviderInstaller(in.LLMProviderInstaller))
		}
		agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithInferenceBrokers(append([]string(nil), in.InferenceBrokers...)))
		agentConfigService, acErr := agentcfgprotocol.NewService(in.AgentConfig, agentConfigOpts...)
		if acErr != nil {
			return nil, wrapErr("agent-config/protocol service", acErr)
		}
		muxOpts = append(muxOpts, transports.WithAgentConfigService(agentConfigService))
		if in.UserSkillImportService != nil {
			muxOpts = append(muxOpts, transports.WithUserSkillImportService(in.UserSkillImportService))
		}
		if in.CompositionPreviewService != nil {
			muxOpts = append(muxOpts, transports.WithCompositionPreviewService(in.CompositionPreviewService))
		}
	}
	if in.AgentResolver != nil {
		muxOpts = append(muxOpts, transports.WithAgentResolver(in.AgentResolver))
	}

	// The HA-64 conversation-turn read service: served ENTIRELY from the
	// durable projection (never a raw history / task fallback), with the
	// canonical signed agent-reach gate wired (an unwired gate would fail
	// closed on named-agent turns).
	if in.TurnsProjector != nil {
		turnsService, tErr := turnsprotocol.NewService(in.TurnsProjector,
			turnsprotocol.WithAgentReachAuthorizer(in.AgentReach),
			turnsprotocol.WithSessionReachAuthorizer(auth.NewSessionReachAuthorizer()),
			turnsprotocol.WithBus(bus),
			turnsprotocol.WithRedactor(red),
			turnsprotocol.WithLogger(logger),
		)
		if tErr != nil {
			return nil, wrapErr("sessions/turns/protocol service", tErr)
		}
		muxOpts = append(muxOpts, transports.WithSessionTurnsService(turnsService))
	}

	// The HA-65 observability rollup service: the closed administrative
	// query over the durable rollup rows. The Querier and the freshness
	// QualitySource point at the SAME underlying store (the wiring's
	// invariant), and the production scope checker admits the admin /
	// console:fleet widening (the closed two-scope set).
	if in.RollupsStore != nil && in.RollupsQuality != nil {
		obsService, oErr := observabilityprotocol.NewService(
			in.RollupsStore,
			in.RollupsQuality,
			server.SearchAdminScopeFromAuth,
			bus.Publish,
			red,
			observabilityprotocol.WithLogger(logger),
		)
		if oErr != nil {
			return nil, wrapErr("observability/protocol service", oErr)
		}
		muxOpts = append(muxOpts, transports.WithObservabilityService(obsService))
	}

	mux, muxErr := transports.NewMux(in.Surface, bus, muxOpts...)
	if muxErr != nil {
		return nil, wrapErr("transports", muxErr)
	}
	// HA-66: the boot-ownership reader is bound into the request context
	// at the mux boundary (the integration-owner wiring the
	// agentcfg/protocol boot-pack guards document — bootpack_guards.go:
	// "The integration owner wires the concrete reader at the handler
	// boundary"). The transports mux has no boot-ownership option, so the
	// serve band composes it HERE: every `/v1/agent_config/*` request
	// carries the immutable boot index, and the mutation guards
	// (upsert / remove / proposal-commit / rollback / activation) fire on
	// every write path. A nil owner keeps the guards inert (no boot
	// baseline bound). The wrapper is a frozen compiled artifact — safe
	// for N concurrent requests.
	var mounted http.Handler = mux
	if in.BootOwnership != nil {
		mounted = bootOwnershipMux(in.BootOwnership, mounted)
	}
	return &BuiltMux{Mux: mounted, FlowRegistry: flowRegistry}, nil
}

// bootOwnershipMux wraps the Protocol mux so every `/v1/agent_config/*`
// request context carries the HA-66 immutable boot-pack ownership
// reader. The reader is the frozen eager boot index (safe for
// concurrent reuse); a request outside the agent_config prefix is
// passed through untouched.
func bootOwnershipMux(owner agentcfgprotocol.BootOwnership, next http.Handler) http.Handler {
	if owner == nil || next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/agent_config") {
			r = r.WithContext(agentcfgprotocol.WithBootOwnership(r.Context(), owner))
		}
		next.ServeHTTP(w, r)
	})
}

// wrapErr is the local error-context wrapper. Kept package-local so the two
// call sites (BuildMux + Boot) format their surface-construction failures
// identically.
func wrapErr(ctx string, err error) error {
	return fmt.Errorf("%s: %w", ctx, err)
}
