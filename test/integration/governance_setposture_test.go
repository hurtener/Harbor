// Package integration_test — cross-subsystem E2E for the admin identity-tier
// policy WRITE (`governance.set_posture`, D-332). Real drivers on the seam: a
// real inmem StateStore + a real EventBus wired with a real audit Redactor
// back the governance.SetPosturePolicy; the governance.PostureProvider reads
// the same StateStore so the write→read round-trip is proven end-to-end. The
// test asserts: a valid write emits `governance.posture_set` on the bus AND
// the following posture read returns the written table; identity propagation
// (the actor's verified triple is on the event); the fail-closed validation
// reject (a budget-widening / ceiling-omitting write is rejected, NO state
// mutation occurs, the next read shows the prior policy unchanged — the real
// fail-loud path); and a concurrency stress (N≥10). Runs under -race.
package integration_test

import (
	"context"
	"errors"
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

func setPostureE2EFixture(t *testing.T) (*governance.SetPosturePolicy, *governance.PostureProvider, events.EventBus) {
	t.Helper()
	// Real bus wired with a real audit Redactor (the patterns redactor).
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
	policy, err := governance.NewSetPosturePolicy(st, bus, governance.Config{}, nil, nil, false)
	if err != nil {
		t.Fatalf("NewSetPosturePolicy: %v", err)
	}
	t.Cleanup(func() { _ = policy.Close(context.Background()) })
	provider := governance.NewPostureProviderWithState(governance.Config{}, st)
	return policy, provider, bus
}

func TestE2E_GovernanceSetPosture_WriteEmitsAndRoundTrips(t *testing.T) {
	t.Parallel()
	policy, provider, bus := setPostureE2EFixture(t)
	actor := identity.Quadruple{Identity: identity.Identity{TenantID: "acme", UserID: "admin", SessionID: "s1"}}

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "acme", User: "admin", Session: "s1",
		Types: []events.EventType{governance.EventTypePostureSet},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {BudgetCeilingUSD: 0.50, MaxTokens: 2048},
		},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Assert the audit event landed, anchored on the actor's verified triple.
	select {
	case ev := <-sub.Events():
		if ev.Type != governance.EventTypePostureSet {
			t.Fatalf("event type = %q", ev.Type)
		}
		p, ok := ev.Payload.(governance.GovernancePostureSetPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Actor.TenantID != "acme" || p.Actor.UserID != "admin" {
			t.Errorf("event actor = %+v, want acme/admin", p.Actor)
		}
		if p.DefaultTierAfter != "free" || p.TierCountAfter != 1 {
			t.Errorf("event summary mismatch: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe governance.posture_set within 2s")
	}

	// Round-trip: the posture read returns exactly what was written.
	ctx, err := identity.With(context.Background(), actor.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	read, err := provider.Posture(ctx)
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if read.DefaultTier != "free" || read.IdentityTiers["free"].BudgetCeilingUSD != 0.50 ||
		read.IdentityTiers["free"].MaxTokens != 2048 {
		t.Fatalf("round-trip mismatch: %+v", read)
	}
}

func TestE2E_GovernanceSetPosture_FailClosedLeavesPriorPolicy(t *testing.T) {
	t.Parallel()
	policy, provider, _ := setPostureE2EFixture(t)
	actor := identity.Quadruple{Identity: identity.Identity{TenantID: "acme", UserID: "admin", SessionID: "s1"}}
	ctx, err := identity.With(context.Background(), actor.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Seed an enforced policy.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.50}},
	}); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	// A budget-widening write (drops the enforced free tier) is rejected
	// fail-closed BEFORE any state mutation — the real fail-loud path.
	_, err = policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "team",
		IdentityTiers: map[string]governance.TierConfig{"team": {BudgetCeilingUSD: 5.0}},
	})
	if !errors.Is(err, governance.ErrPolicyWidening) {
		t.Fatalf("widening write: got %v, want ErrPolicyWidening", err)
	}

	// The next read shows the PRIOR enforced policy, unchanged.
	read, err := provider.Posture(ctx)
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if _, ok := read.IdentityTiers["free"]; !ok || read.IdentityTiers["free"].BudgetCeilingUSD != 0.50 {
		t.Fatalf("prior policy mutated by a rejected write: %+v", read)
	}
	if _, ok := read.IdentityTiers["team"]; ok {
		t.Fatalf("rejected write's team tier leaked into the effective policy: %+v", read)
	}
}

// TestE2E_GovernanceSetPosture_EnforcementTakesEffect proves the write is not
// inert: a shared TierSource wires the ENFORCEMENT subsystem (a real
// MaxTokensEnforcer + CostAccumulator reading tier VALUES through the source)
// to the SetPosturePolicy write path over real drivers. A `set_posture` write
// changes what PreCall ENFORCES on the next call, so the read and the enforced
// policy never diverge.
func TestE2E_GovernanceSetPosture_EnforcementTakesEffect(t *testing.T) {
	t.Parallel()
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

	// Config-default layer: free tier MaxTokens=100. The shared source seeds
	// the enforcer AND is swapped by the writer — the exact production wiring.
	seed := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}
	source := governance.NewTierSource(seed.DefaultTier, seed.IdentityTiers)
	enforcer := governance.NewMaxTokensEnforcer(bus, governance.WithTierSource(governance.Config{}, source))
	policy, err := governance.NewSetPosturePolicy(st, bus, seed, nil, source, true)
	if err != nil {
		t.Fatalf("NewSetPosturePolicy: %v", err)
	}
	t.Cleanup(func() { _ = policy.Close(context.Background()) })
	provider := governance.NewPostureProviderWithState(seed, st)

	actor := identity.Quadruple{Identity: identity.Identity{TenantID: "acme", UserID: "admin", SessionID: "s1"}}
	ctx, err := identity.With(context.Background(), actor.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	req := func(n int) llm.CompleteRequest { return llm.CompleteRequest{Model: "m", MaxTokens: &n} }

	// Config default: 80 permits, 150 rejects.
	if err := enforcer.PreCall(ctx, req(80)); err != nil {
		t.Fatalf("config default 80<=100 should permit: %v", err)
	}
	if err := enforcer.PreCall(ctx, req(150)); !errors.Is(err, governance.ErrMaxTokensExceeded) {
		t.Fatalf("config default 150>100 should reject: %v", err)
	}

	// Write a LOWER cap (50) over the real StateStore — enforcement follows.
	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 50}},
	}); err != nil {
		t.Fatalf("lower Set: %v", err)
	}
	if err := enforcer.PreCall(ctx, req(80)); !errors.Is(err, governance.ErrMaxTokensExceeded) {
		t.Fatalf("after set_posture MaxTokens=50: 80 must now reject (enforcement follows the write): %v", err)
	}
	// The read agrees with what is enforced.
	read, err := provider.Posture(ctx)
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if read.IdentityTiers["free"].MaxTokens != 50 {
		t.Fatalf("read disagrees with enforced policy: read=%d want 50", read.IdentityTiers["free"].MaxTokens)
	}
}

func TestE2E_GovernanceSetPosture_ConcurrencyStress(t *testing.T) {
	t.Parallel()
	policy, provider, _ := setPostureE2EFixture(t)
	actor := identity.Quadruple{Identity: identity.Identity{TenantID: "acme", UserID: "admin", SessionID: "s1"}}
	ctx, err := identity.With(context.Background(), actor.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	const n = 24
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		budget := float64(i%10) + 1.0 // always enforced (> 0)
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
			if _, err := provider.Posture(ctx); err != nil {
				t.Errorf("concurrent Posture: %v", err)
			}
		}()
	}
	wg.Wait()

	read, err := provider.Posture(ctx)
	if err != nil {
		t.Fatalf("final Posture: %v", err)
	}
	if read.IdentityTiers["free"].BudgetCeilingUSD <= 0 {
		t.Fatalf("final policy de-enforced under stress: %+v", read)
	}
}
