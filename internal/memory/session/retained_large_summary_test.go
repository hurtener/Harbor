package session_test

import (
	"context"
	"strings"
	"testing"
	"time"

	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
)

type retainedLargeSummary struct{ fact string }

func (s retainedLargeSummary) Summarise(context.Context, planner.RunContext, *planner.Trajectory) (*planner.TrajectorySummary, error) {
	return &planner.TrajectorySummary{Facts: []string{s.fact}}, nil
}

func TestRetainedCheckpoint_ConfiguredSummaryHasNoFixedByteCeiling(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			base := retainedBase("first", "large-summary")
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: "older"}, planner.Step{LLMObservation: "fresh"})
			presentEarlierCheckpointFixtureSteps(base.Trajectory)
			base.Budget.TokenBudget = 1
			fact := strings.Repeat("preserve-me ", 2048)
			if err := planner.NewCompressionRunner(retainedLargeSummary{fact: fact}).MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
				t.Fatal(err)
			}
			if err := r.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
				t.Fatalf("valid summary rejected by byte ceiling: %v", err)
			}
			next := retainedBase("next", "large-summary")
			r, err = sessionmemory.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 1, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&next); err != nil {
				t.Fatal(err)
			}
			if next.Trajectory.Summary == nil || len(next.Trajectory.Summary.Facts) != 1 || next.Trajectory.Summary.Facts[0] != fact {
				t.Fatal("large checkpoint did not round-trip exactly")
			}
		})
	}
}
