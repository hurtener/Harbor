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
// SafePayload by construction — Token + Reason + Decision only, no
// caller bytes.
//
// `Decision` is the load-bearing typed marker wire consumers (the
// Console, third-party clients, integration tests) switch on to
// distinguish approve from reject from generic resume from timeout —
// the §13 "overloaded `Reason` string" anti-pattern issue #113 / D-096
// closes. `Reason` is the human-readable pause-reason classification
// preserved for context; `Decision` is the typed channel observers
// branch on.
type PauseResumedPayload struct {
	events.SafeSealed
	// Token is the opaque pause Token.
	Token string
	// Reason is the canonical pause reason string (one of the four
	// canonical pause reasons — the reason the pause was *requested*).
	Reason string
	// Decision is the typed marker indicating *how* the pause resumed
	// (approve / reject / resume / timeout). Wire consumers switch on
	// this value rather than parsing `Reason` strings. See D-096.
	Decision Decision
}
