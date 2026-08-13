package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// wrapDescriptorLifecycle returns a copy of d whose Invoke is wrapped in
// the ONE universal lifecycle-emitting shell. The catalog applies this
// at Register time when it carries a bus, so EVERY registered
// descriptor — in-process, MCP, HTTP, A2A, flow — publishes
// tool.invoked / tool.completed / tool.failed (and the invalid-args /
// policy-exhausted variants) around each invocation, regardless of
// transport driver. This is the runtime-owned dispatch record: it lives
// on the descriptor shell every transport's Invoke passes through, not
// per-transport-driver, so all dispatch call sites (single-tool
// executor, parallel branches, the MCP-Apps proxy, declarative-action
// re-invoke) inherit the emit by construction.
//
// The event envelope + payload carry the FULL run quadruple read from
// the invocation ctx, so every tool event is turn-attributable on both
// the live stream and the durable read-back. Payloads carry the tool
// NAME + transport + status + attempts + duration ONLY — never args or
// results (CLAUDE.md §7 rule 7).
//
// A nil bus returns d unchanged (no shell), so a catalog constructed
// without a bus is a pure no-op — the descriptor's own Invoke is used
// verbatim. A descriptor with a nil Invoke is returned unchanged (the
// caller's Register/Replace validation rejects it separately).
func wrapDescriptorLifecycle(bus events.EventBus, d ToolDescriptor) ToolDescriptor {
	if bus == nil || d.Invoke == nil {
		return d
	}
	inner := d.Invoke
	name := d.Tool.Name
	transport := d.Tool.Transport
	wrapped := d
	wrapped.Invoke = func(ctx context.Context, args json.RawMessage) (ToolResult, error) {
		start := time.Now()
		publishToolInvoked(ctx, bus, name, transport, start)
		// The outcome emit is INLINE, not deferred — byte-identical to the
		// per-transport emit this shell supersedes. A genuine panic inside the
		// inner Invoke therefore skips the outcome event (the run fails loudly
		// regardless). A bare `defer` would be wrong here: on panic the named
		// error is nil, so it would emit tool.completed for a panicking call;
		// the only correct panic-hardening is recover-emit-failed-then-repanic,
		// which is not worth the overhead on this hot dispatch path for a case
		// §5 forbids in production code anyway ("no panic in production paths").
		result, err := inner(ctx, args)
		publishToolOutcome(ctx, bus, name, transport, start, err)
		return result, err
	}
	return wrapped
}

// lifecycleQuad reads the run identity from ctx. Prefers the full
// Quadruple (RunID present) so the tool event is turn-attributable;
// falls back to the identity triple (empty RunID) when only that is
// present. Returns ok=false when no identity is in ctx at all — the tool
// boundary already rejects missing identity, so the emit is skipped
// defensively rather than published with an empty envelope.
func lifecycleQuad(ctx context.Context) (identity.Quadruple, bool) {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q, true
	}
	if id, ok := identity.From(ctx); ok {
		return identity.Quadruple{Identity: id}, true
	}
	return identity.Quadruple{}, false
}

// publishToolInvoked emits tool.invoked on the bus. A missing identity
// is treated as "publish skipped" (defensive — the tool boundary rejects
// missing identity before this point).
func publishToolInvoked(ctx context.Context, bus events.EventBus, name string, transport TransportKind, started time.Time) {
	q, ok := lifecycleQuad(ctx)
	if !ok {
		return
	}
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; the tool result is the source of truth.
		Type:       EventTypeToolInvoked,
		Identity:   q,
		OccurredAt: started,
		Payload: ToolInvokedPayload{
			Identity:  q,
			ToolName:  name,
			Transport: transport,
			StartedAt: started,
		},
	})
}

// publishToolOutcome emits tool.completed on a successful invocation and
// the matching failure variant on a terminal failure. Error
// classification mirrors the transport-agnostic shell contract:
// ErrToolInvalidArgs → tool.invalid_args, ErrToolPolicyExhausted →
// tool.policy_exhausted, any other error → tool.failed (permanent
// class). The policy shell (retries) runs INSIDE the wrapped Invoke, so
// the shell-level emit observes the TERMINAL outcome, not per-attempt
// internals; Attempts is reported as the shell's terminal attempt count
// (1 for a resolved call) rather than the internal retry count.
func publishToolOutcome(ctx context.Context, bus events.EventBus, name string, transport TransportKind, started time.Time, err error) {
	q, ok := lifecycleQuad(ctx)
	if !ok {
		return
	}
	dur := time.Since(started).Milliseconds()
	var policyErr *PolicyError
	_ = errors.As(err, &policyErr)
	if err == nil {
		_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; the tool result is the source of truth.
			Type:       EventTypeToolCompleted,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: ToolCompletedPayload{
				Identity:   q,
				ToolName:   name,
				Transport:  transport,
				Attempts:   1,
				DurationMS: dur,
			},
		})
		return
	}
	switch {
	case errors.Is(err, ErrToolInvalidArgs):
		_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; the tool result is the source of truth.
			Type:       EventTypeToolInvalidArgs,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: ToolInvalidArgsPayload{
				Identity:        q,
				ToolName:        name,
				Transport:       transport,
				ValidationError: err.Error(),
			},
		})
	case errors.Is(err, ErrToolPolicyExhausted):
		_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; the tool result is the source of truth.
			Type:       EventTypeToolPolicyExhausted,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: ToolPolicyExhaustedPayload{
				Identity:         q,
				ToolName:         name,
				Transport:        transport,
				Attempts:         policyAttempts(policyErr, 0),
				LastClass:        policyClass(policyErr, ErrClassPermanent),
				ConfiguredBudget: policyBudget(policyErr, 0),
				LastError:        err.Error(),
			},
		})
	default:
		// A downstream insufficient-scope step-up enriches the SAME
		// tool.failed event with its structured shortfall projection, so an
		// operator sees the required-vs-granted scope gap without parsing the
		// error string. Nil for every other failure.
		var shortfall *ScopeShortfallDetail
		var scopeErr *ErrInsufficientScope
		if errors.As(err, &scopeErr) {
			d := scopeErr.ShortfallDetail()
			shortfall = &d
		}
		_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; the tool result is the source of truth.
			Type:       EventTypeToolFailed,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: ToolFailedPayload{
				Identity:         q,
				ToolName:         name,
				Transport:        transport,
				Attempts:         policyAttempts(policyErr, 1),
				ErrorClass:       policyClass(policyErr, ErrClassPermanent),
				ConfiguredBudget: policyBudget(policyErr, 1),
				ErrorMessage:     err.Error(),
				ScopeShortfall:   shortfall,
			},
		})
	}
}

func policyAttempts(err *PolicyError, fallback int) int {
	if err == nil {
		return fallback
	}
	return err.Attempts
}
func policyBudget(err *PolicyError, fallback int) int {
	if err == nil {
		return fallback
	}
	return err.Budget
}
func policyClass(err *PolicyError, fallback ErrorClass) ErrorClass {
	if err == nil {
		return fallback
	}
	return err.Class
}
