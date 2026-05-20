package types

import "time"

// Flows-page wire types (Phase 73i / D-117). These are the canonical
// Protocol projections the Console Flows page renders — the catalog
// row, the engine-graph description, the run-history rows, the per-run
// timeline, the run-invocation request, and the sparkline-metrics
// aggregate. They are NOT re-exports of any runtime Go type: the
// Runtime constructs each from its private flow registry + run-history
// store at request time, and the Console never sees those internals
// (CLAUDE.md §8; RFC §5.1 — a Protocol type that mapped 1:1 onto an
// internal Go struct would be the reject-on-sight smell).
//
// The Flows page is view-only at V1 (D-063): six read methods + one
// run method. There is NO authoring surface — `flows.run` is the only
// mutating method, and it is gated on identity + the appropriate scope
// claim (D-079). Heavy run outputs are shipped by-reference via
// `FlowArtifactRef` (D-026); the page never inlines heavy bytes.

// Flows-page pagination bounds. They mirror the `pause.list` /
// `search.*` pagination contract so the Console-side pagination
// component is shared across pages, not re-implemented per-method. A
// client that omits Page / PageSize gets the documented defaults; a
// request above the max gets a 400 (CodeInvalidRequest) — never a
// silent clamp.
const (
	// DefaultFlowListPageSize is the page size applied when a
	// `flows.list` / `flows.runs.list` request omits PageSize.
	DefaultFlowListPageSize = 50
	// MaxFlowListPageSize is the upper bound on a Flows-page request's
	// PageSize. A request above this is rejected with CodeInvalidRequest.
	MaxFlowListPageSize = 200
)

// FlowBudget is the wire projection of a flow's per-flow Budget (D-023
// — Flow-as-Tool registration, Phase 26a). It mirrors the three caps a
// flow's aggregate `Budget` enforces at the flow boundary: a wall-clock
// deadline, a hop (request) cap, and a cost cap. It is a flat wire type
// — the Protocol owns its vocabulary; the runtime `flow.Budget` struct
// never leaks. Edit of the Budget is `flows.set_budget` — post-V1 per
// page-flows.md §10; at V1 the Budget is read-only.
type FlowBudget struct {
	// DeadlineMS is the flow's wall-clock deadline in milliseconds. Zero
	// means "no deadline cap".
	DeadlineMS int64 `json:"deadline_ms,omitempty"`
	// RequestCap is the flow's hop budget — the maximum number of node
	// hops a single invocation may take. Zero means "no hop cap".
	RequestCap int `json:"request_cap,omitempty"`
	// CostCapUSD is the flow's aggregate cost cap in US dollars. Zero
	// means "no cost cap".
	CostCapUSD float64 `json:"cost_cap_usd,omitempty"`
	// TokenCap is the flow's aggregate token cap. Zero means "no token
	// cap". The token cap is a post-V1 axis on the runtime `Budget`;
	// the field exists on the wire so the Budget meter can render it
	// without a wire-shape break when the axis lands.
	TokenCap int64 `json:"token_cap,omitempty"`
}

// FlowBudgetConsumption is the live consumption of a flow's Budget vs.
// its caps, derived from the flow's run history within the active
// window. It is the data the Budget meter (right rail) renders as
// progress bars. Consumption is observation-only — there is no edit
// surface at V1.
type FlowBudgetConsumption struct {
	// RequestsUsed is the hop count consumed in the active window.
	RequestsUsed int `json:"requests_used"`
	// CostUSDUsed is the cost consumed in the active window.
	CostUSDUsed float64 `json:"cost_usd_used"`
	// TokensUsed is the token count consumed in the active window.
	TokensUsed int64 `json:"tokens_used"`
}

// Flow is one row of the `flows.list` catalog: a registered engine-graph
// flow with its aggregate run metrics over the active window. A "flow"
// in the Console is exactly an engine node graph a graph-family planner
// runs on (D-063); this row is the catalog lens over it.
type Flow struct {
	// ID is the flow's stable identifier — the registered flow name.
	ID string `json:"id"`
	// Name is the operator-facing display name. Equal to ID at V1; the
	// field exists so a future rename surface does not break the wire.
	Name string `json:"name"`
	// Owner is the flow's declared owner (the agent / team that
	// registered it). Empty when the registration carried no owner.
	Owner string `json:"owner,omitempty"`
	// Version is the flow's version string. Empty when unversioned.
	Version string `json:"version,omitempty"`
	// PlannerFamily is the graph-family planner the flow runs on —
	// "graph" / "workflow" / "deterministic". The catalog is filtered
	// to graph-family planners (D-063).
	PlannerFamily string `json:"planner_family,omitempty"`
	// NodeCount is the number of nodes in the flow's engine graph.
	NodeCount int `json:"node_count"`
	// EdgeCount is the number of directed edges in the flow's graph.
	EdgeCount int `json:"edge_count"`
	// Runs24h is the count of invocations in the trailing 24h window
	// scoped to the caller's identity (admin fans across tenants).
	Runs24h int64 `json:"runs_24h"`
	// P50LatencyMS is the median run latency over the window, in ms.
	P50LatencyMS int64 `json:"p50_latency_ms"`
	// P95LatencyMS is the 95th-percentile run latency over the window.
	P95LatencyMS int64 `json:"p95_latency_ms"`
	// SuccessRate is the fraction of runs in the window that succeeded,
	// in [0,1].
	SuccessRate float64 `json:"success_rate"`
	// LastRun is the wall-clock time of the most recent invocation. The
	// zero value (omitted) means the flow has never run.
	LastRun time.Time `json:"last_run,omitempty"`
	// Budget is the flow's per-flow Budget (D-023). Read-only at V1.
	Budget FlowBudget `json:"budget"`
}

// FlowFilter narrows the `flows.list` catalog. An empty filter means
// "the caller's own identity scope". Supplying a `Tenants` value that
// reaches OUTSIDE the caller's own tenant (or naming more than one
// tenant) requires the `auth.ScopeAdmin` scope claim (D-079); a
// missing-claim cross-tenant request is rejected loudly with
// CodeIdentityScopeRequired (HTTP 403) — never silently downgraded.
type FlowFilter struct {
	// Tenants restricts the catalog to the named tenants. Empty means
	// "the caller's own tenant". A value reaching outside the caller's
	// tenant requires the admin scope claim.
	Tenants []string `json:"tenants,omitempty"`
	// PlannerFamilies restricts the catalog to the named planner
	// families. Empty means "all graph-family planners".
	PlannerFamilies []string `json:"planner_families,omitempty"`
	// Query is a free-text substring match over the flow name / owner.
	// Empty means "no text filter".
	Query string `json:"query,omitempty"`
}

// FlowListRequest is the wire request for `flows.list`.
type FlowListRequest struct {
	// Identity is the mandatory caller identity scope. An incomplete
	// triple fails the request closed with CodeIdentityRequired.
	Identity IdentityScope `json:"identity"`
	// Filter narrows the catalog. The zero value lists the caller's
	// own-tenant flows.
	Filter FlowFilter `json:"filter"`
	// Page is the 1-based page index. Zero is treated as page 1.
	Page int `json:"page,omitempty"`
	// PageSize is the per-page row count. Zero applies
	// DefaultFlowListPageSize; a value above MaxFlowListPageSize is
	// rejected with CodeInvalidRequest.
	PageSize int `json:"page_size,omitempty"`
}

// FlowListResponse is the wire response for `flows.list`.
type FlowListResponse struct {
	// Flows is the catalog page, sorted lexicographically by ID for
	// byte-stability across requests.
	Flows []Flow `json:"flows"`
	// Page is the 1-based page index this response covers.
	Page int `json:"page"`
	// PageSize is the per-page row count applied.
	PageSize int `json:"page_size"`
	// PageCount is the total number of pages for the filtered set.
	PageCount int `json:"page_count"`
	// TotalRows is the total number of catalog rows the filter matched
	// before pagination.
	TotalRows int `json:"total_rows"`
}

// FlowNodeKind tags a node's role in the flow's engine graph. The V1
// set is closed — a node is a subflow, a tool invocation, a pause
// point, or an artifact emitter.
type FlowNodeKind string

// The V1 flow-node-kind constants.
const (
	// FlowNodeSubflow tags a node that runs a nested flow.
	FlowNodeSubflow FlowNodeKind = "subflow"
	// FlowNodeTool tags a node that invokes a registered Tool.
	FlowNodeTool FlowNodeKind = "tool"
	// FlowNodePause tags a node that parks the run at a pause point.
	// The wire value is the two-word form pause_point — deliberately
	// distinct from the Protocol method-name vocabulary so the Phase 58
	// single-source checker never flags this enum value.
	FlowNodePause FlowNodeKind = "pause_point"
	// FlowNodeArtifactEmitter tags a node that emits an artifact.
	FlowNodeArtifactEmitter FlowNodeKind = "artifact_emitter"
)

// FlowNodePolicy is the wire projection of a node's per-node policy
// (the runtime `engine.NodePolicy` — retry / timeout). It is a flat
// wire type; the runtime struct never leaks.
type FlowNodePolicy struct {
	// MaxRetries is the per-node retry ceiling.
	MaxRetries int `json:"max_retries,omitempty"`
	// TimeoutMS is the per-node wall-clock timeout in milliseconds.
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// FlowNode is one vertex of a flow's projected engine graph.
type FlowNode struct {
	// ID is the node's unique identifier within the flow.
	ID string `json:"id"`
	// Type is the node's role tag — subflow / tool / pause /
	// artifact_emitter.
	Type FlowNodeKind `json:"type"`
	// Descriptor is a schema reference for the node — the tool name for
	// a tool node, the nested flow id for a subflow node. It is a string
	// reference, never executable code.
	Descriptor string `json:"descriptor,omitempty"`
	// Policy is the node's per-node policy. Nil when the node carries
	// the engine default policy.
	Policy *FlowNodePolicy `json:"policy,omitempty"`
}

// FlowEdge is one directed edge of a flow's projected engine graph.
type FlowEdge struct {
	// From is the upstream node's ID.
	From string `json:"from"`
	// To is the downstream node's ID.
	To string `json:"to"`
}

// FlowDescription is the wire projection of a flow's full engine-graph
// description — the `flows.describe` payload. It carries the catalog
// row plus the node / edge set plus a string source reference. The
// source reference is a Go path or a YAML descriptor path (D-023: Go-
// coded V1; declarative YAML in V1.1) — it is NEVER executable code in
// the Console.
type FlowDescription struct {
	// Flow is the catalog row for the described flow.
	Flow Flow `json:"flow"`
	// Nodes is the flow's node set, sorted lexicographically by ID.
	Nodes []FlowNode `json:"nodes"`
	// Edges is the flow's directed edge set, sorted by (From, To).
	Edges []FlowEdge `json:"edges"`
	// Source is the source-of-truth reference — a Go path or YAML
	// descriptor path. A string reference, never executable code.
	Source string `json:"source,omitempty"`
	// BudgetConsumption is the flow's live Budget consumption over the
	// active window — the data the Budget meter renders.
	BudgetConsumption FlowBudgetConsumption `json:"budget_consumption"`
}

// FlowDescribeRequest is the wire request for `flows.describe`.
type FlowDescribeRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// ID is the flow to describe. An unknown id fails with CodeNotFound.
	ID string `json:"id"`
}

// FlowRunStatus is the typed enum of flow-run outcomes. The V1 set is
// closed — a run is still running, succeeded, failed, or was cancelled.
type FlowRunStatus string

// The V1 flow-run-status constants.
const (
	// FlowRunRunning tags a run still in flight.
	FlowRunRunning FlowRunStatus = "running"
	// FlowRunSucceeded tags a run that completed without error.
	FlowRunSucceeded FlowRunStatus = "succeeded"
	// FlowRunFailed tags a run that terminated with an error.
	FlowRunFailed FlowRunStatus = "failed"
	// FlowRunCancelled tags a run that was cancelled before completion.
	FlowRunCancelled FlowRunStatus = "cancelled"
)

// FlowRunTrigger tags what initiated a flow run.
type FlowRunTrigger string

// The V1 flow-run-trigger constants.
const (
	// FlowTriggerUser tags a run initiated by an operator via
	// `flows.run`.
	FlowTriggerUser FlowRunTrigger = "user"
	// FlowTriggerPlanner tags a run initiated by a planner step.
	FlowTriggerPlanner FlowRunTrigger = "planner"
	// FlowTriggerSystem tags a run initiated by the runtime itself.
	FlowTriggerSystem FlowRunTrigger = "system"
)

// FlowRun is one row of the `flows.runs.list` history: a single
// invocation of a flow with timing + outcome.
type FlowRun struct {
	// RunID is the run's stable identifier.
	RunID string `json:"run_id"`
	// FlowID is the flow this run executed.
	FlowID string `json:"flow_id"`
	// Status is the run's outcome.
	Status FlowRunStatus `json:"status"`
	// Trigger is what initiated the run.
	Trigger FlowRunTrigger `json:"trigger"`
	// StartedAt is the wall-clock time the run started.
	StartedAt time.Time `json:"started_at"`
	// DurationMS is the run's wall-clock duration in milliseconds. Zero
	// for a run still in flight.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// CostUSD is the run's recorded cost in US dollars.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Identity is the (tenant, user, session) the run executed under.
	Identity IdentityScope `json:"identity"`
	// ErrorClass is a short classification of the failure for a failed
	// run. Empty for a non-failed run.
	ErrorClass string `json:"error_class,omitempty"`
}

// FlowRunsListRequest is the wire request for `flows.runs.list`.
type FlowRunsListRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// FlowID is the flow whose run history to list. An empty FlowID
	// fails with CodeInvalidRequest.
	FlowID string `json:"flow_id"`
	// Tenants restricts the history to the named tenants. Empty means
	// "the caller's own tenant". A value reaching outside the caller's
	// tenant requires the admin scope claim (D-079).
	Tenants []string `json:"tenants,omitempty"`
	// Page is the 1-based page index. Zero is treated as page 1.
	Page int `json:"page,omitempty"`
	// PageSize is the per-page row count. Zero applies
	// DefaultFlowListPageSize.
	PageSize int `json:"page_size,omitempty"`
}

// FlowRunsListResponse is the wire response for `flows.runs.list`.
type FlowRunsListResponse struct {
	// Runs is the run-history page, sorted by StartedAt descending
	// (newest first).
	Runs []FlowRun `json:"runs"`
	// Page is the 1-based page index this response covers.
	Page int `json:"page"`
	// PageSize is the per-page row count applied.
	PageSize int `json:"page_size"`
	// PageCount is the total number of pages.
	PageCount int `json:"page_count"`
	// TotalRows is the total number of run rows the filter matched.
	TotalRows int `json:"total_rows"`
}

// FlowArtifactRef is the by-reference shape a `FlowRunDescription`
// carries when a run's final output meets or exceeds the configured
// heavy-content threshold (D-026). It mirrors a subset of
// `internal/artifacts.ArtifactRef` but is a flat wire type, kept
// distinct from `PauseArtifactRef` / `SearchArtifactRef` so a future
// divergence in either surface does not whipsaw the other.
type FlowArtifactRef struct {
	// ID is the content-addressed identifier.
	ID string `json:"id"`
	// MimeType is the IANA media type, when known.
	MimeType string `json:"mime_type,omitempty"`
	// SizeBytes is the length of the referenced bytes.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Filename is metadata only (never used for path construction).
	Filename string `json:"filename,omitempty"`
	// SHA256 is the full hex digest of the referenced bytes.
	SHA256 string `json:"sha256,omitempty"`
}

// FlowNodeRunState is one node's slice of a run's per-node timeline.
type FlowNodeRunState struct {
	// NodeID is the node's identifier within the flow.
	NodeID string `json:"node_id"`
	// Status is the node's outcome within the run.
	Status FlowRunStatus `json:"status"`
	// DurationMS is the node's wall-clock duration in milliseconds.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Retries is the number of times the node was retried.
	Retries int `json:"retries,omitempty"`
	// ErrorClass is a short classification of a node failure. Empty for
	// a non-failed node.
	ErrorClass string `json:"error_class,omitempty"`
}

// FlowRunDescription is the wire projection of a single flow run's
// per-node timeline + final-output reference — the `flows.runs.describe`
// payload. Heavy outputs are shipped by-reference via OutputRef
// (D-026); the description NEVER inlines heavy bytes.
type FlowRunDescription struct {
	// Run is the run-history row for the described run.
	Run FlowRun `json:"run"`
	// NodeStates is the per-node execution timeline, in node order.
	NodeStates []FlowNodeRunState `json:"node_states"`
	// OutputPreview is the run's final output INLINE when its serialised
	// size is below the heavy-content threshold. Empty when the run
	// produced no output or when the output was routed by-reference.
	OutputPreview string `json:"output_preview,omitempty"`
	// OutputRef is populated when the run's final output exceeded the
	// heavy-content threshold (D-026). The Console fetches the bytes via
	// `artifacts.get` when it wants them. When OutputRef is set,
	// OutputPreview is empty.
	OutputRef *FlowArtifactRef `json:"output_ref,omitempty"`
}

// FlowRunDescribeRequest is the wire request for `flows.runs.describe`.
type FlowRunDescribeRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// RunID is the run to describe. An unknown id fails with
	// CodeNotFound.
	RunID string `json:"run_id"`
}

// FlowRunRequest is the wire request for `flows.run` — a one-shot
// invocation of a registered flow. `flows.run` is the ONLY mutating
// Flows-page method; it is gated on identity + the appropriate scope
// claim (D-079).
type FlowRunRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// FlowID is the flow to invoke. An empty / unknown id fails with
	// CodeInvalidRequest / CodeNotFound.
	FlowID string `json:"flow_id"`
	// Inputs is the hand-crafted input form for the invocation. The
	// runtime validates it against the flow's input schema.
	Inputs map[string]any `json:"inputs,omitempty"`
}

// FlowRunResponse is the wire response for `flows.run` — the accepted
// run's identifier so the Console can drill into the run as it
// progresses.
type FlowRunResponse struct {
	// RunID is the identifier of the accepted run.
	RunID string `json:"run_id"`
	// Status is the run's status at acceptance — "running" for an
	// accepted run.
	Status FlowRunStatus `json:"status"`
	// StartedAt is the wall-clock time the run started.
	StartedAt time.Time `json:"started_at"`
}

// FlowMetricsBucket is one time bucket of a flow's sparkline metrics.
type FlowMetricsBucket struct {
	// BucketStart is the wall-clock start of the bucket.
	BucketStart time.Time `json:"bucket_start"`
	// Runs is the count of runs that started within the bucket.
	Runs int64 `json:"runs"`
	// P95LatencyMS is the 95th-percentile run latency within the bucket.
	P95LatencyMS int64 `json:"p95_latency_ms"`
	// SuccessRate is the fraction of bucketed runs that succeeded.
	SuccessRate float64 `json:"success_rate"`
	// CostUSD is the total cost recorded within the bucket.
	CostUSD float64 `json:"cost_usd"`
}

// FlowMetrics is the wire projection of a flow's sparkline aggregates —
// the `flows.metrics` payload. The buckets feed the Flow Metrics card
// (runs-per-hour, p95 latency, success rate, budget consumption).
type FlowMetrics struct {
	// FlowID is the flow these metrics describe.
	FlowID string `json:"flow_id"`
	// WindowStart is the wall-clock start of the aggregation window.
	WindowStart time.Time `json:"window_start"`
	// WindowEnd is the wall-clock end of the aggregation window.
	WindowEnd time.Time `json:"window_end"`
	// Buckets are the time-bucketed aggregates, oldest first.
	Buckets []FlowMetricsBucket `json:"buckets"`
	// BudgetConsumption is the flow's live Budget consumption over the
	// window — the data the budget sparkline renders.
	BudgetConsumption FlowBudgetConsumption `json:"budget_consumption"`
}

// FlowMetricsRequest is the wire request for `flows.metrics`.
type FlowMetricsRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// FlowID is the flow whose metrics to aggregate. An empty FlowID
	// fails with CodeInvalidRequest.
	FlowID string `json:"flow_id"`
	// WindowMS is the aggregation window in milliseconds. Zero applies
	// the default 24h window.
	WindowMS int64 `json:"window_ms,omitempty"`
	// BucketMS is the bucket width in milliseconds. Zero applies the
	// default 1h bucket.
	BucketMS int64 `json:"bucket_ms,omitempty"`
}

// IsValidFlowRunStatus reports whether s is one of the four canonical
// flow-run statuses.
func IsValidFlowRunStatus(s FlowRunStatus) bool {
	switch s {
	case FlowRunRunning, FlowRunSucceeded, FlowRunFailed, FlowRunCancelled:
		return true
	}
	return false
}

// IsValidFlowNodeKind reports whether k is one of the four canonical
// flow-node kinds.
func IsValidFlowNodeKind(k FlowNodeKind) bool {
	switch k {
	case FlowNodeSubflow, FlowNodeTool, FlowNodePause, FlowNodeArtifactEmitter:
		return true
	}
	return false
}
