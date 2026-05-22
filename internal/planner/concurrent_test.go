package planner_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/finish"
)

// TestFinishPlanner_ConcurrentReuse_D025 is the D-025 concurrent-reuse
// gate for the planner subsystem. The stub finish.Planner is a
// reusable artifact (one instance, many Next calls); the contract
// requires N≥100 concurrent invocations against a single shared
// instance under -race with:
//
//   - No data races (the race detector is the gate).
//   - No context bleed (each call's RunID round-trips out through
//     the returned Finish.Metadata).
//   - No cancellation cross-talk (cancelling one ctx must not affect
//     siblings).
//   - No goroutine leaks (baseline runtime.NumGoroutine restored
//     after the WaitGroup join).
//
// N=128 (above the D-025 floor of 100; rounded to a power of two for
// scheduler friendliness).
func TestFinishPlanner_ConcurrentReuse_D025(t *testing.T) {
	const N = 128

	// Baseline goroutine count BEFORE the test body. Run a couple of
	// GCs to drain finalisers from prior tests so the post-join
	// comparison is meaningful.
	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	p := finish.New(finish.WithMetadata(map[string]any{
		"stub": "phase-42",
	}))

	var (
		wg          sync.WaitGroup
		bleedFails  int64
		errFails    int64
		wrongShape  int64
		wrongReason int64
	)
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			runID := fmt.Sprintf("run-%04d", i)
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  "tenant",
					UserID:    "user",
					SessionID: "session",
				},
				RunID: runID,
			}
			rc := planner.RunContext{Quadruple: q}
			dec, err := p.Next(context.Background(), rc)
			if err != nil {
				atomic.AddInt64(&errFails, 1)
				return
			}
			fin, ok := dec.(planner.Finish)
			if !ok {
				atomic.AddInt64(&wrongShape, 1)
				return
			}
			if fin.Reason != planner.FinishGoal {
				atomic.AddInt64(&wrongReason, 1)
				return
			}
			gotRunID, _ := fin.Metadata["run_id"].(string)
			if gotRunID != runID {
				// Context bleed — another call's RunID surfaced here.
				atomic.AddInt64(&bleedFails, 1)
			}
		}()
	}
	wg.Wait()

	if errFails != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned errors", errFails)
	}
	if wrongShape != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned non-Finish decisions", wrongShape)
	}
	if wrongReason != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned wrong FinishReason", wrongReason)
	}
	if bleedFails != 0 {
		t.Errorf("D-025 context bleed: %d concurrent Next calls saw another call's RunID via Finish.Metadata", bleedFails)
	}

	// Goroutine leak check. The stub planner spawns no goroutines of
	// its own; the only goroutines outstanding at this point should
	// be the test scheduler's. Allow a small slack (test runner
	// goroutines may flap by 1-2); D-025's tight bound is "no leak
	// over time", not "exact equality at instant".
	runtime.GC()
	runtime.GC()
	// Wait briefly for any test-runner finalisers to drain.
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

// TestFinishPlanner_CancellationDoesNotCrossTalk verifies that
// cancelling one ctx does not affect concurrent siblings (D-025
// cancellation cross-talk contract).
func TestFinishPlanner_CancellationDoesNotCrossTalk(t *testing.T) {
	p := finish.New()

	const N = 32
	var (
		wg          sync.WaitGroup
		siblingErrs int64
	)
	// Start N goroutines with FRESH per-goroutine contexts. Cancel
	// every odd-indexed one BEFORE the call; even-indexed ones must
	// complete cleanly.
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if i%2 == 1 {
				cancel()
			}
			rc := planner.RunContext{
				Quadruple: identity.Quadruple{
					Identity: identity.Identity{
						TenantID: "t", UserID: "u", SessionID: "s",
					},
					RunID: fmt.Sprintf("r-%d", i),
				},
			}
			_, err := p.Next(ctx, rc)
			if i%2 == 0 && err != nil {
				atomic.AddInt64(&siblingErrs, 1)
			}
		}()
	}
	wg.Wait()

	if siblingErrs != 0 {
		t.Errorf("cancellation cross-talk: %d even-indexed calls failed despite their own ctx being live", siblingErrs)
	}
}
