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

// defaultAggregateScanBound is the number of matching events a single
// aggregation read collects from the windowed substrate. It is generous
// enough to keep the memory profile equal to materializing the whole
// replay ring (whose production default is an order of magnitude smaller),
// while remaining bounded so an unbounded persisted log never forces an
// unbounded scan. A window whose matching events exceed the bound returns
// PARTIAL buckets with Truncated=true — DATA, never a request error.
const defaultAggregateScanBound = 100_000

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
	windowStart := now.Add(-req.Window)
	bucketCount := int(req.Window / req.Bucket)

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
	// of the request's Window. If the wire filter named a tighter
	// Since/Until, those take precedence (the aggregator returns
	// counts strictly inside [max(filter.Since, windowStart),
	// min(filter.Until, now))).
	effectiveSince := windowStart
	if !req.Filter.Since.IsZero() && req.Filter.Since.After(effectiveSince) {
		effectiveSince = req.Filter.Since
	}
	effectiveUntil := now
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

	for _, ev := range page.Events {
		if err := ctx.Err(); err != nil {
			return prototypes.EventAggregateResponse{}, err
		}
		// The substrate already applied MatchWire (identity sets +
		// since/until + type) and excluded bus-internal notices; the guard
		// below is a defensive bucket-range clamp only.
		if ev.OccurredAt.Before(windowStart) || !ev.OccurredAt.Before(now) {
			continue
		}
		idx := int(ev.OccurredAt.Sub(windowStart) / req.Bucket)
		if idx < 0 || idx >= bucketCount {
			continue
		}
		buckets[idx].Counts[string(ev.Type)]++
	}

	return prototypes.EventAggregateResponse{
		Buckets:         buckets,
		Truncated:       page.HasMore || page.Truncated,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}
