package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestCountToolInvocations_CallTool_CountsOne pins the simplest case: a
// single CallTool step contributes exactly one invocation.
func TestCountToolInvocations_CallTool_CountsOne(t *testing.T) {
	t.Parallel()
	tr := &Trajectory{Steps: []Step{
		{Action: CallTool{Tool: "clock.now"}},
	}}
	if got := CountToolInvocations(tr); got != 1 {
		t.Errorf("CountToolInvocations = %d, want 1", got)
	}
}

// TestCountToolInvocations_CallParallel_CountsBranches is the D-274
// regression: a CallParallel step with N branches must count as N
// invocations, not 1 (the pre-274 len(Steps) reading undercounted this).
func TestCountToolInvocations_CallParallel_CountsBranches(t *testing.T) {
	t.Parallel()
	tr := &Trajectory{Steps: []Step{
		{Action: CallParallel{Branches: []CallTool{
			{Tool: "a"}, {Tool: "b"}, {Tool: "c"}, {Tool: "d"},
		}}},
	}}
	if got := CountToolInvocations(tr); got != 4 {
		t.Errorf("CountToolInvocations = %d, want 4 (one per branch)", got)
	}
}

// TestCountToolInvocations_SpawnAndAwait_ExcludedFromCount is the other
// half of the D-274 regression: SpawnTask and AwaitTask steps are runtime
// decisions, never tool dispatches, so they must NOT inflate the count
// (the pre-274 len(Steps) reading overcounted these).
func TestCountToolInvocations_SpawnAndAwait_ExcludedFromCount(t *testing.T) {
	t.Parallel()
	tr := &Trajectory{Steps: []Step{
		{Action: SpawnTask{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "q"}}},
		{Action: AwaitTask{TaskID: "t-1"}},
	}}
	if got := CountToolInvocations(tr); got != 0 {
		t.Errorf("CountToolInvocations = %d, want 0 (SpawnTask/AwaitTask are not tool invocations)", got)
	}
}

// TestCountToolInvocations_Mixed_SumsCorrectly exercises a realistic
// trajectory mixing every shape: 1 (CallTool) + 3 (CallParallel) + 0
// (SpawnTask) + 0 (AwaitTask) + 1 (CallTool) == 5.
func TestCountToolInvocations_Mixed_SumsCorrectly(t *testing.T) {
	t.Parallel()
	tr := &Trajectory{Steps: []Step{
		{Action: CallTool{Tool: "clock.now"}},
		{Action: CallParallel{Branches: []CallTool{
			{Tool: "a"}, {Tool: "b"}, {Tool: "c"},
		}}},
		{Action: SpawnTask{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "q"}}},
		{Action: AwaitTask{TaskID: "t-1"}},
		{Action: CallTool{Tool: "text.echo"}},
	}}
	if got := CountToolInvocations(tr); got != 5 {
		t.Errorf("CountToolInvocations = %d, want 5", got)
	}
}

// TestCountToolInvocations_NilTrajectory_CountsZero — a nil Trajectory
// (a run that never populated one) counts as zero, never a panic.
func TestCountToolInvocations_NilTrajectory_CountsZero(t *testing.T) {
	t.Parallel()
	if got := CountToolInvocations(nil); got != 0 {
		t.Errorf("CountToolInvocations(nil) = %d, want 0", got)
	}
}

// TestCountToolInvocations_EmptySteps_CountsZero — a Trajectory with no
// steps (e.g. Finish on the very first turn) counts as zero.
func TestCountToolInvocations_EmptySteps_CountsZero(t *testing.T) {
	t.Parallel()
	if got := CountToolInvocations(&Trajectory{}); got != 0 {
		t.Errorf("CountToolInvocations(empty) = %d, want 0", got)
	}
}

// TestDecisionInvocationCount_UnknownShape_CountsZero — a step Action
// that is neither a Decision nor nil (e.g. a bare map[string]any, the
// shape a JSON-deserialised-then-not-retyped trajectory step carries)
// counts as zero rather than panicking on the type switch. Typed-nil
// pointers count zero too — never a nil-dereference panic.
func TestDecisionInvocationCount_UnknownShape_CountsZero(t *testing.T) {
	t.Parallel()
	cases := []any{
		nil,
		map[string]any{"tool": "clock.now"},
		Finish{Reason: FinishGoal},
		RequestPause{Reason: PauseApprovalRequired},
		(*CallTool)(nil),
		(*CallParallel)(nil),
		(*Batch)(nil),
	}
	for _, c := range cases {
		if got := DecisionInvocationCount(c); got != 0 {
			t.Errorf("DecisionInvocationCount(%#v) = %d, want 0", c, got)
		}
	}
}

// TestDecisionInvocationCount_Batch_CountsToolsOnly — a Batch counts as
// len(Tools); its Spawns contribute zero (a spawn is never a tool
// invocation), matching SpawnTask's existing zero-count rule. Both the
// value and pointer cases are covered; a typed-nil *Batch counts zero.
func TestDecisionInvocationCount_Batch_CountsToolsOnly(t *testing.T) {
	t.Parallel()
	b := Batch{
		Tools: []CallTool{{Tool: "a"}, {Tool: "b"}},
		Spawns: []SpawnTask{
			{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "x"}},
			{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "y"}},
			{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "z"}},
		},
	}
	if got := DecisionInvocationCount(b); got != 2 {
		t.Errorf("DecisionInvocationCount(Batch) = %d, want 2 (Tools only, Spawns excluded)", got)
	}
	if got := DecisionInvocationCount(&b); got != 2 {
		t.Errorf("DecisionInvocationCount(*Batch) = %d, want 2", got)
	}
	// Spawns-only Batch counts zero tool invocations.
	spawnsOnly := Batch{Spawns: []SpawnTask{
		{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "x"}},
		{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "y"}},
	}}
	if got := DecisionInvocationCount(spawnsOnly); got != 0 {
		t.Errorf("DecisionInvocationCount(spawns-only Batch) = %d, want 0", got)
	}
}

// TestCountToolInvocations_BatchStep_CountsTools — a Batch step in a
// trajectory contributes len(Tools) to the aggregate count, alongside
// the other shapes.
func TestCountToolInvocations_BatchStep_CountsTools(t *testing.T) {
	t.Parallel()
	tr := &Trajectory{Steps: []Step{
		{Action: CallTool{Tool: "clock.now"}}, // 1
		{Action: Batch{ // 2 tools + 2 spawns → 2
			Tools:  []CallTool{{Tool: "a"}, {Tool: "b"}},
			Spawns: []SpawnTask{{Kind: tasks.KindBackground}, {Kind: tasks.KindBackground}},
		}},
	}}
	if got := CountToolInvocations(tr); got != 3 {
		t.Errorf("CountToolInvocations = %d, want 3 (1 CallTool + 2 Batch tools)", got)
	}
}

// TestSerialize_Batch_RoundTripsByteStable — a Batch-carrying step
// serialises without error and its canonical (rehydrated) form
// round-trips byte-stable (the D-049 contract). Batch is a plain struct
// of serialisable fields, so it flows through the reflective walker +
// json.Marshal without special handling, and every field survives the
// round trip.
//
// The byte-stable fixed point is asserted on the canonical map-shaped
// Action, exactly as the subpackage's TestRoundTrip_ByteStable does:
// the FIRST serialize encodes the Go struct in field-declaration order,
// while every serialize AFTER a Deserialize encodes the rehydrated
// map[string]any with json's sorted keys — so the byte-stable invariant
// is Serialize(Deserialize(x)) == Serialize(Deserialize(Serialize(
// Deserialize(x)))). This mirrors production: a rehydrated trajectory
// always carries map-shaped Actions.
func TestSerialize_Batch_RoundTripsByteStable(t *testing.T) {
	t.Parallel()
	tr := &Trajectory{Steps: []Step{
		{Action: Batch{
			Tools: []CallTool{
				{Tool: "search", Args: json.RawMessage(`{"q":"foo"}`), CallID: "call_1"},
			},
			Spawns: []SpawnTask{
				{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "dig"}, CallID: "call_2"},
			},
		}},
	}}
	first, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize #1: %v", err)
	}
	back, err := trajectory.Deserialize(first)
	if err != nil {
		t.Fatalf("Deserialize #1: %v", err)
	}
	// Content preserved through the round trip: the Batch's tool and
	// spawn call-ids survive into the rehydrated map tree.
	assertBatchRoundTripContent(t, back)

	canonical, err := back.Serialize()
	if err != nil {
		t.Fatalf("Serialize #2 (canonical): %v", err)
	}
	back2, err := trajectory.Deserialize(canonical)
	if err != nil {
		t.Fatalf("Deserialize #2: %v", err)
	}
	fixedPoint, err := back2.Serialize()
	if err != nil {
		t.Fatalf("Serialize #3: %v", err)
	}
	if !bytes.Equal(canonical, fixedPoint) {
		t.Fatalf("canonical form not byte-stable:\n#2 %s\n#3 %s", canonical, fixedPoint)
	}
}

// assertBatchRoundTripContent walks the rehydrated (map-shaped) Action
// of the single Batch step and asserts the tool + spawn call-ids
// survived serialisation.
func assertBatchRoundTripContent(t *testing.T, tr *Trajectory) {
	t.Helper()
	if len(tr.Steps) != 1 {
		t.Fatalf("rehydrated Steps len = %d, want 1", len(tr.Steps))
	}
	action, ok := tr.Steps[0].Action.(map[string]any)
	if !ok {
		t.Fatalf("rehydrated Action = %T, want map[string]any", tr.Steps[0].Action)
	}
	tools, ok := action["Tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("rehydrated Tools = %#v, want 1 entry", action["Tools"])
	}
	spawns, ok := action["Spawns"].([]any)
	if !ok || len(spawns) != 1 {
		t.Fatalf("rehydrated Spawns = %#v, want 1 entry", action["Spawns"])
	}
	if cid := tools[0].(map[string]any)["CallID"]; cid != "call_1" {
		t.Errorf("rehydrated tool CallID = %v, want call_1", cid)
	}
	if cid := spawns[0].(map[string]any)["CallID"]; cid != "call_2" {
		t.Errorf("rehydrated spawn CallID = %v, want call_2", cid)
	}
}

// TestSerialize_Batch_FailsLoudOnUnserializable — a non-serialisable
// value co-located on a Batch-carrying step surfaces ErrUnserializable
// with a field-path locator rather than a silent drop.
//
// NOTE (plan deviation, justified): the phase plan's Test-plan text
// describes forcing the failure via "an unserializable Args payload on
// one Tools branch". CallTool.Args is json.RawMessage (a []byte), which
// is statically serialisable — a func/chan cannot live there, and by
// extension a fully-typed Batch is ALWAYS serialisable by construction
// (all its fields are concrete serialisable types). That is a desirable
// property, not a gap. To still exercise the §17.3 fail-loud failure
// mode on a Batch step, the non-serialisable value is placed on the same
// step's Observation (an `any` the reflective walker reaches); the
// walker names the offending path. The contract proven is identical:
// Serialize fails loud with ErrUnserializable and a precise locator.
func TestSerialize_Batch_FailsLoudOnUnserializable(t *testing.T) {
	t.Parallel()
	tr := &Trajectory{Steps: []Step{
		{
			Action: Batch{
				Tools:  []CallTool{{Tool: "search", Args: json.RawMessage(`{}`)}},
				Spawns: []SpawnTask{{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "q"}}},
			},
			Observation: map[string]any{"callback": func() {}},
		},
	}}
	out, err := tr.Serialize()
	if out != nil {
		t.Fatalf("Serialize returned non-nil bytes on non-encodable input — fail-loud violated")
	}
	var unserr ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v, want ErrUnserializable", err)
	}
	if unserr.Field == "" {
		t.Fatalf("ErrUnserializable.Field is empty — locator missing")
	}
}
