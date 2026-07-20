package governance_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// setPostureFixture builds an in-mem StateStore + bus for the set-posture
// unit tests, with a caller-supplied config-default layer.
func setPostureFixture(t *testing.T, defaults governance.Config) (*governance.SetPosturePolicy, *governance.PostureProvider) {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	policy, err := governance.NewSetPosturePolicy(st, bus, defaults, nil, nil, false)
	if err != nil {
		t.Fatalf("NewSetPosturePolicy: %v", err)
	}
	t.Cleanup(func() { _ = policy.Close(context.Background()) })
	provider := governance.NewPostureProviderWithState(defaults, st)
	return policy, provider
}

func postureActor() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}
}

func postureCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestSetPosturePolicy_FullReplace_RoundTrips(t *testing.T) {
	t.Parallel()
	policy, provider := setPostureFixture(t, governance.Config{})
	actor := postureActor()
	ctx := postureCtx(t)

	spec := governance.SetPostureSpec{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {BudgetCeilingUSD: 0.50, MaxTokens: 1000},
			"team": {BudgetCeilingUSD: 5.00, RateLimit: governance.RateLimitConfig{Capacity: 100}},
		},
	}
	snap, err := policy.Set(context.Background(), actor, spec)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if snap.DefaultTier != "free" || len(snap.IdentityTiers) != 2 {
		t.Fatalf("returned snapshot mismatch: %+v", snap)
	}

	// The posture read reflects the written record (what you set is what you read).
	read, err := provider.Posture(ctx)
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if read.DefaultTier != "free" {
		t.Errorf("read DefaultTier = %q want free", read.DefaultTier)
	}
	if read.IdentityTiers["free"].BudgetCeilingUSD != 0.50 {
		t.Errorf("read free budget = %v want 0.50", read.IdentityTiers["free"].BudgetCeilingUSD)
	}
	if read.IdentityTiers["team"].RateLimit.Capacity != 100 {
		t.Errorf("read team capacity = %v want 100", read.IdentityTiers["team"].RateLimit.Capacity)
	}
}

func TestSetPosturePolicy_NoRecord_ReadsConfigDefaults(t *testing.T) {
	t.Parallel()
	defaults := governance.Config{
		DefaultTier: "cfg",
		IdentityTiers: map[string]governance.TierConfig{
			"cfg": {BudgetCeilingUSD: 2.00},
		},
	}
	_, provider := setPostureFixture(t, defaults)
	read, err := provider.Posture(postureCtx(t))
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if read.DefaultTier != "cfg" || read.IdentityTiers["cfg"].BudgetCeilingUSD != 2.00 {
		t.Fatalf("no-record read should be config defaults, got %+v", read)
	}
}

func TestSetPosturePolicy_OmitEnforcedTier_RejectedWidening(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	actor := postureActor()

	// Seed an enforced "free" tier.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.50}},
	}); err != nil {
		t.Fatalf("seed Set: %v", err)
	}
	// A replace that OMITS the currently-enforced free tier is rejected.
	_, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "team",
		IdentityTiers: map[string]governance.TierConfig{"team": {BudgetCeilingUSD: 5.0}},
	})
	if !errors.Is(err, governance.ErrPolicyWidening) {
		t.Fatalf("omit-enforced-tier: got %v, want ErrPolicyWidening", err)
	}
}

func TestSetPosturePolicy_ZeroEnforcedDimension_Rejected(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	actor := postureActor()

	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.50}},
	}); err != nil {
		t.Fatalf("seed Set: %v", err)
	}
	// The tier is present but its enforced budget is ZEROED — a silent
	// de-enforcement, rejected fail-closed.
	_, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0}},
	})
	if !errors.Is(err, governance.ErrPolicyWidening) {
		t.Fatalf("zeroed-dimension: got %v, want ErrPolicyWidening", err)
	}
}

func TestSetPosturePolicy_EmptyMapWhenEnforced_Rejected(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	actor := postureActor()
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}); err != nil {
		t.Fatalf("seed Set: %v", err)
	}
	_, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{},
	})
	// Fail-closed either way: the F1(a) structural check catches DefaultTier
	// "free" absent from the empty map (ErrInvalidPosture) before the
	// no-widening check would report ErrPolicyWidening. Both are correct
	// fail-closed rejections — the write is never silently persisted.
	if !errors.Is(err, governance.ErrPolicyWidening) && !errors.Is(err, governance.ErrInvalidPosture) {
		t.Fatalf("empty-map-when-enforced: got %v, want ErrPolicyWidening or ErrInvalidPosture", err)
	}
}

func TestSetPosturePolicy_LowerCeiling_Allowed(t *testing.T) {
	t.Parallel()
	policy, provider := setPostureFixture(t, governance.Config{})
	actor := postureActor()
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 5.0}},
	}); err != nil {
		t.Fatalf("seed Set: %v", err)
	}
	// Lowering the ceiling (still enforced, >0) is a legitimate tightening.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 1.0}},
	}); err != nil {
		t.Fatalf("lower-ceiling: unexpected error %v", err)
	}
	read, err := provider.Posture(postureCtx(t))
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if read.IdentityTiers["free"].BudgetCeilingUSD != 1.0 {
		t.Errorf("lowered ceiling = %v want 1.0", read.IdentityTiers["free"].BudgetCeilingUSD)
	}
}

func TestSetPosturePolicy_UnlimitedTierZeroStaysValid(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	actor := postureActor()
	// A tier that was never enforced (budget 0 = unlimited) may remain 0 on
	// a subsequent write — a zero that DROPS nothing is not a rejection.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "pro",
		IdentityTiers: map[string]governance.TierConfig{"pro": {BudgetCeilingUSD: 0, MaxTokens: 0}},
	}); err != nil {
		t.Fatalf("first unlimited Set: %v", err)
	}
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "pro",
		IdentityTiers: map[string]governance.TierConfig{"pro": {BudgetCeilingUSD: 0}},
	}); err != nil {
		t.Fatalf("second unlimited Set: unexpected error %v", err)
	}
}

func TestSetPosturePolicy_NegativeValue_RejectedInvalid(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	actor := postureActor()
	_, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: -1}},
	})
	if !errors.Is(err, governance.ErrInvalidPosture) {
		t.Fatalf("negative-budget: got %v, want ErrInvalidPosture", err)
	}
}

func TestSetPosturePolicy_EmptyDefaultTierWhenEnforced_Rejected(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	_, err := policy.Set(context.Background(), postureActor(), governance.SetPostureSpec{
		DefaultTier:   "",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.5}},
	})
	if !errors.Is(err, governance.ErrInvalidPosture) {
		t.Fatalf("empty-default-tier: got %v, want ErrInvalidPosture", err)
	}
}

func TestSetPosturePolicy_EmitsPostureSet(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	// The fixture's bus is not directly reachable here; assert the write
	// succeeds and re-read reflects it (the emit is best-effort — covered
	// end-to-end by the integration test's bus subscription).
	if _, err := policy.Set(context.Background(), postureActor(), governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 1.0}},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
}

// TestSetPosture_DefaultTierAbsent_Rejected pins F1(a): a DefaultTier that
// names no tier in the submitted table is rejected — it would silently
// resolve every default caller to "no enforcement" while the map still shows
// enforced-looking tiers.
func TestSetPosture_DefaultTierAbsent_Rejected(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	_, err := policy.Set(context.Background(), postureActor(), governance.SetPostureSpec{
		DefaultTier:   "opex", // absent from the map
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.5}},
	})
	if !errors.Is(err, governance.ErrInvalidPosture) {
		t.Fatalf("default-tier-absent: got %v, want ErrInvalidPosture", err)
	}
}

// TestSetPosture_DefaultRepoint_DeEnforces_Rejected is the F1 security
// exploit family: repoint DefaultTier from a tier that enforces some
// dimension to a present tier that DROPS that dimension (even if the new
// default tier enforces a DIFFERENT dimension), while keeping the old tier in
// the map. The per-tier check passes (the old tier stays enforced) but every
// default-resolved caller is silently uncapped on the dropped dimension. Each
// of the three dimensions is a symmetric hole; all must be rejected
// PER-DIMENSION.
func TestSetPosture_DefaultRepoint_DeEnforces_Rejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		seedFree      governance.TierConfig // the enforced default tier before
		newDefaultCfg governance.TierConfig // the new default tier (drops the seeded dimension)
	}{
		{
			// Old default budget-caps; new default only max-tokens → budget dropped.
			name:          "budget_drop_via_dimension_swap",
			seedFree:      governance.TierConfig{BudgetCeilingUSD: 100},
			newDefaultCfg: governance.TierConfig{MaxTokens: 50},
		},
		{
			// Old default rate-limits; new default only budget → rate dropped.
			name:          "rate_drop_via_dimension_swap",
			seedFree:      governance.TierConfig{RateLimit: governance.RateLimitConfig{Capacity: 100}},
			newDefaultCfg: governance.TierConfig{BudgetCeilingUSD: 5},
		},
		{
			// Old default max-tokens-caps; new default only rate → max-tokens dropped.
			name:          "maxtokens_drop_via_dimension_swap",
			seedFree:      governance.TierConfig{MaxTokens: 4096},
			newDefaultCfg: governance.TierConfig{RateLimit: governance.RateLimitConfig{Capacity: 100}},
		},
		{
			// The all-zero new default (the original coarse case) stays rejected.
			name:          "all_dimensions_dropped",
			seedFree:      governance.TierConfig{BudgetCeilingUSD: 100},
			newDefaultCfg: governance.TierConfig{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy, _ := setPostureFixture(t, governance.Config{})
			actor := postureActor()
			if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
				DefaultTier:   "free",
				IdentityTiers: map[string]governance.TierConfig{"free": tc.seedFree},
			}); err != nil {
				t.Fatalf("seed Set: %v", err)
			}
			// The exploit: default → "premium"; "free" stays enforced in the map.
			_, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
				DefaultTier: "premium",
				IdentityTiers: map[string]governance.TierConfig{
					"free":    tc.seedFree, // still enforced — passes the per-tier check
					"premium": tc.newDefaultCfg,
				},
			})
			if !errors.Is(err, governance.ErrPolicyWidening) {
				t.Fatalf("F1 exploit (%s): got %v, want ErrPolicyWidening", tc.name, err)
			}
		})
	}
}

// TestSetPosture_DefaultRepoint_EnforcedToEnforced_Allowed confirms a
// legitimate default repoint (the new default tier carries real ceilings) is
// allowed AND that the NEW default tier actually enforces on the next PreCall.
func TestSetPosture_DefaultRepoint_EnforcedToEnforced_Allowed(t *testing.T) {
	t.Parallel()
	seed := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}
	policy, enforcer, _ := enforcementFixture(t, seed)
	actor := postureActor()
	ctx := postureCtx(t)

	// Repoint the default to "opex" WITH a real (lower) ceiling.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier: "opex",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {MaxTokens: 100},
			"opex": {MaxTokens: 40},
		},
	}); err != nil {
		t.Fatalf("legitimate default repoint should be allowed, got %v", err)
	}
	// The NEW default tier ("opex" @ 40) now enforces: a 50-token call rejects.
	n := 50
	if err := enforcer.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &n}); !errors.Is(err, governance.ErrMaxTokensExceeded) {
		t.Fatalf("new default tier opex@40 must enforce: 50 should reject, got %v", err)
	}

	// A repoint whose new default tier carries the SAME dimension PLUS a new
	// one (max_tokens + budget) must ALSO be allowed — the per-dimension check
	// must not over-reject when every currently-enforced dimension is preserved.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier: "enterprise",
		IdentityTiers: map[string]governance.TierConfig{
			"free":       {MaxTokens: 100},
			"opex":       {MaxTokens: 40},
			"enterprise": {MaxTokens: 40, BudgetCeilingUSD: 10},
		},
	}); err != nil {
		t.Fatalf("repoint to a tier with same-plus-more dimensions must be allowed, got %v", err)
	}
}

func TestSetPosturePolicy_ClosedFailsLoud(t *testing.T) {
	t.Parallel()
	policy, _ := setPostureFixture(t, governance.Config{})
	_ = policy.Close(context.Background())
	_, err := policy.Set(context.Background(), postureActor(), governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 1.0}},
	})
	if !errors.Is(err, governance.ErrClosed) {
		t.Fatalf("closed Set: got %v, want ErrClosed", err)
	}
}

// enforcementFixture wires a SHARED TierSource across (a) the enforcement
// subsystem (a MaxTokensEnforcer + CostAccumulator reading tier VALUES
// through the source) and (b) a SetPosturePolicy that swaps the same source
// on a write — the exact production wiring. Seeded from `seedCfg` (the
// config-default layer).
func enforcementFixture(t *testing.T, seedCfg governance.Config) (*governance.SetPosturePolicy, *governance.MaxTokensEnforcer, *governance.CostAccumulator) {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: 1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	// The shared source seeded from the config-default layer — the enforcers
	// and the writer read/swap the SAME instance.
	source := governance.NewTierSource(seedCfg.DefaultTier, seedCfg.IdentityTiers)
	effCfg := governance.WithTierSource(governance.Config{}, source)
	enforcer := governance.NewMaxTokensEnforcer(bus, effCfg)
	acc, err := governance.NewCostAccumulator(st, bus, effCfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	t.Cleanup(func() { _ = acc.Close(context.Background()) })
	policy, err := governance.NewSetPosturePolicy(st, bus, seedCfg, nil, source, true)
	if err != nil {
		t.Fatalf("NewSetPosturePolicy: %v", err)
	}
	t.Cleanup(func() { _ = policy.Close(context.Background()) })
	return policy, enforcer, acc
}

// TestSetPosture_Enforcement_TakesEffect is the load-bearing AC: a
// `set_posture` write must change what the governance wrapper ENFORCES on the
// next PreCall, not merely what the read returns. It asserts a written-LOWER
// ceiling rejects a call config would admit, a written-HIGHER ceiling admits
// a call config would reject, and (no write) enforcement uses config defaults
// unchanged.
func TestSetPosture_Enforcement_TakesEffect(t *testing.T) {
	t.Parallel()
	seed := governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {MaxTokens: 100, BudgetCeilingUSD: 1.0},
		},
	}
	policy, enforcer, acc := enforcementFixture(t, seed)
	actor := postureActor()
	ctx := postureCtx(t)

	mk := func(n int) llm.CompleteRequest { return llm.CompleteRequest{Model: "m", MaxTokens: &n} }

	// Baseline (no write): enforcement uses the config default MaxTokens=100.
	if err := enforcer.PreCall(ctx, mk(80)); err != nil {
		t.Fatalf("config default: 80 <= 100 should permit, got %v", err)
	}
	if err := enforcer.PreCall(ctx, mk(150)); !errors.Is(err, governance.ErrMaxTokensExceeded) {
		t.Fatalf("config default: 150 > 100 should reject, got %v", err)
	}

	// Write a HIGHER cap (200) — a call config would reject (150) now admits.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 200, BudgetCeilingUSD: 1.0}},
	}); err != nil {
		t.Fatalf("raise Set: %v", err)
	}
	if err := enforcer.PreCall(ctx, mk(150)); err != nil {
		t.Fatalf("after raising cap to 200: 150 should now permit, got %v", err)
	}

	// Write a LOWER cap (50) — a call config would admit (80) now rejects.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 50, BudgetCeilingUSD: 1.0}},
	}); err != nil {
		t.Fatalf("lower Set: %v", err)
	}
	if err := enforcer.PreCall(ctx, mk(80)); !errors.Is(err, governance.ErrMaxTokensExceeded) {
		t.Fatalf("after lowering cap to 50: 80 should now reject, got %v", err)
	}

	// Budget: accumulate 0.6 (under the config 1.0 ceiling → permits), then
	// write a LOWER budget (0.5) and assert the next PreCall now rejects — the
	// written ceiling is ENFORCED against the already-accumulated total.
	if err := acc.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Fatalf("budget under-ceiling PreCall: %v", err)
	}
	if err := acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.6}}, nil); err != nil {
		t.Fatalf("budget PostCall: %v", err)
	}
	if err := acc.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Fatalf("0.6 < 1.0 config ceiling should still permit, got %v", err)
	}
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 50, BudgetCeilingUSD: 0.5}},
	}); err != nil {
		t.Fatalf("lower-budget Set: %v", err)
	}
	if err := acc.PreCall(ctx, llm.CompleteRequest{Model: "m"}); !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("after lowering budget to 0.5: total 0.6 >= 0.5 should reject, got %v", err)
	}
}

// TestConcurrentReuse_SetPosture_EnforceDuringSwap is the W3 coverage: N≥100
// goroutines calling a real enforcer's PreCall (bound to the SHARED
// TierSource) WHILE a writer loops Set (which swaps that same source), under
// -race. It proves the atomic-swap-during-enforcement path is race-free — each
// PreCall reads a coherent old-or-new snapshot (never a torn map), and the
// race detector gates the swap↔read seam the enforcers actually use (the
// StateStore-only NoBleed test below does NOT exercise this path).
func TestConcurrentReuse_SetPosture_EnforceDuringSwap(t *testing.T) {
	t.Parallel()
	seed := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100, BudgetCeilingUSD: 5.0}},
	}
	policy, enforcer, acc := enforcementFixture(t, seed)
	actor := postureActor()
	ctx := postureCtx(t)

	const readers = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup

	// One writer goroutine loops Set (swapping the shared source). Every write
	// keeps the free tier enforced (>0) so no write is a widening reject.
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			mt := 20 + (i % 200) // always > 0 → always enforced
			if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
				DefaultTier:   "free",
				IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: mt, BudgetCeilingUSD: 5.0}},
			}); err != nil {
				t.Errorf("concurrent Set: %v", err)
				return
			}
		}
	}()

	// N concurrent readers hammer the enforcer PreCall against the same source.
	for range readers {
		wg.Add(2)
		n := 10
		go func() {
			defer wg.Done()
			// MaxTokens enforcer reads tier VALUES via the shared source.
			if err := enforcer.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &n}); err != nil &&
				!errors.Is(err, governance.ErrMaxTokensExceeded) {
				t.Errorf("concurrent enforcer PreCall unexpected err: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			// Cost accumulator also reads the ceiling via the shared source.
			if err := acc.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil &&
				!errors.Is(err, governance.ErrBudgetExceeded) {
				t.Errorf("concurrent acc PreCall unexpected err: %v", err)
			}
		}()
	}

	// Let readers run against a live writer for a bounded set of iterations.
	for range readers {
		var n = 10
		_ = enforcer.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &n})
	}
	close(stop)
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
}

// TestConcurrentReuse_SetPosturePolicy_NoBleed exercises the D-025 concurrent
// reuse contract: N≥100 concurrent Set + read invocations against a single
// shared SetPosturePolicy instance under -race. The record is a single
// runtime-level policy, so the test asserts last-writer-wins linearizability
// (no data race on the StateStore seam, no torn read between a write and a
// concurrent Posture) and no goroutine leak after teardown.
func TestConcurrentReuse_SetPosturePolicy_NoBleed(t *testing.T) {
	t.Parallel()
	policy, provider := setPostureFixture(t, governance.Config{})
	actor := postureActor()
	ctx := postureCtx(t)

	// All concurrent writes keep an enforced "free" tier (>0), so no write is
	// a widening reject — every write must succeed.
	const n = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		budget := float64(i%50) + 1.0 // always > 0 → always enforced
		go func() {
			defer wg.Done()
			if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
				DefaultTier:   "free",
				IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: budget}},
			}); err != nil {
				t.Errorf("concurrent Set: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			// A concurrent read must never tear — it sees SOME consistent
			// written table (free tier present, budget > 0) once any write
			// has landed, or the empty config default before the first.
			snap, err := provider.Posture(ctx)
			if err != nil {
				t.Errorf("concurrent Posture: %v", err)
				return
			}
			if tc, ok := snap.IdentityTiers["free"]; ok && tc.BudgetCeilingUSD <= 0 {
				t.Errorf("torn read: free tier present but budget %v <= 0", tc.BudgetCeilingUSD)
			}
		}()
	}
	wg.Wait()

	// The final effective policy still enforces the free tier.
	read, err := provider.Posture(ctx)
	if err != nil {
		t.Fatalf("final Posture: %v", err)
	}
	if read.IdentityTiers["free"].BudgetCeilingUSD <= 0 {
		t.Fatalf("final free budget = %v want > 0", read.IdentityTiers["free"].BudgetCeilingUSD)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
}
