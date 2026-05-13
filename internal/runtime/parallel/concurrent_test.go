package parallel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestConcurrent_ExecutorIsReusableAcrossNCalls — the §11 + §5 +
// D-025 mandate. One shared *parallel.Executor; N=128 concurrent
// invocations under -race; per-call identity quadruple round-trips
// without bleed; cancelling one ctx does not affect siblings;
// goroutine baseline restored after WaitGroup.Wait.
func TestConcurrent_ExecutorIsReusableAcrossNCalls(t *testing.T) {
	t.Parallel()

	resolver := newStub()
	// Register a tool that reads identity from ctx and echoes it back
	// in the result so the test can detect cross-call bleed.
	resolver.Register("echo_identity",
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			q, ok := identity.QuadrupleFrom(ctx)
			if !ok {
				return tools.ToolResult{}, fmt.Errorf("identity missing")
			}
			return tools.ToolResult{Value: map[string]any{
				"run_id":  q.RunID,
				"session": q.SessionID,
				"args":    string(args),
			}}, nil
		},
		nil,
	)
	exec := parallel.New(resolver)

	const N = 128
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(N)
	var failures atomic.Int64
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()

			runID := fmt.Sprintf("r-%d", i)
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", i%4),
					UserID:    fmt.Sprintf("u-%d", i),
					SessionID: fmt.Sprintf("s-%d", i),
				},
				RunID: runID,
			}
			ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
			if err != nil {
				failures.Add(1)
				return
			}

			// Cancel-one-not-affecting-others stress: a sub-context
			// cancelled at random does not affect the executor's
			// derived ctxes for other goroutines.
			if i%17 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			results, err := exec.Execute(ctx, planner.CallParallel{
				Branches: []planner.CallTool{
					{Tool: "echo_identity", Args: json.RawMessage(fmt.Sprintf(`{"i":%d,"b":1}`, i))},
					{Tool: "echo_identity", Args: json.RawMessage(fmt.Sprintf(`{"i":%d,"b":2}`, i))},
				},
				Join: &planner.JoinSpec{Kind: planner.JoinAll},
			})
			if i%17 == 0 {
				// The cancelled ctx path is allowed to surface either
				// an error from Execute (identity check happens before
				// branch dispatch — ctx is checked after identity, so
				// we may get context.Canceled) OR a results slice with
				// every Result.Err == context.Canceled (the branches'
				// invokeBranch ctx.Err() short-circuit). Either is
				// valid — we just must not see a result whose run_id
				// belongs to a DIFFERENT goroutine (bleed).
				if err == nil {
					for _, r := range results {
						if r.Result != nil {
							got, _ := r.Result.Value.(map[string]any)["run_id"].(string)
							if got != runID {
								failures.Add(1)
							}
						}
					}
				}
				return
			}
			if err != nil {
				failures.Add(1)
				return
			}
			if len(results) != 2 {
				failures.Add(1)
				return
			}
			for _, r := range results {
				if r.Err != nil {
					failures.Add(1)
					return
				}
				m, ok := r.Result.Value.(map[string]any)
				if !ok {
					failures.Add(1)
					return
				}
				// Identity-bleed detector — the per-call ctx must
				// surface to the tool's Invoke without leakage.
				if got, _ := m["run_id"].(string); got != runID {
					failures.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Errorf("failures = %d, want 0 (D-025 — no races / bleed / cross-talk under N=128)", failures.Load())
	}

	// Goroutine baseline restoration. Bounded poll (no time.Sleep for
	// sync — we use an `eventually` shape per §17.4).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+8 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if leak := runtime.NumGoroutine() - baseline; leak > 16 {
		t.Errorf("goroutine leak: baseline=%d, after=%d, leak=%d (D-025 — no leaks)",
			baseline, runtime.NumGoroutine(), leak)
	}
}
