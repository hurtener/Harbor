package deterministic_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/tasks"
)

// validQuadruple returns a populated identity quadruple suitable for
// the planner's identity-mandatory pre-check.
func validQuadruple() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "tenant-test",
			UserID:    "user-test",
			SessionID: "session-test",
		},
		RunID: "run-test",
	}
}

// staticStep claims every call with a pre-built decision. Used for
// fast walker tests.
type staticStep struct {
	decision planner.Decision
	claim    bool
	err      error
}

func (s *staticStep) Decide(_ context.Context, _ planner.RunContext) (planner.Decision, bool, error) {
	return s.decision, s.claim, s.err
}

func TestNew_RejectsEmptySteps(t *testing.T) {
	_, err := deterministic.NewDeterministicPlanner()
	if err == nil {
		t.Fatal("expected error on empty step set, got nil")
	}
	if !errors.Is(err, planner.ErrInvalidConfig) {
		t.Errorf("err = %v, want errors.Is planner.ErrInvalidConfig", err)
	}
}

func TestNew_RejectsNilStep(t *testing.T) {
	_, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(nil),
	)
	if err == nil {
		t.Fatal("expected error on nil step entry, got nil")
	}
	if !errors.Is(err, planner.ErrInvalidConfig) {
		t.Errorf("err = %v, want errors.Is planner.ErrInvalidConfig", err)
	}
}

func TestNew_RejectsGroupAwareStepWithoutRegistry(t *testing.T) {
	_, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.SpawnAndAwaitStep{}),
	)
	if err == nil {
		t.Fatal("expected error on group-aware step without registry, got nil")
	}
	if !errors.Is(err, planner.ErrInvalidConfig) {
		t.Errorf("err = %v, want errors.Is planner.ErrInvalidConfig", err)
	}
}

func TestNew_AcceptsBasicFinishStep(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{
			Reason: planner.FinishGoal,
		}),
	)
	if err != nil {
		t.Fatalf("unexpected construction error: %v", err)
	}
	if p == nil {
		t.Fatal("constructor returned nil planner without error")
	}
	if p.Name() != deterministic.DefaultName {
		t.Errorf("Name() = %q, want %q", p.Name(), deterministic.DefaultName)
	}
}

func TestNew_WithName(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithName("workflow-A"),
		deterministic.WithSteps(&deterministic.FinishStep{Reason: planner.FinishGoal}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Name() != "workflow-A" {
		t.Errorf("Name() = %q, want %q", p.Name(), "workflow-A")
	}
}

func TestNext_RejectsMissingIdentity(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{Reason: planner.FinishGoal}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := []struct {
		name string
		q    identity.Quadruple
	}{
		{"empty", identity.Quadruple{}},
		{"no-tenant", identity.Quadruple{Identity: identity.Identity{UserID: "u", SessionID: "s"}, RunID: "r"}},
		{"no-user", identity.Quadruple{Identity: identity.Identity{TenantID: "t", SessionID: "s"}, RunID: "r"}},
		{"no-session", identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}, RunID: "r"}},
		{"no-run", identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := planner.RunContext{Quadruple: tc.q}
			_, err := p.Next(context.Background(), rc)
			if err == nil {
				t.Fatal("expected ErrIdentityRequired, got nil")
			}
			if !errors.Is(err, planner.ErrIdentityRequired) {
				t.Errorf("err = %v, want errors.Is planner.ErrIdentityRequired", err)
			}
		})
	}
}

func TestNext_HonoursCtxCancel(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{Reason: planner.FinishGoal}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Next(ctx, planner.RunContext{Quadruple: validQuadruple()})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestNext_ObservesSteeringCancellation(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{Reason: planner.FinishGoal}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rc := planner.RunContext{
		Quadruple: validQuadruple(),
		Control: planner.ControlSignals{
			Cancelled: true,
		},
	}
	dec, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("dec = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishCancelled {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishCancelled)
	}
	if got, _ := fin.Metadata["steering"].(string); got != "cancelled" {
		t.Errorf("Metadata[steering] = %v, want %q", fin.Metadata["steering"], "cancelled")
	}
}

func TestNext_WalksStepsInOrder(t *testing.T) {
	want := planner.CallTool{Tool: "alpha", Args: json.RawMessage(`{}`)}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(
			&staticStep{claim: false},
			&staticStep{claim: true, decision: want},
			// Sentinel: a third step that would claim if reached.
			&staticStep{claim: true, decision: planner.Finish{Reason: planner.FinishGoal}},
		),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec, err := p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := dec.(planner.CallTool)
	if !ok {
		t.Fatalf("dec = %T, want planner.CallTool", dec)
	}
	if got.Tool != "alpha" {
		t.Errorf("Tool = %q, want %q (a later step was reached — walker order broken)", got.Tool, "alpha")
	}
}

func TestNext_NoMatchReturnsFinishNoPath(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(
			&staticStep{claim: false},
			&staticStep{claim: false},
		),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec, err := p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("dec = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["deterministic"].(string); got != "no_step_matched" {
		t.Errorf("Metadata[deterministic] = %v, want %q", fin.Metadata["deterministic"], "no_step_matched")
	}
}

func TestNext_StepErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(
			&staticStep{claim: false},
			&staticStep{err: sentinel},
		),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected step error, got nil")
	}
	if !errors.Is(err, planner.ErrDeterministicStep) {
		t.Errorf("err = %v, want errors.Is planner.ErrDeterministicStep", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel wrapped via errors.Is", err)
	}
}

func TestNext_StepReturnsClaimWithNilDecision(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(
			&staticStep{claim: true, decision: nil},
		),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected step error, got nil")
	}
	if !errors.Is(err, planner.ErrDeterministicStep) {
		t.Errorf("err = %v, want errors.Is planner.ErrDeterministicStep", err)
	}
}

func TestWakeMode_DeclaresPoll(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{Reason: planner.FinishGoal}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.WakeMode() != planner.WakePoll {
		t.Errorf("WakeMode() = %q, want %q", p.WakeMode(), planner.WakePoll)
	}
	if planner.ResolveWakeMode(p) != planner.WakePoll {
		t.Errorf("ResolveWakeMode(p) = %q, want %q", planner.ResolveWakeMode(p), planner.WakePoll)
	}
}

func TestWakeMode_PlannerInterfaceSatisfied(t *testing.T) {
	// Compile-time check (already in deterministic.go); this is a
	// runtime parity gate too — exercise the interfaces.
	var p planner.Planner = mustBuildSimplePlanner(t)
	if p == nil {
		t.Fatal("DeterministicPlanner does not satisfy planner.Planner")
	}
	wa, ok := p.(planner.WakeAware)
	if !ok {
		t.Fatal("DeterministicPlanner does not satisfy planner.WakeAware")
	}
	if wa.WakeMode() != planner.WakePoll {
		t.Errorf("WakeAware.WakeMode() = %q, want %q", wa.WakeMode(), planner.WakePoll)
	}
}

func mustBuildSimplePlanner(t *testing.T) *deterministic.DeterministicPlanner {
	t.Helper()
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.FinishStep{Reason: planner.FinishGoal}),
	)
	if err != nil {
		t.Fatalf("unexpected construction error: %v", err)
	}
	return p
}

// Sanity: a configured CallToolStep emits the expected CallTool
// decision when invoked through the walker. (steps_test.go covers
// the step in isolation; this ties the walker + step together for
// the most basic integration shape.)
func TestNext_CallToolStepEndToEnd(t *testing.T) {
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(&deterministic.CallToolStep{
			Tool: "search",
			ArgsBuilder: func(_ planner.RunContext) (json.RawMessage, error) {
				return json.RawMessage(`{"q":"foo"}`), nil
			},
			Reasoning: "test",
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec, err := p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := dec.(planner.CallTool)
	if !ok {
		t.Fatalf("dec = %T, want planner.CallTool", dec)
	}
	if call.Tool != "search" {
		t.Errorf("Tool = %q, want %q", call.Tool, "search")
	}
}

// Sanity helper to keep tasks dep referenced (compile-time only).
var _ = tasks.KindBackground
