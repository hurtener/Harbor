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
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
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
	"github.com/hurtener/Harbor/internal/skills"
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
	LLMSnapshot llm.ConfigSnapshot

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
	State          state.StateStore
	Skills         skills.SkillStore

	// Control-plane handles.
	AgentConfig    agentcfg.Registry
	AgentConfigID  string
	SessionOverlay sessionoverlay.Store
	RunsStore      *runsprotocol.Store
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
	// BootDeclaredOAuth is the set of boot-declared OAuth provider names
	// (tools.oauth_providers[].name); an install/uninstall of one of these is
	// refused (boot wins).
	BootDeclaredOAuth []string

	// Auth. Validator nil mounts the transports WithoutValidator (the
	// explicit test-kit opt-out); AuthSurface nil leaves auth.rotate_token
	// un-mounted (the production posture — no in-runtime token issuer).
	Validator   auth.Validator
	AuthSurface *auth.RotateSurface

	// Posture stamps.
	DisplayName  string
	InstanceID   string
	BuildVersion string
	BuildCommit  string
	// TopologyAvailable advertises the topology.snapshot capability (true
	// only when the caller wired a topology accessor onto the surface).
	TopologyAvailable bool
}

// BuiltMux is the result of BuildMux: the mounted Protocol mux plus the flow
// registry the flows surface serves (the caller seeds the same registry).
type BuiltMux struct {
	// Mux is the transports mux the caller mounts under /v1/.
	Mux *http.ServeMux
	// FlowRegistry is the empty registry the flows surface serves. Nil when
	// the flows surface was not mounted (no artifact store / task registry).
	FlowRegistry *flow.Registry
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

	postureSurface, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Build: types.RuntimeInfo{
			BuildVersion:       in.BuildVersion,
			BuildCommit:        in.BuildCommit,
			BuildGoVersion:     goruntime.Version(),
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
		Metrics:                   runtimeposture.MetricsProvider(in.Metrics, logger),
		Governance:                governance.NewPostureProviderWithState(governance.ConfigFromOperator(cfg.Governance), in.State),
		LLM:                       llm.NewPostureProvider(in.LLMSnapshot),
		Redactor:                  red,
		Bus:                       bus,
		DisplayName:               in.DisplayName,
		InstanceID:                in.InstanceID,
		TopologyAvailable:         in.TopologyAvailable,
		AgentConfigAvailable:      in.AgentConfig != nil,
		StateSnapshotsAvailable:   stateSnapshotsAvailable,
		SessionLifecycleAvailable: sessionLifecycleAvailable,
		ToolAnnotationsAvailable:  toolAnnotationsAvailable,
	})
	if err != nil {
		return nil, wrapErr("posture surface", err)
	}
	muxOpts = append(muxOpts, transports.WithPostureSurface(postureSurface))

	// MCP servers + Apps host surfaces (the catalog band).
	if in.MCPRegistry != nil {
		// Wire the on-demand OAuth-requirement discovery walker into
		// the probe path: a probe against a server that answered a
		// `WWW-Authenticate` OAuth step-up triggers the RFC 9728 → RFC 8414
		// chain walk, whose verbatim result the operator reads via
		// mcp.servers.get. Harbor never runs the flow or holds a token.
		mcpRegAccessor, aErr := mcpconsole.NewRegistryAccessor(
			in.MCPRegistry,
			mcpconsole.WithOAuthDiscoverer(toolauth.NewDiscoverer()),
		)
		if aErr != nil {
			return nil, wrapErr("mcp accessor", aErr)
		}
		mcpSurface, sErr := protocol.NewMCPSurface(protocol.MCPDeps{
			MCP:      mcpRegAccessor,
			OAuth:    mcpconsole.NewNoOAuthAccessor(),
			Redactor: red,
			Bus:      bus,
		})
		if sErr != nil {
			return nil, wrapErr("mcp surface", sErr)
		}
		muxOpts = append(muxOpts, transports.WithMCPSurface(mcpSurface))

		appsAccessor, aaErr := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
			Registry:       in.MCPRegistry,
			Catalog:        in.Catalog,
			Store:          in.Artifacts,
			Bus:            bus,
			ToolContext:    in.MCPToolContext,
			AgentConfig:    in.AgentConfig,
			AgentID:        in.AgentConfigID,
			SessionOverlay: in.SessionOverlay,
			Threshold:      cfg.Artifacts.HeavyOutputThresholdBytes,
		})
		if aaErr != nil {
			return nil, wrapErr("mcp apps accessor", aaErr)
		}
		appsSurface, asErr := protocol.NewAppsSurface(protocol.AppsDeps{
			Resource:    appsAccessor,
			Invoker:     appsAccessor,
			ToolContext: appsAccessor,
		})
		if asErr != nil {
			return nil, wrapErr("mcp apps surface", asErr)
		}
		muxOpts = append(muxOpts, transports.WithAppsSurface(appsSurface))
	}

	if in.Coordinator != nil && in.Artifacts != nil {
		muxOpts = append(muxOpts, transports.WithPauseList(
			in.Coordinator, in.Artifacts, cfg.Artifacts.HeavyOutputThresholdBytes))
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
				toolsprotocol.WithLoadingResolver(projection.LoadingResolverAdapter{Registry: in.AgentConfig}))
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
				Catalog:             in.Catalog,
				Approval:            policyStore,
				Events:              bus,
				OAuth:               annotate.NewProviderOAuthReader(in.OAuthProviders),
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
		flowCatalog, fcErr := flowprotocol.NewRegistryCatalog(flowRegistry, in.Artifacts, cfg.Artifacts.HeavyOutputThresholdBytes)
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
			sessionEnricher, eErr := sessionsprotocol.NewCounterEnricher(sessionsprotocol.CounterEnricherDeps{
				Bus:    bus,
				Tasks:  in.Tasks,
				Pauses: in.Coordinator,
				Logger: logger,
			})
			if eErr != nil {
				return nil, wrapErr("sessions/protocol counter enricher", eErr)
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
				Bus:       bus,
				Redactor:  red,
				Logger:    logger,
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
		})
		if asErr != nil {
			return nil, wrapErr("protocol artifacts surface", asErr)
		}
		muxOpts = append(muxOpts, transports.WithArtifactsSurface(artifactsSurface))
	}

	if in.Sessions != nil && in.Tasks != nil && in.Artifacts != nil {
		searchDeps := search.Deps{Redactor: red, AdminScope: server.SearchAdminScopeFromAuth}
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
			agentcfgprotocol.WithValidModels(in.ValidModels),
			agentcfgprotocol.WithBootDeclaredMCPServers(append([]string(nil), in.BootDeclaredMCP...)),
			agentcfgprotocol.WithBootDeclaredOAuthProviders(append([]string(nil), in.BootDeclaredOAuth...)),
		}
		if in.MCPAttacher != nil {
			agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithConnectionAttacher(in.MCPAttacher))
			// The production attacher concrete also applies the OAuth-discovery
			// allow-list live (the set_mcp_discovery_origins write path); wire it as
			// the applier when the attacher satisfies the seam.
			if applier, ok := in.MCPAttacher.(agentcfgprotocol.DiscoveryOriginApplier); ok {
				agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithDiscoveryOriginApplier(applier))
			}
		}
		if in.OAuthProviderInstaller != nil {
			agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithProviderInstaller(in.OAuthProviderInstaller))
		}
		agentConfigService, acErr := agentcfgprotocol.NewService(in.AgentConfig, agentConfigOpts...)
		if acErr != nil {
			return nil, wrapErr("agent-config/protocol service", acErr)
		}
		muxOpts = append(muxOpts, transports.WithAgentConfigService(agentConfigService))
	}

	mux, muxErr := transports.NewMux(in.Surface, bus, muxOpts...)
	if muxErr != nil {
		return nil, wrapErr("transports", muxErr)
	}
	return &BuiltMux{Mux: mux, FlowRegistry: flowRegistry}, nil
}

// wrapErr is the local error-context wrapper. Kept package-local so the two
// call sites (BuildMux + Boot) format their surface-construction failures
// identically.
func wrapErr(ctx string, err error) error {
	return fmt.Errorf("%s: %w", ctx, err)
}
