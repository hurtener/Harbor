package steering

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// invalidDecisionErr shapes a wrapped planner.ErrInvalidDecision that
// mirrors the react projector's structural-rejection errors (malformed
// _spawn_task args JSON, retain-turn-in-batch, finish co-occurrence).
// The RunLoop treats any error that wraps planner.ErrInvalidDecision as a
// REPAIRABLE step observation.
func invalidDecisionErr(detail string) error {
	return fmt.Errorf("%w: react._spawn_task %s", planner.ErrInvalidDecision, detail)
}

// nativeRepairBudget is the explicit consecutive-invalid budget these
// tests pin (equal to DefaultMaxConsecutiveInvalidDecisions).
const nativeRepairBudget = 2

// nativeRepairSpec builds a RunSpec carrying a fresh Trajectory (so the
// fed-back rejection observation has somewhere to land) and the pinned
// consecutive-invalid budget.
func nativeRepairSpec(p planner.Planner, tr *planner.Trajectory) RunSpec {
	return RunSpec{
		Planner: p,
		Base: planner.RunContext{
			Quadruple:  runA,
			Goal:       "reach the goal",
			Trajectory: tr,
		},
		MaxSteps:                       32,
		MaxConsecutiveInvalidDecisions: nativeRepairBudget,
	}
}

// TestRun_InvalidDecision_FedBackAsObservation_RunSurvives asserts the F1
// fix: a native-path projector structural rejection (a wrapped
// planner.ErrInvalidDecision returned from Planner.Next) is NOT fatal —
// the RunLoop records it as a step observation and re-enters the planner,
// which re-plans and finishes. The run survives.
func TestRun_InvalidDecision_FedBackAsObservation_RunSurvives(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	tr := &planner.Trajectory{}
	p := &scriptedPlanner{script: []scriptStep{
		{err: invalidDecisionErr("args malformed JSON: unexpected end of JSON input")},
		{dec: planner.Finish{Reason: planner.FinishGoal, Payload: "recovered"}},
	}}

	fin, err := rl.Run(context.Background(), nativeRepairSpec(p, tr))
	if err != nil {
		t.Fatalf("Run: %v (the projector rejection must be repaired, not fatal)", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %q, want goal (the re-plan step's finish)", fin.Reason)
	}
	if p.stepCount() != 2 {
		t.Fatalf("planner step count = %d, want 2 (rejection + re-plan)", p.stepCount())
	}
	// The rejection was fed back as a trajectory step carrying the error,
	// so the react prompt builder surfaces it on the re-plan turn.
	if len(tr.Steps) != 1 {
		t.Fatalf("trajectory steps = %d, want 1 (the rejection observation)", len(tr.Steps))
	}
	if tr.Steps[0].Action != nil {
		t.Errorf("rejection step Action = %v, want nil", tr.Steps[0].Action)
	}
	if !strings.Contains(tr.Steps[0].Error, "malformed JSON") {
		t.Errorf("rejection step Error = %q, want it to carry the projector violation", tr.Steps[0].Error)
	}
}

// TestRun_InvalidDecision_BudgetExhausted_FailsLoud asserts the bound: a
// model that emits a structural rejection on EVERY step eventually
// terminates the run loudly (never an infinite re-plan loop). The
// surfaced error still wraps planner.ErrInvalidDecision AND names the
// budget exhaustion.
func TestRun_InvalidDecision_BudgetExhausted_FailsLoud(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	tr := &planner.Trajectory{}
	// Every step returns the same structural rejection.
	p := &scriptedPlanner{script: []scriptStep{
		{err: invalidDecisionErr("args malformed JSON: unexpected end of JSON input")},
		{err: invalidDecisionErr("args malformed JSON: unexpected end of JSON input")},
		{err: invalidDecisionErr("args malformed JSON: unexpected end of JSON input")},
		{err: invalidDecisionErr("args malformed JSON: unexpected end of JSON input")},
	}}

	_, err := rl.Run(context.Background(), nativeRepairSpec(p, tr))
	if err == nil {
		t.Fatal("Run: expected a loud failure after the repair budget is exhausted, got nil")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("Run err = %v, want errors.Is planner.ErrInvalidDecision", err)
	}
	if !strings.Contains(err.Error(), "repair budget exhausted") {
		t.Errorf("Run err = %q, want it to name the budget exhaustion", err.Error())
	}
	// Budget of 2 with the >= boundary terminates on the 2nd consecutive
	// rejection: the planner is entered exactly twice, never looping to
	// MaxSteps.
	if p.stepCount() != 2 {
		t.Fatalf("planner step count = %d, want 2 (bounded, not MaxSteps)", p.stepCount())
	}
	// The fail-loudly observability surface fired.
	if n := bus.countType(planner.EventTypePlannerRepairExhausted); n != 1 {
		t.Errorf("planner.repair_exhausted emitted %d times, want 1", n)
	}
}

// TestRun_NonInvalidDecisionError_StaysFatal asserts a genuinely-fatal
// Planner.Next error (one that does NOT wrap planner.ErrInvalidDecision —
// e.g. an LLM transport error, ctx cancellation, missing identity) stays
// fatal on the first occurrence: it is NOT model-repairable, so no
// observation is fed back and the run aborts immediately.
func TestRun_NonInvalidDecisionError_StaysFatal(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	tr := &planner.Trajectory{}
	transportErr := errors.New("llm transport: connection reset")
	p := &scriptedPlanner{script: []scriptStep{
		{err: transportErr},
		{dec: planner.Finish{Reason: planner.FinishGoal}}, // must never be reached
	}}

	_, err := rl.Run(context.Background(), nativeRepairSpec(p, tr))
	if err == nil {
		t.Fatal("Run: a non-repairable error must be fatal, got nil")
	}
	if !errors.Is(err, transportErr) {
		t.Fatalf("Run err = %v, want it to wrap the transport error verbatim", err)
	}
	if p.stepCount() != 1 {
		t.Fatalf("planner step count = %d, want 1 (fatal on first, no re-plan)", p.stepCount())
	}
	if len(tr.Steps) != 0 {
		t.Errorf("trajectory steps = %d, want 0 (no observation fed back for a fatal error)", len(tr.Steps))
	}
}

// TestRun_InvalidDecision_CounterResetsOnValidDecision asserts the
// consecutive-failure budget counts only CONSECUTIVE rejections: a valid
// decision between rejections resets it. Two rejections each preceded by
// a valid decision must NOT exhaust a budget of 2 (without the reset, the
// 2nd rejection would trip the budget and abort the run).
func TestRun_InvalidDecision_CounterResetsOnValidDecision(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	tr := &planner.Trajectory{}
	// A valid CallTool with no ToolExecutor appends an empty-observation
	// step and continues — the run keeps going, resetting the budget.
	p := &scriptedPlanner{script: []scriptStep{
		{err: invalidDecisionErr("args malformed JSON")},
		{dec: planner.CallTool{Tool: "noop"}},
		{err: invalidDecisionErr("args malformed JSON")},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.Finish{Reason: planner.FinishGoal, Payload: "done"}},
	}}

	fin, err := rl.Run(context.Background(), nativeRepairSpec(p, tr))
	if err != nil {
		t.Fatalf("Run: %v (interleaved valid decisions must reset the budget)", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %q, want goal", fin.Reason)
	}
	if p.stepCount() != 5 {
		t.Fatalf("planner step count = %d, want 5 (2 rejections, 2 tool steps, 1 finish)", p.stepCount())
	}
}
