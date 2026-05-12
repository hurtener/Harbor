package planner

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Planner-emitted event types. Phase 42 registers the constants with
// the events package's canonical registry so future concretes
// (Phase 45 ReAct, Phase 48 Deterministic, etc.) can emit without
// re-registering.
//
// Most payload structs land at the phase that first emits each type —
// Phase 42 ships only the type-name registration for `planner.decision`
// / `planner.finish` / `planner.error`, since the stub finish.Planner
// does not emit. Decoupling type registration from payload definition
// matches the events-package convention (RFC §6.2 / events/events.go
// §RegisterEventType).
//
// Phase 44 adds `planner.repair_exhausted` AND its typed payload — the
// repair loop is the first emitter, so the payload ships in the same
// PR (CLAUDE.md §13 fail-loudly principle: the emit is the
// observability surface that makes graceful failure NOT silent).
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

	// EventTypePlannerRepairExhausted — emitted by the Phase 44
	// repair loop on the graceful-failure path: after
	// `max_consecutive_arg_failures` consecutive arg-validation
	// failures OR `repair_attempts` exceeded, the loop returns
	// Finish{Reason: NoPath, Metadata["followup"]=true} AND emits
	// this event so operators see the failure loudly. The event is
	// the load-bearing surface that distinguishes Harbor's graceful
	// failure from the silent-degradation pattern banned by §13.
	EventTypePlannerRepairExhausted events.EventType = "planner.repair_exhausted"
)

func init() {
	events.RegisterEventType(EventTypePlannerDecision)
	events.RegisterEventType(EventTypePlannerFinish)
	events.RegisterEventType(EventTypePlannerError)
	events.RegisterEventType(EventTypePlannerRepairExhausted)
}

// RepairExhaustedPayload is the typed payload for
// EventTypePlannerRepairExhausted. SafePayload — every field is
// operator-visible debug data, not secret-shaped:
//
//   - `Identity` is the run's identity quadruple.
//   - `Attempts` is the total LLM re-asks the loop burned before
//     giving up (1-based; 1 means the loop made the initial call,
//     observed the failure, and exited without re-asking).
//   - `ConsecutiveArgFailures` is the consecutive-arg-failure counter
//     value at the moment of exhaustion. When it equals
//     `RepairLoop.cfg.MaxConsecutiveArgFailures`, the storm-guard path
//     fired; when it is less, the `RepairAttempts` budget exhausted.
//   - `Reasons` is the truncated chain of validator failures seen
//     across attempts (each entry capped to 256 chars by the loop).
//
// Phase 44 ships the payload with the emit; Phase 49's conformance
// pack asserts the round-trip.
type RepairExhaustedPayload struct {
	events.SafeSealed
	Identity               identity.Quadruple
	Attempts               int
	ConsecutiveArgFailures int
	Reasons                []string
	OccurredAt             time.Time
}
