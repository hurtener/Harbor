package output_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/output"
)

// approxUSD compares two USD floats with a tolerance that absorbs binary
// float64 rounding of decimal cents.
func approxUSD(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

func downgradeProfile() llm.ConfigSnapshot {
	return snapshotWithProfile("m", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModeNative,
	})
}

// TestDowngrade_ReportsEveryErroredAttempt_Exhaustion drives the chain to
// exhaustion with distinct per-attempt costs and asserts EVERY errored
// attempt is reported into the tap exactly once (the exhaustion return
// discards a zero response, so nothing propagates).
func TestDowngrade_ReportsEveryErroredAttempt_Exhaustion(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, attempt int) (llm.CompleteResponse, error) {
		// Powers of ten so any drop/double-count is unambiguous. Every
		// attempt is a schema-class failure → the chain steps Native →
		// Prompted → text, then exhausts.
		cost := 0.001 * pow10(attempt-1) // 0.001, 0.01, 0.1
		return llm.CompleteResponse{Cost: llm.Cost{TotalCost: cost}},
			fmt.Errorf("%w: attempt %d", llm.ErrInvalidJSONSchema, attempt)
	})
	client := output.Wrap(rec, downgradeProfile(), llm.Deps{Bus: bus})

	ctx, tap := llm.ContextWithAttemptCostTap(ctxWithIdentity(t))
	resp, err := client.Complete(ctx, sampleRequest("m", llm.FormatJSONSchema))
	if !errors.Is(err, llm.ErrDowngradeExhausted) {
		t.Fatalf("err = %v, want ErrDowngradeExhausted", err)
	}
	if resp.Cost.TotalCost != 0 {
		t.Fatalf("exhaustion returns a zero response; resp.Cost = %v", resp.Cost.TotalCost)
	}
	total, n := tap.Drain()
	if n != 3 {
		t.Fatalf("reported attempts = %d, want 3 (every errored attempt)", n)
	}
	if !approxUSD(total, 0.111) {
		t.Fatalf("tap total = %v, want 0.111 (0.001+0.01+0.1)", total)
	}
}

// TestDowngrade_ReportsNonSchemaErrorAttempt asserts the non-schema-error
// branch (immediate return of a zero response discarding the errored
// attempt) reports that attempt's cost.
func TestDowngrade_ReportsNonSchemaErrorAttempt(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	sentinel := errors.New("provider 503 transient")
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.07}}, sentinel
	})
	client := output.Wrap(rec, downgradeProfile(), llm.Deps{Bus: bus})

	ctx, tap := llm.ContextWithAttemptCostTap(ctxWithIdentity(t))
	resp, err := client.Complete(ctx, sampleRequest("m", llm.FormatJSONSchema))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the non-schema sentinel", err)
	}
	if resp.Cost.TotalCost != 0 {
		t.Fatalf("non-schema error returns a zero response; resp.Cost = %v", resp.Cost.TotalCost)
	}
	total, n := tap.Drain()
	if n != 1 {
		t.Fatalf("reported attempts = %d, want 1", n)
	}
	if !approxUSD(total, 0.07) {
		t.Fatalf("tap total = %v, want 0.07", total)
	}
}

// TestDowngrade_SuccessPropagatesUnreported asserts that when a downgraded
// attempt succeeds, the errored earlier attempt is reported but the final
// SUCCESS propagates unreported (its cost rides resp.Cost).
func TestDowngrade_SuccessPropagatesUnreported(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, attempt int) (llm.CompleteResponse, error) {
		if attempt == 1 {
			// Native fails (schema) → reported.
			return llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.02}},
				fmt.Errorf("%w: native", llm.ErrInvalidJSONSchema)
		}
		// Prompted succeeds → propagates, NOT reported.
		return llm.CompleteResponse{Content: "ok", Cost: llm.Cost{TotalCost: 0.5}}, nil
	})
	client := output.Wrap(rec, downgradeProfile(), llm.Deps{Bus: bus})

	ctx, tap := llm.ContextWithAttemptCostTap(ctxWithIdentity(t))
	resp, err := client.Complete(ctx, sampleRequest("m", llm.FormatJSONSchema))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" || !approxUSD(resp.Cost.TotalCost, 0.5) {
		t.Fatalf("resp = %+v, want ok/0.5 propagated", resp.Cost)
	}
	total, n := tap.Drain()
	if n != 1 {
		t.Fatalf("reported attempts = %d, want 1 (only the errored native attempt)", n)
	}
	if !approxUSD(total, 0.02) {
		t.Fatalf("tap total = %v, want 0.02 (success must NOT be reported)", total)
	}
}

// TestDowngrade_PassThrough_ReportsNothing — when the profile has no
// OutputMode (pure pass-through), the wrapper drives one inner call and
// reports nothing (it propagates whatever it gets).
func TestDowngrade_PassThrough_ReportsNothing(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "ok", Cost: llm.Cost{TotalCost: 0.9}}, nil
	})
	// No OutputMode → pass-through.
	cfg := snapshotWithProfile("m", llm.ModelProfile{ContextWindowTokens: 1000})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	ctx, tap := llm.ContextWithAttemptCostTap(ctxWithIdentity(t))
	if _, err := client.Complete(ctx, sampleRequest("m", llm.FormatJSONSchema)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if total, n := tap.Drain(); total != 0 || n != 0 {
		t.Fatalf("tap = (%v, %d), want (0, 0) for pass-through", total, n)
	}
}

func pow10(n int) float64 {
	out := 1.0
	for range n {
		out *= 10
	}
	return out
}
