// Package finish ships Harbor's stub Planner — a planner that always
// returns Finish{Reason: Goal}. The stub exists to prove the Planner
// interface holds end-to-end at Phase 42:
//
//   - It compiles against `internal/planner.Planner`.
//   - It demonstrates the §13 import-graph contract (no
//     `internal/runtime/...` imports).
//   - It exercises the D-025 concurrent-reuse contract — a shared
//     stub instance handles N=128 concurrent Next calls under -race
//     without races, context bleed, cancel cross-talk, or goroutine
//     leaks.
//
// Production planner concretes land at Phase 45 (ReAct) and Phase 48
// (Deterministic). The stub stays in V1 as a test fixture for the
// conformance pack + integration tests that need a deterministic
// terminal result.
package finish

import (
	"context"

	"github.com/hurtener/Harbor/internal/planner"
)

// Planner is the stub planner. It always returns
// Finish{Reason: Goal}; the optional payload is configurable via
// WithPayload / WithMetadata. The planner is goroutine-safe: no
// per-run mutable state lives on the receiver.
//
// Implements planner.WakeAware → WakePush (the safe default for a
// concrete that never spawns non-retain-turn tasks).
type Planner struct {
	// payload is the fixed Finish.Payload value. May be nil.
	payload any
	// metadata is the fixed Finish.Metadata template. The Next path
	// shallow-copies it and adds run_id for per-call identity
	// round-trip in the conformance pack.
	metadata map[string]any
}

// Option is the functional-options constructor knob set.
type Option func(*Planner)

// WithPayload sets the fixed Finish.Payload value. Default: nil.
func WithPayload(payload any) Option {
	return func(p *Planner) {
		p.payload = payload
	}
}

// WithMetadata sets the fixed Finish.Metadata template. The Next
// path shallow-copies the map and adds a `run_id` entry sourced from
// RunContext.Quadruple.RunID so the conformance pack (and the
// concurrent-reuse test) can assert per-call identity round-trip.
//
// Default: nil (no metadata).
func WithMetadata(metadata map[string]any) Option {
	return func(p *Planner) {
		p.metadata = metadata
	}
}

// New constructs a stub planner. The returned value satisfies
// planner.Planner and planner.WakeAware.
//
// The stub is safe for concurrent reuse (D-025) — no per-call state
// lives on the receiver.
func New(opts ...Option) *Planner {
	p := &Planner{}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Next is the planner contract. The stub always returns
// Finish{Reason: planner.FinishGoal} with the configured payload +
// per-call metadata (including the run's RunID).
//
// Honours ctx.Err() — when the context is cancelled before Next runs,
// returns nil + ctx.Err() so the runtime executor can surface the
// cancellation as the standard ctx-derived error rather than a stub-
// invented one.
func (p *Planner) Next(ctx context.Context, run planner.RunContext) (planner.Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Shallow-copy the configured metadata template and stamp the
	// per-call RunID so the conformance pack + the D-025 concurrent-
	// reuse test can assert per-call identity round-trip.
	var meta map[string]any
	if p.metadata != nil || run.Quadruple.RunID != "" {
		meta = make(map[string]any, len(p.metadata)+1)
		for k, v := range p.metadata {
			meta[k] = v
		}
		if run.Quadruple.RunID != "" {
			meta["run_id"] = run.Quadruple.RunID
		}
	}

	return planner.Finish{
		Reason:   planner.FinishGoal,
		Payload:  p.payload,
		Metadata: meta,
	}, nil
}

// WakeMode declares the stub's wake-on-resolution strategy (D-032).
// The stub never spawns non-retain-turn tasks, so the choice is
// structurally irrelevant; WakePush is the documented default the
// conformance pack assumes for non-WakeAware planners (the stub
// implements the interface explicitly so the conformance pack can
// exercise the WakeAware code path).
func (p *Planner) WakeMode() planner.WakeMode {
	return planner.WakePush
}

// Verify the interface satisfaction at compile time.
var (
	_ planner.Planner   = (*Planner)(nil)
	_ planner.WakeAware = (*Planner)(nil)
)
