package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// AggregatorClock abstracts the runtime's notion of "now" so the
// aggregator can be tested with a fake clock. Production callers pass
// nil and the aggregator falls back to time.Now (UTC). The interface
// is intentionally narrow: only Now() — the aggregator never needs to
// tick. Mirrors the inmem-driver Clock seam (no second clock vocabulary
// added).
type AggregatorClock interface {
	Now() time.Time
}

// realClock is the production AggregatorClock used when caller passes
// nil. UTC.
type realAggregatorClock struct{}

func (realAggregatorClock) Now() time.Time { return time.Now().UTC() }

// floorToGrid returns the start of the grid cell that contains t on the
// fixed grid `anchor + k·bucket` (k integer). It is a pure function of
// (t, anchor, bucket) — the same inputs always yield the same instant,
// so two aggregate calls at two different clock instants that fall in
// the same cell produce IDENTICAL bucket boundaries (addressability),
// including across a runtime restart. bucket MUST be > 0 (the caller
// validates Window/Bucket before reaching here).
//
// The division floors toward negative infinity (not toward zero, which
// is Go's `/` default) so instants BEFORE the anchor still land on the
// grid without an off-by-one cell at k transitions.
func floorToGrid(t, anchor time.Time, bucket time.Duration) time.Time {
	delta := t.Sub(anchor)
	q := delta / bucket
	if delta%bucket != 0 && (delta < 0) != (bucket < 0) {
		q--
	}
	return anchor.Add(q * bucket)
}

// defaultAggregateScanBound is the number of matching events a single
// aggregation read collects from the windowed substrate. It is generous
// enough to keep the memory profile equal to materializing the whole
// replay ring (whose production default is an order of magnitude smaller),
// while remaining bounded so an unbounded persisted log never forces an
// unbounded scan. A window whose matching events exceed the bound returns
// PARTIAL buckets with Truncated=true — DATA, never a request error.
const defaultAggregateScanBound = 100_000

// maxAnchorDistance bounds how far an origin-anchored grid's Anchor may sit
// from the effective Now. `floorToGrid` computes `now.Sub(anchor)` as an
// int64-nanosecond Duration; an Anchor astronomically far from Now would
// overflow that subtraction and yield a garbage grid. A century in either
// direction is far more than any real caller needs (the globally-shared Unix
// epoch grid is ~decades back) and keeps every intermediate computation well
// inside the int64-nanosecond range, so a farther Anchor fails loudly with
// ErrAggregateBadWindow rather than silently producing nonsense boundaries
// (CLAUDE.md §5 "fail loudly").
const maxAnchorDistance = 100 * 365 * 24 * time.Hour

// ErrAggregateBadWindow — the request's Window / Bucket pair was
// structurally invalid: zero or negative Window, zero or negative
// Bucket, or Bucket does not evenly divide Window. The aggregator
// fails loudly (CLAUDE.md §5) rather than silently rounding so a
// rendering client never sees a fractional trailing bucket.
var ErrAggregateBadWindow = errors.New("events: aggregate Window/Bucket is invalid")

// ErrAggregateIdentityRequired — the request's filter elided the
// identity triple AND the caller's identity tuple was not supplied.
// Mirrors ErrIdentityRequired's shape but distinguished so callers
// can branch on the cause when they want to.
var ErrAggregateIdentityRequired = errors.New("events: aggregate filter must specify (tenant, user, session) unless the caller's identity tuple was supplied")

// Aggregator is the compiled artifact that produces time-bucketed
// event-type counts over a window. It is a reusable artifact in the
// sense: bus and clock are set once at construction and never
// mutated; Aggregate() holds no per-request state on the Aggregator
// (each request creates its own buckets slice). One Aggregator serves
// N concurrent requests safely (the concurrent-reuse test pins it under
// -race).
//
// The aggregator sources its window snapshot from the bus's
// HistoryReplayer cross-session windowed fan-in — the SAME substrate the
// windowed raw-event read (`events.list`) uses — so an aggregate and a
// list of the same window agree by construction on what a session-less
// admin (fleet) read means. When the bus does not implement
// HistoryReplayer (a forward-only driver, or one whose replay ring is
// disabled), Aggregate returns ErrReplayUnavailable — fail loudly, never
// an empty series that looks like "no events."
type Aggregator struct {
	bus       EventBus
	clock     AggregatorClock
	scanBound int
}

// AggregatorOption configures NewAggregator at construction.
type AggregatorOption func(*Aggregator)

// WithAggregatorClock injects an AggregatorClock for tests. Production
// callers do not use this; the default realAggregatorClock is correct.
func WithAggregatorClock(c AggregatorClock) AggregatorOption {
	return func(a *Aggregator) {
		if c != nil {
			a.clock = c
		}
	}
}

// WithAggregateScanBound overrides the single-read aggregation bound (the
// number of matching events one Aggregate call collects from the windowed
// substrate). Production callers do not use this — the default is sized to
// match the whole-ring materialization; it exists so a test can drive the
// over-bound Truncated path without publishing an impractically large
// fixture. A non-positive value is ignored (the default stands).
func WithAggregateScanBound(n int) AggregatorOption {
	return func(a *Aggregator) {
		if n > 0 {
			a.scanBound = n
		}
	}
}

// NewAggregator builds the aggregator over a bus. bus is mandatory —
// a nil fails loud rather than producing an aggregator that nil-panics
// on the first request. The returned *Aggregator is immutable after
// construction and safe for concurrent use.
func NewAggregator(bus EventBus, opts ...AggregatorOption) (*Aggregator, error) {
	if bus == nil {
		return nil, fmt.Errorf("events: NewAggregator: bus is nil")
	}
	a := &Aggregator{
		bus:       bus,
		clock:     realAggregatorClock{},
		scanBound: defaultAggregateScanBound,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// Aggregate executes one aggregation request. The request's Window +
// Bucket define a contiguous time series of buckets; the aggregator
// issues ONE windowed fan-in read over the effective [since, until)
// window (via the bus's HistoryReplayer.ListWindow), then counts each
// returned event into its bucket.
//
// Window MUST be > 0 AND Bucket MUST be > 0 AND Window % Bucket == 0,
// else ErrAggregateBadWindow.
//
// The buckets in the response are in chronological order (oldest
// first); empty buckets are present with an empty Counts map so the
// rendering client sees a contiguous time axis.
//
// authority (`widened`) is the server-derived cross-principal-OR-multi-value
// fan-in decision the transport computed on the RAW, PRE-FOLD wire filter
// (never read from the request body). It is passed straight through as the
// substrate query's Admin flag: a widened read fans in across sessions and
// emits exactly one audit.admin_scope_used; a non-widened read scopes to the
// caller's own (handler-folded) triple and emits none. The runtime never
// re-derives widening from the post-fold filter — a genuine cross-tenant read
// whose other axes fold to the caller carries a complete triple and would be
// mis-classified as non-widened, running un-audited and un-fanned.
//
// The window bounds are threaded onto the substrate query itself
// (q.Filter.Since / q.Filter.Until), so an Until-clamped sub-window is
// enforced by the driver's per-event predicate rather than only by a
// post-read guard.
//
// A window with more matching events than the single-read aggregation bound
// (or a best-effort ring that evicted older events below the requested
// window) returns the PARTIAL buckets with Truncated=true — never a request
// error, never a silent undercount (CLAUDE.md §13). A bus without
// HistoryReplayer returns ErrReplayUnavailable — never a silent empty series.
//
// Aggregate does NOT enforce the cross-tenant scope claim — that is
// the wire transport's job (the transport calls FilterFromWire,
// reads RequiresAdminScope, gates on auth.HasScope). By the time
// Aggregate runs, the request is authorised.
//
// Per-tenant attribution (req.ByTenant) is HONOURED ONLY when the read is
// also widened — the two together gate `EventBucket.CountsByTenant`, a
// re-projection of each bucket's Counts by the counted events' existing
// tenant identity. It is computed in the SAME pass that fills Counts, over
// the SAME already-authorised matched events, so it never widens the read
// and per bucket `Σ_tenant CountsByTenant[tenant][type] == Counts[type]`
// holds by construction. On a non-widened read ByTenant is ignored (nil
// CountsByTenant) so an unelevated caller gains attribution for nothing it
// could not already read, and the response stays byte-identical to a
// request that never opted in (CLAUDE.md §6 defence-in-depth, §13
// fail-closed).
//
// The aggregator respects ctx — ctx.Err() is checked before any
// expensive work and before each bucket fill. A long aggregate against
// a high-cardinality bus that is cancelled by the caller returns
// ctx.Err() promptly.
func (a *Aggregator) Aggregate(ctx context.Context, req prototypes.EventAggregateRequest, widened bool) (prototypes.EventAggregateResponse, error) {
	if req.Window <= 0 {
		return prototypes.EventAggregateResponse{}, fmt.Errorf("%w: Window=%v must be > 0", ErrAggregateBadWindow, req.Window)
	}
	if req.Bucket <= 0 {
		return prototypes.EventAggregateResponse{}, fmt.Errorf("%w: Bucket=%v must be > 0", ErrAggregateBadWindow, req.Bucket)
	}
	if req.Window%req.Bucket != 0 {
		return prototypes.EventAggregateResponse{}, fmt.Errorf("%w: Window=%v not evenly divisible by Bucket=%v", ErrAggregateBadWindow, req.Window, req.Bucket)
	}

	if err := ctx.Err(); err != nil {
		return prototypes.EventAggregateResponse{}, err
	}

	// The aggregator sources its snapshot from the bus's HistoryReplayer
	// cross-session windowed fan-in — the same substrate the windowed
	// raw-event read uses. A bus that does not implement it (or whose ring
	// is disabled) fails loud downstream on the read — never an empty series
	// that looks like "no events."
	hr, ok := a.bus.(HistoryReplayer)
	if !ok {
		return prototypes.EventAggregateResponse{}, ErrReplayUnavailable
	}

	now := a.clock.Now().UTC()
	bucketCount := int(req.Window / req.Bucket)

	// windowStart is the lower bound of the first bucket. Without an
	// anchor it is Now-Window (the clock-anchored grid); with an anchor
	// it is floored onto the fixed grid `anchor + k·Bucket` so a
	// bucket_start is an addressable coordinate — a PURE function of
	// (anchor, bucket) with no residual dependence on Now.
	var windowStart time.Time
	if req.Anchor != nil {
		anchor := req.Anchor.UTC()
		// Fail loudly on an Anchor so far from Now that the grid arithmetic
		// (now.Sub(anchor), int64-nanosecond) would overflow into garbage
		// boundaries rather than a garbage-without-error grid (CLAUDE.md §5).
		// The bound compares instants (no subtraction), so the check itself
		// cannot overflow.
		if anchor.Before(now.Add(-maxAnchorDistance)) || anchor.After(now.Add(maxAnchorDistance)) {
			return prototypes.EventAggregateResponse{}, fmt.Errorf(
				"%w: anchor %s is more than %s from now", ErrAggregateBadWindow, anchor.Format(time.RFC3339), maxAnchorDistance)
		}
		// The bucket that CONTAINS Now, floored onto the grid, is the
		// LAST bucket; the series runs back bucketCount-1 buckets from
		// there. The grid only re-phases the boundaries: the span stays
		// exactly Window wide and each edge shifts by at most one Bucket
		// relative to the clock-anchored [Now-Window, Now).
		lastBucketStart := floorToGrid(now, anchor, req.Bucket)
		windowStart = lastBucketStart.Add(-time.Duration(bucketCount-1) * req.Bucket)
	} else {
		windowStart = now.Add(-req.Window)
	}
	// windowEnd is the upper bound of the last bucket. Clock-anchored it
	// equals Now; grid-anchored it is the grid boundary covering Now
	// (generally just after Now). It replaces the bare `now` upper bound
	// so the bucket-range clamp and the substrate read span both track
	// the (possibly re-phased) grid, never the raw clock.
	windowEnd := windowStart.Add(req.Window)

	// Pre-allocate the buckets in chronological order. Every bucket is
	// present even if empty so the rendering client sees a contiguous
	// time axis without gap arithmetic.
	buckets := make([]prototypes.EventBucket, bucketCount)
	for i := range bucketCount {
		buckets[i] = prototypes.EventBucket{
			Start:  windowStart.Add(time.Duration(i) * req.Bucket),
			End:    windowStart.Add(time.Duration(i+1) * req.Bucket),
			Counts: make(map[string]int64),
		}
	}

	// Apply the explicit time-window from the request's filter on top
	// of the grid. If the wire filter named a tighter Since/Until, those
	// take precedence (the aggregator returns counts strictly inside
	// [max(filter.Since, windowStart), min(filter.Until, windowEnd))).
	effectiveSince := windowStart
	if !req.Filter.Since.IsZero() && req.Filter.Since.After(effectiveSince) {
		effectiveSince = req.Filter.Since
	}
	effectiveUntil := windowEnd
	if !req.Filter.Until.IsZero() && req.Filter.Until.Before(effectiveUntil) {
		effectiveUntil = req.Filter.Until
	}

	// Thread the EFFECTIVE window bounds onto the substrate query so the
	// driver's per-event MatchWire enforces them — NOT only a post-read
	// guard. An Until-clamped sub-window whose newest global matches are all
	// newer than Until must be excluded at the source, else the single read
	// fills its bound with too-new events and undercounts the window.
	effFilter := req.Filter
	effFilter.Since = effectiveSince
	effFilter.Until = effectiveUntil

	// One windowed fan-in read over the whole effective window: one read ⇒
	// exactly one audit.admin_scope_used on the widened path. The read is
	// tail-first (Before=0) with a generous bound; a window exceeding the
	// bound comes back with HasMore=true, which the aggregator surfaces as
	// Truncated (DATA, never a 400).
	page, err := hr.ListWindow(ctx, EventListQuery{
		Filter: effFilter,
		Before: 0,
		Limit:  a.scanBound,
		Admin:  widened,
	})
	if err != nil {
		// ErrReplayUnavailable is the documented "this bus does not
		// support windowed history reads" case — surface verbatim so the
		// caller can branch on errors.Is.
		if errors.Is(err, ErrReplayUnavailable) {
			return prototypes.EventAggregateResponse{}, err
		}
		return prototypes.EventAggregateResponse{}, fmt.Errorf("events: aggregate windowed read: %w", err)
	}

	// Per-tenant attribution is a second accumulator over the SAME matched
	// events, gated on BOTH the opt-in flag AND the server-derived widened
	// authority. On a non-widened read the flag is ignored (no attribution
	// map is ever allocated), so the response is byte-identical to a request
	// that never opted in — an unelevated caller gains nothing.
	attribute := req.ByTenant && widened

	for _, ev := range page.Events {
		if err := ctx.Err(); err != nil {
			return prototypes.EventAggregateResponse{}, err
		}
		// The substrate already applied MatchWire (identity sets +
		// since/until + type) and excluded bus-internal notices; the guard
		// below is a defensive bucket-range clamp only.
		if ev.OccurredAt.Before(windowStart) || !ev.OccurredAt.Before(windowEnd) {
			continue
		}
		idx := int(ev.OccurredAt.Sub(windowStart) / req.Bucket)
		if idx < 0 || idx >= bucketCount {
			continue
		}
		typ := string(ev.Type)
		buckets[idx].Counts[typ]++
		if attribute {
			// Re-project the same increment onto the event's existing tenant.
			// The outer map is allocated lazily so empty buckets omit the
			// field (nothing to attribute). Because this counts the identical
			// matched event that just bumped Counts, the per-bucket invariant
			// Σ_tenant CountsByTenant[tenant][type] == Counts[type] holds by
			// construction — attribution is a pure re-projection, never a
			// second, looser read path.
			tid := ev.Identity.TenantID
			if buckets[idx].CountsByTenant == nil {
				buckets[idx].CountsByTenant = make(map[string]map[string]int64)
			}
			byType := buckets[idx].CountsByTenant[tid]
			if byType == nil {
				byType = make(map[string]int64)
				buckets[idx].CountsByTenant[tid] = byType
			}
			byType[typ]++
		}
	}

	return prototypes.EventAggregateResponse{
		Buckets:         buckets,
		Truncated:       page.HasMore || page.Truncated,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}
