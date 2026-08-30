package serve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

type runLoopFlushBus struct {
	events.EventBus
	calls   atomic.Int32
	flushed atomic.Bool
	err     error
}

func (b *runLoopFlushBus) Flush(context.Context) error {
	b.calls.Add(1)
	if b.err == nil {
		b.flushed.Store(true)
	}
	return b.err
}

type flushOrderingRegistry struct {
	tasks.TaskRegistry
	flushed             *atomic.Bool
	completeBeforeFlush atomic.Bool
}

func (r *flushOrderingRegistry) MarkComplete(ctx context.Context, id tasks.TaskID, result tasks.TaskResult) error {
	if !r.flushed.Load() {
		r.completeBeforeFlush.Store(true)
	}
	return r.TaskRegistry.MarkComplete(ctx, id, result)
}

func TestRunOne_FinishGoalFlushesBeforeMarkComplete(t *testing.T) {
	env := newFailDriverEnv(t)
	barrier := &runLoopFlushBus{EventBus: env.bus}
	env.bus = barrier
	reg := &flushOrderingRegistry{TaskRegistry: env.reg, flushed: &barrier.flushed}
	startFailDriver(t, env, func(opts *RunLoopDriverOptions) {
		opts.Tasks = reg
	})

	id := spawnOn(t, env.reg, nil)
	if status := waitForTaskStatus(t, env.reg, id, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
		t.Fatalf("task status = %q, want complete", status)
	}
	if got := barrier.calls.Load(); got != 1 {
		t.Fatalf("Flush calls = %d, want 1", got)
	}
	if reg.completeBeforeFlush.Load() {
		t.Fatal("MarkComplete ran before the successful publication barrier")
	}
}

func TestRunOne_FlushFailureFailsTaskWithoutSuccessfulSeal(t *testing.T) {
	env := newFailDriverEnv(t)
	barrier := &runLoopFlushBus{EventBus: env.bus, err: errInjected}
	env.bus = barrier
	startFailDriver(t, env, nil)

	spawnAndAwaitFailure(t, env.reg, nil,
		planner.TaskErrorCodeRunLoopError,
		"event flush before successful turn completion")
	if got := barrier.calls.Load(); got != 1 {
		t.Fatalf("Flush calls = %d, want 1", got)
	}
}

func TestRunOne_NonGoalFailureDoesNotInvokeSuccessBarrier(t *testing.T) {
	env := newFailDriverEnv(t)
	barrier := &runLoopFlushBus{EventBus: env.bus, err: errInjected}
	env.bus = barrier
	startFailDriver(t, env, func(opts *RunLoopDriverOptions) {
		opts.Planner = &noPathPlanner{}
	})

	spawnAndAwaitFailure(t, env.reg, nil, "", "")
	if got := barrier.calls.Load(); got != 0 {
		t.Fatalf("Flush calls on non-goal failure = %d, want 0", got)
	}
}
