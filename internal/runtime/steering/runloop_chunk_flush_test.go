package steering

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

type chunkFlushPlanner struct {
	order *[]string
}

func (p *chunkFlushPlanner) Next(context.Context, planner.RunContext) (planner.Decision, error) {
	*p.order = append(*p.order, "next")
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func TestRun_AfterPlannerStepRunsBeforeDecisionProgression(t *testing.T) {
	order := make([]string, 0, 2)
	rl, _, _ := newTestRunLoop(t)
	spec := runSpecFor(runA, &chunkFlushPlanner{order: &order})
	spec.Base.AfterPlannerStep = func(context.Context) error {
		order = append(order, "flush")
		return nil
	}
	if _, err := rl.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"next", "flush"}) {
		t.Fatalf("step order = %v, want [next flush]", order)
	}
}

func TestRun_AfterPlannerStepFailurePropagates(t *testing.T) {
	want := errors.New("chunk persistence failed")
	rl, _, _ := newTestRunLoop(t)
	spec := runSpecFor(runA, &chunkFlushPlanner{order: new([]string)})
	var calls int
	spec.Base.AfterPlannerStep = func(context.Context) error {
		calls++
		return want
	}
	if _, err := rl.Run(context.Background(), spec); err == nil || !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want wrapped chunk persistence failure", err)
	}
	if calls != 1 {
		t.Fatalf("AfterPlannerStep calls = %d, want 1", calls)
	}
}
