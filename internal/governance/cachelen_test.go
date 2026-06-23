package governance_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/llm"
)

// TestCacheLen_NilSubsystemIsZero asserts the exported CacheLen helper
// reports zero for the latent no-enforcement default (a nil Subsystem) —
// the runtime gauge reads zero rather than panicking when no tiers are
// configured.
func TestCacheLen_NilSubsystemIsZero(t *testing.T) {
	t.Parallel()
	if n := governance.CacheLen(nil); n != 0 {
		t.Errorf("CacheLen(nil) = %d, want 0", n)
	}
}

// TestCacheLen_CompoundSumsMembers asserts CacheLen over a compound
// Subsystem (the shape NewSubsystemFromConfig returns) sums the
// identity-scoped caches of its cost + rate members and ignores the
// stateless MaxTokens member. One identity's traffic populates one cost
// key and one rate key, so the sum is 2.
func TestCacheLen_CompoundSumsMembers(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	cfg := governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {
				BudgetCeilingUSD: 100.0,
				RateLimit:        governance.RateLimitConfig{Capacity: 1000},
			},
		},
	}
	sub, err := governance.NewSubsystemFromConfig(cfg, st, bus)
	if err != nil {
		t.Fatalf("NewSubsystemFromConfig: %v", err)
	}
	if sub == nil {
		t.Fatal("NewSubsystemFromConfig returned nil for a populated tier map")
	}

	if n := governance.CacheLen(sub); n != 0 {
		t.Fatalf("CacheLen before traffic = %d, want 0", n)
	}

	ctx := ctxWith(t, "T", "U", "S", "R1")
	// PreCall populates the rate-limit cache; PostCall populates the
	// cost-accumulator cache — both keyed by the single identity.
	if err := sub.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Fatalf("PreCall: %v", err)
	}
	if err := sub.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.01}}, nil); err != nil {
		t.Fatalf("PostCall: %v", err)
	}

	if n := governance.CacheLen(sub); n != 2 {
		t.Errorf("CacheLen after one identity's PreCall+PostCall = %d, want 2 (one cost key + one rate key)", n)
	}
}
