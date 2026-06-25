package types

import "time"

// State-snapshots windowed event-replay wire types (RFC §5.2 State
// snapshots row). These back the `state.history` Protocol method — a
// by-id, identity-scoped, read-only tail-first windowed read of a
// session's durable event stream. The shapes are flat and
// Protocol-owned: a wire type that re-exported an internal runtime Go
// struct would be the RFC §5.1 reject-on-sight smell. The events →
// chat-messages reduction stays client-side on the reducer the Console
// already owns; the surface returns flat events, not pre-reduced turns.

// Default + maximum window bounds for the `state.history` read. A client
// that omits Limit gets DefaultStateHistoryLimit; a Limit above
// MaxStateHistoryLimit is clamped down to MaxStateHistoryLimit (a larger
// ask is satisfied with the maximum window, not rejected). A negative
// Limit is the one invalid value (CodeInvalidRequest).
const (
	// DefaultStateHistoryLimit is the window size used when a request
	// omits Limit (or sends zero).
	DefaultStateHistoryLimit = 50
	// MaxStateHistoryLimit is the largest window a single request may
	// ask for; a Limit above it is clamped down to this value.
	MaxStateHistoryLimit = 200
)

// StateHistoryRequest is the `state.history` request: a by-id, bounded,
// tail-first backward window read of a session's durable event stream.
type StateHistoryRequest struct {
	// Identity is the caller's identity scope. The verified identity from
	// the JWT (or carrier headers) is authoritative; a body identity that
	// disagrees with the verified one is rejected.
	Identity IdentityScope `json:"identity"`
	// SessionID names the session whose durable event stream is read. An
	// unknown or cross-identity session is CodeNotFound (existence is
	// never revealed across identities).
	SessionID string `json:"session_id"`
	// Before is the exclusive upper bound: only events with
	// Sequence < Before are returned. Zero means "from the tail" (the
	// newest retained events). To scroll one window older, pass the prior
	// response's NextCursor back here.
	Before uint64 `json:"before,omitempty"`
	// Limit is the window size K (clamped to MaxStateHistoryLimit;
	// zero ⇒ DefaultStateHistoryLimit).
	Limit int `json:"limit,omitempty"`
}

// StateArtifactRef is the flat, routable artifact reference a replayed
// heavy-payload event carries — routed via `artifacts.get_ref` (which
// returns a presigned URL on an S3-compat Presigner store, or the typed
// `presign_unsupported` on the default CGo-free inmem/fs stores). It
// mirrors SearchArtifactRef / MemoryArtifactRef and deliberately does NOT
// reuse ArtifactRefSummary (which has no ID/SHA256 — unroutable). No
// inline heavy bytes ever travel through the surface.
type StateArtifactRef struct {
	// ID is the content-addressed identifier
	// (`{namespace}_{sha256_hex[:12]}`). Well-formed and routable to
	// `artifacts.get_ref`.
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

// StateEvent is the flat, single-sourced exported wire projection of one
// durable event — the same field set the SSE wire-event projection
// carries, plus routable artifact refs. A page of these is what the
// client reduces into chat messages.
type StateEvent struct {
	// Type is the canonical event type wire string.
	Type string `json:"type"`
	// Sequence is the per-bus monotonic, gap-free sequence assigned at
	// publish. It is the page's scroll-up cursor unit.
	Sequence uint64 `json:"sequence"`
	// OccurredAt is the event's wall-clock instant.
	OccurredAt time.Time `json:"occurred_at"`
	// Tenant / User / Session flatten the event's identity triple.
	Tenant  string `json:"tenant"`
	User    string `json:"user"`
	Session string `json:"session"`
	// Run, when non-empty, is the run the event belongs to inside the
	// session.
	Run string `json:"run,omitempty"`
	// Payload is the event's redaction-safe payload (post-redaction by
	// construction — the durable log persists events after redaction).
	Payload any `json:"payload,omitempty"`
	// Extra carries the bounded low-cardinality metric labels, when set.
	Extra map[string]string `json:"extra,omitempty"`
	// Artifacts carries the routable references to any heavy payloads
	// offloaded above the heavy-output threshold (RFC §6.5 / §6.10).
	Artifacts []StateArtifactRef `json:"artifacts,omitempty"`
}

// StateHistoryResponse is the `state.history` reply: a bounded page of
// flat events oldest-first within the window, plus the discovered
// head/tail sequence, a scroll-up cursor, and the honest retention-gap
// signal.
type StateHistoryResponse struct {
	// Events is the window's events, oldest-first within the window.
	Events []StateEvent `json:"events"`
	// HeadSequence is the lowest retained sequence for the session (the
	// oldest event still in the durable log).
	HeadSequence uint64 `json:"head_sequence"`
	// TailSequence is the highest retained sequence for the session (the
	// newest event).
	TailSequence uint64 `json:"tail_sequence"`
	// NextCursor is the lowest Sequence in this page — the value to pass
	// back as Before to scroll one window older. Zero when the head is
	// reached (no older events).
	NextCursor uint64 `json:"next_cursor"`
	// HasMore reports whether older events remain before the page's
	// oldest event (i.e. NextCursor is above HeadSequence).
	HasMore bool `json:"has_more"`
	// Truncated is true when the durable log's retained head sits above
	// the session's first sequence (retention trimmed older events) — the
	// honest gap signal, never a silent drop.
	Truncated bool `json:"truncated,omitempty"`
}
