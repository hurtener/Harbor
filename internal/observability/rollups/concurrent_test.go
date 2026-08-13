package rollups_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestProjector_ConcurrentReuse pins the D-025-style concurrent-reuse
// contract on the shared Projector + Store pair: N≥100 goroutines run a mix
// of Query (read), Quality (read), and Advance (idempotent write) against
// ONE shared projector under -race, asserting no races, no context bleed,
// no cancellation cross-talk, and no goroutine leak.
func TestProjector_ConcurrentReuse(t *testing.T) {
	ctx := context.Background()
	baseline := runtime.NumGoroutine()

	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)

	// A fixed log of 600 events (300 cost records + 300 task completions)
	// over 4 tenants — the source every goroutine reads.
	quad := func(i int) identity.Quadruple {
		return eventID(fmt.Sprintf("tenant-%02d", i%4), fmt.Sprintf("user-%02d", i%3), fmt.Sprintf("session-%02d", i))
	}
	var log []events.Event
	for i := 0; i < 300; i++ {
		q := quad(i)
		log = append(log, events.Event{
			Type:       llm.EventTypeCostRecorded,
			Identity:   q,
			OccurredAt: h.Add(time.Duration(i) * time.Second),
			Sequence:   uint64(2*i + 1),
			Payload:    llm.CostRecordedPayload{Identity: q, Model: "model-a", Cost: llm.Cost{TotalCost: 1.0, Currency: "USD"}, Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20}},
		})
		log = append(log, events.Event{
			Type:       tasks.EventTypeTaskCompleted,
			Identity:   q,
			OccurredAt: h.Add(time.Duration(i) * time.Second),
			Sequence:   uint64(2*i + 2),
			Payload:    tasks.TaskCompletedPayload{TaskID: tasks.TaskID(fmt.Sprintf("t-%d", i))},
		})
	}
	src := &testSource{events: log}
	store := memstore.New()
	defer func() { _ = store.Close(ctx) }()

	// The shared projector: one instance, N consumers.
	p, err := rollups.NewProjector(src, store, rollups.WithProjectorBatchSize(37))
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}

	// One goroutine's ctx is cancelled mid-flight (cancellation
	// cross-talk); the other readers must complete normally.
	cancelled, cancelReader := context.WithCancel(ctx)
	cancelReader()

	const readers = 120
	var wg sync.WaitGroup
	var failures atomic.Int64

	// Readers: each queries its own tenant (context-bleed check) and reads
	// quality; a handful of Advancers drain the shared log concurrently.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			gctx := ctx
			if idx == 0 {
				gctx = cancelled
			}
			q := rollups.Query{
				From:     h,
				To:       h.Add(2 * time.Hour),
				Bucket:   rollups.BucketHour,
				Filter:   rollups.Filter{TenantIDs: []string{fmt.Sprintf("tenant-%02d", idx%4)}},
				Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureTasksCompleted},
				Sort:     rollups.SortKeyBucketAsc,
				Limit:    100,
			}
			res, err := store.Query(gctx, q)
			if err != nil {
				if idx == 0 && err == context.Canceled {
					return // the expected cancellation path
				}
				failures.Add(1)
				t.Errorf("reader %d: %v", idx, err)
				return
			}
			// Context bleed + drain consistency: the reader's own tenant
			// only — 0 or 1 rows (the drain may not have landed every
			// event yet), and each measure is a partial or complete
			// per-tenant sum in [0,75] (a batch boundary may split a
			// session's cost/task pair, so the two are not equal
			// mid-drain).
			if len(res.Rows) > 1 {
				failures.Add(1)
				t.Errorf("reader %d: rows=%d want ≤ 1 (one hour bucket)", idx, len(res.Rows))
				return
			}
			if len(res.Rows) == 1 {
				cost := res.Rows[0].Measures[rollups.MeasureLLMCostMicros].N
				tasksDone := res.Rows[0].Measures[rollups.MeasureTasksCompleted].N
				if cost < 0 || cost > 75_000_000 || tasksDone < 0 || tasksDone > 75 {
					failures.Add(1)
					t.Errorf("reader %d: cost=%d tasks=%d want cost in [0,75_000_000] tasks in [0,75]", idx, cost, tasksDone)
				}
			}
		}(i)
	}

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Advancers are idempotent: concurrent Advances serialise on
			// the store's checkpoint guard; every drain lands exactly once.
			if _, err := p.Advance(ctx); err != nil {
				failures.Add(1)
				t.Errorf("advancer %d: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent-reuse assertions failed", failures.Load())
	}

	// The concurrent wave drains at most 8 batches (8 × 37); the rest is
	// drained sequentially, then the log must be fully applied exactly
	// once: watermark 600, and one final empty Advance reports caught up.
	if err := p.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp after wave: %v", err)
	}
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 600 {
		t.Fatalf("watermark = %d; want 600", q.Watermark)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state = %q; want current", q.State)
	}
	caughtUp, err := p.Advance(ctx)
	if err != nil {
		t.Fatalf("final Advance: %v", err)
	}
	if !caughtUp {
		t.Fatal("final Advance must report caught up")
	}

	// Post-drain exactness: every tenant has all 150 of its events.
	for i := 0; i < 4; i++ {
		res, err := store.Query(ctx, rollups.Query{
			From:     h,
			To:       h.Add(2 * time.Hour),
			Bucket:   rollups.BucketHour,
			Filter:   rollups.Filter{TenantIDs: []string{fmt.Sprintf("tenant-%02d", i)}},
			Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureTasksCompleted},
			Sort:     rollups.SortKeyBucketAsc,
			Limit:    100,
		})
		if err != nil {
			t.Fatalf("post-drain query tenant-%d: %v", i, err)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("post-drain rows tenant-%d = %d; want 1", i, len(res.Rows))
		}
		if got := res.Rows[0].Measures[rollups.MeasureLLMCostMicros].N; got != 75_000_000 {
			t.Fatalf("post-drain cost tenant-%d = %d micros; want 75_000_000", i, got)
		}
		if got := res.Rows[0].Measures[rollups.MeasureTasksCompleted].N; got != 75 {
			t.Fatalf("post-drain tasks tenant-%d = %d; want 75", i, got)
		}
	}

	// Goroutine-leak check.
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
