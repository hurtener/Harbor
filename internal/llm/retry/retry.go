// Package retry is Harbor's retry-with-feedback wrapper (Phase 36 —
// RFC §6.5).
//
// When `CompleteRequest.Validator` is non-nil, the wrapper runs the
// validator after each successful `Complete`. A non-nil validator
// error triggers a corrective re-ask: the wrapper appends a system
// message to the original request describing the failure, then re-
// invokes the inner client. The loop is bounded by
// `ModelProfile.MaxRetries` (default 1) and emits one
// `llm.retry_with_feedback` event per retry.
//
// Composition: the retry wrapper is the OUTERMOST layer (D-043):
//
//	Open() → retry(downgrade(corrections(safety(driver))))
//
// — every retry attempt flows a fresh corrective turn through the
// downgrade + corrections + safety stack. If the validator's error
// matches `IsInvalidJSONSchemaError`, the inner downgrade chain
// engages within the same attempt before the retry counter advances.
//
// A `nil` Validator (the common case) makes the wrapper a pure
// pass-through.
//
// Concurrent-reuse (D-025): the wrapper is stateless across calls.
// `Wrap` returns a value holding the inner `LLMClient`, the snapshot,
// and the deps; all are read-only after construction.
package retry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// init registers `Wrap` as the retry-wrapper hook in the `llm`
// package. The production binary blank-imports this package.
func init() {
	llm.RegisterRetryWrapper(Wrap)
}

// Wrap composes the retry-with-feedback layer on top of `inner`.
//
// Nil `inner` panics — composition error caught at boot.
func Wrap(inner llm.LLMClient, cfg llm.ConfigSnapshot, deps llm.Deps) llm.LLMClient {
	if inner == nil {
		panic("retry.Wrap: inner is nil")
	}
	return &client{inner: inner, cfg: cfg, deps: deps}
}

type client struct {
	inner  llm.LLMClient
	cfg    llm.ConfigSnapshot
	deps   llm.Deps
	closed atomic.Bool
}

var _ llm.LLMClient = (*client)(nil)

// Complete runs the validator-driven retry loop. When the request has
// no Validator, the call is a pure pass-through.
func (c *client) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if c.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	if req.Validator == nil {
		return c.inner.Complete(ctx, req)
	}

	// Default the model from the snapshot when the caller left it empty
	// (the react planner does — see safety.go). This wrapper is the
	// outermost profile-consumer; without the default `resolveMaxRetries`
	// would key `ModelProfiles[""]` and silently fall back to the global
	// default instead of the model's configured MaxRetries.
	if req.Model == "" {
		req.Model = c.cfg.Model
	}

	maxRetries := resolveMaxRetries(c.cfg, req.Model)
	id := identityFromCtx(ctx)

	var (
		reasons    []string
		current    = req
		lastResp   llm.CompleteResponse
		lastValErr error
	)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return llm.CompleteResponse{}, err
		}

		resp, err := c.inner.Complete(ctx, current)
		if err != nil {
			return resp, err
		}

		valErr := req.Validator(resp)
		if valErr == nil {
			return resp, nil
		}

		// Validator rejected. Record + emit + augment for next loop.
		reasons = append(reasons, valErr.Error())
		lastResp = resp
		lastValErr = valErr

		// If we've already burned the last retry, surface the chain.
		if attempt == maxRetries {
			break
		}

		nextAttempt := attempt + 1
		emitRetryWithFeedback(ctx, c.deps.Bus, id, req.Model, nextAttempt, maxRetries, valErr)

		current = appendCorrectiveTurn(req, resp, valErr)
	}

	// Bound exceeded. Surface the wrapped chain.
	return lastResp, fmt.Errorf("%w: %d retries: %s",
		llm.ErrRetryExhausted, maxRetries, summariseReasons(reasons, lastValErr))
}

// Close marks the wrapper closed and tears down the inner. Idempotent.
func (c *client) Close(ctx context.Context) error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return c.inner.Close(ctx)
}

// resolveMaxRetries reads `ModelProfile.MaxRetries` for the given
// model. Returns `DefaultMaxRetries` when no profile exists or
// `MaxRetries` is unset (zero). Negative values are rejected at config
// validation; defensively we clamp to zero here.
func resolveMaxRetries(cfg llm.ConfigSnapshot, model string) int {
	if p, ok := cfg.ModelProfiles[model]; ok {
		if p.MaxRetries > 0 {
			return p.MaxRetries
		}
		// Zero hit the defaults pass at `applyDefaults`, but if a
		// programmatic caller bypassed it, fall through to the default.
	}
	return llm.DefaultMaxRetries
}

// appendCorrectiveTurn returns a copy of `req` with the rejected
// response and the validator's complaint appended as a user-role
// message asking for a corrected response. Mutates neither the
// original request nor its message slice.
//
// The corrective turn is shaped as a user message describing the
// validation failure; we don't impersonate the assistant. This keeps
// the conversation history coherent for downstream observers (audit,
// memory).
func appendCorrectiveTurn(req llm.CompleteRequest, badResp llm.CompleteResponse, validatorErr error) llm.CompleteRequest {
	out := req
	clone := make([]llm.ChatMessage, 0, len(req.Messages)+2)
	clone = append(clone, req.Messages...)

	// Echo the assistant's rejected output back into the thread so the
	// model sees what was wrong with its own response.
	if badResp.Content != "" {
		assistantContent := badResp.Content
		clone = append(clone, llm.ChatMessage{
			Role:    llm.RoleAssistant,
			Content: llm.Content{Text: &assistantContent},
		})
	}

	complaint := "Your previous response failed validation: " +
		truncate(validatorErr.Error(), 256) +
		". Please respond again, addressing this issue exactly."
	clone = append(clone, llm.ChatMessage{
		Role:    llm.RoleUser,
		Content: llm.Content{Text: &complaint},
	})
	out.Messages = clone
	return out
}

// summariseReasons builds the chain history string surfaced in the
// `ErrRetryExhausted` wrap. Bounded — each reason truncated.
func summariseReasons(reasons []string, last error) string {
	if len(reasons) == 0 && last != nil {
		return last.Error()
	}
	parts := make([]string, 0, len(reasons))
	for i, r := range reasons {
		parts = append(parts, fmt.Sprintf("attempt=%d reason=%s", i, truncate(r, 160)))
	}
	return strings.Join(parts, " | ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// identityFromCtx mirrors the safety pass's helper.
func identityFromCtx(ctx context.Context) identity.Quadruple {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q
	}
	id, _ := identity.From(ctx)
	return identity.Quadruple{Identity: id}
}

// emitRetryWithFeedback publishes the `llm.retry_with_feedback`
// event. Best-effort; never blocks on the bus.
func emitRetryWithFeedback(ctx context.Context, bus events.EventBus, id identity.Quadruple, model string, attempt, maxRetries int, cause error) {
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort telemetry emit — must not block the LLM retry path.
		Type:       llm.EventTypeRetryWithFeedback,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload: llm.RetryWithFeedbackPayload{
			Identity:   id,
			Model:      model,
			Attempt:    attempt,
			MaxRetries: maxRetries,
			Reason:     truncate(cause.Error(), 256),
			OccurredAt: time.Now(),
		},
	})
}

// errorIsSentinel is a small helper for tests; not exported to
// avoid widening the public surface.
//
//nolint:unused // referenced by tests via export_test.go pattern.
func errorIsSentinel(err, sentinel error) bool {
	return errors.Is(err, sentinel)
}
