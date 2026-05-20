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
// D-025 sense: bus and clock are set once at construction and never
// mutated; Aggregate() holds no per-request state on the Aggregator
// (each request creates its own buckets slice). One Aggregator serves
// N concurrent requests safely (concurrent_test.go pins it under
// -race).
//
// The aggregator consumes the bus's Replayer surface for the historical
// snapshot — runtime-side aggregation per brief 11 §CC-4 ("events are
// high-cardinality runtime-side; the runtime owns the index and
// exposes a search method"). When the bus does not implement Replayer
// (a forward-only driver), Aggregate returns ErrReplayUnavailable —
// fail loudly, never an empty series that looks like "no events."
type Aggregator struct {
	bus   EventBus
	clock AggregatorClock
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

// NewAggregator builds the aggregator over a bus. bus is mandatory —
// a nil fails loud rather than producing an aggregator that nil-panics
// on the first request. The returned *Aggregator is immutable after
// construction (D-025) and safe for concurrent use.
func NewAggregator(bus EventBus, opts ...AggregatorOption) (*Aggregator, error) {
	if bus == nil {
		return nil, fmt.Errorf("events: NewAggregator: bus is nil")
	}
	a := &Aggregator{
		bus:   bus,
		clock: realAggregatorClock{},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// Aggregate executes one aggregation request. The request's Window +
// Bucket define a contiguous time series of buckets; the aggregator
// snapshots the bus's ring (via Replayer) once, filters in Go using
// MatchWire over the wire filter, and counts each matching event into
// its bucket.
//
// Window MUST be > 0 AND Bucket MUST be > 0 AND Window % Bucket == 0,
// else ErrAggregateBadWindow.
//
// The buckets in the response are in chronological order (oldest
// first); empty buckets are present with an empty Counts map so the
// rendering client sees a contiguous time axis.
//
// The aggregator respects ctx — ctx.Err() is checked before any
// expensive work and before each bucket fill. A long aggregate against
// a high-cardinality bus that is cancelled by the caller returns
// ctx.Err() promptly.
//
// Aggregate does NOT enforce the cross-tenant scope claim — that is
// the wire transport's job (the transport calls FilterFromWire,
// reads RequiresAdminScope, gates on auth.HasScope). By the time
// Aggregate runs, the request is authorised.
func (a *Aggregator) Aggregate(ctx context.Context, req prototypes.EventAggregateRequest) (prototypes.EventAggregateResponse, error) {
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

	// The aggregator consumes the bus's Replayer surface for the
	// historical snapshot. A bus that does not implement Replayer (or
	// whose ring is disabled) fails loud — never an empty series that
	// looks like "no events."
	replayer, ok := a.bus.(Replayer)
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
	for i := 0; i < bucketCount; i++ {
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

	// Snapshot from the bus. We pass an Admin: true bus-filter to get
	// every retained event from the ring — the aggregator does its
	// own per-event MatchWire filtering. The bus already validated
	// the request's identity / scope upstream (in the transport).
	//
	// The Filter passed to Replay carries no triple because the
	// aggregator's filter is enforced in MatchWire. Identity-source
	// fail-loudness lives at the transport edge: by the time Aggregate
	// runs, the request is identity-validated.
	busFilter := Filter{Admin: true}
	snapshot, err := replayer.Replay(ctx, Cursor{Sequence: 0}, busFilter)
	if err != nil {
		// ErrReplayUnavailable is the documented "this bus does not
		// support replay" case — surface verbatim so the caller can
		// branch on errors.Is.
		if errors.Is(err, ErrReplayUnavailable) {
			return prototypes.EventAggregateResponse{}, err
		}
		// ErrCursorTooOld with Cursor.Sequence==0 should not happen
		// (the inmem driver explicitly bypasses the check for the
		// from-beginning case); if it does, surface it.
		return prototypes.EventAggregateResponse{}, fmt.Errorf("events: aggregate replay: %w", err)
	}

	// Mutate the wire filter's window bounds to the EFFECTIVE bounds so
	// MatchWire enforces them per-event. The wire request value is a
	// copy by value into req; we modify our local view.
	effFilter := req.Filter
	effFilter.Since = effectiveSince
	effFilter.Until = effectiveUntil

	for _, ev := range snapshot {
		if err := ctx.Err(); err != nil {
			return prototypes.EventAggregateResponse{}, err
		}
		if !MatchWire(ev, effFilter) {
			continue
		}
		// Bucket index by elapsed-from-windowStart / bucketWidth. An
		// event whose OccurredAt is before windowStart (caught by
		// MatchWire above when Since==windowStart, but defensive here)
		// is dropped; one at or after now is also dropped.
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
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}
