package pauseresume

// This file ships the typed step-tranche pause payload — the canonical
// Go shape for "planner step-budget exhaustion" recorded as a pause.
//
// The runtime's step-tranche counter (the steering RunLoop) is the ONE
// authoritative counter: when a configured tranche of planner steps is
// consumed without a terminal Finish, the RunLoop parks the run through
// the unified Coordinator with Reason = constraints_conflict and this
// typed payload. The payload is the "emits typed
// {cause, max_steps, steps_observed}" contract: observers read it from
// the pause record via Coordinator.List / Coordinator.Status without
// deserialising the checkpointed trajectory (which is separately
// checkpointed on the same record).
//
// The payload deliberately mirrors the planner-side breaker's
// observability vocabulary (`planner.MaxStepsExceededPayload` — the
// planner's absolute breaker is the subordinate terminal backstop):
// `max_steps` is the configured cap that fired, `steps_observed` is the
// cumulative trajectory step count at the moment the counter fired.

// TrancheCause is the typed cause value a step-tranche pause carries.
type TrancheCause string

// The canonical step-tranche pause causes. The set is closed — a
// pause with this payload names exactly one cause.
const (
	// TrancheCauseMaxStepsExceeded — the step-tranche counter reached
	// its configured cap without a terminal Finish; the run is parked
	// for an authorised resume that grants a fresh tranche.
	TrancheCauseMaxStepsExceeded TrancheCause = "max_steps_exceeded"
)

// Payload map keys of a step-tranche pause — the wire contract of the
// pause record's Payload (visible verbatim through the pause-list
// projection). Exported so producers and consumers share one
// vocabulary.
const (
	// TranchePayloadCauseKey is the "cause" payload key.
	TranchePayloadCauseKey = "cause"
	// TranchePayloadMaxStepsKey is the "max_steps" payload key — the
	// configured tranche cap that fired.
	TranchePayloadMaxStepsKey = "max_steps"
	// TranchePayloadStepsKey is the "steps_observed" payload key — the
	// cumulative trajectory step count when the counter fired.
	TranchePayloadStepsKey = "steps_observed"
)

// TrancheExceededPayload is the typed shape of a step-tranche pause's
// Payload. It is the canonical Go type for the emitted
// `{cause: max_steps_exceeded, max_steps, steps_observed}` triple.
// JSON-encodable by construction — every field is the runtime's own
// bookkeeping, so a Coordinator.Request with
// TrancheExceededPayload.Map() never trips trajectory.ErrUnserializable.
type TrancheExceededPayload struct {
	// Cause is the typed cause of the tranche pause —
	// TrancheCauseMaxStepsExceeded for the step-budget shape.
	Cause TrancheCause `json:"cause"`
	// MaxSteps is the configured step-tranche cap that fired.
	MaxSteps int `json:"max_steps"`
	// StepsObserved is the cumulative trajectory step count at the
	// moment the counter fired (always ≥ MaxSteps; equality is the
	// first-tranche case).
	StepsObserved int `json:"steps_observed"`
}

// NewTrancheExceededPayload builds the step-budget-exhaustion payload
// for a tranche cap that fired with the given cumulative observed step
// count.
func NewTrancheExceededPayload(maxSteps, stepsObserved int) TrancheExceededPayload {
	return TrancheExceededPayload{
		Cause:         TrancheCauseMaxStepsExceeded,
		MaxSteps:      maxSteps,
		StepsObserved: stepsObserved,
	}
}

// Map renders the payload in the pause-record Payload map form — the
// shape a Coordinator records and the pause-list projection ships.
func (p TrancheExceededPayload) Map() map[string]any {
	return map[string]any{
		TranchePayloadCauseKey:    string(p.Cause),
		TranchePayloadMaxStepsKey: p.MaxSteps,
		TranchePayloadStepsKey:    p.StepsObserved,
	}
}

// TrancheExceededFromMap decodes a pause-record Payload map back into
// the typed shape. Returns ok=false when the map does not carry a
// complete, well-formed step-tranche payload (a different pause
// reason's payload, or a malformed shape) — never a silently
// half-decoded value.
func TrancheExceededFromMap(m map[string]any) (TrancheExceededPayload, bool) {
	if m == nil {
		return TrancheExceededPayload{}, false
	}
	cause, ok := m[TranchePayloadCauseKey].(string)
	if !ok || TrancheCause(cause) != TrancheCauseMaxStepsExceeded {
		return TrancheExceededPayload{}, false
	}
	maxSteps, ok1 := intFromMap(m[TranchePayloadMaxStepsKey])
	steps, ok2 := intFromMap(m[TranchePayloadStepsKey])
	if !ok1 || !ok2 {
		return TrancheExceededPayload{}, false
	}
	return TrancheExceededPayload{
		Cause:         TrancheCauseMaxStepsExceeded,
		MaxSteps:      maxSteps,
		StepsObserved: steps,
	}, true
}

// intFromMap coerces a payload-map number leaf to int. JSON numbers
// decode as float64; in-process producers may store int directly.
func intFromMap(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
		return 0, false
	default:
		return 0, false
	}
}
