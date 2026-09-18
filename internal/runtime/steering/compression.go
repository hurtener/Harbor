package steering

import (
	"context"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/planner"
)

// The run loop is the only evidence writer. Inspectors share the mutex; they
// must see either the old checkpoint or its fully installed successor, and may
// continue reading while the summarizing model is running.
func compressTrajectory(ctx context.Context, spec RunSpec, rc planner.RunContext) error {
	return compressTrajectoryWith(ctx, spec, rc, spec.Compression.MaybeCompress)
}

func compressRequest(ctx context.Context, spec RunSpec, rc planner.RunContext, inputTokens, target int) (bool, error) {
	if rc.Trajectory == nil {
		return false, planner.ErrNilTrajectory
	}
	original := rc.Trajectory.Summary
	rc.Budget.TokenBudget = target
	err := compressTrajectoryWith(ctx, spec, rc, func(ctx context.Context, rc planner.RunContext, tr *planner.Trajectory) error {
		return spec.Compression.MaybeCompressRequest(ctx, rc, tr, inputTokens)
	})
	return rc.Trajectory.Summary != original, err
}

func compressTrajectoryWith(ctx context.Context, spec RunSpec, rc planner.RunContext, compress func(context.Context, planner.RunContext, *planner.Trajectory) error) error {
	tr := rc.Trajectory
	if spec.TrajectoryMu == nil || tr == nil {
		return compress(ctx, rc, tr)
	}
	mu := spec.TrajectoryMu
	mu.RLock()
	snapshot := *tr
	snapshot.Steps = append([]planner.Step(nil), tr.Steps...)
	original := tr.Summary
	mu.RUnlock()

	var pending []events.Event
	copyRC := rc
	copyRC.Trajectory = &snapshot
	copyRC.Emit = func(ev events.Event) { pending = append(pending, ev) }
	err := compress(ctx, copyRC, &snapshot)
	if err == nil && snapshot.Summary != original {
		mu.Lock()
		candidate := snapshot.Summary
		digest, digestErr := tr.PrefixDigest(candidate.Coverage.ThroughStep)
		if digestErr != nil || digest != candidate.Coverage.PrefixDigest || tr.Summary != original || ctx.Err() != nil {
			err = planner.ErrStaleSummary
			if ctx.Err() != nil {
				err = ctx.Err()
			}
		} else {
			tr.Summary = candidate
		}
		mu.Unlock()
	}
	if rc.Emit != nil {
		for _, ev := range pending {
			// A candidate that lost the publication race was not installed.
			if err == nil || ev.Type != planner.EventTypeTrajectoryCompressed {
				rc.Emit(ev)
			}
		}
	}
	return err
}
