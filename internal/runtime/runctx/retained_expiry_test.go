package runctx_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
)

// An admitted run's frozen historical view is not an unlimited extension of
// source retention. Check before inference and again before accepting an action.
func TestRetainedContext_ExpiredFrozenSource(t *testing.T) {
	for _, during := range []bool{false, true} {
		name := "before inference"
		if during {
			name = "during inference"
		}
		t.Run(name, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			old := retainedBase("old", "expiry")
			first, err := runctx.BeginRetainedRun(t.Context(), store, redactor, old.Quadruple, 4, time.Minute, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err = first.Apply(&old); err != nil {
				t.Fatal(err)
			}
			old.Trajectory.Steps = append(old.Trajectory.Steps, planner.Step{LLMObservation: "source with limited retention"})
			if err = first.Finish(t.Context(), old.Trajectory, old.Query, "saved", "complete"); err != nil {
				t.Fatal(err)
			}
			base := retainedBase("next", "expiry")
			second, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Minute, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err = second.Apply(&base); err != nil {
				t.Fatal(err)
			}
			called := false
			guarded := second.GuardPlanner(retainedDecisionFunc(func(context.Context, planner.RunContext) (planner.Decision, error) {
				called = true
				if during {
					now = now.Add(time.Minute)
				}
				return planner.CallTool{Tool: "save"}, nil
			}), nil)
			if !during {
				now = now.Add(time.Minute)
			}
			decision, err := guarded.Next(t.Context(), base)
			if !errors.Is(err, runctx.ErrRetainedContextUnavailable) || decision != nil || called != during {
				t.Fatalf("expired source remained actionable: decision=%v error=%v called=%t", decision, err, called)
			}
		})
	}
}
