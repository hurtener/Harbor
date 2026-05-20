package notifications

import "github.com/hurtener/Harbor/internal/events"

// Severity classifies a notification's operator-visible urgency. Per
// Brief 11 §CC-3 the Console renders notifications in alert ribbons /
// notification centres with a severity-derived colour and ordering.
//
// V1 is a fixed three-value enum. A richer model (payload-derived
// severity, dynamic thresholds) is post-V1.
type Severity string

const (
	// SeverityInfo — operator-visible but non-actionable. Example:
	// notification.pause_requested (a planner asked for human input;
	// the runtime is healthy).
	SeverityInfo Severity = "info"

	// SeverityWarning — operator-actionable but not blocking. Example:
	// notification.tool_approval_requested (a tool call is parked on
	// an approval gate); notification.auth_required (a tool needs
	// OAuth binding).
	SeverityWarning Severity = "warning"

	// SeverityError — operator-actionable and blocking. Example:
	// notification.task_failed (a task entered a terminal failure
	// state); notification.governance_budget_exceeded (a budget cap
	// triggered).
	SeverityError Severity = "error"
)

// NotificationPayload is the typed payload for every notification.*
// event. Embeds events.Sealed (NOT events.SafeSealed): the Summary
// field is human-readable and derived from caller-controlled bytes on
// the originating event's typed payload, so the bus's redactor walks
// the payload on Publish per D-020.
//
// Field semantics:
//
//   - Class is the notification.* class — one of the V1 constants.
//     Echoed here so subscribers parsing only the payload (e.g. when
//     reading from a durable event log without the bus envelope) still
//     have the class.
//   - Severity is the operator-visible urgency.
//   - Summary is a one-liner the Console renders in its notification
//     centre. Derived from the originating event's typed payload (e.g.
//     `task.failed` → "Task <id> failed with code <code>"). The
//     mapper builds the summary; the audit redactor walks it.
//   - DeepLink is a Protocol-relative path the Console's router
//     deep-links into. V1 hard-codes the shape per class; if the
//     Console's route shape changes post-V1 the mapper updates without
//     a Protocol break (the runtime stays the source of truth).
//   - OriginEventType is the bus event-type the mapper consumed. Lets
//     subscribers join the notification with its source event without
//     additional bus traffic.
//   - OriginEventSequence is the bus Sequence of the originating
//     event. Stable correlation key across the runtime's lifetime.
type NotificationPayload struct {
	events.Sealed

	// Class is the notification.* class this payload belongs to. One
	// of EventTypeNotificationTaskFailed / ToolApprovalRequested /
	// GovernanceBudgetExceeded / AuthRequired / PauseRequested.
	Class events.EventType

	// Severity is the operator-visible urgency.
	Severity Severity

	// Summary is a human-readable one-liner the Console renders.
	// Caller-controlled (the mapper derives it from the originating
	// event's typed payload); the audit redactor walks it on Publish.
	Summary string

	// DeepLink is a Protocol-relative path (e.g. "/console/tasks/<id>").
	// The Console renders this via its router.
	DeepLink string

	// OriginEventType is the originating event's Type
	// (e.g. "task.failed").
	OriginEventType events.EventType

	// OriginEventSequence is the originating event's bus Sequence
	// (correlation key).
	OriginEventSequence uint64
}

// IdentityRejectedPayload reports a Subscriber-side identity rejection:
// the trigger event arrived with the D-033 `<missing>` sentinel
// substituted into one or more identity components, so the Subscriber
// emits this rejection event instead of silently dropping the input or
// synthesising a malformed notification (CLAUDE.md §13 fail-loudly +
// mirrors the `memory.identity_rejected` shape).
//
// SafePayload by construction — `Operation` is a bounded constant
// ("Subscriber.Run"), `Reason` is a short static string naming the
// missing component(s), `OriginEventType` is an EventType enum.
type IdentityRejectedPayload struct {
	events.SafeSealed

	// Operation is the rejected operation name (always
	// "Subscriber.Run" for V1; future operations may extend the
	// vocabulary).
	Operation string

	// Reason names the missing identity components ("tenant_id empty",
	// "user_id and session_id empty", etc.). Deterministic ordering.
	Reason string

	// OriginEventType is the bus event-type the Subscriber was trying
	// to map when the identity check failed.
	OriginEventType events.EventType
}
