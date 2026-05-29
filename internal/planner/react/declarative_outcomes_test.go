package react

import (
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

// TestDeclarativeActionToolName_MirrorsBuiltinAuthority pins the
// reserved-name constant against the builtin's canonical registration
// name. The constant lives in two places (here + the registry's map
// key); a drift here is a §13 forbidden-pattern surface.
func TestDeclarativeActionToolName_MirrorsBuiltinAuthority(t *testing.T) {
	t.Parallel()
	// The builtin registry uses "declarative_action" as the map key.
	// `builtin.KnownNames` returns it; we assert membership rather than
	// equality so adding adjacent builtins doesn't break this test.
	found := false
	for _, name := range builtin.KnownNames() {
		if name == DeclarativeActionToolName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("react.DeclarativeActionToolName=%q is not registered in builtin.KnownNames=%v",
			DeclarativeActionToolName, builtin.KnownNames())
	}
}

// TestDeclarativeRepairFields_MirrorBuiltinShape pins the JSON tags
// of the planner's projection struct against the builtin's authoritative
// struct. Without this, a rename / retag in `builtin` silently breaks
// the planner's observation parsing.
func TestDeclarativeRepairFields_MirrorBuiltinShape(t *testing.T) {
	t.Parallel()
	// Encode an authoritative builtin shape; decode through the
	// planner's projection. Every field must round-trip.
	enc, err := json.Marshal(struct {
		RepairOutcome builtin.DeclarativeRepairOutcome `json:"repair_outcome"`
	}{
		RepairOutcome: builtin.DeclarativeRepairOutcome{
			ArgsRepaired: true,
			MultiAction:  true,
			FinishRepair: true,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got declarativeObservation
	if err := json.Unmarshal(enc, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RepairOutcome == nil {
		t.Fatal("RepairOutcome nil after round-trip")
	}
	if !got.RepairOutcome.ArgsRepaired || !got.RepairOutcome.MultiAction || !got.RepairOutcome.FinishRepair {
		t.Fatalf("round-trip lost fields: %+v", got.RepairOutcome)
	}
}

// TestApplyDeclarativeOutcome_NoTrajectory — when the run has no
// trajectory or no steps, applyDeclarativeOutcome is a no-op.
func TestApplyDeclarativeOutcome_NoTrajectory(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		RepairCounters: &planner.RepairCounters{},
	}
	if applyDeclarativeOutcome(rc) {
		t.Errorf("bumped on empty rc")
	}
	if got := *rc.RepairCounters; got != (planner.RepairCounters{}) {
		t.Errorf("counters mutated: %+v", got)
	}
}

// TestApplyDeclarativeOutcome_NilCounters — when the runtime opted
// out of counters, the function is a no-op even on a populated
// trajectory. Mirrors updateRepairCounters semantics.
func TestApplyDeclarativeOutcome_NilCounters(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Trajectory: &planner.Trajectory{Steps: []planner.Step{{
			Action: planner.CallTool{Tool: DeclarativeActionToolName},
			Observation: builtin.DeclarativeActionOut{
				RepairOutcome: &builtin.DeclarativeRepairOutcome{ArgsRepaired: true},
			},
		}}},
	}
	if applyDeclarativeOutcome(rc) {
		t.Errorf("bumped despite nil counters")
	}
}

// TestApplyDeclarativeOutcome_BumpsArgsRepair — the canonical case.
func TestApplyDeclarativeOutcome_BumpsArgsRepair(t *testing.T) {
	t.Parallel()
	c := &planner.RepairCounters{}
	rc := planner.RunContext{
		RepairCounters: c,
		Trajectory: &planner.Trajectory{Steps: []planner.Step{{
			Action: planner.CallTool{Tool: DeclarativeActionToolName},
			Observation: builtin.DeclarativeActionOut{
				RepairOutcome: &builtin.DeclarativeRepairOutcome{ArgsRepaired: true},
			},
		}}},
	}
	if !applyDeclarativeOutcome(rc) {
		t.Fatal("did not bump")
	}
	if c.ArgsRepair != 1 {
		t.Errorf("ArgsRepair = %d, want 1", c.ArgsRepair)
	}
}

// TestApplyDeclarativeOutcome_BumpsMultiAction — pinned independently
// from ArgsRepair so the planner's escalating-guidance routing keeps
// the right `Counter` payload on the bus.
func TestApplyDeclarativeOutcome_BumpsMultiAction(t *testing.T) {
	t.Parallel()
	c := &planner.RepairCounters{}
	rc := planner.RunContext{
		RepairCounters: c,
		Trajectory: &planner.Trajectory{Steps: []planner.Step{{
			Action: planner.CallTool{Tool: DeclarativeActionToolName},
			Observation: builtin.DeclarativeActionOut{
				RepairOutcome: &builtin.DeclarativeRepairOutcome{MultiAction: true},
			},
		}}},
	}
	if !applyDeclarativeOutcome(rc) {
		t.Fatal("did not bump")
	}
	if c.MultiAction != 1 {
		t.Errorf("MultiAction = %d, want 1", c.MultiAction)
	}
}

// TestApplyDeclarativeOutcome_BumpsFinishRepair — the `_finish`
// reserved-name path.
func TestApplyDeclarativeOutcome_BumpsFinishRepair(t *testing.T) {
	t.Parallel()
	c := &planner.RepairCounters{}
	rc := planner.RunContext{
		RepairCounters: c,
		Trajectory: &planner.Trajectory{Steps: []planner.Step{{
			Action: planner.CallTool{Tool: DeclarativeActionToolName},
			Observation: builtin.DeclarativeActionOut{
				RepairOutcome: &builtin.DeclarativeRepairOutcome{FinishRepair: true},
			},
		}}},
	}
	if !applyDeclarativeOutcome(rc) {
		t.Fatal("did not bump")
	}
	if c.FinishRepair != 1 {
		t.Errorf("FinishRepair = %d, want 1", c.FinishRepair)
	}
}

// TestApplyDeclarativeOutcome_CleanDispatchNoBump — a successful
// declarative_action dispatch (no repair_outcome) leaves counters
// alone. The end-of-step updateRepairCounters handles reset semantics
// per the existing flow.
func TestApplyDeclarativeOutcome_CleanDispatchNoBump(t *testing.T) {
	t.Parallel()
	c := &planner.RepairCounters{ArgsRepair: 2, MultiAction: 1, FinishRepair: 3}
	rc := planner.RunContext{
		RepairCounters: c,
		Trajectory: &planner.Trajectory{Steps: []planner.Step{{
			Action: planner.CallTool{Tool: DeclarativeActionToolName},
			Observation: builtin.DeclarativeActionOut{
				Dispatched: true,
				Tool:       "text.echo",
			},
		}}},
	}
	if applyDeclarativeOutcome(rc) {
		t.Errorf("bumped on clean dispatch")
	}
	want := planner.RepairCounters{ArgsRepair: 2, MultiAction: 1, FinishRepair: 3}
	if *c != want {
		t.Errorf("counters mutated: got %+v, want %+v", *c, want)
	}
}

// TestApplyDeclarativeOutcome_NonDeclarativeStepNoOp — when the last
// trajectory step was a normal tool call (not declarative_action), no
// bump fires regardless of the observation shape.
func TestApplyDeclarativeOutcome_NonDeclarativeStepNoOp(t *testing.T) {
	t.Parallel()
	c := &planner.RepairCounters{}
	rc := planner.RunContext{
		RepairCounters: c,
		Trajectory: &planner.Trajectory{Steps: []planner.Step{{
			Action: planner.CallTool{Tool: "text.echo"},
			// Observation carries a repair-shaped object — but the
			// action is NOT declarative_action so we ignore it.
			Observation: map[string]any{
				"repair_outcome": map[string]any{"args_repaired": true},
			},
		}}},
	}
	if applyDeclarativeOutcome(rc) {
		t.Errorf("bumped on non-declarative step")
	}
}

// TestApplyDeclarativeOutcome_BytesObservation — when the dispatcher
// serialised the observation to bytes (D-026 heavy-content projection
// path), the planner still extracts the repair outcome. Mirrors the
// extractDiscoveredNames tolerance pattern.
func TestApplyDeclarativeOutcome_BytesObservation(t *testing.T) {
	t.Parallel()
	c := &planner.RepairCounters{}
	rc := planner.RunContext{
		RepairCounters: c,
		Trajectory: &planner.Trajectory{Steps: []planner.Step{{
			Action:      planner.CallTool{Tool: DeclarativeActionToolName},
			Observation: json.RawMessage(`{"dispatched":false,"repair_outcome":{"args_repaired":true}}`),
		}}},
	}
	if !applyDeclarativeOutcome(rc) {
		t.Fatal("did not bump on bytes observation")
	}
	if c.ArgsRepair != 1 {
		t.Errorf("ArgsRepair = %d, want 1", c.ArgsRepair)
	}
}

// TestApplyDeclarativeOutcome_PrefersLLMObservation — when both
// Observation and LLMObservation are set, the LLMObservation wins
// (matches the trajectory walker's heavy-content discipline; the
// LLMObservation is the small projection the planner already prefers).
func TestApplyDeclarativeOutcome_PrefersLLMObservation(t *testing.T) {
	t.Parallel()
	c := &planner.RepairCounters{}
	rc := planner.RunContext{
		RepairCounters: c,
		Trajectory: &planner.Trajectory{Steps: []planner.Step{{
			Action: planner.CallTool{Tool: DeclarativeActionToolName},
			Observation: map[string]any{
				"repair_outcome": map[string]any{"args_repaired": true},
			},
			LLMObservation: map[string]any{
				"repair_outcome": map[string]any{"multi_action": true},
			},
		}}},
	}
	if !applyDeclarativeOutcome(rc) {
		t.Fatal("did not bump")
	}
	if c.MultiAction != 1 {
		t.Errorf("MultiAction = %d, want 1 (LLMObservation should win)", c.MultiAction)
	}
	if c.ArgsRepair != 0 {
		t.Errorf("ArgsRepair = %d, want 0 (Observation should not win)", c.ArgsRepair)
	}
}

// TestIsDeclarativeActionDispatch_CoverDecisionShapes asserts the
// helper's coverage of every Decision sum entry.
func TestIsDeclarativeActionDispatch_CoverDecisionShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dec  planner.Decision
		want bool
	}{
		{"call tool match", planner.CallTool{Tool: DeclarativeActionToolName}, true},
		{"call tool non-match", planner.CallTool{Tool: "text.echo"}, false},
		{"call parallel with match in first branch", planner.CallParallel{Branches: []planner.CallTool{{Tool: DeclarativeActionToolName}}}, true},
		{"call parallel with match in second branch", planner.CallParallel{Branches: []planner.CallTool{{Tool: "x"}, {Tool: DeclarativeActionToolName}}}, true},
		{"call parallel no match", planner.CallParallel{Branches: []planner.CallTool{{Tool: "x"}}}, false},
		{"finish", planner.Finish{Reason: planner.FinishGoal}, false},
		{"spawn", planner.SpawnTask{}, false},
		{"await", planner.AwaitTask{}, false},
	}
	for _, tc := range cases {
		if got := isDeclarativeActionDispatch(tc.dec); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
