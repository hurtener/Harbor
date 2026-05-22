package governance

// posture.go — the Phase 72g (D-112) read-only posture accessor over the
// configured governance policy. The `governance.posture` Protocol method
// (Phase 72g) consumes a PostureProvider; the provider returns a deep-
// copied immutable Snapshot of the operator-configured `IdentityTiers`
// map plus the `DefaultTier` selector and the caller-resolved tier.
//
// Why a separate type, not a method on the enforcement Subsystem: the
// `Subsystem` interface is the enforcement seam (PreCall / PostCall) —
// it carries no read-only-config accessor, and the three concrete
// enforcers (CostAccumulator, RateLimiter, MaxTokensEnforcer) each hold
// only the slice of Config their policy needs. The posture surface
// wants the WHOLE configured shape; the natural source of truth is the
// `governance.Config` value the binary built at boot. PostureProvider
// wraps that Config and exposes the read projection.
//
// Concurrent reuse (D-025): PostureProvider is immutable after
// construction — `cfg` is set once and never mutated. `Posture` reads
// the configured map and DEEP-COPIES it before returning, so a caller
// that mutates the returned Snapshot cannot reach back into the
// provider's Config. Safe to share across N concurrent goroutines.

import (
	"context"
	"fmt"
)

// PostureProvider is the Phase 72g read-only accessor over a configured
// `governance.Config`. Built once per Runtime process via
// NewPostureProvider; `Posture` is safe for concurrent use by N
// goroutines (D-025).
type PostureProvider struct {
	cfg Config
}

// NewPostureProvider builds a PostureProvider over the operator-supplied
// governance configuration. The Config is copied by value at
// construction; the provider holds its own immutable copy. A latent
// (zero-value) Config is valid — `Posture` returns an empty
// `IdentityTiers` map and empty tier selectors, which the Console
// renders as the explicit "No tiers configured" state.
func NewPostureProvider(cfg Config) *PostureProvider {
	return &PostureProvider{cfg: cfg}
}

// Snapshot is a deep-copied, immutable view of the configured governance
// posture: the `IdentityTiers` map, the `DefaultTier` selector, and the
// tier the caller's identity resolves to. Mutating any field of a
// returned Snapshot (including the `IdentityTiers` map) does NOT mutate
// the provider's underlying Config — the snapshot is a defensive deep
// copy.
type Snapshot struct {
	IdentityTiers map[string]TierConfig
	DefaultTier   string
	ResolvedTier  string
}

// Posture returns a deep-copied Snapshot of the configured governance
// posture for the caller's identity. Identity is mandatory (CLAUDE.md §6
// rule 9, RFC §5.5): a missing / incomplete identity in `ctx` fails
// loudly with wrapped `ErrIdentityRequired` — there is no opt-out.
//
// The returned `IdentityTiers` map is a fresh deep copy; the
// `ResolvedTier` is computed by applying the Config's `TierResolver`
// (falling back to `DefaultTier`) to the caller's identity. A nil
// resolver makes `ResolvedTier == DefaultTier` for every caller.
func (p *PostureProvider) Posture(ctx context.Context) (Snapshot, error) {
	id, err := identityFromCtx(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("governance: posture read: %w", err)
	}

	// Deep-copy the tier map so a caller mutating the snapshot cannot
	// reach back into the provider's Config (D-025 — no shared mutable
	// state crossing the call boundary). Always non-nil.
	tiers := make(map[string]TierConfig, len(p.cfg.IdentityTiers))
	for name, tc := range p.cfg.IdentityTiers {
		// TierConfig is a value type whose only nested field
		// (RateLimitConfig) is itself a value type — a plain map copy
		// is a full deep copy.
		tiers[name] = tc
	}

	return Snapshot{
		DefaultTier:   p.cfg.DefaultTier,
		ResolvedTier:  p.cfg.resolveTier(id),
		IdentityTiers: tiers,
	}, nil
}
