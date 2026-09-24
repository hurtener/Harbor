package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestInvocationFence_ConcurrentScopes(t *testing.T) {
	if InvocationInvalidated(t.Context()) != nil || CheckInvocationFence(t.Context()) != nil {
		t.Fatal("unfenced calls changed")
	}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			signal := make(chan struct{})
			ctx := WithInvocationFence(t.Context(), signal)
			if i%2 == 0 {
				close(signal)
			}
			calls := 0
			_, err := RunWithPolicy(ctx, nil, func(context.Context, json.RawMessage) (ToolResult, error) {
				calls++
				return ToolResult{}, nil
			}, nil, nil, ToolPolicy{})
			if (errors.Is(err, ErrInvocationSuperseded)) != (i%2 == 0) || calls != i%2 {
				t.Errorf("scope=%d calls=%d err=%v", i, calls, err)
			}
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(CheckInvocationFence(ctx), context.Canceled) {
		t.Fatal("hard cancellation lost")
	}
}

func TestInvocationFence_PolicyPreservesUncertainAttempt(t *testing.T) {
	for _, at := range []string{"between_attempts", "during_backoff"} {
		t.Run(at, func(t *testing.T) {
			signal := make(chan struct{})
			ctx := WithInvocationFence(t.Context(), signal)
			uncertain := errors.New("connection lost after dispatch")
			calls := 0
			invoke := func(context.Context, json.RawMessage) (ToolResult, error) {
				calls++
				if at == "between_attempts" {
					close(signal)
				}
				return ToolResult{Value: "partial receipt"}, uncertain
			}
			jitter := func() float64 {
				close(signal) // invalidation exactly at the retry-wait boundary
				return 0
			}
			result, err := runWithPolicy(ctx, nil, invoke, nil, nil, ToolPolicy{MaxRetries: 2, BackoffBase: time.Hour, BackoffMax: time.Hour}, jitter, nil)
			var policyErr *PolicyError
			if calls != 1 || result.Value != "partial receipt" || !errors.Is(err, uncertain) || !errors.Is(err, ErrInvocationSuperseded) || !errors.As(err, &policyErr) || policyErr.Attempts != 1 || policyErr.Class != ErrClassPermanent {
				t.Fatalf("calls=%d result=%+v err=%v policy=%+v", calls, result, err, policyErr)
			}
		})
	}
	for _, err := range []error{ErrInvocationSuperseded, ErrInvocationCleanupFailed} {
		if ClassifyError(err, false) != ErrClassPermanent || ClassifyError(errors.Join(err, context.DeadlineExceeded), true) != ErrClassPermanent {
			t.Fatalf("fence error became retryable: %v", err)
		}
	}
}
