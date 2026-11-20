package approval

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

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
	// Tool is the tool name the planner / runtime chose; surfaced by
	// the Console as "Approve call to <Tool>?".
	Tool string
	// PauseToken is the unified pause/resume Coordinator's Token —
	// observers correlate this to the `pause.requested` event and the
	// later `pause.resumed` / `tool.approved` / `tool.rejected`.
	PauseToken string
	// Reason is the operator-facing classification the
	// `ApprovalPolicy` returned. Low-cardinality by convention.
	Reason string
	// Tags is the caller-supplied classification surface the policy
	// branched on.
	Tags []string
	// ArgsSummary is the audit-redacted summary of the tool args.
	// The redactor runs over a generic shape (map[string]any) at the
	// gate's emit boundary; secret-shaped values are elided. The
	// post-APPROVE tool invocation uses the ORIGINAL args (held in
	// the gate's pending map), so a redactor that elides a field
	// does NOT corrupt the executed call.
	ArgsSummary any
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
	// DecidedByTenant / DecidedByUser / DecidedBySession name the
	// verified principal whose control decided the approval — the
	// identity the approve / reject request authenticated as (the acting
	// user when a session participant decided inside another user's
	// session). DecidedBySpeaker is that bearer's signed display label,
	// when it carried one. The event envelope keeps the identity of the
	// gated run. All empty when no verified principal decided.
	DecidedByTenant  string
	DecidedByUser    string
	DecidedBySession string
	DecidedBySpeaker string
	// DecidedAt is when the decision resolved the approval.
	DecidedAt time.Time
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
	// Decision is the typed outcome that resolved the approval without
	// running the tool: "reject" (an approver rejected it), "timeout"
	// (the approval expired: the configured
	// pauseresume.max_park_duration elapsed with no decision) or
	// "interrupted" (the runtime restarted while the approval waited).
	// Same vocabulary as pause.resumed's Decision. Hosts render
	// "timeout" as an expired approval.
	Decision string
	// DecidedByTenant / DecidedByUser / DecidedBySession name the
	// verified principal whose control decided the approval — the
	// identity the approve / reject request authenticated as (the acting
	// user when a session participant decided inside another user's
	// session). DecidedBySpeaker is that bearer's signed display label,
	// when it carried one. The event envelope keeps the identity of the
	// gated run. All empty when no verified principal decided.
	DecidedByTenant  string
	DecidedByUser    string
	DecidedBySession string
	DecidedBySpeaker string
	// DecidedAt is when the decision resolved the approval.
	DecidedAt time.Time
}
