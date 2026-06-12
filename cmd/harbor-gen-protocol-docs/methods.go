package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// methodEntry is the generator-side join row for one canonical Protocol
// method: the wire route it is served on, the request/response wire
// types (names resolving into singlesource.CanonicalWireTypes / the
// typeInstanceIndex), whether the method mutates runtime state, and its
// auth posture beyond the identity-mandatory baseline. The route is
// derived from the transports' exported *RoutePattern constants (never a
// hand-typed path); the classification column is computed from the
// methods package's own Is*Method predicates at render time.
//
// TestGen_MethodTableInLockstep pins the table: every methods.Methods()
// entry MUST have a row here (and no row may name an unregistered
// method), every request/response name MUST resolve, and every route
// MUST derive from an exported *RoutePattern constant.
type methodEntry struct {
	// Route is the concrete wire route ("POST /v1/control/start"),
	// derived from a transports route-pattern constant.
	Route string
	// Request / Response are wire-type names from CanonicalWireTypes.
	// Empty Request means "no JSON body" (the SSE subscribe's filter
	// travels in query/header form); the rendered cell then carries
	// RequestNote / ResponseNote instead.
	Request  string
	Response string
	// RequestNote / ResponseNote replace the type link for the
	// non-JSON-body surfaces (SSE).
	RequestNote  string
	ResponseNote string
	// Mutates is true when the method changes runtime state (spawn,
	// steer, put, delete, admin verbs). Read-only projections are false.
	Mutates bool
	// Auth is the posture beyond the identity-mandatory baseline
	// (empty = baseline only).
	Auth string
	// CrossTenant is the machine-readable cross-tenant entitlement for
	// a read method's fan-in. It is the SINGLE source both the rendered
	// Auth note (crossTenantNote) and the live-probe lockstep test
	// (TestGen_AuthColumnMatchesHandlerGates) consult — the §17.5
	// Protocol-track audit found the hand-typed note had drifted from
	// the handlers' actual scope gates on ~10 rows, so the cell is now
	// pinned by driving fleet-only vs admin-only tokens against every
	// noted row and asserting the observed accept/reject matches.
	CrossTenant crossTenantPolicy
}

// crossTenantPolicy enumerates what a read method's handler actually
// does with a cross-tenant request. The values mirror the deployed
// gates — document what the code does, never what symmetry suggests.
type crossTenantPolicy int

const (
	// crossTenantNone — no cross-tenant fan-in exists under any scope:
	// the handler overlays the verified identity onto the request (or
	// rejects a body identity that differs from it), so every caller
	// sees their own tuple only (e.g. tools.* reads, agents.* reads,
	// memory.get).
	crossTenantNone crossTenantPolicy = iota
	// crossTenantAdminOnly — the cross-tenant filter gate consults the
	// verified `admin` scope claim ONLY; `console:fleet` is rejected
	// (tasks.list, pause.list, flows.list, flows.runs.list,
	// topology.snapshot).
	crossTenantAdminOnly
	// crossTenantAdminOrFleet — the cross-tenant filter gate admits the
	// closed two-scope set: `admin` OR `console:fleet` (events,
	// search, sessions.list, artifacts.list, memory.list, posture).
	crossTenantAdminOrFleet
	// crossTenantAdminWidens — no rejecting cross-tenant filter shape
	// exists on the wire; the verified `admin` claim widens result
	// visibility across tenants instead (flows.describe,
	// flows.runs.describe, flows.metrics — run aggregates are
	// tenant-scoped unless admin). Not status-probeable, so the
	// lockstep test pins only that no rejecting gate is claimed.
	crossTenantAdminWidens
)

// crossTenantAuthNote renders the per-policy Auth-column note. One
// switch — the doc cell and the probe semantics derive from the same
// crossTenantPolicy value.
func crossTenantAuthNote(p crossTenantPolicy) string {
	switch p {
	case crossTenantAdminOnly:
		return "cross-tenant fan-in requires the verified `admin` scope claim (`console:fleet` is not consulted)"
	case crossTenantAdminOrFleet:
		return "cross-tenant fan-in requires `admin` or `console:fleet`"
	case crossTenantAdminWidens:
		return "tenant-scoped results; the verified `admin` scope claim widens run visibility across tenants"
	case crossTenantNone:
		return ""
	default:
		return ""
	}
}

// methodToControlType mirrors the protocol package's (unexported)
// method-name → steering.ControlType translation for the nine
// steering-control methods, so the generator can render each control's
// RFC §6.3 minimum steering scope via steering.RequiredScope. Pinned by
// TestGen_MethodTableInLockstep: every IsControlMethod entry must be
// here and resolve to a registered ControlType.
var methodToControlType = map[methods.Method]steering.ControlType{
	methods.MethodCancel:        steering.ControlCancel,
	methods.MethodPause:         steering.ControlPause,
	methods.MethodResume:        steering.ControlResume,
	methods.MethodRedirect:      steering.ControlRedirect,
	methods.MethodInjectContext: steering.ControlInjectContext,
	methods.MethodApprove:       steering.ControlApprove,
	methods.MethodReject:        steering.ControlReject,
	methods.MethodPrioritize:    steering.ControlPrioritize,
	methods.MethodUserMessage:   steering.ControlUserMessage,
}

// adminNote is the shared auth-posture string for the admin-gated
// methods (closed two-scope set).
const adminNote = "requires the verified `admin` scope claim"

// controlRoute derives the concrete control-transport route for a
// method from control.RoutePattern.
func controlRoute(m methods.Method) string {
	return strings.Replace(control.RoutePattern, "{method}", string(m), 1)
}

// wildcardRoute substitutes a {method} wildcard route pattern with the
// method's verb suffix (the part after the cluster prefix).
func wildcardRoute(pattern, prefix string, m methods.Method) string {
	return strings.Replace(pattern, "{method}", strings.TrimPrefix(string(m), prefix), 1)
}

// subtreeRoute appends a suffix to a trailing-slash subtree route
// pattern ("POST /v1/flows/"), converting the method's dotted remainder
// to path segments (flows.runs.list -> runs/list).
func subtreeRoute(pattern, prefix string, m methods.Method) string {
	suffix := strings.ReplaceAll(strings.TrimPrefix(string(m), prefix), ".", "/")
	return pattern + suffix
}

// methodTable returns the full per-method join table. Built by a
// function (not a package-level var) so route derivation reads the
// transports constants at call time and the lockstep test exercises the
// same construction the renderer uses.
func methodTable() map[methods.Method]methodEntry {
	t := map[methods.Method]methodEntry{
		// --- Task control: start + the nine steering controls.
		methods.MethodStart: {
			Route: controlRoute(methods.MethodStart), Mutates: true,
			Request: "StartRequest", Response: "StartResponse",
		},

		// --- Streaming events.
		methods.MethodEventsSubscribe: {
			Route: stream.RoutePattern, Mutates: false,
			RequestNote:  "no JSON body — the filter travels as query/header values (`X-Harbor-Run`, `X-Harbor-Event-Type`, `?admin=1`, `Last-Event-ID`)",
			ResponseNote: "SSE stream (`text/event-stream`); see [events.md](./events.md)",
			CrossTenant:  crossTenantAdminOrFleet,
		},
		methods.MethodEventsAggregate: {
			Route: stream.AggregateRoutePattern, Mutates: false,
			Request: "EventAggregateRequest", Response: "EventAggregateResponse",
			CrossTenant: crossTenantAdminOrFleet,
		},

		// --- Search cluster — all five share one shape.
		methods.MethodSearchQuery:     searchEntry(methods.MethodSearchQuery),
		methods.MethodSearchSessions:  searchEntry(methods.MethodSearchSessions),
		methods.MethodSearchTasks:     searchEntry(methods.MethodSearchTasks),
		methods.MethodSearchEvents:    searchEntry(methods.MethodSearchEvents),
		methods.MethodSearchArtifacts: searchEntry(methods.MethodSearchArtifacts),

		// --- Posture cluster — read-only; all seven
		// take the shared RuntimeInfoRequest identity envelope.
		methods.MethodRuntimeInfo:       postureEntry(methods.MethodRuntimeInfo, "RuntimeInfo"),
		methods.MethodRuntimeHealth:     postureEntry(methods.MethodRuntimeHealth, "RuntimeHealth"),
		methods.MethodRuntimeCounters:   postureEntry(methods.MethodRuntimeCounters, "RuntimeCounters"),
		methods.MethodRuntimeDrivers:    postureEntry(methods.MethodRuntimeDrivers, "RuntimeDrivers"),
		methods.MethodMetricsSnapshot:   postureEntry(methods.MethodMetricsSnapshot, "MetricsSnapshot"),
		methods.MethodGovernancePosture: postureEntry(methods.MethodGovernancePosture, "GovernancePostureResponse"),
		methods.MethodLLMPosture:        postureEntry(methods.MethodLLMPosture, "LLMPostureResponse"),

		// --- Pause snapshot.
		methods.MethodPauseList: {
			Route: stream.PauseListRoutePattern, Mutates: false,
			Request: "PauseListRequest", Response: "PauseListResponse",
			CrossTenant: crossTenantAdminOnly,
		},

		// --- Topology.
		methods.MethodTopologySnapshot: {
			Route: controlRoute(methods.MethodTopologySnapshot), Mutates: false,
			Request: "TopologySnapshotRequest", Response: "TopologyProjection",
			CrossTenant: crossTenantAdminOnly,
		},

		// --- Artifacts.
		methods.MethodArtifactsList: {
			Route: controlRoute(methods.MethodArtifactsList), Mutates: false,
			Request: "ArtifactsListRequest", Response: "ArtifactsListResponse",
			CrossTenant: crossTenantAdminOrFleet,
		},
		methods.MethodArtifactsPut: {
			Route: controlRoute(methods.MethodArtifactsPut), Mutates: true,
			Request: "ArtifactsPutRequest", Response: "ArtifactsPutResponse",
		},
		methods.MethodArtifactsGetRef: {
			Route: controlRoute(methods.MethodArtifactsGetRef), Mutates: false,
			Request: "ArtifactsGetRefRequest", Response: "ArtifactsGetRefResponse",
		},
		methods.MethodArtifactsDelete: {
			Route: controlRoute(methods.MethodArtifactsDelete), Mutates: true,
			Request: "ArtifactsDeleteRequest", Response: "ArtifactsDeleteResponse",
			Auth: adminNote,
		},

		// --- Memory.
		methods.MethodMemoryList: {
			Route: stream.MemoryListRoutePattern, Mutates: false,
			Request: "MemoryListRequest", Response: "MemoryListResponse",
			CrossTenant: crossTenantAdminOrFleet,
		},
		methods.MethodMemoryGet: {
			Route: stream.MemoryGetRoutePattern, Mutates: false,
			Request: "MemoryGetRequest", Response: "MemoryGetResponse",
		},
		methods.MethodMemoryHealth: {
			Route: stream.MemoryHealthRoutePattern, Mutates: false,
			Request: "MemoryHealthRequest", Response: "MemoryHealthResponse",
		},
		methods.MethodMemoryStrategyTrace: {
			Route: stream.MemoryStrategyTraceRoutePattern, Mutates: false,
			Request: "MemoryStrategyTraceRequest", Response: "MemoryStrategyTraceResponse",
		},
		methods.MethodMemoryPut: {
			Route: stream.MemoryPutRoutePattern, Mutates: true,
			Request: "MemoryPutRequest", Response: "MemoryPutResponse",
			Auth: adminNote,
		},
		methods.MethodMemoryDelete: {
			Route: stream.MemoryDeleteRoutePattern, Mutates: true,
			Request: "MemoryDeleteRequest", Response: "MemoryDeleteResponse",
			Auth: adminNote,
		},

		// --- MCP Connections: nine reads + three
		// admin verbs, all on the control transport.
		methods.MethodMCPServersList:             mcpEntry(methods.MethodMCPServersList, "MCPServersListRequest", "MCPServersListResponse", false),
		methods.MethodMCPServersGet:              mcpEntry(methods.MethodMCPServersGet, "MCPServerGetRequest", "MCPServerGetResponse", false),
		methods.MethodMCPServersResources:        mcpEntry(methods.MethodMCPServersResources, "MCPServerResourcesRequest", "MCPServerResourcesResponse", false),
		methods.MethodMCPServersPrompts:          mcpEntry(methods.MethodMCPServersPrompts, "MCPServerPromptsRequest", "MCPServerPromptsResponse", false),
		methods.MethodMCPServersRefreshDiscovery: mcpEntry(methods.MethodMCPServersRefreshDiscovery, "MCPServerRefreshDiscoveryRequest", "MCPServerRefreshDiscoveryResponse", true),
		methods.MethodMCPServersProbe:            mcpEntry(methods.MethodMCPServersProbe, "MCPServerProbeRequest", "MCPServerProbeResponse", true),
		methods.MethodMCPServersHealth:           mcpEntry(methods.MethodMCPServersHealth, "MCPServerHealthRequest", "MCPServerHealthResponse", false),
		methods.MethodMCPServersBindingsList:     mcpEntry(methods.MethodMCPServersBindingsList, "MCPServerBindingsListRequest", "MCPServerBindingsListResponse", false),
		methods.MethodMCPServersPolicy:           mcpEntry(methods.MethodMCPServersPolicy, "MCPServerPolicyRequest", "MCPServerPolicyResponse", false),
		methods.MethodMCPServersRefreshBinding:   mcpEntry(methods.MethodMCPServersRefreshBinding, "MCPServerRefreshBindingRequest", "MCPServerRefreshBindingResponse", true),
		methods.MethodMCPServersRevokeBinding:    mcpEntry(methods.MethodMCPServersRevokeBinding, "MCPServerRevokeBindingRequest", "MCPServerRevokeBindingResponse", true),
		methods.MethodMCPServersSetRawHTMLTrust:  mcpEntry(methods.MethodMCPServersSetRawHTMLTrust, "MCPServerSetRawHTMLTrustRequest", "MCPServerSetRawHTMLTrustResponse", true),

		// --- Tools: five reads + two admin verbs.
		methods.MethodToolsList: {
			Route: wildcardRoute(stream.ToolsRoutePattern, "tools.", methods.MethodToolsList), Mutates: false,
			Request: "ToolListRequest", Response: "ToolListResponse",
		},
		methods.MethodToolsGet: {
			Route: wildcardRoute(stream.ToolsRoutePattern, "tools.", methods.MethodToolsGet), Mutates: false,
			Request: "ToolGetRequest", Response: "Tool",
		},
		methods.MethodToolsDescribe: {
			Route: wildcardRoute(stream.ToolsRoutePattern, "tools.", methods.MethodToolsDescribe), Mutates: false,
			Request: "ToolDescribeRequest", Response: "ToolManifest",
		},
		methods.MethodToolsMetrics: {
			Route: wildcardRoute(stream.ToolsRoutePattern, "tools.", methods.MethodToolsMetrics), Mutates: false,
			Request: "ToolMetricsRequest", Response: "ToolMetrics",
		},
		methods.MethodToolsContentStats: {
			Route: wildcardRoute(stream.ToolsRoutePattern, "tools.", methods.MethodToolsContentStats), Mutates: false,
			Request: "ToolContentStatsRequest", Response: "ToolContentStats",
		},
		methods.MethodToolsSetApprovalPolicy: {
			Route: wildcardRoute(stream.ToolsRoutePattern, "tools.", methods.MethodToolsSetApprovalPolicy), Mutates: true,
			Request: "ToolSetApprovalPolicyRequest", Response: "ToolSetApprovalPolicyResponse",
			Auth: adminNote,
		},
		methods.MethodToolsRevokeOAuth: {
			Route: wildcardRoute(stream.ToolsRoutePattern, "tools.", methods.MethodToolsRevokeOAuth), Mutates: true,
			Request: "ToolRevokeOAuthRequest", Response: "ToolRevokeOAuthResponse",
			Auth: adminNote,
		},

		// --- Tasks: both read-only.
		methods.MethodTasksList: {
			Route: wildcardRoute(stream.TasksRoutePattern, "tasks.", methods.MethodTasksList), Mutates: false,
			Request: "TaskListRequest", Response: "TaskListResponse",
			CrossTenant: crossTenantAdminOnly,
		},
		methods.MethodTasksGet: {
			Route: wildcardRoute(stream.TasksRoutePattern, "tasks.", methods.MethodTasksGet), Mutates: false,
			Request: "TaskGetRequest", Response: "TaskDetail",
		},

		// --- Agents (reads +
		// fleet-control verbs).
		methods.MethodAgentsList:        agentsRead(methods.MethodAgentsList, "AgentListRequest", "AgentListResponse"),
		methods.MethodAgentsGet:         agentsRead(methods.MethodAgentsGet, "AgentGetRequest", "AgentGetResponse"),
		methods.MethodAgentsTools:       agentsRead(methods.MethodAgentsTools, "AgentToolsRequest", "AgentToolsResponse"),
		methods.MethodAgentsMemory:      agentsRead(methods.MethodAgentsMemory, "AgentMemoryRequest", "AgentMemoryResponse"),
		methods.MethodAgentsGovernance:  agentsRead(methods.MethodAgentsGovernance, "AgentGovernanceRequest", "AgentGovernanceResponse"),
		methods.MethodAgentsSkills:      agentsRead(methods.MethodAgentsSkills, "AgentSkillsRequest", "AgentSkillsResponse"),
		methods.MethodAgentsPermissions: agentsRead(methods.MethodAgentsPermissions, "AgentPermissionsRequest", "AgentPermissionsResponse"),
		methods.MethodAgentsMetrics:     agentsRead(methods.MethodAgentsMetrics, "AgentMetricsRequest", "AgentMetricsResponse"),
		methods.MethodAgentsPause:       agentsControl(methods.MethodAgentsPause),
		methods.MethodAgentsDrain:       agentsControl(methods.MethodAgentsDrain),
		methods.MethodAgentsRestart:     agentsControl(methods.MethodAgentsRestart),
		methods.MethodAgentsForceStop:   agentsControl(methods.MethodAgentsForceStop),
		methods.MethodAgentsDeregister:  agentsControl(methods.MethodAgentsDeregister),

		// --- Sessions: both read-only.
		methods.MethodSessionsList: {
			Route: subtreeRoute(stream.SessionsRoutePattern, "sessions.", methods.MethodSessionsList), Mutates: false,
			Request: "SessionsListRequest", Response: "SessionsListResponse",
			CrossTenant: crossTenantAdminOrFleet,
		},
		methods.MethodSessionsInspect: {
			Route: subtreeRoute(stream.SessionsRoutePattern, "sessions.", methods.MethodSessionsInspect), Mutates: false,
			Request: "SessionsInspectRequest", Response: "SessionsInspectResponse",
		},

		// --- Flows: five reads + the one admin run.
		methods.MethodFlowsList:         flowsRead(methods.MethodFlowsList, "FlowListRequest", "FlowListResponse", crossTenantAdminOnly),
		methods.MethodFlowsDescribe:     flowsRead(methods.MethodFlowsDescribe, "FlowDescribeRequest", "FlowDescription", crossTenantAdminWidens),
		methods.MethodFlowsRunsList:     flowsRead(methods.MethodFlowsRunsList, "FlowRunsListRequest", "FlowRunsListResponse", crossTenantAdminOnly),
		methods.MethodFlowsRunsDescribe: flowsRead(methods.MethodFlowsRunsDescribe, "FlowRunDescribeRequest", "FlowRunDescription", crossTenantAdminWidens),
		methods.MethodFlowsMetrics:      flowsRead(methods.MethodFlowsMetrics, "FlowMetricsRequest", "FlowMetrics", crossTenantAdminWidens),
		methods.MethodFlowsRun: {
			Route: subtreeRoute(stream.FlowsRoutePattern, "flows.", methods.MethodFlowsRun), Mutates: true,
			Request: "FlowRunRequest", Response: "FlowRunResponse",
			Auth: adminNote,
		},

		// --- Runs.
		methods.MethodRunsSetOverrides: {
			Route: subtreeRoute(stream.RunsRoutePattern, "runs.", methods.MethodRunsSetOverrides), Mutates: true,
			Request: "RunSetOverridesRequest", Response: "RunSetOverridesResponse",
		},

		// --- Auth.
		methods.MethodAuthRotateToken: {
			Route: wildcardRoute(stream.AuthRoutePattern, "auth.", methods.MethodAuthRotateToken), Mutates: true,
			Request: "AuthRotateTokenRequest", Response: "AuthRotateTokenResponse",
			Auth: adminNote,
		},
	}

	// The nine steering controls share one wire shape; their per-control
	// minimum steering scope comes from steering.RequiredScope so the
	// docs render the same RFC §6.3 binding the inbox enforces.
	for m, ct := range methodToControlType {
		scope, _ := steering.RequiredScope(ct)
		t[m] = methodEntry{
			Route: controlRoute(m), Mutates: true,
			Request: "ControlRequest", Response: "ControlResponse",
			Auth: fmt.Sprintf("steering scope ≥ `%s` (RFC §6.3); cross-tenant steering requires `admin`", scope),
		}
	}
	return t
}

// searchEntry builds the shared row shape for the five search.* methods.
func searchEntry(m methods.Method) methodEntry {
	return methodEntry{
		Route: controlRoute(m), Mutates: false,
		Request: "SearchRequest", Response: "SearchResponse",
		CrossTenant: crossTenantAdminOrFleet,
	}
}

// postureEntry builds the shared row shape for the seven read-only
// posture methods (all decode the one RuntimeInfoRequest envelope).
// Cross-tenant posture reads gate on `admin` OR `console:fleet`
// (internal/protocol/posture.go).
func postureEntry(m methods.Method, response string) methodEntry {
	return methodEntry{
		Route: controlRoute(m), Mutates: false,
		Request: "RuntimeInfoRequest", Response: response,
		CrossTenant: crossTenantAdminOrFleet,
	}
}

// mcpEntry builds a row for one mcp.servers.* method; admin selects the
// admin-verb posture.
func mcpEntry(m methods.Method, req, resp string, mutates bool) methodEntry {
	e := methodEntry{Route: controlRoute(m), Mutates: mutates, Request: req, Response: resp}
	if methods.IsMCPAdminMethod(m) {
		e.Auth = adminNote
	}
	return e
}

// agentsRead builds a read-only agents.* row. The registry protocol
// service overlays the verified identity onto every read — no
// cross-tenant fan-in exists under any scope, so the rows carry no
// elevated-scope note.
func agentsRead(m methods.Method, req, resp string) methodEntry {
	return methodEntry{
		Route: wildcardRoute(stream.AgentsRoutePattern, "agents.", m), Mutates: false,
		Request: req, Response: resp,
	}
}

// agentsControl builds a fleet-control agents.* row.
func agentsControl(m methods.Method) methodEntry {
	return methodEntry{
		Route: wildcardRoute(stream.AgentsRoutePattern, "agents.", m), Mutates: true,
		Request: "AgentControlRequest", Response: "AgentControlResponse",
		Auth: adminNote,
	}
}

// flowsRead builds a read-only flows.* row. ct is the per-method
// cross-tenant policy: `flows.list` / `flows.runs.list` carry the
// admin-only rejecting filter gate (internal/runtime/flow/protocol
// ErrCrossTenantScope); the describe/metrics reads have no rejecting
// filter shape — the admin claim widens run visibility instead.
func flowsRead(m methods.Method, req, resp string, ct crossTenantPolicy) methodEntry {
	return methodEntry{
		Route: subtreeRoute(stream.FlowsRoutePattern, "flows.", m), Mutates: false,
		Request: req, Response: resp,
		CrossTenant: ct,
	}
}

// classify renders the methods package's own predicate family into the
// classification cell — the docs publish the same routing taxonomy the
// transports branch on.
func classify(m methods.Method) string {
	switch {
	case m == methods.MethodStart:
		return "task control — spawn"
	case methods.IsControlMethod(m):
		return "task control — steering"
	case methods.IsStreamingEventsMethod(m):
		return "streaming events"
	case methods.IsSearchMethod(m):
		return "search"
	case methods.IsPostureMethod(m):
		return "posture (read-only)"
	case methods.IsPauseMethod(m):
		return "pause snapshot (read-only)"
	case methods.IsTopologyMethod(m):
		return "topology (read-only)"
	case methods.IsArtifactsMethod(m):
		return "artifacts"
	case methods.IsMemoryMethod(m):
		return "memory"
	case methods.IsMCPServersMethod(m):
		return "mcp servers"
	case methods.IsTasksMethod(m):
		return "tasks (read-only)"
	case methods.IsToolsMethod(m):
		return "tools"
	case methods.IsFlowsMethod(m):
		return "flows"
	case methods.IsAgentsControlMethod(m):
		return "agents — fleet control"
	case methods.IsAgentsMethod(m):
		return "agents (read-only)"
	case methods.IsSessionsMethod(m):
		return "sessions (read-only)"
	case methods.IsRunsMethod(m):
		return "runs"
	case methods.IsAuthMethod(m):
		return "auth"
	default:
		return "unclassified"
	}
}

// methodClusters fixes the rendering order: one H2 section per cluster,
// methods sorted lexicographically inside each. A method matching no
// cluster prefix lands in the renderer's error path (the lockstep test
// keeps that unreachable).
var methodClusters = []struct {
	Title  string
	Match  func(methods.Method) bool
	Anchor string
}{
	{"Task control", func(m methods.Method) bool { return m == methods.MethodStart || methods.IsControlMethod(m) }, "task-control"},
	{"Streaming events", methods.IsStreamingEventsMethod, "streaming-events"},
	{"Tasks", methods.IsTasksMethod, "tasks"},
	{"Sessions", methods.IsSessionsMethod, "sessions"},
	{"Pause snapshot", methods.IsPauseMethod, "pause-snapshot"},
	{"Search", methods.IsSearchMethod, "search"},
	{"Posture", methods.IsPostureMethod, "posture"},
	{"Topology", methods.IsTopologyMethod, "topology"},
	{"Artifacts", methods.IsArtifactsMethod, "artifacts"},
	{"Memory", methods.IsMemoryMethod, "memory"},
	{"Tools", methods.IsToolsMethod, "tools"},
	{"Flows", methods.IsFlowsMethod, "flows"},
	{"Agents", methods.IsAgentsMethod, "agents"},
	{"MCP servers", methods.IsMCPServersMethod, "mcp-servers"},
	{"Runs", methods.IsRunsMethod, "runs"},
	{"Auth", methods.IsAuthMethod, "auth"},
}

// renderMethodsPage emits methods.md: every methods.Methods() entry,
// grouped by cluster, with route / classification / wire types / auth
// posture per row.
func renderMethodsPage() (string, error) {
	table := methodTable()

	var b strings.Builder
	b.WriteString(generatedHeader + "\n\n")
	b.WriteString("# Protocol methods\n\n")
	fmt.Fprintf(&b, "The %d canonical Harbor Protocol methods, generated from the single-source registry\n", len(methods.Methods()))
	b.WriteString("(`internal/protocol/methods`) joined against the wire transports' route patterns\n")
	b.WriteString("(`internal/protocol/transports/{control,stream}`). The classification column is computed\n")
	b.WriteString("from the same `Is*Method` predicates the transports branch on.\n\n")
	b.WriteString("Every method is **identity-mandatory**: the request carries a verified JWT bearer\n")
	b.WriteString("(asymmetric algorithms only — RFC §5.5) and resolves a complete `(tenant, user, session)`\n")
	b.WriteString("triple, with the session chosen per-request via `X-Harbor-Session`\n")
	b.WriteString("(see [Auth & identity](./auth-and-identity.md)). The Auth column lists what a method\n")
	b.WriteString("demands **beyond** that baseline. Request/response shapes link into [types.md](./types.md);\n")
	b.WriteString("error envelopes are catalogued in [errors.md](./errors.md).\n")

	seen := make(map[methods.Method]bool, len(table))
	for _, cluster := range methodClusters {
		var members []methods.Method
		for _, m := range methods.Methods() {
			if cluster.Match(m) && !seen[m] {
				members = append(members, m)
				seen[m] = true
			}
		}
		if len(members) == 0 {
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })

		fmt.Fprintf(&b, "\n## %s\n\n", cluster.Title)
		b.WriteString("| Method | Route | Classification | Request | Response | Auth (beyond identity) |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, m := range members {
			e, ok := table[m]
			if !ok {
				return "", fmt.Errorf("method %q has no methodTable entry — extend the table (the lockstep test pins this)", m)
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s | %s |\n",
				m, e.Route, classify(m), requestCell(e), responseCell(e), authCell(e))
		}
	}

	// Exhaustiveness at render time, mirrored by the lockstep test: a
	// method outside every cluster would silently vanish from the page.
	for _, m := range methods.Methods() {
		if !seen[m] {
			return "", fmt.Errorf("method %q matched no cluster — extend methodClusters", m)
		}
	}

	return b.String(), nil
}

// requestCell renders the Request column for one row.
func requestCell(e methodEntry) string {
	if e.Request == "" {
		return e.RequestNote
	}
	return typeLink(e.Request)
}

// responseCell renders the Response column for one row.
func responseCell(e methodEntry) string {
	if e.Response == "" {
		return e.ResponseNote
	}
	return typeLink(e.Response)
}

// authCell renders the Auth column for one row: the read-only/mutating
// verb, the explicit Auth posture (admin verbs, steering scopes), and
// the cross-tenant note derived from the row's crossTenantPolicy.
func authCell(e methodEntry) string {
	verb := "read-only"
	if e.Mutates {
		verb = "mutating"
	}
	parts := []string{verb}
	if e.Auth != "" {
		parts = append(parts, e.Auth)
	}
	if note := crossTenantAuthNote(e.CrossTenant); note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, "; ")
}

// typeLink renders a wire-type name as a link into types.md.
func typeLink(name string) string {
	return fmt.Sprintf("[`%s`](./types.md#%s)", name, strings.ToLower(name))
}
