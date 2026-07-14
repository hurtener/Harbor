package serve

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// ApprovalChecker is the production tasks.list approval-gate seam — it
// answers "does this task's run hold an open HITL / tool-approval gate?"
// by reading the pause coordinator, scoped to the task's own identity + run
// (the projection-completeness gate). It satisfies the tasks/protocol.ApprovalChecker interface
// structurally, keeping the pause package out of tasks/protocol's import
// graph (§4.4 seam).
//
// safe for concurrent reuse: immutable after construction — it holds only
// the coordinator reference (itself safe for concurrent reuse) and reads all
// per-call state from its arguments.
type ApprovalChecker struct {
	pauses pauseresume.Coordinator
}

// NewApprovalChecker builds the coordinator-backed approval checker. A nil
// coordinator returns nil so the caller leaves the seam unwired (the row's
// has_pending_approval stays at its honest false — the projection gate's
// Half-B prod-wiring test proves production wires a non-nil coordinator).
func NewApprovalChecker(pauses pauseresume.Coordinator) *ApprovalChecker {
	if pauses == nil {
		return nil
	}
	return &ApprovalChecker{pauses: pauses}
}

// HasPendingApproval reports whether the task's run holds at least one
// PAUSED pause record with the ApprovalRequired reason, scoped to the
// task's own (tenant, user, session) [+ run]. The read is identity-scoped
// so session A's open gate never bleeds into session B's row. A read error
// or an incomplete triple returns false (honest — the row shows no gate
// rather than a fabricated one; the failure is logged nowhere hotter than
// the coordinator's own diagnostics).
func (c *ApprovalChecker) HasPendingApproval(ctx context.Context, taskIdentity identity.Identity, runID string) bool {
	if err := identity.Validate(taskIdentity); err != nil {
		return false
	}
	filter := pauseresume.ListFilter{
		States:     []pauseresume.State{pauseresume.StatusPaused},
		Reasons:    []pauseresume.Reason{pauseresume.ReasonApprovalRequired},
		UserIDs:    []string{taskIdentity.UserID},
		SessionIDs: []string{taskIdentity.SessionID},
	}
	if runID != "" {
		filter.RunIDs = []string{runID}
	}
	resp, err := c.pauses.List(ctx, pauseresume.ListRequest{
		Identity: taskIdentity,
		Filter:   filter,
		Page:     1,
		PageSize: 1,
	})
	if err != nil {
		return false
	}
	return resp.TotalRows > 0
}
