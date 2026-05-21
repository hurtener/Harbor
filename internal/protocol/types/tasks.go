package types

import "time"

// Phase 73d (Wave 13 / D-123) — the Console Tasks-page wire types.
//
// These structs are the single source of truth (D-002) for the two
// `tasks.*` read methods the Console Tasks page consumes:
//
//   - tasks.list — TaskListRequest → TaskListResponse
//   - tasks.get  — TaskGetRequest  → TaskDetail
//
// The wire vocabulary is the Protocol's own (RFC §5.1 / CLAUDE.md §13
// single-source rule): the Console never reads a runtime-internal Go
// type. `tasks.Task` / `tasks.TaskStatus` / `tasks.TaskKind` are
// runtime concepts; the projector in `internal/tasks/protocol` maps
// them onto these flat wire shapes. Keeping `internal/protocol/types`
// free of an `internal/tasks` import is deliberate — the Protocol layer
// owns its own vocabulary and a third-party Console implementation
// branches on the wire enums below, not on a Go-internal type.
//
// Identity is mandatory on every request (RFC §5.5 / CLAUDE.md §6
// rule 9): a request whose embedded IdentityScope is incomplete fails
// closed at the wire edge with CodeIdentityRequired. A cross-tenant
// `tasks.list` fan-in additionally requires the verified
// `auth.ScopeAdmin` claim (D-079); a cross-tenant `tasks.get` lookup
// returns CodeNotFound — existence is never revealed across tenants.
//
// The Console Tasks page consumes the EXISTING Phase 54 task-control
// verbs (`cancel` / `pause` / `resume` / `prioritize` / `approve` /
// `reject`) for mutation — there is NO `tasks.*` mutating method
// (CLAUDE.md §13 "no parallel implementations"). The two `tasks.*`
// methods here are pure reads.

// Tasks-page pagination bounds for `tasks.list`. Mirrors the
// tools.list / pause.list / search.* contract so a future shared
// Console-side pagination component is reused, not re-implemented per
// page. A request above MaxTaskListPageSize gets a 400
// (CodeInvalidRequest) — never a silent clamp.
const (
	// DefaultTaskListPageSize is the page size applied when a
	// TaskListRequest omits PageSize (or passes a non-positive value).
	DefaultTaskListPageSize = 50
	// MaxTaskListPageSize bounds the page size a client may request.
	MaxTaskListPageSize = 200
)

// TaskStatus is the wire enum for a task's lifecycle state. It is the
// Protocol projection of the runtime-internal `tasks.TaskStatus`.
type TaskStatus string

// Canonical task statuses — the closed lifecycle set the kanban board
// and list-mode table render.
const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusPaused    TaskStatus = "paused"
	TaskStatusComplete  TaskStatus = "complete"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
)

// IsValidTaskStatus reports whether s is one of the six canonical task
// statuses. `tasks.list` rejects a filter naming any other value with
// CodeInvalidRequest.
func IsValidTaskStatus(s TaskStatus) bool {
	switch s {
	case TaskStatusPending, TaskStatusRunning, TaskStatusPaused,
		TaskStatusComplete, TaskStatusFailed, TaskStatusCancelled:
		return true
	}
	return false
}

// TaskKind is the wire enum discriminating a foreground task (a run
// inside a session's primary turn) from a background task (a
// spawned-without-blocking task). The Protocol projection of the
// runtime-internal `tasks.TaskKind`.
type TaskKind string

// Canonical task kinds — the closed set.
const (
	TaskKindForeground TaskKind = "foreground"
	TaskKindBackground TaskKind = "background"
)

// IsValidTaskKind reports whether k is one of the two canonical task
// kinds.
func IsValidTaskKind(k TaskKind) bool {
	switch k {
	case TaskKindForeground, TaskKindBackground:
		return true
	}
	return false
}

// TaskRow is the compact projection returned by `tasks.list`. It is the
// row shape the Console Tasks-page kanban card + list-mode table
// render. Wire-only — never the internal `tasks.Task` struct
// (CLAUDE.md §8 — the Console never reads internal runtime objects).
// Heavy fields (the result / error payloads) stay on TaskDetail; the
// row is compact for kanban + table density.
//
// NO session-level priority field appears here — D-065 carve-out. Only
// Priority (the task-level priority shipped via the Phase 54
// `prioritize` method, D-072) is rendered.
type TaskRow struct {
	// ID is the unified task identifier (foreground run or background
	// task; one TaskID namespace).
	ID string `json:"id"`
	// Kind discriminates foreground vs background.
	Kind TaskKind `json:"kind"`
	// Status is the task's lifecycle state.
	Status TaskStatus `json:"status"`
	// Priority is the TASK-level priority (D-072 — the `prioritize`
	// control method). NEVER a session-level priority (D-065 carve-out).
	Priority int `json:"priority"`
	// Identity is the (tenant, user, session) triple the task runs
	// within — the Console truncates it for display on the card.
	Identity IdentityScope `json:"identity"`
	// ParentSessionID is the session the task belongs to.
	ParentSessionID string `json:"parent_session_id"`
	// ParentTaskID is set when the task was spawned by a planner
	// `SpawnTask` — it names the spawning task ("" when not a child).
	ParentTaskID string `json:"parent_task_id,omitempty"`
	// Description is the planner-facing task summary.
	Description string `json:"description"`
	// Query is the originating query / prompt text.
	Query string `json:"query"`
	// StartedAt is the task's creation timestamp.
	StartedAt time.Time `json:"started_at"`
	// UpdatedAt is the timestamp of the most recent lifecycle transition.
	UpdatedAt time.Time `json:"updated_at"`
	// DurationMS is the elapsed wall-clock time in milliseconds
	// (StartedAt → UpdatedAt).
	DurationMS int64 `json:"duration_ms"`
	// ErrorClass is the short failure-class string; set only when
	// Status is "failed" ("" otherwise).
	ErrorClass string `json:"error_class,omitempty"`
	// ToolCount is the count of child tool tasks spawned by this task.
	ToolCount int `json:"tool_count"`
	// BackgroundAcknowledged latches true once a completed background
	// task has been acknowledged (the `task.background_acknowledged`
	// event).
	BackgroundAcknowledged bool `json:"background_acknowledged"`
	// GroupID is the TaskGroup the task is a member of ("" when the
	// task is not a group member).
	GroupID string `json:"group_id,omitempty"`
	// Progress is the planner-emitted numeric progress hint in the
	// [0,1] range. It is a pointer so the wire shape distinguishes "the
	// planner emitted progress 0.0" from "the planner emitted no
	// progress at all" — nil ⇒ no hint, and the Console renders an
	// indeterminate Progress mini-bar rather than a 0% bar. Phase 73h
	// (D-128): the Background Jobs page's Progress column.
	Progress *float64 `json:"progress,omitempty"`
	// Tags is the slice of short labels the Background Jobs page renders
	// in its Tags column — the parent task type plus any planner-emitted
	// labels. Empty slice ⇒ no tags. Phase 73h (D-128).
	Tags []string `json:"tags,omitempty"`
	// LastActivityAt is the timestamp of the most recent activity on the
	// task — the max of UpdatedAt and any event on the run's stream. The
	// Background Jobs page's `Stuck > 1h` saved-filter chip derives off
	// this field (a Console-local rule — D-061). Phase 73h (D-128).
	LastActivityAt time.Time `json:"last_activity_at"`
	// IsBackground mirrors `Kind == "background"` — a convenience
	// boolean so a Console row-renderer branches without re-comparing
	// the enum. Phase 73h (D-128): the Background Jobs queue is the
	// `IsBackground == true` projection of the unified task set.
	IsBackground bool `json:"is_background"`
	// HasPendingApproval is true when the task's run has at least one
	// open HITL approval / tool-approval gate. The Background Jobs
	// page's `Has pending approval` facet filters on it, and the
	// per-job right-rail's Pending-approvals tab is populated when it is
	// true. Phase 73h (D-128).
	HasPendingApproval bool `json:"has_pending_approval"`
}

// TaskFilter is the server-enforced facet filter on `tasks.list`. An
// empty facet slice matches every value on that axis. The filter is
// applied AFTER the identity-scope predicate — it never widens
// visibility.
type TaskFilter struct {
	// Statuses restricts to tasks whose Status is in this set.
	Statuses []TaskStatus `json:"statuses,omitempty"`
	// Kinds restricts to tasks whose Kind is in this set; empty matches
	// both foreground and background.
	Kinds []TaskKind `json:"kinds,omitempty"`
	// ParentTaskID drills into a SpawnTask group — restricts to tasks
	// whose ParentTaskID equals this value ("" = no parent-task filter).
	ParentTaskID string `json:"parent_task_id,omitempty"`
	// Identities scopes the query. A query naming more than one distinct
	// tenant is a cross-tenant fan-in and requires the `auth.ScopeAdmin`
	// claim (D-079). Empty = the caller's own identity scope.
	Identities []IdentityScope `json:"identities,omitempty"`
	// Since is an optional lower bound on StartedAt (zero = unbounded).
	Since time.Time `json:"since,omitempty"`
	// Until is an optional upper bound on StartedAt (zero = unbounded).
	Until time.Time `json:"until,omitempty"`
	// ErrorClasses facets on TaskRow.ErrorClass for Failed tasks.
	ErrorClasses []string `json:"error_classes,omitempty"`
	// LatencyAboveMS facets on DurationMS — restricts to tasks whose
	// DurationMS is at or above this value (0 = no latency filter).
	LatencyAboveMS int64 `json:"latency_above_ms,omitempty"`
	// Search is a free-text substring filter over the task's
	// description + query, routed to the runtime search.tasks index.
	Search string `json:"search,omitempty"`
	// GroupID drills into one `TaskGroup` — restricts to tasks whose
	// GroupID equals this value ("" = no group filter). The Background
	// Jobs page's per-job right-rail "Related Sessions" tab issues a
	// `tasks.list` with this facet set to surface the sibling tasks
	// (foreground + background) under the same group. Identity scope is
	// still enforced — a cross-tenant `group_id` lookup never widens
	// visibility. Phase 73h (D-128).
	GroupID string `json:"group_id,omitempty"`
	// HasPendingApproval, when non-nil, restricts to tasks whose
	// HasPendingApproval row field equals the pointee. nil = no
	// approval filter; *true = only tasks with an open approval gate;
	// *false = only tasks with none. The Background Jobs page's `Has
	// pending approval` facet chip binds this. Phase 73h (D-128).
	HasPendingApproval *bool `json:"has_pending_approval,omitempty"`
}

// TaskListAggregates carries the per-status counters the kanban
// renders at each column header, computed over the FILTERED view.
type TaskListAggregates struct {
	// Pending is the count of filtered tasks in the Pending column.
	Pending int64 `json:"pending"`
	// Running is the count of filtered tasks in the Running column.
	Running int64 `json:"running"`
	// Paused is the count of filtered tasks in the Paused column.
	Paused int64 `json:"paused"`
	// Failed is the count of filtered tasks in the Failed column.
	Failed int64 `json:"failed"`
	// Complete is the count of filtered tasks in the Done column.
	Complete int64 `json:"complete"`
	// Cancelled is the count of filtered tasks that were cancelled.
	Cancelled int64 `json:"cancelled"`
}

// TaskListCursor is the opaque pagination cursor for `tasks.list`.
type TaskListCursor struct {
	// NextPageToken is the opaque continuation token; empty when there
	// are no more pages.
	NextPageToken string `json:"next_page_token,omitempty"`
}

// TasksListStatusCounterStrip is the Phase 73b (Wave 13 / D-126)
// status-counter-strip aggregate the Console Live Runtime page renders
// as its header-level five-chip strip (`pending / running / completed /
// paused / failed`). It is an OPT-IN projection on the `tasks.list`
// response — a caller requests it via TaskListRequest.IncludeStatusCounterStrip.
//
// The strip is distinct from TaskListAggregates in two deliberate ways:
//
//   - It is computed over the FULL identity-scoped task set, NOT the
//     filtered view. The Live Runtime header strip reports session-wide
//     posture ("how many tasks are running in this session right now"),
//     so it never narrows with the page's facet filter.
//   - It keys on the canonical lifecycle vocabulary the Live Runtime
//     page-spec mockup uses (`completed` rather than `complete`); the
//     `cancelled` status is folded out — the strip is a five-chip
//     present-tense posture, not the six-status kanban tally.
//
// The aggregate is identity-scoped and computed server-side per request
// (CLAUDE.md §6 rule 2): the counter NEVER crosses the isolation
// boundary — a second session never sees the first's counts.
type TasksListStatusCounterStrip struct {
	// Pending is the count of identity-scoped tasks in the Pending state.
	Pending int `json:"pending"`
	// Running is the count of identity-scoped tasks in the Running state.
	Running int `json:"running"`
	// Completed is the count of identity-scoped tasks in the Complete
	// state (the strip's `completed` chip — see the type godoc on the
	// `complete` → `completed` vocabulary choice).
	Completed int `json:"completed"`
	// Paused is the count of identity-scoped tasks in the Paused state.
	Paused int `json:"paused"`
	// Failed is the count of identity-scoped tasks in the Failed state.
	Failed int `json:"failed"`
}

// TaskListRequest is the `tasks.list` request body.
type TaskListRequest struct {
	// Identity is the (tenant, user, session) scope the task list is
	// projected for. Mandatory — an incomplete triple fails closed.
	Identity IdentityScope `json:"identity"`
	// Filter is the optional facet filter; the zero value lists every
	// visible task.
	Filter TaskFilter `json:"filter"`
	// PageSize is the rows-per-page; a non-positive value applies
	// DefaultTaskListPageSize. A value above MaxTaskListPageSize is a
	// 400 (CodeInvalidRequest) — never a silent clamp.
	PageSize int `json:"page_size,omitempty"`
	// Cursor is the pagination cursor; the zero value requests the
	// first page.
	Cursor TaskListCursor `json:"cursor"`
	// IncludeStatusCounterStrip opts the response into carrying the
	// Phase 73b (D-126) TasksListStatusCounterStrip aggregate. It is
	// off by default — the Console Live Runtime page sets it on its
	// initial-load `tasks.list` call and then maintains the strip live
	// from the `task.*` SSE stream (the aggregate is the initial-load
	// shape only; see the phase-73b plan's "tasks.list aggregate cost"
	// risk). The Tasks page (73d) never sets it — its kanban renders
	// the filtered-view TaskListAggregates instead.
	IncludeStatusCounterStrip bool `json:"include_status_counter_strip,omitempty"`
}

// TaskListResponse is the `tasks.list` reply: a paginated slice of
// task rows, the continuation cursor, and the filtered-view
// per-status aggregates.
type TaskListResponse struct {
	// Rows is the page of task rows, sorted by StartedAt descending
	// (newest first).
	Rows []TaskRow `json:"rows"`
	// Cursor carries the continuation token for the next page.
	Cursor TaskListCursor `json:"cursor"`
	// Aggregates carries the per-status counters for the filtered view.
	Aggregates TaskListAggregates `json:"aggregates"`
	// StatusCounterStrip carries the Phase 73b (D-126) header-strip
	// aggregate — non-nil ONLY when the request set
	// IncludeStatusCounterStrip. It is computed over the FULL identity-
	// scoped task set (not the filtered view) and is the Console Live
	// Runtime page's header five-chip strip. nil on a request that did
	// not opt in (the Tasks page never opts in).
	StatusCounterStrip *TasksListStatusCounterStrip `json:"status_counter_strip,omitempty"`
}

// TaskGetRequest is the `tasks.get` request body.
type TaskGetRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the task identifier to project.
	ID string `json:"id"`
}

// TaskParentSessionRef is the parent-session reference card `tasks.get`
// returns — the Console Tasks-page right-rail "Parent Session" card.
type TaskParentSessionRef struct {
	// SessionID is the parent session's identifier.
	SessionID string `json:"session_id"`
	// AgentName is the agent the session runs.
	AgentName string `json:"agent_name"`
	// Status is the parent session's lifecycle status.
	Status string `json:"status"`
	// StartedAt is the parent session's start timestamp.
	StartedAt time.Time `json:"started_at"`
	// LatestEventAt is the timestamp of the most recent event on the
	// parent session.
	LatestEventAt time.Time `json:"latest_event_at"`
}

// TaskParentTaskRef is the parent-task reference `tasks.get` returns
// when the task is a child spawned by a planner `SpawnTask`.
type TaskParentTaskRef struct {
	// TaskID is the spawning task's identifier.
	TaskID string `json:"task_id"`
	// Kind is the spawning task's kind.
	Kind TaskKind `json:"kind"`
	// Status is the spawning task's lifecycle status.
	Status TaskStatus `json:"status"`
}

// TaskCostStep is one planner-step entry in a TaskCostRollup — the
// aggregated cost of a single planner step within the task.
type TaskCostStep struct {
	// StepIndex is the 0-based planner-step index.
	StepIndex int `json:"step_index"`
	// Tokens is the total token count attributed to the step.
	Tokens int64 `json:"tokens"`
	// USD is the dollar cost attributed to the step.
	USD float64 `json:"usd"`
}

// TaskCostRollup is the per-task cost aggregation `tasks.get` returns —
// the Console Tasks-page right-rail "Cost Breakdown" card. The rollup
// carries only sums + a per-step breakdown; heavy per-event payloads
// stay on the event bus and are never inlined here.
type TaskCostRollup struct {
	// TotalTokens is the total token count (prompt + output).
	TotalTokens int64 `json:"total_tokens"`
	// PromptTokens is the prompt-side token count.
	PromptTokens int64 `json:"prompt_tokens"`
	// OutputTokens is the output-side token count.
	OutputTokens int64 `json:"output_tokens"`
	// USD is the total dollar cost.
	USD float64 `json:"usd"`
	// PerStep is the cost breakdown, one entry per planner step.
	PerStep []TaskCostStep `json:"per_step"`
}

// TaskPlannerSnapshotRef is the planner-checkpoint reference `tasks.get`
// returns — it points at the planner checkpoint that existed at task
// spawn time. Heavy checkpoint content is fetched on demand via the
// existing Phase 73 `state.load_planner_checkpoint` method, never
// inlined in the `tasks.get` response.
type TaskPlannerSnapshotRef struct {
	// CheckpointID is the planner-checkpoint identifier (resolvable via
	// state.load_planner_checkpoint).
	CheckpointID string `json:"checkpoint_id"`
	// Summary is the pre-truncated checkpoint summary.
	Summary string `json:"summary"`
}

// TaskDetail is the enriched payload `tasks.get` returns. It carries
// the compact TaskRow projection plus the four enrichment fields the
// Console Tasks-page detail tabs + right-rail cards render. Heavy
// payload content is referenced via ArtifactRef (D-026) — `tasks.get`
// MUST NOT inline bytes that exceed the heavy-content threshold.
type TaskDetail struct {
	// Task is the compact row projection (same shape `tasks.list`
	// returns).
	Task TaskRow `json:"task"`
	// ParentSession is the parent-session reference card.
	ParentSession TaskParentSessionRef `json:"parent_session"`
	// ParentTask is the parent-task reference; nil when the task is not
	// a child of a SpawnTask group.
	ParentTask *TaskParentTaskRef `json:"parent_task,omitempty"`
	// Cost is the per-task cost rollup aggregated from
	// `llm.cost.recorded` events scoped to the task.
	Cost TaskCostRollup `json:"cost"`
	// PlannerSnapshot is the planner-checkpoint reference at spawn time;
	// nil when no checkpoint exists.
	PlannerSnapshot *TaskPlannerSnapshotRef `json:"planner_snapshot,omitempty"`
	// ResultRef references the task's result payload by artifact stub
	// when the result exceeds the heavy-content threshold (D-026); nil
	// when the task has no result or the result is inlined small.
	ResultRef *ArtifactRef `json:"result_ref,omitempty"`
	// ResultInline carries the task's result payload directly when it is
	// below the heavy-content threshold ("" when absent or referenced).
	ResultInline string `json:"result_inline,omitempty"`
}
