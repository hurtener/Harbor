package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestDefaultPolicy_Values(t *testing.T) {
	p := tools.DefaultPolicy()
	if p.TimeoutMS != 30000 {
		t.Errorf("TimeoutMS: got %d, want 30000", p.TimeoutMS)
	}
	if p.MaxRetries != 3 {
		t.Errorf("MaxRetries: got %d, want 3", p.MaxRetries)
	}
	if p.BackoffBase != 100*time.Millisecond {
		t.Errorf("BackoffBase: got %v, want 100ms", p.BackoffBase)
	}
	if p.BackoffMax != 30*time.Second {
		t.Errorf("BackoffMax: got %v, want 30s", p.BackoffMax)
	}
	if p.BackoffMult != 2 {
		t.Errorf("BackoffMult: got %v, want 2", p.BackoffMult)
	}
	if p.Validate != tools.ValidateBoth {
		t.Errorf("Validate: got %v, want ValidateBoth", p.Validate)
	}
	if len(p.RetryOn) != 3 {
		t.Errorf("RetryOn: got %d entries, want 3", len(p.RetryOn))
	}
}

func TestRunWithPolicy_DefaultsApplied_RetriesOnTransient(t *testing.T) {
	var counter atomic.Int64
	policy := tools.ToolPolicy{
		BackoffBase: 1 * time.Millisecond,
		BackoffMax:  5 * time.Millisecond,
	}
	out, err := tools.RunWithPolicy(context.Background(), json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			n := counter.Add(1)
			if n <= 2 {
				return tools.ToolResult{}, fmt.Errorf("transient: attempt %d", n)
			}
			return tools.ToolResult{Value: n}, nil
		},
		nil, nil, policy,
	)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := out.Value.(int64); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

func TestRunWithPolicy_ExhaustsRetries_WrapsSentinel(t *testing.T) {
	policy := tools.ToolPolicy{
		MaxRetries:  2,
		BackoffBase: 1 * time.Millisecond,
		BackoffMax:  5 * time.Millisecond,
		BackoffMult: 2,
		TimeoutMS:   1000,
		Validate:    tools.ValidateBoth,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
	}
	_, err := tools.RunWithPolicy(context.Background(), json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, fmt.Errorf("transient: never recovers")
		},
		nil, nil, policy,
	)
	if err == nil {
		t.Fatalf("expected ErrToolPolicyExhausted, got nil")
	}
	if !errors.Is(err, tools.ErrToolPolicyExhausted) {
		t.Fatalf("expected ErrToolPolicyExhausted, got: %v", err)
	}
}

func TestRunWithPolicy_PermanentError_NoRetry(t *testing.T) {
	var counter atomic.Int64
	policy := tools.ToolPolicy{
		MaxRetries:  3,
		BackoffBase: 1 * time.Millisecond,
		BackoffMax:  5 * time.Millisecond,
		BackoffMult: 2,
		TimeoutMS:   1000,
		Validate:    tools.ValidateBoth,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
	}
	_, err := tools.RunWithPolicy(context.Background(), json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			counter.Add(1)
			return tools.ToolResult{}, context.Canceled
		},
		nil, nil, policy,
	)
	if err == nil {
		t.Fatalf("expected permanent error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if counter.Load() != 1 {
		t.Errorf("expected 1 attempt (permanent), got %d", counter.Load())
	}
}

func TestRunWithPolicy_InvalidArgs_FailsBeforeFirstAttempt(t *testing.T) {
	var counter atomic.Int64
	policy := tools.DefaultPolicy()
	policy.BackoffBase = 1 * time.Millisecond
	policy.BackoffMax = 5 * time.Millisecond
	_, err := tools.RunWithPolicy(context.Background(), json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			counter.Add(1)
			return tools.ToolResult{}, nil
		},
		func(args json.RawMessage) error {
			return fmt.Errorf("missing required field 'name'")
		},
		nil, policy,
	)
	if err == nil {
		t.Fatalf("expected ErrToolInvalidArgs, got nil")
	}
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs, got: %v", err)
	}
	if counter.Load() != 0 {
		t.Errorf("expected 0 invocations (invalid args), got %d", counter.Load())
	}
}

func TestRunWithPolicy_OutputValidation_FailsOnFirstAttempt(t *testing.T) {
	policy := tools.ToolPolicy{
		MaxRetries:  2,
		BackoffBase: 1 * time.Millisecond,
		BackoffMax:  5 * time.Millisecond,
		BackoffMult: 2,
		TimeoutMS:   1000,
		Validate:    tools.ValidateOut,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
	}
	_, err := tools.RunWithPolicy(context.Background(), json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: "result"}, nil
		},
		nil,
		func(result tools.ToolResult) error {
			return fmt.Errorf("output doesn't match schema")
		},
		policy,
	)
	if err == nil {
		t.Fatalf("expected output-validation failure, got nil")
	}
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs (output validation wraps it), got: %v", err)
	}
}

func TestRunWithPolicy_CtxCancellation_ImmediateExit(t *testing.T) {
	policy := tools.ToolPolicy{
		MaxRetries:  3,
		BackoffBase: 50 * time.Millisecond,
		BackoffMax:  100 * time.Millisecond,
		BackoffMult: 2,
		TimeoutMS:   1000,
		Validate:    tools.ValidateBoth,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tools.RunWithPolicy(ctx, json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, fmt.Errorf("transient")
		},
		nil, nil, policy,
	)
	if err == nil {
		t.Fatalf("expected ctx cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestClassifyError_Heuristics(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		perAttempt   bool
		expectedCls  tools.ErrorClass
	}{
		{"DeadlineExceeded+perAttempt", context.DeadlineExceeded, true, tools.ErrClassTimeout},
		{"DeadlineExceeded+parent", context.DeadlineExceeded, false, tools.ErrClassPermanent},
		{"Canceled", context.Canceled, false, tools.ErrClassPermanent},
		{"InvalidArgs", tools.ErrToolInvalidArgs, false, tools.ErrClassPermanent},
		{"timeout-text", fmt.Errorf("call timeout"), false, tools.ErrClassTimeout},
		{"5xx-text", fmt.Errorf("upstream returned status 503 service unavailable"), false, tools.ErrClass5xx},
		{"transient-default", fmt.Errorf("eof reading body"), false, tools.ErrClassTransient},
	}
	for _, c := range cases {
		got := tools.ClassifyError(c.err, c.perAttempt)
		if got != c.expectedCls {
			t.Errorf("%s: got %s, want %s", c.name, got, c.expectedCls)
		}
	}
}

func TestRunWithPolicy_ZeroValuePolicy_UsesAllDefaults(t *testing.T) {
	_, err := tools.RunWithPolicy(context.Background(), json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
		func(args json.RawMessage) error {
			return fmt.Errorf("bad arg")
		},
		nil, tools.ToolPolicy{},
	)
	if err == nil || !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("zero-policy with bad args: expected ErrToolInvalidArgs, got %v", err)
	}
}

func TestRunWithPolicy_Hooks_OnAttemptFires(t *testing.T) {
	var attempts atomic.Int64
	policy := tools.ToolPolicy{
		MaxRetries:  2,
		BackoffBase: 1 * time.Millisecond,
		BackoffMax:  5 * time.Millisecond,
		BackoffMult: 2,
		TimeoutMS:   1000,
		Validate:    tools.ValidateBoth,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
	}
	_, err := tools.RunWithPolicyHooked(context.Background(), json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, fmt.Errorf("transient")
		}, nil, nil, policy,
		tools.InvokeHooks{
			OnAttempt: func(attempt int, err error) {
				attempts.Add(1)
			},
		},
	)
	if err == nil {
		t.Fatalf("expected exhaustion, got nil")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("expected 3 hook fires, got %d", got)
	}
}

func TestRunWithPolicy_PanicRecovery(t *testing.T) {
	policy := tools.ToolPolicy{
		MaxRetries:  0,
		BackoffBase: 1 * time.Millisecond,
		BackoffMax:  5 * time.Millisecond,
		BackoffMult: 2,
		TimeoutMS:   1000,
		Validate:    tools.ValidateBoth,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
	}
	_, err := tools.RunWithPolicy(context.Background(), json.RawMessage(`{}`),
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			panic("tool exploded")
		},
		nil, nil, policy,
	)
	if err == nil {
		t.Fatalf("expected panic-recovery error, got nil")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("expected error mentioning panic, got: %v", err)
	}
}
