package steering

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Canonical event types this package registers into the events
// package's canonical registry from init(), so a Publish never trips
// events.ErrUnknownEventType.
//
// emitted exactly one of these: control.rejected, on a
// validation / scope failure at Enqueue time. Harbor adds the two
// canonical lifecycle events — control.received (a control
// event was drained from the per-run inbox by the RunLoop) and
// control.applied (the RunLoop applied the control's side effect).
// Together with control.rejected they are the full steering audit
// trail; the Protocol edge surfaces them over the wire.
const (
	// EventTypeControlRejected — emitted when a steering submission is
	// rejected at the edge: an unknown control type, a payload-bounds
	// violation, or — the master-plan acceptance case — a per-event
	// scope mismatch. Payload is ControlRejectedPayload.
	EventTypeControlRejected events.EventType = "control.rejected"

	// EventTypeControlReceived — emitted by the RunLoop when a control
	// event is drained from the per-run inbox at a step boundary
	// (before its side effect is applied). Payload is
	// ControlLifecyclePayload.
	EventTypeControlReceived events.EventType = "control.received"

	// EventTypeControlApplied — emitted by the RunLoop after a drained
	// control event's side effect has been applied (the goal was
	// rewritten, the pause was requested / resumed, the task was
	// reprioritised, etc.). Payload is ControlLifecyclePayload — the
	// Err field is non-empty when the side effect failed.
	EventTypeControlApplied events.EventType = "control.applied"

	// EventTypeRunHookDispatched — emitted by the RunLoop when the
	// run-completion hook successfully dispatched the run transcript to
	// the configured catalog tool at the run's terminal boundary. Payload
	// is RunHookDispatchedPayload (metadata only — never transcript
	// content). A healthy hook is otherwise invisible as a hook (the
	// generic tool.* events do not say "this was the completion hook"), so
	// the symmetry with run.hook_failed earns the slot.
	EventTypeRunHookDispatched events.EventType = "run.hook_dispatched"

	// EventTypeRunHookFailed — emitted by the RunLoop when the
	// run-completion hook dispatch failed (unknown tool, transport error,
	// timeout, missing executor). The hook failure NEVER alters the run
	// outcome — this event plus a Warn log are the whole failure posture.
	// Payload is RunHookFailedPayload (metadata only — identity, tool,
	// outcome, error class; never transcript content or raw error text).
	EventTypeRunHookFailed events.EventType = "run.hook_failed"

	// EventTypeSessionNamingFailed — emitted by the run loop's
	// terminal-boundary auto-naming trigger when a title could not be
	// produced or applied: an LLM error, a timeout, an empty / unusable
	// candidate, a governance block, a manual-title skip, or a contained
	// internal panic. The naming failure NEVER alters the settled run
	// outcome — this event plus a Warn log are the whole failure posture.
	// Payload is SessionNamingFailedPayload (SafePayload: identity scope +
	// session id + a stable error class; NEVER the transcript, the prompt,
	// or the candidate title).
	EventTypeSessionNamingFailed events.EventType = "session.naming_failed"
)

func init() {
	events.RegisterEventType(EventTypeControlRejected)
	events.RegisterEventType(EventTypeControlReceived)
	events.RegisterEventType(EventTypeControlApplied)
	events.RegisterEventType(EventTypeRunHookDispatched)
	events.RegisterEventType(EventTypeRunHookFailed)
	events.RegisterEventType(EventTypeSessionNamingFailed)
}

// RunHookDispatchedPayload is the typed payload for a run.hook_dispatched
// event. SafePayload by construction: every field is the RunLoop's own
// bookkeeping — the tool name, the low-cardinality outcome classification,
// and bounded integer sizes/timings. The transcript CONTENT is never
// carried (it travels only as tool args to the target's transport).
type RunHookDispatchedPayload struct {
	events.SafeSealed
	// Tool is the catalog tool the transcript was dispatched to.
	Tool string
	// Outcome is the run's terminal outcome carried in the payload.
	Outcome string
	// DurationMS is the hook dispatch's wall-clock duration in ms.
	DurationMS int64
	// TranscriptBytes is the size of the JSON-encoded RunCompletionPayload
	// dispatched as tool args.
	TranscriptBytes int
	// EntryCount is the number of ordered transcript entries.
	EntryCount int
}

// RunHookFailedPayload is the typed payload for a run.hook_failed event.
// SafePayload by construction: the tool name, the low-cardinality outcome,
// and a stable sentinel-derived error class — never the transcript content
// or the raw error message (which may quote caller data — §7).
type RunHookFailedPayload struct {
	events.SafeSealed
	// Tool is the catalog tool the dispatch targeted.
	Tool string
	// Outcome is the run's terminal outcome carried in the payload.
	Outcome string
	// ErrorClass is a stable, low-cardinality classification of the
	// dispatch failure ("timeout" / "no_executor" / "encode_failed" /
	// "unsupported_shape" / "dispatch_failed" / "cancelled" / "panic").
	// Never the raw error message.
	ErrorClass string
}

// SessionNamingFailedPayload is the typed payload for a
// session.naming_failed event. SafePayload by construction: the session id
// (a bounded id) and a stable, low-cardinality error class ONLY — never the
// transcript, the prompt, the raw model output, the candidate title, or a raw
// error message (any of which could carry user-derived content — §7). The
// identity scope rides on the Event itself, not duplicated here.
type SessionNamingFailedPayload struct {
	events.SafeSealed
	// SessionID is the session whose auto-naming was skipped / failed.
	SessionID string
	// ErrorClass is a stable, low-cardinality classification of the failure
	// or skip ("llm_error" / "timeout" / "empty_title" / "governance_blocked"
	// / "manual_title" / "internal"). Never the raw error message.
	ErrorClass string
}

// ControlLifecyclePayload is the typed payload for control.received and
// control.applied events. SafePayload by construction: every field is
// the RunLoop's own bookkeeping — the control Type is one of nine
// canonical enum values, the Outcome / Err strings are low-cardinality
// runtime-derived classifications. The caller-controlled control
// payload itself is NOT carried (mirroring ControlRejectedPayload):
// these events are a low-cardinality audit trail, not a payload
// archive.
type ControlLifecyclePayload struct {
	events.SafeSealed
	// Type is the control type that was received / applied.
	Type string
	// Outcome is a stable, low-cardinality classification of the apply
	// result — "received" for control.received, and one of "applied" /
	// "failed" for control.applied. Empty on a control.received event.
	Outcome string
	// Err is a short, redaction-safe description of why the side
	// effect failed, when Outcome == "failed". Empty otherwise. The
	// RunLoop derives this from a sentinel classification, never the
	// raw error message (which may quote caller data).
	Err string
}

// Control lifecycle outcome strings — stable, low-cardinality (safe for
// metric derivation).
const (
	outcomeReceived = "received"
	outcomeApplied  = "applied"
	outcomeFailed   = "failed"
)

// ControlRejectedPayload is the typed payload for a control.rejected
// event. SafePayload by construction: every field is the steering
// edge's own bookkeeping — the control Type is one of nine canonical
// enum values, the Reason is a fixed sentinel-derived string, the
// CallerScope is one of three canonical enum values. The rejected
// payload itself is NOT carried — it may hold caller-controlled data
// and is exactly what was rejected; persisting it would defeat the
// rejection.
type ControlRejectedPayload struct {
	events.SafeSealed
	// Type is the control type that was rejected (may be the empty
	// string when the rejection was an unknown / unparsable type).
	Type string
	// Reason is a stable, low-cardinality classification of why the
	// submission was rejected — one of "unknown_type",
	// "payload_invalid", "scope_mismatch", "identity_invalid".
	Reason string
	// CallerScope is the scope the rejected caller presented.
	CallerScope string
}

// Rejection reason strings — stable, low-cardinality (safe for
// metric derivation).
const (
	reasonUnknownType     = "unknown_type"
	reasonPayloadInvalid  = "payload_invalid"
	reasonScopeMismatch   = "scope_mismatch"
	reasonIdentityInvalid = "identity_invalid"
)

// classifyRejection maps an Enqueue error to its stable
// control.rejected Reason string. An error that is not one of the
// known rejection sentinels classifies as "payload_invalid" (the
// catch-all for a malformed submission) — it is never silently
// dropped.
func classifyRejection(err error) string {
	switch {
	case errors.Is(err, ErrUnknownControlType):
		return reasonUnknownType
	case errors.Is(err, ErrScopeMismatch), errors.Is(err, ErrInvalidScope):
		return reasonScopeMismatch
	case errors.Is(err, ErrIdentityRequired):
		return reasonIdentityInvalid
	default:
		// ErrPayloadInvalid, ErrUnsupportedPayloadValue, and any
		// other Enqueue failure.
		return reasonPayloadInvalid
	}
}

// EmitRejection publishes a control.rejected event onto the bus for a
// steering submission that Inbox.Enqueue rejected. It is the
// audit-on-scope-mismatch path the master-plan acceptance
// names ("per-event scope mismatch returns 403 + audit") — the 403 is
// the Protocol edge's job; the audit emit is this. The
// Protocol edge calls EmitRejection whenever Enqueue returns a
// non-nil error.
//
// rejectErr is the error Enqueue returned; it is classified into a
// stable Reason string (never inspected for its message bytes, which
// may quote caller data). The event carries the run's identity
// quadruple so identity-scoped subscribers see it. A nil bus, a nil
// rejectErr, or an events.Publish failure is returned wrapped — the
// caller (the Protocol edge) decides whether an un-emittable audit
// event should fail the request loud; EmitRejection never silently
// swallows it.
func EmitRejection(ctx context.Context, bus events.EventBus, q identity.Quadruple, t ControlType, callerScope Scope, rejectErr error) error {
	if bus == nil {
		return fmt.Errorf("steering: EmitRejection called with nil bus")
	}
	if rejectErr == nil {
		return fmt.Errorf("steering: EmitRejection called with nil rejectErr")
	}
	ev := events.Event{
		Type:     EventTypeControlRejected,
		Identity: q,
		Payload: ControlRejectedPayload{
			Type:        string(t),
			Reason:      classifyRejection(rejectErr),
			CallerScope: string(callerScope),
		},
	}
	if err := bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("steering: publishing control.rejected: %w", err)
	}
	return nil
}
