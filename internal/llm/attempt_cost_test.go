package llm_test

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestAttemptCostTap_ReportWithoutTap_NoOp(t *testing.T) {
	t.Parallel()
	// No tap installed on a plain ctx — Report/Peek/Drain must all no-op.
	ctx := context.Background()
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 1.5})
	if total, n := llm.PeekAttemptCost(ctx); total != 0 || n != 0 {
		t.Fatalf("PeekAttemptCost without tap = (%v, %d), want (0, 0)", total, n)
	}
	if total, n := llm.DrainAttemptCost(ctx); total != 0 || n != 0 {
		t.Fatalf("DrainAttemptCost without tap = (%v, %d), want (0, 0)", total, n)
	}
}

func TestAttemptCostTap_ReportFold_Sums(t *testing.T) {
	t.Parallel()
	ctx, tap := llm.ContextWithAttemptCostTap(context.Background())
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 0.1})
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 0.02})
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 0.003})

	// Peek is non-destructive.
	if total, n := tap.Drain(); !approx(total, 0.123) || n != 3 {
		t.Fatalf("Drain = (%v, %d), want (0.123, 3)", total, n)
	}
}

func TestAttemptCostTap_Peek_NonDestructive(t *testing.T) {
	t.Parallel()
	ctx, tap := llm.ContextWithAttemptCostTap(context.Background())
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 0.5})

	// Peek twice; both return the same, and the tap stays drainable.
	if total, n := llm.PeekAttemptCost(ctx); !approx(total, 0.5) || n != 1 {
		t.Fatalf("first Peek = (%v, %d), want (0.5, 1)", total, n)
	}
	if total, n := llm.PeekAttemptCost(ctx); !approx(total, 0.5) || n != 1 {
		t.Fatalf("second Peek = (%v, %d), want (0.5, 1)", total, n)
	}
	if total, n := tap.Drain(); !approx(total, 0.5) || n != 1 {
		t.Fatalf("Drain after Peek = (%v, %d), want (0.5, 1)", total, n)
	}
}

func TestAttemptCostTap_Drain_OneShot(t *testing.T) {
	t.Parallel()
	ctx, tap := llm.ContextWithAttemptCostTap(context.Background())
	llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: 2.0})

	if total, n := tap.Drain(); !approx(total, 2.0) || n != 1 {
		t.Fatalf("first Drain = (%v, %d), want (2.0, 1)", total, n)
	}
	// Second drain (whether via handle or ctx) returns zero — no double-count.
	if total, n := tap.Drain(); total != 0 || n != 0 {
		t.Fatalf("second Drain(handle) = (%v, %d), want (0, 0)", total, n)
	}
	if total, n := llm.DrainAttemptCost(ctx); total != 0 || n != 0 {
		t.Fatalf("second Drain(ctx) = (%v, %d), want (0, 0)", total, n)
	}
}

func TestAttemptCostTap_PerCallIsolation_AcrossCtxTrees(t *testing.T) {
	t.Parallel()
	base := context.Background()
	ctxA, tapA := llm.ContextWithAttemptCostTap(base)
	ctxB, tapB := llm.ContextWithAttemptCostTap(base)

	llm.ReportAttemptCost(ctxA, llm.Cost{TotalCost: 0.10})
	llm.ReportAttemptCost(ctxB, llm.Cost{TotalCost: 0.99})

	if total, n := tapA.Drain(); !approx(total, 0.10) || n != 1 {
		t.Fatalf("tapA Drain = (%v, %d), want (0.10, 1)", total, n)
	}
	if total, n := tapB.Drain(); !approx(total, 0.99) || n != 1 {
		t.Fatalf("tapB Drain = (%v, %d), want (0.99, 1)", total, n)
	}
}

func TestAttemptCostTap_ConcurrentReport_NoLostUpdate(t *testing.T) {
	t.Parallel()
	ctx, tap := llm.ContextWithAttemptCostTap(context.Background())
	const workers = 200
	const each = 0.01

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			llm.ReportAttemptCost(ctx, llm.Cost{TotalCost: each})
		}()
	}
	wg.Wait()

	total, n := tap.Drain()
	if n != workers {
		t.Fatalf("attempt count = %d, want %d", n, workers)
	}
	if !approx(total, workers*each) {
		t.Fatalf("total = %v, want %v (lost update under concurrency)", total, workers*each)
	}
}

// approx compares two USD floats with a tolerance that absorbs binary
// float64 rounding of decimal cents.
func approx(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
