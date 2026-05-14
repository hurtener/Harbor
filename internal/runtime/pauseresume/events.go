package pauseresume

import "github.com/hurtener/Harbor/internal/events"

// Canonical event types emitted by the Coordinator. Registered into
// the events package's canonical registry from this package's init()
// so a Publish never trips events.ErrUnknownEventType.
const (
	// EventTypePauseRequested — emitted by Coordinator.Request when a
	// pause record is created. Payload is PauseRequestedPayload.
	EventTypePauseRequested events.EventType = "pause.requested"
	// EventTypePauseResumed — emitted by Coordinator.Resume when a
	// pause record terminates. Payload is PauseResumedPayload.
	EventTypePauseResumed events.EventType = "pause.resumed"
)

func init() {
	events.RegisterEventType(EventTypePauseRequested)
	events.RegisterEventType(EventTypePauseResumed)
}

// PauseRequestedPayload is the typed payload for a pause.requested
// event. SafePayload by construction: every field is the Coordinator's
// own bookkeeping. The Token is opaque and carries no caller bytes;
// the Reason is one of four canonical enum values. The pause Payload
// itself is NOT carried on the event — it may carry caller-controlled
// data (an OAuth auth URL, approval context) and is left to the
// Protocol-edge projection (a later phase) to redact/bound. Observers
// that need the payload read it via Coordinator.Status.
type PauseRequestedPayload struct {
	events.SafeSealed
	// Token is the opaque pause Token.
	Token string
	// Reason is the canonical pause reason string.
	Reason string
}

// PauseResumedPayload is the typed payload for a pause.resumed event.
// SafePayload by construction — Token + Reason only, no caller bytes.
type PauseResumedPayload struct {
	events.SafeSealed
	// Token is the opaque pause Token.
	Token string
	// Reason is the canonical pause reason string.
	Reason string
}
