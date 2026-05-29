package react

import (
	"encoding/json"

	"github.com/hurtener/Harbor/internal/planner"
)

// DeclarativeActionToolName is the canonical built-in meta-tool name
// the React planner inspects for repair-outcome signals (Phase 107c
// step 10 — AC-13 + AC-20c). When the runloop dispatches
// `declarative_action`, the meta-tool body classifies its outcome into
// a `repair_outcome` field on its observation. The planner walks the
// last trajectory step at the START of its next `Next()` invocation
// and — when that step's Action is `CallTool{declarative_action}` —
// reads the outcome from the observation and bumps the per-run
// `RepairCounters` accordingly.
//
// The constant lives here (not imported from `internal/tools/builtin`)
// to keep the React package's import graph free of `internal/tools/...`
// concrete dependencies. A drift test in `declarative_outcomes_test.go`
// pins the constant against the authoritative builtin name.
const DeclarativeActionToolName = "declarative_action"

// declarativeRepairFields is the structured `repair_outcome` shape the
// `declarative_action` meta-tool emits. Mirrors the
// `builtin.DeclarativeRepairOutcome` struct field-for-field; declared
// independently here to keep the React → builtin import edge unforged
// (the CLAUDE.md §13 import-graph contract). The JSON tags must stay
// in lockstep with `builtin.DeclarativeRepairOutcome`; the drift test
// asserts this.
type declarativeRepairFields struct {
	ArgsRepaired bool `json:"args_repaired,omitempty"`
	MultiAction  bool `json:"multi_action,omitempty"`
	FinishRepair bool `json:"finish_repair,omitempty"`
}

// declarativeObservation is the structural projection of the
// `declarative_action` meta-tool's `DeclarativeActionOut` shape the
// planner cares about. Only the `repair_outcome` field is consumed;
// other fields (`dispatched`, `observation`, `error`, `tool`) round-
// trip through the trajectory unchanged for the LLM's next turn.
type declarativeObservation struct {
	RepairOutcome *declarativeRepairFields `json:"repair_outcome,omitempty"`
}

// applyDeclarativeOutcome bumps the run's `RepairCounters` when the
// last trajectory step was a `CallTool{declarative_action}` whose
// observation carried a `repair_outcome` signal. Returns true when the
// counters were touched — the planner uses this to suppress the
// end-of-step reset so a clean LLM emission of declarative_action
// doesn't immediately wipe the bump (the wipe is correct ONLY for a
// genuine clean native response, which declarative_action is not).
//
// Semantics:
//
//   - args_repaired → counters.ArgsRepair++ (does not touch the other
//     fields; the reset of MultiAction / FinishRepair happens via the
//     end-of-step updateRepairCounters in the normal flow).
//   - multi_action → counters.MultiAction++
//   - finish_repair → counters.FinishRepair++
//
// A nil RepairCounters means the runtime opted out of dynamic
// augmentation; the call is then a no-op (matches updateRepairCounters
// semantics).
//
// Identity-mandatory: the function reads from rc directly; the
// trajectory it walks already carries identity via the run's quadruple.
func applyDeclarativeOutcome(rc planner.RunContext) bool {
	if rc.RepairCounters == nil {
		return false
	}
	t := rc.Trajectory
	if t == nil || len(t.Steps) == 0 {
		return false
	}
	last := t.Steps[len(t.Steps)-1]
	call, ok := last.Action.(planner.CallTool)
	if !ok || call.Tool != DeclarativeActionToolName {
		return false
	}
	outcome, ok := extractDeclarativeRepairOutcome(last.LLMObservation)
	if !ok {
		outcome, ok = extractDeclarativeRepairOutcome(last.Observation)
	}
	if !ok || outcome == nil {
		return false
	}
	bumped := false
	if outcome.ArgsRepaired {
		rc.RepairCounters.ArgsRepair++
		bumped = true
	}
	if outcome.MultiAction {
		rc.RepairCounters.MultiAction++
		bumped = true
	}
	if outcome.FinishRepair {
		rc.RepairCounters.FinishRepair++
		bumped = true
	}
	return bumped
}

// extractDeclarativeRepairOutcome attempts to read a structured
// `repair_outcome` field from the meta-tool's observation. The
// observation may arrive in three shapes (matching the
// `extractDiscoveredNames` tolerance pattern):
//
//  1. A `builtin.DeclarativeActionOut` struct (the inproc executor
//     path before D-026 heavy-content projection — typical case).
//  2. A `map[string]any` (post-projection generic representation).
//  3. A `json.RawMessage` / `[]byte` / `string` (when the dispatcher
//     serialised before storing — D-026 truncation path also lands
//     here).
//
// Returns (nil, true) when the observation parsed cleanly but carried
// NO repair_outcome field — a clean dispatch resets nothing here
// (the end-of-step updateRepairCounters does that). Returns
// (nil, false) when the observation cannot be parsed.
func extractDeclarativeRepairOutcome(obs any) (*declarativeRepairFields, bool) {
	if obs == nil {
		return nil, false
	}
	switch v := obs.(type) {
	case map[string]any:
		return extractDeclarativeOutcomeFromMap(v), true
	case json.RawMessage:
		return extractDeclarativeOutcomeFromBytes(v), true
	case []byte:
		return extractDeclarativeOutcomeFromBytes(v), true
	case string:
		return extractDeclarativeOutcomeFromBytes([]byte(v)), true
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		return extractDeclarativeOutcomeFromBytes(raw), true
	}
}

func extractDeclarativeOutcomeFromBytes(raw []byte) *declarativeRepairFields {
	if len(raw) == 0 {
		return nil
	}
	var shaped declarativeObservation
	if err := json.Unmarshal(raw, &shaped); err != nil {
		return nil
	}
	if shaped.RepairOutcome == nil {
		return nil
	}
	if !shaped.RepairOutcome.ArgsRepaired &&
		!shaped.RepairOutcome.MultiAction &&
		!shaped.RepairOutcome.FinishRepair {
		return nil
	}
	return shaped.RepairOutcome
}

// isDeclarativeActionDispatch reports whether the decision dispatches
// the `declarative_action` meta-tool. Used by `react.Next` to decide
// whether to suppress the end-of-step counter reset (a declarative_action
// dispatch defers reset to the next step's `applyDeclarativeOutcome`
// read of the trajectory). A CallParallel whose first branch dispatches
// declarative_action is treated the same way.
func isDeclarativeActionDispatch(d planner.Decision) bool {
	switch v := d.(type) {
	case planner.CallTool:
		return v.Tool == DeclarativeActionToolName
	case planner.CallParallel:
		for _, br := range v.Branches {
			if br.Tool == DeclarativeActionToolName {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func extractDeclarativeOutcomeFromMap(m map[string]any) *declarativeRepairFields {
	raw, ok := m["repair_outcome"]
	if !ok || raw == nil {
		return nil
	}
	enc, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	// `enc` is the inner repair_outcome shape (the value side of the
	// map entry). Decode it directly into `declarativeRepairFields`
	// rather than the outer-shape `declarativeObservation`. The bytes
	// path handles the outer shape; the map path already extracted
	// the inner value.
	var inner declarativeRepairFields
	if err := json.Unmarshal(enc, &inner); err != nil {
		return nil
	}
	if !inner.ArgsRepaired && !inner.MultiAction && !inner.FinishRepair {
		return nil
	}
	return &inner
}
