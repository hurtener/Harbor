package steering

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// TestConcurrentReuse_Registry is the mandatory D-025 concurrent-reuse
// test (CLAUDE.md §5 + §11). It runs N=200 goroutines (≥100 per the
// contract) through the full per-run inbox lifecycle —
// Open → Enqueue → Lookup → Drain → Retire — against ONE shared
// Registry under -race, asserting:
//
//   - no data races (the -race detector is the gate);
//   - no context bleed — each goroutine uses a distinct run
//     quadruple; a drained event carrying a foreign RunID would mean
//     one run's inbox leaked into another's;
//   - no goroutine leak — runtime.NumGoroutine returns to baseline
//     after all goroutines join.
//
// Cancellation cross-talk is not applicable: the steering Registry /
// Inbox surface takes no context.Context (it is a synchronous
// in-memory queue) — there is no per-run ctx to cancel. The
// integration test (test/integration) exercises the ctx-carrying
// EventBus seam.
func TestConcurrentReuse_Registry(t *testing.T) {
	const n = 200

	baseline := runtime.NumGoroutine()
	reg := NewRegistry(WithClock(newFakeClock()))

	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()

			// Distinct per-goroutine run quadruple — a context bleed
			// surfaces as a wrong RunID on a drained event.
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("tenant-%d", i),
					UserID:    fmt.Sprintf("user-%d", i),
					SessionID: fmt.Sprintf("session-%d", i),
				},
				RunID: fmt.Sprintf("run-%d", i),
			}

			in, err := reg.Open(q)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: Open: %w", i, err)
				return
			}

			// Enqueue a handful of events; each carries this
			// goroutine's own run identity.
			const perRun = 4
			for j := 0; j < perRun; j++ {
				ev := ControlEvent{
					Type:         ControlCancel,
					Identity:     q,
					CallerScope:  ScopeOwnerUser,
					CallerTenant: q.TenantID,
					Payload:      map[string]any{"i": i, "j": j},
				}
				if err := in.Enqueue(ev); err != nil {
					errCh <- fmt.Errorf("goroutine %d: Enqueue: %w", i, err)
					return
				}
			}

			// Look the inbox back up — must be the same instance.
			got, err := reg.Lookup(q)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: Lookup: %w", i, err)
				return
			}
			if got != in {
				errCh <- fmt.Errorf("goroutine %d: Lookup returned a foreign *Inbox", i)
				return
			}

			drained, err := in.Drain()
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: Drain: %w", i, err)
				return
			}
			if len(drained) != perRun {
				errCh <- fmt.Errorf("goroutine %d: drained %d events, want %d", i, len(drained), perRun)
				return
			}
			// Context-bleed assertion: every drained event belongs
			// to THIS goroutine's run.
			for _, ev := range drained {
				if ev.Identity != q {
					errCh <- fmt.Errorf("goroutine %d: drained event for foreign run %+v", i, ev.Identity)
					return
				}
			}

			if err := reg.Retire(q); err != nil {
				errCh <- fmt.Errorf("goroutine %d: Retire: %w", i, err)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// All inboxes retired — the shared Registry holds no per-run
	// state after the runs end.
	if reg.Len() != 0 {
		t.Errorf("Registry.Len() = %d after all runs retired, want 0 (per-run state leaked)", reg.Len())
	}

	// Goroutine-leak check: give the scheduler a moment, then assert
	// the count returned to baseline.
	assertNoGoroutineLeak(t, baseline)
}

// TestConcurrentReuse_SingleInbox stresses ONE shared Inbox with N
// concurrent producers and a concurrent draining consumer — the
// Protocol-edge-vs-run-loop concurrency shape (CLAUDE.md §5). It
// asserts every enqueued event is drained exactly once: no loss, no
// duplication, no race.
func TestConcurrentReuse_SingleInbox(t *testing.T) {
	const producers = 120
	const perProducer = 8

	reg := NewRegistry(WithClock(newFakeClock()))
	in, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(producers)
	for p := 0; p < producers; p++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perProducer; j++ {
				if err := in.Enqueue(validEvent(runA)); err != nil {
					t.Errorf("Enqueue: %v", err)
					return
				}
			}
		}()
	}

	// Concurrent draining consumer collects events while producers
	// are still running.
	collected := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			batch, err := in.Drain()
			if err != nil {
				t.Errorf("Drain: %v", err)
				return
			}
			collected += len(batch)
			if collected >= producers*perProducer {
				return
			}
			runtime.Gosched()
		}
	}()

	wg.Wait()
	<-done

	// Final sweep for anything enqueued after the consumer's last
	// drain.
	final, err := in.Drain()
	if err != nil {
		t.Fatalf("final Drain: %v", err)
	}
	collected += len(final)

	if want := producers * perProducer; collected != want {
		t.Errorf("collected %d events, want %d (loss or duplication under concurrency)", collected, want)
	}
}

// assertNoGoroutineLeak polls until the goroutine count is at or
// below baseline, or fails after a bounded wait. No fixed sleep —
// per CLAUDE.md §11.
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine count %d did not return to baseline %d — leak", runtime.NumGoroutine(), baseline)
}
