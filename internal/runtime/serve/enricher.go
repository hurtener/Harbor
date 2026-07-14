package serve

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Enricher is the production tasks.get Enricher for the dev stack.
// It provides parent-session / planner-snapshot / trajectory enrichment
// from in-memory runtime state.
//
// safe for concurrent reuse: the enricher is immutable after construction —
// the trajectory accessor is a pure function, the session lister is itself
// safe for concurrent reuse, and no mutable receiver state is added.
type Enricher struct {
	trajectoryFn func(tasks.TaskID) *planner.Trajectory
	// sessions is the OPTIONAL session lister the parent-session card reads
	// the parent session's lifecycle status + timestamps from. Nil ⇒ the
	// card falls back to the projector's SessionID-only baseline (honest
	// "we don't have this data").
	sessions sessions.SessionLister
}

// EnricherOption configures NewEnricher.
type EnricherOption func(*Enricher)

// WithSessionLister wires the session lister the parent-session card reads
// the parent session's status + timestamps from. A nil lister is treated as
// "not supplied".
func WithSessionLister(l sessions.SessionLister) EnricherOption {
	return func(e *Enricher) {
		if l != nil {
			e.sessions = l
		}
	}
}

// NewEnricher builds the tasks.get Enricher over a trajectory accessor.
// The accessor is a pure function (the run-loop driver's TrajectoryByTaskID),
// so the returned Enricher is safe for concurrent reuse.
func NewEnricher(trajectoryFn func(tasks.TaskID) *planner.Trajectory, opts ...EnricherOption) *Enricher {
	e := &Enricher{trajectoryFn: trajectoryFn}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// ParentSession returns the parent-session card for the task — the
// session's lifecycle status + start/last-activity timestamps, read from
// the session lister scoped to the caller's own identity (the task's
// session is the caller's session; GetTask already scoped the lookup).
//
// AgentName is left EMPTY on purpose: no single-valued session→agent
// binding exists in V1 (a session may run several agents over its life), so
// its absence is representable (omitted) rather than a fabricated value —
// the same honest-omission the sessions projection takes for its agent
// fields (the silent-absence class rule). The projector overlays only the non-zero fields, so the
// SessionID baseline it set is preserved.
func (e *Enricher) ParentSession(ctx context.Context, id identity.Identity, _ string) prototypes.TaskParentSessionRef {
	if e.sessions == nil || id.SessionID == "" {
		return prototypes.TaskParentSessionRef{}
	}
	snaps, err := e.sessions.ListSnapshots(ctx, sessions.SessionListFilter{
		SessionIDs:    []string{id.SessionID},
		TenantIDs:     []string{id.TenantID},
		UserIDs:       []string{id.UserID},
		IncludeClosed: true,
	})
	if err != nil {
		// A lister failure leaves the card at the SessionID baseline — an
		// honest empty overlay, never a fabricated status.
		return prototypes.TaskParentSessionRef{}
	}
	for _, snap := range snaps {
		if snap.ID != id.SessionID {
			continue
		}
		return prototypes.TaskParentSessionRef{
			Status:        parentSessionStatus(snap),
			StartedAt:     snap.OpenedAt,
			LatestEventAt: snap.LastSeen,
		}
	}
	return prototypes.TaskParentSessionRef{}
}

// parentSessionStatus maps a session snapshot onto the parent-session
// card's lifecycle status string — the same lens the sessions projection
// uses (an open or running session reads "running"; a closed-failed session
// "failed"; an otherwise-closed session "completed").
func parentSessionStatus(snap sessions.SessionSnapshot) string {
	switch {
	case snap.Running:
		return string(prototypes.SessionStatusRunning)
	case !snap.Closed:
		return string(prototypes.SessionStatusRunning)
	case snap.Closed && snap.ClosedReason == "failed":
		return string(prototypes.SessionStatusFailed)
	default:
		return string(prototypes.SessionStatusCompleted)
	}
}

// Cost returns a zero-valued cost rollup. Per-task cost aggregation from
// the `llm.cost.recorded` event stream is DEFERRED to the tools annotator
// follow-up (a documented risk-note deferral): the sessions cost helper is
// session-scoped and not extractable for a task-scoped rollup without a
// divergent second reader, so the un-stub lands with the tools annotator
// work rather than forking the cost path. This is display-only (no
// facet/sort operates over the task cost rollup), so the honest zero is
// gate-clean.
func (e *Enricher) Cost(_ context.Context, _ identity.Identity, _ string) prototypes.TaskCostRollup {
	return prototypes.TaskCostRollup{PerStep: []prototypes.TaskCostStep{}}
}

// PlannerSnapshot returns nil — planner-checkpoint references are
// deferred to the checkpoint store.
func (e *Enricher) PlannerSnapshot(_ context.Context, _ identity.Identity, _ string) *prototypes.TaskPlannerSnapshotRef {
	return nil
}

// Trajectory projects the planner's in-memory reasoning trace onto
// the Protocol wire. Steps with empty ReasoningTrace are filtered out.
// Returns nil when the task's trajectory is unavailable (evicted or
// the run-loop didn't store one).
func (e *Enricher) Trajectory(_ context.Context, _ identity.Identity, taskID string) *prototypes.TaskTrajectoryRef {
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
