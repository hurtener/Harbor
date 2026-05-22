package approval

import "github.com/hurtener/Harbor/internal/events"

// Canonical tool-approval event types. Registered from this package's
// init() so a Publish never trips events.ErrUnknownEventType.
//
// The three events form the gate's full lifecycle:
//
//   - `tool.approval_requested` — emitted at gate entry when the
//     `ApprovalPolicy` returns `Required=true`. Payload is
//     `ToolApprovalRequestedPayload`. SafePayload — never carries the
//     original args (those stay in the gate's pending map).
//   - `tool.approved` — emitted on APPROVE resolution. Payload is
//     `ToolApprovedPayload`.
//   - `tool.rejected` — emitted on REJECT resolution; this is the
//     master-plan acceptance criterion event. Payload is
//     `ToolRejectedPayload`.
//
// All three payload types embed `events.SafeSealed` so the bus accepts
// them on the typed path. The audit redactor is still run by the gate
// on `ToolApprovalRequestedPayload.ArgsSummary` BEFORE construction —
// the SafePayload tag asserts "this struct itself carries no
// secret-shaped data" once the summary has been redacted.
const (
	// EventTypeToolApprovalRequested — emitted when a tool invocation
	// is gated and the `ApprovalPolicy` returned `Required=true`.
	// Payload is `ToolApprovalRequestedPayload`.
	EventTypeToolApprovalRequested events.EventType = "tool.approval_requested"

	// EventTypeToolApproved — emitted by `ApprovalGate` on a
	// resolution of `DecisionApprove`. Payload is `ToolApprovedPayload`.
	EventTypeToolApproved events.EventType = "tool.approved"

	// EventTypeToolRejected — emitted by `ApprovalGate` on a
	// resolution of `DecisionReject`. THE master-plan acceptance
	// event ("reject path raises typed `tool.rejected` events").
	// Payload is `ToolRejectedPayload`.
	EventTypeToolRejected events.EventType = "tool.rejected"
)

func init() {
	events.RegisterEventType(EventTypeToolApprovalRequested)
	events.RegisterEventType(EventTypeToolApproved)
	events.RegisterEventType(EventTypeToolRejected)
}

// ToolApprovalRequestedPayload is the typed payload for a
// `tool.approval_requested` event. SafePayload by construction: every
// field is either runtime bookkeeping or operator-supplied
// configuration metadata. The ArgsSummary field is the redactor's
// output, NOT the original args; the original args stay in the gate's
// pending map and never reach the bus.
type ToolApprovalRequestedPayload struct {
	events.SafeSealed
	ArgsSummary any
	Tool        string
	PauseToken  string
	Reason      string
	Tags        []string
}

// ToolApprovedPayload is the typed payload for a `tool.approved`
// event. SafePayload by construction.
type ToolApprovedPayload struct {
	events.SafeSealed
	// Tool is the tool name that was approved.
	Tool string
	// PauseToken correlates with the originating
	// `tool.approval_requested` payload.
	PauseToken string
	// ApproverReason is the optional caller-supplied note attached
	// to the APPROVE submission. Free-form but low-cardinality by
	// convention.
	ApproverReason string
}

// ToolRejectedPayload is the typed payload for a `tool.rejected`
// event. THIS is the master-plan acceptance criterion shape. The
// rejected RunID + identity triple live on the Event envelope
// (`Event.Identity`); the payload carries the per-event detail.
// SafePayload by construction.
type ToolRejectedPayload struct {
	events.SafeSealed
	// Tool is the tool name that was rejected.
	Tool string
	// PauseToken correlates with the originating
	// `tool.approval_requested` payload.
	PauseToken string
	// Reason is the approver's free-form classification of the
	// rejection. Low-cardinality by convention.
	Reason string
}
