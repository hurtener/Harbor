package protocol

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/projectioncheck"
	"github.com/hurtener/Harbor/internal/tasks"
)

// probeApprovalChecker is the gate-scoped ApprovalChecker double the tasks
// projection probe wires so the `has_pending_approval` assignment path runs
// over a populated source. It is NOT a production default — it exists only
// for the projection-completeness probe (Half A wires its own fake; Half B's
// prod-wiring test proves the real coordinator-backed checker is installed
// by mux.go).
type probeApprovalChecker struct{}

func (probeApprovalChecker) HasPendingApproval(context.Context, identity.Identity, string) bool {
	return true
}

// init self-registers the tasks projection surface into the
// projection-completeness gate (§4.4). It proves the tasks list
// projector assigns every facet field the `tasks.list` filter operates over
// — most sharply `has_pending_approval`, whose false-absence facet this
// phase fixed.
func init() {
	projectioncheck.Register(projectioncheck.ProjectionContract{
		Surface: "tasks",
		// Probe runs the PRODUCTION row projection (projectRow +
		// applyApproval) over a fully-populated task with the approval seam
		// wired, returning the projected TaskRow the gate reflects.
		Probe: func() any {
			p := &RegistryProjector{approvals: probeApprovalChecker{}}
			started := time.Unix(1_700_000_000, 0).UTC()
			task := &tasks.Task{
				ID: "probe-task",
				Identity: identity.Quadruple{
					Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
					RunID:    "run-1",
				},
				Kind:      tasks.KindBackground,
				Status:    tasks.StatusRunning,
				Priority:  5,
				CreatedAt: started.UnixNano(),
				UpdatedAt: started.Add(time.Second).UnixNano(),
			}
			row := projectRow(task)
			p.applyApproval(context.Background(), task, &row)
			return row
		},
		// The facet / sort axes tasks.list operates over that a single
		// populated fixture assigns. `has_pending_approval` is the
		// enrichment-seam field this phase fixed; the rest are the
		// always-assigned lifecycle axes the kanban filters/sorts on.
		OperatedFields: []string{"has_pending_approval", "status", "kind", "started_at", "duration_ms"},
		ProdWiringTest: "TestProdWiring_TasksProjectorInstallsApprovalChecker",
	})
}
