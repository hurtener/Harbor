package tools

import (
	"context"
	"errors"
)

// ErrInvocationSuperseded refuses further invocation of an obsolete plan.
// It makes no claim about outcomes of earlier attempts or sibling calls.
var ErrInvocationSuperseded = errors.New("plan superseded; no further invocation permitted")

// ErrInvocationCleanupFailed identifies required cleanup that must terminate
// execution, rather than become an observation inviting another tool attempt.
var ErrInvocationCleanupFailed = errors.New("required invocation cleanup failed")

type invocationFenceKey struct{}

// WithInvocationFence binds dispatch to its instruction generation's signal.
// Closing the signal refuses future invocations without cancelling active ones.
func WithInvocationFence(ctx context.Context, invalidated <-chan struct{}) context.Context {
	return context.WithValue(ctx, invocationFenceKey{}, invalidated)
}

// InvocationInvalidated is nil for callers outside a fenced run.
func InvocationInvalidated(ctx context.Context) <-chan struct{} {
	signal, ok := ctx.Value(invocationFenceKey{}).(<-chan struct{})
	if !ok {
		return nil
	}
	return signal
}

// CheckInvocationFence is the admission check immediately before invocation.
func CheckInvocationFence(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-InvocationInvalidated(ctx):
		return ErrInvocationSuperseded
	default:
		return nil
	}
}
