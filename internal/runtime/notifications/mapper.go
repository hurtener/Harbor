package notifications

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// Map translates a triggering bus event into zero or more synthesised
// notification.* events.
//
// Map is a PURE function. No I/O. No global state. No time.Now()
// dependency (the bus's Publish path fills OccurredAt on the
// synthesised event). Concurrent calls against a single shared mapper
// instance are trivially safe — there is nothing to share. The concurrent-reuse
// contract is satisfied by construction.
//
// Return contract:
//
//   - (nil, nil) — the event's Type is not in V1TriggerEventTypes.
//     The vast majority of bus traffic hits this branch; the
//     Subscriber's bus filter already narrows the input set so the
//     happy-unmapped case is rare in practice, but the contract is
//     defined here so the function is safe to call against any event.
//
//   - ([]events.Event with one element, nil) — the event was a known
//     trigger type with a well-formed typed payload; the slice carries
//     the synthesised notification.* event.
//
//   - (nil, wrapped ErrUnmappable) — the event was a known trigger
//     type BUT the typed payload assertion failed (wrong payload type
//     for the declared event type, or a RedactedMap arrived where the
//     bus normally delivers the typed shape). Fail-loudly per CLAUDE.md
//     §13 — never silently degrade to "no notifications."
//
// The synthesised event carries the trigger's identity.Quadruple, the
// V1 class's per-class severity (a settled heuristic — see the
// per-case comments below for rationale), and a per-class deep-link
// shape. The bus's Publish path fills Sequence and OccurredAt.
//
// Note: trigger event payloads in V1 are all SafePayload by
// construction (TaskFailedPayload, ToolApprovalRequestedPayload,
// BudgetExceededPayload, ToolAuthRequiredPayload, PauseRequestedPayload
// all embed events.SafeSealed). The bus therefore delivers them with
// their typed shape preserved. A RedactedMap on a known trigger type
// is a contract violation upstream — Map fails loudly via
// ErrUnmappable so the violation does not silently downgrade.
func Map(_ context.Context, ev events.Event) ([]events.Event, error) {
	switch ev.Type {
	case tasks.EventTypeTaskFailed:
		return mapTaskFailed(ev)
	case approval.EventTypeToolApprovalRequested:
		return mapToolApprovalRequested(ev)
	case governance.EventTypeBudgetExceeded:
		return mapGovernanceBudgetExceeded(ev)
	case auth.EventTypeToolAuthRequired:
		return mapToolAuthRequired(ev)
	case pauseresume.EventTypePauseRequested:
		return mapPauseRequested(ev)
	case tasks.EventTypeTaskGroupResolved:
		return mapTaskGroupResolved(ev)
	case tasks.EventTypeTaskGroupCancelled:
		return mapTaskGroupCancelled(ev)
	case tasks.EventTypeTaskCompleted:
		return mapTaskCompleted(ev)
	default:
		// Unmapped — the overwhelming majority of bus traffic. The
		// Subscriber's bus filter already narrows the input set so this
		// branch is rare in practice, but Map stays safe to call
		// against any event.
		return nil, nil
	}
}

// mapTaskFailed synthesises a notification.task_failed event from a
// task.failed bus event. Severity Error (a task entered a terminal
// failure state — operator-actionable, blocking). The failing TaskID
// is carried on the payload so a conversational consumer can key the
// foreground-turn dedup (a foreground turn's failure is surfaced by the
// dedicated turn-failure line, not the background mirror).
func mapTaskFailed(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(tasks.TaskFailedPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want tasks.TaskFailedPayload)",
			ev.Type, ev.Payload)
	}
	summary := fmt.Sprintf("Task %s failed (error_code=%s)", payload.TaskID, payload.ErrorCode)
	deepLink := fmt.Sprintf("/console/tasks/%s", payload.TaskID)
	out := synthesisePayload(ev, NotificationPayload{
		Class:    EventTypeNotificationTaskFailed,
		Severity: SeverityError,
		Summary:  summary,
		DeepLink: deepLink,
		TaskID:   string(payload.TaskID),
	})
	return out, nil
}

// mapTaskGroupResolved synthesises a notification.task_group_resolved
// event from a task.group_resolved bus event — the conversational
// mirror of the typed GroupCompletion wake the planner consumes over
// WatchGroup. Severity Info when every member succeeded, Warning when
// any member failed or was cancelled. The per-member summaries are
// ref-shaped (TaskID/Status/Description only — never Result/Error
// bytes) and bounded at MaxMemberSummaries; when the group has more
// members than the cap, MembersTruncated is set (never a silent drop).
func mapTaskGroupResolved(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(tasks.TaskGroupResolvedPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want tasks.TaskGroupResolvedPayload)",
			ev.Type, ev.Payload)
	}
	c := payload.Completion
	summaries := make([]MemberOutcomeSummary, 0, min(len(c.Members), MaxMemberSummaries))
	var succeeded, failed, cancelled int
	for _, m := range c.Members {
		switch m.Status {
		case tasks.StatusComplete:
			succeeded++
		case tasks.StatusFailed:
			failed++
		default:
			// Cancelled (or any non-terminal snapshot on the cancel path)
			// counts as cancelled for the operator-visible rollup.
			cancelled++
		}
		if len(summaries) < MaxMemberSummaries {
			summaries = append(summaries, MemberOutcomeSummary{
				TaskID:      string(m.TaskID),
				Status:      string(m.Status),
				Description: m.Description,
			})
		}
	}
	sev := SeverityInfo
	if failed > 0 || cancelled > 0 {
		sev = SeverityWarning
	}
	summary := fmt.Sprintf("Background group resolved: %d of %d succeeded",
		succeeded, len(c.Members))
	if failed > 0 {
		summary += fmt.Sprintf(", %d failed", failed)
	}
	if cancelled > 0 {
		summary += fmt.Sprintf(", %d cancelled", cancelled)
	}
	deepLink := fmt.Sprintf("/console/tasks/groups/%s", c.GroupID)
	return synthesisePayload(ev, NotificationPayload{
		Class:            EventTypeNotificationTaskGroupResolved,
		Severity:         sev,
		Summary:          summary,
		DeepLink:         deepLink,
		GroupID:          string(c.GroupID),
		Members:          summaries,
		MembersTruncated: len(c.Members) > MaxMemberSummaries,
		MemberSucceeded:  succeeded,
		MemberFailed:     failed,
		MemberCancelled:  cancelled,
	}), nil
}

// mapTaskGroupCancelled synthesises a notification.task_group_cancelled
// event from a task.group_cancelled bus event — but SELECTIVELY, keyed
// on the cancel's typed origin:
//
//   - CancelOriginOperator → (nil, nil). The operator (or an agent
//     acting on their behalf) drove the cancel directly and already
//     knows; a conversational mirror would be noise. This is the settled
//     suppression rule — analogous in intent to how task_failed suppresses
//     a foreground turn's own failure (suppress what the operator already
//     knows), but enforced earlier and more decisively: here at the mapper
//     on a typed origin (so NO notification.* event is synthesised at all,
//     Console included), rather than in the TUI reducer on projection state.
//   - CancelOriginCascade / CancelOriginFailFast → synthesised. An
//     unprompted, runtime-initiated cancel is narrative the operator did
//     not ask for — exactly what a wake mirror exists to surface.
//   - any other value, including the empty string (an older or
//     hand-built event that carried no origin) → synthesised. An
//     unclassified cancel FAILS LOUD by being surfaced, never silently
//     swallowed (CLAUDE.md §13): the failure mode is an extra
//     notification, never a hidden one.
//
// The per-member summaries reuse the resolved-group shape: ref-shaped
// (TaskID/Status/Description only — never Result/Error bytes), bounded
// at MaxMemberSummaries with MembersTruncated set on overflow, and the
// counts reflect the full membership. Severity is Warning — a cancelled
// group is operator-relevant (work stopped short of completion).
func mapTaskGroupCancelled(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(tasks.TaskGroupCancelledPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want tasks.TaskGroupCancelledPayload)",
			ev.Type, ev.Payload)
	}
	// Suppression rule: an operator-driven cancel is not news.
	if payload.Origin == tasks.CancelOriginOperator {
		return nil, nil
	}

	c := payload.Completion
	summaries := make([]MemberOutcomeSummary, 0, min(len(c.Members), MaxMemberSummaries))
	var succeeded, failed, cancelled int
	for _, m := range c.Members {
		switch m.Status {
		case tasks.StatusComplete:
			succeeded++
		case tasks.StatusFailed:
			failed++
		default:
			// Cancelled (or any non-terminal snapshot on the cancel path)
			// counts as cancelled for the operator-visible rollup.
			cancelled++
		}
		if len(summaries) < MaxMemberSummaries {
			summaries = append(summaries, MemberOutcomeSummary{
				TaskID:      string(m.TaskID),
				Status:      string(m.Status),
				Description: m.Description,
			})
		}
	}
	summary := fmt.Sprintf("Background group cancelled (%s): %d cancelled of %d",
		cancelReasonLabel(payload.Origin), cancelled, len(c.Members))
	if succeeded > 0 {
		summary += fmt.Sprintf(", %d already succeeded", succeeded)
	}
	if failed > 0 {
		summary += fmt.Sprintf(", %d failed", failed)
	}
	deepLink := fmt.Sprintf("/console/tasks/groups/%s", c.GroupID)
	return synthesisePayload(ev, NotificationPayload{
		Class:            EventTypeNotificationTaskGroupCancelled,
		Severity:         SeverityWarning,
		Summary:          summary,
		DeepLink:         deepLink,
		GroupID:          string(c.GroupID),
		Members:          summaries,
		MembersTruncated: len(c.Members) > MaxMemberSummaries,
		MemberSucceeded:  succeeded,
		MemberFailed:     failed,
		MemberCancelled:  cancelled,
	}), nil
}

// cancelReasonLabel renders a short, human-readable label for an
// unprompted cancel origin, used in the rollup summary. An unknown or
// empty origin (a surfaced-not-swallowed unclassified cancel) reads as
// "unspecified" rather than an empty parenthetical.
func cancelReasonLabel(origin tasks.CancelOrigin) string {
	switch origin {
	case tasks.CancelOriginCascade:
		return "cascade"
	case tasks.CancelOriginFailFast:
		return "fail-fast"
	default:
		return "unspecified"
	}
}

// mapTaskCompleted synthesises a notification.task_completed event from
// a task.completed bus event — but ONLY when the completing task
// carried the NotifyOnComplete opt-in. A completion without the opt-in
// returns (nil, nil): it is a known trigger type with a well-formed
// payload that deliberately produces no notification (the vast majority
// of completions — foreground turns and un-opted background tasks). This
// closes the silent-background-success gap without flooding the surface
// with a line per completed task. Severity Info.
func mapTaskCompleted(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(tasks.TaskCompletedPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want tasks.TaskCompletedPayload)",
			ev.Type, ev.Payload)
	}
	if !payload.NotifyOnComplete {
		return nil, nil
	}
	summary := fmt.Sprintf("Background task %s completed", payload.TaskID)
	deepLink := fmt.Sprintf("/console/tasks/%s", payload.TaskID)
	return synthesisePayload(ev, NotificationPayload{
		Class:    EventTypeNotificationTaskCompleted,
		Severity: SeverityInfo,
		Summary:  summary,
		DeepLink: deepLink,
		TaskID:   string(payload.TaskID),
	}), nil
}

// mapToolApprovalRequested synthesises a notification.tool_approval_requested
// event from a tool.approval_requested bus event. Severity Warning
// (a tool call is parked on an approval gate — operator-actionable,
// non-blocking).
func mapToolApprovalRequested(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want approval.ToolApprovalRequestedPayload)",
			ev.Type, ev.Payload)
	}
	summary := fmt.Sprintf("Tool %q awaiting approval (reason=%s)", payload.Tool, payload.Reason)
	deepLink := fmt.Sprintf("/console/tools/%s/approvals/%s", payload.Tool, payload.PauseToken)
	return synthesise(ev, EventTypeNotificationToolApprovalRequested, SeverityWarning, summary, deepLink), nil
}

// mapGovernanceBudgetExceeded synthesises a
// notification.governance_budget_exceeded event from a
// governance.budget_exceeded bus event. Severity Error (a budget cap
// triggered — operator-actionable, blocking).
func mapGovernanceBudgetExceeded(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(governance.BudgetExceededPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want governance.BudgetExceededPayload)",
			ev.Type, ev.Payload)
	}
	summary := fmt.Sprintf("Budget exceeded for tier %s on model %s (total=%.4f ceiling=%.4f %s)",
		payload.Tier, payload.Model, payload.TotalCost, payload.Ceiling, payload.Currency)
	deepLink := fmt.Sprintf("/console/settings/governance/tiers/%s", payload.Tier)
	return synthesise(ev, EventTypeNotificationGovernanceBudgetExceeded, SeverityError, summary, deepLink), nil
}

// mapToolAuthRequired synthesises a notification.auth_required event
// from a tool.auth_required bus event. Severity Warning (a tool needs
// OAuth binding — operator-actionable, non-blocking).
func mapToolAuthRequired(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(auth.ToolAuthRequiredPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want auth.ToolAuthRequiredPayload)",
			ev.Type, ev.Payload)
	}
	summary := fmt.Sprintf("Connect %s to authorise tool access (binding=%s)",
		payload.SourceName, payload.BindingScope)
	deepLink := fmt.Sprintf("/console/mcp-connections/%s/oauth?state=%s",
		payload.Source, payload.State)
	return synthesise(ev, EventTypeNotificationAuthRequired, SeverityWarning, summary, deepLink), nil
}

// mapPauseRequested synthesises a notification.pause_requested event
// from a pause.requested bus event. Severity Info (a planner asked
// for human input — operator-visible, non-blocking, the runtime is
// healthy).
func mapPauseRequested(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(pauseresume.PauseRequestedPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want pauseresume.PauseRequestedPayload)",
			ev.Type, ev.Payload)
	}
	summary := fmt.Sprintf("Run paused awaiting intervention (reason=%s)", payload.Reason)
	deepLink := fmt.Sprintf("/console/interventions/%s", payload.Token)
	return synthesise(ev, EventTypeNotificationPauseRequested, SeverityInfo, summary, deepLink), nil
}

// synthesise builds the notification event from the trigger event +
// per-class derived fields. Identity is preserved from the trigger;
// Sequence and OccurredAt are left zero so the bus's Publish path
// assigns them (matching the bus convention).
func synthesise(trigger events.Event, class events.EventType, sev Severity, summary, deepLink string) []events.Event {
	return synthesisePayload(trigger, NotificationPayload{
		Class:    class,
		Severity: sev,
		Summary:  summary,
		DeepLink: deepLink,
	})
}

// synthesisePayload wraps a per-class NotificationPayload (Class /
// Severity / Summary / DeepLink and any additive fields already set)
// into a bus event carrying the trigger's identity. It fills the
// origin-correlation fields from the trigger; Sequence and OccurredAt
// stay zero so the bus's Publish path assigns them (the mapper stays
// pure). Classes that populate the additive TaskID / GroupID / Members
// fields go through here directly.
func synthesisePayload(trigger events.Event, payload NotificationPayload) []events.Event {
	payload.OriginEventType = trigger.Type
	payload.OriginEventSequence = trigger.Sequence
	return []events.Event{{
		Type:     payload.Class,
		Identity: trigger.Identity,
		Payload:  payload,
	}}
}

// wrap formats a sentinel error with %w plus contextual key=value
// pairs. Mirrors the events package's wrap helper.
func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}
