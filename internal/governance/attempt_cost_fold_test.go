package governance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

// TestCostAccumulator_PostCall_FoldsAttemptTap asserts PostCall folds the
// per-call attempt-cost tap total with the final resp.Cost in a single
// accumulation, and drains the tap exactly once.
func TestCostAccumulator_PostCall_FoldsAttemptTap(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	ctx := ctxWith(t, "T", "U", "S", "R")
	ctx, tap := llm.ContextWithAttemptCostTap(ctx)
	// Two consumed intermediate attempts (powers of ten).
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 0.01})
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 0.1})

	// Final response carries 1.0.
	if err := acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 1.0}, Content: "x"}, nil); err != nil {
		t.Fatalf("PostCall: %v", err)
	}

	q := identity.MustQuadrupleFrom(ctx)
	total, byModel, err := acc.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !floatNear(total, 1.11) {
		t.Fatalf("total = %v, want 1.11 (0.01 + 0.1 + 1.0)", total)
	}
	if !floatNear(byModel["m"], 1.11) {
		t.Fatalf("byModel[m] = %v, want 1.11", byModel["m"])
	}
	// The tap was drained by PostCall — a second drain yields zero.
	if td, n := tap.Drain(); td != 0 || n != 0 {
		t.Fatalf("tap not drained by PostCall: Drain = (%v, %d)", td, n)
	}
}

// TestCostAccumulator_PostCall_ZeroGuardDoesNotSwallowTap asserts the
// extended zero-work early return: a zero final response with a NONZERO
// tap still accumulates the tap; a fully-zero call with no tap is a no-op.
func TestCostAccumulator_PostCall_ZeroGuardDoesNotSwallowTap(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	// Case 1: zero final resp, nonzero tap → must accumulate the tap.
	ctx1 := ctxWith(t, "T1", "U", "S", "R")
	ctx1, _ = llm.ContextWithAttemptCostTap(ctx1)
	llm.ReportAttemptCost(ctx1, llm.Cost{TotalCost: 0.42})
	if err := acc.PostCall(ctx1, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{}, errors.New("outer errored")); err != nil {
		t.Fatalf("PostCall case1: %v", err)
	}
	q1 := identity.MustQuadrupleFrom(ctx1)
	if total, _, _ := acc.Snapshot(ctx1, q1); !floatNear(total, 0.42) {
		t.Fatalf("case1 total = %v, want 0.42 (nonzero tap must not be swallowed)", total)
	}

	// Case 2: fully-zero call with no tap → no-op (total stays zero).
	ctx2 := ctxWith(t, "T2", "U", "S", "R")
	if err := acc.PostCall(ctx2, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{}, nil); err != nil {
		t.Fatalf("PostCall case2: %v", err)
	}
	q2 := identity.MustQuadrupleFrom(ctx2)
	if total, _, _ := acc.Snapshot(ctx2, q2); total != 0 {
		t.Fatalf("case2 total = %v, want 0 (no-op)", total)
	}
}

// TestCostAccumulator_PostCall_StrandedTapFailsLoud asserts that when
// keyState resolution fails before the drain, PostCall surfaces a loud
// error naming the at-risk attempt total (never silently zeroed).
func TestCostAccumulator_PostCall_StrandedTapFailsLoud(t *testing.T) {
	t.Parallel()
	bus, cleanup := openBusForStateFailTest(t)
	defer cleanup()
	acc, err := governance.NewCostAccumulator(failingStateStore{}, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	ctx := ctxWith(t, "T", "U", "S", "R")
	ctx, _ = llm.ContextWithAttemptCostTap(ctx)
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 0.05})

	err = acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 1.0}, Content: "x"}, nil)
	if err == nil {
		t.Fatal("PostCall returned nil; want a loud stranded-tap error")
	}
	if !errors.Is(err, governance.ErrStateUnavailable) {
		t.Fatalf("err = %v, want wrapped ErrStateUnavailable", err)
	}
	// The at-risk amount is named in the error.
	if !strings.Contains(err.Error(), "0.05") || !strings.Contains(err.Error(), "at risk") {
		t.Fatalf("error must name the at-risk attempt total (0.05): %v", err)
	}
}

// TestCostAccumulator_PostCall_PersistFailNamesFoldedTotal asserts that a
// persist (Save) failure after a successful fold names the at-risk folded
// total (tap + final resp cost).
func TestCostAccumulator_PostCall_PersistFailNamesFoldedTotal(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	// Wrap the real inmem store so Load succeeds (keyState resolves) but
	// Save fails (persist path).
	sfs := &saveFailingStore{StateStore: st}
	acc, err := governance.NewCostAccumulator(sfs, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	ctx := ctxWith(t, "T", "U", "S", "R")
	ctx, _ = llm.ContextWithAttemptCostTap(ctx)
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 0.25})

	err = acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.75}, Content: "x"}, nil)
	if err == nil {
		t.Fatal("PostCall returned nil; want a loud persist error")
	}
	// Folded total = 0.25 + 0.75 = 1.0 must be named as at risk.
	if !strings.Contains(err.Error(), "1.0") || !strings.Contains(err.Error(), "at risk") {
		t.Fatalf("error must name the folded at-risk total (1.0): %v", err)
	}
}

// saveFailingStore embeds a real StateStore but forces Save to fail,
// exercising the PostCall persist error path after a successful keyState
// load + fold.
type saveFailingStore struct {
	state.StateStore
}

func (s *saveFailingStore) Save(_ context.Context, _ state.StateRecord) error {
	return errors.New("io: simulated save failure")
}
