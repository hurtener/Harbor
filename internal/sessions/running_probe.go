package sessions

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TaskRunningProbe adapts a tasks.TaskRegistry into the RunningProbe
// seam so the GC sweeper can enforce RFC §6.9's "GC never reaps a
// session with a RUNNING task" invariant. The probe lists the
// session's tasks filtered to tasks.StatusRunning (the exact status
// the RFC names — PENDING / PAUSED tasks do not block GC) and reports
// whether any exist.
//
// Wiring: both reference assemblies (cmd/harbor `bootDevStack` and
// harbortest/devstack `Assemble`) pass this probe via
// sessions.New(..., WithGCPolicy(GCPolicy{RunningProbe: ...})). A
// registry constructed without it falls back to the no-op default,
// which reports no running tasks — sessions with in-flight work
// become reapable, so assemblies that own a TaskRegistry MUST wire
// this adapter.
//
// The import direction is sound by construction: internal/tasks does
// not import internal/sessions (verified at the time of authoring),
// so the adapter lives here, next to the RunningProbe type it
// satisfies.
func TaskRunningProbe(reg tasks.TaskRegistry) RunningProbe {
	return func(ctx context.Context, q identity.Quadruple) (bool, error) {
		if reg == nil {
			return false, fmt.Errorf("sessions: TaskRunningProbe requires a non-nil TaskRegistry")
		}
		status := tasks.StatusRunning
		summaries, err := reg.List(ctx, q.Identity, tasks.TaskFilter{Status: &status})
		if err != nil {
			return false, fmt.Errorf("sessions: running probe list: %w", err)
		}
		return len(summaries) > 0, nil
	}
}
