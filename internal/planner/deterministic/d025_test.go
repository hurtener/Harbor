package deterministic_test

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
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestDeterministicPlanner_ConcurrentReuse_D025 is the D-025
// concurrent-reuse gate for the Phase 48 DeterministicPlanner. N≥100
// concurrent Next calls against ONE shared *DeterministicPlanner
// instance. Each goroutine carries a unique identity quadruple; the
// configured step set stamps the per-call RunID into Finish.Metadata
// so the test can assert no identity bleed at the Decision level.
//
// Asserts:
//
//   - No data races (the race detector is the gate).
//   - No identity bleed: each call's Finish.Metadata["run_id"]
//     matches the goroutine's RunID.
//   - No cancellation cross-talk: a pre-cancelled ctx on i%5==0
//     returns ctx.Err() without affecting siblings.
//   - No goroutine leak: baseline runtime.NumGoroutine restored
//     after the WaitGroup join (within 500ms slack).
//
// N=128 (above the D-025 floor of 100; power-of-two for scheduler
// friendliness).
func TestDeterministicPlanner_ConcurrentReuse_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// ONE shared planner — the D-025 contract. The step set has a
	// single FinishStep whose default RunID-stamp behaviour is the
	// identity-round-trip signal.
	shared, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{
			Reason: planner.FinishGoal,
		}),
	)
	if err != nil {
		t.Fatalf("unexpected construction error: %v", err)
	}

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

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if i%5 == 0 {
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
			gotRunID, _ := fin.Metadata["run_id"].(string)
			if gotRunID != runID {
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
		t.Errorf("D-025 identity bleed: %d calls saw another goroutine's RunID in Finish.Metadata", bleedFails)
	}

	// Goroutine leak check. Allow a small slack for the test runner's
	// own finalisers.
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

// TestSpawnAndAwaitStep_ConcurrentSameSession_D025 is the D-025 gate
// for the SpawnAndAwaitStep's per-`(SessionID, StepID)` spawnState.
// Unlike the planner-level D-025 test above (which keys every
// goroutine with a DISTINCT SessionID and therefore never contends on
// a shared spawnState), this test deliberately shares ONE SessionID +
// StepID across all N goroutines so every call resolves to the SAME
// *spawnState. Without the per-state mutex the spawned/resolved/
// groupID/ownerTaskID transitions race; the race detector is the gate.
//
// Asserts:
//
//   - No data races on the shared spawnState (the -race gate).
//   - Exactly one goroutine wins the spawn transition → exactly one
//     SpawnTask decision; every other goroutine sees state.spawned
//     and emits AwaitTask (the group never resolves in this test
//     because nothing marks the member complete).
//   - No call returns an error.
func TestSpawnAndAwaitStep_ConcurrentSameSession_D025(t *testing.T) {
	const N = 128
	deps := mustStepsDeps(t)

	// ONE shared SessionID + StepID → ONE shared *spawnState.
	q := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "t-d025",
			UserID:    "u-d025",
			SessionID: "s-shared",
		},
		RunID: "r-shared",
	}
	ctxIdent, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	spawnStep := &deterministic.SpawnAndAwaitStep{
		StepID: "d025-shared-spawn",
		Kind:   tasks.KindBackground,
		SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
			return planner.SpawnSpec{Description: "d025 background task", Query: "work"}, nil
		},
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
	}
	shared, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(spawnStep),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected construction error: %v", err)
	}

	var (
		wg         sync.WaitGroup
		spawnCount int64
		awaitCount int64
		errCount   int64
		shapeFails int64
	)
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			rc := planner.RunContext{Quadruple: q, Goal: "d025-shared"}
			rc.Trajectory = &planner.Trajectory{}
			dec, callErr := shared.Next(ctxIdent, rc)
			if callErr != nil {
				atomic.AddInt64(&errCount, 1)
				return
			}
			switch dec.(type) {
			case planner.SpawnTask:
				atomic.AddInt64(&spawnCount, 1)
			case planner.AwaitTask:
				atomic.AddInt64(&awaitCount, 1)
			default:
				atomic.AddInt64(&shapeFails, 1)
			}
		}()
	}
	wg.Wait()

	if errCount != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned unexpected errors", errCount)
	}
	if shapeFails != 0 {
		t.Errorf("D-025: %d calls returned a decision shape other than SpawnTask/AwaitTask", shapeFails)
	}
	if spawnCount != 1 {
		t.Errorf("D-025: spawnCount = %d, want exactly 1 (the per-state mutex must serialise the spawn transition)", spawnCount)
	}
	if spawnCount+awaitCount != N {
		t.Errorf("D-025: spawn(%d)+await(%d) = %d, want %d", spawnCount, awaitCount, spawnCount+awaitCount, N)
	}
}

// TestDeterministicPlanner_CancellationDoesNotCrossTalk asserts that
// cancelling one ctx does not affect concurrent siblings.
func TestDeterministicPlanner_CancellationDoesNotCrossTalk(t *testing.T) {
	const N = 32
	shared, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{
			Reason: planner.FinishGoal,
		}),
	)
	if err != nil {
		t.Fatalf("unexpected construction error: %v", err)
	}

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
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if i%2 == 1 {
				cancel()
			}
			rc := planner.RunContext{Quadruple: q}
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

// TestDeterministicPlanner_SharedAcrossIsolatedSessions asserts a
// single planner instance produces decisions whose metadata tracks
// per-call identity exactly (D-025 isolation guarantee). The test
// runs M sessions sequentially against the SAME planner and verifies
// each session's Finish.Metadata["run_id"] reflects its own RunID.
func TestDeterministicPlanner_SharedAcrossIsolatedSessions(t *testing.T) {
	t.Parallel()
	shared, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{
			Reason: planner.FinishGoal,
		}),
	)
	if err != nil {
		t.Fatalf("unexpected construction error: %v", err)
	}
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
		rc := planner.RunContext{Quadruple: q}
		dec, err := shared.Next(context.Background(), rc)
		if err != nil {
			t.Fatalf("Next[%d]: %v", i, err)
		}
		fin, ok := dec.(planner.Finish)
		if !ok {
			t.Fatalf("dec[%d] = %T, want Finish", i, dec)
		}
		if got, _ := fin.Metadata["run_id"].(string); got != runID {
			t.Errorf("session %d metadata.run_id = %q, want %q (identity isolation breach)", i, got, runID)
		}
	}
}
