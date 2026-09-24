package serve

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestRunOne_MemoryBudgetProjectionError_FailsRun(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.AgentConfig = &countingFailRegistry{failAt: 6}
		o.AgentConfigID = "fail-agent"
	})
	spawnAndAwaitFailure(t, env.reg, nil, planner.TaskErrorCodeRunLoopError, "memory-budget projection failed")
}

type memoryBudgetPlanner struct {
	seen    chan int
	release chan struct{}
}

func (p *memoryBudgetPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	select {
	case p.seen <- rc.Budget.TokenBudget:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

type unexpectedBudgetSummary struct{}

func (unexpectedBudgetSummary) Summarise(context.Context, planner.RunContext, *planner.Trajectory) (*planner.TrajectorySummary, error) {
	return nil, fmt.Errorf("empty trajectory must not need compaction")
}

func TestRunOne_MemoryBudgetNextRun(t *testing.T) {
	reg := acTestRegistry(t)
	q := identity.Quadruple{Identity: runLoopDriverTestID}
	const agent = "budget-agent"
	set := func(n int) {
		t.Helper()
		if _, err := reg.SetRevision(t.Context(), q, agent, agentcfg.ConfigScopeAgent,
			agentcfg.ConfigPayload{Memory: &agentcfg.MemorySection{BudgetTokens: n}}, agentcfg.SetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	set(64000)
	env := newFailDriverEnv(t)
	probe := &memoryBudgetPlanner{seen: make(chan int, 2), release: make(chan struct{})}
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.AgentConfig = reg
		o.AgentConfigID = agent
		o.TokenBudget = 12000
		o.Compression = planner.NewCompressionRunner(unexpectedBudgetSummary{})
		o.Planner = probe
	})
	first := spawnOn(t, env.reg, nil)
	select {
	case got := <-probe.seen:
		if got != 64000 {
			t.Fatalf("planner budget=%d, want revision override", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not reach planner")
	}
	set(32000)
	close(probe.release)
	if got := waitForTaskStatus(t, env.reg, first, tasks.StatusComplete, 5*time.Second); got != tasks.StatusComplete {
		t.Fatalf("first run: %s", got)
	}
	second := spawnOn(t, env.reg, nil)
	select {
	case got := <-probe.seen:
		if got != 32000 {
			t.Fatalf("next-run budget=%d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("next run did not reach planner")
	}
	if got := waitForTaskStatus(t, env.reg, second, tasks.StatusComplete, 5*time.Second); got != tasks.StatusComplete {
		t.Fatalf("second run: %s", got)
	}
}
