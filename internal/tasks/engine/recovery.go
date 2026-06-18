package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/tasks"
)

// RecoveryErrorCode is the reserved TaskError.Code stamped on a task
// the recovery sweep transitions to Failed because it was left
// Running by a runtime restart. Consumers (the Console, lineage
// tooling) match on it to distinguish a restart-interrupted task from
// a task that failed during normal execution.
const RecoveryErrorCode = "runtime_restarted"

// recoveryErrorMessage is the human-readable message paired with
// RecoveryErrorCode.
const recoveryErrorMessage = "task did not survive a runtime restart"

// RecoverInterruptedTasks is the open-time recovery sweep a durable
// driver runs immediately after [New], before the registry is handed
// to any caller. It transitions every task left at StatusRunning by a
// crash to StatusFailed with [RecoveryErrorCode], emitting one
// task.failed event per recovered task.
//
// Scope. Only StatusRunning tasks are swept. StatusPaused is a durable
// wait state by design (HITL / tool-side OAuth survive a restart via
// the unified pause/resume primitive) and is left untouched.
// StatusPending tasks never began executing and remain discoverable
// for a future re-drive (auto-re-drive of recovered work is a
// runloop/steering concern and is out of scope here — the record is
// recovered, execution is not).
//
// Idempotency. The sweep persists each recovered task as Failed before
// returning, so a second open of the same store observes it as Failed
// (no longer Running) and does not re-sweep or re-emit — exactly-once
// in the normal case, and never a double-write/double-emit. A bus
// failure mid-sweep leaves the task correctly persisted as Failed but
// drops that one recovery event (at-most-once), surfaced in the
// returned error rather than silently swallowed.
//
// Group interaction. Recovery reuses the normal terminal-transition
// path, so a recovered (failed) group member drives the group resolve
// gate: a FailFast group cancels its remaining members and resolves,
// and a sealed group whose last member just recovered resolves to
// Completed. The returned count covers task recoveries; cascade
// cancellations triggered by fail-fast are reflected in the persisted
// group/member records.
func (e *Engine) RecoverInterruptedTasks(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() {
		return 0, tasks.ErrRegistryClosed
	}

	// Snapshot the Running IDs first. transitionLocked can resolve a
	// group fail-fast and cancel sibling members, mutating the status
	// of OTHER tasks mid-loop; iterating a fixed snapshot (and
	// re-checking each task is still Running) keeps the sweep stable
	// and idempotent.
	var running []tasks.TaskID
	for id, t := range e.tasks {
		if t.Status == tasks.StatusRunning {
			running = append(running, id)
		}
	}

	recovered := 0
	var errs []error
	for _, id := range running {
		t, ok := e.tasks[id]
		if !ok || t.Status != tasks.StatusRunning {
			// Already transitioned by a fail-fast cascade earlier in
			// this sweep.
			continue
		}
		priorError := t.Error
		t.Error = &tasks.TaskError{Code: RecoveryErrorCode, Message: recoveryErrorMessage}
		if err := e.transitionLocked(ctx, t, tasks.StatusFailed); err != nil {
			// transitionLocked rolled status back to Running on a persist
			// failure; restore the error too so the record stays
			// consistent and is re-swept cleanly on the next open.
			t.Error = priorError
			errs = append(errs, fmt.Errorf("recover task %q: %w", id, err))
			continue
		}
		if err := e.publish(ctx, t, tasks.EventTypeTaskFailed, tasks.TaskFailedPayload{
			TaskID:    t.ID,
			ErrorCode: RecoveryErrorCode,
		}); err != nil {
			errs = append(errs, fmt.Errorf("recover publish task %q: %w", id, err))
			continue
		}
		recovered++
	}

	// Reconcile group resolution from member terminality. This heals a
	// group whose resolution was computed in a PRIOR session but whose
	// group-record persist failed before the crash (members durably
	// terminal, group durably non-terminal) — recovery sweeps only
	// Running TASKS, so without this a diverged Sealed group would never
	// re-resolve and a post-restart WatchGroup would never fire. The
	// plan's Risks section requires this ("the sweep recomputes group
	// resolution from member terminality").
	if rerr := e.reconcileGroupsLocked(ctx); rerr != nil {
		errs = append(errs, rerr)
	}

	if len(errs) > 0 {
		return recovered, errors.Join(errs...)
	}
	return recovered, nil
}

// reconcileGroupsLocked re-evaluates every non-terminal group's resolve
// gate against current member terminality, mirroring
// onMemberTerminalLocked but driven per-group rather than per-member
// event. resolveGroupLocked is idempotent on an already-terminal group
// (the gate skips it), so a group the Running sweep already resolved is
// not touched again. Caller MUST hold e.mu.
func (e *Engine) reconcileGroupsLocked(ctx context.Context) error {
	ids := make([]tasks.TaskGroupID, 0, len(e.groups))
	for gid := range e.groups {
		ids = append(ids, gid)
	}
	var errs []error
	for _, gid := range ids {
		g, ok := e.groups[gid]
		if !ok || isGroupTerminal(g.Status) {
			continue
		}
		// FailFast: a durably-failed member whose cancellation/resolve
		// did not persist before the crash. Cancel remaining members and
		// resolve Cancelled (the deterministic recompute).
		if g.FailFast {
			var failed *tasks.Task
			for _, mid := range g.Members {
				if m := e.tasks[mid]; m != nil && m.Status == tasks.StatusFailed {
					failed = m
					break
				}
			}
			if failed != nil {
				reason := "fail-fast"
				if failed.Error != nil && failed.Error.Code != "" {
					reason = "fail-fast:" + failed.Error.Code
				}
				for _, mid := range g.Members {
					m := e.tasks[mid]
					if m == nil || isTerminal(m.Status) {
						continue
					}
					if cerr := e.cancelTaskLocked(ctx, m, reason, true); cerr != nil {
						errs = append(errs, fmt.Errorf("reconcile group %q cancel member %q: %w", gid, mid, cerr))
					}
				}
				if rerr := e.resolveGroupLocked(ctx, g, tasks.GroupCancelled, reason); rerr != nil {
					errs = append(errs, fmt.Errorf("reconcile group %q resolve cancelled: %w", gid, rerr))
				}
				continue
			}
		}
		// Sealed + all members terminal → Completed.
		if g.Status == tasks.GroupSealed && e.allMembersTerminalLocked(g) {
			if rerr := e.resolveGroupLocked(ctx, g, tasks.GroupCompleted, ""); rerr != nil {
				errs = append(errs, fmt.Errorf("reconcile group %q resolve completed: %w", gid, rerr))
			}
		}
	}
	return errors.Join(errs...)
}
