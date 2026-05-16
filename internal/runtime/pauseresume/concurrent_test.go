package pauseresume_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// TestCoordinator_ConcurrentReuse_D025 pins the D-025 concurrent-reuse
// contract (CLAUDE.md §5 + §11): N≥100 goroutines run the full
// Request → Status → Resume → Status lifecycle against ONE shared
// Coordinator, each under its own identity quadruple. The test asserts:
//
//   - no data races (the -race detector is the gate);
//   - no context bleed — each goroutine's pause carries ONLY its own
//     identity and payload; a cross-talk would surface as a wrong
//     tenant/user/session or a foreign payload value;
//   - no cross-cancellation — a subset of goroutines run with a
//     pre-cancelled ctx; their Request must fail with context.Canceled
//     while every other goroutine succeeds untouched;
//   - no goroutine leak — runtime.NumGoroutine returns to baseline
//     after every goroutine has joined.
func TestCoordinator_ConcurrentReuse_D025(t *testing.T) {
	const N = 200 // ≥100 per the D-025 contract

	// One shared, immutable-after-construction Coordinator. A
	// checkpoint store is configured so the durability path is
	// exercised concurrently too.
	store := newStore(t)
	c := pauseresume.New(pauseresume.WithCheckpointStore(store))

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan error, N)

	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()

			// Per-goroutine identity quadruple — distinct tenant so a
			// context bleed is detectable.
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%03d", i),
				UserID:    fmt.Sprintf("user-%03d", i),
				SessionID: fmt.Sprintf("session-%03d", i),
			}
			runID := fmt.Sprintf("run-%03d", i)
			ctx, err := identity.WithRun(context.Background(), id, runID)
			if err != nil {
				errCh <- fmt.Errorf("g%d: WithRun: %w", i, err)
				return
			}

			// Every fifth goroutine runs with a pre-cancelled ctx —
			// its Request MUST fail closed without affecting any peer.
			if i%5 == 0 {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				if _, rerr := c.Request(cctx, pauseresume.PauseRequest{
					Identity: id,
					Reason:   pauseresume.ReasonApprovalRequired,
				}); rerr == nil {
					errCh <- fmt.Errorf("g%d: Request on cancelled ctx succeeded, want failure", i)
				}
				return
			}

			payloadMark := fmt.Sprintf("mark-%03d", i)
			p, err := c.Request(ctx, pauseresume.PauseRequest{
				Identity: id,
				Reason:   pauseresume.ReasonAwaitInput,
				Payload:  map[string]any{"mark": payloadMark},
			})
			if err != nil {
				errCh <- fmt.Errorf("g%d: Request: %w", i, err)
				return
			}

			// Context-bleed check: the returned pause carries ONLY
			// this goroutine's identity and payload.
			if p.Identity != id {
				errCh <- fmt.Errorf("g%d: pause identity %+v, want %+v (context bleed)", i, p.Identity, id)
				return
			}
			if got, _ := p.Payload["mark"].(string); got != payloadMark {
				errCh <- fmt.Errorf("g%d: pause payload mark %q, want %q (context bleed)", i, got, payloadMark)
				return
			}

			st, err := c.Status(ctx, p.Token)
			if err != nil {
				errCh <- fmt.Errorf("g%d: Status (paused): %w", i, err)
				return
			}
			if st.State != pauseresume.StatusPaused {
				errCh <- fmt.Errorf("g%d: Status.State %q, want paused", i, st.State)
				return
			}

			if err := c.Resume(ctx, p.Token, pauseresume.DecisionResume, map[string]any{"resumed_by": runID}); err != nil {
				errCh <- fmt.Errorf("g%d: Resume: %w", i, err)
				return
			}

			st, err = c.Status(ctx, p.Token)
			if err != nil {
				errCh <- fmt.Errorf("g%d: Status (resumed): %w", i, err)
				return
			}
			if st.State != pauseresume.StatusResumed {
				errCh <- fmt.Errorf("g%d: Status.State %q after resume, want resumed", i, st.State)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Goroutine-leak check: give the scheduler a bounded window to
	// reap any transient goroutines, then assert the baseline is
	// restored. This is a bounded poll, not a synchronisation sleep.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Errorf("goroutine leak: NumGoroutine=%d, baseline=%d", got, baseline)
	}
}

// TestCoordinator_ConcurrentResumeSameToken asserts that N goroutines
// racing to Resume the SAME token produce exactly one success and
// N-1 ErrAlreadyResumed — never a double-apply, never a data race.
func TestCoordinator_ConcurrentResumeSameToken(t *testing.T) {
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")

	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	const racers = 32
	var wg sync.WaitGroup
	wg.Add(racers)
	results := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			results <- c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil)
		}()
	}
	wg.Wait()
	close(results)

	var ok, alreadyResumed, other int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, pauseresume.ErrAlreadyResumed):
			alreadyResumed++
		default:
			other++
			t.Errorf("unexpected Resume error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("Resume successes = %d, want exactly 1", ok)
	}
	if alreadyResumed != racers-1 {
		t.Errorf("ErrAlreadyResumed count = %d, want %d", alreadyResumed, racers-1)
	}
	if other != 0 {
		t.Errorf("unexpected error count = %d, want 0", other)
	}
}
