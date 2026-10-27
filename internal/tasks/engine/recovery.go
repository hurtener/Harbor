package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

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

// PendingAbandonedErrorCode is the TaskError.Code stamped on a foreground
// task that stayed Pending (never started) for longer than
// PendingForegroundAbandonAfter, for example because its task.spawned
// event was never published.
const PendingAbandonedErrorCode = "interrupted_before_start"

// pendingAbandonedMessage is the user-safe message paired with
// PendingAbandonedErrorCode.
const pendingAbandonedMessage = "interrupted before it started; please retry"

const (
	// PendingRedriveAfter is the minimum age before a Pending task is
	// considered stranded. A healthy spawn reaches Running within
	// milliseconds (the run loop calls MarkRunning as soon as it sees
	// task.spawned), so two minutes never touches a task whose spawn is
	// still in flight, including one spawned by a peer instance during a
	// rolling deploy.
	PendingRedriveAfter = 2 * time.Minute
	// PendingForegroundAbandonAfter is the age past which a stranded
	// foreground (interactive) task is failed instead of re-driven. Past
	// fifteen minutes the user is no longer waiting on that chat turn, so
	// running it now would answer a question nobody is watching (and
	// spend model budget on it); a clear terminal failure lets the client
	// show the turn as interrupted and the user retry.
	PendingForegroundAbandonAfter = 15 * time.Minute
)

// RecoverInterruptedTasks is the open-time recovery sweep a durable
// driver runs immediately after [New], before the registry is handed
// to any caller. It transitions every task left at StatusRunning by a
// crash to StatusFailed with [RecoveryErrorCode], emitting one
// task.failed event per recovered task.
//
// Scope. StatusRunning tasks are swept. StatusPaused is a durable wait
// state by design (HITL / tool-side OAuth survive a restart via the
// unified pause/resume primitive) and is left untouched. StatusPending
// foreground tasks older than PendingForegroundAbandonAfter are failed
// with PendingAbandonedErrorCode (see failAbandonedPendingLocked); other
// Pending tasks are left for RedrivePendingTasks, which runs once the run
// loop is subscribed so a re-emitted task.spawned is not missed.
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
	// Stranded Pending foreground tasks past the abandon bound reach a
	// terminal state now (younger ones and background tasks are re-driven
	// by RedrivePendingTasks once the run loop is subscribed).
	n, perrs := e.failAbandonedPendingLocked(ctx)
	recovered += n
	errs = append(errs, perrs...)
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
				// The crash-recovery recompute of a fail-fast group: the
				// fail-fast gate (not the operator) is the cancel's origin,
				// so a conversational mirror surfaces it like any other
				// fail-fast cancel.
				if rerr := e.resolveGroupLocked(ctx, g, tasks.GroupCancelled, reason, tasks.CancelOriginFailFast); rerr != nil {
					errs = append(errs, fmt.Errorf("reconcile group %q resolve cancelled: %w", gid, rerr))
				}
				continue
			}
		}
		// Sealed + all members terminal → Completed.
		if g.Status == tasks.GroupSealed && e.allMembersTerminalLocked(g) {
			if rerr := e.resolveGroupLocked(ctx, g, tasks.GroupCompleted, "", ""); rerr != nil {
				errs = append(errs, fmt.Errorf("reconcile group %q resolve completed: %w", gid, rerr))
			}
		}
	}
	return errors.Join(errs...)
}

// RedrivePendingTasks heals tasks stranded at StatusPending, for example
// because their task.spawned event was never published. Every Pending
// foreground or background task older than PendingRedriveAfter is either
// failed (foreground older than PendingForegroundAbandonAfter) or has its
// task.spawned re-emitted, at most once per PendingRedriveAfter per task.
//
// It is safe to call repeatedly and while tasks are being spawned: a
// re-emitted task.spawned for a task that a run loop already picked up is
// harmless, because MarkRunning admits exactly one Pending → Running
// transition and the turns projection treats a repeated spawn as a no-op.
// The runtime calls it after the run-loop driver subscribes and then on a
// bounded interval. It returns the number of tasks re-emitted or failed.
func (e *Engine) RedrivePendingTasks(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() {
		return 0, tasks.ErrRegistryClosed
	}
	handled, errs := e.failAbandonedPendingLocked(ctx)
	now := e.clock().UnixNano()
	for _, t := range e.stalePendingLocked(PendingRedriveAfter) {
		if last, ok := e.lastRedrive[t.ID]; ok && now-last < int64(PendingRedriveAfter) {
			continue
		}
		if err := e.publishSpawnedLocked(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("redrive task %q: %w", t.ID, err))
			continue
		}
		e.lastRedrive[t.ID] = now
		handled++
	}
	return handled, errors.Join(errs...)
}

// stalePendingLocked returns the Pending foreground/background tasks
// whose last update is at least minAge old, in creation order. Other task
// kinds have their own executors and are left untouched. Caller MUST hold
// e.mu.
func (e *Engine) stalePendingLocked(minAge time.Duration) []*tasks.Task {
	cutoff := e.clock().Add(-minAge).UnixNano()
	var out []*tasks.Task
	for _, t := range e.tasks {
		if t.Status != tasks.StatusPending || t.UpdatedAt > cutoff {
			continue
		}
		if t.Kind != tasks.KindForeground && t.Kind != tasks.KindBackground {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// failAbandonedPendingLocked fails every Pending foreground task older
// than PendingForegroundAbandonAfter with PendingAbandonedErrorCode. The
// FSM has no Pending → Failed edge, so the task passes through Running
// without a task.started event (a crash between the two persists is
// healed by the Running sweep on the next open). It then emits
// task.spawned (so a projection that never saw the turn can place it)
// followed by task.failed. The task is terminal before either event is
// published, so a run loop receiving the task.spawned cannot start it.
// Caller MUST hold e.mu.
func (e *Engine) failAbandonedPendingLocked(ctx context.Context) (int, []error) {
	failed := 0
	var errs []error
	for _, t := range e.stalePendingLocked(PendingForegroundAbandonAfter) {
		if t.Kind != tasks.KindForeground || t.Status != tasks.StatusPending {
			continue
		}
		if err := e.transitionLocked(ctx, t, tasks.StatusRunning); err != nil {
			errs = append(errs, fmt.Errorf("abandon pending task %q: %w", t.ID, err))
			continue
		}
		priorError := t.Error
		t.Error = &tasks.TaskError{Code: PendingAbandonedErrorCode, Message: pendingAbandonedMessage}
		if err := e.transitionLocked(ctx, t, tasks.StatusFailed); err != nil {
			t.Error = priorError
			errs = append(errs, fmt.Errorf("abandon pending task %q: %w", t.ID, err))
			continue
		}
		delete(e.lastRedrive, t.ID)
		failed++
		if err := e.publishSpawnedLocked(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("abandon pending task %q publish spawned: %w", t.ID, err))
			continue
		}
		if err := e.publish(ctx, t, tasks.EventTypeTaskFailed, tasks.TaskFailedPayload{
			TaskID:    t.ID,
			ErrorCode: PendingAbandonedErrorCode,
		}); err != nil {
			errs = append(errs, fmt.Errorf("abandon pending task %q publish failed: %w", t.ID, err))
		}
	}
	return failed, errs
}
