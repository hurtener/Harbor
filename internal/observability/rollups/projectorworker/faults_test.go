package projectorworker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
	"github.com/hurtener/Harbor/internal/observability/rollups/projectorworker"
)

// faultyStore wraps a memstore with per-method failure injection —
// the deterministic knob for the worker's store-error paths.
type faultyStore struct {
	*memstore.Store
	failCheckpoint bool
	failApplyBatch bool
	failRebuild    bool
	failRetention  bool
	failIsFenced   bool
}

func (f *faultyStore) Checkpoint(ctx context.Context) (uint64, error) {
	if f.failCheckpoint {
		return 0, errors.New("faulty: checkpoint read")
	}
	return f.Store.Checkpoint(ctx)
}

func (f *faultyStore) ApplyBatch(ctx context.Context, b rollups.Batch) error {
	if f.failApplyBatch {
		return errors.New("faulty: apply")
	}
	return f.Store.ApplyBatch(ctx, b)
}

func (f *faultyStore) Rebuild(ctx context.Context) error {
	if f.failRebuild {
		return errors.New("faulty: rebuild")
	}
	return f.Store.Rebuild(ctx)
}

func (f *faultyStore) Retention(ctx context.Context) (time.Time, time.Time, error) {
	if f.failRetention {
		return time.Time{}, time.Time{}, errors.New("faulty: retention")
	}
	return f.Store.Retention(ctx)
}

func (f *faultyStore) IsFenced(ctx context.Context, id identity.Identity) (bool, error) {
	if f.failIsFenced {
		return false, errors.New("faulty: fence check")
	}
	return f.Store.IsFenced(ctx, id)
}

// fixedClock is a controllable rollups.Clock for the watermark-stamp
// assertions.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

// TestWorker_NewFailurePaths pins the construction contract: nil source,
// nil store, and an unreadable (closed) store all fail loudly.
func TestWorker_NewFailurePaths(t *testing.T) {
	store := newMemStore(t)
	if _, err := projectorworker.New(nil, store); err == nil {
		t.Fatal("nil source must fail construction")
	}
	src := &stubSource{}
	if _, err := projectorworker.New(src, nil); err == nil {
		t.Fatal("nil store must fail construction")
	}
	closed := newMemStore(t)
	if err := closed.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := projectorworker.New(src, closed); err == nil {
		t.Fatal("a closed store must fail construction (checkpoint read fails loud)")
	}
}

// TestWorker_StoreFailurePaths drives every store-error path the worker
// must surface as StateUnavailable without corrupting the durable
// watermark, plus the Quality read error paths.
func TestWorker_StoreFailurePaths(t *testing.T) {
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")

	t.Run("checkpoint-read-fails", func(t *testing.T) {
		store := &faultyStore{Store: memstore.New()}
		defer func() { _ = store.Close(ctx) }()
		src := &stubSource{events: seq(costEvent(base.Add(time.Minute), a, "m", 0.01))}
		w, err := projectorworker.New(src, store)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		store.failCheckpoint = true
		if _, err := w.Advance(ctx); err == nil {
			t.Fatal("Advance over a checkpoint-failing store must error")
		}
		store.failCheckpoint = false
		q, err := w.Quality(ctx)
		if err != nil {
			t.Fatalf("Quality: %v", err)
		}
		if q.State != rollups.StateUnavailable || q.Err == nil {
			t.Fatalf("quality = %+v; want unavailable with Err", q)
		}
	})

	t.Run("apply-fails-keeps-watermark", func(t *testing.T) {
		store := &faultyStore{Store: memstore.New()}
		defer func() { _ = store.Close(ctx) }()
		src := &stubSource{events: seq(costEvent(base.Add(time.Minute), a, "m", 0.01))}
		w, err := projectorworker.New(src, store)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		store.failApplyBatch = true
		if _, err := w.Advance(ctx); err == nil {
			t.Fatal("Advance over an apply-failing store must error")
		}
		q, err := w.Quality(ctx)
		if err != nil {
			t.Fatalf("Quality: %v", err)
		}
		if q.State != rollups.StateUnavailable {
			t.Fatalf("state = %q; want unavailable", q.State)
		}
		if ck, err := store.Checkpoint(ctx); err != nil || ck != 0 {
			t.Fatalf("checkpoint after failed apply = %d, %v; want 0 (untouched)", ck, err)
		}
		// Heal: the next advance applies the event exactly once.
		store.failApplyBatch = false
		if err := w.CatchUp(ctx); err != nil {
			t.Fatalf("CatchUp after heal: %v", err)
		}
		assertMeasure(t, store, sessionFilter("session-a", "m"), rollups.MeasureLLMCostMicros, 10_000)
	})

	t.Run("fence-check-fails", func(t *testing.T) {
		store := &faultyStore{Store: memstore.New()}
		defer func() { _ = store.Close(ctx) }()
		src := &stubSource{events: seq(costEvent(base.Add(time.Minute), a, "m", 0.01))}
		w, err := projectorworker.New(src, store)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		store.failIsFenced = true
		if _, err := w.Advance(ctx); err == nil {
			t.Fatal("Advance over a fence-check-failing store must error")
		}
		q, err := w.Quality(ctx)
		if err != nil {
			t.Fatalf("Quality: %v", err)
		}
		if q.State != rollups.StateUnavailable {
			t.Fatalf("state = %q; want unavailable", q.State)
		}
	})

	t.Run("rebuild-fails", func(t *testing.T) {
		store := &faultyStore{Store: memstore.New()}
		defer func() { _ = store.Close(ctx) }()
		w, err := projectorworker.New(&stubSource{}, store)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		store.failRebuild = true
		if err := w.Rebuild(ctx); err == nil {
			t.Fatal("Rebuild over a rebuild-failing store must error")
		}
		store.failRebuild = false
	})

	t.Run("quality-read-fails", func(t *testing.T) {
		store := &faultyStore{Store: memstore.New()}
		defer func() { _ = store.Close(ctx) }()
		w, err := projectorworker.New(&stubSource{}, store)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		store.failCheckpoint = true
		if _, err := w.Quality(ctx); err == nil {
			t.Fatal("Quality must error when the checkpoint read fails")
		}
		store.failCheckpoint = false
		store.failRetention = true
		if _, err := w.Quality(ctx); err == nil {
			t.Fatal("Quality must error when the retention read fails")
		}
	})
}

// TestWorker_ExtractFailureFailsLoud pins the fail-loud extraction
// contract: a corrupted payload (negative token count) refuses the
// whole page — StateUnavailable, checkpoint untouched — instead of
// silently undercounting a bucket.
func TestWorker_ExtractFailureFailsLoud(t *testing.T) {
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")
	store := newMemStore(t)
	// Direct-construction event with a NEGATIVE prompt-token count — the
	// durable log would only ever carry this if corrupted.
	ev := costEvent(base.Add(time.Minute), a, "m", 0.01, llm.Usage{PromptTokens: -1, TotalTokens: -1})
	src := &stubSource{events: seq(ev)}

	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Advance(ctx); err == nil {
		t.Fatal("Advance over a corrupted-payload event must fail loudly")
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateUnavailable || q.Err == nil {
		t.Fatalf("quality = %+v; want unavailable with Err", q)
	}
	if ck, err := store.Checkpoint(ctx); err != nil || ck != 0 {
		t.Fatalf("checkpoint after extract failure = %d, %v; want 0", ck, err)
	}
}

// TestWorker_CtxCancellationHonoured pins ctx honouring: a cancelled
// context fails the step immediately without touching the worker's
// state or the store.
func TestWorker_CtxCancellationHonoured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := newMemStore(t)
	w, err := projectorworker.New(&stubSource{}, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Advance(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Advance over a cancelled ctx: err=%v; want context.Canceled", err)
	}
	q, err := w.Quality(context.Background())
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCatchingUp {
		t.Fatalf("state = %q; want catching_up (a cancelled step does not mark unavailable)", q.State)
	}
}

// TestWorker_CursorNextBeyondLastEvent pins the second cursor guard: a
// page whose Next points past its last returned event would silently
// skip the gap — the worker refuses loudly.
func TestWorker_CursorNextBeyondLastEvent(t *testing.T) {
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")
	store := newMemStore(t)
	src := &stubSource{events: seq(
		costEvent(base.Add(time.Minute), a, "m", 0.01),
		costEvent(base.Add(2*time.Minute), a, "m", 0.02),
		costEvent(base.Add(3*time.Minute), a, "m", 0.03),
	)}
	n := uint64(4) // Next past the last event (3)
	src.badNext = &n

	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Advance(ctx); err == nil {
		t.Fatal("Advance over a Next-past-last-event page must fail loudly")
	}
	if ck, err := store.Checkpoint(ctx); err != nil || ck != 0 {
		t.Fatalf("checkpoint = %d, %v; want 0", ck, err)
	}
}

// TestWorker_WithClockInjectsWatermarkAt verifies the clock option
// controls the WatermarkAt stamp.
func TestWorker_WithClockInjectsWatermarkAt(t *testing.T) {
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")
	store := newMemStore(t)
	src := &stubSource{events: seq(costEvent(base.Add(time.Minute), a, "m", 0.01))}
	now := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	clk := &fixedClock{now: now}

	w, err := projectorworker.New(src, store, projectorworker.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Advance(ctx); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if !q.WatermarkAt.Equal(now) {
		t.Fatalf("WatermarkAt = %v; want %v (injected clock)", q.WatermarkAt, now)
	}
}

// TestWorker_Run_WatchRefusedFailsLoud pins the Run startup contract: a
// source with no retained substrate refuses the wake sink, so Run fails
// loudly (recorded in Quality) instead of silently serving an empty
// stream.
func TestWorker_Run_WatchRefusedFailsLoud(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newMemStore(t)
	src := &stubSource{unavail: true}

	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = w.Run(ctx)
	if !errors.Is(err, events.ErrProjectionUnavailable) {
		t.Fatalf("Run over an unavailable source: err=%v; want ErrProjectionUnavailable", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateUnavailable || q.Err == nil {
		t.Fatalf("quality = %+v; want unavailable with Err", q)
	}
}

// TestWorker_Run_TransientFailureRetriesAndHeals pins the Run retry
// posture: a transient source failure leaves Run alive with Quality
// unavailable (it NEVER returns — a rollup failure must never be able
// to fail the canonical publication path), and the lost-wake poll heals
// it once the source recovers.
func TestWorker_Run_TransientFailureRetriesAndHeals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")
	src := &stubSource{events: seq(costEvent(base.Add(time.Minute), a, "m", 0.01))}
	src.fail = errors.New("transient: durable log scan failed")

	w, err := projectorworker.New(src, store, projectorworker.WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()

	// While the source is broken, Run stays alive and reports
	// unavailable — it must NOT return.
	eventually(t, "unavailable state recorded", func() bool {
		q, err := w.Quality(ctx)
		return err == nil && q.State == rollups.StateUnavailable
	})
	select {
	case err := <-runErr:
		t.Fatalf("Run returned for a transient failure: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Heal the source: the poll drives a retry that drains to current.
	src.setFail(nil)
	eventually(t, "healed projection reaches current", func() bool {
		q, err := w.Quality(ctx)
		return err == nil && q.State == rollups.StateCurrent && q.Watermark == 1
	})
	assertMeasure(t, store, sessionFilter("session-a", "m"), rollups.MeasureLLMCostMicros, 10_000)

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}
}

// TestWorker_Run_CancelledBeforeStart pins the pre-flight ctx check.
func TestWorker_Run_CancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := newMemStore(t)
	w, err := projectorworker.New(&stubSource{}, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run over a cancelled ctx: err=%v; want context.Canceled", err)
	}
}
