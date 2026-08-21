// Package transports is the Harbor Protocol wire-transport seam — the
// binding of RFC §5.4's resolved transport choice (SSE for the
// event stream + REST/JSON for the control surface) onto net/http.
//
// # The seam (CLAUDE.md §3, §4.4)
//
// Each wire transport is its own sub-package:
//
//   - internal/protocol/transports/control — REST/JSON over the
//     transport-agnostic protocol.ControlSurface.
//   - internal/protocol/transports/stream — SSE over the events.EventBus.
//
// This package composes them: NewMux wires both handlers into one
// *http.ServeMux a future server (the `harbor dev` subcommand)
// mounts. The layout is the §4.4-style seam read for transports
// rather than drivers: RFC §5.4 explicitly leaves WebSocket as an
// additive alternate transport. Adding it is a third sub-package
// (internal/protocol/transports/websocket) plus one more mux entry here
// — neither `control` nor `stream` is reshaped, and no caller outside
// this package changes. There is NO driver-registry / factory ceremony:
// the transport set is small, closed, and mounted in code at boot, not
// resolved by name from config — the same posture An earlier phase took for the
// ControlSurface.
//
// # Why no http.Server here
//
// Harbor ships the transport HANDLERS, not the server that listens.
// `harbor dev` owns the net.Listener, the graceful-shutdown
// lifecycle, and the /healthz + /readyz endpoints; it calls NewMux and
// serves the result. Keeping the listen/shutdown lifecycle out of this
// package means the transports are exercised end-to-end today via
// httptest (the package + integration tests) without waiting on the
// server phase — the same decoupling used to stay testable
// ahead of its wire binding.
//
// # Concurrent reuse
//
// The *http.ServeMux NewMux returns is immutable after construction and
// both mounted handlers are themselves safe for concurrent reuse compiled artifacts
// (control.Handler, stream.Handler). One mux serves N concurrent
// requests safely; concurrent_test.go pins it under -race.
package transports

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/memory"
	observabilityprotocol "github.com/hurtener/Harbor/internal/observability/protocol"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	governanceprotocol "github.com/hurtener/Harbor/internal/runtime/governance/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	turnsprotocol "github.com/hurtener/Harbor/internal/sessions/turns/protocol"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// muxConfig holds the optional knobs NewMux threads into the two
// transport handlers. Set once at construction; never mutated after.
//
// withoutAuth is the explicit, test-only escape hatch — see
// WithoutValidator. A nil validator + withoutAuth=false fails closed
// at NewMux per CLAUDE.md §13 ("Test stubs as production defaults on
// operator-facing seams"; PR #91 amendment).
type muxConfig struct {
	logger          *slog.Logger
	keepalive       time.Duration
	validator       auth.Validator
	reach           auth.AgentReachAuthorizer
	agentResolver   protocol.AgentResolver
	withoutAuth     bool
	aggregatorClock events.AggregatorClock
	// redactor is the audit.Redactor wired into the control transport
	// for the admin-impersonation audit emit. Optional in the
	// mux config so existing call-sites compile unchanged, but
	// production wiring (the `harbor dev` boot path) SHOULD supply it
	// so impersonation works end-to-end. When unsupplied, the control
	// transport refuses impersonation requests fail-closed with
	// CodeRuntimeError (CLAUDE.md §13 "Silent degradation").
	redactor audit.Redactor
	// searchSurface is the search dispatcher wired
	// into the control transport so the five `search.*` methods route
	// through it instead of the task-control ControlSurface. Optional —
	// when unsupplied, the control transport rejects search calls with
	// CodeUnknownMethod (the 404 → SKIP path the smoke script relies
	// on). Production wiring (`harbor dev` / `harbor console`) supplies
	// it so the Console search surface works out of the box.
	searchSurface control.SearchSurface
	// postureSurface is the posture
	// dispatcher wired into the control transport so the seven posture
	// methods — the five `runtime.*` / `metrics.*` reads plus the two
	// `governance.posture` / `llm.posture` reads — route through it.
	// Optional — when unsupplied, the control transport rejects posture
	// calls with CodeUnknownMethod (the 404 → SKIP path the smoke
	// script relies on).
	postureSurface control.PostureSurface
	// artifactsSurface is the artifacts dispatcher
	// wired into the control transport so the three `artifacts.*`
	// methods route through it. Optional — when unsupplied, the control
	// transport rejects artifacts calls with CodeUnknownMethod (the 404
	// → SKIP path the smoke script relies on).
	artifactsSurface control.ArtifactsSurface
	// mcpSurface is the MCP-Connections dispatcher
	// wired into the control transport so the twelve `mcp.servers.*`
	// methods route through it. Optional — when unsupplied, the control
	// transport rejects MCP calls with CodeUnknownMethod (the 404 → SKIP
	// path the smoke script relies on).
	mcpSurface control.MCPSurface
	// appsSurface is the MCP Apps dispatcher wired into the control
	// transport so the two MCP Apps methods (`mcp.servers.read_resource`
	// / `mcp.apps.call_tool`) route through it. Optional — when
	// unsupplied, the control transport rejects MCP Apps calls with
	// CodeUnknownMethod (the 404 → SKIP path the smoke script relies on).
	appsSurface control.AppsSurface
	// skillPublicationsSurface is the HA-68 publication dispatcher. Optional:
	// an unassembled same-runtime publication store leaves these methods
	// unmounted rather than advertising an inert surface.
	skillPublicationsSurface control.SkillPublicationsSurface
	// pauseCoordinator + artifactStore + heavyThreshold feed the
	// `pause.list` snapshot handler. All three are OPTIONAL in the
	// mux config so existing call-sites compile unchanged — when the
	// coordinator or store is unsupplied the `pause.list` route is NOT
	// mounted, so the smoke script's `skip_if_404` keeps preflight
	// green on a partial build. Production wiring (`harbor dev`) SHOULD
	// supply all three so the Console intervention queue works.
	pauseCoordinator pauseresume.Coordinator
	artifactStore    artifacts.ArtifactStore
	heavyThreshold   int
	// memoryStore + memoryDriverName feed the three
	// `memory.*` read routes. memoryStore is OPTIONAL in the mux config
	// so existing call-sites compile unchanged — when it is unsupplied
	// the three `memory.*` routes are NOT mounted, so the smoke
	// script's `skip_if_404` keeps preflight green on a partial build.
	// The memory handler reuses artifactStore + heavyThreshold (the
	// `pause.list` deps) for the heavy-value bypass; production
	// wiring (`harbor dev`) supplies all of them so the Console Memory
	// page works.
	memoryStore      memory.MemoryStore
	memoryDriverName string
	// toolsService feeds the `tools.*` handler (the Console
	// Tools page surface). OPTIONAL — when unsupplied the
	// `POST /v1/tools/{method}` route is NOT mounted, so the smoke
	// script's `skip_if_404` keeps preflight green on a partial build.
	// Production wiring (`harbor dev`) SHOULD supply it so the Console
	// Tools page has a live surface.
	toolsService *toolsprotocol.Service
	// flowsSurface feeds the Console Flows-page
	// handler — the six `POST /v1/flows/*` routes. OPTIONAL: when
	// unsupplied the Flows routes are NOT mounted, so the smoke
	// script's `skip_if_404` keeps preflight green on a partial build.
	// Production wiring (`harbor dev`) SHOULD supply it so the Console
	// Flows page works.
	flowsSurface *flowprotocol.Surface
	// tasksService feeds the `tasks.*` handler — the
	// two Console Tasks-page read methods (`tasks.list` / `tasks.get`).
	// OPTIONAL — when unsupplied the `POST /v1/tasks/{method}` route is
	// NOT mounted, so the smoke script's `skip_if_404` keeps preflight
	// green on a partial build. Production wiring (`harbor dev`) SHOULD
	// supply it so the Console Tasks page has a live surface.
	tasksService *tasksprotocol.Service
	// agentsService feeds the Console Agents-page
	// handler — the eight `POST /v1/agents/{method}` read routes.
	// OPTIONAL: when unsupplied the `POST /v1/agents/{method}` route is
	// NOT mounted, so the smoke script's `skip_if_404` keeps preflight
	// green on a partial build. Production wiring (`harbor dev`) SHOULD
	// supply it so the Console Agents page works.
	agentsService *agentsprotocol.Service
	// sessionsService feeds the Console Sessions-page
	// handler — the two `POST /v1/sessions/*` routes (`sessions.list` /
	// `sessions.inspect`). OPTIONAL: when unsupplied the Sessions routes
	// are NOT mounted, so the smoke script's `skip_if_404` keeps
	// preflight green on a partial build. Production wiring
	// (`harbor dev`) SHOULD supply it so the Console Sessions page has
	// a live surface.
	sessionsService *sessionsprotocol.Service
	// runsService feeds the Console Playground-page
	// handler — the `POST /v1/runs/set_overrides` route. OPTIONAL: when
	// unsupplied the Runs route is NOT mounted, so the smoke script's
	// `skip_if_404` keeps preflight green on a partial build. Production
	// wiring (`harbor dev`) SHOULD supply it so the Console Playground
	// page can record next-message overrides.
	runsService *runsprotocol.Service
	// rotateSurface feeds the `auth.*` handler — the
	// single net-new Console Settings-page method (`auth.rotate_token`).
	// OPTIONAL: when unsupplied the `POST /v1/auth/{method}` route is
	// NOT mounted, so the smoke script's `skip_if_404` keeps preflight
	// green on a partial build. Production wiring (`harbor dev` /
	// `harbor console`) SHOULD supply it so the Console Settings page
	// "Rotate token" action has a live surface.
	rotateSurface *auth.RotateSurface
	// governanceService feeds the admin-scoped governance tenant-override
	// handler — the `POST /v1/governance/{set,get}_tenant_overrides`
	// routes. OPTIONAL: when unsupplied the `/v1/governance/*` route is
	// NOT mounted, so the smoke script's `skip_if_404` keeps preflight
	// green on a partial build. Production wiring (`harbor dev` /
	// `harbor console`) SHOULD supply it so an admin can change a tenant's
	// default LLM parameters live (no redeploy).
	governanceService *governanceprotocol.Service
	// governanceKeyRotate feeds the `POST /v1/governance/rotate_key`
	// route. OPTIONAL: when unsupplied the route 501s (the partial-build
	// convention). Built only alongside governanceService. Production
	// wiring supplies it so an admin can rotate the LLM provider key live.
	governanceKeyRotate *governanceprotocol.KeyRotateService
	// governancePostureWrite feeds the `POST /v1/governance/set_posture`
	// route (the admin identity-tier policy WRITE surface). OPTIONAL: when
	// unsupplied the route 501s (the partial-build convention). Built only
	// alongside governanceService. Production wiring supplies it so an admin
	// can replace the identity-tier policy table live (no redeploy).
	governancePostureWrite *governanceprotocol.PostureWriteService
	// stateHistoryBus + stateHistoryArtifacts feed the `state.history`
	// windowed event-replay handler — the `POST /v1/state/history` route.
	// Both are OPTIONAL in the mux config so existing call-sites compile
	// unchanged — when either is unsupplied the `state.history` route is
	// NOT mounted, so the smoke script's `skip_if_404` keeps preflight
	// green on a partial build. Production wiring (`harbor dev` /
	// `harbortest/devstack`) supplies both so the Console session-reopen
	// hydration has a live windowed-read surface.
	stateHistoryBus       events.EventBus
	stateHistoryArtifacts artifacts.ArtifactStore
	// eventsListArtifacts feeds the `events.list` durable, time-ranged,
	// cross-session raw-event read handler — the `POST /v1/events/list`
	// route. The bus is the mandatory NewMux bus (always present); the
	// ArtifactStore is OPTIONAL — when unsupplied the `events.list` route is
	// NOT mounted, so the smoke script's `skip_if_404` keeps preflight green
	// on a partial build. Production wiring (`harbor dev` /
	// `harbortest/devstack`) supplies it so the Console Events page has a
	// live historical-window read.
	eventsListArtifacts artifacts.ArtifactStore
	// agentConfigService feeds the admin-scoped agent-config control-plane
	// handler — the `POST /v1/agent_config/*` routes. OPTIONAL: when
	// unsupplied the `/v1/agent_config/*` route is NOT mounted, so the
	// smoke script's `skip_if_404` keeps preflight green on a partial
	// build. Production wiring (`harbor dev` / `harbor console`) supplies it
	// so an admin can version an agent's config (skills now) live.
	agentConfigService *agentcfgprotocol.Service
	// userSkillImportService feeds the two-phase verified-caller
	// skill-package import routes (`agent_config.user.skills.import_*`).
	// OPTIONAL: when unsupplied the routes answer 501 (the partial-build
	// convention) — the `/v1/agent_config/*` route must be mounted first.
	userSkillImportService *agentcfgprotocol.UserSkillImportService
	// compositionPreviewService feeds the read-only
	// `agent_config.composition.preview` route. OPTIONAL: when unsupplied
	// the route answers 501 (the partial-build convention) — the
	// `/v1/agent_config/*` route must be mounted first.
	compositionPreviewService *agentcfgprotocol.CompositionPreviewService
	// sessionTurnsService feeds the session-turns read handler — the two
	// `POST /v1/sessions/turns/*` routes (`sessions.turns.list` /
	// `sessions.turns.get`). OPTIONAL: when unsupplied the routes are NOT
	// mounted, so the smoke script's `skip_if_404` keeps preflight green
	// on a partial build.
	sessionTurnsService *turnsprotocol.Service
	// observabilityService feeds the observability handler — the
	// `POST /v1/observability/query` route. OPTIONAL: when unsupplied the
	// route is NOT mounted, so the smoke script's `skip_if_404` keeps
	// preflight green on a partial build.
	observabilityService *observabilityprotocol.Service
}

// Option configures NewMux.
type Option func(*muxConfig)

// WithLogger sets the slog.Logger both transport handlers log to. A nil
// logger is ignored; the handlers fall back to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *muxConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithKeepalive overrides the SSE keepalive-comment interval on the
// stream transport. A non-positive value is ignored.
func WithKeepalive(d time.Duration) Option {
	return func(c *muxConfig) {
		if d > 0 {
			c.keepalive = d
		}
	}
}

// requestMiddleware returns the identity decorator every mounted route
// is wrapped in. A wired validator gives the bearer decorator; the
// explicit bearer-less opt-in gives the carrier decorator, which
// establishes the triple from the X-Harbor-* headers and refuses a
// request that supplies no complete one. NewMux fails construction when
// neither is chosen, so there is no posture in which a handler runs
// without an established identity on ctx.
func (c *muxConfig) requestMiddleware() func(http.Handler) http.Handler {
	if c.validator != nil {
		return auth.Middleware(c.validator, auth.MWLogger(c.logger))
	}
	return auth.CarrierIdentityMiddleware(c.logger)
}

// WithValidator wires the JWT auth.Validator into NewMux.
// BOTH transport handlers (REST control + SSE stream) are wrapped in
// auth.Middleware: every request must carry a verified
// `Authorization: Bearer <jwt>`; the middleware injects the verified
// identity + scopes into the request context.Context before the
// underlying handler runs.
//
// A validator is **mandatory** — `NewMux` returns `ErrMisconfigured`
// when neither `WithValidator` nor `WithoutValidator` is supplied
// (PR #91 amendment; CLAUDE.md §13 "Test stubs as
// production defaults on operator-facing seams"). A nil validator is
// treated as "WithValidator not supplied"; tests that legitimately
// need the unauthenticated path use `WithoutValidator()` explicitly.
func WithValidator(v auth.Validator) Option {
	return func(c *muxConfig) {
		if v != nil {
			c.validator = v
		}
	}
}

// WithAggregateClock injects a deterministic clock into the
// events.aggregate handler's underlying *events.Aggregator. Production
// callers do not use this; the default real clock (UTC) is correct.
// Tests that exercise the aggregate path with backdated events use
// this to anchor the aggregator's "now" deterministically — the same
// posture WithKeepalive takes for the SSE keepalive interval.
func WithAggregateClock(c events.AggregatorClock) Option {
	return func(cfg *muxConfig) {
		if c != nil {
			cfg.aggregatorClock = c
		}
	}
}

// WithRedactor wires the audit.Redactor into the control transport so
// the admin-impersonation gate can publish a redacted
// `audit.admin_scope_used` event onto the bus on every accepted
// impersonation. The bus is already mandatory at NewMux (it feeds the
// SSE event transport); the redactor is the second half of the pair the
// control transport needs to enable impersonation.
//
// The option is OPTIONAL at the type level so existing call-sites
// compile unchanged. When the redactor is not supplied, the control
// transport refuses impersonation requests fail-closed with
// CodeRuntimeError (CLAUDE.md §13 "Silent degradation"). Production
// wiring (the `harbor dev` boot path) SHOULD supply it.
//
// A nil redactor is treated as "WithRedactor not supplied".
func WithRedactor(r audit.Redactor) Option {
	return func(c *muxConfig) {
		if r != nil {
			c.redactor = r
		}
	}
}

// WithPostureSurface wires the posture
// dispatcher into the control transport. When supplied, the control
// handler routes the seven posture methods — the five `runtime.*` /
// `metrics.*` reads plus `governance.posture` / `llm.posture` — to the
// posture surface instead of falling through to the task-control
// ControlSurface.
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied, the control transport rejects posture calls with
// CodeUnknownMethod (HTTP 404) — the 404 → SKIP path the smoke script
// relies on. Production wiring (`harbor dev`) supplies it so the
// Console Settings page has a live surface. A nil surface
// is treated as "WithPostureSurface not supplied".
func WithPostureSurface(s control.PostureSurface) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.postureSurface = s
		}
	}
}

// WithSearch wires the search dispatcher into the
// control transport. When supplied, the handler routes the five
// `search.*` methods to s.Dispatch instead of falling through to the
// task-control ControlSurface.
//
// The surface is OPTIONAL so existing call-sites compile unchanged.
// When unsupplied (or supplied a nil surface), the control transport
// rejects search calls with CodeUnknownMethod — the smoke script's
// `skip_if_404` keeps preflight green on a partial build. Production
// wiring (`harbor dev` / `harbor console`) supplies it so the five
// `search.*` methods serve live results. A nil surface is treated as
// "WithSearch not supplied".
func WithSearch(s control.SearchSurface) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.searchSurface = s
		}
	}
}

// WithMCPSurface wires the MCP-Connections dispatcher
// into the control transport. When supplied, the control handler routes
// the twelve `mcp.servers.*` methods to the MCP surface instead of
// falling through to the task-control ControlSurface.
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied, the control transport rejects MCP calls with
// CodeUnknownMethod (HTTP 404) — the 404 → SKIP path the smoke script
// relies on. Production wiring (`harbor dev`) supplies it so the Console
// MCP Connections page has a live surface. A nil surface is
// treated as "WithMCPSurface not supplied".
func WithMCPSurface(s control.MCPSurface) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.mcpSurface = s
		}
	}
}

// WithAppsSurface wires the MCP Apps dispatcher into the control
// transport. When supplied, the control handler routes the two MCP Apps
// methods (`mcp.servers.read_resource` / `mcp.apps.call_tool`) to the
// Apps surface instead of falling through to the task-control
// ControlSurface.
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied, the control transport rejects MCP Apps calls with
// CodeUnknownMethod (HTTP 404) — the 404 → SKIP path the smoke script
// relies on. Production wiring (`harbor dev`) supplies it so the Console
// can render MCP Apps. A nil surface is treated as "WithAppsSurface not
// supplied".
func WithAppsSurface(s control.AppsSurface) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.appsSurface = s
		}
	}
}

// WithSkillPublicationsSurface wires the HA-68 publication dispatcher into
// the control transport. The option is optional so a Runtime that has not
// assembled its same-runtime publication store does not mount an inert
// surface.
func WithSkillPublicationsSurface(s control.SkillPublicationsSurface) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.skillPublicationsSurface = s
		}
	}
}

// WithPauseList wires the `pause.list` snapshot handler into
// NewMux. coord is the unified pause/resume Coordinator the
// snapshot projects from; store is the ArtifactStore the
// heavy-content bypass routes oversized pause payloads through;
// heavyThreshold is the inline-payload bound in bytes. The runtime
// wiring supplies the PINNED Console inline-payload bound
// (config.DefaultConsoleInlinePayloadBytes), NOT the operator's
// `artifacts.heavy_output_threshold_bytes`: `pause.list` is a
// browser-facing read whose inline-versus-reference selection is
// Protocol-visible, so raising the prompt-size threshold must not
// reshape it.
//
// All three are required together — supplying the option with a nil
// coord, a nil store, or a non-positive threshold leaves the
// `pause.list` route UN-mounted (the route's smoke `skip_if_404` keeps
// preflight green on a partial build). When supplied correctly the
// route `POST /v1/pause/list` is mounted and, when WithValidator is
// also set, wrapped in auth.Middleware like every other transport.
//
// pause.list is READ-ONLY against the Coordinator (CLAUDE.md §7 rule 4
// / §13) — it reads the shipped pause-coordinator state, it does not
// reinvent pause coordination.
func WithPauseList(coord pauseresume.Coordinator, store artifacts.ArtifactStore, heavyThreshold int) Option {
	return func(c *muxConfig) {
		c.pauseCoordinator = coord
		c.artifactStore = store
		c.heavyThreshold = heavyThreshold
	}
}

// WithArtifactsSurface wires the artifacts dispatcher
// into the control transport. When supplied, the control handler routes
// the three artifacts methods — `artifacts.list`, `artifacts.put`,
// `artifacts.get_ref` — to the artifacts surface instead of falling
// through to the task-control ControlSurface.
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied, the control transport rejects artifacts calls with
// CodeUnknownMethod (HTTP 404) — the 404 → SKIP path the smoke script
// relies on. Production wiring (`harbor dev`) supplies it so the Console
// Artifacts page has a live surface. A nil surface is
// treated as "WithArtifactsSurface not supplied".
func WithArtifactsSurface(s control.ArtifactsSurface) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.artifactsSurface = s
		}
	}
}

// WithFlows wires the Console Flows-page handler into
// NewMux. surface is the transport-agnostic flowprotocol.Surface the
// six `POST /v1/flows/*` routes dispatch through.
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied (or supplied a nil surface), the Flows routes are NOT
// mounted — the smoke script's `skip_if_404` keeps preflight green on a
// partial build. Production wiring (`harbor dev`) supplies it so the
// Console Flows page has a live surface.
//
// Five of the six routes are read-only; `POST /v1/flows/run` mutates
// and the Surface gates it on the verified `auth.ScopeAdmin` claim
// (closed two-scope set — no new scope is minted). When
// WithValidator is also set, the handler is wrapped in auth.Middleware
// like every other transport.
func WithFlows(surface *flowprotocol.Surface) Option {
	return func(c *muxConfig) {
		if surface != nil {
			c.flowsSurface = surface
		}
	}
}

// WithToolsService wires the `tools.*` handler into NewMux —
// the seven Console Tools-page methods (`tools.list` / `tools.get` /
// `tools.describe` / `tools.metrics` / `tools.content_stats` /
// `tools.set_approval_policy` / `tools.revoke_oauth`).
//
// The service is OPTIONAL so existing call-sites compile unchanged.
// When unsupplied, the `POST /v1/tools/{method}` route is NOT mounted —
// the smoke script's `skip_if_404` keeps preflight green on a partial
// build. Production wiring (`harbor dev`) supplies it so the Console
// Tools page has a live surface. When supplied AND
// WithValidator is set, the route is wrapped in auth.Middleware like
// every other transport — the two admin methods then gate on the
// verified `auth.ScopeAdmin` claim. A nil service is treated
// as "WithToolsService not supplied".
func WithToolsService(s *toolsprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.toolsService = s
		}
	}
}

// WithAgentReachAuthorizer supplies the one immutable effective-agent gate to
// the agent-addressed stream projections. A nil value is retained so those
// projections fail closed instead of silently widening authority.
func WithAgentReachAuthorizer(a auth.AgentReachAuthorizer) Option {
	return func(c *muxConfig) { c.reach = a }
}

// WithAgentResolver supplies the lifecycle-aware resolver shared by
// control.start and explicit tools.describe projections. Reach remains a
// separate gate and always runs first.
func WithAgentResolver(resolver protocol.AgentResolver) Option {
	return func(c *muxConfig) { c.agentResolver = resolver }
}

// WithTasksService wires the `tasks.*` handler into
// NewMux — the two Console Tasks-page read methods (`tasks.list` /
// `tasks.get`).
//
// The service is OPTIONAL so existing call-sites compile unchanged.
// When unsupplied, the `POST /v1/tasks/{method}` route is NOT mounted —
// the smoke script's `skip_if_404` keeps preflight green on a partial
// build. Production wiring (`harbor dev`) supplies it so the Console
// Tasks page has a live surface. When supplied AND
// WithValidator is set, the route is wrapped in auth.Middleware like
// every other transport — a cross-tenant `tasks.list` fan-in then
// gates on the verified `auth.ScopeAdmin` claim. A nil service
// is treated as "WithTasksService not supplied".
//
// Both `tasks.*` methods are READ-ONLY (CLAUDE.md §13) — the Console
// Tasks page consumes the shipped task-control verbs for
// mutation; no `tasks.*` mutation path is mounted.
func WithTasksService(s *tasksprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.tasksService = s
		}
	}
}

// WithAgentsService wires the `agents.*` handler into
// NewMux — the eight Console Agents-page methods (`agents.list` /
// `agents.get` / `agents.tools` / `agents.memory` / `agents.governance`
// / `agents.skills` / `agents.permissions` / `agents.metrics`).
//
// The service is OPTIONAL so existing call-sites compile unchanged.
// When unsupplied, the `POST /v1/agents/{method}` route is NOT mounted
// — the smoke script's `skip_if_404` keeps preflight green on a partial
// build. Production wiring (`harbor dev`) supplies it so the Console
// Agents page has a live surface. When supplied AND
// WithValidator is set, the route is wrapped in auth.Middleware like
// every other transport.
//
// All eight `agents.*` methods are READ-ONLY projections of the Agent
// Registry — the five agent-control verbs (Pause / Drain / Restart /
// Force-Stop / Deregister) are the EXISTING shipped `registry.*`
// control verbs, not new methods (CLAUDE.md §13). A nil service
// is treated as "WithAgentsService not supplied".
func WithAgentsService(s *agentsprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.agentsService = s
		}
	}
}

// WithSessionsService wires the Console Sessions-page
// handler into NewMux — the two `POST /v1/sessions/*` routes
// (`sessions.list` / `sessions.inspect`).
//
// The service is OPTIONAL so existing call-sites compile unchanged.
// When unsupplied, the `POST /v1/sessions/{method}` routes are NOT
// mounted — the smoke script's `skip_if_404` keeps preflight green on a
// partial build. Production wiring (`harbor dev`) supplies it so the
// Console Sessions page has a live surface. When supplied
// AND WithValidator is set, the route is wrapped in auth.Middleware
// like every other transport — a cross-tenant `sessions.list` filter
// then gates on the verified `auth.ScopeAdmin` claim. A nil
// service is treated as "WithSessionsService not supplied".
func WithSessionsService(s *sessionsprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.sessionsService = s
		}
	}
}

// WithRunsService wires the Console Playground-page
// handler into NewMux — the `POST /v1/runs/set_overrides` route.
//
// The service is OPTIONAL so existing call-sites compile unchanged.
// When unsupplied, the `POST /v1/runs/{method}` route is NOT mounted —
// the smoke script's `skip_if_404` keeps preflight green on a partial
// build. Production wiring (`harbor dev`) supplies it so the Console
// Playground page can record next-message overrides. When
// supplied AND WithValidator is set, the route is wrapped in
// auth.Middleware like every other transport. A nil service is treated
// as "WithRunsService not supplied".
func WithRunsService(s *runsprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.runsService = s
		}
	}
}

// WithMemory wires the three `memory.*` read routes
// into NewMux. store is the memory.MemoryStore the Console Memory page
// projects from; driverName is the configured memory-
// driver name surfaced on each row. The memory handler reuses the
// ArtifactStore + heavy-content threshold supplied via WithPauseList
// for the heavy-value bypass — so WithPauseList must also be set
// for the three `memory.*` routes to mount.
//
// store is OPTIONAL — when it is nil (or the ArtifactStore /
// heavyThreshold from WithPauseList are unset), the three `memory.*`
// routes are left UN-mounted (the route's smoke `skip_if_404` keeps
// preflight green on a partial build). When supplied correctly the
// routes `POST /v1/memory/list` / `/get` / `/health` are mounted and,
// when WithValidator is also set, wrapped in auth.Middleware like every
// other transport.
//
// The three methods are READ-ONLY (CLAUDE.md §13) — they project the
// shipped MemoryStore surface; no mutation path is mounted.
func WithMemory(store memory.MemoryStore, driverName string) Option {
	return func(c *muxConfig) {
		c.memoryStore = store
		c.memoryDriverName = driverName
	}
}

// WithAuthSurface wires the `auth.*` handler into
// NewMux — the single net-new Console Settings-page method
// (`auth.rotate_token`). surface is the *auth.RotateSurface the
// `POST /v1/auth/rotate_token` route dispatches through.
//
// The option is OPTIONAL so existing call-sites compile unchanged.
// When not supplied (or supplied a nil surface), the `auth.*` route is
// NOT mounted — the smoke script's `skip_if_404` keeps preflight green
// on a partial build. Production wiring (`harbor dev` / `harbor
// console`) supplies it so the Console Settings page "Rotate token"
// action has a live surface. When supplied AND WithValidator is set,
// the route is wrapped in auth.Middleware like every other transport —
// `auth.rotate_token` then gates on the verified `auth.ScopeAdmin`
// claim. A nil surface is treated as "WithAuthSurface not
// supplied".
func WithAuthSurface(surface *auth.RotateSurface) Option {
	return func(c *muxConfig) {
		if surface != nil {
			c.rotateSurface = surface
		}
	}
}

// WithGovernanceService wires the admin-scoped governance tenant-override
// handler into NewMux — the `POST /v1/governance/{set,get}_tenant_overrides`
// routes.
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied (or supplied a nil service), the `/v1/governance/*` route
// is NOT mounted — the smoke script's `skip_if_404` keeps preflight green
// on a partial build. Production wiring (`harbor dev` / `harbor console`)
// supplies it so an admin can change a tenant's default LLM parameters
// live. When supplied AND WithValidator is set, the route is wrapped in
// auth.Middleware like every other transport — the handler then gates on
// the verified `auth.ScopeAdmin` claim. A nil service is treated as
// "WithGovernanceService not supplied".
func WithGovernanceService(s *governanceprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.governanceService = s
		}
	}
}

// WithGovernanceKeyRotate wires the rotate-key service so the
// `POST /v1/governance/rotate_key` route is live (the admin LLM-provider
// key-rotation surface). OPTIONAL; built only alongside WithGovernance
// Service. A nil service leaves the route returning 501. When supplied AND
// WithValidator is set, the route inherits the same admin gate as the rest
// of `/v1/governance/*`.
func WithGovernanceKeyRotate(s *governanceprotocol.KeyRotateService) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.governanceKeyRotate = s
		}
	}
}

// WithGovernancePostureWrite wires the set-posture service so the
// `POST /v1/governance/set_posture` route is live (the admin identity-tier
// policy WRITE surface — the write sibling of `governance.posture`).
// OPTIONAL; built only alongside WithGovernanceService. A nil service leaves
// the route returning 501. When supplied AND WithValidator is set, the route
// inherits the same admin gate as the rest of `/v1/governance/*`
// (auth.ScopeAdmin ONLY — a leaked read-only `console:fleet` token cannot
// widen a budget).
func WithGovernancePostureWrite(s *governanceprotocol.PostureWriteService) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.governancePostureWrite = s
		}
	}
}

// WithAgentConfigService wires the admin-scoped agent-config
// control-plane handler into NewMux — the `POST /v1/agent_config/*`
// routes (the versioned desired-state registry + skills control).
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied (or supplied a nil service), the `/v1/agent_config/*` route
// is NOT mounted — the smoke script's `skip_if_404` keeps preflight green
// on a partial build. When supplied AND WithValidator is set, the route is
// wrapped in auth.Middleware like every other transport — the handler then
// gates on the verified `auth.ScopeAdmin` claim (the agent-config authorization model). A nil service is
// treated as "WithAgentConfigService not supplied".
func WithAgentConfigService(s *agentcfgprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.agentConfigService = s
		}
	}
}

// WithUserSkillImportService wires the two-phase verified-caller
// skill-package import service (HA-61) so the
// `agent_config.user.skills.import_validate` /
// `agent_config.user.skills.import_commit` routes are live. OPTIONAL:
// when unsupplied the routes answer 501 (the partial-build convention).
// The routes require WithAgentConfigService to be mounted first.
func WithUserSkillImportService(s *agentcfgprotocol.UserSkillImportService) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.userSkillImportService = s
		}
	}
}

// WithCompositionPreviewService wires the read-only composition-preview
// service (HA-66) so the `agent_config.composition.preview` route is
// live. OPTIONAL: when unsupplied the route answers 501 (the
// partial-build convention). The route requires WithAgentConfigService
// to be mounted first.
func WithCompositionPreviewService(s *agentcfgprotocol.CompositionPreviewService) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.compositionPreviewService = s
		}
	}
}

// WithSessionTurnsService wires the session-turns read handler into
// NewMux — the two `POST /v1/sessions/turns/*` routes
// (`sessions.turns.list` / `sessions.turns.get`).
//
// The service is OPTIONAL so existing call-sites compile unchanged. When
// unsupplied, the routes are NOT mounted — the smoke script's
// `skip_if_404` keeps preflight green on a partial build. When supplied
// AND WithValidator is set, the routes are wrapped in auth.Middleware
// like every other transport. The operations lane gates on the verified
// `admin` OR `console:fleet` claim inside the service (closed two-scope
// set) and audits widened reads. A nil service is treated as
// "WithSessionTurnsService not supplied".
func WithSessionTurnsService(s *turnsprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.sessionTurnsService = s
		}
	}
}

// WithObservabilityService wires the observability handler into NewMux —
// the `POST /v1/observability/query` route (the one bounded
// administrative rollup query).
//
// The service is OPTIONAL so existing call-sites compile unchanged. When
// unsupplied, the route is NOT mounted — the smoke script's
// `skip_if_404` keeps preflight green on a partial build. When supplied
// AND WithValidator is set, the route is wrapped in auth.Middleware like
// every other transport — the widening gates on the verified `admin` OR
// `console:fleet` claim inside the service (closed two-scope set) and
// audits widened reads. A nil service is treated as
// "WithObservabilityService not supplied".
func WithObservabilityService(s *observabilityprotocol.Service) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.observabilityService = s
		}
	}
}

// WithStateHistory wires the `state.history` windowed event-replay
// handler into NewMux — the `POST /v1/state/history` route. bus is the
// durable event bus the windowed read scans (it MUST implement
// events.HistoryReplayer to serve a non-error page; a bus without the
// capability surfaces CodeRuntimeError per request); arts is the
// ArtifactStore the heavy-payload refs are enriched from.
//
// Both are required together — supplying the option with a nil bus or a
// nil store leaves the `state.history` route UN-mounted (the route's
// smoke `skip_if_404` keeps preflight green on a partial build). When
// supplied correctly the route is mounted and, when WithValidator is
// also set, wrapped in auth.Middleware like every other transport.
//
// state.history is READ-ONLY (CLAUDE.md §13) — it projects the durable
// event log; no mutation path is mounted.
func WithStateHistory(bus events.EventBus, arts artifacts.ArtifactStore) Option {
	return func(c *muxConfig) {
		if bus != nil {
			c.stateHistoryBus = bus
		}
		if arts != nil {
			c.stateHistoryArtifacts = arts
		}
	}
}

// WithEventsList wires the `events.list` durable, time-ranged,
// cross-session raw-event read handler into NewMux — the
// `POST /v1/events/list` route. The bus is the mandatory NewMux bus (it
// MUST implement events.HistoryReplayer to serve a non-error page; a bus
// without the capability surfaces CodeRuntimeError per request); arts is
// the ArtifactStore the heavy-payload refs are enriched from.
//
// A nil store leaves the `events.list` route UN-mounted (the route's smoke
// `skip_if_404` keeps preflight green on a partial build). When supplied
// the route is mounted and, when WithValidator is also set, wrapped in
// auth.Middleware like every other transport.
//
// events.list is READ-ONLY (CLAUDE.md §13) — it projects the durable event
// log; no mutation path is mounted.
func WithEventsList(arts artifacts.ArtifactStore) Option {
	return func(c *muxConfig) {
		if arts != nil {
			c.eventsListArtifacts = arts
		}
	}
}

// WithoutValidator selects the bearer-less identity posture: the mux
// establishes each request's identity from the X-Harbor-* carrier
// headers instead of a bearer token, and refuses 401 when they supply
// no complete triple. It does NOT waive identity — no mounted route
// ever runs without an established one — it names a different carrier
// for it.
//
// It remains test-only. Production deployments wire a validator; the
// serve band never takes this path, and the curated SDK facade does not
// expose it. What changed is only what the posture MEANS: "identity
// arrives on the headers", not "there is no identity".
//
// WithoutValidator is the explicit, test-only escape hatch for cases
// that legitimately need the trust-based posture (the REST
// handler inherits `ControlSurface.Dispatch`'s identity-from-body
// gate, the SSE handler resolves identity from the `X-Harbor-*`
// carrier headers via `resolveIdentity`). It is used by the
// own package tests + the wire-transport integration suite under
// `test/integration/` to assert the pre-auth transport surface still
// works.
//
// PRODUCTION CODE MUST NEVER USE THIS OPTION. A Runtime that boots
// with `WithoutValidator` exposes an unauthenticated Protocol surface,
// which violates CLAUDE.md §13's "Test stubs as production defaults"
// rule. The option is named for grepability: an audit can find every
// production-side WithoutValidator call site at compile time.
func WithoutValidator() Option {
	return func(c *muxConfig) { c.withoutAuth = true }
}

// ErrMisconfigured — NewMux was called with a nil ControlSurface or a
// nil EventBus. Both are mandatory: the former feeds the REST control
// transport, the latter feeds the SSE event transport. Fails closed
// (CLAUDE.md §5) rather than mounting a half-built mux.
var ErrMisconfigured = errors.New("transports: NewMux missing a mandatory dependency")

// NewMux composes the Protocol wire transports into a single
// *http.ServeMux:
//
//   - POST /v1/control/{method} — the REST/JSON control surface.
//   - GET  /v1/events           — the SSE event stream.
//
// All three configuration choices are mandatory:
//   - a non-nil ControlSurface,
//   - a non-nil EventBus,
//   - and EITHER `WithValidator(v)` (the production posture — JWT
//     bearer auth at the edge) OR `WithoutValidator()` (the explicit,
//     test-only escape hatch for the trust-based posture).
//
// A missing dependency — including the auth choice — fails loud with
// ErrMisconfigured rather than mounting a half-built mux or an
// unauthenticated production surface (CLAUDE.md §13 "Test stubs as
// production defaults on operator-facing seams"; PR #91).
//
// The returned mux is immutable after construction and safe
// to share across N concurrent requests — a future server
// (`harbor dev`) mounts it and owns the listen / shutdown
// lifecycle.
func NewMux(cs *protocol.ControlSurface, bus events.EventBus, opts ...Option) (*http.ServeMux, error) {
	if cs == nil {
		return nil, fmt.Errorf("%w: protocol.ControlSurface is nil", ErrMisconfigured)
	}
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is nil", ErrMisconfigured)
	}

	cfg := muxConfig{logger: slog.Default()}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Auth posture is mandatory: WithValidator OR WithoutValidator.
	// Neither = fail loud (CLAUDE.md §13 / PR #91).
	if cfg.validator == nil && !cfg.withoutAuth {
		return nil, fmt.Errorf("%w: WithValidator is required (or WithoutValidator() for the explicit test-only escape hatch — see CLAUDE.md §13)", ErrMisconfigured)
	}

	controlOpts := []control.Option{
		control.WithLogger(cfg.logger),
		// thread the bus and (optional) redactor into the
		// control transport so the admin-impersonation gate can emit
		// `audit.admin_scope_used` events. The bus is mandatory at
		// NewMux already; the redactor is optional at the type level
		// but mandatory in practice for impersonation (the control
		// handler refuses impersonation paths fail-closed when either
		// is missing).
		control.WithEventBus(bus),
	}
	if cfg.redactor != nil {
		controlOpts = append(controlOpts, control.WithRedactor(cfg.redactor))
	}
	if cfg.searchSurface != nil {
		controlOpts = append(controlOpts, control.WithSearchSurface(cfg.searchSurface))
	}
	if cfg.postureSurface != nil {
		controlOpts = append(controlOpts, control.WithPostureSurface(cfg.postureSurface))
	}
	if cfg.artifactsSurface != nil {
		controlOpts = append(controlOpts, control.WithArtifactsSurface(cfg.artifactsSurface))
	}
	if cfg.mcpSurface != nil {
		controlOpts = append(controlOpts, control.WithMCPSurface(cfg.mcpSurface))
	}
	if cfg.appsSurface != nil {
		controlOpts = append(controlOpts, control.WithAppsSurface(cfg.appsSurface))
	}
	if cfg.skillPublicationsSurface != nil {
		controlOpts = append(controlOpts, control.WithSkillPublicationsSurface(cfg.skillPublicationsSurface))
	}
	controlHandler, err := control.NewHandler(cs, controlOpts...)
	if err != nil {
		return nil, fmt.Errorf("transports: build control handler: %w", err)
	}

	streamOpts := []stream.Option{stream.WithLogger(cfg.logger)}
	if cfg.keepalive > 0 {
		streamOpts = append(streamOpts, stream.WithKeepalive(cfg.keepalive))
	}
	streamHandler, err := stream.NewHandler(bus, streamOpts...)
	if err != nil {
		return nil, fmt.Errorf("transports: build stream handler: %w", err)
	}

	// The events.aggregate handler shares the
	// bus and lives in the same package. It is built once per Runtime
	// process; if the bus does not implement events.Replayer, the
	// handler still mounts — the per-request error path returns
	// CodeRuntimeError + HTTP 500 with a clear "no historical
	// aggregation" message.
	aggregatorOpts := []events.AggregatorOption{}
	if cfg.aggregatorClock != nil {
		aggregatorOpts = append(aggregatorOpts, events.WithAggregatorClock(cfg.aggregatorClock))
	}
	aggregator, err := events.NewAggregator(bus, aggregatorOpts...)
	if err != nil {
		return nil, fmt.Errorf("transports: build events aggregator: %w", err)
	}
	aggregateHandler, err := stream.NewAggregateHandler(aggregator, stream.WithAggregateLogger(cfg.logger))
	if err != nil {
		return nil, fmt.Errorf("transports: build events.aggregate handler: %w", err)
	}

	// The pause.list snapshot handler. Built only
	// when WithPauseList supplied all three dependencies (coordinator,
	// store, positive threshold). When any is missing the route is left
	// un-mounted — the smoke `skip_if_404` keeps preflight green on a
	// partial build.
	var pauseListHandler *stream.PauseListHandler
	if cfg.pauseCoordinator != nil && cfg.artifactStore != nil && cfg.heavyThreshold > 0 {
		plh, err := stream.NewPauseListHandler(
			cfg.pauseCoordinator, cfg.artifactStore, cfg.heavyThreshold,
			stream.WithPauseListLogger(cfg.logger),
			stream.WithPauseListBus(bus),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build pause.list handler: %w", err)
		}
		pauseListHandler = plh
	}

	// The three `memory.*` read handlers.
	// Built only when WithMemory supplied a MemoryStore AND the
	// ArtifactStore + inline-payload bound (shared with pause.list —
	// the pinned Console bound, not the operator's LLM-context
	// threshold) are set. When any is missing the three routes are left un-mounted
	// — the smoke `skip_if_404` keeps preflight green on a partial
	// build. The memory handler reuses the events Aggregator built
	// above for the 24h identity-rejected / recovery-dropped counters.
	var memoryHandler *stream.MemoryHandler
	if cfg.memoryStore != nil && cfg.artifactStore != nil && cfg.heavyThreshold > 0 {
		mh, err := stream.NewMemoryHandler(
			cfg.memoryStore, cfg.artifactStore, cfg.heavyThreshold,
			stream.WithMemoryLogger(cfg.logger),
			stream.WithMemoryAggregator(aggregator),
			stream.WithMemoryDriverName(cfg.memoryDriverName),
			// the mutation methods (memory.put /
			// memory.delete) publish their audit events on the same bus the
			// Mux is built over.
			stream.WithMemoryBus(bus),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build memory handler: %w", err)
		}
		memoryHandler = mh
	}

	// The `tools.*` handler. Built only when
	// WithToolsService supplied a non-nil service. When unsupplied the
	// `POST /v1/tools/{method}` route is left un-mounted — the smoke
	// `skip_if_404` keeps preflight green on a partial build.
	var toolsHandler *stream.ToolsHandler
	if cfg.toolsService != nil {
		th, err := stream.NewToolsHandler(cfg.toolsService,
			stream.WithToolsLogger(cfg.logger),
			stream.WithToolsReachAuthorizer(cfg.reach),
			stream.WithToolsAgentResolver(cfg.agentResolver))
		if err != nil {
			return nil, fmt.Errorf("transports: build tools handler: %w", err)
		}
		toolsHandler = th
	}

	// The `tasks.*` handler. Built only
	// when WithTasksService supplied a non-nil service. When unsupplied
	// the `POST /v1/tasks/{method}` route is left un-mounted — the smoke
	// `skip_if_404` keeps preflight green on a partial build.
	var tasksHandler *stream.TasksHandler
	if cfg.tasksService != nil {
		th, err := stream.NewTasksHandler(cfg.tasksService, stream.WithTasksLogger(cfg.logger))
		if err != nil {
			return nil, fmt.Errorf("transports: build tasks handler: %w", err)
		}
		tasksHandler = th
	}

	// The Console Flows-page handler. Built
	// only when WithFlows supplied a non-nil surface. When unsupplied
	// the six `/v1/flows/*` routes are left un-mounted — the smoke
	// `skip_if_404` keeps preflight green on a partial build.
	var flowsHandler *stream.FlowsHandler
	if cfg.flowsSurface != nil {
		fh, err := stream.NewFlowsHandler(
			cfg.flowsSurface,
			stream.WithFlowsLogger(cfg.logger),
			stream.WithFlowsBus(bus),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build flows handler: %w", err)
		}
		flowsHandler = fh
	}

	// The Console Agents-page handler.
	// Built only when WithAgentsService supplied a non-nil service.
	// When unsupplied the `POST /v1/agents/{method}` route is left
	// un-mounted — the smoke `skip_if_404` keeps preflight green on a
	// partial build.
	var agentsHandler *stream.AgentsHandler
	if cfg.agentsService != nil {
		ah, err := stream.NewAgentsHandler(cfg.agentsService, stream.WithAgentsLogger(cfg.logger))
		if err != nil {
			return nil, fmt.Errorf("transports: build agents handler: %w", err)
		}
		agentsHandler = ah
	}

	// The Console Sessions-page handler.
	// Built only when WithSessionsService supplied a non-nil service.
	// When unsupplied the two `/v1/sessions/*` routes are left
	// un-mounted — the smoke `skip_if_404` keeps preflight green on a
	// partial build.
	var sessionsHandler *stream.SessionsHandler
	if cfg.sessionsService != nil {
		sh, err := stream.NewSessionsHandler(
			cfg.sessionsService,
			stream.WithSessionsLogger(cfg.logger),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build sessions handler: %w", err)
		}
		sessionsHandler = sh
	}

	// The Console Playground-page handler.
	// Built only when WithRunsService supplied a non-nil service. When
	// unsupplied the `/v1/runs/*` route is left un-mounted — the smoke
	// `skip_if_404` keeps preflight green on a partial build.
	var runsHandler *stream.RunsHandler
	if cfg.runsService != nil {
		rh, err := stream.NewRunsHandler(
			cfg.runsService,
			stream.WithRunsLogger(cfg.logger),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build runs handler: %w", err)
		}
		runsHandler = rh
	}

	// The `auth.*` handler. Built only
	// when WithAuthSurface supplied a non-nil *auth.RotateSurface. When
	// unsupplied the `POST /v1/auth/{method}` route is left un-mounted
	// — the smoke `skip_if_404` keeps preflight green on a partial
	// build.
	var authHandler *stream.AuthHandler
	if cfg.rotateSurface != nil {
		ah, err := stream.NewAuthHandler(cfg.rotateSurface, stream.WithAuthLogger(cfg.logger))
		if err != nil {
			return nil, fmt.Errorf("transports: build auth handler: %w", err)
		}
		authHandler = ah
	}

	// The admin-scoped governance tenant-override handler. Built only when
	// WithGovernanceService supplied a non-nil service. When unsupplied
	// the `/v1/governance/*` route is left un-mounted — the smoke
	// `skip_if_404` keeps preflight green on a partial build.
	var governanceHandler *stream.GovernanceHandler
	if cfg.governanceService != nil {
		govOpts := []stream.GovernanceOption{stream.WithGovernanceLogger(cfg.logger)}
		if cfg.governanceKeyRotate != nil {
			govOpts = append(govOpts, stream.WithGovernanceKeyRotate(cfg.governanceKeyRotate))
		}
		if cfg.governancePostureWrite != nil {
			govOpts = append(govOpts, stream.WithGovernancePostureWrite(cfg.governancePostureWrite))
		}
		gh, err := stream.NewGovernanceHandler(cfg.governanceService, govOpts...)
		if err != nil {
			return nil, fmt.Errorf("transports: build governance handler: %w", err)
		}
		governanceHandler = gh
	}

	// The `state.history` windowed event-replay handler. Built only when
	// WithStateHistory supplied a non-nil bus AND a non-nil artifact
	// store. When either is missing the `POST /v1/state/history` route is
	// left un-mounted — the smoke `skip_if_404` keeps preflight green on a
	// partial build.
	var stateHistoryHandler *stream.StateHistoryHandler
	if cfg.stateHistoryBus != nil && cfg.stateHistoryArtifacts != nil {
		shh, err := stream.NewStateHistoryHandler(
			cfg.stateHistoryBus, cfg.stateHistoryArtifacts,
			stream.WithStateHistoryLogger(cfg.logger),
			stream.WithStateHistoryRedactor(cfg.redactor),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build state.history handler: %w", err)
		}
		stateHistoryHandler = shh
	}

	// The `events.list` durable, time-ranged, cross-session raw-event read
	// handler. Built only when WithEventsList supplied a non-nil artifact
	// store (the bus is the mandatory NewMux bus). When missing the
	// `POST /v1/events/list` route is left un-mounted — the smoke
	// `skip_if_404` keeps preflight green on a partial build.
	var eventsListHandler *stream.EventsListHandler
	if cfg.eventsListArtifacts != nil {
		elh, err := stream.NewEventsListHandler(
			bus, cfg.eventsListArtifacts,
			stream.WithEventsListLogger(cfg.logger),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build events.list handler: %w", err)
		}
		eventsListHandler = elh
	}

	// The admin-scoped agent-config control-plane handler. Built only when
	// WithAgentConfigService supplied a non-nil service. When unsupplied
	// the `/v1/agent_config/*` route is left un-mounted — the smoke
	// `skip_if_404` keeps preflight green on a partial build.
	var agentConfigHandler *stream.AgentConfigHandler
	if cfg.agentConfigService != nil {
		opts := []stream.AgentConfigOption{
			stream.WithAgentConfigLogger(cfg.logger),
			stream.WithAgentConfigReachAuthorizer(cfg.reach),
		}
		if cfg.userSkillImportService != nil {
			opts = append(opts, stream.WithUserSkillImportService(cfg.userSkillImportService))
		}
		if cfg.compositionPreviewService != nil {
			opts = append(opts, stream.WithCompositionPreviewService(cfg.compositionPreviewService))
		}
		ach, err := stream.NewAgentConfigHandler(cfg.agentConfigService, opts...)
		if err != nil {
			return nil, fmt.Errorf("transports: build agent-config handler: %w", err)
		}
		agentConfigHandler = ach
	}

	// The session-turns read handler. Built only when
	// WithSessionTurnsService supplied a non-nil service. When unsupplied
	// the two `POST /v1/sessions/turns/*` routes are left un-mounted —
	// the smoke `skip_if_404` keeps preflight green on a partial build.
	var sessionTurnsHandler *stream.SessionTurnsHandler
	if cfg.sessionTurnsService != nil {
		sth, err := stream.NewSessionTurnsHandler(
			cfg.sessionTurnsService,
			stream.WithSessionTurnsLogger(cfg.logger),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build session-turns handler: %w", err)
		}
		sessionTurnsHandler = sth
	}

	// The observability handler. Built only when WithObservabilityService
	// supplied a non-nil service. When unsupplied the
	// `POST /v1/observability/query` route is left un-mounted — the smoke
	// `skip_if_404` keeps preflight green on a partial build.
	var observabilityHandler *stream.ObservabilityHandler
	if cfg.observabilityService != nil {
		oh, err := stream.NewObservabilityHandler(
			cfg.observabilityService,
			stream.WithObservabilityLogger(cfg.logger),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build observability handler: %w", err)
		}
		observabilityHandler = oh
	}

	mux := http.NewServeMux()

	// when WithValidator was supplied, wrap both transport
	// handlers in auth.Middleware. The middleware enforces the JWT
	// bearer at the edge, injects the verified identity + scopes into
	// r.Context(), and then calls the wrapped handler — which reads
	// identity from ctx (preferred) or the trust-based
	// carriers (fallback when WithValidator is not set).
	// Every mounted route is wrapped in the same identity middleware, so
	// every handler runs with an established identity on ctx or does not
	// run at all. requestMiddleware picks the bearer decorator when a
	// validator is wired and the carrier decorator on the explicit
	// bearer-less opt-in; there is no third posture in which a handler
	// sees an unestablished request.
	mw := cfg.requestMiddleware()
	mountedControl := mw(controlHandler)
	// SSE-only: promote `?access_token=...` to a synthesized
	// Authorization header so EventSource cross-origin clients
	// (Console multi-process posture) can subscribe. The shim
	// wraps OUTSIDE the auth middleware so the request is
	// rewritten BEFORE the JWT validation reads the header.
	mountedStream := auth.SSEAccessTokenShim(mw(streamHandler))
	mountedAggregate := mw(aggregateHandler)

	mux.Handle(control.RoutePattern, mountedControl)
	mux.Handle(stream.RoutePattern, mountedStream)
	mux.Handle(stream.AggregateRoutePattern, mountedAggregate)

	if pauseListHandler != nil {
		mountedPauseList := mw(pauseListHandler)
		mux.Handle(stream.PauseListRoutePattern, mountedPauseList)
	}

	if memoryHandler != nil {
		mountMemory := func(pattern string, h http.Handler) {
			mux.Handle(pattern, mw(h))
		}
		mountMemory(stream.MemoryListRoutePattern, memoryHandler.ListHandler())
		mountMemory(stream.MemoryGetRoutePattern, memoryHandler.GetHandler())
		mountMemory(stream.MemoryHealthRoutePattern, memoryHandler.HealthHandler())
		// the strategy-trace read + the admin-gated
		// mutation pair. Same mount family as the read trio.
		mountMemory(stream.MemoryStrategyTraceRoutePattern, memoryHandler.StrategyTraceHandler())
		mountMemory(stream.MemoryPutRoutePattern, memoryHandler.PutHandler())
		mountMemory(stream.MemoryDeleteRoutePattern, memoryHandler.DeleteHandler())
	}

	if toolsHandler != nil {
		mountedTools := mw(toolsHandler)
		mux.Handle(stream.ToolsRoutePattern, mountedTools)
	}

	if flowsHandler != nil {
		mountedFlows := mw(flowsHandler)
		mux.Handle(stream.FlowsRoutePattern, mountedFlows)
	}

	if tasksHandler != nil {
		mountedTasks := mw(tasksHandler)
		mux.Handle(stream.TasksRoutePattern, mountedTasks)
	}

	if agentsHandler != nil {
		mountedAgents := mw(agentsHandler)
		mux.Handle(stream.AgentsRoutePattern, mountedAgents)
	}

	if sessionsHandler != nil {
		mountedSessions := mw(sessionsHandler)
		mux.Handle(stream.SessionsRoutePattern, mountedSessions)
	}

	if runsHandler != nil {
		mountedRuns := mw(runsHandler)
		mux.Handle(stream.RunsRoutePattern, mountedRuns)
	}

	if authHandler != nil {
		mountedAuth := mw(authHandler)
		mux.Handle(stream.AuthRoutePattern, mountedAuth)
	}

	if governanceHandler != nil {
		mountedGovernance := mw(governanceHandler)
		mux.Handle(stream.GovernanceRoutePattern, mountedGovernance)
	}

	if agentConfigHandler != nil {
		mountedAgentConfig := mw(agentConfigHandler)
		mux.Handle(stream.AgentConfigRoutePattern, mountedAgentConfig)
	}

	if sessionTurnsHandler != nil {
		mountedSessionTurns := mw(sessionTurnsHandler)
		mux.Handle(stream.SessionTurnsRoutePattern, mountedSessionTurns)
	}

	if observabilityHandler != nil {
		mountedObservability := mw(observabilityHandler)
		mux.Handle(stream.ObservabilityRoutePattern, mountedObservability)
	}

	if stateHistoryHandler != nil {
		mux.Handle(stream.StateHistoryRoutePattern, mw(stateHistoryHandler.Handler()))
	}

	if eventsListHandler != nil {
		mux.Handle(stream.EventsListRoutePattern, mw(eventsListHandler.Handler()))
	}
	return mux, nil
}
