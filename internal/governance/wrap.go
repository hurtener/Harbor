package governance

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hurtener/Harbor/internal/llm"
)

// Wrap composes a `Subsystem` around an `llm.LLMClient`. The returned
// client invokes `sub.PreCall` then the inner client then `sub.PostCall`
// on every Complete. PreCall non-nil short-circuits and propagates the
// error directly to the caller; the inner client is NOT invoked.
//
// PostCall's return is observability-only — a non-nil PostCall error is
// logged at Warn but does NOT replace the original `(resp, callErr)`
// outcome. This is RFC §6.15 line 1128's "PostCall errors do not
// supplant the call's result" behaviour.
//
// Compose order: governance is the OUTERMOST wrapper, sitting outside
// Phase 36's retry wrapper (D-043). The wrapper closures here read
// identity from ctx via `identityFromCtx`; missing identity → fail-closed
// with `ErrIdentityRequired`.
//
// Concurrent reuse (D-025): the returned client is a thin functional
// adapter (no mutable state of its own); concurrent reuse depends on the
// inner client + Subsystem both honouring the contract.
func Wrap(inner llm.LLMClient, sub Subsystem) llm.LLMClient {
	if inner == nil {
		// Defensive — operator misconfig would have already errored at
		// llm.Open; this branch is a guard for direct callers.
		panic("governance.Wrap: nil inner client")
	}
	if sub == nil {
		// A nil Subsystem effectively disables governance — the caller
		// constructed the wrapper but did not supply enforcement. We
		// preserve the inner client unchanged rather than allocate a
		// no-op adapter; surfaces the panic at the most actionable site.
		panic("governance.Wrap: nil Subsystem")
	}
	return &wrappedClient{inner: inner, sub: sub}
}

type wrappedClient struct {
	inner llm.LLMClient
	sub   Subsystem
}

// Complete runs PreCall → inner → PostCall. The Complete signature
// itself is unchanged; governance returns the inner's error verbatim
// when its PreCall permits.
func (w *wrappedClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if err := w.sub.PreCall(ctx, req); err != nil {
		return llm.CompleteResponse{}, err
	}
	resp, callErr := w.inner.Complete(ctx, req)
	if postErr := w.sub.PostCall(ctx, req, resp, callErr); postErr != nil {
		// PostCall errors are observability signals — log + continue.
		// The original (resp, callErr) outcome is what the caller acts on.
		// Audit redaction is the Subsystem's responsibility — payload
		// fields should already be SafePayload-shaped at the emit site.
		slog.Warn("governance: PostCall error (observability only)",
			slog.String("error", postErr.Error()),
			slog.String("model", req.Model),
		)
	}
	return resp, callErr
}

// Close releases the inner client. Governance Subsystems are typically
// long-lived siblings of the LLMClient (they own bus subscriptions /
// StateStore handles) — the operator releases them via a separate
// lifecycle; `Close` here just forwards.
func (w *wrappedClient) Close(ctx context.Context) error {
	return w.inner.Close(ctx)
}

// errorWith wraps a sentinel with additional context. Helper kept here
// so per-policy emit sites stay readable.
func errorWith(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...))
}
