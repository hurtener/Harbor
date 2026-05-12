package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestDecision_SumTypeShapesCompile(t *testing.T) {
	// Compile-time assertions that each of the six shapes satisfies
	// the sealed Decision interface. Removing a shape from the sum
	// would fail to compile here — the test exists to make that
	// failure surface as a test failure (clearer locator than a
	// random consumer's compile error).
	var (
		_ planner.Decision = planner.CallTool{Tool: "x", Args: json.RawMessage(`{}`)}
		_ planner.Decision = planner.CallParallel{Branches: []planner.CallTool{{Tool: "a"}}}
		_ planner.Decision = planner.SpawnTask{Kind: tasks.KindBackground}
		_ planner.Decision = planner.AwaitTask{TaskID: tasks.TaskID("t")}
		_ planner.Decision = planner.RequestPause{Reason: planner.PauseAwaitInput}
		_ planner.Decision = planner.Finish{Reason: planner.FinishGoal}
	)
}

func TestPauseReason_ValidValues(t *testing.T) {
	cases := []struct {
		name  string
		r     planner.PauseReason
		valid bool
	}{
		{"approval_required", planner.PauseApprovalRequired, true},
		{"await_input", planner.PauseAwaitInput, true},
		{"external_event", planner.PauseExternalEvent, true},
		{"constraints_conflict", planner.PauseConstraintsConflict, true},
		{"unknown", planner.PauseReason("totally_unknown"), false},
		{"empty", planner.PauseReason(""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planner.IsValidPauseReason(c.r); got != c.valid {
				t.Fatalf("IsValidPauseReason(%q) = %v want %v", c.r, got, c.valid)
			}
		})
	}
}

func TestFinishReason_ValidValues(t *testing.T) {
	cases := []struct {
		name  string
		r     planner.FinishReason
		valid bool
	}{
		{"goal", planner.FinishGoal, true},
		{"no_path", planner.FinishNoPath, true},
		{"cancelled", planner.FinishCancelled, true},
		{"deadline_exceeded", planner.FinishDeadlineExceeded, true},
		{"constraints_conflict", planner.FinishConstraintsConflict, true},
		{"unknown", planner.FinishReason("nope"), false},
		{"empty", planner.FinishReason(""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planner.IsValidFinishReason(c.r); got != c.valid {
				t.Fatalf("IsValidFinishReason(%q) = %v want %v", c.r, got, c.valid)
			}
		})
	}
}

func TestWakeMode_ValidValuesAndString(t *testing.T) {
	if !planner.IsValidWakeMode(planner.WakePush) {
		t.Fatalf("WakePush must be valid")
	}
	if !planner.IsValidWakeMode(planner.WakePoll) {
		t.Fatalf("WakePoll must be valid")
	}
	if !planner.IsValidWakeMode(planner.WakeHybrid) {
		t.Fatalf("WakeHybrid must be valid")
	}
	if planner.IsValidWakeMode("") {
		t.Fatalf("empty WakeMode must be invalid")
	}
	if planner.IsValidWakeMode("eager") {
		t.Fatalf("'eager' is not one of the canonical modes")
	}
	if planner.WakePush.String() != "push" {
		t.Fatalf("WakePush.String() = %q want \"push\"", planner.WakePush.String())
	}
}

// ResolveWakeMode falls back to WakePush for planners that don't
// implement WakeAware (the documented default).
func TestResolveWakeMode_FallbackToPush(t *testing.T) {
	p := &fakeNoAware{}
	if got := planner.ResolveWakeMode(p); got != planner.WakePush {
		t.Fatalf("ResolveWakeMode(non-WakeAware) = %q want %q", got, planner.WakePush)
	}
}

// ResolveWakeMode returns the planner's declared mode when it
// implements WakeAware.
func TestResolveWakeMode_HonoursWakeAware(t *testing.T) {
	p := &fakeAware{mode: planner.WakePoll}
	if got := planner.ResolveWakeMode(p); got != planner.WakePoll {
		t.Fatalf("ResolveWakeMode(WakePoll) = %q want %q", got, planner.WakePoll)
	}
}

func TestTrajectory_SerializeFailsLoudly(t *testing.T) {
	// Phase 42's contract: Serialize returns ErrTrajectoryNotImplemented.
	// Phase 43 closes the contract; until then, callers fail loudly.
	tr := &planner.Trajectory{Query: "hello"}
	out, err := tr.Serialize()
	if !errors.Is(err, planner.ErrTrajectoryNotImplemented) {
		t.Fatalf("Serialize err = %v want ErrTrajectoryNotImplemented", err)
	}
	if out != nil {
		t.Fatalf("Serialize returned non-nil bytes (%d) on stub path", len(out))
	}
}

func TestRunContext_ZeroValueIsUsable(t *testing.T) {
	// A zero-value RunContext compiles and reads sanely — the
	// Quadruple is zero (invalid identity, but zero-value), every
	// view interface is nil (callers MUST nil-check before Resolve).
	// This is intentional: the runtime constructs a fully-populated
	// RunContext per planner step; the zero value is only used in
	// pure compile-time / type-assertion tests.
	var rc planner.RunContext
	if rc.Quadruple.RunID != "" {
		t.Fatalf("zero RunContext.Quadruple.RunID should be empty")
	}
	if rc.Catalog != nil {
		t.Fatalf("zero RunContext.Catalog should be nil")
	}
	if rc.Emit != nil {
		t.Fatalf("zero RunContext.Emit should be nil")
	}
}

// fakeNoAware satisfies planner.Planner without implementing WakeAware.
type fakeNoAware struct{}

func (*fakeNoAware) Next(ctx context.Context, run planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// fakeAware satisfies planner.Planner AND planner.WakeAware.
type fakeAware struct {
	mode planner.WakeMode
}

func (f *fakeAware) Next(ctx context.Context, run planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func (f *fakeAware) WakeMode() planner.WakeMode {
	return f.mode
}

// TestRunContext_QuadrupleRoundTrip verifies that identity.Quadruple
// flows through RunContext unchanged.
func TestRunContext_QuadrupleRoundTrip(t *testing.T) {
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "r-42",
	}
	rc := planner.RunContext{Quadruple: q}
	if rc.Quadruple.RunID != "r-42" {
		t.Fatalf("Quadruple.RunID lost: got %q", rc.Quadruple.RunID)
	}
	if rc.Quadruple.TenantID != "t1" || rc.Quadruple.UserID != "u1" || rc.Quadruple.SessionID != "s1" {
		t.Fatalf("Identity triple did not round-trip through RunContext: %+v", rc.Quadruple)
	}
}
