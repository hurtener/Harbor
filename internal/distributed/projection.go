package distributed

import "github.com/hurtener/Harbor/internal/events"

// EventTypeDistributedBusEnvelope is the canonical event type a
// MessageBus driver projects each published BusEnvelope onto the typed
// events.EventBus as. Subscribers wired to the event bus observe
// envelopes by filtering on this type.
//
// It is the SHARED projection contract every bus driver uses (loopback,
// durable, and any future driver), so one set of EventBus subscribers
// consumes envelopes regardless of which driver is configured. It lives
// in the contract package — not in any one driver — so a driver never
// imports another driver to reach it.
const EventTypeDistributedBusEnvelope events.EventType = "distributed.bus_envelope"

func init() {
	events.RegisterEventType(EventTypeDistributedBusEnvelope)
}

// BusEnvelopePayload is the typed event payload carrying a BusEnvelope
// projection. It is a SafePayload (embeds events.SafeSealed): the
// envelope's Payload bytes are assumed pre-redacted by the publisher
// (BusEnvelope.Payload is caller-redacted before Publish), so the
// projection bypasses the redactor. Consumers idempotency-key on
// (Envelope.TaskID, Envelope.Edge, Envelope.EventID).
type BusEnvelopePayload struct {
	events.SafeSealed
	// Envelope is the published BusEnvelope, projected onto the typed
	// event bus.
	Envelope BusEnvelope
}
