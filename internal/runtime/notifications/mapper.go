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
// instance are trivially safe — there is nothing to share. D-025
// concurrent-reuse is satisfied by construction.
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
// V1 class's per-class severity (Brief 11 §CC-3 heuristic — see the
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
// failure state — operator-actionable, blocking).
func mapTaskFailed(ev events.Event) ([]events.Event, error) {
	payload, ok := ev.Payload.(tasks.TaskFailedPayload)
	if !ok {
		return nil, wrap(ErrUnmappable, "type=%q payload=%T (want tasks.TaskFailedPayload)",
			ev.Type, ev.Payload)
	}
	summary := fmt.Sprintf("Task %s failed (error_code=%s)", payload.TaskID, payload.ErrorCode)
	deepLink := fmt.Sprintf("/console/tasks/%s", payload.TaskID)
	return synthesise(ev, EventTypeNotificationTaskFailed, SeverityError, summary, deepLink), nil
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
	return []events.Event{{
		Type:     class,
		Identity: trigger.Identity,
		// Sequence and OccurredAt are intentionally zero — the bus's
		// Publish path owns sequencing (rejects pre-filled Sequence)
		// and fills OccurredAt when zero. The mapper stays pure.
		Payload: NotificationPayload{
			Class:               class,
			Severity:            sev,
			Summary:             summary,
			DeepLink:            deepLink,
			OriginEventType:     trigger.Type,
			OriginEventSequence: trigger.Sequence,
		},
	}}
}

// wrap formats a sentinel error with %w plus contextual key=value
// pairs. Mirrors the events package's wrap helper.
func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}
