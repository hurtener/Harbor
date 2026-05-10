package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// makeEnv builds a complete-identity envelope for shell tests.
func makeEnv(payload string) messages.Envelope {
	return messages.Envelope{
		Headers: messages.Headers{
			TenantID: "T", UserID: "U", Topic: "policy-test",
		},
		SessionID: "S",
		RunID:     "R-1",
		Payload:   payload,
	}
}

// TestNodePolicy_ZeroValue_BareWorker pins the "no policy" path:
// single invocation, no validate, no timeout, no retry.
func TestNodePolicy_ZeroValue_BareWorker(t *testing.T) {
	t.Parallel()
	calls := atomic.Int32{}
	fn := NodeFunc(func(_ context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		calls.Add(1)
		return in, nil
	})
	out, err := runWithReliability(context.Background(), makeEnv("p"), fn, NodePolicy{}, nil, "n", nil, nil)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if out.Payload != "p" {
		t.Errorf("payload=%v, want p", out.Payload)
	}
	if calls.Load() != 1 {
		t.Errorf("calls=%d, want 1", calls.Load())
	}
}

// TestNodePolicy_ValidateBoth_RejectsMalformedIn — input fails validate.
func TestNodePolicy_ValidateBoth_RejectsMalformedIn(t *testing.T) {
	t.Parallel()
	calls := atomic.Int32{}
	fn := NodeFunc(func(_ context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		calls.Add(1)
		return in, nil
	})
	policy := NodePolicy{
		Validate:     ValidateBoth,
		ValidateFunc: func(_ messages.Envelope) error { return errors.New("bad") },
	}
	_, err := runWithReliability(context.Background(), makeEnv("p"), fn, policy, nil, "n", nil, nil)
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("err=%v, want *RunError", err)
	}
	if re.Code != CodeValidationFailed {
		t.Errorf("code=%v, want %v", re.Code, CodeValidationFailed)
	}
	if calls.Load() != 0 {
		t.Errorf("Func should not have been invoked when input validation fails: calls=%d", calls.Load())
	}
}

// TestNodePolicy_ValidateBoth_RejectsMalformedOut — output fails validate.
func TestNodePolicy_ValidateBoth_RejectsMalformedOut(t *testing.T) {
	t.Parallel()
	fn := NodeFunc(func(_ context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		// Mutate payload so we can have a validator that says "in is OK, out is not".
		out := in
		out.Payload = "MUTATED"
		return out, nil
	})
	policy := NodePolicy{
		Validate: ValidateBoth,
		ValidateFunc: func(env messages.Envelope) error {
			if s, ok := env.Payload.(string); ok && s == "MUTATED" {
				return errors.New("output rejected")
			}
			return nil
		},
	}
	_, err := runWithReliability(context.Background(), makeEnv("p"), fn, policy, nil, "n", nil, nil)
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("err=%v, want *RunError", err)
	}
	if re.Code != CodeValidationFailed {
		t.Errorf("code=%v, want %v", re.Code, CodeValidationFailed)
	}
}

// TestNodePolicy_ValidateNone_SkipsValidator — none mode never calls validator.
func TestNodePolicy_ValidateNone_SkipsValidator(t *testing.T) {
	t.Parallel()
	validatorCalled := atomic.Int32{}
	fn := NodeFunc(func(_ context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		return in, nil
	})
	policy := NodePolicy{
		Validate: ValidateNone,
		ValidateFunc: func(_ messages.Envelope) error {
			validatorCalled.Add(1)
			return errors.New("should not run")
		},
	}
	if _, err := runWithReliability(context.Background(), makeEnv("p"), fn, policy, nil, "n", nil, nil); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if validatorCalled.Load() != 0 {
		t.Errorf("validator was called %d times under ValidateNone", validatorCalled.Load())
	}
}

// TestNodePolicy_Timeout_ProducesRunError — TimeoutMS fires before
// the slow node returns.
func TestNodePolicy_Timeout_ProducesRunError(t *testing.T) {
	t.Parallel()
	fn := NodeFunc(func(ctx context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		select {
		case <-ctx.Done():
			return messages.Envelope{}, ctx.Err()
		case <-time.After(2 * time.Second):
			return in, nil
		}
	})
	policy := NodePolicy{TimeoutMS: 50, MaxRetries: 0}

	start := time.Now()
	_, err := runWithReliability(context.Background(), makeEnv("p"), fn, policy, nil, "slow", nil, nil)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("shell did not honor TimeoutMS — took %s", elapsed)
	}
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("err=%v, want *RunError", err)
	}
	if re.Code != CodeNodeTimeout {
		t.Errorf("code=%v, want %v", re.Code, CodeNodeTimeout)
	}
}

// TestNodePolicy_MaxRetries_StopsAfterN — exactly MaxRetries+1 calls.
func TestNodePolicy_MaxRetries_StopsAfterN(t *testing.T) {
	t.Parallel()
	calls := atomic.Int32{}
	fn := NodeFunc(func(_ context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		calls.Add(1)
		return messages.Envelope{}, errors.New("always fails")
	})
	policy := NodePolicy{MaxRetries: 3, BackoffBase: 0}
	_, err := runWithReliability(context.Background(), makeEnv("p"), fn, policy, nil, "fail", nil, nil)
	if calls.Load() != 4 {
		t.Errorf("calls=%d, want 4 (MaxRetries=3 → 1 initial + 3 retries)", calls.Load())
	}
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("err=%v, want *RunError", err)
	}
	if re.Code != CodeNodeException {
		t.Errorf("code=%v, want %v", re.Code, CodeNodeException)
	}
	if got, _ := re.Metadata["attempts"].(int); got != 4 {
		t.Errorf("metadata.attempts=%v, want 4", re.Metadata["attempts"])
	}
}

// TestNodePolicy_CtxCancelled_AbortsRetries — ctx cancel during
// backoff returns CodeRunCancelled.
func TestNodePolicy_CtxCancelled_AbortsRetries(t *testing.T) {
	t.Parallel()
	calls := atomic.Int32{}
	fn := NodeFunc(func(_ context.Context, _ messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		calls.Add(1)
		return messages.Envelope{}, errors.New("transient")
	})
	policy := NodePolicy{
		MaxRetries:  10,
		BackoffBase: 50 * time.Millisecond,
		BackoffMult: 2.0,
		MaxBackoff:  500 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct {
		err error
	})
	go func() {
		_, err := runWithReliability(ctx, makeEnv("p"), fn, policy, nil, "n", nil, nil)
		done <- struct{ err error }{err}
	}()

	// Let one or two attempts run, then cancel.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		var re *RunError
		if !errors.As(got.err, &re) {
			t.Fatalf("err=%v, want *RunError", got.err)
		}
		if re.Code != CodeRunCancelled {
			t.Errorf("code=%v, want %v", re.Code, CodeRunCancelled)
		}
		if calls.Load() >= 10 {
			t.Errorf("retries continued past cancel: calls=%d", calls.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell did not honor ctx cancel within 3s")
	}
}

// TestNodePolicy_CtxCancelled_BeforeFirstAttempt — pre-cancelled ctx
// short-circuits to CodeRunCancelled with attempt=0.
func TestNodePolicy_CtxCancelled_BeforeFirstAttempt(t *testing.T) {
	t.Parallel()
	fn := NodeFunc(func(_ context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		t.Error("Func should not be invoked when ctx is pre-cancelled")
		return in, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runWithReliability(ctx, makeEnv("p"), fn, NodePolicy{}, nil, "n", nil, nil)
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("err=%v, want *RunError", err)
	}
	if re.Code != CodeRunCancelled {
		t.Errorf("code=%v, want %v", re.Code, CodeRunCancelled)
	}
}

// TestRunError_CarriesIdentity — every RunError populated by the
// shell carries the failing envelope's quadruple.
func TestRunError_CarriesIdentity(t *testing.T) {
	t.Parallel()
	fn := NodeFunc(func(_ context.Context, _ messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		return messages.Envelope{}, errors.New("oops")
	})
	in := messages.Envelope{
		Headers:   messages.Headers{TenantID: "tenant-A", UserID: "user-A"},
		SessionID: "sess-A",
		RunID:     "run-A-1",
	}
	_, err := runWithReliability(context.Background(), in, fn, NodePolicy{}, nil, "n", nil, nil)
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("err=%v, want *RunError", err)
	}
	if re.TenantID != "tenant-A" || re.UserID != "user-A" || re.SessionID != "sess-A" || re.RunID != "run-A-1" {
		t.Errorf("identity propagation failed: %+v", re)
	}
}

// TestNodePolicy_RetrySuccess — the second attempt succeeds; no error.
func TestNodePolicy_RetrySuccess(t *testing.T) {
	t.Parallel()
	calls := atomic.Int32{}
	fn := NodeFunc(func(_ context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		if calls.Add(1) == 1 {
			return messages.Envelope{}, errors.New("first fails")
		}
		return in, nil
	})
	policy := NodePolicy{MaxRetries: 3, BackoffBase: 0}
	out, err := runWithReliability(context.Background(), makeEnv("p"), fn, policy, nil, "n", nil, nil)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if out.Payload != "p" {
		t.Errorf("payload=%v, want p", out.Payload)
	}
	if calls.Load() != 2 {
		t.Errorf("calls=%d, want 2 (first failed, second succeeded)", calls.Load())
	}
}

// TestNodePolicy_PanicRecovery — a panicking Func is caught and
// classified as CodeNodeException; the shell does not crash.
func TestNodePolicy_PanicRecovery(t *testing.T) {
	t.Parallel()
	fn := NodeFunc(func(_ context.Context, _ messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		panic("synthetic panic")
	})
	_, err := runWithReliability(context.Background(), makeEnv("p"), fn, NodePolicy{}, nil, "n", nil, nil)
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("err=%v, want *RunError", err)
	}
	if re.Code != CodeNodeException {
		t.Errorf("code=%v, want %v", re.Code, CodeNodeException)
	}
}
