package governance

import "sync/atomic"

// tiersource.go — the hot-swappable effective identity-tier policy the
// enforcement subsystem reads per-call. RFC §6.15 lists "Ceilings, rate
// limits, MaxTokens tiers" as HOT-RELOADABLE; this is the mechanism that
// makes an admin `governance.set_posture` write take effect on the NEXT
// PreCall with NO restart and NO per-call StateStore read on the hot path.
//
// The atomic-swap shape mirrors the key-rotation seam
// (`llm.ProviderKeyRotator`): the enforcers hold an immutable reference to
// one TierSource, and a write publishes a fresh immutable policy snapshot
// behind an `atomic.Pointer`. The CostAccumulator / RateLimiter /
// MaxTokensEnforcer read the current snapshot per PreCall via
// `Config.tierConfig` / `Config.resolveTier`, so a swap is observed on the
// very next call — cache + swap, never a DB hit per call.
//
// Concurrent reuse: the swap is a single `atomic.Pointer.Store`; every
// snapshot is immutable after publish (Store deep-copies the incoming map),
// so concurrent PreCalls read a consistent policy and a concurrent swap
// cannot tear a read (the concurrent-reuse contract). The TierSource
// carries no per-run state.

// tierPolicy is one immutable effective-policy snapshot. Never mutated
// after `TierSource.Store` publishes it — a swap replaces the whole
// pointer, so a reader either sees the old snapshot or the new one, never a
// half-updated map.
type tierPolicy struct {
	defaultTier string
	tiers       map[string]TierConfig
}

// TierSource holds the current effective identity-tier policy behind an
// atomic pointer. Built once at boot (seeded from the StateStore record
// layered over the config defaults) and shared — by reference — between the
// enforcement subsystem (which reads it per PreCall) and the
// `SetPosturePolicy` write path (which swaps it on a successful write). Safe
// for N concurrent readers + a concurrent writer.
type TierSource struct {
	cur atomic.Pointer[tierPolicy]
}

// NewTierSource builds a TierSource holding the given effective policy. The
// tier map is deep-copied so a later caller mutation cannot reach the
// published snapshot.
func NewTierSource(defaultTier string, tiers map[string]TierConfig) *TierSource {
	s := &TierSource{}
	s.Store(defaultTier, tiers)
	return s
}

// Store atomically publishes a new effective policy. The incoming map is
// deep-copied before publish so the snapshot is immutable. Observed on the
// next `resolveTier` / `tierConfig` — the hot-reload swap.
func (s *TierSource) Store(defaultTier string, tiers map[string]TierConfig) {
	s.cur.Store(&tierPolicy{defaultTier: defaultTier, tiers: copyTiers(tiers)})
}

// load returns the current immutable snapshot (never nil after
// construction).
func (s *TierSource) load() *tierPolicy {
	return s.cur.Load()
}

// DefaultTier returns the current effective default-tier assignment.
func (s *TierSource) DefaultTier() string {
	if p := s.load(); p != nil {
		return p.defaultTier
	}
	return ""
}

// Tiers returns a deep copy of the current effective tier table (always
// non-nil). Used at boot for the latent-default decision and by tests.
func (s *TierSource) Tiers() map[string]TierConfig {
	if p := s.load(); p != nil {
		return copyTiers(p.tiers)
	}
	return map[string]TierConfig{}
}
