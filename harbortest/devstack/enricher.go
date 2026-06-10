// enricher.go — the devstack tasks.get Enricher, the D-094 mirror of
// `cmd/harbor/dev_enricher.go` (Phase 107a; D-195 dated-note
// follow-up). The duplication is intentional per D-094's
// source-of-truth invariant: the run-loop driver shell — and the
// enricher that projects its trajectory map — stays per-caller
// (D-197 call 4). When the production shape evolves, both move in the
// same PR.

package devstack

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// devStackEnricher is the devstack tasks.get Enricher. It provides
// parent-session / cost / planner-snapshot enrichment from in-memory
// runtime state, plus (Phase 107a) trajectory projection.
//
// D-025-safe: the enricher is immutable after construction — the
// trajectory accessor is a pure function (no mutable receiver state).
type devStackEnricher struct {
	trajectoryFn func(tasks.TaskID) *planner.Trajectory
}

// ParentSession returns a zero-valued ref — the parent-session card
// is populated by the projector from the task identity when no
// enricher backfills it.
func (e *devStackEnricher) ParentSession(_ context.Context, _ identity.Identity, _ string) types.TaskParentSessionRef {
	return types.TaskParentSessionRef{}
}

// Cost returns a zero-valued cost rollup — cost aggregation is
// deferred to the `llm.cost.recorded` event stream.
func (e *devStackEnricher) Cost(_ context.Context, _ identity.Identity, _ string) types.TaskCostRollup {
	return types.TaskCostRollup{PerStep: []types.TaskCostStep{}}
}

// PlannerSnapshot returns nil — planner-checkpoint references are
// deferred to Phase 51's checkpoint store.
func (e *devStackEnricher) PlannerSnapshot(_ context.Context, _ identity.Identity, _ string) *types.TaskPlannerSnapshotRef {
	return nil
}

// Trajectory projects the planner's in-memory reasoning trace onto
// the Protocol wire. Steps with empty ReasoningTrace are filtered out.
// Returns nil when the task's trajectory is unavailable (evicted or
// the run-loop didn't store one).
func (e *devStackEnricher) Trajectory(_ context.Context, _ identity.Identity, taskID string) *types.TaskTrajectoryRef {
	if e.trajectoryFn == nil {
		return nil
	}
	traj := e.trajectoryFn(tasks.TaskID(taskID))
	if traj == nil {
		return nil
	}
	steps := make([]types.TaskTrajectoryStep, 0, len(traj.Steps))
	for i, step := range traj.Steps {
		if step.ReasoningTrace == "" {
			continue
		}
		steps = append(steps, types.TaskTrajectoryStep{
			Index:          i,
			ReasoningTrace: step.ReasoningTrace,
		})
	}
	if len(steps) == 0 {
		return nil
	}
	return &types.TaskTrajectoryRef{Steps: steps}
}
