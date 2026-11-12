package steering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// The run loop is the only evidence writer. Inspectors share the mutex; they
// must see either the old checkpoint or its fully installed successor, and may
// continue reading while the summarizing model is running.
func compressTrajectory(ctx context.Context, spec RunSpec, rc planner.RunContext) error {
	return compressTrajectoryWith(ctx, spec, rc, spec.Compression.MaybeCompress)
}

// compressRequest maintains the assembled request's context. A maintenance
// failure never fails the run by itself: the previous checkpoint is kept, and a
// request that still fits continues unchanged (ErrCompactionNoProgress tells
// the preparation layer so). A request that no longer fits is reduced
// deterministically, oldest observation first, until it does.
func compressRequest(ctx context.Context, spec RunSpec, rc planner.RunContext, inputTokens, target int) (bool, error) {
	if rc.Trajectory == nil {
		return false, planner.ErrNilTrajectory
	}
	original := rc.Trajectory.Summary
	rc.Budget.TokenBudget = target
	err := compressTrajectoryWith(ctx, spec, rc, func(ctx context.Context, rc planner.RunContext, tr *planner.Trajectory) error {
		return spec.Compression.MaybeCompressRequest(ctx, rc, tr, inputTokens)
	})
	changed := rc.Trajectory.Summary != original
	if err == nil || changed || !recoverableCompaction(ctx, err) {
		return changed, err
	}
	fit := llm.CheckContextCandidate(ctx)
	if !errors.Is(fit, llm.ErrContextWindowExceeded) {
		if fit != nil && !errors.Is(fit, llm.ErrCompactionNoProgress) {
			return false, fit
		}
		slog.WarnContext(ctx, "steering: context maintenance failed; continuing with the previous checkpoint",
			slog.String("run_id", rc.Quadruple.RunID))
		return false, fmt.Errorf("%w: %w", llm.ErrCompactionNoProgress, err)
	}
	reduced, rerr := reduceObservations(ctx, spec, rc)
	if rerr != nil {
		return reduced > 0, rerr
	}
	if reduced == 0 {
		return false, err
	}
	slog.WarnContext(ctx, "steering: context maintenance failed; reduced oldest observations to previews with references",
		slog.String("run_id", rc.Quadruple.RunID), slog.Int("reduced_exchanges", reduced))
	return true, nil
}

// reduceObservations installs reduced exchanges one at a time under the
// inspection mutex and stops as soon as the rebuilt request fits.
func reduceObservations(ctx context.Context, spec RunSpec, rc planner.RunContext) (int, error) {
	tr := rc.Trajectory
	mu := spec.TrajectoryMu
	return spec.Compression.ReduceOldestObservations(ctx, rc, tr, func(index int, reduced planner.Step) (bool, error) {
		if mu != nil {
			mu.Lock()
			defer mu.Unlock()
		}
		if index >= len(tr.Steps) {
			return false, planner.ErrStaleSummary
		}
		tr.Steps[index] = reduced
		check := llm.CheckContextCandidate(ctx)
		switch {
		case check == nil:
			return true, nil
		case errors.Is(check, llm.ErrContextWindowExceeded), errors.Is(check, llm.ErrCompactionNoProgress):
			return false, nil
		default:
			return false, check
		}
	})
}

// recoverableCompaction separates maintenance failures the run survives from
// cancellation and missing identity, which stay terminal.
func recoverableCompaction(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, llm.ErrIdentityMissing) {
		return false
	}
	return true
}

func compressTrajectoryWith(ctx context.Context, spec RunSpec, rc planner.RunContext, compress func(context.Context, planner.RunContext, *planner.Trajectory) error) error {
	tr := rc.Trajectory
	if tr == nil {
		return compress(ctx, rc, tr)
	}
	mu := spec.TrajectoryMu
	if mu != nil {
		mu.RLock()
	}
	snapshot := *tr
	snapshot.Steps = append([]planner.Step(nil), tr.Steps...)
	original := tr.Summary
	if mu != nil {
		mu.RUnlock()
	}

	var pending []events.Event
	copyRC := rc
	copyRC.Trajectory = &snapshot
	copyRC.Emit = func(ev events.Event) { pending = append(pending, ev) }
	err := compress(ctx, copyRC, &snapshot)
	if err == nil && snapshot.Summary != original {
		if mu != nil {
			mu.Lock()
		}
		candidate := snapshot.Summary
		digest, digestErr := tr.PrefixDigest(candidate.Coverage.ThroughStep)
		if digestErr != nil || digest != candidate.Coverage.PrefixDigest || tr.Summary != original || ctx.Err() != nil {
			err = planner.ErrStaleSummary
			if ctx.Err() != nil {
				err = ctx.Err()
			}
		} else {
			tr.Summary = candidate
			if checkErr := llm.CheckContextCandidate(ctx); checkErr != nil {
				tr.Summary = original
				err = checkErr
				now := time.Now()
				if rc.Clock != nil {
					now = rc.Clock()
				}
				pending = append(pending, events.Event{
					Type:     planner.EventTypeTrajectoryCompressionFailed,
					Identity: rc.Quadruple, OccurredAt: now,
					Payload: planner.TrajectoryCompressionFailedPayload{
						Identity: rc.Quadruple, OccurredAt: now,
						StepsObserved: len(tr.Steps), ErrorCode: "candidate_request_rejected",
						ErrorMessage: "candidate request failed validation; prior checkpoint retained",
					},
				})
			}
		}
		if mu != nil {
			mu.Unlock()
		}
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
