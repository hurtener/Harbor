package types

import "time"

// Phase 73c (Wave 13 / D-122) — the Console Sessions-page wire types.
//
// These structs are the single source of truth (D-002 / CLAUDE.md §13)
// for the two `sessions.*` Protocol methods the Console Sessions page
// consumes:
//
//   - sessions.list    — SessionsListRequest    → SessionsListResponse
//   - sessions.inspect — SessionsInspectRequest → SessionsInspectResponse
//
// The wire vocabulary is the Protocol's own (RFC §5.1 / CLAUDE.md §13
// single-source rule): the Console never reads a runtime-internal Go
// type. `sessions.Session` / `sessions.SessionSnapshot` are runtime
// concepts; the projector in `internal/sessions/protocol` maps them
// onto these flat wire shapes.
//
// Identity is mandatory on every request (RFC §5.5 / CLAUDE.md §6
// rule 9): a request whose embedded IdentityScope is incomplete fails
// closed at the wire edge with CodeIdentityRequired. A cross-tenant
// filter — a `TenantIDs` entry naming a tenant other than the caller's
// verified tenant — additionally requires the verified `auth.ScopeAdmin`
// claim (D-079 closed two-scope set; there is NO new sessions scope) —
// a missing claim fails closed with CodeScopeMismatch (HTTP 403).
//
// Carve-outs pinned by D-122:
//   - NO Priority field on SessionRow (D-065 dropped session-level
//     priority from V1).
//   - Saved filters are Console-local (D-061) — the wire shape carries
//     only the inflated filter, NEVER a saved-filter ID.
//   - SessionsListResponse emits `Truncated bool` rather than a silent
//     exact total — D-026 fail-loudly: an exact O(N) count under high
//     cardinality is not promised.

// Sessions-page pagination bounds for `sessions.list`. Mirrors the
// tools.list / pause.list / search.* contract so the shared Console
// pagination component is reused, not re-implemented per page. A
// request above MaxSessionListLimit gets a 400 (CodeInvalidRequest) —
// never a silent clamp.
const (
	// DefaultSessionListLimit is the page size applied when a
	// SessionsListRequest omits Limit (or passes a non-positive value).
	DefaultSessionListLimit = 50
	// MaxSessionListLimit bounds the page size a client may request.
	MaxSessionListLimit = 200
	// MaxSessionInterventionSummaries caps the RecentInterventions slice
	// the right-rail Session Summary projection carries.
	MaxSessionInterventionSummaries = 5
	// MaxSessionArtifactSummaries caps the RecentArtifacts slice the
	// right-rail Session Summary projection carries.
	MaxSessionArtifactSummaries = 5
)

// SessionStatus is the wire enum for a session's lifecycle status.
type SessionStatus string

// Canonical session statuses — the closed set the Sessions-page status
// facet filters on and the table renders.
const (
	// SessionStatusRunning — the session has at least one running run.
	SessionStatusRunning SessionStatus = "running"
	// SessionStatusPaused — the session has a paused run awaiting
	// resume / approval.
	SessionStatusPaused SessionStatus = "paused"
	// SessionStatusCompleted — the session is closed and finished
	// cleanly.
	SessionStatusCompleted SessionStatus = "completed"
	// SessionStatusFailed — the session is closed and at least one run
	// failed.
	SessionStatusFailed SessionStatus = "failed"
)

// IsValidSessionStatus reports whether s is one of the four canonical
// session statuses.
func IsValidSessionStatus(s SessionStatus) bool {
	switch s {
	case SessionStatusRunning, SessionStatusPaused,
		SessionStatusCompleted, SessionStatusFailed:
		return true
	}
	return false
}

// SessionSort is the wire enum for the `sessions.list` row ordering.
type SessionSort string

// Canonical session sort orders — the Sort-By toggle in the mockup.
const (
	// SessionSortStartedDesc orders newest-first by StartedAt (default).
	SessionSortStartedDesc SessionSort = "started_desc"
	// SessionSortStartedAsc orders oldest-first by StartedAt.
	SessionSortStartedAsc SessionSort = "started_asc"
	// SessionSortLastActivityDesc orders most-recently-active first.
	SessionSortLastActivityDesc SessionSort = "last_activity_desc"
	// SessionSortCostDesc orders most-expensive first.
	SessionSortCostDesc SessionSort = "cost_desc"
)

// IsValidSessionSort reports whether s is one of the four canonical
// sort orders. An empty value resolves to SessionSortStartedDesc.
func IsValidSessionSort(s SessionSort) bool {
	switch s {
	case SessionSortStartedDesc, SessionSortStartedAsc,
		SessionSortLastActivityDesc, SessionSortCostDesc:
		return true
	}
	return false
}

// Window is a half-open time range filter. Both bounds are optional;
// a nil bound means "unbounded on that side". A non-nil From / To
// filters sessions whose StartedAt falls inside [From, To].
type Window struct {
	// From is the inclusive lower bound; nil ⇒ unbounded below.
	From *time.Time `json:"from,omitempty"`
	// To is the inclusive upper bound; nil ⇒ unbounded above.
	To *time.Time `json:"to,omitempty"`
}

// SessionFilter is the server-enforced filter on `sessions.list`. An
// empty facet slice matches every value on that axis. The filter is
// applied AFTER the identity-scope predicate — it never widens
// visibility (CLAUDE.md §6).
type SessionFilter struct {
	// Statuses restricts to sessions whose Status is in this set.
	Statuses []SessionStatus `json:"statuses,omitempty"`
	// AgentIDs restricts to sessions whose AgentID is in this set.
	AgentIDs []string `json:"agent_ids,omitempty"`
	// UserIDs restricts to sessions whose UserID is in this set.
	UserIDs []string `json:"user_ids,omitempty"`
	// TenantIDs restricts to sessions whose TenantID is in this set.
	// A TenantIDs entry naming a tenant other than the caller's
	// verified tenant requires the `auth.ScopeAdmin` claim (D-079).
	TenantIDs []string `json:"tenant_ids,omitempty"`
	// StartedWindow filters by the session's StartedAt timestamp.
	StartedWindow Window `json:"started_window,omitempty"`
	// HasIntervention, when non-nil, restricts to sessions that do
	// (true) / do not (false) have a pending intervention.
	HasIntervention *bool `json:"has_intervention,omitempty"`
	// HasFailedTask, when non-nil, restricts to sessions that do
	// (true) / do not (false) have a failed task.
	HasFailedTask *bool `json:"has_failed_task,omitempty"`
	// CostAboveCents, when non-nil, restricts to sessions whose
	// TotalCostCents is strictly above this threshold.
	CostAboveCents *int64 `json:"cost_above_cents,omitempty"`
	// Query is a free-text substring filter over session id + agent
	// name + user. When non-empty, the runtime forwards it to the
	// `search.sessions` index (Brief 11 §CC-4) and the SessionFilter
	// axes are post-search refinements (D-122 — forward then filter).
	Query string `json:"query,omitempty"`
}

// SessionsListRequest is the `sessions.list` request body.
type SessionsListRequest struct {
	// Identity is the (tenant, user, session) scope the listing is
	// projected for. Mandatory — an incomplete triple fails closed.
	Identity IdentityScope `json:"identity"`
	// Filter is the optional facet filter; the zero value lists every
	// visible session.
	Filter SessionFilter `json:"filter"`
	// Sort selects the row ordering; an empty value applies
	// SessionSortStartedDesc.
	Sort SessionSort `json:"sort,omitempty"`
	// Cursor is the opaque pagination cursor returned by a previous
	// SessionsListResponse.NextCursor. Empty starts from the first page.
	Cursor string `json:"cursor,omitempty"`
	// Limit is the rows-per-page; a non-positive value applies
	// DefaultSessionListLimit. A value above MaxSessionListLimit is a
	// 400 (CodeInvalidRequest) — never a silent clamp.
	Limit int `json:"limit,omitempty"`
}

// SessionRow is the catalog-row projection of a session. It is the row
// shape the Console Sessions-page table renders. Flat, low-cardinality
// fields — the Console branches on the enum fields.
//
// There is NO Priority field — D-065 dropped session-level priority
// from V1.
type SessionRow struct {
	// SessionID is the stable session identifier.
	SessionID string `json:"session_id"`
	// Status is the session's lifecycle status.
	Status SessionStatus `json:"status"`
	// AgentID is the registered agent identifier the session ran under
	// ("" when no agent is bound).
	AgentID string `json:"agent_id"`
	// AgentName is the planner-facing agent display name.
	AgentName string `json:"agent_name"`
	// UserID is the user component of the session's identity triple.
	UserID string `json:"user_id"`
	// TenantID is the tenant component of the session's identity triple.
	TenantID string `json:"tenant_id"`
	// StartedAt is the session-open timestamp.
	StartedAt time.Time `json:"started_at"`
	// LastActivityAt is the timestamp of the most-recent activity.
	LastActivityAt time.Time `json:"last_activity_at"`
	// Duration is the elapsed span (LastActivityAt-StartedAt for closed
	// sessions, now-StartedAt for live ones). Marshalled as int64
	// nanoseconds (Go's time.Duration JSON form).
	Duration time.Duration `json:"duration"`
	// TasksCount is the number of tasks the session has spawned.
	TasksCount int `json:"tasks_count"`
	// EventsCount is the number of events the session has emitted.
	EventsCount int `json:"events_count"`
	// TotalCostCents is the session's accumulated LLM cost in US cents.
	TotalCostCents int64 `json:"total_cost_cents"`
	// TotalTokens is the session's accumulated LLM token count.
	TotalTokens int64 `json:"total_tokens"`
	// HasPendingIntervention reports whether the session has a pause
	// awaiting resume / approval.
	HasPendingIntervention bool `json:"has_pending_intervention"`
	// HasFailedTask reports whether the session has at least one failed
	// task.
	HasFailedTask bool `json:"has_failed_task"`
	// Identity is the impersonation triplet (Phase 72b / D-107). When
	// the session is a normal non-impersonated run, only the top-level
	// Tenant/User/Session carry meaning and Impersonating is nil; when
	// the session is an admin-initiated impersonated run, Actor carries
	// the verified admin identity and Impersonating carries the target
	// identity the run executed under. The Console Sessions-page
	// Identity column renders the Actor triple plus a separate
	// `impersonating` chip when Impersonating is non-empty.
	Identity IdentityScope `json:"identity"`
}

// SessionsListResponse is the `sessions.list` reply: a page of catalog
// rows plus the opaque next-page cursor.
//
// There is NO exact total count — D-026 fail-loudly: an exact O(N)
// count under high cardinality is not promised. The server emits
// `Truncated: true` when the candidate set hit Limit+1 rows; the page
// lazily fetches more pages until NextCursor == "".
type SessionsListResponse struct {
	// Rows is the page of session rows, in the requested sort order.
	Rows []SessionRow `json:"rows"`
	// NextCursor is the opaque cursor for the next page; "" when this
	// is the last page.
	NextCursor string `json:"next_cursor"`
	// Truncated is true when the candidate set hit Limit+1 rows — the
	// honest "there are more" signal, NEVER a silent exact total.
	Truncated bool `json:"truncated"`
}

// InterventionSummary is one entry in the right-rail Session Summary's
// Recent Interventions card. Capped at MaxSessionInterventionSummaries.
type InterventionSummary struct {
	// Type is the intervention kind — a HITL pause, a tool-approval
	// gate, or a tool-OAuth gate.
	Type string `json:"type"`
	// Reason is the operator-facing reason the intervention fired.
	Reason string `json:"reason"`
	// Outcome is the intervention's resolution — pending, resolved,
	// approved, or rejected.
	Outcome string `json:"outcome"`
	// OccurredAt is when the intervention fired.
	OccurredAt time.Time `json:"occurred_at"`
}

// ArtifactRefSummary is one entry in the right-rail Session Summary's
// Recent Artifacts card. Capped at MaxSessionArtifactSummaries. It
// carries metadata only — never inline bytes (D-026).
type ArtifactRefSummary struct {
	// Filename is the artifact's display filename.
	Filename string `json:"filename"`
	// MIME is the artifact's content type.
	MIME string `json:"mime"`
	// SizeBytes is the artifact's byte size.
	SizeBytes int64 `json:"size_bytes"`
	// CreatedAt is when the artifact was produced.
	CreatedAt time.Time `json:"created_at"`
}

// SessionsInspectRequest is the `sessions.inspect` request body.
type SessionsInspectRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// SessionID is the session to inspect.
	SessionID string `json:"session_id"`
}

// SessionsInspectResponse is the `sessions.inspect` reply — the full
// per-session snapshot the Console Sessions detail view renders. The
// Row carries the same projection as a `sessions.list` row; the two
// capped slices feed the right-rail Recent Interventions / Recent
// Artifacts cards.
type SessionsInspectResponse struct {
	// Row is the session's catalog-row projection (same shape as a
	// `sessions.list` row).
	Row SessionRow `json:"row"`
	// RecentInterventions is the capped slice of recent interventions
	// (≤ MaxSessionInterventionSummaries), most-recent first.
	RecentInterventions []InterventionSummary `json:"recent_interventions"`
	// RecentArtifacts is the capped slice of recent artifacts
	// (≤ MaxSessionArtifactSummaries), most-recent first.
	RecentArtifacts []ArtifactRefSummary `json:"recent_artifacts"`
}
