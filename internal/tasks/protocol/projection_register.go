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
		// Probe runs the PRODUCTION row projection (projectRow + applyGroup +
		// applyApproval — the exact three assignment sites ListTasks uses)
		// over a fully-populated FAILED + PARENTED + GROUPED task with the
		// approval seam wired, so every CONDITIONALLY-assigned facet field
		// (`error_class` only on a failed task, `parent_task_id` only on a
		// child, `group_id` only on a group member) is actually exercised —
		// a regression dropping any one of those assignments fails the gate.
		Probe: func() any {
			p := &RegistryProjector{approvals: probeApprovalChecker{}}
			started := time.Unix(1_700_000_000, 0).UTC()
			parent := tasks.TaskID("parent-task")
			task := &tasks.Task{
				ID: "probe-task",
				Identity: identity.Quadruple{
					Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
					RunID:    "run-1",
				},
				Kind:         tasks.KindBackground,
				Status:       tasks.StatusFailed,
				Priority:     5,
				ParentTaskID: &parent,
				Description:  "probe description",
				Query:        "probe query",
				Error:        &tasks.TaskError{Code: "probe_error_class"},
				CreatedAt:    started.UnixNano(),
				UpdatedAt:    started.Add(time.Second).UnixNano(),
			}
			row := projectRow(task)
			applyGroup(&row, map[tasks.TaskID]tasks.TaskGroupID{task.ID: "probe-group"}, task.ID)
			p.applyApproval(context.Background(), task, &row)
			return row
		},
		// Every facet / sort / search axis tasks.list operates over
		// (list.go filterMatches). `has_pending_approval` is the
		// enrichment-seam field this phase fixed; `error_class` /
		// `parent_task_id` / `group_id` are the CONDITIONALLY-assigned axes
		// (the regression-prone shape the gate exists to guard); the rest are
		// always-assigned lifecycle / search / identity axes.
		OperatedFields: []string{
			"has_pending_approval", "status", "kind", "started_at", "duration_ms",
			"error_class", "parent_task_id", "group_id", "identity",
			"description", "query",
		},
		ProdWiringTest: "TestProdWiring_TasksListThroughBuildMux",
	})
}
