package retry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/retry"
)

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

// TestRetry_ReportsRejectedNonFinalAttempts asserts the retry wrapper
// reports each validator-rejected NON-final attempt into the attempt-cost
// tap, and never the final (accepted) attempt — the accepted response
// propagates and is accounted at the governance boundary via resp.Cost.
func TestRetry_ReportsRejectedNonFinalAttempts(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	// Distinct per-attempt costs (powers of ten) so any drop/double-count
	// is arithmetically unambiguous.
	rec := newRecorder(func(_ llm.CompleteRequest, attempt int) (llm.CompleteResponse, error) {
		switch attempt {
		case 1:
			return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: 0.01}}, nil
		case 2:
			return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: 0.1}}, nil
		default:
			return llm.CompleteResponse{Content: "good", Cost: llm.Cost{TotalCost: 1.0}}, nil
		}
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 3})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	validator := func(r llm.CompleteResponse) error {
		if r.Content == "good" {
			return nil
		}
		return errors.New("rejected")
	}

	ctx, tap := llm.ContextWithAttemptCostTap(ctxWithIdentity(t))
	resp, err := client.Complete(ctx, sampleRequest(validator))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "good" {
		t.Fatalf("Content = %q, want good", resp.Content)
	}
	// Attempts 1 and 2 were rejected non-final → reported (0.01 + 0.1).
	// Attempt 3 (good) propagates → NOT reported (its 1.0 rides resp.Cost).
	total, n := tap.Drain()
	if n != 2 {
		t.Fatalf("reported attempts = %d, want 2", n)
	}
	if !approx(total, 0.11) {
		t.Fatalf("tap total = %v, want 0.11 (final attempt must NOT be reported)", total)
	}
	if !approx(resp.Cost.TotalCost, 1.0) {
		t.Fatalf("final resp.Cost = %v, want 1.0", resp.Cost.TotalCost)
	}
}

// TestRetry_ExhaustionNeverReportsFinalAttempt asserts that on retry
// exhaustion the wrapper reports every rejected non-final attempt but NOT
// the final one — the exhaustion path RETURNS lastResp with
// ErrRetryExhausted, so its cost propagates and is accounted downstream.
func TestRetry_ExhaustionNeverReportsFinalAttempt(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, attempt int) (llm.CompleteResponse, error) {
		switch attempt {
		case 1:
			return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: 0.01}}, nil
		default:
			return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: 0.1}}, nil
		}
	})
	// MaxRetries=1 → attempts: initial + 1 retry = 2 total; both rejected.
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 1})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	validator := func(_ llm.CompleteResponse) error { return errors.New("always bad") }

	ctx, tap := llm.ContextWithAttemptCostTap(ctxWithIdentity(t))
	resp, err := client.Complete(ctx, sampleRequest(validator))
	if !errors.Is(err, llm.ErrRetryExhausted) {
		t.Fatalf("err = %v, want ErrRetryExhausted", err)
	}
	// Attempt 1 (0.01) reported; the FINAL attempt (0.1) returned as
	// lastResp → NOT reported (propagates via resp.Cost).
	total, n := tap.Drain()
	if n != 1 {
		t.Fatalf("reported attempts = %d, want 1", n)
	}
	if !approx(total, 0.01) {
		t.Fatalf("tap total = %v, want 0.01", total)
	}
	if !approx(resp.Cost.TotalCost, 0.1) {
		t.Fatalf("returned lastResp.Cost = %v, want 0.1 (must propagate, not be reported)", resp.Cost.TotalCost)
	}
}

// TestRetry_NoValidator_ReportsNothing — a nil-Validator pass-through
// consumes no attempt; nothing is reported.
func TestRetry_NoValidator_ReportsNothing(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "ok", Cost: llm.Cost{TotalCost: 0.5}}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 3})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	ctx, tap := llm.ContextWithAttemptCostTap(ctxWithIdentity(t))
	if _, err := client.Complete(ctx, sampleRequest(nil)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if total, n := tap.Drain(); total != 0 || n != 0 {
		t.Fatalf("tap = (%v, %d), want (0, 0) for nil-validator pass-through", total, n)
	}
}

// TestRetry_InnerErrorPropagates_ReportsNothing — an inner error
// propagates (resp, err) unreported; the tap stays empty.
func TestRetry_InnerErrorPropagates_ReportsNothing(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	sentinel := errors.New("provider exploded")
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.3}}, sentinel
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 3})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	validator := func(_ llm.CompleteResponse) error { return errors.New("unused") }

	ctx, tap := llm.ContextWithAttemptCostTap(ctxWithIdentity(t))
	_, err := client.Complete(ctx, sampleRequest(validator))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the propagated sentinel", err)
	}
	if total, n := tap.Drain(); total != 0 || n != 0 {
		t.Fatalf("tap = (%v, %d), want (0, 0) for propagated inner error", total, n)
	}
}

// TestRetry_ReportsWithoutTap_NoPanic — with no tap installed (governance
// latent / absent), the report call is a silent no-op.
func TestRetry_ReportsWithoutTap_NoPanic(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, attempt int) (llm.CompleteResponse, error) {
		if attempt == 1 {
			return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: 0.01}}, nil
		}
		return llm.CompleteResponse{Content: "good"}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 3})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})
	validator := func(r llm.CompleteResponse) error {
		if r.Content == "good" {
			return nil
		}
		return errors.New("bad")
	}
	// Plain ctx — no tap.
	if _, err := client.Complete(ctxWithIdentity(t), sampleRequest(validator)); err != nil {
		t.Fatalf("Complete without tap: %v", err)
	}
	// Nothing to assert beyond no panic — Peek on a plain ctx is (0,0).
	if total, n := llm.PeekAttemptCost(context.Background()); total != 0 || n != 0 {
		t.Fatalf("Peek = (%v, %d), want (0, 0)", total, n)
	}
}
