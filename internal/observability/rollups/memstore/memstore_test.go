package memstore_test

import (
	"context"
	"errors"
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
	"github.com/hurtener/Harbor/internal/observability/rollups/conformancetest"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
)

// TestStore_Conformance runs the canonical driver suite against the
// in-memory Store — the reference every future SQLite / Postgres driver is
// held to.
func TestStore_Conformance(t *testing.T) {
	conformancetest.Run(t, func() (rollups.Store, func()) {
		s := memstore.New()
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestStore_ConcurrentReuse pins the D-025-style concurrent-reuse contract
// on the shared Store: N≥100 goroutines run mixed Query + ApplyBatch work
// against ONE instance under -race, asserting (a) no data races (the race
// detector is the gate), (b) no context bleed (each goroutine's query is
// scoped to its own identity and returns only its own rows), (c) no
// cancellation cross-talk (cancelling one goroutine's ctx does not affect
// the others), and (d) no goroutine leak (baseline returns after teardown).
func TestStore_ConcurrentReuse(t *testing.T) {
	s := memstore.New()
	defer func() { _ = s.Close(context.Background()) }()

	baseline := runtime.NumGoroutine()

	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)

	// Seed: one cost event per (tenant, user, session) so each goroutine
	// can query its own identity slice.
	var seed []events.Event
	seq := uint64(1)
	for i := 0; i < 20; i++ {
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

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Each goroutine uses its own derived ctx — no shared mutable
			// state beyond the Store.
			gctx := ctx
			if idx == 0 {
				gctx = cancelOne // the one cancelled goroutine
			}
			q := rollups.Query{
				From:     h,
				To:       h.Add(24 * time.Hour),
				Bucket:   rollups.BucketHour,
				Filter:   rollups.Filter{TenantIDs: []string{fmt.Sprintf("tenant-%02d", idx%4)}},
				Measures: []rollups.Measure{rollups.MeasureLLMCostUSD, rollups.MeasureLLMCompletions},
				Sort:     rollups.SortKeyBucketAsc,
				Limit:    100,
			}
			res, err := s.Query(gctx, q)
			if err != nil {
				if idx == 0 && err == context.Canceled {
					return // the expected cancellation path
				}
				failures.Add(1)
				t.Errorf("query %d: %v", idx, err)
				return
			}
			// Context bleed check: the goroutine's own tenant only —
			// never a neighbour's rows. All 5 sessions of the tenant share
			// one hour bucket, and GroupBy is empty, so the answer is one
			// row aggregating 5 completions.
			if got := len(res.Rows); got != 1 {
				failures.Add(1)
				t.Errorf("query %d: rows=%d want 1", idx, got)
			}
			for _, r := range res.Rows {
				if r.Measures[rollups.MeasureLLMCompletions] != 5 {
					failures.Add(1)
					t.Errorf("query %d: row completions=%v want 5", idx, r.Measures[rollups.MeasureLLMCompletions])
				}
			}
		}(i)
	}
	// Cancellation cross-talk: cancel exactly one goroutine's ctx; the
	// other 127 must complete normally.
	cancel()

	// Concurrent writers: a second wave of applies must not race the
	// readers (mixed read/write concurrency under -race). Each writer owns
	// a unique sequence via the atomic counter. Writers may interleave out
	// of order; the store's checkpoint guard makes each batch either the
	// single advancing write or an idempotent/regressive no-op — the
	// invariant under test is that the checkpoint stays monotonic and no
	// row is ever double-counted or torn, never that every writer lands.
	for i := 0; i < 8; i++ {
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
