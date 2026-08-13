package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/conformancetest"
	"github.com/hurtener/Harbor/internal/observability/rollups/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestStore_Conformance_TempDirFile drives the canonical driver suite
// against a fresh tempdir-backed SQLite database. Each top-level subtest
// gets its own database file so subtests are independent.
func TestStore_Conformance_TempDirFile(t *testing.T) {
	conformancetest.Run(t, func() (rollups.Store, func()) {
		dir := t.TempDir()
		dsn := filepath.Join(dir, "rollups.sqlite")
		s, err := sqlite.New(dsn)
		if err != nil {
			t.Fatalf("sqlite.New(%q): %v", dsn, err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestStore_Conformance_InMemory exercises the same suite against the
// `:memory:` DSN — the degenerate dev case that removes the disk seek
// path from the test budget while still exercising the full SQL stack.
func TestStore_Conformance_InMemory(t *testing.T) {
	conformancetest.Run(t, func() (rollups.Store, func()) {
		s, err := sqlite.New(":memory:")
		if err != nil {
			t.Fatalf("sqlite.New(:memory:): %v", err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestStore_RestartDurableCheckpointRowsAndFence pins the durable
// restart contract: the watermark, the projection rows (retention
// horizon), and the erasure fence all live in the SQLite file, so a
// fresh Store over the same DSN resumes exactly where the last run
// stopped — the checkpoint does not regress, the sums do not
// double-count on replay, and an erased session stays erased. This is
// the driver-side half of the projector's restart catch-up path.
func TestStore_RestartDurableCheckpointRowsAndFence(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "rollups.sqlite")

	s, err := sqlite.New(dsn)
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}

	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	quadA := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-1"}}
	quadB := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-2"}}

	evs := []events.Event{
		costEvent(1, h.Add(time.Minute), quadA, "model-a", 1.25, 100, 50, 800),
		costEvent(2, h.Add(2*time.Minute), quadA, "model-a", 0.75, 200, 40, 400),
		taskEvent(3, h.Add(3*time.Minute), quadB, tasks.EventTypeTaskCompleted),
	}
	apply(ctx, t, s, evs...)

	// Fence session-1 before the restart: the fence must survive it.
	if err := s.FenceSession(ctx, quadA.Identity); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}

	if ck, err := s.Checkpoint(ctx); err != nil || ck != 3 {
		t.Fatalf("checkpoint = %d, %v; want 3", ck, err)
	}
	// Retention is captured AFTER the fence: session-1's rows (12:01,
	// 12:02) were erased, so the surviving horizon is exactly session-2's
	// task row at 12:03.
	wantOld, wantNew, err := s.Retention(ctx)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if !wantOld.Equal(h.Add(3*time.Minute)) || !wantNew.Equal(h.Add(3*time.Minute)) {
		t.Fatalf("retention = %v..%v; want %v..%v (only session-2's 12:03 row survives the fence)", wantOld, wantNew, h.Add(3*time.Minute), h.Add(3*time.Minute))
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// "Restart": a fresh Store over the SAME file must resume at the
	// durable checkpoint (3), not at 0.
	s2, err := sqlite.New(dsn)
	if err != nil {
		t.Fatalf("sqlite.New (restart): %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()

	if ck, err := s2.Checkpoint(ctx); err != nil || ck != 3 {
		t.Fatalf("restart checkpoint = %d, %v; want 3 (durable watermark)", ck, err)
	}
	old, new, err := s2.Retention(ctx)
	if err != nil {
		t.Fatalf("Retention (restart): %v", err)
	}
	if !old.Equal(wantOld) || !new.Equal(wantNew) {
		t.Fatalf("retention after restart = %v..%v; want %v..%v", old, new, wantOld, wantNew)
	}
	// The durable fence: the erased session is still fenced.
	if f, err := s2.IsFenced(ctx, quadA.Identity); err != nil || !f {
		t.Fatalf("IsFenced after restart = %v, %v; want true", f, err)
	}

	// Replay idempotency + erasure durability after the restart: the
	// surviving session-2 task row is applied exactly once (no
	// double-count), and the fenced session-1 cost rows stay erased —
	// replay cannot resurrect them.
	apply(ctx, t, s2, evs...)
	from := rollups.BucketStart(h, rollups.BucketHour)
	res, err := s2.Query(ctx, rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureTasksCompleted},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query (restart replay): %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows after restart replay = %d; want 1 (session-1 fenced, session-2 task only)", len(res.Rows))
	}
	if got := sumCost(res); got != 0 {
		t.Fatalf("cost after restart replay = %d micros; want 0 (session-1's rows are erased and stay erased)", got)
	}
	if got := sumTasks(res, rollups.MeasureTasksCompleted); got != 1 {
		t.Fatalf("tasks after restart replay = %d; want 1 (no double-count)", got)
	}
	// A late event for the fenced triple is STILL refused after restart.
	late := costEvent(4, h.Add(4*time.Minute), quadA, "model-a", 100, 10, 10, 10)
	ds, err := rollups.Extract(late)
	if err != nil {
		t.Fatalf("Extract(late): %v", err)
	}
	if err := s2.ApplyBatch(ctx, rollups.Batch{Checkpoint: 4, Deltas: ds}); !errors.Is(err, rollups.ErrSessionFenced) {
		t.Fatalf("post-restart late apply err = %v; want ErrSessionFenced", err)
	}
}

// TestStore_ErasureFencePermanent pins the driver-side erasure fence on
// top of the conformance suite: the fence survives Rebuild AND a full
// close/reopen cycle, and a late event is refused forever — the erasure
// is never resurrected by reprojection or by an asynchronous tail event.
func TestStore_ErasureFencePermanent(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "rollups.sqlite")

	s, err := sqlite.New(dsn)
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}

	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-1"}}
	other := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-2"}}

	apply(ctx, t, s,
		costEvent(1, h, triple, "model-a", 1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), other, "model-a", 2, 10, 10, 10),
	)

	if err := s.FenceSession(ctx, triple.Identity); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	from := rollups.BucketStart(h, rollups.BucketHour)
	res := mustQuery(ctx, t, s, from, from.Add(time.Hour), rollups.BucketHour)
	if got := sumCost(res); got != 2_000_000 {
		t.Fatalf("cost after fence = %d micros; want 2_000_000 (other session only)", got)
	}

	// Rebuild clears rows + checkpoint but never the fence.
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if f, err := s.IsFenced(ctx, triple.Identity); err != nil || !f {
		t.Fatalf("IsFenced after rebuild = %v, %v; want true", f, err)
	}

	// Reopen the same file: the fence row is durable.
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := sqlite.New(dsn)
	if err != nil {
		t.Fatalf("sqlite.New (reopen): %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()
	if f, err := s2.IsFenced(ctx, triple.Identity); err != nil || !f {
		t.Fatalf("IsFenced after reopen = %v, %v; want true (durable fence)", f, err)
	}
	// Reprojection of the log (fenced triples dropped at ingestion) must
	// reconstruct only the survivor.
	var survivor []rollups.Delta
	for _, ev := range []events.Event{
		costEvent(1, h, triple, "model-a", 1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), other, "model-a", 2, 10, 10, 10),
	} {
		fenced, err := s2.IsFenced(ctx, ev.Identity.Identity)
		if err != nil {
			t.Fatalf("IsFenced(replay): %v", err)
		}
		if fenced {
			continue
		}
		ds, err := rollups.Extract(ev)
		if err != nil {
			t.Fatalf("Extract(replay): %v", err)
		}
		survivor = append(survivor, ds...)
	}
	if err := s2.ApplyBatch(ctx, rollups.Batch{Checkpoint: 2, Deltas: survivor}); err != nil {
		t.Fatalf("replay ApplyBatch: %v", err)
	}
	res = mustQuery(ctx, t, s2, from, from.Add(time.Hour), rollups.BucketHour)
	if got := sumCost(res); got != 2_000_000 {
		t.Fatalf("cost after rebuild+replay = %d micros; want 2_000_000 (fenced session never resurrected)", got)
	}
}

// TestStore_PrecisionExactIntegers pins the exact-integer precision
// model through the SQLite round-trip: cost sums are exact micro-units
// (no float drift, no float storage), and a counter above 2^53
// round-trips exactly — the schema has no REAL/DOUBLE column and the
// driver never converts a measure through float64.
func TestStore_PrecisionExactIntegers(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite.New(:memory:): %v", err)
	}
	defer func() { _ = s.Close(ctx) }()

	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	quad := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-1"}}

	// 0.1 + 0.2 + (0.1+0.2) must sum to EXACTLY 600_000 micro-units —
	// the classic float artifact must not appear.
	apply(ctx, t, s,
		costEvent(1, h, quad, "m", 0.1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), quad, "m", 0.2, 10, 10, 10),
		costEvent(3, h.Add(2*time.Minute), quad, "m", 0.1+0.2, 10, 10, 10),
	)
	from := rollups.BucketStart(h, rollups.BucketHour)
	res, err := s.Query(ctx, rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := sumCost(res); got != 600_000 {
		t.Fatalf("0.1+0.2+(0.1+0.2) = %d micros; want 600_000 (0.60 USD exactly)", got)
	}
	if got := res.Rows[0].Measures[rollups.MeasureLLMCostMicros].Scale; got != rollups.CostScaleMicros {
		t.Fatalf("cost scale = %d; want %d", got, rollups.CostScaleMicros)
	}

	// The >2^53 counter guarantee through the full SQLite round-trip:
	// 2^53+1 is not representable in float64, so a REAL column or a
	// float64 conversion would lose the low bit.
	big := int64(1<<53) + 1
	h2 := h.Add(10 * time.Minute)
	delta := rollups.Delta{
		Key: rollups.Key{BucketStart: h2, TenantID: "tenant-b", UserID: "user-2", SessionID: "session-2", Model: "model-b"},
		Add: rollups.MeasureSet{LLMTokensTotal: big},
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 100, Deltas: []rollups.Delta{delta}}); err != nil {
		t.Fatalf("ApplyBatch (big counter): %v", err)
	}
	from2 := rollups.BucketStart(h2, rollups.BucketHour)
	res, err = s.Query(ctx, rollups.Query{
		From:     from2,
		To:       from2.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Filter:   rollups.Filter{TenantIDs: []string{"tenant-b"}},
		Measures: []rollups.Measure{rollups.MeasureLLMTokensTotal},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query (big counter): %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMTokensTotal].N != big {
		t.Fatalf("big counter = %+v; want exactly %d (float64 would lose the low bit)", res.Rows, big)
	}
}

// TestStore_ConcurrentReuse pins the D-025-style concurrent-reuse
// contract on the shared SQLite Store: N≥100 goroutines run mixed Query
// + ApplyBatch work against ONE instance under -race, asserting (a) no
// data races (the race detector is the gate), (b) no context bleed (each
// goroutine's query is scoped to its own tenant and returns only its own
// rows), (c) no cancellation cross-talk (cancelling one goroutine's ctx
// does not affect the others), and (d) no goroutine leak (baseline
// returns after teardown).
func TestStore_ConcurrentReuse(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite.New(:memory:): %v", err)
	}
	defer func() { _ = s.Close(ctx) }()

	baseline := runtime.NumGoroutine()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)

	// Seed: one cost event per (tenant, user, session) so each goroutine
	// can query its own identity slice.
	var seed []events.Event
	seq := uint64(1)
	for i := range 20 {
		quad := identity.Quadruple{Identity: identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%02d", i%4),
			UserID:    fmt.Sprintf("user-%02d", i%5),
			SessionID: fmt.Sprintf("session-%02d", i),
		}}
		seed = append(seed, events.Event{
			Type:       llm.EventTypeCostRecorded,
			Identity:   quad,
			OccurredAt: h.Add(time.Duration(i) * time.Minute),
			Sequence:   seq,
			Payload: llm.CostRecordedPayload{
				Identity: quad,
				Model:    "model-a",
				Cost:     llm.Cost{TotalCost: 1.0, Currency: "USD"},
				Usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20},
			},
		})
		seq++
	}
	var deltas []rollups.Delta
	for _, ev := range seed {
		ds, err := rollups.Extract(ev)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		deltas = append(deltas, ds...)
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: seq - 1, Deltas: deltas}); err != nil {
		t.Fatalf("seed ApplyBatch: %v", err)
	}

	const n = 128
	var wg sync.WaitGroup
	var failures atomic.Int64
	cancelOne, cancel := context.WithCancel(ctx)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			gctx := ctx
			if idx == 0 {
				gctx = cancelOne // the one cancelled goroutine
			}
			q := rollups.Query{
				From:     h,
				To:       h.Add(24 * time.Hour),
				Bucket:   rollups.BucketHour,
				Filter:   rollups.Filter{TenantIDs: []string{fmt.Sprintf("tenant-%02d", idx%4)}},
				Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureLLMCompletions},
				Sort:     rollups.SortKeyBucketAsc,
				Limit:    100,
			}
			res, err := s.Query(gctx, q)
			if err != nil {
				if idx == 0 && errors.Is(err, context.Canceled) {
					return // the expected cancellation path (possibly wrapped by the driver)
				}
				failures.Add(1)
				t.Errorf("query %d: %v", idx, err)
				return
			}
			// Context bleed check: the goroutine's own tenant only —
			// never a neighbour's rows. All 5 sessions of the tenant
			// share one hour bucket, and GroupBy is empty, so the answer
			// is one row aggregating 5 completions.
			if got := len(res.Rows); got != 1 {
				failures.Add(1)
				t.Errorf("query %d: rows=%d want 1", idx, got)
				return
			}
			for _, r := range res.Rows {
				if r.Measures[rollups.MeasureLLMCompletions].N != 5 {
					failures.Add(1)
					t.Errorf("query %d: row completions=%d want 5", idx, r.Measures[rollups.MeasureLLMCompletions].N)
				}
			}
		}(i)
	}
	cancel() // cancellation cross-talk: exactly one goroutine's ctx dies

	// Concurrent writers: a second wave of applies must not race the
	// readers (mixed read/write concurrency under -race). Each writer owns
	// a unique sequence via the atomic counter; writers may interleave out
	// of order, and the checkpoint guard makes each batch either the
	// single advancing write or an idempotent/regressive no-op — the
	// invariant under test is that the checkpoint stays monotonic and no
	// row is ever double-counted or torn, never that every writer lands.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := events.Event{
				Type:       llm.EventTypeCostRecorded,
				Identity:   identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-extra", UserID: "u", SessionID: "s"}},
				OccurredAt: h.Add(time.Minute),
				Sequence:   atomic.AddUint64(&seq, 1),
				Payload: llm.CostRecordedPayload{
					Model: "model-a",
					Cost:  llm.Cost{TotalCost: 1.0},
				},
			}
			ds, err := rollups.Extract(ev)
			if err != nil {
				failures.Add(1)
				t.Errorf("writer extract: %v", err)
				return
			}
			if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: ev.Sequence, Deltas: ds}); err != nil &&
				!errors.Is(err, rollups.ErrSessionFenced) {
				failures.Add(1)
				t.Errorf("writer apply: %v", err)
			}
		}()
	}

	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent-reuse assertions failed", failures.Load())
	}

	// Goroutine-leak check: after all work joins, the baseline must be
	// restored. Poll briefly for the scheduler to settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Fatalf("goroutine leak: baseline=%d now=%d", baseline, got)
	}
}

// TestStore_ApplyBatchOverflowFailsLoudly pins the checked-accumulation
// contract on the write path: a delta whose merge would overflow a
// measure's exact int64 representation rejects the WHOLE batch with
// rollups.ErrMeasureOverflow — nothing is applied, no partial row exists,
// and the checkpoint does not advance.
func TestStore_ApplyBatchOverflowFailsLoudly(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite.New(:memory:): %v", err)
	}
	defer func() { _ = s.Close(ctx) }()

	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	k := rollups.Key{BucketStart: h, TenantID: "tenant-a", UserID: "user-1", SessionID: "session-1", Model: "model-a"}

	// A valid first batch establishes a row near the int64 boundary.
	if err := s.ApplyBatch(ctx, rollups.Batch{
		Checkpoint: 1,
		Deltas:     []rollups.Delta{{Key: k, Add: rollups.MeasureSet{LLMTokensTotal: math.MaxInt64 - 5}}},
	}); err != nil {
		t.Fatalf("seed ApplyBatch: %v", err)
	}

	// The overflow batch carries BOTH a fitting delta and the overflow
	// delta. The whole batch must be refused: the fitting delta is NOT
	// applied, the seed row is untouched, and the checkpoint stays at 1.
	other := rollups.Key{BucketStart: h, TenantID: "tenant-b", UserID: "user-2", SessionID: "session-2", Model: "model-b"}
	err = s.ApplyBatch(ctx, rollups.Batch{
		Checkpoint: 2,
		Deltas: []rollups.Delta{
			{Key: other, Add: rollups.MeasureSet{LLMCompletions: 1}}, // would fit
			{Key: k, Add: rollups.MeasureSet{LLMTokensTotal: 6}},     // MaxInt64-5 + 6 overflows
		},
	})
	if !errors.Is(err, rollups.ErrMeasureOverflow) {
		t.Fatalf("overflow ApplyBatch err = %v; want ErrMeasureOverflow", err)
	}
	if ck, err := s.Checkpoint(ctx); err != nil || ck != 1 {
		t.Fatalf("checkpoint after refused batch = %d, %v; want 1 (no advance)", ck, err)
	}
	res, err := s.Query(ctx, rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Filter:   rollups.Filter{TenantIDs: []string{"tenant-b"}},
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("post-refusal query: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("rows after refused batch = %d; want 0 (no partial application)", len(res.Rows))
	}
	res, err = s.Query(ctx, rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMTokensTotal},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("seed query: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMTokensTotal].N != math.MaxInt64-5 {
		t.Fatalf("seed row after refused batch = %+v; want 1 row with %d tokens", res.Rows, int64(math.MaxInt64-5))
	}
}

// TestStore_New_RejectsEmptyDSN locks in the no-silent-degradation
// rule: an empty DSN is a misconfiguration, not a default-fallback
// invitation.
func TestStore_New_RejectsEmptyDSN(t *testing.T) {
	if _, err := sqlite.New(""); err == nil {
		t.Fatal("sqlite.New with empty DSN: err=nil, want error")
	}
}

// TestStore_Close_Idempotent — repeat Close calls are safe and return
// nil after the first.
func TestStore_Close_Idempotent(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second Close (should be no-op): %v", err)
	}
}

// TestStore_ClosedStoreRefusesWork — every operation on a closed store
// returns rollups.ErrClosed, not a half-closed-pool race.
func TestStore_ClosedStoreRefusesWork(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Checkpoint(ctx); !errors.Is(err, rollups.ErrClosed) {
		t.Fatalf("Checkpoint after close err = %v; want ErrClosed", err)
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 1}); !errors.Is(err, rollups.ErrClosed) {
		t.Fatalf("ApplyBatch after close err = %v; want ErrClosed", err)
	}
	if _, err := s.IsFenced(ctx, identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}); !errors.Is(err, rollups.ErrClosed) {
		t.Fatalf("IsFenced after close err = %v; want ErrClosed", err)
	}
}

// --- fixtures ------------------------------------------------------------

// costEvent builds a successfully-persisted `llm.cost.recorded` event.
func costEvent(seq uint64, at time.Time, quad identity.Quadruple, model string, costUSD float64, prompt, completion, latencyMS int) events.Event {
	return events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   quad,
		OccurredAt: at,
		Sequence:   seq,
		Payload: llm.CostRecordedPayload{
			Identity:   quad,
			Model:      model,
			Cost:       llm.Cost{TotalCost: costUSD, Currency: "USD"},
			Usage:      llm.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion, LatencyMS: int64(latencyMS)},
			OccurredAt: at,
		},
	}
}

// taskEvent builds a successfully-persisted task outcome event.
func taskEvent(seq uint64, at time.Time, quad identity.Quadruple, typ events.EventType) events.Event {
	return events.Event{
		Type:       typ,
		Identity:   quad,
		OccurredAt: at,
		Sequence:   seq,
		Payload:    tasks.TaskCompletedPayload{TaskID: "t"},
	}
}

// apply applies a run of contiguous events (sequences 1..len in order)
// as one atomic batch.
func apply(ctx context.Context, t *testing.T, s rollups.Store, evs ...events.Event) {
	t.Helper()
	if len(evs) == 0 {
		return
	}
	var deltas []rollups.Delta
	for _, ev := range evs {
		ds, err := rollups.Extract(ev)
		if err != nil {
			t.Fatalf("Extract(seq=%d): %v", ev.Sequence, err)
		}
		deltas = append(deltas, ds...)
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: evs[len(evs)-1].Sequence, Deltas: deltas}); err != nil {
		t.Fatalf("ApplyBatch(checkpoint=%d): %v", evs[len(evs)-1].Sequence, err)
	}
}

func mustQuery(ctx context.Context, t *testing.T, s rollups.Store, from, to time.Time, bucket rollups.BucketSize) rollups.Result {
	t.Helper()
	res, err := s.Query(ctx, rollups.Query{
		From:     from,
		To:       to,
		Bucket:   bucket,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureLLMCompletions, rollups.MeasureTasksCompleted},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    1000,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return res
}

func sumCost(res rollups.Result) int64 {
	var total int64
	for _, r := range res.Rows {
		total += r.Measures[rollups.MeasureLLMCostMicros].N
	}
	return total
}

func sumTasks(res rollups.Result, m rollups.Measure) int64 {
	var total int64
	for _, r := range res.Rows {
		total += r.Measures[m].N
	}
	return total
}
