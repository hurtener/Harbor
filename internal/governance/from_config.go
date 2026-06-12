// from_config.go — the exported config→governance projection (Phase
// the config consolidation). Previously the `config.GovernanceConfig` →
// `governance.Config` projection existed twice — unexported
// `governanceConfigFromConfig` in `cmd/harbor/cmd_dev.go` and its
// hand-maintained `governanceConfigForDevstack` mirror in
// `harbortest/devstack` — the exact silent-field-drop mechanism.
// The projection now lives next to the type it fills.
//
// Import direction: the subsystem imports `internal/config`
// additively; config stays a leaf. The projection is optional sugar —
// programmatic `governance.Config` construction remains the headless
// golden path.

package governance

import "github.com/hurtener/Harbor/internal/config"

// ConfigFromOperator projects the operator-supplied
// `config.GovernanceConfig` onto the `governance.Config` shape. Only
// the declarative policy fields are projected — `DefaultTier` +
// `IdentityTiers` (each tier's `BudgetCeilingUSD` / `RateLimit` /
// `MaxTokens`). The runtime-injection fields (`Resolver` / `Clock`)
// stay zero by design: they are Go-level seams (a custom TierResolver,
// a test clock) with no YAML surface; callers wire them after
// projection when needed. The read-only posture surface consumes the
// projection as-is (a nil Resolver is correct — every caller's
// `ResolvedTier` falls back to `DefaultTier`).
//
// `config.GovernanceConfig.RepairAttempts` is NOT part of this
// projection: it is the LLM repair-loop knob (consumed by the retry
// surface), not a tier-policy field — `governance.Config` carries no
// equivalent. The field-parity test names this exclusion.
//
// An empty `IdentityTiers` in the YAML (the latent default)
// yields a `governance.Config` with an empty tier map — the posture
// surface reports it verbatim and enforcement stays latent.
func ConfigFromOperator(in config.GovernanceConfig) Config {
	tiers := make(map[string]TierConfig, len(in.IdentityTiers))
	for name, tc := range in.IdentityTiers {
		tiers[name] = TierConfig{
			BudgetCeilingUSD: tc.BudgetCeilingUSD,
			RateLimit: RateLimitConfig{
				Capacity:       tc.RateLimit.Capacity,
				RefillTokens:   tc.RateLimit.RefillTokens,
				RefillInterval: tc.RateLimit.RefillInterval,
			},
			MaxTokens: tc.MaxTokens,
		}
	}
	return Config{
		DefaultTier:   in.DefaultTier,
		IdentityTiers: tiers,
	}
}
