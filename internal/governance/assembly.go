// assembly.go — the governance enforcement assembly.
// Previously the enforcers (cost accumulator, rate limiter,
// MaxTokens enforcer) and the `SetFactory` seam all shipped with zero
// production consumers — a populated `governance.identity_tiers` map
// drove only the read-only posture surface (the SDK friction audit §1
// finding; a §13 primitive-without-consumer standing violation). This
// file is the one composition helper both consumption paths share:
//
//   - Zero-ceremony (binary + devstack): `assemble.Assemble` builds the
//     Subsystem from the operator config and installs it via
//     `SetFactory` before `llm.Open` composes the wrapper chain.
//   - Headless / multi-runtime: an embedder calls
//     `NewSubsystemFromConfig` directly and composes
//     `Wrap(client, sub)` per stack — no process-global state.

package governance

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/state"
)

// NewSubsystemFromConfig composes the V1 enforcement Subsystem from an
// operator-shaped `Config`. The members fan out in the documented
// cheapest-reject-first order (see `NewCompound`'s godoc):
//
//  1. MaxTokensEnforcer — purely in-memory cap check; cheapest reject.
//  2. RateLimiter — bucket drain (lock + small state lookup).
//  3. CostAccumulator — accumulator lookup (state I/O).
//
// Latent default: an empty `cfg.IdentityTiers` map returns
// `(nil, nil)` — the ONE sanctioned "no enforcement" state. The
// `llm.Open` wrapper hook treats a nil Subsystem as pass-through, and
// the posture surface reports the empty map verbatim, so the latency is
// visible, never silent.
//
// Fail-loud: a non-empty tier map with a nil store or nil bus returns a
// wrapped `ErrInvalidConfig` — enforcement without persistence or
// observability is not a degraded mode, it is a misconfiguration
// (CLAUDE.md §13, no silent degradation).
//
// Lifecycle: the returned Subsystem owns no goroutines and no
// persistent handles beyond the caller-owned store/bus (each member's
// Close only flips a closed flag), so it rides the caller's lifecycle —
// the production assembly keeps it for the stack's lifetime; a headless
// embedder disposes it with the wrapped client.
//
// Concurrent reuse: one returned Subsystem is safe to share
// across N concurrent goroutines — each member carries its own
// guarantee (atomics + per-key mutexes; per-run scope flows via ctx).
func NewSubsystemFromConfig(cfg Config, store state.StateStore, bus events.EventBus) (Subsystem, error) {
	if len(cfg.IdentityTiers) == 0 {
		// latent default: no tiers, no enforcement, no Subsystem.
		return nil, nil
	}
	if store == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required when identity tiers are configured", ErrInvalidConfig)
	}
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is required when identity tiers are configured", ErrInvalidConfig)
	}
	limiter, err := NewRateLimiter(store, bus, cfg)
	if err != nil {
		return nil, fmt.Errorf("governance: rate limiter: %w", err)
	}
	accumulator, err := NewCostAccumulator(store, bus, cfg)
	if err != nil {
		return nil, fmt.Errorf("governance: cost accumulator: %w", err)
	}
	return NewCompound(NewMaxTokensEnforcer(bus, cfg), limiter, accumulator), nil
}
