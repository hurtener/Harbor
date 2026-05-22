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

	// EventTypePlannerMaxStepsExceeded — emitted by the Phase 45
	// ReAct planner's MaxSteps circuit breaker. When a planner step
	// observes a trajectory whose step count is ≥ the configured
	// MaxSteps, the planner returns
	// Finish{Reason: NoPath, Metadata["max_steps_exceeded"]=true}
	// AND emits this event before returning so operators see the
	// circuit-breaker fire loudly. Companion to repair_exhausted —
	// same fail-loudly shape, different graceful-failure source
	// (repair loop vs. planner-side step cap). D-051.
	EventTypePlannerMaxStepsExceeded events.EventType = "planner.max_steps_exceeded"

	// EventTypeTrajectoryCompressed — emitted by the Phase 46
	// CompressionRunner when the trajectory summariser successfully
	// produces a compaction artefact. Payload
	// (TrajectoryCompressedPayload, SafePayload) carries the run's
	// identity quadruple + step count + token estimate at the moment
	// of compression. The success-path companion to
	// trajectory.compression_failed (the fail-loudly surface). D-055.
	EventTypeTrajectoryCompressed events.EventType = "trajectory.compressed"

	// EventTypeTrajectoryCompressionFailed — emitted by the Phase 46
	// CompressionRunner when the summariser returns an error, the
	// estimator fails, or the summariser returns (nil, nil). Payload
	// (TrajectoryCompressionFailedPayload, SafePayload) carries the
	// identity + step count + token estimate + error code + truncated
	// error message. The load-bearing fail-loudly observability surface
	// for compression failures (§13 — silent degradation banned). D-055.
	EventTypeTrajectoryCompressionFailed events.EventType = "trajectory.compression_failed"

	// EventTypePlannerRepairGuidanceInjected — emitted by the Phase 83c
	// ReAct prompt builder each turn it merges an escalating repair-
	// guidance block into the system prompt because a
	// [RunContext.RepairCounters] field tripped. Payload
	// (RepairGuidanceInjectedPayload, SafePayload) carries the run
	// identity + the tier (`reminder` / `warning` / `critical`) + the
	// counter name (`finish` / `args` / `multi_action`) + the counter
	// value at render time. The emit lets the Console / operator see
	// when the LLM is struggling to produce well-formed output across
	// steps — the across-step companion to `planner.repair_exhausted`
	// (the per-step terminal). D-145.
	EventTypePlannerRepairGuidanceInjected events.EventType = "planner.repair_guidance_injected"

	// EventTypePlannerActionExtraFieldDropped — emitted by the Phase 44
	// repair loop when an incoming action object carried a field the
	// Phase 83e-narrowed action schema no longer recognises (`reasoning`
	// / `thought` — D-147). The loop strips the field and emits this
	// event for telemetry; it is a soft signal, NOT a failure — the
	// runtime fails open on extra fields for backward compatibility with
	// older trained models. Payload (ActionExtraFieldDroppedPayload,
	// SafePayload) carries the run identity + the dropped field name.
	EventTypePlannerActionExtraFieldDropped events.EventType = "planner.action_extra_field_dropped"
)

func init() {
	events.RegisterEventType(EventTypePlannerDecision)
	events.RegisterEventType(EventTypePlannerFinish)
	events.RegisterEventType(EventTypePlannerError)
	events.RegisterEventType(EventTypePlannerRepairExhausted)
	events.RegisterEventType(EventTypePlannerMaxStepsExceeded)
	events.RegisterEventType(EventTypeTrajectoryCompressed)
	events.RegisterEventType(EventTypeTrajectoryCompressionFailed)
	events.RegisterEventType(EventTypePlannerRepairGuidanceInjected)
	events.RegisterEventType(EventTypePlannerActionExtraFieldDropped)
}

// DecisionPayload is the typed payload for EventTypePlannerDecision.
// Phase 42 registered the event type; Phase 83e (D-147) ships the
// first emitter (the ReAct planner) AND the payload — satisfying the
// §13 primitive-with-consumer rule for the long-registered type.
// SafePayload — every field is operator-visible debug data:
//
//   - `Identity` is the run's identity quadruple.
//   - `DecisionKind` is the resolved Decision shape name (`CallTool`,
//     `CallParallel`, `Finish`, `SpawnTask`, `AwaitTask`,
//     `RequestPause`).
//   - `Tool` is the tool name when DecisionKind is `CallTool`; empty
//     otherwise.
//   - `ReasoningChars` is the rune count of the captured reasoning
//     trace — a scannable size signal that never carries raw content.
//   - `ReasoningTrace` is the provider-side thinking trace captured
//     for the step (Phase 83e). Reasoning can be sensitive; the event
//     is published onto the bus where the audit redactor processes it
//     before any sink persists it (CLAUDE.md §7). `inspect-runs`
//     surfaces it as `steps[].reasoning_trace`.
//
// The emit is the observability surface that lets `harbor inspect-runs`
// reconstruct a run's reasoning channel from the event stream.
type DecisionPayload struct {
	events.SafeSealed
	Identity       identity.Quadruple
	DecisionKind   string
	Tool           string
	ReasoningChars int
	ReasoningTrace string
	OccurredAt     time.Time
}

// ActionExtraFieldDroppedPayload is the typed payload for
// EventTypePlannerActionExtraFieldDropped (Phase 83e — D-147).
// SafePayload — every field is operator-visible debug data, never
// secret-shaped:
//
//   - `Identity` is the run's identity quadruple.
//   - `Field` is the name of the dropped extra field (`reasoning` or
//     `thought`) — the parser strips fields the narrowed `{tool, args}`
//     action schema no longer recognises.
//
// The event is a SOFT telemetry signal: extra fields are stripped, the
// step proceeds. It is NOT a fail-loudly surface — the runtime fails
// OPEN on extra fields for backward compatibility with older trained
// models. The captured thinking trace flows through the provider
// channel onto `trajectory.Step.ReasoningTrace` instead.
type ActionExtraFieldDroppedPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Field      string
	OccurredAt time.Time
}

// RepairGuidanceInjectedPayload is the typed payload for
// EventTypePlannerRepairGuidanceInjected (Phase 83c — D-145).
// SafePayload — every field is operator-visible debug data, never
// secret-shaped:
//
//   - `Identity` is the run's identity quadruple.
//   - `Tier` is the escalation tier the builder rendered: `reminder`
//     (counter == 1), `warning` (counter == 2), `critical`
//     (counter >= 3).
//   - `Counter` names the tripped counter: `finish`, `args`, or
//     `multi_action`.
//   - `Count` is the [RepairCounters] field value at render time.
//
// The emit is the observability surface that lets the Console show an
// operator when the LLM is repeatedly producing malformed output —
// the across-step companion to `planner.repair_exhausted` (the
// per-step terminal). Phase 83c ships the payload alongside the first
// emitter (the ReAct prompt builder).
type RepairGuidanceInjectedPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Tier       string
	Counter    string
	Count      int
	OccurredAt time.Time
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

// MaxStepsExceededPayload is the typed payload for
// EventTypePlannerMaxStepsExceeded. SafePayload — every field is
// operator-visible debug data, not secret-shaped:
//
//   - `Identity` is the run's identity quadruple.
//   - `MaxSteps` is the configured circuit-breaker cap that fired.
//   - `StepsObserved` is the trajectory step count at the moment the
//     breaker fired (always ≥ MaxSteps; equality is the typical case
//     when the breaker is the load-bearing terminator).
//   - `LastTool` is the most-recently-dispatched tool name (from the
//     last trajectory step's Action), or empty when the trajectory
//     was empty AND MaxSteps == 0 (a degenerate config).
//
// Phase 45 ships the payload alongside the emit site; the emit is
// the load-bearing fail-loudly surface that makes the circuit
// breaker not silent (§13). D-051.
type MaxStepsExceededPayload struct {
	events.SafeSealed
	Identity      identity.Quadruple
	MaxSteps      int
	StepsObserved int
	LastTool      string
	OccurredAt    time.Time
}

// TrajectoryCompressedPayload is the typed payload for
// EventTypeTrajectoryCompressed (Phase 46). SafePayload — every field
// is operator-visible debug data, not secret-shaped:
//
//   - `Identity` is the run's identity quadruple.
//   - `StepsBefore` is the trajectory step count when compression ran.
//   - `StepsAfter` is the step count after compression. Phase 46 does
//     NOT truncate the Steps slice; the runner only stamps Summary.
//     `StepsAfter == StepsBefore` in V1; the field exists so future
//     phases that truncate (free memory; trade observability for
//     footprint) extend the schema without a payload-version bump.
//   - `TokenEstimate` is the estimator's count at the moment the
//     budget was breached.
//
// The emit is the success-path observability surface that pairs with
// trajectory.compression_failed for the failure path; together they
// make compression observable in both directions (§13 fail-loudly).
// D-055.
type TrajectoryCompressedPayload struct {
	events.SafeSealed
	Identity      identity.Quadruple
	StepsBefore   int
	StepsAfter    int
	TokenEstimate int
	OccurredAt    time.Time
}

// TrajectoryCompressionFailedPayload is the typed payload for
// EventTypeTrajectoryCompressionFailed (Phase 46). SafePayload — the
// fields carry operator-visible debug data:
//
//   - `Identity` is the run's identity quadruple.
//   - `StepsObserved` is the trajectory step count at the moment of
//     the failure.
//   - `TokenEstimate` is the estimator's count when the failure
//     happened (zero when the estimator itself failed).
//   - `ErrorCode` classifies the failure into one of three buckets:
//     `summariser_error` (the Summariser returned a non-nil error),
//     `empty_summary` (the Summariser returned (nil, nil) — contract
//     violation), `estimator_error` (the TokenEstimator returned an
//     error, typically a Phase 43 ErrUnserializable surfaced through
//     DefaultTokenEstimator's Serialize call).
//   - `ErrorMessage` is the truncated original error message (capped
//     at 256 chars to keep audit payloads bounded). Never carries raw
//     trajectory content.
//
// Phase 46 ships the payload + the emit; the bus subscribers observe
// the failure end-to-end. The emit is the load-bearing fail-loudly
// observability surface (§13 — silent degradation banned). D-055.
type TrajectoryCompressionFailedPayload struct {
	events.SafeSealed
	Identity      identity.Quadruple
	StepsObserved int
	TokenEstimate int
	ErrorCode     string
	ErrorMessage  string
	OccurredAt    time.Time
}
