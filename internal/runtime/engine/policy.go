package engine

import (
	"time"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// NodePolicy controls per-node reliability semantics. Zero value is
// "no policy" — Phase 10's bare worker behavior (single invocation, no
// timeout, no retry, no validation). Construct explicitly for
// production nodes; the engine never silently applies defaults (per
// AGENTS.md §5 "Fail loudly").
//
// Concurrent-reuse safe: NodePolicy is a value type. ValidateFunc is a
// function pointer; the function itself must be safe for concurrent
// invocation across the same Node.
type NodePolicy struct {
	// Validate selects which side(s) of the invocation the worker
	// runs ValidateFunc on. Zero value is ValidateNone (no validation
	// — matches the Phase 10 bare-worker behavior).
	Validate ValidateMode
	// TimeoutMS is the per-invocation deadline in milliseconds. 0
	// means "no timeout" — the worker invokes Func with the engine's
	// ctx unchanged. > 0 wraps each invocation in
	// context.WithTimeout(ctx, time.Duration(TimeoutMS) * Millisecond).
	TimeoutMS int
	// MaxRetries is the count of retry attempts AFTER the initial
	// invocation. 0 means "no retries" (the node runs exactly once on
	// failure). Total invocations = MaxRetries + 1.
	MaxRetries int
	// BackoffBase is the first-retry sleep before retry attempt 1.
	// Subsequent retries multiply by BackoffMult, capped at
	// MaxBackoff. 0 means no sleep between retries.
	BackoffBase time.Duration
	// BackoffMult is the multiplier between successive retry sleeps.
	// 0 or 1 means "no growth" (linear retries at BackoffBase). 2 is
	// the canonical exponential value.
	BackoffMult float64
	// MaxBackoff caps the per-retry sleep regardless of BackoffMult.
	// 0 means no cap.
	MaxBackoff time.Duration
	// ValidateFunc is the function pointer the worker calls on input
	// (Validate=Both/In) and/or output (Validate=Both/Out) envelopes.
	// nil with Validate != ValidateNone is a no-op (the worker treats
	// it as "no validator configured" — fail-loud at construction
	// time is the engine's responsibility, not the shell's; Phase 10
	// could harden this if needed).
	ValidateFunc func(messages.Envelope) error
}

// ValidateMode selects which side of a node invocation the worker
// passes through ValidateFunc.
type ValidateMode string

const (
	// ValidateNone disables validation. The perf escape hatch for hot
	// streaming paths (Phase 12 will lean on this for stream nodes).
	// Zero value of ValidateMode.
	ValidateNone ValidateMode = ""
	// ValidateBoth runs ValidateFunc on the input AND output envelope.
	// The default for production nodes — fail-loud per CLAUDE.md §5.
	ValidateBoth ValidateMode = "both"
	// ValidateIn runs ValidateFunc on the input only.
	ValidateIn ValidateMode = "in"
	// ValidateOut runs ValidateFunc on the output only.
	ValidateOut ValidateMode = "out"
)

// shouldValidateIn reports whether the policy validates input envelopes.
func (p NodePolicy) shouldValidateIn() bool {
	return p.ValidateFunc != nil && (p.Validate == ValidateBoth || p.Validate == ValidateIn)
}

// shouldValidateOut reports whether the policy validates output envelopes.
func (p NodePolicy) shouldValidateOut() bool {
	return p.ValidateFunc != nil && (p.Validate == ValidateBoth || p.Validate == ValidateOut)
}
