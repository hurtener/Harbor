package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// runWithReliability wraps a single NodeFunc invocation with the
// reliability shell:
//
//  1. Optional input validation per NodePolicy.Validate.
//  2. Per-attempt invocation under context.WithTimeout (when
//     TimeoutMS > 0); panic recovery; ctx-cancel mapping.
//  3. On error, if attempts < MaxRetries+1, sleep nextBackoff and
//     retry. ctx.Done() between retries aborts the loop with
//     CodeRunCancelled.
//  4. On terminal failure, build a *RunError tagged with the
//     envelope's identity quadruple and return it. The worker emits
//     to the logger + bus via the Phase 04→05 wiring.
//  5. Optional output validation on the successful path.
//
// Concurrent-reuse safe: all per-invocation state lives on the
// goroutine stack (attempt counter, last err, validate calls). The
// NodePolicy is a value type captured by value; ValidateFunc is a
// shared pointer the caller guarantees concurrent-safe.
//
// Worker integration: this is the function the Phase 11 worker loop
// calls in place of the bare `node.Func(ctx, env, nctx)` Phase 10
// shipped. On nil err, the returned envelope flows downstream; on
// non-nil, the worker emits the *RunError to the logger and continues.
func runWithReliability(
	ctx context.Context,
	in messages.Envelope,
	fn NodeFunc,
	policy NodePolicy,
	nctx *NodeContext,
	nodeName string,
	jitter func() float64,
) (messages.Envelope, error) {
	// 1. Input validation.
	if policy.shouldValidateIn() {
		if err := policy.ValidateFunc(in); err != nil {
			return messages.Envelope{}, newRunError(in, nodeName, CodeValidationFailed,
				fmt.Sprintf("input validation failed: %v", err), err, map[string]any{"side": "in"})
		}
	}

	// Default jitter source.
	if jitter == nil {
		jitter = rand.Float64
	}

	totalAttempts := policy.MaxRetries + 1
	if totalAttempts <= 0 {
		totalAttempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < totalAttempts; attempt++ {
		// Honor ctx cancellation before each (re)try.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return messages.Envelope{}, newRunError(in, nodeName, CodeRunCancelled,
				"run cancelled", ctxErr, map[string]any{"attempt": attempt})
		}

		// Sleep before retries (attempt > 0 means we're retrying).
		if attempt > 0 {
			delay := nextBackoff(attempt, policy.BackoffBase, policy.MaxBackoff, policy.BackoffMult, jitter)
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return messages.Envelope{}, newRunError(in, nodeName, CodeRunCancelled,
						"run cancelled during backoff", ctx.Err(),
						map[string]any{"attempt": attempt, "backoff_ms": delay.Milliseconds()})
				case <-timer.C:
				}
			}
		}

		// Per-attempt invocation context. Honors TimeoutMS.
		invokeCtx := ctx
		var cancel context.CancelFunc
		if policy.TimeoutMS > 0 {
			invokeCtx, cancel = context.WithTimeout(ctx, time.Duration(policy.TimeoutMS)*time.Millisecond)
		}

		out, invokeErr := safeInvoke(invokeCtx, fn, in, nctx)
		if cancel != nil {
			cancel()
		}

		if invokeErr == nil {
			// 5. Output validation on success.
			if policy.shouldValidateOut() {
				if vErr := policy.ValidateFunc(out); vErr != nil {
					return messages.Envelope{}, newRunError(in, nodeName, CodeValidationFailed,
						fmt.Sprintf("output validation failed: %v", vErr), vErr,
						map[string]any{"side": "out", "attempt": attempt})
				}
			}
			return out, nil
		}

		lastErr = invokeErr

		// Cancel precedence over timeout: if the invokeCtx died
		// because the parent ctx was cancelled (not because TimeoutMS
		// fired), terminate immediately.
		if parentErr := ctx.Err(); parentErr != nil {
			return messages.Envelope{}, newRunError(in, nodeName, CodeRunCancelled,
				"run cancelled mid-invocation", parentErr,
				map[string]any{"attempt": attempt})
		}
	}

	// Terminal failure: classify and return.
	code := classify(lastErr, policy)
	msg := fmt.Sprintf("node failed after %d attempt(s)", totalAttempts)
	return messages.Envelope{}, newRunError(in, nodeName, code, msg, lastErr,
		map[string]any{"attempts": totalAttempts})
}

// classify maps a final attempt's error to a RunErrorCode. The shell
// distinguishes timeout (per-invocation TimeoutMS hit) from generic
// node exceptions.
func classify(err error, policy NodePolicy) RunErrorCode {
	if err == nil {
		return CodeNodeException
	}
	// context.DeadlineExceeded only fires from a per-invocation
	// timeout (TimeoutMS > 0); the parent ctx errors are caught
	// earlier in the loop and mapped to CodeRunCancelled.
	if errors.Is(err, context.DeadlineExceeded) && policy.TimeoutMS > 0 {
		return CodeNodeTimeout
	}
	if errors.Is(err, context.Canceled) {
		return CodeRunCancelled
	}
	return CodeNodeException
}

// newRunError populates a *RunError from the failing envelope's
// identity quadruple + the per-attempt context. Centralises the
// "carry identity into RunError" contract so every code path
// satisfies the audit-subscriber filter.
func newRunError(in messages.Envelope, nodeName string, code RunErrorCode, msg string, cause error, meta map[string]any) *RunError {
	q := in.Identity()
	return &RunError{
		RunID:     q.RunID,
		TenantID:  q.TenantID,
		UserID:    q.UserID,
		SessionID: q.SessionID,
		NodeName:  nodeName,
		NodeID:    nodeName, // V1: NodeID == NodeName.
		Code:      code,
		Message:   msg,
		Cause:     cause,
		Metadata:  meta,
	}
}
