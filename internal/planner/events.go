package planner

import "github.com/hurtener/Harbor/internal/events"

// Planner-emitted event types. Phase 42 registers the constants with
// the events package's canonical registry so future concretes
// (Phase 45 ReAct, Phase 48 Deterministic, etc.) can emit without
// re-registering.
//
// The payload structs land at the phase that first emits each type —
// Phase 42 ships only the type-name registration, since the stub
// finish.Planner does not emit. Decoupling type registration from
// payload definition matches the events-package convention (RFC §6.2
// / events/events.go §RegisterEventType).
const (
	// EventTypePlannerDecision — emitted by a planner concrete after
	// each Next call. Payload (defined at Phase 45) carries the
	// Decision shape + reasoning hash + step latency.
	EventTypePlannerDecision events.EventType = "planner.decision"

	// EventTypePlannerFinish — emitted when a planner returns
	// Finish{}. Payload (defined at Phase 45) carries the
	// FinishReason + terminal metadata.
	EventTypePlannerFinish events.EventType = "planner.finish"

	// EventTypePlannerError — emitted when Planner.Next returns an
	// error. Payload (defined at Phase 45) carries the error code +
	// message + step index.
	EventTypePlannerError events.EventType = "planner.error"
)

func init() {
	events.RegisterEventType(EventTypePlannerDecision)
	events.RegisterEventType(EventTypePlannerFinish)
	events.RegisterEventType(EventTypePlannerError)
}
