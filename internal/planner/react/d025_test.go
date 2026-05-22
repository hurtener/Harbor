package react_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// sharedClient is the shared inner LLM client for the D-025 stress.
// It produces a deterministic per-goroutine answer by reading the
// run's identity from ctx (the planner contract: per-call state lives
// in ctx, not on the receiver — the client honours the same).
type sharedClient struct{}

func (s *sharedClient) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	id, _ := identity.QuadrupleFrom(ctx)
	// Emit a JSON `_finish` envelope whose payload is the run's
	// RunID — this lets the test's per-goroutine assertion confirm
	// each goroutine's Decision carries its OWN RunID (no bleed).
	content := fmt.Sprintf(`{"tool":"_finish","args":{"answer":%q},"reasoning":"d025"}`, id.RunID)
	return llm.CompleteResponse{Content: content}, nil
}

func (s *sharedClient) Close(_ context.Context) error { return nil }

// TestReactPlanner_ConcurrentReuse_D025 is the D-025 concurrent-reuse
// gate for the Phase 45 ReActPlanner. N≥100 concurrent Next calls
// against ONE shared *ReActPlanner instance. Each goroutine carries
// a unique identity quadruple; the LLM client returns a per-call
// `_finish` envelope whose payload is the run's RunID, so the test
// can assert no identity bleed at the Decision level.
//
// Asserts:
//
//   - No data races (the race detector is the gate).
//   - No identity bleed: each call's Finish.Payload (or
//     Metadata["run_id"] for the breaker / cancellation paths)
//     matches the goroutine's RunID.
//   - No cancellation cross-talk: a pre-cancelled ctx on i%5==0
//     returns ctx.Err() without affecting siblings.
//   - No goroutine leak: baseline runtime.NumGoroutine restored after
//     the WaitGroup join (within 500ms slack).
//
// N=128 (above the D-025 floor of 100; power-of-two for scheduler
// friendliness).
func TestReactPlanner_ConcurrentReuse_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// ONE shared planner — the D-025 contract.
	shared := react.New(&sharedClient{})

	var (
		wg          sync.WaitGroup
		bleedFails  int64
		shapeFails  int64
		cancelFails int64
		errFails    int64
	)

	wg.Add(N)
	for i := range N {

		go func() {
			defer wg.Done()

			runID := fmt.Sprintf("run-%04d", i)
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("tenant-%d", i%8),
					UserID:    fmt.Sprintf("user-%d", i),
					SessionID: fmt.Sprintf("session-%d", i),
				},
				RunID: runID,
			}
			ctx, err := identity.WithRun(context.Background(), q.Identity, runID)
			if err != nil {
				atomic.AddInt64(&errFails, 1)
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if i%5 == 0 {
				// Pre-cancel BEFORE the call — sibling goroutines
				// MUST NOT see this cancellation.
				cancel()
			}

			rc := planner.RunContext{
				Quadruple: q,
				Goal:      "d025-stress",
			}

			dec, callErr := shared.Next(ctx, rc)
			if i%5 == 0 {
				// Expected: pre-cancelled ctx returns ctx.Err().
				if callErr == nil {
					atomic.AddInt64(&cancelFails, 1)
				}
				return
			}
			if callErr != nil {
				atomic.AddInt64(&errFails, 1)
				return
			}

			fin, ok := dec.(planner.Finish)
			if !ok {
				atomic.AddInt64(&shapeFails, 1)
				return
			}
			if fin.Reason != planner.FinishGoal {
				atomic.AddInt64(&shapeFails, 1)
				return
			}
			// Identity round-trip via Payload (set by sharedClient
			// based on ctx-derived RunID; the translateFinishCall
			// path copies the LLM-emitted `args.answer`).
			answer, _ := fin.Payload.(string)
			if answer != runID {
				atomic.AddInt64(&bleedFails, 1)
			}
		}()
	}
	wg.Wait()

	if errFails != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned unexpected errors", errFails)
	}
	if shapeFails != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned non-Finish-FinishGoal decisions", shapeFails)
	}
	if cancelFails != 0 {
		t.Errorf("D-025: %d pre-cancelled goroutines did NOT return ctx.Err()", cancelFails)
	}
	if bleedFails != 0 {
		t.Errorf("D-025 identity bleed: %d calls saw another goroutine's RunID in Finish.Payload", bleedFails)
	}

	// Counter: shared.StepsTaken() == N - pre-cancelled count (the
	// breaker / pre-cancel paths short-circuit BEFORE the increment).
	// Computed exactly the way the loop did (i%5 == 0).
	preCancelled := 0
	for i := range N {
		if i%5 == 0 {
			preCancelled++
		}
	}
	wantSteps := int64(N - preCancelled)
	if got := shared.StepsTaken(); got != wantSteps {
		t.Errorf("StepsTaken = %d, want %d (N=%d, preCancelled=%d)", got, wantSteps, N, preCancelled)
	}

	// Goroutine leak check. Allow a small slack for the test
	// runner's own finalisers (matches the Phase 42 + Phase 44 D-025
	// pattern).
	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(500 * time.Millisecond)
	final := runtime.NumGoroutine()
	for final > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		final = runtime.NumGoroutine()
	}
	if final > baseline+2 {
		t.Errorf("D-025 goroutine leak: baseline=%d final=%d (delta=%d)", baseline, final, final-baseline)
	}
}

// TestReactPlanner_CancellationDoesNotCrossTalk asserts cancelling
// one ctx does not affect concurrent siblings (D-025 cancellation
// cross-talk contract). Distinct from the bleed test above: this one
// uses a less-aggressive pattern (each goroutine has its OWN fresh
// ctx; odd-indexed ones cancel BEFORE the call).
func TestReactPlanner_CancellationDoesNotCrossTalk(t *testing.T) {
	const N = 32
	shared := react.New(&sharedClient{})

	var (
		wg          sync.WaitGroup
		siblingErrs int64
	)
	wg.Add(N)
	for i := range N {

		go func() {
			defer wg.Done()
			runID := fmt.Sprintf("r-%d", i)
			q := identity.Quadruple{
				Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
				RunID:    runID,
			}
			ctx, err := identity.WithRun(context.Background(), q.Identity, runID)
			if err != nil {
				atomic.AddInt64(&siblingErrs, 1)
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if i%2 == 1 {
				cancel()
			}
			rc := planner.RunContext{
				Quadruple: q,
				Goal:      "cancel-stress",
			}
			_, callErr := shared.Next(ctx, rc)
			if i%2 == 0 && callErr != nil {
				atomic.AddInt64(&siblingErrs, 1)
			}
		}()
	}
	wg.Wait()

	if siblingErrs != 0 {
		t.Errorf("cancellation cross-talk: %d even-indexed calls failed despite their own ctx being live", siblingErrs)
	}
}

// TestReactPlanner_SharedAcrossIsolatedSessions asserts that one
// planner instance produces decisions whose terminal payloads track
// per-call identity exactly (D-025 isolation guarantee). The test
// runs M sessions sequentially against the SAME planner and verifies
// each session's Finish.Payload reflects its own RunID.
func TestReactPlanner_SharedAcrossIsolatedSessions(t *testing.T) {
	t.Parallel()
	shared := react.New(&sharedClient{})
	const M = 16
	for i := range M {
		runID := fmt.Sprintf("seq-%d", i)
		q := identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i),
				UserID:    fmt.Sprintf("u-%d", i),
				SessionID: fmt.Sprintf("s-%d", i),
			},
			RunID: runID,
		}
		ctx, err := identity.WithRun(context.Background(), q.Identity, runID)
		if err != nil {
			t.Fatalf("identity.WithRun: %v", err)
		}
		rc := planner.RunContext{Quadruple: q, Goal: "iso"}
		dec, err := shared.Next(ctx, rc)
		if err != nil {
			t.Fatalf("Next[%d]: %v", i, err)
		}
		fin, ok := dec.(planner.Finish)
		if !ok {
			t.Fatalf("dec[%d] = %T, want Finish", i, dec)
		}
		if got, _ := fin.Payload.(string); got != runID {
			t.Errorf("session %d payload = %q, want %q (identity isolation breach)", i, got, runID)
		}
	}
}
