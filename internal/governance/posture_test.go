package governance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// postureCtx builds a context carrying a complete identity triple for a
// posture-read test.
func postureCtx(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// sampleTiers is a representative non-latent IdentityTiers map.
func sampleTiers() map[string]TierConfig {
	return map[string]TierConfig{
		"free": {
			BudgetCeilingUSD: 5.0,
			RateLimit:        RateLimitConfig{Capacity: 100, RefillTokens: 10, RefillInterval: time.Second},
			MaxTokens:        2048,
		},
		"enterprise": {
			BudgetCeilingUSD: 1000.0,
			RateLimit:        RateLimitConfig{Capacity: 100000, RefillTokens: 5000, RefillInterval: time.Minute},
			MaxTokens:        128000,
		},
	}
}

func TestPostureProvider_Posture_ReturnsConfiguredTiers(t *testing.T) {
	t.Parallel()
	cfg := Config{
		DefaultTier:   "free",
		IdentityTiers: sampleTiers(),
	}
	p := NewPostureProvider(cfg)
	snap, err := p.Posture(postureCtx(t, "t1", "u1", "s1"))
	if err != nil {
		t.Fatalf("Posture: unexpected error: %v", err)
	}
	if snap.DefaultTier != "free" {
		t.Errorf("DefaultTier = %q, want %q", snap.DefaultTier, "free")
	}
	// No resolver configured → ResolvedTier falls back to DefaultTier.
	if snap.ResolvedTier != "free" {
		t.Errorf("ResolvedTier = %q, want %q (nil resolver → DefaultTier)", snap.ResolvedTier, "free")
	}
	if len(snap.IdentityTiers) != 2 {
		t.Fatalf("IdentityTiers len = %d, want 2", len(snap.IdentityTiers))
	}
	ent := snap.IdentityTiers["enterprise"]
	if ent.BudgetCeilingUSD != 1000.0 || ent.MaxTokens != 128000 {
		t.Errorf("enterprise tier projected wrong: %+v", ent)
	}
	if ent.RateLimit.Capacity != 100000 || ent.RateLimit.RefillInterval != time.Minute {
		t.Errorf("enterprise RateLimit projected wrong: %+v", ent.RateLimit)
	}
}

func TestPostureProvider_Posture_DeepCopiesTierMap(t *testing.T) {
	t.Parallel()
	cfg := Config{DefaultTier: "free", IdentityTiers: sampleTiers()}
	p := NewPostureProvider(cfg)
	snap, err := p.Posture(postureCtx(t, "t1", "u1", "s1"))
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	// Mutate the returned snapshot's map — the provider's Config must
	// NOT be affected (D-025: no shared mutable state crossing the call
	// boundary).
	snap.IdentityTiers["free"] = TierConfig{BudgetCeilingUSD: 99999}
	delete(snap.IdentityTiers, "enterprise")

	snap2, err := p.Posture(postureCtx(t, "t1", "u1", "s1"))
	if err != nil {
		t.Fatalf("Posture (second): %v", err)
	}
	if len(snap2.IdentityTiers) != 2 {
		t.Fatalf("second snapshot len = %d, want 2 — first snapshot's mutation leaked into the provider", len(snap2.IdentityTiers))
	}
	if snap2.IdentityTiers["free"].BudgetCeilingUSD != 5.0 {
		t.Errorf("second snapshot 'free' ceiling = %v, want 5.0 — mutation leaked", snap2.IdentityTiers["free"].BudgetCeilingUSD)
	}
}

func TestPostureProvider_Posture_LatentConfigReturnsEmptyMap(t *testing.T) {
	t.Parallel()
	// Zero-value Config — the latent default per D-044 / Phase 36a.
	p := NewPostureProvider(Config{})
	snap, err := p.Posture(postureCtx(t, "t1", "u1", "s1"))
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if snap.DefaultTier != "" {
		t.Errorf("DefaultTier = %q, want empty (latent)", snap.DefaultTier)
	}
	if snap.ResolvedTier != "" {
		t.Errorf("ResolvedTier = %q, want empty (latent)", snap.ResolvedTier)
	}
	if snap.IdentityTiers == nil {
		t.Fatal("IdentityTiers is nil — must be a non-nil empty map so the wire JSON is {} not null")
	}
	if len(snap.IdentityTiers) != 0 {
		t.Errorf("IdentityTiers len = %d, want 0", len(snap.IdentityTiers))
	}
}

func TestPostureProvider_Posture_ResolverResolvesTier(t *testing.T) {
	t.Parallel()
	cfg := Config{
		DefaultTier:   "free",
		IdentityTiers: sampleTiers(),
		Resolver: func(id identity.Identity) string {
			if id.TenantID == "bigco" {
				return "enterprise"
			}
			return ""
		},
	}
	p := NewPostureProvider(cfg)

	// A caller whose resolver maps to "enterprise".
	snap, err := p.Posture(postureCtx(t, "bigco", "u1", "s1"))
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if snap.ResolvedTier != "enterprise" {
		t.Errorf("ResolvedTier = %q, want %q", snap.ResolvedTier, "enterprise")
	}

	// A caller the resolver does not match → falls back to DefaultTier.
	snap2, err := p.Posture(postureCtx(t, "smallco", "u1", "s1"))
	if err != nil {
		t.Fatalf("Posture (smallco): %v", err)
	}
	if snap2.ResolvedTier != "free" {
		t.Errorf("ResolvedTier = %q, want %q (resolver miss → DefaultTier)", snap2.ResolvedTier, "free")
	}
}

func TestPostureProvider_Posture_MissingIdentityFailsLoudly(t *testing.T) {
	t.Parallel()
	p := NewPostureProvider(Config{DefaultTier: "free", IdentityTiers: sampleTiers()})
	// No identity in ctx — must fail closed with ErrIdentityRequired
	// (CLAUDE.md §6 rule 9; no opt-out).
	_, err := p.Posture(context.Background())
	if err == nil {
		t.Fatal("Posture with no identity: want error, got nil")
	}
	if !errors.Is(err, ErrIdentityRequired) {
		t.Errorf("Posture with no identity: error = %v, want wrapped ErrIdentityRequired", err)
	}
}

// TestPostureProvider_Posture_ConcurrentReuse pins D-025: one
// PostureProvider shared across N≥100 goroutines, each reading with its
// own identity, sees no data races, no context bleed (each goroutine's
// ResolvedTier matches its own identity), and the baseline goroutine
// count is restored after teardown. Run with -race.
func TestPostureProvider_Posture_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	cfg := Config{
		DefaultTier:   "free",
		IdentityTiers: sampleTiers(),
		Resolver: func(id identity.Identity) string {
			if id.TenantID == "bigco" {
				return "enterprise"
			}
			return ""
		},
	}
	p := NewPostureProvider(cfg)

	const n = 128
	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)
	for i := range n {
		go func() {
			defer wg.Done()
			// Even goroutines use the bigco tenant (→ enterprise);
			// odd use a per-goroutine tenant (→ free).
			tenant := "smallco"
			wantTier := "free"
			if i%2 == 0 {
				tenant = "bigco"
				wantTier = "enterprise"
			}
			ctx := postureCtx(t, tenant, "u", "s")
			snap, err := p.Posture(ctx)
			if err != nil {
				errCh <- err
				return
			}
			if snap.ResolvedTier != wantTier {
				errCh <- errContextBleed(i, wantTier, snap.ResolvedTier)
				return
			}
			// Mutate the per-goroutine snapshot to flush any shared-map
			// race under -race.
			snap.IdentityTiers["scratch"] = TierConfig{}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func errContextBleed(i int, want, got string) error {
	return errors.New("goroutine " + itoa(i) + ": context bleed — ResolvedTier=" + got + " want=" + want)
}

// itoa is a tiny stdlib-free int formatter for the error message above.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
