package serve

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// multiSlowCallToolPlanner emits `calls` CallTool decisions (each to a
// deliberately slow tool) before finishing — widening the trajectory-append
// window so a concurrent out-of-band reader is GUARANTEED to overlap the
// RunLoop's per-step append, making the race regression test deterministic
// rather than probabilistic.
type multiSlowCallToolPlanner struct {
	mu    sync.Mutex
	seen  int
	calls int
}

func (p *multiSlowCallToolPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen++
	if p.seen <= p.calls {
		return planner.CallTool{Tool: "slow-echo", Args: json.RawMessage(`{}`)}, nil
	}
	return planner.Finish{Reason: planner.FinishGoal, Payload: map[string]any{"answer": "done"}}, nil
}

// TestRunLoopDriver_TrajectoryByTaskID_ConcurrentDuringAppend is the
// DETERMINISTIC regression pin for the trajectory-append data race the phase
// closed (D-293 as-built #6): the serve Enricher's `tasks.get` read of an
// IN-FLIGHT run's reasoning trace (via TrajectoryByTaskID) raced the steering
// RunLoop's per-step `Trajectory.Steps` append.
//
// It drives a REAL multi-step run through a REAL RunLoopDriver + steering
// RunLoop (each step dispatches a slow tool, so the append window spans real
// time) and hammers `TrajectoryByTaskID` from N reader goroutines FOR THE
// ENTIRE run. Under `-race` this exercises the fixed path end-to-end: the read
// takes a defensive snapshot of the Steps slice under the per-run
// `RunSpec.TrajectoryMu` the steering append also holds. WITHOUT that shared
// mutex (verified by temporarily dropping the RLock in TrajectoryByTaskID) the
// snapshot's `append([]Step(nil), traj.Steps...)` races the RunLoop append and
// the race detector fires; WITH it, the run completes cleanly.
func TestRunLoopDriver_TrajectoryByTaskID_ConcurrentDuringAppend(t *testing.T) {
	env := newFailDriverEnv(t)

	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "slow-echo"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			// Widen the per-step append window so the concurrent reader
			// reliably overlaps the RunLoop's Steps append.
			time.Sleep(2 * time.Millisecond)
			return tools.ToolResult{Value: "echoed"}, nil
		},
	}); err != nil {
		t.Fatalf("register slow-echo: %v", err)
	}

	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })
	executor := dispatch.NewToolExecutor(cat, artStore, env.reg,
		dispatch.WithHeavyThreshold(32768), dispatch.WithMaxSpawnDepth(2))

	const calls = 8
	d := startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Planner = &multiSlowCallToolPlanner{calls: calls}
		o.Catalog = cat
		o.Executor = executor
		o.ArtifactStore = artStore
		o.MaxStepsRunLoop = calls + 2
	})

	id := spawnOn(t, env.reg, nil)

	// N readers hammer TrajectoryByTaskID for the entire in-flight run — the
	// exact out-of-band read (a tasks.get of a running task) that raced the
	// append before the fix.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if tr := d.TrajectoryByTaskID(id); tr != nil {
					// Touch the snapshot's Steps — the read the race detector
					// pairs against the RunLoop append.
					n := 0
					for range tr.Steps {
						n++
					}
					_ = n
				}
			}
		}()
	}

	status := waitForTaskStatus(t, env.reg, id, tasks.StatusComplete, 15*time.Second)
	close(stop)
	wg.Wait()
	if status != tasks.StatusComplete {
		t.Fatalf("multi-tool run stuck at %q, want complete", status)
	}

	// Sanity: the completed run appended its tool steps (proves the readers
	// were racing a REAL append window, not a no-op run).
	final := d.TrajectoryByTaskID(id)
	if final == nil || len(final.Steps) < calls {
		got := 0
		if final != nil {
			got = len(final.Steps)
		}
		t.Fatalf("completed trajectory has %d steps, want ≥ %d (the append window the readers raced)", got, calls)
	}
}
