package planner_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// countingSummariser stamps the goroutine ID into the summary's
// Note field so the D-025 context-bleed detector can assert each
// goroutine recovers its own summary. Thread-safe (the per-goroutine
// data lives on the stack; the shared summariser only carries the
// call counter).
type countingSummariser struct {
	calls atomic.Int64
}

func (c *countingSummariser) Summarise(
	_ context.Context,
	rc planner.RunContext,
	_ *planner.Trajectory,
) (*planner.TrajectorySummary, error) {
	c.calls.Add(1)
	return &planner.TrajectorySummary{
		Goals: []string{"goal-" + rc.Quadruple.RunID},
		Note:  "from " + rc.Quadruple.RunID,
	}, nil
}

// TestCompressionRunner_ConcurrentReuse_D025 is the D-025 contract
// gate (CLAUDE.md §5 + §11). N=128 concurrent invocations against ONE
// shared CompressionRunner:
//
//   - No data races (the race detector is the gate).
//   - No context bleed (each goroutine recovers its own RunID via
//     the stamped Summary.Note).
//   - No cancellation cross-talk (pre-cancelled ctxes on i%5==0
//     return ctx.Err() without affecting siblings).
//   - No goroutine leak (baseline runtime.NumGoroutine restored
//     within 500ms of WaitGroup join).
//
// Identity-mandatory: each goroutine carries a UNIQUE RunID (so we
// can detect bleed) inside the same tenant/user/session triple (the
// runner accepts any non-empty quadruple).
func TestCompressionRunner_ConcurrentReuse_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	summ := &countingSummariser{}
	runner := planner.NewCompressionRunner(summ)

	var (
		wg               sync.WaitGroup
		failures         int64
		bleeds           int64
		cancelCrossTalk  int64
		cancelHonoured   int64
		nonCancelSuccess int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()

			runID := fmt.Sprintf("run-%d", i)
			q := identity.Quadruple{
				Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
				RunID:    runID,
			}
			rc := planner.RunContext{
				Quadruple: q,
				Budget:    planner.Budget{TokenBudget: 10},
			}
			// Per-goroutine trajectory — heavy enough to exceed the
			// budget. Each carries the goroutine ID so we can detect
			// any cross-goroutine summary clobber.
			tr := &planner.Trajectory{
				Query: runID,
				LLMContext: map[string]any{
					"bulk":  strings.Repeat("x", 4096),
					"runid": runID,
				},
				ToolContext: trajectory.ToolContext{
					Serializable: map[string]any{"runid": runID},
				},
			}

			// Pre-cancel on i%5==0 to exercise the cancellation
			// honour path.
			ctx, cancel := context.WithCancel(context.Background())
			if i%5 == 0 {
				cancel()
			} else {
				defer cancel()
			}

			err := runner.MaybeCompress(ctx, rc, tr)
			if i%5 == 0 {
				// Pre-cancelled: expect ctx.Err().
				if !errors.Is(err, context.Canceled) {
					atomic.AddInt64(&cancelCrossTalk, 1)
				} else {
					atomic.AddInt64(&cancelHonoured, 1)
				}
				return
			}

			// Non-cancelled path: expect a stamped summary whose Note
			// names this goroutine's runID. A mismatch would mean
			// either a write race or a context bleed.
			if err != nil {
				atomic.AddInt64(&failures, 1)
				return
			}
			if tr.Summary == nil {
				atomic.AddInt64(&failures, 1)
				return
			}
			if !strings.HasSuffix(tr.Summary.Note, runID) {
				atomic.AddInt64(&bleeds, 1)
				return
			}
			atomic.AddInt64(&nonCancelSuccess, 1)
		}()
	}
	wg.Wait()

	if failures != 0 {
		t.Errorf("D-025: %d goroutines failed (race / summariser error)", failures)
	}
	if bleeds != 0 {
		t.Errorf("D-025 context bleed: %d goroutines saw another goroutine's summary Note", bleeds)
	}
	if cancelCrossTalk != 0 {
		t.Errorf("D-025 cancellation cross-talk: %d pre-cancelled goroutines did not see ctx.Canceled", cancelCrossTalk)
	}

	// Sanity: at least some of each category actually ran.
	if cancelHonoured == 0 {
		t.Errorf("no pre-cancelled goroutines observed (test plumbing broken)")
	}
	if nonCancelSuccess == 0 {
		t.Errorf("no non-cancelled goroutines succeeded (test plumbing broken)")
	}

	// Summariser was invoked only for non-cancelled goroutines (the
	// breaker honour fires before the summariser call). With i%5==0
	// pre-cancelled, the expected count is N - ceil(N/5) = 128 - 26 = 102.
	expectedCalls := int64(N - (N/5 + 1))
	if got := summ.calls.Load(); got != expectedCalls && got != expectedCalls+1 {
		t.Errorf("summariser calls = %d, want %d (= N - cancelled)", got, expectedCalls)
	}

	// Goroutine leak check: runtime.NumGoroutine should return to
	// baseline (within a tolerance) within 500ms.
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

// TestCompressionRunner_SharedAcrossGoroutines_NoRaceOnEstimator
// asserts that when N goroutines share the SAME trajectory (read-only)
// the estimator does not race. The runner's idempotency on
// tr.Summary != nil means only one goroutine actually stamps; the
// rest short-circuit. Race-free regardless.
func TestCompressionRunner_SharedAcrossGoroutines_NoRaceOnEstimator(t *testing.T) {
	const N = 64

	summ := &countingSummariser{}
	runner := planner.NewCompressionRunner(summ)

	// Single trajectory shared across N goroutines. Pre-stamp the
	// summary so every call short-circuits — this exercises the
	// idempotent read path under -race.
	tr := &planner.Trajectory{
		Query:   "shared",
		Summary: &planner.TrajectorySummary{Note: "pre-stamped"},
	}

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			rc := planner.RunContext{
				Quadruple: identity.Quadruple{
					Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
					RunID:    fmt.Sprintf("rs-%d", i),
				},
				Budget: planner.Budget{TokenBudget: 10},
			}
			_ = runner.MaybeCompress(context.Background(), rc, tr)
		}()
	}
	wg.Wait()

	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked %d times on idempotent shared-trajectory reads — want 0", summ.calls.Load())
	}
	if tr.Summary.Note != "pre-stamped" {
		t.Errorf("pre-stamped Summary clobbered under concurrent idempotent reads: Note=%q", tr.Summary.Note)
	}
}

// TestCompressionRunner_EmitClosure_ConcurrentSafe asserts the Emit
// closure receives every event without drops when N goroutines emit
// from a shared runner. The recordingEmit mutex serialises the writes;
// the test asserts the count matches the over-budget goroutine count.
func TestCompressionRunner_EmitClosure_ConcurrentSafe(t *testing.T) {
	const N = 64

	summ := &countingSummariser{}
	runner := planner.NewCompressionRunner(summ)

	rec := &recordingEmit{}
	emit := func(ev events.Event) { rec.emit(ev) }

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			rc := planner.RunContext{
				Quadruple: identity.Quadruple{
					Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
					RunID:    fmt.Sprintf("re-%d", i),
				},
				Budget: planner.Budget{TokenBudget: 10},
				Emit:   emit,
			}
			tr := &planner.Trajectory{
				LLMContext: map[string]any{"bulk": strings.Repeat("x", 4096)},
			}
			_ = runner.MaybeCompress(context.Background(), rc, tr)
		}()
	}
	wg.Wait()

	evs := rec.snapshot()
	if len(evs) != N {
		t.Errorf("emitted %d events, want %d (one per over-budget goroutine)", len(evs), N)
	}
}
