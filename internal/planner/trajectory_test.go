package planner

import (
	"testing"

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
// counts as zero rather than panicking on the type switch.
func TestDecisionInvocationCount_UnknownShape_CountsZero(t *testing.T) {
	t.Parallel()
	cases := []any{
		nil,
		map[string]any{"tool": "clock.now"},
		Finish{Reason: FinishGoal},
		RequestPause{Reason: PauseApprovalRequired},
	}
	for _, c := range cases {
		if got := DecisionInvocationCount(c); got != 0 {
			t.Errorf("DecisionInvocationCount(%#v) = %d, want 0", c, got)
		}
	}
}
