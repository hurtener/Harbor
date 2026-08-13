package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/drivers/postgres"
)

// TestPostgres_ConcurrentApplies_IdempotentFanout pins the driver's
// conditional sequence/version coordination under concurrency: N≥100
// goroutines ApplyBatch the IDENTICAL batch against ONE shared driver
// under -race. Every apply serializes on the checkpoint row; the first
// advancing apply wins and the rest are idempotent no-ops — the batch is
// applied EXACTLY ONCE (no double-count, no partial row), the final
// checkpoint is the batch's checkpoint, and concurrent readers always
// observe a consistent committed row (never a torn write). This is the
// "transactional idempotent application" half of the contract — the driver
// makes no active-active exactly-once claim (rollups.go: single-writer
// projector), but concurrent duplicate delivery can never double-apply.
func TestPostgres_ConcurrentApplies_IdempotentFanout(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	ctx := context.Background()
	baseline := runtime.NumGoroutine()

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(ctx) }()

	key := rollups.Key{
		BucketStart: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		TenantID:    "tenant-a", UserID: "user-1", SessionID: "session-1", Model: "model-a",
	}
	// Seed checkpoint 1 with one completion; the fan-out batch then adds
	// exactly one more IF and only if it advances.
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 1, Deltas: []rollups.Delta{
		{Key: key, Add: rollups.MeasureSet{LLMCompletions: 1}},
	}}); err != nil {
		t.Fatalf("seed ApplyBatch: %v", err)
	}

	from := rollups.BucketStart(key.BucketStart, rollups.BucketHour)
	q := rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}

	const n = 120
	// One pre-cancelled reader proves cancellation cross-talk is impossible
	// (a cancelled ctx must not affect the other goroutines' results).
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_ = cancelled

	var wg sync.WaitGroup
	var failures atomic.Int64
	var readersOK atomic.Int64

	// Fan-out: every goroutine applies the same batch. All must return nil
	// (the losers are idempotent no-ops, which is nil).
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := rollups.Batch{Checkpoint: 2, Deltas: []rollups.Delta{
				{Key: key, Add: rollups.MeasureSet{LLMCompletions: 1}},
			}}
			if err := s.ApplyBatch(ctx, batch); err != nil {
				failures.Add(1)
				t.Errorf("fan-out apply: %v", err)
			}
		}()
	}
	// Readers: concurrent queries observe 1 or 2 completions — the seed or
	// the seed + one application — never anything else (no torn row, no
	// partial merge), and only the cancelled one fails.
	for i := range n / 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			gctx := ctx
			if idx == 0 {
				gctx = cancelled
			}
			res, err := s.Query(gctx, q)
			if err != nil {
				if idx == 0 && errors.Is(err, context.Canceled) {
					return
				}
				failures.Add(1)
				t.Errorf("reader %d: %v", idx, err)
				return
			}
			if len(res.Rows) != 1 {
				failures.Add(1)
				t.Errorf("reader %d: rows=%d want 1", idx, len(res.Rows))
				return
			}
			got := res.Rows[0].Measures[rollups.MeasureLLMCompletions].N
			if got != 1 && got != 2 {
				failures.Add(1)
				t.Errorf("reader %d: completions=%d want 1 or 2 (never a torn/partial row)", idx, got)
				return
			}
			readersOK.Add(1)
		}(i)
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("%d concurrent assertions failed", failures.Load())
	}

	// Exactly-once: the batch advanced the checkpoint once and added one
	// completion — never more.
	if ck, err := s.Checkpoint(ctx); err != nil || ck != 2 {
		t.Fatalf("checkpoint after fan-out = %d, %v; want 2", ck, err)
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("final query: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 2 {
		t.Fatalf("final completions = %+v; want exactly 2 (seed + ONE fan-out application)", res.Rows)
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

// TestPostgres_ConcurrentFenceAndApply pins the permanent-erasure fence
// under concurrency: a FenceSession races N apply goroutines, some of
// whose batches carry a delta for the erased triple. The checkpoint-row
// serialization point makes the two orders safe: either the apply lands
// first (the fence then erases its rows) or the fence lands first (the
// apply's fence check refuses the whole batch with ErrSessionFenced).
// Either way the erased triple's rows can never appear after the fence,
// and after the wave a direct apply for the triple is deterministically
// refused — the erasure is never resurrected by a replay.
func TestPostgres_ConcurrentFenceAndApply(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	ctx := context.Background()
	baseline := runtime.NumGoroutine()

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(ctx) }()

	h := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	erased := identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-erased"}
	survivor := identity.Identity{TenantID: "tenant-b", UserID: "user-2", SessionID: "session-survivor"}
	erasedKey := rollups.Key{BucketStart: h, TenantID: erased.TenantID, UserID: erased.UserID, SessionID: erased.SessionID, Model: "m"}
	survivorKey := rollups.Key{BucketStart: h, TenantID: survivor.TenantID, UserID: survivor.UserID, SessionID: survivor.SessionID, Model: "m"}

	// Seed checkpoint 1 so the race is between the fence and REAL advancing
	// applies.
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 1, Deltas: []rollups.Delta{
		{Key: survivorKey, Add: rollups.MeasureSet{LLMCompletions: 1}},
	}}); err != nil {
		t.Fatalf("seed ApplyBatch: %v", err)
	}

	const n = 60
	var wg sync.WaitGroup
	var applyFailures atomic.Int64
	var fencedRejections atomic.Int64

	// The fence races the applies.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.FenceSession(ctx, erased); err != nil {
			t.Errorf("FenceSession: %v", err)
		}
	}()

	// Each apply batch carries a delta for the erased triple AND one for
	// the survivor. Depending on the interleaving it either lands whole
	// (before the fence) or is refused whole with ErrSessionFenced (the
	// batch-level rejection — the survivor delta never rides alone).
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: uint64(2 + i), Deltas: []rollups.Delta{
				{Key: erasedKey, Add: rollups.MeasureSet{LLMCompletions: 1}},
				{Key: survivorKey, Add: rollups.MeasureSet{LLMCompletions: 1}},
			}})
			if err == nil {
				return
			}
			if errors.Is(err, rollups.ErrSessionFenced) {
				fencedRejections.Add(1)
				return
			}
			applyFailures.Add(1)
			t.Errorf("apply %d: %v", i, err)
		}(i)
	}
	wg.Wait()

	if applyFailures.Load() != 0 {
		t.Fatalf("%d unexpected apply failures", applyFailures.Load())
	}

	// Post-condition: the erased triple is fenced, its rows NEVER appear,
	// and a direct apply for it is deterministically refused — the fence
	// outlives every interleaving and every replay.
	if f, err := s.IsFenced(ctx, erased); err != nil || !f {
		t.Fatalf("IsFenced(erased) = %v, %v; want true", f, err)
	}
	from := rollups.BucketStart(h, rollups.BucketHour)
	res, err := s.Query(ctx, rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, r := range res.Rows {
		if r.Dimensions[rollups.DimensionTenant] == erased.TenantID {
			t.Fatalf("erased triple's row survived the fence: %+v", r)
		}
	}

	// Direct replay for the erased triple: refused loudly, no resurrection.
	ck, err := s.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	err = s.ApplyBatch(ctx, rollups.Batch{Checkpoint: ck + 1, Deltas: []rollups.Delta{
		{Key: erasedKey, Add: rollups.MeasureSet{LLMCompletions: 1}},
	}})
	if !errors.Is(err, rollups.ErrSessionFenced) {
		t.Fatalf("direct replay err = %v; want ErrSessionFenced", err)
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

// TestPostgres_ConcurrentDistinctBatches pins the serialized-advance
// invariant for DISTINCT advancing batches: N goroutines each apply a
// batch with a unique checkpoint and a unique key. The final checkpoint is
// ALWAYS the maximum checkpoint (the max-ckpt batch advances regardless of
// order; every other batch either advances before it or no-ops), the max
// batch's row is applied exactly once, and no apply errors — the
// conditional sequence logic coordinates the winners without lost or
// doubled applications of any individual batch.
func TestPostgres_ConcurrentDistinctBatches(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	ctx := context.Background()

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(ctx) }()

	const n = 40
	h := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := rollups.Key{
				BucketStart: h,
				TenantID:    fmt.Sprintf("tenant-%02d", idx%4),
				UserID:      "user-1",
				SessionID:   fmt.Sprintf("session-%02d", idx),
				Model:       "model-a",
			}
			errs[idx] = s.ApplyBatch(ctx, rollups.Batch{Checkpoint: uint64(idx + 2), Deltas: []rollups.Delta{
				{Key: key, Add: rollups.MeasureSet{LLMCompletions: 1}},
			}})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	// The final checkpoint is always the maximum — the max-ckpt batch
	// advances regardless of acquisition order.
	if ck, err := s.Checkpoint(ctx); err != nil || ck != n+1 {
		t.Fatalf("checkpoint = %d, %v; want %d (the max checkpoint)", ck, err, n+1)
	}
	// The max-ckpt batch's row is present exactly once.
	from := rollups.BucketStart(h, rollups.BucketHour)
	res, err := s.Query(ctx, rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Filter:   rollups.Filter{SessionIDs: []string{fmt.Sprintf("session-%02d", n-1)}},
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 1 {
		t.Fatalf("max-ckpt row = %+v; want exactly 1 completion", res.Rows)
	}
}
