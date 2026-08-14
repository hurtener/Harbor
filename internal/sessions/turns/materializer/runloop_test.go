package materializer

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestMaterialize_RunLoop_DroppedWakeConvergesWithPoll pins the bounded
// lost-wake recovery: with WithPollInterval set, a deliberately DROPPED
// best-effort wake (no source notification is ever delivered) converges
// WITHOUT a restart — the poll re-checks the source watermark on a
// bounded cadence and drives the catch-up.
func TestMaterialize_RunLoop_DroppedWakeConvergesWithPoll(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t, WithPollInterval(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Publish a full lifecycle AFTER Run has caught up and deliberately
	// drop the wake: h.src.notify() is never called. Only the poll can
	// converge it.
	h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")

	if !eventually(t, 5*time.Second, func() bool {
		row, err := h.proj.Get(context.Background(), h.id, "task-1")
		return err == nil && row.Sealed
	}) {
		t.Fatal("dropped wake never converged via the poll")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit on cancellation")
	}
}

// TestMaterialize_RunLoop_WakeStaysPrimaryWithPoll pins that the poll
// is the safety net, never the fast path: with a LONG poll interval, a
// DELIVERED wake converges promptly (well before the first poll tick).
func TestMaterialize_RunLoop_WakeStaysPrimaryWithPoll(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t, WithPollInterval(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")
	h.src.notify()

	if !eventually(t, 5*time.Second, func() bool {
		row, err := h.proj.Get(context.Background(), h.id, "task-1")
		return err == nil && row.Sealed
	}) {
		t.Fatal("wake-driven convergence failed (the wake must stay primary)")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit on cancellation")
	}
}

// TestMaterialize_RunLoop_CancellationStopsWatcherAndTimer pins the
// no-leak contract of the polling Run: cancelling it stops the watcher
// (the source's Unsubscribe runs), the Run goroutine exits cleanly, and
// the goroutine count returns to baseline — the poll timer never leaks
// and never outlives the loop.
func TestMaterialize_RunLoop_CancellationStopsWatcherAndTimer(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t, WithPollInterval(2*time.Millisecond))

	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Wait for the initial catch-up page (Run is inside the loop), let
	// a few poll ticks fire, then cancel.
	if !eventually(t, 5*time.Second, func() bool { return h.src.pageCallCount() > 0 }) {
		t.Fatal("run loop never reached its catch-up")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit on cancellation")
	}
	if !h.src.wasUnsubscribed() {
		t.Error("Run exited without unsubscribing the watcher")
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, got)
	}
}
