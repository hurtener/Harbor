package types

import "time"

// EventFilter is the canonical wire predicate every events.* Protocol
// method consumes — `events.subscribe` (Phase 72) for live subscriptions
// and `events.aggregate` (Phase 72a) for time-bucketed count series. The
// shape is identity-scope-aware: cross-tenant filters (TenantIDs with
// more than one entry, or a tenant other than the caller's) require the
// `auth.ScopeAdmin` OR `auth.ScopeConsoleFleet` claim per D-079 — there
// is NO dedicated `events.crosstenant` scope (the closed two-scope set
// is the wire posture).
//
// Identity is mandatory. An EventFilter with empty UserIDs / SessionIDs
// is interpreted as "the caller's own identity tuple" — the wire edge
// resolves the missing components from the caller's identity quadruple.
// A filter that elides the triple WITHOUT a scope claim is rejected
// loudly at the wire edge with `CodeIdentityRequired`; one that names
// multiple tenants WITHOUT a scope claim is rejected with
// `CodeIdentityScopeRequired` (Phase 72; HTTP 403).
//
// Heavy payloads (D-026 / RFC §6.5): the filter operates on event
// HEADER fields only (type, identity, timestamp). Predicates over event
// payload bytes would force the runtime to materialise heavy payloads
// through the LLM-edge safety net — explicitly out of scope per Brief 11
// §CC-4 (substring payload search is post-V1).
//
// Since / Until are UTC; the empty zero-value means "unbounded on that
// side." A non-zero Until that precedes Since is a structurally-invalid
// filter (`CodeInvalidRequest`).
type EventFilter struct {
	// EventTypes narrows to events whose `Type` is in the set. Empty
	// matches every type (today's `events.subscribe` default). The
	// strings are the registered `events.EventType` values
	// (`tool.failed`, `planner.repair_exhausted`, etc.).
	EventTypes []string `json:"event_types,omitempty"`
	// TenantIDs / UserIDs / SessionIDs / RunIDs narrow to events whose
	// identity tuple matches one of the supplied values. Empty on any
	// axis is interpreted as "the caller's own component"; >1 on the
	// tenant axis (or a single tenant other than the caller's) requires
	// `auth.ScopeAdmin` OR `auth.ScopeConsoleFleet` per D-079.
	TenantIDs  []string `json:"tenant_ids,omitempty"`
	UserIDs    []string `json:"user_ids,omitempty"`
	SessionIDs []string `json:"session_ids,omitempty"`
	RunIDs     []string `json:"run_ids,omitempty"`
	// Since is the optional lower bound (inclusive) on `OccurredAt`.
	// Zero means "unbounded on the lower side." UTC.
	Since time.Time `json:"since,omitempty"`
	// Until is the optional upper bound (exclusive) on `OccurredAt`.
	// Zero means "unbounded on the upper side." UTC.
	Until time.Time `json:"until,omitempty"`
}

// EventBucket is a single time-bucketed count series — one stripe of the
// per-event-type stacked-area sparkline the Events page renders (Phase
// 73g). Start (inclusive) and End (exclusive) are UTC; Counts is keyed
// by event-type string.
//
// Bucket boundaries are computed deterministically by the aggregator:
// for a request whose window is W and bucket size is B (B divides W),
// the response carries exactly ceil(W/B) buckets, each spanning [Start,
// Start+B). Empty buckets are present (with an empty Counts map) so a
// rendering client can scan a contiguous time axis without gap
// arithmetic.
type EventBucket struct {
	// Start is the bucket's lower bound (inclusive). UTC. The JSON tag
	// is namespaced (bucket_start rather than start) so the wire
	// surface does not collide with the Phase 54 task-control method
	// name start — the Phase 58 single-source grep backstop is
	// substring-shaped (it does not parse struct tags) and a bare
	// start tag would false-positive against it.
	Start time.Time `json:"bucket_start"`
	// End is the bucket's upper bound (exclusive). UTC. End - Start ==
	// the request's `Bucket` width.
	End time.Time `json:"bucket_end"`
	// Counts maps event-type string → count of events in this bucket.
	// An empty map means "no matching events in this window slice" —
	// the bucket is still present so the rendering client sees the gap.
	Counts map[string]int64 `json:"counts"`
}

// EventAggregateRequest is the wire request for the `events.aggregate`
// Protocol method (Phase 72a). It returns a deterministic time series
// of event-type counts over `Window`, bucketed by `Bucket`. The window
// is anchored at the request's effective `Now` (the runtime's
// monotonic clock at handler entry) — the response slice runs
// [Now-Window, Now) bucketed at `Bucket` width.
//
// The request body's `filter` is the EventFilter above; the same
// identity-scope rules apply (a cross-tenant aggregate is gated on the
// `auth.ScopeAdmin` OR `auth.ScopeConsoleFleet` claim per D-079).
type EventAggregateRequest struct {
	// Filter narrows the events the aggregator counts. The empty filter
	// (zero EventFilter) is interpreted as "every event in the caller's
	// own identity tuple" (per the EventFilter godoc).
	Filter EventFilter `json:"filter"`
	// Window is the inclusive lookback span. Must be > 0. The response
	// covers [Now-Window, Now) where Now is the runtime's clock at
	// handler entry.
	Window time.Duration `json:"window"`
	// Bucket is the per-bucket width. Must be > 0 and must evenly
	// divide Window (Window % Bucket == 0) — a non-dividing pair is
	// rejected with `CodeInvalidRequest` so the response's bucket count
	// is deterministic and a rendering client never sees a fractional
	// trailing bucket.
	Bucket time.Duration `json:"bucket"`
}

// EventAggregateResponse is the wire response for the `events.aggregate`
// Protocol method. Buckets are in chronological order (oldest first);
// `len(Buckets) == Window/Bucket`.
type EventAggregateResponse struct {
	// Buckets is the per-bucket count series, oldest first. Each
	// bucket's [Start, End) span is exactly `Request.Bucket` wide; the
	// last bucket's End equals the request's effective Now.
	Buckets []EventBucket `json:"buckets"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew (mirrors the
	// Phase 54 ControlResponse / StartResponse shape).
	ProtocolVersion string `json:"protocol_version"`
}
