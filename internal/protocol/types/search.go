package types

import (
	"encoding/json"
	"time"
)

// SearchIndex is the typed enum of canonical search indexes the
// runtime-side `search.*` cluster ships in Phase 72c. Four indexes —
// sessions, tasks, events, artifacts — match the high-cardinality
// runtime-side split from Brief 11 §CC-4. Console-side Tools / Agents /
// Flows / MCP catalog searches do NOT appear here: those land in their
// per-page Stage-2 Console phases (73c/d/e/f/g/i/k) per the split.
type SearchIndex string

// Canonical search-index values. The set is closed; a new index is a
// new Protocol-surface phase.
const (
	SearchIndexSessions  SearchIndex = "sessions"
	SearchIndexTasks     SearchIndex = "tasks"
	SearchIndexEvents    SearchIndex = "events"
	SearchIndexArtifacts SearchIndex = "artifacts"
)

// IsValidSearchIndex reports whether i is one of the four canonical
// runtime-side indexes.
func IsValidSearchIndex(i SearchIndex) bool {
	switch i {
	case SearchIndexSessions, SearchIndexTasks, SearchIndexEvents, SearchIndexArtifacts:
		return true
	}
	return false
}

// SearchFilter narrows results to the caller's identity scope and
// optional time-window. The TenantIDs / UserIDs / SessionIDs fields
// default to the caller's authenticated triple; supplying values that
// reach OUTSIDE the caller's own scope requires the `auth.ScopeAdmin`
// scope claim (D-079 closed two-scope set; D-108 reuse for search). A
// missing-claim cross-tenant request is rejected loudly with
// `CodeAuthRejected` (HTTP 403) — NEVER silently downgraded to an
// empty result set.
type SearchFilter struct {
	TenantIDs  []string  `json:"tenant_ids,omitempty"`
	UserIDs    []string  `json:"user_ids,omitempty"`
	SessionIDs []string  `json:"session_ids,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Until      time.Time `json:"until,omitempty"`
}

// SearchFacet is a per-index dimension selector — e.g.
// `{Key:"tasks.status", Value:"running"}` or
// `{Key:"events.type", Value:"tool.failed"}`. Unknown facets are
// silently ignored at V1 (post-V1 may tighten to error).
type SearchFacet struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// SearchRequest is the shared wire shape for all five `search.*`
// methods. `search.query` honours `Indexes` (the palette dispatcher
// selects 1..4 of the runtime-side indexes); the four per-index methods
// ignore `Indexes` and operate on their own index.
//
// Identity is mandatory at the Protocol edge per RFC §5.5 — the request
// flows out of an auth-verified identity in ctx, never trusted from the
// body. The `Filter` field supplies optional scope-narrowing within the
// caller's verified identity (and, with the admin claim, cross-tenant
// expansion).
type SearchRequest struct {
	// Query is the free-text query — substring / prefix match against
	// the searched fields per index. Empty Query is permitted (lists
	// everything in scope subject to filters).
	Query string `json:"query,omitempty"`
	// Indexes selects which runtime-side indexes `search.query` should
	// fan out to. Empty means "all four." Ignored by the four per-index
	// methods. Unknown index values are rejected with
	// `CodeInvalidRequest`.
	Indexes []SearchIndex `json:"indexes,omitempty"`
	// Filter narrows results. Identity defaulting + cross-tenant gating
	// happens in the search handler — the wire surface accepts every
	// caller's filter and rejects elevated requests loudly.
	Filter SearchFilter `json:"filter,omitempty"`
	// Facets are optional per-index dimension selectors. The runtime
	// applies whichever facets are recognised for the index being
	// searched and silently ignores the rest.
	Facets []SearchFacet `json:"facets,omitempty"`
	// Page is the 1-based page number; defaults to 1 when zero.
	Page int `json:"page,omitempty"`
	// PageSize is the per-page row count. Defaults to
	// `DefaultSearchPageSize` (20) when zero; values above
	// `MaxSearchPageSize` (200) are rejected with `CodeInvalidRequest`.
	PageSize int `json:"page_size,omitempty"`
}

// Default + maximum pagination bounds, shared by every `search.*`
// method per the Phase 72c plan acceptance criteria. The defaults are
// the wire contract — a client that omits Page / PageSize gets the
// documented defaults; a client requesting more than the max gets a
// 400.
const (
	DefaultSearchPageSize = 20
	MaxSearchPageSize     = 200
)

// SearchArtifactRef is the by-reference shape a `SearchResultRow`
// carries when the underlying entity's preview payload would exceed
// the heavy-content threshold (D-026). It mirrors a subset of
// `internal/artifacts.ArtifactRef` but is a flat wire type — the
// Protocol owns its vocabulary; runtime Go structs never leak (RFC
// §5.1 / CLAUDE.md §13 single-source rule).
type SearchArtifactRef struct {
	// ID is the content-addressed identifier (`{namespace}_{sha256[:12]}`).
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

// SearchResultRow is the uniform result-row shape across all five
// `search.*` methods. `Preview` is REDACTED via `audit.Redactor` before
// emission. Heavy payloads (≥ D-026 threshold) ship as a populated
// `Ref` field, NEVER inline bytes — `Preview` is then empty.
type SearchResultRow struct {
	// Index identifies which subsystem produced the row. Required.
	Index SearchIndex `json:"index"`
	// ID is the entity identifier (session ID / task ID / event ID
	// (composed `<session>:<sequence>`) / artifact ID).
	ID string `json:"id"`
	// TenantID + UserID + SessionID + RunID flatten the runtime's
	// identity quadruple so a Protocol client never round-trips a
	// runtime Go struct. RunID is empty for session-scoped entities.
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	// OccurredAt is the row's anchor timestamp — for sessions, the
	// open / last-seen / close time; for tasks, UpdatedAt; for events,
	// OccurredAt; for artifacts, the put time (mirrored by the
	// driver's UpdatedAt when known).
	OccurredAt time.Time `json:"occurred_at,omitempty"`
	// Preview is the redacted short summary (≤ heavy-content
	// threshold). Empty when Ref is populated.
	Preview string `json:"preview,omitempty"`
	// Ref is populated when the underlying entity's preview would
	// exceed the heavy-content threshold (D-026). The Console fetches
	// the bytes via `artifacts.get` / `artifacts.get_ref` when it
	// wants them.
	Ref *SearchArtifactRef `json:"ref,omitempty"`
	// Facets carries per-index dimension values relevant to the row
	// (e.g. `{"status":"running"}` for tasks). The set of populated
	// keys per index is documented per Searcher implementation.
	Facets map[string]string `json:"facets,omitempty"`
}

// SearchResponse is the uniform response shape returned by every
// `search.*` method. Pagination is identical across the cluster.
type SearchResponse struct {
	// Rows is the page slice — at most `PageSize` rows. Empty when
	// the filter matched nothing.
	Rows []SearchResultRow `json:"rows"`
	// Page echoes the (defaulted) request page.
	Page int `json:"page"`
	// PageSize echoes the (defaulted) per-page size.
	PageSize int `json:"page_size"`
	// PageCount is the total page count given TotalCount + PageSize.
	PageCount int `json:"page_count"`
	// TotalCount is the total matching row count BEFORE pagination.
	TotalCount int64 `json:"total_count"`
	// HasMore is a convenience flag — true when Page < PageCount.
	HasMore bool `json:"has_more"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}

// applyDefaults is the wire-edge default-application helper. Page=0 →
// 1; PageSize=0 → DefaultSearchPageSize. Returns the normalised page +
// size. The bounds-check (max page size) is enforced separately by the
// handler so it can return `CodeInvalidRequest` rather than silently
// clamping.
func (r *SearchRequest) applyDefaults() (page, size int) {
	page = r.Page
	if page < 1 {
		page = 1
	}
	size = r.PageSize
	if size <= 0 {
		size = DefaultSearchPageSize
	}
	return
}

// PageBounds returns the normalised (page, size) pair without mutating
// the receiver. Useful for handlers that need to validate-then-pass.
func (r SearchRequest) PageBounds() (page, size int) {
	return (&r).applyDefaults()
}

// MarshalJSON is the standard json.Marshaler — kept explicit so the
// SearchResponse shape stays stable across a refactor that might
// reorder fields. Round-trip is verified by search_test.go.
func (r SearchResponse) MarshalJSON() ([]byte, error) {
	type alias SearchResponse
	return json.Marshal(alias(r))
}
