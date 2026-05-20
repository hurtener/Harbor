package types

import "time"

// Pause-snapshot pagination bounds, shared by the `pause.list` method
// per the Phase 72e plan acceptance criteria. The values mirror the
// Phase 72c `search.*` pagination contract (DefaultSearchPageSize /
// MaxSearchPageSize) so a future Console-side pagination component is
// shared across pages, not re-implemented per-method. A client that
// omits Page / PageSize gets the documented defaults; a request above
// the max gets a 400 (CodeInvalidRequest) — never a silent clamp,
// since a silent clamp would defeat the per-row identity boundary the
// integration test asserts.
const (
	DefaultPauseListPageSize = 50
	MaxPauseListPageSize     = 200
)

// PauseSnapshotState is the typed enum of pause-record lifecycle states
// the `pause.list` snapshot projects. It is the wire projection of the
// runtime-internal `pauseresume.State`; the Protocol owns its own
// vocabulary (RFC §5.1 / CLAUDE.md §13 single-source rule) so the wire
// shape never re-exports a runtime Go type.
type PauseSnapshotState string

// Canonical pause-snapshot states. The set is closed — a pause record
// is either still parked or already resumed.
const (
	PauseStatePaused  PauseSnapshotState = "paused"
	PauseStateResumed PauseSnapshotState = "resumed"
)

// IsValidPauseSnapshotState reports whether s is one of the two
// canonical pause-snapshot states.
func IsValidPauseSnapshotState(s PauseSnapshotState) bool {
	switch s {
	case PauseStatePaused, PauseStateResumed:
		return true
	}
	return false
}

// PauseArtifactRef is the by-reference shape a `PauseSnapshot` carries
// when the pause record's `Payload` serialised size meets or exceeds
// the configured heavy-content threshold (D-026). It mirrors a subset
// of `internal/artifacts.ArtifactRef` but is a flat wire type — the
// Protocol owns its vocabulary; runtime Go structs never leak (RFC
// §5.1 / CLAUDE.md §13 single-source rule). It is the same flat shape
// `SearchResultRow.Ref` (`SearchArtifactRef`) uses, kept as a distinct
// type so a future divergence in either surface does not whipsaw the
// other.
type PauseArtifactRef struct {
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

// PauseSnapshot is the wire projection of a single Coordinator pause
// record. Cross-package single source per CLAUDE.md §8 + D-002; the
// typed Protocol client (D-093) regenerates from this without
// hand-editing.
type PauseSnapshot struct {
	// Token is the opaque runtime-issued pause Token (RFC §6.3). A
	// client never constructs or parses it; it is the handle the
	// Phase 54 `resume` / `approve` / `reject` methods take.
	Token string `json:"token"`
	// Reason is one of the four canonical pause reasons (RFC §6.3).
	Reason string `json:"reason"`
	// State is the lifecycle state — "paused" or "resumed".
	State PauseSnapshotState `json:"state"`
	// Identity is the (tenant, user, session [, run]) the pause was
	// recorded under. Flat strings — never a re-export of the runtime
	// identity quadruple.
	Identity IdentityScope `json:"identity"`
	// PausedAt is the wall-clock time the pause was recorded.
	PausedAt time.Time `json:"paused_at"`
	// ResumedAt is the wall-clock time Resume was called; the zero
	// value (omitted) unless State == "resumed".
	ResumedAt time.Time `json:"resumed_at,omitempty"`
	// Payload is the sanitised pause payload INLINE when its serialised
	// size is below the heavy-content threshold. Otherwise the runtime
	// routes it through the ArtifactStore and ships PayloadRef instead
	// (D-026 — context-window safety net applied to Protocol snapshots).
	// Exactly one of Payload / PayloadRef is populated for a pause
	// carrying a payload; both are empty for a payload-free pause.
	Payload map[string]any `json:"payload,omitempty"`
	// PayloadRef is populated when the pause record's Payload exceeded
	// the heavy-content threshold (D-026). The Console fetches the
	// bytes via `artifacts.get` / `artifacts.get_ref` when it wants
	// them. When PayloadRef is set, Payload is nil.
	PayloadRef *PauseArtifactRef `json:"payload_ref,omitempty"`
}

// PauseFilter narrows the `pause.list` response. An empty filter means
// "the caller's own identity scope, status=paused" — the
// intervention-queue default. Supplying a `TenantIDs` value that
// reaches OUTSIDE the caller's own tenant (or naming more than one
// tenant) requires the `auth.ScopeAdmin` scope claim (D-079); a
// missing-claim cross-tenant request is rejected loudly with
// `CodeIdentityScopeRequired` (HTTP 403) — NEVER silently downgraded
// to an empty result set.
type PauseFilter struct {
	// Status filters by lifecycle state; empty defaults to ["paused"].
	// Each value must be a canonical PauseSnapshotState.
	Status []string `json:"status,omitempty"`
	// TenantIDs narrows to a tenant set; empty defaults to the
	// caller's own tenant. A foreign tenant OR len>1 requires admin.
	TenantIDs []string `json:"tenant_ids,omitempty"`
	// UserIDs narrows to a user set within the visible tenants.
	UserIDs []string `json:"user_ids,omitempty"`
	// SessionIDs narrows to a session set.
	SessionIDs []string `json:"session_ids,omitempty"`
	// RunIDs narrows to a run set.
	RunIDs []string `json:"run_ids,omitempty"`
	// Reasons narrows to one or more canonical pause reasons.
	Reasons []string `json:"reasons,omitempty"`
	// Since is an optional lower bound on PausedAt (inclusive).
	Since time.Time `json:"since,omitempty"`
	// Until is an optional upper bound on PausedAt (inclusive).
	Until time.Time `json:"until,omitempty"`
}

// PauseListRequest is the wire request for the `pause.list` method.
//
// Identity is mandatory at the Protocol edge per RFC §5.5 — the request
// flows out of an auth-verified identity in ctx, never trusted from the
// body. The `Identity` field exists for the Phase 60 trust-based
// carrier-header posture and for body-side echo; the pause-list handler
// reads the verified identity from ctx (preferred) and defends the body
// identity against it.
type PauseListRequest struct {
	// Identity is the request's identity scope. The triple is
	// mandatory; an incomplete triple fails the request closed at the
	// Protocol edge with CodeIdentityRequired (401).
	Identity IdentityScope `json:"identity"`
	// Filter narrows the snapshot. Empty filter = caller's own
	// identity scope, status=paused.
	Filter PauseFilter `json:"filter,omitempty"`
	// Page is the 1-based page number; defaults to 1 when zero. A
	// negative Page is rejected with CodeInvalidRequest (400).
	Page int `json:"page,omitempty"`
	// PageSize is the per-page row count. Defaults to
	// DefaultPauseListPageSize (50) when zero; values above
	// MaxPauseListPageSize (200) — or negative — are rejected with
	// CodeInvalidRequest (400). Never silently clamped.
	PageSize int `json:"page_size,omitempty"`
}

// PauseListResponse is the wire response for the `pause.list` method.
type PauseListResponse struct {
	// Snapshots is the page of pause records, ordered PausedAt
	// descending (newest first) for a deterministic intervention
	// queue.
	Snapshots []PauseSnapshot `json:"snapshots"`
	// Page is the 1-based page number this response covers.
	Page int `json:"page"`
	// PageSize is the per-page row count applied.
	PageSize int `json:"page_size"`
	// PageCount is the total number of pages over the filtered set.
	PageCount int `json:"page_count"`
	// TotalRows is the total filtered row count across all pages.
	TotalRows int `json:"total_rows"`
	// Truncated is true when a status=resumed filter aged out beyond
	// the in-memory registry's retention. The Coordinator's
	// resumed-records retention is bounded by the destructive-on-resume
	// contract (Phase 50 / coordinator.go) — a resumed Token is
	// queryable only until the Coordinator clears it. Operators
	// inspecting historical resume activity should use
	// `events.subscribe` on the `pause.resumed` topic instead.
	Truncated bool `json:"truncated,omitempty"`
}
