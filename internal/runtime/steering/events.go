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
// Phase 52 emits exactly one of these: control.rejected, on a
// validation / scope failure at Enqueue time. The control.received /
// control.applied lifecycle events (brief 02 §3) are Phase 53's
// concern — Phase 53 wires the drain loop and the side-effect
// application that those events report. Registering control.rejected
// here keeps the audit-on-scope-mismatch path (master-plan Phase 52
// acceptance) end-to-end testable against a real EventBus without
// waiting for the Phase 54 Protocol edge.
const (
	// EventTypeControlRejected — emitted when a steering submission is
	// rejected at the edge: an unknown control type, a payload-bounds
	// violation, or — the master-plan acceptance case — a per-event
	// scope mismatch. Payload is ControlRejectedPayload.
	EventTypeControlRejected events.EventType = "control.rejected"
)

func init() {
	events.RegisterEventType(EventTypeControlRejected)
}

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
// Phase 56 metric derivation).
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
// audit-on-scope-mismatch path the master-plan Phase 52 acceptance
// names ("per-event scope mismatch returns 403 + audit") — the 403 is
// the Protocol edge's job (Phase 54); the audit emit is this. The
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
