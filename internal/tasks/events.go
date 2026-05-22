package tasks

import (
	"github.com/hurtener/Harbor/internal/events"
)

// Task lifecycle event types. Each is registered with the events
// package's exhaustive registry via init() so Publish accepts them
// without ErrUnknownEventType. Registration follows the Phase 08
// sessions precedent — the consumer subsystem registers its own
// events alongside its typed payloads, keeping the events package
// free of task-domain knowledge.
//
// Phase 20 ships eight types: seven lifecycle transitions plus
// `task.prioritised` for caller-driven priority updates. Phase 21
// adds seven more group/patch types — `task.group_created`,
// `task.group_sealed`, `task.group_resolved`, `task.group_cancelled`,
// `task.patch_applied`, `task.patch_rejected`,
// `task.background_acknowledged`. The `task.group_resolved` payload
// IS `GroupCompletion` so subscribers (planner runtime, Console,
// durable event log, sidecar status emitters) consume one canonical
// shape regardless of how they're wired.
const (
	// EventTypeTaskSpawned — emitted by Spawn / SpawnTool when a
	// fresh task is persisted (NOT emitted on idempotency-key reuse).
	EventTypeTaskSpawned events.EventType = "task.spawned"
	// EventTypeTaskStarted — emitted by MarkRunning when a task
	// transitions from Pending or Paused → Running.
	EventTypeTaskStarted events.EventType = "task.started"
	// EventTypeTaskPaused — emitted by MarkPaused (Running → Paused).
	EventTypeTaskPaused events.EventType = "task.paused"
	// EventTypeTaskResumed — emitted by MarkResumed (Paused → Running).
	// Distinct from task.started so subscribers can tell pause-resume
	// from initial start.
	EventTypeTaskResumed events.EventType = "task.resumed"
	// EventTypeTaskCompleted — emitted by MarkComplete (Running →
	// Complete; terminal).
	EventTypeTaskCompleted events.EventType = "task.completed"
	// EventTypeTaskFailed — emitted by MarkFailed (Running → Failed;
	// terminal).
	EventTypeTaskFailed events.EventType = "task.failed"
	// EventTypeTaskCancelled — emitted by Cancel for the target task
	// AND each cascaded descendant.
	EventTypeTaskCancelled events.EventType = "task.cancelled"
	// EventTypeTaskPrioritised — emitted by Prioritize when the
	// task's priority value changes.
	EventTypeTaskPrioritised events.EventType = "task.prioritised"

	// EventTypeTaskGroupCreated — emitted by ResolveOrCreateGroup on
	// the first creation (NOT emitted on idempotent-return).
	EventTypeTaskGroupCreated events.EventType = "task.group_created"
	// EventTypeTaskGroupSealed — emitted by SealGroup /
	// ApplyGroup(ActionSeal) when the group transitions Open → Sealed.
	EventTypeTaskGroupSealed events.EventType = "task.group_sealed"
	// EventTypeTaskGroupResolved — emitted when a sealed group's last
	// non-terminal member transitions to terminal AND the group's
	// final status is `GroupCompleted`. Payload is `GroupCompletion`
	// — the SAME canonical shape `WatchGroup` delivers.
	EventTypeTaskGroupResolved events.EventType = "task.group_resolved"
	// EventTypeTaskGroupCancelled — emitted by CancelGroup or by the
	// driver's FailFast cascade when the group transitions to
	// `GroupCancelled`. Payload is `GroupCompletion` with
	// `FinalStatus = GroupCancelled` and `Reason` populated.
	EventTypeTaskGroupCancelled events.EventType = "task.group_cancelled"
	// EventTypeTaskPatchApplied — emitted by
	// ApplyPatch(action=PatchAccept) on the pending → applied
	// transition.
	EventTypeTaskPatchApplied events.EventType = "task.patch_applied"
	// EventTypeTaskPatchRejected — emitted by
	// ApplyPatch(action=PatchReject) on the pending → rejected
	// transition.
	EventTypeTaskPatchRejected events.EventType = "task.patch_rejected"
	// EventTypeTaskBackgroundAcknowledged — emitted once per task by
	// AcknowledgeBackground on the un-ack → ack transition.
	EventTypeTaskBackgroundAcknowledged events.EventType = "task.background_acknowledged"
)

func init() {
	events.RegisterEventType(EventTypeTaskSpawned)
	events.RegisterEventType(EventTypeTaskStarted)
	events.RegisterEventType(EventTypeTaskPaused)
	events.RegisterEventType(EventTypeTaskResumed)
	events.RegisterEventType(EventTypeTaskCompleted)
	events.RegisterEventType(EventTypeTaskFailed)
	events.RegisterEventType(EventTypeTaskCancelled)
	events.RegisterEventType(EventTypeTaskPrioritised)

	events.RegisterEventType(EventTypeTaskGroupCreated)
	events.RegisterEventType(EventTypeTaskGroupSealed)
	events.RegisterEventType(EventTypeTaskGroupResolved)
	events.RegisterEventType(EventTypeTaskGroupCancelled)
	events.RegisterEventType(EventTypeTaskPatchApplied)
	events.RegisterEventType(EventTypeTaskPatchRejected)
	events.RegisterEventType(EventTypeTaskBackgroundAcknowledged)
}

// TaskSpawnedPayload reports a successful Spawn / SpawnTool. Carries
// the assigned TaskID, the kind, and the parent (when any). Identity
// lives on the Event itself, intentionally not duplicated here.
//
// SafePayload by construction — every field is internal bookkeeping
// (TaskID is a registry-assigned ULID; Kind is an enum; ParentID is
// caller-controlled but bounded; IdempotencyKey is caller-controlled
// and may carry caller-meaningful tokens, but task identifiers are
// not secret-shaped — same threat model as session.opened's
// SessionID field).
type TaskSpawnedPayload struct {
	events.SafeSealed
	TaskID         TaskID
	Kind           TaskKind
	ParentTaskID   TaskID
	IdempotencyKey string
	Priority       int
}

// TaskStartedPayload reports MarkRunning. Carries the prior status
// (Pending or Paused) and the TaskID. SafePayload by construction.
type TaskStartedPayload struct {
	events.SafeSealed
	TaskID     TaskID
	PriorState TaskStatus
}

// TaskPausedPayload reports MarkPaused. SafePayload by construction.
type TaskPausedPayload struct {
	events.SafeSealed
	TaskID TaskID
}

// TaskResumedPayload reports MarkResumed. SafePayload by construction.
type TaskResumedPayload struct {
	events.SafeSealed
	TaskID TaskID
}

// TaskCompletedPayload reports MarkComplete. The result is on the
// Task record itself; this payload only signals the transition so
// subscribers do not see an unredacted result by accident. The
// caller pre-redacts `TaskResult.Value` before MarkComplete (D-020).
//
// SafePayload by construction.
type TaskCompletedPayload struct {
	events.SafeSealed
	TaskID TaskID
}

// TaskFailedPayload reports MarkFailed. Carries the error code; the
// caller-controlled message is on the Task record (already redacted
// by the caller). SafePayload by construction.
type TaskFailedPayload struct {
	events.SafeSealed
	TaskID    TaskID
	ErrorCode string
}

// TaskCancelledPayload reports a Cancel transition. `Cascaded` is
// true when this payload landed because of a parent's cascade-cancel
// (so subscribers can distinguish operator cancel from cascade).
// `Reason` is the operator-supplied reason (a short caller-controlled
// string; callers MUST NOT pass tool args, raw user input, or any
// secret-shaped material — the same SafePayload contract sessions
// uses for ClosedReason).
//
// SafePayload by construction.
type TaskCancelledPayload struct {
	events.SafeSealed
	TaskID   TaskID
	Reason   string
	Cascaded bool
}

// TaskPrioritisedPayload reports Prioritize. Carries the new and
// prior priority values. SafePayload by construction.
type TaskPrioritisedPayload struct {
	events.SafeSealed
	TaskID        TaskID
	PriorPriority int
	NewPriority   int
}

// TaskGroupCreatedPayload reports a successful
// ResolveOrCreateGroup on the first-creation path. Carries the
// assigned group ID, owner task, retain-turn / fail-fast flags, and
// the description. SafePayload by construction — the description is
// a caller-controlled short string with the same SafePayload contract
// as session.opened's `ClosedReason`.
type TaskGroupCreatedPayload struct {
	events.SafeSealed
	GroupID     TaskGroupID
	OwnerTaskID TaskID
	Description string
	RetainTurn  bool
	FailFast    bool
}

// TaskGroupSealedPayload reports a SealGroup transition.
// SafePayload by construction.
type TaskGroupSealedPayload struct {
	events.SafeSealed
	GroupID  TaskGroupID
	Members  []TaskID
	SealedAt int64 // unix nanoseconds
}

// TaskGroupResolvedPayload reports a sealed group's terminal
// transition to `GroupCompleted`. The payload doubles as the
// `WatchGroup` wake-up payload — same canonical `GroupCompletion`
// shape so subscribers (Console, planner, sidecar status emitters)
// consume one shape regardless of how they're wired.
//
// SafePayload by construction. `MemberOutcome.Result` is ref-shaped
// (D-022, D-026); heavy bytes should already be ArtifactRefs
// upstream.
type TaskGroupResolvedPayload struct {
	events.SafeSealed
	Completion GroupCompletion
}

// TaskGroupCancelledPayload reports a CancelGroup transition (or a
// FailFast cascade-cancel). Same `GroupCompletion` shape as
// resolved, with `FinalStatus = GroupCancelled` and `Reason`
// populated.
//
// SafePayload by construction.
type TaskGroupCancelledPayload struct {
	events.SafeSealed
	Completion GroupCompletion
}

// TaskPatchAppliedPayload reports ApplyPatch(action=PatchAccept).
// Carries the patch ID; the patch payload bytes are on the Patch
// record (already through the audit redactor; we do NOT inline the
// bytes here). SafePayload by construction.
type TaskPatchAppliedPayload struct {
	events.SafeSealed
	PatchID string
}

// TaskPatchRejectedPayload reports ApplyPatch(action=PatchReject).
// SafePayload by construction.
type TaskPatchRejectedPayload struct {
	events.SafeSealed
	PatchID string
}

// TaskBackgroundAcknowledgedPayload reports AcknowledgeBackground
// for a single task (one event per task). SafePayload by
// construction.
type TaskBackgroundAcknowledgedPayload struct {
	events.SafeSealed
	TaskID TaskID
}
