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
// `task.prioritised` for caller-driven priority updates.
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
	ParentTaskID   TaskID // empty when no parent
	Priority       int
	IdempotencyKey string
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
