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
	ValidateFunc func(messages.Envelope) error
	Validate     ValidateMode
	TimeoutMS    int
	MaxRetries   int
	BackoffBase  time.Duration
	BackoffMult  float64
	MaxBackoff   time.Duration
	RunCapacity  int
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
