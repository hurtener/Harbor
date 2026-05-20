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
	// EventTypePausePayloadArtifactRouted — emitted by the Phase 72e
	// `pause.list` snapshot path when a pause record's Payload
	// serialised size meets or exceeds the configured heavy-content
	// threshold and is routed through the ArtifactStore instead of
	// shipped inline (D-026 — context-window safety net applied to
	// Protocol read snapshots). The emit makes the bypass LOUD — a
	// heavy payload is never silently truncated. Payload is
	// PausePayloadArtifactRoutedPayload.
	EventTypePausePayloadArtifactRouted events.EventType = "pause.payload_artifact_routed"
)

func init() {
	events.RegisterEventType(EventTypePauseRequested)
	events.RegisterEventType(EventTypePauseResumed)
	events.RegisterEventType(EventTypePausePayloadArtifactRouted)
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

// PausePayloadArtifactRoutedPayload is the typed payload for a
// `pause.payload_artifact_routed` event. SafePayload by construction:
// every field is the runtime's own bookkeeping — the opaque pause
// Token, the content-addressed artifact ref ID, and the byte sizes
// involved. NO caller-controlled payload bytes are carried (the heavy
// payload itself went to the ArtifactStore, which is the whole point
// of the bypass). Phase 72e (D-110, D-026).
type PausePayloadArtifactRoutedPayload struct {
	events.SafeSealed
	// Token is the opaque pause Token whose Payload was routed.
	Token string
	// ArtifactID is the content-addressed ID of the artifact the
	// heavy Payload was materialised into.
	ArtifactID string
	// PayloadBytes is the serialised byte length of the routed Payload.
	PayloadBytes int
	// ThresholdBytes is the configured heavy-content threshold the
	// PayloadBytes met or exceeded.
	ThresholdBytes int
}
