package types

import "time"

// Memory-page pagination bounds, shared by the `memory.list` method per
// the Phase 73j plan acceptance criteria. The values mirror the Phase
// 72c `search.*` / Phase 72e `pause.list` pagination contracts so a
// future Console-side pagination component is shared across pages, not
// re-implemented per-method. A client that omits Page / PageSize gets
// the documented defaults; a request above the max — or negative —
// gets a 400 (CodeInvalidRequest), never a silent clamp.
const (
	DefaultMemoryListPageSize = 50
	MaxMemoryListPageSize     = 200
)

// MemoryScope is the typed enum of the three memory-record scopes the
// `memory.list` filter accepts. Memory is session-scoped by default
// (CLAUDE.md §6 rule 4); user- and tenant-scoped records exist only
// through an explicit declared promotion policy. The Protocol owns its
// own vocabulary (RFC §5.1 / CLAUDE.md §13 single-source rule) — this
// wire enum never re-exports a runtime Go type.
type MemoryScope string

// Canonical memory-record scopes. The set is closed.
const (
	MemoryScopeSession MemoryScope = "session"
	MemoryScopeUser    MemoryScope = "user"
	MemoryScopeTenant  MemoryScope = "tenant"
)

// IsValidMemoryScope reports whether s is one of the three canonical
// memory-record scopes.
func IsValidMemoryScope(s MemoryScope) bool {
	switch s {
	case MemoryScopeSession, MemoryScopeUser, MemoryScopeTenant:
		return true
	}
	return false
}

// MemoryStrategyName is the typed enum of the memory strategies a
// `memory.list` filter facet accepts. The three V1 values map onto the
// runtime-side `memory.Strategy` taxonomy (Phase 24); the overlay-chip
// names (`pinned`, `episodic`, `recent`, `persistent`) are reserved for
// post-V1 strategies and intentionally NOT in the closed set — a filter
// naming an unshipped strategy is rejected with CodeInvalidRequest, not
// silently dropped.
type MemoryStrategyName string

// Canonical V1 memory-strategy names — the wire projection of the
// `memory.Strategy` taxonomy shipped at Phase 24.
const (
	MemoryStrategyNone           MemoryStrategyName = "none"
	MemoryStrategyTruncation     MemoryStrategyName = "truncation"
	MemoryStrategyRollingSummary MemoryStrategyName = "rolling_summary"
)

// IsValidMemoryStrategy reports whether s is one of the three V1
// memory-strategy names.
func IsValidMemoryStrategy(s MemoryStrategyName) bool {
	switch s {
	case MemoryStrategyNone, MemoryStrategyTruncation, MemoryStrategyRollingSummary:
		return true
	}
	return false
}

// MemoryDriverName is the typed enum of the three V1 persistence
// drivers a `memory.list` filter facet accepts. The set matches the
// persistence triad (in-memory / SQLite / Postgres) every persistence-
// shaped subsystem ships at V1 (CLAUDE.md §9).
type MemoryDriverName string

// Canonical V1 memory-driver names.
const (
	MemoryDriverInmem    MemoryDriverName = "inmem"
	MemoryDriverSQLite   MemoryDriverName = "sqlite"
	MemoryDriverPostgres MemoryDriverName = "postgres"
)

// IsValidMemoryDriver reports whether d is one of the three V1
// memory-driver names.
func IsValidMemoryDriver(d MemoryDriverName) bool {
	switch d {
	case MemoryDriverInmem, MemoryDriverSQLite, MemoryDriverPostgres:
		return true
	}
	return false
}

// MemoryItem is one row in the Memory page's main table — the wire
// projection of a single memory record. Heavy values are NEVER inlined
// on this row shape: a row carries the `SizeBytes` count and the
// `HeavyContent` flag, and the Console calls `memory.get` for the
// per-item detail (which produces an `MemoryArtifactRef` above the
// heavy-content threshold per D-026). Cross-package single source per
// CLAUDE.md §8 + D-002; the typed Protocol client (D-093) regenerates
// from this without hand-editing.
//
// D-065 invariant: there is NO `Priority` field. The `Pinned` strategy
// overlay chip is a Phase 24 strategy, not a session-level priority
// dimension — no priority surfaces on a memory row, ever.
type MemoryItem struct {
	// Key is the memory record key — a deterministic per-record
	// identifier the Console passes back to `memory.get`. A client
	// never constructs or parses it.
	Key string `json:"key"`
	// Strategy is the memory strategy this record was produced under —
	// one of the MemoryStrategyName values.
	Strategy string `json:"strategy"`
	// Scope is the record's scope — "session" | "user" | "tenant".
	Scope string `json:"scope"`
	// Identity is the (tenant, user, session [, run]) the record lives
	// under. Flat strings — never a re-export of the runtime identity
	// quadruple.
	Identity IdentityScope `json:"identity"`
	// AgentID is the registration identity of the agent that produced
	// the record, when known. NOT an isolation principal (CLAUDE.md §6
	// clarifying note) — surfaced for the Owner column only.
	AgentID string `json:"agent_id,omitempty"`
	// CreatedAt is the wall-clock time the record was first written.
	CreatedAt time.Time `json:"created_at"`
	// LastUpdatedAt is the wall-clock time the record was last touched.
	LastUpdatedAt time.Time `json:"last_updated_at"`
	// ExpiresAt is the record's TTL expiry; the zero value (omitted)
	// means the record has no TTL.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// SizeBytes is the byte length of the record's post-redaction
	// value. A count only — the bytes themselves are never inlined on
	// this row shape.
	SizeBytes int64 `json:"size_bytes"`
	// HeavyContent is true when SizeBytes meets or exceeds the D-026
	// heavy-content threshold; the Console renders a heavy-content icon
	// and `memory.get` returns a `MemoryArtifactRef` for the value.
	HeavyContent bool `json:"heavy_content,omitempty"`
	// Driver is the persistence driver backing this record —
	// "inmem" | "sqlite" | "postgres".
	Driver string `json:"driver"`
}

// MemoryFilter narrows the `memory.list` response. An empty filter
// means "the caller's own identity scope, every record." Supplying a
// `TenantIDs` value that reaches OUTSIDE the caller's own tenant (or
// names more than one tenant) requires the `auth.ScopeAdmin` (or
// `auth.ScopeConsoleFleet`) scope claim from the D-079 closed two-scope
// set; a missing-claim cross-tenant request is rejected loudly with
// `CodeIdentityScopeRequired` (HTTP 403) — NEVER silently downgraded to
// an empty result set. There is NO dedicated memory scope: cross-tenant
// memory listing gates on the same closed set every other Stage-2 page
// uses (audit B1 resolution).
type MemoryFilter struct {
	// TenantIDs narrows to a tenant set; empty = the caller's own
	// tenant. A foreign tenant OR len>1 requires auth.ScopeAdmin
	// (D-079 closed set).
	TenantIDs []string `json:"tenant_ids,omitempty"`
	// UserIDs narrows to a user set within the visible tenants.
	UserIDs []string `json:"user_ids,omitempty"`
	// SessionIDs narrows to a session set.
	SessionIDs []string `json:"session_ids,omitempty"`
	// AgentIDs narrows to records produced by a given agent set. NOT
	// an isolation filter (CLAUDE.md §6 clarifying note).
	AgentIDs []string `json:"agent_ids,omitempty"`
	// Scopes narrows to a subset of ["session", "user", "tenant"].
	// Each value must be a canonical MemoryScope.
	Scopes []string `json:"scopes,omitempty"`
	// Drivers narrows to a subset of ["inmem", "sqlite", "postgres"].
	// Each value must be a canonical MemoryDriverName.
	Drivers []string `json:"drivers,omitempty"`
	// Strategies narrows to a subset of the V1 MemoryStrategyName set.
	Strategies []string `json:"strategies,omitempty"`
	// HasTTLExpiring, when true, narrows to records whose ExpiresAt
	// falls within (now, now+1h].
	HasTTLExpiring bool `json:"has_ttl_expiring,omitempty"`
	// ContentSearch is an optional substring matched against the
	// post-redaction record value text. The match is runtime-side
	// (brief 11 §CC-4) — never a Console-side scan over an exported
	// snapshot.
	ContentSearch string `json:"content_search,omitempty"`
}

// MemoryListRequest is the wire request for the `memory.list` method.
//
// Identity is mandatory at the Protocol edge per RFC §5.5 — the request
// flows out of an auth-verified identity in ctx, never trusted from the
// body. The `Identity` field exists for the Phase 60 trust-based
// carrier-header posture and for body-side echo; the memory-list
// handler reads the verified identity from ctx (preferred) and defends
// the body identity against it.
type MemoryListRequest struct {
	// Identity is the request's identity scope. The triple is
	// mandatory; an incomplete triple fails the request closed at the
	// Protocol edge with CodeIdentityRequired (401).
	Identity IdentityScope `json:"identity"`
	// Filter narrows the listed records. Empty filter = the caller's
	// own identity scope, every record.
	Filter MemoryFilter `json:"filter,omitempty"`
	// Page is the 1-based page number; defaults to 1 when zero. A
	// negative Page is rejected with CodeInvalidRequest (400).
	Page int `json:"page,omitempty"`
	// PageSize is the per-page row count. Defaults to
	// DefaultMemoryListPageSize (50) when zero; values above
	// MaxMemoryListPageSize (200) — or negative — are rejected with
	// CodeInvalidRequest (400). Never silently clamped.
	PageSize int `json:"page_size,omitempty"`
}

// MemoryAggregates carries the page-level counters the Memory page's
// sub-header strip renders. The 24-h-window counters derive from
// `events.aggregate` (Phase 72a) over the `memory.*` event types —
// runtime-side computation per brief 11 §CC-4.
type MemoryAggregates struct {
	// Total is the total record count across the filtered set.
	Total int64 `json:"total"`
	// ExpiringIn1h is the count of records whose ExpiresAt falls
	// within (now, now+1h].
	ExpiringIn1h int64 `json:"expiring_in_1h"`
	// IdentityRejected24h is the count of `memory.identity_rejected`
	// events (D-033) observed in the last 24 hours.
	IdentityRejected24h int64 `json:"identity_rejected_24h"`
	// RecoveryDropped24h is the count of `memory.recovery_dropped`
	// events (D-035) observed in the last 24 hours. The shipped wire
	// string is `memory.recovery_dropped` — page-memory.md §12 names a
	// mockup-refinement `memory.overflow_drop_oldest`; this phase uses
	// the shipped constant (see the phase plan "Findings I'm departing
	// from").
	RecoveryDropped24h int64 `json:"recovery_dropped_24h"`
}

// MemoryListResponse is the wire response for the `memory.list` method.
type MemoryListResponse struct {
	// Items is the page of memory records, ordered LastUpdatedAt
	// descending (newest first) for a deterministic table.
	Items []MemoryItem `json:"items"`
	// Page is the 1-based page number this response covers.
	Page int `json:"page"`
	// PageSize is the per-page row count applied.
	PageSize int `json:"page_size"`
	// PageCount is the total number of pages over the filtered set.
	PageCount int `json:"page_count"`
	// TotalRows is the total filtered row count across all pages.
	TotalRows int `json:"total_rows"`
	// Aggregates carries the page-level counters.
	Aggregates MemoryAggregates `json:"aggregates"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}

// MemoryArtifactRef is the by-reference shape `memory.get` returns when
// the record's value serialised size meets or exceeds the configured
// heavy-content threshold (D-026). It mirrors a subset of
// `internal/artifacts.ArtifactRef` but is a flat wire type — the
// Protocol owns its vocabulary; runtime Go structs never leak (RFC
// §5.1 / CLAUDE.md §13 single-source rule). It is the same flat shape
// `PauseArtifactRef` / `SearchArtifactRef` use, kept as a distinct type
// so a future divergence in either surface does not whipsaw the other.
type MemoryArtifactRef struct {
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

// MemoryMetadata carries the per-record metadata the Memory page's
// right-rail Selected-item-detail card renders.
type MemoryMetadata struct {
	// TTL is the record's configured time-to-live. The zero value
	// means the record has no TTL.
	TTL time.Duration `json:"ttl,omitempty"`
	// StrategyConfig is the bounded, strategy-named knob map surfaced
	// for the right-rail detail (e.g. budget tokens for `truncation`).
	StrategyConfig map[string]string `json:"strategy_config,omitempty"`
	// RelatedEventIDs are the recent event IDs touching this record's
	// key — for the right-rail "Inspect related events" deep-link.
	RelatedEventIDs []string `json:"related_event_ids,omitempty"`
}

// MemoryGetRequest is the wire request for the `memory.get` method.
type MemoryGetRequest struct {
	// Identity is the request's identity scope. The triple is
	// mandatory; an incomplete triple fails closed with
	// CodeIdentityRequired (401).
	Identity IdentityScope `json:"identity"`
	// Key is the memory record key — the value carried on a
	// `MemoryItem.Key` returned by `memory.list`.
	Key string `json:"key"`
}

// MemoryItemDetail is the `memory.get` result. EXACTLY ONE of `Value` /
// `ValueArtifact` is populated; never both. Above the heavy-content
// threshold (D-026), `Value` is empty and `ValueArtifact` carries the
// by-reference stub; below it, `Value` carries the post-redaction
// bytes and `ValueArtifact` is nil. A driver that returns raw heavy
// bytes is a leak — the runtime fails loudly rather than inlining them.
type MemoryItemDetail struct {
	// Item is the row-shaped projection of the record (the same shape
	// `memory.list` returns).
	Item MemoryItem `json:"item"`
	// Value is the post-redaction record value, populated ONLY when
	// SizeBytes is below the heavy-content threshold (D-026).
	Value []byte `json:"value,omitempty"`
	// ValueArtifact is populated when SizeBytes meets or exceeds the
	// heavy-content threshold (D-026). The Console fetches the bytes
	// via `artifacts.get` against this stub. When ValueArtifact is set,
	// Value is nil — and vice-versa.
	ValueArtifact *MemoryArtifactRef `json:"value_artifact,omitempty"`
	// Metadata carries the per-record metadata.
	Metadata MemoryMetadata `json:"metadata"`
}

// MemoryGetResponse is the wire response for the `memory.get` method.
type MemoryGetResponse struct {
	// Detail is the full record detail.
	Detail MemoryItemDetail `json:"detail"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}

// MemoryHealthRequest is the wire request for the `memory.health`
// method. The identity is read from ctx; the body Identity is the
// Phase 60 carrier-header echo, defended against the verified identity.
type MemoryHealthRequest struct {
	// Identity is the request's identity scope. The triple is
	// mandatory; an incomplete triple fails closed with
	// CodeIdentityRequired (401).
	Identity IdentityScope `json:"identity"`
}

// MemoryHealthAggregate carries the aggregate memory-health counters
// the Memory page's right-rail Memory-health card renders. The 24-h-
// window counters derive from `events.aggregate` (Phase 72a) over the
// `memory.*` event types — runtime-side computation per brief 11 §CC-4.
type MemoryHealthAggregate struct {
	// Total is the total memory-record count for the caller's scope.
	Total int64 `json:"total"`
	// ExpiringIn1h is the count of records whose TTL expires within
	// the next hour.
	ExpiringIn1h int64 `json:"expiring_in_1h"`
	// IdentityRejected24h is the count of `memory.identity_rejected`
	// events (D-033) observed in the last 24 hours.
	IdentityRejected24h int64 `json:"identity_rejected_24h"`
	// RecoveryDropped24h is the count of `memory.recovery_dropped`
	// events (D-035) observed in the last 24 hours.
	RecoveryDropped24h int64 `json:"recovery_dropped_24h"`
	// DriverByScope maps each memory scope to the persistence driver
	// backing it — e.g. {"session":"inmem", "tenant":"postgres"}. The
	// driver-comparison rollup the Memory page renders.
	DriverByScope map[string]string `json:"driver_by_scope"`
}

// MemoryHealthResponse is the wire response for the `memory.health`
// method.
type MemoryHealthResponse struct {
	// Aggregate carries the aggregate health counters.
	Aggregate MemoryHealthAggregate `json:"aggregate"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}
