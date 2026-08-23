package projectorworker_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/projectorworker"
)

// eventually polls fn until it returns true or a bounded real-time
// deadline elapses — an eventually-style assertion with a bounded
// timeout (never a sleep-as-synchronisation primitive).
func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within 5s: %s", what)
}

// eventuallyGoroutinesSettle polls until the live goroutine count
// drains to within tol of base, or a bounded deadline elapses (the
// integration suite's settle idiom).
func eventuallyGoroutinesSettle(t *testing.T, base, tol int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for {
		runtime.GC()
		got = runtime.NumGoroutine()
		if got <= base+tol || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got > base+tol {
		t.Errorf("goroutine leak: base=%d after=%d (did not drain within 5s, tol=%d)", base, got, tol)
	}
}

// TestWorker_AdvanceRebuildSerialization pins the full-step advance
// serialization: the advance mutex is held for the ENTIRE step, and
// Rebuild coordinates through the SAME mutex, so a delayed advance can
// never land AFTER a rebuild (which would jump the fresh watermark over
// the pre-rebuild events) and no advance can interleave with a rebuild.
//
// Deterministic interleaving: drain 1..7, then start a SECOND Advance
// that blocks inside Source.Page holding the advance mutex; start
// Rebuild while it is blocked (on the fixed code Rebuild waits for the
// advance; on unfixed code it runs immediately — the corruption
// window). Release the gate, join both, then drain to the head. The
// final watermark/checkpoint must be the newest sequence (10) with
// every event applied exactly once.
func TestWorker_AdvanceRebuildSerialization(t *testing.T) {
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")

	var evs []events.Event
	for i := 1; i <= 10; i++ {
		evs = append(evs, costEvent(base.Add(time.Duration(i)*time.Minute), a, "model-a", float64(i)))
	}
	src := &stubSource{
		events:   seq(evs...),
		gate:     make(chan struct{}),
		gateCall: 2, // the SECOND Advance (the delayed one) blocks
		blocked:  make(chan struct{}),
	}
	store := newMemStore(t)

	w, err := projectorworker.New(src, store, projectorworker.WithBatchSize(7))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Drain 1..7 (one full batch of 7).
	caughtUp, err := w.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance 1: %v", err)
	}
	if caughtUp {
		t.Fatal("full batch must not report caught up")
	}
	if ck, err := store.Checkpoint(ctx); err != nil || ck != 7 {
		t.Fatalf("checkpoint after drain 1..7 = %d, %v; want 7", ck, err)
	}

	// The DELAYED Advance: reads after=7, blocks inside Page (holding
	// the advance mutex on the fixed code) waiting for events 8..10.
	advDone := make(chan error, 1)
	go func() {
		_, err := w.Advance(ctx)
		advDone <- err
	}()
	<-src.blocked // the delayed Advance is now mid-step

	// Quality stays READABLE while the state-mutating step is in-flight
	// (the concurrent-read guarantee).
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality during in-flight advance: %v", err)
	}
	if q.Watermark != 7 {
		t.Fatalf("quality watermark during in-flight advance = %d; want 7", q.Watermark)
	}

	// The staggered Rebuild: launched while the advance is blocked. On
	// the fixed code it waits on the advance mutex until the advance
	// finishes; on unfixed code it runs immediately and wipes the store
	// under the in-flight advance (the corruption window).
	rbDone := make(chan error, 1)
	go func() {
		rbDone <- w.Rebuild(ctx)
	}()

	// Release the delayed advance. Fixed code order: the advance
	// completes (applies 8..10 on top of 1..7), THEN the rebuild resets
	// rows + checkpoint to 0. Unfixed code order: rebuild already ran,
	// then the advance applies {checkpoint 10} over the emptied store —
	// events 1..7 are lost forever.
	close(src.gate)
	if err := <-advDone; err != nil {
		t.Fatalf("delayed Advance: %v", err)
	}
	if err := <-rbDone; err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// After the rebuild completed LAST, the store is empty and the
	// worker must re-drain the WHOLE log.
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality after rebuild: %v", err)
	}
	if q.Watermark != 0 || q.State != rollups.StateCatchingUp {
		t.Fatalf("post-rebuild quality = %+v; want watermark 0, catching_up", q)
	}

	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp after rebuild: %v", err)
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality final: %v", err)
	}
	if q.Watermark != 10 || q.State != rollups.StateCurrent {
		t.Fatalf("final quality = %+v; want watermark 10, current", q)
	}
	assertMeasure(t, store, sessionFilter("session-a", "model-a"), rollups.MeasureLLMCostMicros, 55_000_000)
	assertMeasure(t, store, sessionFilter("session-a", "model-a"), rollups.MeasureLLMCompletions, 10)
}

// TestWorker_ConcurrentReuse_NoRaceNoDoubleCount runs N=100 concurrent
// invocations (Advance / CatchUp / Quality / Rebuild) against ONE
// shared worker under -race, asserting the concurrent-reuse contract:
// no data race, no identity bleed, no double-count, and no goroutine
// leak after teardown. Interleaved Rebuilds reset the store mid-flight;
// the final catch-up re-drains the whole log so the totals must equal
// the full fixture exactly once.
func TestWorker_ConcurrentReuse_NoRaceNoDoubleCount(t *testing.T) {
	ctx := context.Background()
	bus, src := newInmemProjectionBus(t, 256)
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")
	b := tq("tenant-a", "user-1", "session-b")

	// 100 cost events, alternating sessions/models: odd → A/model-a
	// 1.00 USD, even → B/model-b 2.00 USD, all inside the 1h window.
	var evs []events.Event
	for i := 1; i <= 100; i++ {
		id, model, cost := a, "model-a", 1.0
		if i%2 == 0 {
			id, model, cost = b, "model-b", 2.0
		}
		evs = append(evs, costEvent(base.Add(time.Duration(i%59+1)*time.Minute), id, model, cost))
	}
	publish(t, bus, evs...)

	w, err := projectorworker.New(src, store, projectorworker.WithBatchSize(7))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	goroBase := runtime.NumGoroutine()
	var (
		wg   sync.WaitGroup
		errs = make(chan error, 128)
	)
	record := func(err error) {
		if err != nil {
			errs <- err
		}
	}

	// 40 advance goroutines, 30 quality readers, 20 catch-up loops, and
	// 10 rebuilds — all against one shared worker instance.
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.Advance(ctx)
			record(err)
		}()
	}
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.Quality(ctx)
			record(err)
		}()
	}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record(w.CatchUp(ctx))
		}()
	}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record(w.Rebuild(ctx))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent invocation failed: %v", err)
	}

	// Final catch-up: whatever interleaving of advances/rebuilds
	// happened, the store must converge to the FULL fixture exactly once.
	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("final CatchUp: %v", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 100 || q.State != rollups.StateCurrent {
		t.Fatalf("final quality = %+v; want watermark 100, current", q)
	}
	if ck, err := store.Checkpoint(ctx); err != nil || ck != 100 {
		t.Fatalf("final checkpoint = %d, %v; want 100", ck, err)
	}

	// Exact per-identity totals (no bleed, no double-count).
	assertMeasure(t, store, sessionFilter("session-a", "model-a"), rollups.MeasureLLMCompletions, 50)
	assertMeasure(t, store, sessionFilter("session-a", "model-a"), rollups.MeasureLLMCostMicros, 50_000_000)
	assertMeasure(t, store, sessionFilter("session-b", "model-b"), rollups.MeasureLLMCompletions, 50)
	assertMeasure(t, store, sessionFilter("session-b", "model-b"), rollups.MeasureLLMCostMicros, 100_000_000)
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCompletions); got != 100 {
		t.Fatalf("total completions = %d; want 100 (no double-count under concurrency)", got)
	}

	// Teardown, then assert the goroutine count returns to baseline.
	_ = bus.Close(ctx)
	_ = store.Close(ctx)
	eventuallyGoroutinesSettle(t, goroBase, 3)
}

// TestWorker_TwoWorkersOneStore_AtLeastOnceIdempotent runs TWO worker
// replicas over the SAME durable store and the SAME source — the
// concurrent-replica-application acceptance. Because each advance reads
// the shared durable checkpoint at step start and both replicas share
// the batch size, they produce identical pages and the Store's
// non-advancing gate no-ops the loser: values are neither lost nor
// double-counted (at-least-once idempotent on the local sequence; no
// active-active exactly-once claim).
func TestWorker_TwoWorkersOneStore_AtLeastOnceIdempotent(t *testing.T) {
	ctx := context.Background()
	bus, src := newInmemProjectionBus(t, 64)
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")

	var evs []events.Event
	for i := 1; i <= 20; i++ {
		evs = append(evs, costEvent(base.Add(time.Duration(i)*time.Minute), a, "m", 0.01))
	}
	publish(t, bus, evs...)

	w1, err := projectorworker.New(src, store, projectorworker.WithBatchSize(7))
	if err != nil {
		t.Fatalf("New w1: %v", err)
	}
	w2, err := projectorworker.New(src, store, projectorworker.WithBatchSize(7))
	if err != nil {
		t.Fatalf("New w2: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = w1.CatchUp(ctx) }()
	go func() { defer wg.Done(); _ = w2.CatchUp(ctx) }()
	wg.Wait()

	// Both replicas converged on the same durable watermark.
	q1, err := w1.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality w1: %v", err)
	}
	q2, err := w2.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality w2: %v", err)
	}
	if q1.Watermark != 20 || q2.Watermark != 20 {
		t.Fatalf("watermarks = %d, %d; want 20, 20", q1.Watermark, q2.Watermark)
	}
	if ck, err := store.Checkpoint(ctx); err != nil || ck != 20 {
		t.Fatalf("checkpoint = %d, %v; want 20", ck, err)
	}

	// Every event applied EXACTLY once despite two concurrent replicas.
	assertMeasure(t, store, sessionFilter("session-a", "m"), rollups.MeasureLLMCompletions, 20)
	assertMeasure(t, store, sessionFilter("session-a", "m"), rollups.MeasureLLMCostMicros, 200_000)
}

// TestWorker_RunLoop_WakeDrivenCatchUpAndShutdown drives the
// background run loop: it registers a wake sink, drains, catches up on
// wake notifications for events published after startup (no polling),
// returns nil on graceful ctx cancellation, and leaves no goroutine
// behind.
func TestWorker_RunLoop_WakeDrivenCatchUpAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus, src := newInmemProjectionBus(t, 64)
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")

	w, err := projectorworker.New(src, store, projectorworker.WithPollInterval(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	goroBase := runtime.NumGoroutine()

	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()

	// Events published AFTER Run started are picked up wake-driven.
	publish(t, bus,
		costEvent(base.Add(time.Minute), a, "m", 0.10),
		costEvent(base.Add(2*time.Minute), a, "m", 0.20),
		costEvent(base.Add(3*time.Minute), a, "m", 0.30),
	)
	eventually(t, "wake-driven catch-up reflects published events", func() bool {
		return measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCompletions) == 3
	})
	eventually(t, "quality reaches current", func() bool {
		q, err := w.Quality(ctx)
		return err == nil && q.State == rollups.StateCurrent && q.Watermark == 3
	})

	// Graceful shutdown: Run returns nil and joins.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}

	// A further publish after shutdown must NOT be projected (the wake
	// sink is unsubscribed and the loop has returned).
	publish(t, bus, costEvent(base.Add(4*time.Minute), a, "m", 0.40))
	time.Sleep(50 * time.Millisecond)
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCompletions); got != 3 {
		t.Fatalf("completions after shutdown = %d; want 3 (worker stopped)", got)
	}

	_ = bus.Close(ctx)
	_ = store.Close(ctx)
	eventuallyGoroutinesSettle(t, goroBase, 3)
}

// TestWorker_RunLoop_IdlePollChecksWatermarkBeforePage pins the durable
// lost-wake healer's idle path: a current worker polls the cheap source
// watermark and durable checkpoint without re-reading the global source
// page. A later source sequence still crosses the gate and is projected.
func TestWorker_RunLoop_IdlePollChecksWatermarkBeforePage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newMemStore(t)
	src := &stubSource{}
	w, err := projectorworker.New(src, store, projectorworker.WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()

	eventually(t, "initial empty page", func() bool {
		src.mu.Lock()
		defer src.mu.Unlock()
		return src.calls >= 1
	})
	src.mu.Lock()
	initialPages := src.calls
	src.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	src.mu.Lock()
	idlePages := src.calls
	src.mu.Unlock()
	if idlePages != initialPages {
		t.Fatalf("idle poll issued %d extra source pages; initial=%d idle=%d", idlePages-initialPages, initialPages, idlePages)
	}

	id := tq("tenant-idle", "user-idle", "session-idle")
	src.mu.Lock()
	src.events = seq(costEvent(base.Add(time.Minute), id, "idle-model", 0.01))
	src.mu.Unlock()
	eventually(t, "watermark change crosses page gate", func() bool {
		return measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCompletions) == 1
	})
	src.mu.Lock()
	projectedPages := src.calls
	src.mu.Unlock()
	if projectedPages <= idlePages {
		t.Fatal("new source sequence was projected without a source page")
	}

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
