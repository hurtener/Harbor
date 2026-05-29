package main

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// devEnricher is the production tasks.get Enricher for the dev stack.
// It provides parent-session / cost / planner-snapshot enrichment from
// in-memory runtime state, plus (Phase 107a) trajectory projection.
//
// D-025-safe: the enricher is immutable after construction — the
// trajectory accessor is a pure function (no mutable receiver state).
type devEnricher struct {
	trajectoryFn func(tasks.TaskID) *planner.Trajectory
}

// ParentSession returns a zero-valued ref — the parent-session card
// is populated by the projector from the task identity when no
// enricher backfills it.
func (e *devEnricher) ParentSession(_ context.Context, _ identity.Identity, _ string) prototypes.TaskParentSessionRef {
	return prototypes.TaskParentSessionRef{}
}

// Cost returns a zero-valued cost rollup — cost aggregation is
// deferred to the `llm.cost.recorded` event stream.
func (e *devEnricher) Cost(_ context.Context, _ identity.Identity, _ string) prototypes.TaskCostRollup {
	return prototypes.TaskCostRollup{PerStep: []prototypes.TaskCostStep{}}
}

// PlannerSnapshot returns nil — planner-checkpoint references are
// deferred to Phase 51's checkpoint store.
func (e *devEnricher) PlannerSnapshot(_ context.Context, _ identity.Identity, _ string) *prototypes.TaskPlannerSnapshotRef {
	return nil
}

// Trajectory projects the planner's in-memory reasoning trace onto
// the Protocol wire. Steps with empty ReasoningTrace are filtered out.
// Returns nil when the task's trajectory is unavailable (evicted or
// the run-loop didn't store one).
func (e *devEnricher) Trajectory(_ context.Context, _ identity.Identity, taskID string) *prototypes.TaskTrajectoryRef {
	if e.trajectoryFn == nil {
		return nil
	}
	traj := e.trajectoryFn(tasks.TaskID(taskID))
	if traj == nil {
		return nil
	}
	steps := make([]prototypes.TaskTrajectoryStep, 0, len(traj.Steps))
	for i, step := range traj.Steps {
		if step.ReasoningTrace == "" {
			continue
		}
		steps = append(steps, prototypes.TaskTrajectoryStep{
			Index:          i,
			ReasoningTrace: step.ReasoningTrace,
		})
	}
	if len(steps) == 0 {
		return nil
	}
	return &prototypes.TaskTrajectoryRef{Steps: steps}
}
