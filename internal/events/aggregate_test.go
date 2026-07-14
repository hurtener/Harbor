package events_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// fixedAggregatorClock returns the same UTC instant from Now(). Lets the
// aggregator tests assert bucket boundaries deterministically.
type fixedAggregatorClock struct{ t time.Time }

func (f fixedAggregatorClock) Now() time.Time { return f.t }

// aggregatorTestBus builds a fresh inmem bus with a non-zero ring
// (Replayer enabled) for aggregator tests. The bus is closed on
// t.Cleanup so the reaper goroutine does not leak past the test.
func aggregatorTestBus(t *testing.T) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     16,
		IdleTimeout:              200 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         1024,
	}
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// publishEvent helper for aggregator tests. Backdates the event to `at`
// so the aggregator's bucket arithmetic is testable against known
// timestamps. Returns the assigned Sequence (post-publish).
func publishEvent(t *testing.T, bus events.EventBus, typ events.EventType, tenant, user, session string, at time.Time) {
	t.Helper()
	ev := events.Event{
		Type: typ,
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  tenant,
				UserID:    user,
				SessionID: session,
			},
		},
		OccurredAt: at.UTC(),
		// SafePayload — skips the audit redactor (otherwise the redactor
		// reshapes the payload and we have to wire up a redactor pattern
		// for every test).
		Payload: events.BusDroppedPayload{
			FromSeq: 1, ToSeq: 1, DroppedCount: 0, SubscriberID: 0,
		},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish %s: %v", typ, err)
	}
}

// TestAggregate_BucketArithmetic_DeterministicCounts pins the
// load-bearing acceptance criterion: deterministically emit N events
// across two known types over a known window, query aggregate, and
// assert bucket sums match.
func TestAggregate_BucketArithmetic_DeterministicCounts(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)

	// Anchor the window on a known instant. Use a clock that returns
	// THIS instant as Now() so the aggregator's bucket starts are
	// deterministic.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-1 * time.Hour) // 1h window
	bucketWidth := 10 * time.Minute        // 6 buckets

	// Bucket 0 [12:00-12:10 ago → 11:00-11:10]: 1 RuntimeError, 2 ToolFailed
	publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1", windowStart.Add(2*time.Minute))
	publishEvent(t, bus, events.EventTypeRuntimeWarning, "t1", "u1", "s1", windowStart.Add(3*time.Minute))
	publishEvent(t, bus, events.EventTypeRuntimeWarning, "t1", "u1", "s1", windowStart.Add(4*time.Minute))
	// Bucket 2 [+20m..+30m]: 3 RuntimeError
	publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1", windowStart.Add(21*time.Minute))
	publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1", windowStart.Add(22*time.Minute))
	publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1", windowStart.Add(23*time.Minute))
	// Bucket 5 [+50m..+60m]: 1 RuntimeWarning
	publishEvent(t, bus, events.EventTypeRuntimeWarning, "t1", "u1", "s1", windowStart.Add(55*time.Minute))

	agg, err := events.NewAggregator(bus, events.WithAggregatorClock(fixedAggregatorClock{t: now}))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{"t1"},
			UserIDs:    []string{"u1"},
			SessionIDs: []string{"s1"},
		},
		Window: 1 * time.Hour,
		Bucket: bucketWidth,
	}, false)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(resp.Buckets) != 6 {
		t.Fatalf("expected 6 buckets (1h/10m), got %d", len(resp.Buckets))
	}

	// Boundary check: the first bucket starts at windowStart; the last
	// ends at now.
	if !resp.Buckets[0].Start.Equal(windowStart) {
		t.Fatalf("first bucket Start = %v, want %v", resp.Buckets[0].Start, windowStart)
	}
	if !resp.Buckets[len(resp.Buckets)-1].End.Equal(now) {
		t.Fatalf("last bucket End = %v, want %v", resp.Buckets[len(resp.Buckets)-1].End, now)
	}

	// Bucket 0: 1 RuntimeError + 2 RuntimeWarning.
	if got := resp.Buckets[0].Counts["runtime.error"]; got != 1 {
		t.Errorf("bucket 0 runtime.error count = %d, want 1", got)
	}
	if got := resp.Buckets[0].Counts["runtime.warning"]; got != 2 {
		t.Errorf("bucket 0 runtime.warning count = %d, want 2", got)
	}
	// Bucket 1: empty.
	if len(resp.Buckets[1].Counts) != 0 {
		t.Errorf("bucket 1 should be empty, got %v", resp.Buckets[1].Counts)
	}
	// Bucket 2: 3 RuntimeError.
	if got := resp.Buckets[2].Counts["runtime.error"]; got != 3 {
		t.Errorf("bucket 2 runtime.error count = %d, want 3", got)
	}
	// Buckets 3,4: empty.
	if len(resp.Buckets[3].Counts) != 0 || len(resp.Buckets[4].Counts) != 0 {
		t.Errorf("buckets 3-4 should be empty: %v %v", resp.Buckets[3].Counts, resp.Buckets[4].Counts)
	}
	// Bucket 5: 1 RuntimeWarning.
	if got := resp.Buckets[5].Counts["runtime.warning"]; got != 1 {
		t.Errorf("bucket 5 runtime.warning count = %d, want 1", got)
	}
}

// TestAggregate_FilterRespected — emitting events for two tenants and
// filtering for one returns only that tenant's counts.
func TestAggregate_FilterRespected(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-30 * time.Minute)

	publishEvent(t, bus, events.EventTypeRuntimeError, "tenant-A", "u1", "s1", windowStart.Add(5*time.Minute))
	publishEvent(t, bus, events.EventTypeRuntimeError, "tenant-A", "u1", "s1", windowStart.Add(10*time.Minute))
	publishEvent(t, bus, events.EventTypeRuntimeError, "tenant-B", "u2", "s2", windowStart.Add(5*time.Minute))
	publishEvent(t, bus, events.EventTypeRuntimeError, "tenant-B", "u2", "s2", windowStart.Add(15*time.Minute))

	agg, err := events.NewAggregator(bus, events.WithAggregatorClock(fixedAggregatorClock{t: now}))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{"tenant-A"},
			UserIDs:    []string{"u1"},
			SessionIDs: []string{"s1"},
		},
		Window: 30 * time.Minute,
		Bucket: 5 * time.Minute,
	}, false)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var total int64
	for _, b := range resp.Buckets {
		total += b.Counts["runtime.error"]
	}
	if total != 2 {
		t.Fatalf("tenant-A filter returned total=%d, want 2 (excluded tenant-B)", total)
	}
}

// TestAggregate_RejectsBadWindow — Window <= 0, Bucket <= 0, or
// Window % Bucket != 0 all fail loud with ErrAggregateBadWindow.
func TestAggregate_RejectsBadWindow(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)
	agg, err := events.NewAggregator(bus)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	cases := []struct {
		name string
		req  prototypes.EventAggregateRequest
	}{
		{"zero window", prototypes.EventAggregateRequest{Window: 0, Bucket: time.Minute}},
		{"negative window", prototypes.EventAggregateRequest{Window: -time.Hour, Bucket: time.Minute}},
		{"zero bucket", prototypes.EventAggregateRequest{Window: time.Hour, Bucket: 0}},
		{"negative bucket", prototypes.EventAggregateRequest{Window: time.Hour, Bucket: -time.Minute}},
		{"non-dividing pair", prototypes.EventAggregateRequest{Window: time.Hour, Bucket: 7 * time.Minute}},
	}
	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := agg.Aggregate(context.Background(), tc.req, false)
			if !errors.Is(err, events.ErrAggregateBadWindow) {
				t.Fatalf("err = %v, want %v wrapped", err, events.ErrAggregateBadWindow)
			}
		})
	}
}

// TestAggregate_HonoursCtxCancellation — a cancelled ctx returns
// ctx.Err() promptly, before any expensive work.
func TestAggregate_HonoursCtxCancellation(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)
	agg, err := events.NewAggregator(bus)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err = agg.Aggregate(ctx, prototypes.EventAggregateRequest{
		Window: time.Hour,
		Bucket: time.Minute,
		Filter: prototypes.EventFilter{TenantIDs: []string{"t1"}, UserIDs: []string{"u1"}, SessionIDs: []string{"s1"}},
	}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestAggregate_NilBus_FailsLoud — NewAggregator(nil) fails rather than
// producing an aggregator that nil-panics on the first request.
func TestAggregate_NilBus_FailsLoud(t *testing.T) {
	t.Parallel()
	if _, err := events.NewAggregator(nil); err == nil {
		t.Fatal("NewAggregator(nil) must fail loud")
	}
}

// TestAggregate_EmptyWindow — when no events fall in the window, every
// bucket is present with an empty Counts map (so a rendering client
// sees a contiguous time axis without gap arithmetic).
func TestAggregate_EmptyWindow(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	agg, err := events.NewAggregator(bus, events.WithAggregatorClock(fixedAggregatorClock{t: now}))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{"t1"},
			UserIDs:    []string{"u1"},
			SessionIDs: []string{"s1"},
		},
		Window: time.Hour,
		Bucket: time.Minute,
	}, false)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(resp.Buckets) != 60 {
		t.Fatalf("expected 60 buckets (1h/1m), got %d", len(resp.Buckets))
	}
	for i, b := range resp.Buckets {
		if len(b.Counts) != 0 {
			t.Errorf("bucket %d should be empty, got %v", i, b.Counts)
		}
	}
}

// TestAggregate_ConcurrentReuse — D-025 binding test: N=100+
// concurrent Aggregate calls against ONE shared Aggregator+bus, each
// with a different filter, under -race. Asserts:
//
//   - no data races (-race is the gate);
//   - no context bleed (each goroutine's filter is preserved — verified
//     by per-call tenant assertion);
//   - baseline goroutine count restored after all calls return.
func TestAggregate_ConcurrentReuse(t *testing.T) {
	// NB: cannot run with t.Parallel — the baseline goroutine count
	// would be polluted by parallel siblings.
	bus := aggregatorTestBus(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-30 * time.Minute)

	// Publish a batch of events for 8 distinct tenants. Each tenant
	// gets a unique signature so the aggregator's per-call result is
	// distinguishable.
	const tenantCount = 8
	type tenantSig struct {
		tenant string
		count  int64
	}
	sigs := make([]tenantSig, tenantCount)
	for i := range tenantCount {
		t.Helper()
		tenant := "tenant-" + string(rune('A'+i))
		// Publish i+1 events for tenant i, so a tenant=A filter returns 1,
		// tenant=B returns 2, etc.
		for j := range i + 1 {
			publishEvent(t, bus, events.EventTypeRuntimeError, tenant, "u", "s",
				windowStart.Add(time.Duration(j+1)*time.Minute))
		}
		sigs[i] = tenantSig{tenant: tenant, count: int64(i + 1)}
	}

	agg, err := events.NewAggregator(bus, events.WithAggregatorClock(fixedAggregatorClock{t: now}))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	// Baseline goroutine count BEFORE the concurrent fan-in. The
	// aggregator must not leak goroutines past Aggregate's return.
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const concurrency = 128 // > 100 per D-025
	var (
		wg       sync.WaitGroup
		failures atomic.Int64
	)
	wg.Add(concurrency)
	for i := range concurrency {

		go func() {
			defer wg.Done()
			sig := sigs[i%tenantCount]
			resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
				Filter: prototypes.EventFilter{
					TenantIDs:  []string{sig.tenant},
					UserIDs:    []string{"u"},
					SessionIDs: []string{"s"},
				},
				Window: 30 * time.Minute,
				Bucket: time.Minute,
			}, false)
			if err != nil {
				t.Errorf("goroutine %d: Aggregate: %v", i, err)
				failures.Add(1)
				return
			}
			var total int64
			for _, b := range resp.Buckets {
				total += b.Counts["runtime.error"]
			}
			if total != sig.count {
				t.Errorf("goroutine %d (%s): got total=%d, want %d — context bleed?", i, sig.tenant, total, sig.count)
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent failures", failures.Load())
	}

	// Goroutine leak check — the aggregator must join all per-call
	// work before Aggregate returns. Allow a small slack for the
	// runtime's own goroutines settling.
	runtime.GC()
	settled := runtime.NumGoroutine()
	if settled > baseline+2 {
		t.Fatalf("goroutine leak: baseline=%d, after=%d", baseline, settled)
	}
}

// TestAggregate_FailsLoudOnReplayUnavailable — a bus whose driver does
// not implement Replayer (or whose ring is disabled) returns
// ErrReplayUnavailable. Never an empty series that looks like "no
// events" (silent degradation; CLAUDE.md §5).
func TestAggregate_FailsLoudOnReplayUnavailable(t *testing.T) {
	t.Parallel()
	// Build a bus with ReplayBufferSize=0 — the inmem driver still
	// satisfies the Replayer interface but Replay returns
	// ErrReplayUnavailable.
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 4,
		SubscriberBufferSize:     4,
		IdleTimeout:              100 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         0, // <-- replay disabled
	}
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	agg, err := events.NewAggregator(bus)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	_, err = agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{"t1"},
			UserIDs:    []string{"u1"},
			SessionIDs: []string{"s1"},
		},
		Window: time.Hour,
		Bucket: time.Minute,
	}, false)
	if !errors.Is(err, events.ErrReplayUnavailable) {
		t.Fatalf("err = %v, want ErrReplayUnavailable", err)
	}
}

// boundedAggregatorTestBus builds a fresh inmem bus with a ring of exactly
// `capacity` events so a test can force ring eviction below the aggregation
// window (the honest inmem Truncated path).
func boundedAggregatorTestBus(t *testing.T, capacity int) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     16,
		IdleTimeout:              200 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         capacity,
	}
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// aggregateAttributedAdminScopeUsed counts the audit.admin_scope_used
// notices an aggregate against bus emitted. It reads the whole ring via an
// admin Replay (which itself emits exactly one admin_scope_used, folded into
// the snapshot) and subtracts that one, so the returned count is exactly the
// number of notices the preceding aggregate produced. Requires a ring large
// enough not to have evicted any notice.
func aggregateAttributedAdminScopeUsed(t *testing.T, bus events.EventBus) int {
	t.Helper()
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("bus %T does not implement events.Replayer", bus)
	}
	snap, err := rp.Replay(context.Background(), events.Cursor{Sequence: 0}, events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Replay(admin) to count notices: %v", err)
	}
	n := 0
	for _, ev := range snap {
		if ev.Type == events.EventTypeAdminScopeUsed {
			n++
		}
	}
	// Subtract the notice this Replay call itself emitted.
	return n - 1
}

// TestAggregate_WidenedEmitsExactlyOneAdminScopeUsed — a widened (fleet)
// aggregate issues ONE windowed fan-in read, so exactly one
// audit.admin_scope_used is emitted per request (parity with events.list).
func TestAggregate_WidenedEmitsExactlyOneAdminScopeUsed(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-30 * time.Minute)

	// Two sessions in two tenants — a genuine cross-session fixture.
	publishEvent(t, bus, events.EventTypeRuntimeError, "t-A", "u-A", "s-A", windowStart.Add(5*time.Minute))
	publishEvent(t, bus, events.EventTypeRuntimeError, "t-B", "u-B", "s-B", windowStart.Add(6*time.Minute))

	agg, err := events.NewAggregator(bus, events.WithAggregatorClock(fixedAggregatorClock{t: now}))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	// widened=true, empty filter → fan in across every session.
	resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Window: 30 * time.Minute,
		Bucket: time.Minute,
	}, true)
	if err != nil {
		t.Fatalf("Aggregate(widened): %v", err)
	}
	var total int64
	for _, b := range resp.Buckets {
		total += b.Counts["runtime.error"]
	}
	if total != 2 {
		t.Fatalf("widened fan-in total = %d, want 2 (both sessions)", total)
	}
	if got := aggregateAttributedAdminScopeUsed(t, bus); got != 1 {
		t.Fatalf("widened aggregate emitted %d admin_scope_used, want exactly 1", got)
	}
}

// TestAggregate_NonAdminEmitsZeroAdminScopeUsed — a non-admin own-session
// aggregate emits ZERO audit.admin_scope_used. This guards a REAL current
// defect: the old hardcoded Filter{Admin:true} emitted one on EVERY aggregate
// (including non-admin own-session reads) via the inmem Replay-admin path.
func TestAggregate_NonAdminEmitsZeroAdminScopeUsed(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-30 * time.Minute)
	publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1", windowStart.Add(5*time.Minute))

	agg, err := events.NewAggregator(bus, events.WithAggregatorClock(fixedAggregatorClock{t: now}))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	// widened=false, full triple → own-session read, no fan-in, no audit.
	if _, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs: []string{"t1"}, UserIDs: []string{"u1"}, SessionIDs: []string{"s1"},
		},
		Window: 30 * time.Minute,
		Bucket: time.Minute,
	}, false); err != nil {
		t.Fatalf("Aggregate(non-admin): %v", err)
	}
	if got := aggregateAttributedAdminScopeUsed(t, bus); got != 0 {
		t.Fatalf("non-admin own-session aggregate emitted %d admin_scope_used, want 0", got)
	}
}

// TestAggregate_TruncatedUniform_RingEvictionAndBound — a window too wide to
// observe fully returns PARTIAL buckets with Truncated=true, via BOTH honest
// mechanisms uniformly: an inmem ring that evicted older events below the
// window, and the single-read aggregation bound being hit. Never a 400.
func TestAggregate_TruncatedUniform_RingEvictionAndBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-30 * time.Minute)

	t.Run("ring_eviction_below_window", func(t *testing.T) {
		t.Parallel()
		// Ring holds 4 events; publish 8 in the window → 4 evicted.
		bus := boundedAggregatorTestBus(t, 4)
		for i := range 8 {
			publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1",
				windowStart.Add(time.Duration(i+1)*time.Minute))
		}
		agg, err := events.NewAggregator(bus, events.WithAggregatorClock(fixedAggregatorClock{t: now}))
		if err != nil {
			t.Fatalf("NewAggregator: %v", err)
		}
		resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
			Filter: prototypes.EventFilter{TenantIDs: []string{"t1"}, UserIDs: []string{"u1"}, SessionIDs: []string{"s1"}},
			Window: 30 * time.Minute,
			Bucket: time.Minute,
		}, false)
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		if !resp.Truncated {
			t.Fatalf("ring evicted below the window but Truncated=false — partial counts must be flagged")
		}
	})

	t.Run("scan_bound_hit", func(t *testing.T) {
		t.Parallel()
		bus := aggregatorTestBus(t) // large ring, no eviction
		for i := range 6 {
			publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1",
				windowStart.Add(time.Duration(i+1)*time.Minute))
		}
		// A bound of 3 matching events is below the 6 published → HasMore.
		agg, err := events.NewAggregator(bus,
			events.WithAggregatorClock(fixedAggregatorClock{t: now}),
			events.WithAggregateScanBound(3))
		if err != nil {
			t.Fatalf("NewAggregator: %v", err)
		}
		resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
			Filter: prototypes.EventFilter{TenantIDs: []string{"t1"}, UserIDs: []string{"u1"}, SessionIDs: []string{"s1"}},
			Window: 30 * time.Minute,
			Bucket: time.Minute,
		}, false)
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		if !resp.Truncated {
			t.Fatalf("scan bound hit but Truncated=false — partial counts must be flagged, never a 400")
		}
		// The partial buckets still carry the newest `bound` events.
		var total int64
		for _, b := range resp.Buckets {
			total += b.Counts["runtime.error"]
		}
		if total != 3 {
			t.Fatalf("bounded aggregate counted %d, want the newest 3 (partial)", total)
		}
	})
}

// TestAggregate_ExcludesBusInternalNotice — a published bus-internal
// notice-TYPE event (audit.admin_scope_used) is EXCLUDED from the aggregate,
// while real-event-type counts are unaffected. The prior Replay+MatchWire
// substrate counted such notices into type buckets and self-polluted; the
// windowed substrate excludes them (IsBusInternalNotice) — a latent-bug fix
// adopted intentionally.
func TestAggregate_ExcludesBusInternalNotice(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-30 * time.Minute)

	// A real event type — counted.
	publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1", windowStart.Add(5*time.Minute))
	// A bus-internal NOTICE-type event backdated into the window — excluded.
	noticeEv := events.Event{
		Type: events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID: "t1", UserID: "u1", SessionID: "s1",
		}},
		OccurredAt: windowStart.Add(6 * time.Minute).UTC(),
		Payload:    events.AdminScopeUsedPayload{Tenant: "t1", User: "u1", Session: "s1"},
	}
	if err := bus.Publish(context.Background(), noticeEv); err != nil {
		t.Fatalf("publish notice-type event: %v", err)
	}

	agg, err := events.NewAggregator(bus, events.WithAggregatorClock(fixedAggregatorClock{t: now}))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{TenantIDs: []string{"t1"}, UserIDs: []string{"u1"}, SessionIDs: []string{"s1"}},
		Window: 30 * time.Minute,
		Bucket: time.Minute,
	}, false)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var realTotal, noticeTotal int64
	for _, b := range resp.Buckets {
		realTotal += b.Counts["runtime.error"]
		noticeTotal += b.Counts[string(events.EventTypeAdminScopeUsed)]
	}
	if realTotal != 1 {
		t.Fatalf("real-event-type count = %d, want 1 (unaffected)", realTotal)
	}
	if noticeTotal != 0 {
		t.Fatalf("bus-internal notice counted %d times, want 0 (excluded)", noticeTotal)
	}
}

// TestAggregate_UntilClampedSubWindow_ThreadedOntoSubstrate — an Until-clamped
// sub-window is enforced at the substrate (q.Filter.Until), not only by a
// post-read guard. The fixture publishes events NEWER than the clamp's Until
// so that, were the bound filled tail-first without threading Until, the
// in-window matches would be crowded out. Threading Until makes the read
// return exactly the in-sub-window events.
func TestAggregate_UntilClampedSubWindow_ThreadedOntoSubstrate(t *testing.T) {
	t.Parallel()
	bus := aggregatorTestBus(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// The request window is 3h. The filter clamps to the sub-window
	// [now-2h, now-1h). Publish:
	//   - 2 events INSIDE the sub-window (should be counted);
	//   - 5 events NEWER than the sub-window's Until (in [now-1h, now),
	//     should be excluded — they are the ones that would crowd a
	//     tail-first bound read if Until were not threaded).
	inWindowA := now.Add(-90 * time.Minute)
	inWindowB := now.Add(-80 * time.Minute)
	publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1", inWindowA)
	publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1", inWindowB)
	for i := range 5 {
		publishEvent(t, bus, events.EventTypeRuntimeError, "t1", "u1", "s1",
			now.Add(-time.Duration(50-i)*time.Minute)) // all newer than now-1h
	}

	agg, err := events.NewAggregator(bus,
		events.WithAggregatorClock(fixedAggregatorClock{t: now}),
		events.WithAggregateScanBound(3)) // small bound: newest-first would drop the sub-window matches
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs: []string{"t1"}, UserIDs: []string{"u1"}, SessionIDs: []string{"s1"},
			Since: now.Add(-2 * time.Hour),
			Until: now.Add(-1 * time.Hour),
		},
		Window: 3 * time.Hour,
		Bucket: 10 * time.Minute,
	}, false)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var total int64
	for _, b := range resp.Buckets {
		total += b.Counts["runtime.error"]
	}
	if total != 2 {
		t.Fatalf("Until-clamped sub-window total = %d, want 2 (newer-than-Until events must be excluded at the substrate, not crowd the bound)", total)
	}
}
