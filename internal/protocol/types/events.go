package types

import "time"

// EventFilter is the canonical wire predicate every events.* Protocol
// method consumes — `events.subscribe` for live subscriptions
// and `events.aggregate` for time-bucketed count series. The
// shape is identity-scope-aware: cross-tenant filters (TenantIDs with
// more than one entry, or a tenant other than the caller's) require the
// `auth.ScopeAdmin` OR `auth.ScopeConsoleFleet` claim per the closed admin-scope set — there
// is NO dedicated `events.crosstenant` scope (the closed two-scope set
// is the wire posture).
//
// Identity is mandatory. An EventFilter with empty UserIDs / SessionIDs
// is interpreted as "the caller's own identity tuple" — the wire edge
// resolves the missing components from the caller's identity quadruple.
// A filter that elides the triple WITHOUT a scope claim is rejected
// loudly at the wire edge with `CodeIdentityRequired`; one that names
// multiple tenants WITHOUT a scope claim is rejected with
// `CodeIdentityScopeRequired` (HTTP 403).
//
// Heavy payloads (RFC §6.5): the filter operates on event
// HEADER fields only (type, identity, timestamp). Predicates over event
// payload bytes would force the runtime to materialise heavy payloads
// through the LLM-edge safety net — explicitly out of scope per the design
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
	// `auth.ScopeAdmin` OR `auth.ScopeConsoleFleet` per the closed admin-scope set.
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

// Default + maximum page bounds for the `events.list` read. A client that
// omits Limit gets DefaultEventsListLimit; a Limit above MaxEventsListLimit
// is clamped down to MaxEventsListLimit (a larger ask is satisfied with the
// maximum page, not rejected). A negative Limit is the one invalid value
// (`CodeInvalidRequest`). The bounds mirror `state.history` so the two
// windowed reads share a paging grammar.
const (
	// DefaultEventsListLimit is the page size used when an `events.list`
	// request omits Limit (or sends zero).
	DefaultEventsListLimit = 50
	// MaxEventsListLimit is the largest page a single `events.list` request
	// may ask for; a Limit above it is clamped down to this value.
	MaxEventsListLimit = 200
)

// EventsListRequest is the wire request for the `events.list` Protocol
// method — the durable, time-ranged, cross-session raw-event read. It
// reuses the EventFilter above (identity axes + since/until time bounds +
// event-type selector) plus a tail-first, sequence-based paging cursor
// mirroring `state.history`'s grammar. Unlike `state.history` (by-id,
// single session) it honours the filter's multi-valued identity sets and,
// under a verified `admin` or `console:fleet` claim, fans in across
// sessions in global-sequence order. The rows returned are the SAME flat
// StateEvent projection `state.history` and the SSE stream carry — no new
// row shape, no redaction change, no heavy-payload inlining.
type EventsListRequest struct {
	// Identity is the (tenant, user, session) scope envelope every
	// Protocol client carries on the body. The handler pulls the
	// authoritative identity from the JWT / X-Harbor-* edge headers
	// (RFC §5.5) — this field is shape-only, declared so the body decode's
	// `DisallowUnknownFields` accepts the Console's standard payload shape
	// (`Transport.request` auto-injects `identity` on every request body).
	// Optional in the wire schema; ignored by the handler.
	Identity IdentityScope `json:"identity,omitempty"`
	// Filter narrows the events returned. The empty filter (zero
	// EventFilter) is interpreted as "every event in the caller's own
	// identity tuple" (per the EventFilter godoc). A cross-tenant filter
	// (a tenant other than the caller's, or multiple tenants) requires the
	// verified `admin` OR `console:fleet` claim per the closed admin-scope
	// set; scope is derived server-side, never from this body.
	Filter EventFilter `json:"filter"`
	// Cursor is the exclusive upper sequence bound (the scroll-up cursor):
	// only events with Sequence < Cursor are returned. Zero means "from the
	// tail" (the newest retained events). To scroll one page older, pass the
	// prior response's NextCursor back here.
	Cursor uint64 `json:"cursor,omitempty"`
	// Limit is the page size K (clamped to MaxEventsListLimit;
	// zero ⇒ DefaultEventsListLimit).
	Limit int `json:"limit,omitempty"`
}

// EventsListResponse is the `events.list` reply: a bounded page of flat
// events oldest-first within the window, plus a scroll-up cursor and the
// honest retention-gap signal. Row shape is byte-identical to
// `state.history`'s StateEvent projection.
type EventsListResponse struct {
	// Events is the page's events, oldest-first within the window.
	Events []StateEvent `json:"events"`
	// NextCursor is the lowest Sequence in this page — the value to pass
	// back as Cursor to scroll one page older. Zero when the retained head
	// is reached (no older events).
	NextCursor uint64 `json:"next_cursor"`
	// HasMore reports whether older matching events remain before the
	// page's oldest event.
	HasMore bool `json:"has_more"`
	// Truncated is true when the substrate's oldest retained row sits above
	// the true first matching event (a best-effort ring evicted older
	// events) — the honest gap signal, never a silent drop. False on the
	// gap-free durable log.
	Truncated bool `json:"truncated,omitempty"`
}

// EventBucket is a single time-bucketed count series — one stripe of the
// per-event-type stacked-area sparkline the Events page renders.
// Start (inclusive) and End (exclusive) are UTC; Counts is keyed
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
	// surface does not collide with the task-control method
	// name start — the single-source grep backstop is
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
	// CountsByTenant is the OPTIONAL per-tenant attribution of this
	// bucket's Counts: tenant → event-type → count. It is present ONLY
	// when the request set ByTenant AND the read was admin-widened (a
	// verified `admin` / `console:fleet` fan-in, derived server-side —
	// never from the request body). Absent (nil, omitted) on every other
	// read, so a caller that does not opt in sees a byte-identical
	// response.
	//
	// It is a pure re-projection of the SAME counted events by their
	// existing tenant identity — no new data path, no new identity axis.
	// The keys are a SUBSET of the authorized (named-or-folded)
	// Filter.TenantIDs; Counts (the totals) and CountsByTenant are scoped
	// to the IDENTICAL authorized filter by construction (one pass over
	// the same matched events), so per bucket
	// `Σ_tenant CountsByTenant[tenant][type] == Counts[type]`. That
	// reconciliation is what makes the tenant boundary independently
	// verifiable on an aggregate the way it already is on a row read
	// (every row carries its own tenant): the widened caller can check the
	// attribution keys against the entitled set it asked for.
	//
	// Empty buckets (no matching events) omit the field even under an
	// attribution request — there is nothing to attribute.
	CountsByTenant map[string]map[string]int64 `json:"counts_by_tenant,omitempty"`
}

// EventAggregateRequest is the wire request for the `events.aggregate`
// Protocol method. It returns a deterministic time series
// of event-type counts over `Window`, bucketed by `Bucket`. By default
// the window is anchored at the request's effective `Now` (the runtime's
// clock at handler entry) — the response slice runs [Now-Window, Now)
// bucketed at `Bucket` width. When the optional `Anchor` is set, the
// grid is instead floored onto `Anchor + k·Bucket` (see the field), so a
// `bucket_start` becomes a stable, addressable coordinate.
//
// The request body's `filter` is the EventFilter above; the same
// identity-scope rules apply (a cross-tenant aggregate is gated on the
// `auth.ScopeAdmin` OR `auth.ScopeConsoleFleet` claim per the closed admin-scope set).
type EventAggregateRequest struct {
	// Identity is the (tenant, user, session) scope envelope every
	// Protocol client carries on the body. The aggregate handler pulls
	// the authoritative identity from the JWT / X-Harbor-* edge headers
	// (RFC §5.5) — this field is shape-only, declared so the body
	// decode's `DisallowUnknownFields` accepts the Console's standard
	// payload shape (`Transport.request` auto-injects `identity` on
	// every request body). Optional in the wire schema; ignored by the
	// handler. Before this field existed it was the only typed request
	// shape on the Console-facing surface that REFUSED the identity
	// envelope, breaking every Events page `events.aggregate` call
	// cross-origin.
	Identity IdentityScope `json:"identity,omitempty"`
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
	// Anchor is the OPTIONAL origin of the bucket grid. When nil (the
	// default), the grid is anchored at the effective Now — the
	// response covers [Now-Window, Now) and a `bucket_start` is NOT
	// addressable twice (two calls at two instants return two boundary
	// sets). When set, bucket boundaries are floored onto the fixed grid
	// `Anchor + k·Bucket`: the response still covers a `Window`'s worth
	// of buckets, but aligned to that grid, so two calls at two instants
	// with the SAME Anchor + Window + Bucket share boundary instants — a
	// `bucket_start` is a stable coordinate a consumer can cache and
	// re-request, INCLUDING across a runtime restart and across two
	// fleet runtimes (the boundary is a pure function of Anchor+Bucket,
	// with no residual dependence on Now). Passing the Unix epoch
	// (`1970-01-01T00:00:00Z`) yields a globally-shared grid.
	//
	// It is a POINTER (not a `time.Time` value) because `omitempty` is a
	// no-op on a struct — a value type would always serialize the zero
	// time and break the additive/optional contract and its TS mirror.
	//
	// Grid-edge note: with an Anchor, the LAST bucket's `bucket_end` is
	// the grid boundary that covers Now — it is generally AFTER Now (by
	// up to one Bucket width), not equal to it. A rendering client must
	// therefore not treat the final bucket as "up to this instant"; the
	// grid re-phases the boundaries and never widens the read span by
	// more than one Bucket at either edge. UTC.
	Anchor *time.Time `json:"anchor,omitempty"`
	// ByTenant opts IN to per-tenant attribution on each bucket
	// (EventBucket.CountsByTenant). It is HONOURED ONLY on an
	// admin-widened read (a verified `admin` / `console:fleet` cross-
	// principal-or-multi-value fan-in, derived server-side); on any other
	// read it is IGNORED and the response is byte-identical to a request
	// that never set it (fail-closed — the body flag never elevates a
	// read, and never widens what is counted).
	//
	// When honoured, each bucket carries CountsByTenant re-projecting that
	// bucket's Counts by the counted events' existing tenant identity, so
	// the widened caller can independently verify the tenant boundary the
	// runtime enforced (the attribution keys must be a subset of the
	// tenants it asked for, and they must sum back to the totals). It adds
	// NO new identity axis and NO payload — a count per `(tenant,
	// event_type)` is the whole surface. Absent/false ⇒ no attribution.
	ByTenant bool `json:"by_tenant,omitempty"`
}

// EventAggregateResponse is the wire response for the `events.aggregate`
// Protocol method. Buckets are in chronological order (oldest first);
// `len(Buckets) == Window/Bucket`.
type EventAggregateResponse struct {
	// Buckets is the per-bucket count series, oldest first. Each
	// bucket's [Start, End) span is exactly `Request.Bucket` wide. When
	// the request carried no `Anchor`, the last bucket's End equals the
	// request's effective Now; when it carried an `Anchor`, the last
	// bucket's End is the grid boundary covering Now (generally AFTER
	// Now, by up to one Bucket — see EventAggregateRequest.Anchor).
	Buckets []EventBucket `json:"buckets"`
	// Truncated is true when the counts are PARTIAL: the aggregation could
	// not observe every matching event in the window. It is set UNIFORMLY
	// across drivers — a best-effort ring that has evicted matching events
	// (older matches may be lost) sets it, and a durable log whose window held more
	// matching events than the single-read aggregation bound could count
	// sets it too. It is the honest DATA signal that a driver difference
	// (retention depth, an observed horizon) changed WHAT the method
	// returns without ever changing WHETHER it works: an over-wide window is
	// never a request error and never a silent undercount — it is partial
	// buckets flagged truncated. False means the counts are complete for the
	// window.
	Truncated bool `json:"truncated,omitempty"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew (mirrors the
	// ControlResponse / StartResponse shape).
	ProtocolVersion string `json:"protocol_version"`
}
