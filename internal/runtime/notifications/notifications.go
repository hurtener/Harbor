// Package notifications ships Harbor's `notification.*` event family —
// a NEW event topic on the typed event bus carrying operator-facing
// notifications (alerts surfaced by the Console's notification centre,
// the Overview page alert ribbon, and the Settings notification-routing
// matrix per Brief 11 §CC-3).
//
// The family uses **per-class topic naming**:
//
//   - notification.task_failed                (severity Error)
//   - notification.tool_approval_requested    (severity Warning)
//   - notification.governance_budget_exceeded (severity Error)
//   - notification.auth_required              (severity Warning)
//   - notification.pause_requested            (severity Info)
//
// Per-class topics compose naturally with the rest of Harbor's event
// taxonomy (`tool.failed`, `task.completed`, `governance.budget_exceeded`
// all use per-class topics) and with the `events.subscribe` topic-filter
// shape (Phase 72a). The alternative — a single `notification.emit` with
// a payload class field — was rejected per `docs/plans/wave-13-decomposition.md`
// §12 ("Wire-shape decision left to me").
//
// Architecture:
//
//   - Map(ctx, triggering ev) → []events.Event is a pure function. No
//     I/O. No global state. No time.Now() dependency (OccurredAt is
//     filled by the bus's Publish path, matching the existing bus
//     convention). Concurrent calls against a single shared mapper are
//     safe by construction (no state to share). D-025 is trivially
//     satisfied; the test still runs N=100 concurrent invocations under
//     -race to validate end-to-end.
//
//   - Subscriber is a long-lived bus consumer that wires Map onto Publish.
//     NewSubscriber(bus, log) constructs the wiring; (*Subscriber).Run(ctx)
//     opens an Admin-scope subscription on the V1 trigger event types,
//     runs Map on every delivered event, and republishes each synthesised
//     notification.* event onto the same bus. The originating event's
//     identity.Quadruple is preserved on the synthesised event so
//     identity-scope filtering on the downstream subscriber works without
//     elevation.
//
//   - The NotificationPayload embeds events.Sealed (NOT SafeSealed). The
//     Summary field is caller-controlled (a human-readable one-liner
//     derived from the originating event's typed payload), so the bus's
//     redactor walks the payload on Publish (per D-020 audit-redactor-
//     as-bus-boundary). The synthesised event still rides the same
//     bus.Publish path every other event ships through.
//
// What is OUT of scope for Phase 72d:
//
//   - Notification routing fan-out (email / Slack / web-push). Per-user
//     routing lives in 73m Settings + the Console DB's notifications_routing
//     table (Phase 72h).
//   - Severity escalation policy ("notify only when severity >= warning").
//     V1 emits one notification per matching trigger; downstream filtering
//     uses the existing events.subscribe filter shape.
//   - Snooze / dismiss / mute-this-trigger user actions (Console DB only,
//     not runtime entities).
//   - Anomaly detection (post-V1).
//   - Persistence of notification history (rides the same events bus
//     replay surface — Phase 06 ring + Phase 57 durable log).
//   - New Protocol methods (notification.* is an event topic, consumed
//     via the existing events.subscribe surface).
//
// Phase 72d implements the §13 primitive-with-consumer rule via a
// Stage-1 test consumer (`subscriber_test.go::TestSubscriber_TaskFailedSynthesisesNotificationTaskFailed`)
// that fires a deliberate `task.failed` and asserts the synthesised
// `notification.task_failed` arrives at a separately-scoped subscriber
// via the bus. The UI consumers (73a Overview alert ribbon, 73m Settings
// notification-routing matrix) land in Stage 2 and cannot substitute for
// the Stage-1 test consumer per `docs/plans/wave-13-decomposition.md`
// §12 item 5. See also docs/decisions.md D-109.
package notifications

import "github.com/hurtener/Harbor/internal/events"

// V1 notification event-type constants. Each is registered with the
// canonical events.EventType registry from this package's init() so a
// Publish never trips events.ErrUnknownEventType.
//
// Per-class topic naming locked per docs/plans/wave-13-decomposition.md
// §12 + D-109.
const (
	// EventTypeNotificationTaskFailed — synthesised from task.failed
	// (Phase 20). Severity Error. The deep-link points at the failed
	// task in the Console.
	EventTypeNotificationTaskFailed events.EventType = "notification.task_failed"

	// EventTypeNotificationToolApprovalRequested — synthesised from
	// tool.approval_requested (Phase 31). Severity Warning. The deep-link
	// points at the pending approval in the Console's Tools page.
	EventTypeNotificationToolApprovalRequested events.EventType = "notification.tool_approval_requested"

	// EventTypeNotificationGovernanceBudgetExceeded — synthesised from
	// governance.budget_exceeded (Phase 36a). Severity Error. The
	// deep-link points at the affected tenant/session's governance
	// posture in the Console.
	EventTypeNotificationGovernanceBudgetExceeded events.EventType = "notification.governance_budget_exceeded"

	// EventTypeNotificationAuthRequired — synthesised from
	// tool.auth_required (Phase 30). Severity Warning. The deep-link
	// points at the OAuth-binding flow in the Console's MCP Connections
	// page.
	EventTypeNotificationAuthRequired events.EventType = "notification.auth_required"

	// EventTypeNotificationPauseRequested — synthesised from
	// pause.requested (Phase 50). Severity Info. The deep-link points
	// at the paused task in the Console's Interventions queue.
	EventTypeNotificationPauseRequested events.EventType = "notification.pause_requested"

	// EventTypeNotificationIdentityRejected — emitted by the Subscriber
	// when a trigger event arrives with the D-033 `<missing>` sentinel
	// substituted into one or more identity components. Mirrors the
	// `memory.identity_rejected` / `skill.identity_rejected` shape so
	// audit observers consume one identity-rejection vocabulary across
	// the runtime. Fail-loudly per CLAUDE.md §13 — the Subscriber
	// emits this rejection event instead of silently dropping the input
	// or silently publishing a notification with a malformed identity.
	EventTypeNotificationIdentityRejected events.EventType = "notification.identity_rejected"
)

func init() {
	events.RegisterEventType(EventTypeNotificationTaskFailed)
	events.RegisterEventType(EventTypeNotificationToolApprovalRequested)
	events.RegisterEventType(EventTypeNotificationGovernanceBudgetExceeded)
	events.RegisterEventType(EventTypeNotificationAuthRequired)
	events.RegisterEventType(EventTypeNotificationPauseRequested)
	events.RegisterEventType(EventTypeNotificationIdentityRejected)
}

// V1NotificationClasses returns a deterministic snapshot of every
// notification event-type the V1 mapper synthesises. Useful for boot-
// log output, for tests asserting exhaustiveness, and for the Subscriber
// to scope its bus subscription. The identity-rejection class is NOT
// included — it is emitted only by the Subscriber's own error path and
// is not a class consumers filter on positively.
func V1NotificationClasses() []events.EventType {
	return []events.EventType{
		EventTypeNotificationTaskFailed,
		EventTypeNotificationToolApprovalRequested,
		EventTypeNotificationGovernanceBudgetExceeded,
		EventTypeNotificationAuthRequired,
		EventTypeNotificationPauseRequested,
	}
}

// V1TriggerEventTypes returns the set of bus event types the V1 mapper
// listens for. Anything outside this set is unmapped (Map returns
// (nil, nil)); anything inside maps to exactly one notification.*
// event.
//
// `agent.credentials_expired` and `runtime.health_degraded` are named
// in Brief 11 §CC-3's "starter list" but are not shipped V1 event
// types; the mapper accepts them via the input-type registry as future
// inputs but emits nothing for V1 unmapped inputs. Adding the mappings
// is a one-line change in a future phase.
func V1TriggerEventTypes() []events.EventType {
	return []events.EventType{
		"task.failed",                // Phase 20
		"tool.approval_requested",    // Phase 31
		"governance.budget_exceeded", // Phase 36a
		"tool.auth_required",         // Phase 30
		"pause.requested",            // Phase 50
	}
}
