package serve

import (
	"context"
	"errors"
	"fmt"
	"time"

	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// runWithRetainedContext shares the embedded runner's private context window
// and required dispatch checkpoints.
// Admission occurs after agent/route/catalog resolution, before the first model
// or tool action. Child tasks never import or publish a root conversation turn.
func (d *RunLoopDriver) runWithRetainedContext(ctx context.Context, spec steering.RunSpec, root bool, inputIDs []string) (planner.Finish, error) {
	if d.retainedContextTurns == 0 || !root {
		return d.runLoop.Run(ctx, spec)
	}
	if err := sessionmemory.ValidateRetainedInputs(inputIDs, spec.Base.InputArtifacts); err != nil {
		return planner.Finish{}, err
	}
	retained, err := sessionmemory.BeginRetainedRun(ctx, d.stateStore, d.redactor, spec.Base.Quadruple, d.retainedContextTurns, d.retainedContextTTL, nil)
	if err != nil {
		return planner.Finish{}, fmt.Errorf("serve: retained context admission: %w", err)
	}
	// The trajectory is already discoverable by the inspector. Apply its
	// frozen prefix under the same mutex the run loop uses for later appends.
	if spec.TrajectoryMu != nil {
		spec.TrajectoryMu.Lock()
	}
	err = retained.Apply(&spec.Base)
	if spec.TrajectoryMu != nil {
		spec.TrajectoryMu.Unlock()
	}
	var fin planner.Finish
	if err == nil {
		err = retained.Start(ctx, spec.Base)
	}
	if err == nil {
		spec.DispatchCheckpoint = retained
		spec.CompactBeforeFirstDecision = retained.CompactionRequired()
		spec.Planner = retained.GuardPlanner(spec.Planner, d.artifactStore)
		fin, err = d.runLoop.Run(ctx, spec)
	}
	status, answer := "interrupted", ""
	if errors.Is(err, context.Canceled) || fin.Reason == planner.FinishCancelled {
		status = "cancelled"
	}
	if err == nil && fin.Reason == planner.FinishGoal {
		// An invalid final answer cannot become a successful retained turn.
		// The same builder validates the outward TaskResult later in runOne.
		envelope, envelopeErr := runctx.FinishAnswerEnvelope(fin, spec.Base.Trajectory, spec.Base.OutputSchema)
		if envelopeErr != nil {
			err = envelopeErr
		} else {
			status, answer = "complete", envelope.Answer
		}
	}
	// Execution has returned, including cancellation. Preserve known outcomes
	// before exposing terminal task status; this bounded write never retries an
	// external action or treats a lost receipt as proof a write did not happen.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if persistErr := retained.Finish(persistCtx, spec.Base.Trajectory, spec.Base.Query, answer, status); persistErr != nil {
		err = errors.Join(err, fmt.Errorf("serve: retained context terminal write: %w", persistErr))
	}
	return fin, err
}
