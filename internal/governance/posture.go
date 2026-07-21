package governance

// posture.go — the read-only posture accessor over the
// configured governance policy. The `governance.posture` Protocol method
// consumes a PostureProvider; the provider returns a deep-
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
// Concurrent reuse: PostureProvider is immutable after
// construction — `cfg` is set once and never mutated. `Posture` reads
// the configured map and DEEP-COPIES it before returning, so a caller
// that mutates the returned Snapshot cannot reach back into the
// provider's Config. Safe to share across N concurrent goroutines.

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/state"
)

// PostureProvider is the read-only accessor over the effective governance
// identity-tier policy — the StateStore-backed policy record (written via
// `governance.set_posture`) layered over the operator-configured
// `governance.Config` defaults. Built once per Runtime process via
// NewPostureProvider / NewPostureProviderWithState; `Posture` is safe for
// concurrent use by N goroutines.
type PostureProvider struct {
	cfg Config
	// state is the StateStore the effective-policy layer reads through.
	// Optional — a nil state (the config-only posture) makes `Posture`
	// return exactly the config defaults, so a runtime that wires no
	// StateStore (or a test) is unaffected.
	state state.StateStore
}

// NewPostureProvider builds a PostureProvider over the operator-supplied
// governance configuration WITHOUT a StateStore layer — `Posture` returns
// exactly the config defaults. A latent (zero-value) Config is valid —
// `Posture` returns an empty `IdentityTiers` map and empty tier selectors,
// which the Console renders as the explicit "No tiers configured" state.
func NewPostureProvider(cfg Config) *PostureProvider {
	return &PostureProvider{cfg: cfg}
}

// NewPostureProviderWithState builds a PostureProvider whose `Posture`
// reflects the StateStore-backed effective policy layered over the config
// defaults: when a `governance.set_posture` record has been written, the
// provider returns that record's tiers + default tier; otherwise it returns
// the config-declared defaults (additive / backward-compatible). `st` is
// the StateStore the runtime persists the policy record through.
func NewPostureProviderWithState(cfg Config, st state.StateStore) *PostureProvider {
	return &PostureProvider{cfg: cfg, state: st}
}

// Snapshot is a deep-copied, immutable view of the configured governance
// posture: the `IdentityTiers` map, the `DefaultTier` selector, and the
// tier the caller's identity resolves to. Mutating any field of a
// returned Snapshot (including the `IdentityTiers` map) does NOT mutate
// the provider's underlying Config — the snapshot is a defensive deep
// copy.
type Snapshot struct {
	// DefaultTier is the operator-configured default tier name. Empty
	// when no default is configured.
	DefaultTier string
	// ResolvedTier is the tier the caller's identity resolves to via the
	// configured TierResolver (or DefaultTier when no resolver is set).
	// Empty when no tier resolves.
	ResolvedTier string
	// IdentityTiers is a deep copy of the configured tier map. Always
	// non-nil — an empty map signals no enforcement (latent default).
	IdentityTiers map[string]TierConfig
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

	// Read the effective policy — the StateStore record (written via
	// set_posture) layered over the config defaults, else the config
	// defaults. loadEffectivePolicy deep-copies the tier map so a caller
	// mutating the snapshot cannot reach the provider's Config or the
	// persisted record (no shared mutable state crossing the call
	// boundary). Always non-nil.
	defaultTier, tiers, err := loadEffectivePolicy(ctx, p.state, p.cfg)
	if err != nil {
		return Snapshot{}, fmt.Errorf("governance: posture read: %w", err)
	}

	// Resolve the caller's tier against the EFFECTIVE default tier (the
	// written record's default when present), applying the config's
	// Resolver — a nil resolver makes ResolvedTier == the effective
	// DefaultTier for every caller.
	resolved := defaultTier
	if p.cfg.Resolver != nil {
		if name := p.cfg.Resolver(id); name != "" {
			resolved = name
		}
	}

	return Snapshot{
		DefaultTier:   defaultTier,
		ResolvedTier:  resolved,
		IdentityTiers: tiers,
	}, nil
}
