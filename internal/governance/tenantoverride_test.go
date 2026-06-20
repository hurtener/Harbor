package governance_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func sp(s string) *string   { return &s }
func fp(f float64) *float64 { return &f }
func ip(i int) *int         { return &i }

type policyHarness struct {
	policy *governance.TenantOverridePolicy
	state  state.StateStore
	bus    events.EventBus
}

func newPolicyHarness(t *testing.T) policyHarness {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	pol, err := governance.NewTenantOverridePolicy(st, bus, []string{"model-a", "model-b"}, nil)
	if err != nil {
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("NewTenantOverridePolicy: %v", err)
	}
	t.Cleanup(func() {
		_ = pol.Close(context.Background())
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return policyHarness{policy: pol, state: st, bus: bus}
}

func adminActor(tenant string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "admin", SessionID: "admin-sess"}}
}

// TestTenantOverride_NilDeps_FailLoud asserts the constructor rejects a
// nil state / bus.
func TestTenantOverride_NilDeps_FailLoud(t *testing.T) {
	if _, err := governance.NewTenantOverridePolicy(nil, nil, nil, nil); !errors.Is(err, governance.ErrInvalidConfig) {
		t.Errorf("nil state: want ErrInvalidConfig, got %v", err)
	}
}

// TestTenantOverride_SetGet_RoundTrip_AndAudit asserts a full set is
// readable back and emits the audit event with the right flags + model.
func TestTenantOverride_SetGet_RoundTrip_AndAudit(t *testing.T) {
	h := newPolicyHarness(t)
	actor := adminActor("t1")
	sub, err := h.bus.Subscribe(context.Background(), events.Filter{
		Tenant: "t1", User: "admin", Session: "admin-sess",
		Types: []events.EventType{governance.EventTypeTenantOverridesSet},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	spec := governance.TenantOverrideSpec{
		Model:             sp("model-b"),
		ExtraInstructions: sp("be terse"),
		Temperature:       fp(0.7),
		MaxTokens:         ip(2048),
		ReasoningEffort:   sp("high"),
	}
	if err := h.policy.Set(context.Background(), actor, "t1", spec); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, set, err := h.policy.Get(context.Background(), "t1")
	if err != nil || !set {
		t.Fatalf("Get: set=%v err=%v", set, err)
	}
	if got.Model == nil || *got.Model != "model-b" {
		t.Errorf("model = %v, want model-b", got.Model)
	}
	if got.ExtraInstructions == nil || *got.ExtraInstructions != "be terse" {
		t.Errorf("extra = %v", got.ExtraInstructions)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("temp = %v", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 2048 {
		t.Errorf("maxtokens = %v", got.MaxTokens)
	}
	if got.ReasoningEffort == nil || *got.ReasoningEffort != "high" {
		t.Errorf("reasoning = %v", got.ReasoningEffort)
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(governance.TenantOverridesSetPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Tenant != "t1" || p.Model != "model-b" {
			t.Errorf("payload tenant=%q model=%q", p.Tenant, p.Model)
		}
		if !p.SetModel || !p.SetExtraInstructions || !p.SetTemperature || !p.SetMaxTokens || !p.SetReasoningEffort {
			t.Errorf("payload flags = %+v, want all true", p)
		}
		if p.Actor.UserID != "admin" {
			t.Errorf("actor user = %q, want admin", p.Actor.UserID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no tenant_overrides_set event observed")
	}
}

// TestTenantOverride_UnknownModel_Rejected asserts a model with no
// configured ModelProfile is rejected at set time.
func TestTenantOverride_UnknownModel_Rejected(t *testing.T) {
	h := newPolicyHarness(t)
	err := h.policy.Set(context.Background(), adminActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: sp("ghost-model")})
	if !errors.Is(err, governance.ErrUnknownModel) {
		t.Fatalf("want ErrUnknownModel, got %v", err)
	}
	// Nothing persisted.
	if _, set, _ := h.policy.Get(context.Background(), "t1"); set {
		t.Error("rejected set must not persist a record")
	}
}

// TestTenantOverride_InvalidValues_Rejected asserts out-of-range tuning
// values fail loud.
func TestTenantOverride_InvalidValues_Rejected(t *testing.T) {
	h := newPolicyHarness(t)
	cases := []governance.TenantOverrideSpec{
		{Temperature: fp(2.5)},
		{Temperature: fp(-0.1)},
		{MaxTokens: ip(0)},
		{MaxTokens: ip(-5)},
		{ReasoningEffort: sp("turbo")},
	}
	for i, c := range cases {
		if err := h.policy.Set(context.Background(), adminActor("t1"), "t1", c); !errors.Is(err, governance.ErrInvalidOverride) {
			t.Errorf("case %d: want ErrInvalidOverride, got %v", i, err)
		}
	}
}

// TestTenantOverride_TenantIsolation asserts tenant A's default is
// invisible to tenant B.
func TestTenantOverride_TenantIsolation(t *testing.T) {
	h := newPolicyHarness(t)
	if err := h.policy.Set(context.Background(), adminActor("A"), "A",
		governance.TenantOverrideSpec{Model: sp("model-a")}); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	if _, set, err := h.policy.Get(context.Background(), "B"); err != nil || set {
		t.Errorf("tenant B leaked A's override: set=%v err=%v", set, err)
	}
	gotA, setA, _ := h.policy.Get(context.Background(), "A")
	if !setA || gotA.Model == nil || *gotA.Model != "model-a" {
		t.Errorf("tenant A lost its override: set=%v model=%v", setA, gotA.Model)
	}
}

// TestTenantOverride_Clear asserts an all-nil spec clears the record.
func TestTenantOverride_Clear(t *testing.T) {
	h := newPolicyHarness(t)
	if err := h.policy.Set(context.Background(), adminActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: sp("model-a")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := h.policy.Set(context.Background(), adminActor("t1"), "t1",
		governance.TenantOverrideSpec{}); err != nil {
		t.Fatalf("clear Set: %v", err)
	}
	if _, set, err := h.policy.Get(context.Background(), "t1"); err != nil || set {
		t.Errorf("after clear: set=%v err=%v, want set=false", set, err)
	}
}

// TestTenantOverride_Durability asserts a record set by one policy
// instance is readable by a fresh policy over the same StateStore (the
// lazy-load path).
func TestTenantOverride_Durability(t *testing.T) {
	h := newPolicyHarness(t)
	if err := h.policy.Set(context.Background(), adminActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: sp("model-b"), Temperature: fp(0.3)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Fresh policy over the same state — exercises lazyLoad, not the cache.
	fresh, err := governance.NewTenantOverridePolicy(h.state, h.bus, []string{"model-a", "model-b"}, nil)
	if err != nil {
		t.Fatalf("fresh policy: %v", err)
	}
	defer fresh.Close(context.Background())
	got, set, err := fresh.Get(context.Background(), "t1")
	if err != nil || !set {
		t.Fatalf("fresh Get: set=%v err=%v", set, err)
	}
	if got.Model == nil || *got.Model != "model-b" || got.Temperature == nil || *got.Temperature != 0.3 {
		t.Errorf("durability lost the record: %+v", got)
	}
}

// TestTenantOverride_EmptyTenant_FailLoud asserts an empty tenant fails
// closed (identity is mandatory).
func TestTenantOverride_EmptyTenant_FailLoud(t *testing.T) {
	h := newPolicyHarness(t)
	if err := h.policy.Set(context.Background(), adminActor(""), "",
		governance.TenantOverrideSpec{Model: sp("model-a")}); !errors.Is(err, governance.ErrIdentityRequired) {
		t.Errorf("Set empty tenant: want ErrIdentityRequired, got %v", err)
	}
	if _, _, err := h.policy.Get(context.Background(), ""); !errors.Is(err, governance.ErrIdentityRequired) {
		t.Errorf("Get empty tenant: want ErrIdentityRequired, got %v", err)
	}
}

// TestTenantOverride_Closed asserts Set/Get fail after Close.
func TestTenantOverride_Closed(t *testing.T) {
	h := newPolicyHarness(t)
	_ = h.policy.Close(context.Background())
	if err := h.policy.Set(context.Background(), adminActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: sp("model-a")}); !errors.Is(err, governance.ErrClosed) {
		t.Errorf("Set after Close: want ErrClosed, got %v", err)
	}
	if _, _, err := h.policy.Get(context.Background(), "t1"); !errors.Is(err, governance.ErrClosed) {
		t.Errorf("Get after Close: want ErrClosed, got %v", err)
	}
}

// TestTenantOverride_ConcurrentReuse asserts N≥100 concurrent Set/Get
// across many tenants against ONE shared policy stay isolated (no
// cross-talk, no race — run under -race). CLAUDE.md §5/§11 concurrent-reuse
// contract.
func TestTenantOverride_ConcurrentReuse(t *testing.T) {
	h := newPolicyHarness(t)
	const n = 128
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", i)
			model := "model-a"
			if i%2 == 0 {
				model = "model-b"
			}
			if err := h.policy.Set(context.Background(), adminActor(tenant), tenant,
				governance.TenantOverrideSpec{Model: sp(model), MaxTokens: ip(100 + i)}); err != nil {
				t.Errorf("tenant %s Set: %v", tenant, err)
				return
			}
			got, set, err := h.policy.Get(context.Background(), tenant)
			if err != nil || !set {
				t.Errorf("tenant %s Get: set=%v err=%v", tenant, set, err)
				return
			}
			if got.Model == nil || *got.Model != model {
				t.Errorf("tenant %s cross-talk: model=%v want %s", tenant, got.Model, model)
			}
			if got.MaxTokens == nil || *got.MaxTokens != 100+i {
				t.Errorf("tenant %s cross-talk: maxtokens=%v want %d", tenant, got.MaxTokens, 100+i)
			}
		}(i)
	}
	wg.Wait()
}

// TestTenantOverride_MultiReplica_Freshness is the Phase 92b item-3 gate:
// two policy instances over ONE shared StateStore model two runtime
// replicas. A Set on replica A must be visible to replica B's NEXT Get —
// AND a SECOND Set on A must also land on B (the per-read freshness
// guarantee; the prior loaded-permanent cache served the first value
// forever). This is the cross-replica staleness the change closes.
func TestTenantOverride_MultiReplica_Freshness(t *testing.T) {
	h := newPolicyHarness(t) // replica A over h.state
	replicaB, err := governance.NewTenantOverridePolicy(h.state, h.bus, []string{"model-a", "model-b"}, nil)
	if err != nil {
		t.Fatalf("replica B: %v", err)
	}
	defer replicaB.Close(context.Background())

	// A sets model-a; B loads it (first read populates B's slot).
	if err := h.policy.Set(context.Background(), adminActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: sp("model-a")}); err != nil {
		t.Fatalf("A Set 1: %v", err)
	}
	got, set, err := replicaB.Get(context.Background(), "t1")
	if err != nil || !set || got.Model == nil || *got.Model != "model-a" {
		t.Fatalf("B Get 1: set=%v err=%v model=%v, want model-a", set, err, got.Model)
	}

	// A sets model-b. B has already loaded t1 — under the OLD permanent
	// cache it would still serve model-a. Per-read freshness means B's
	// next Get re-reads and sees model-b.
	if err := h.policy.Set(context.Background(), adminActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: sp("model-b")}); err != nil {
		t.Fatalf("A Set 2: %v", err)
	}
	got, set, err = replicaB.Get(context.Background(), "t1")
	if err != nil || !set || got.Model == nil || *got.Model != "model-b" {
		t.Fatalf("B Get 2: set=%v err=%v model=%v, want model-b (stale cross-replica cache)", set, err, got.Model)
	}

	// A clears the record. B sees the clear too (set=false).
	if err := h.policy.Set(context.Background(), adminActor("t1"), "t1",
		governance.TenantOverrideSpec{}); err != nil {
		t.Fatalf("A clear: %v", err)
	}
	if _, set, err := replicaB.Get(context.Background(), "t1"); err != nil || set {
		t.Errorf("B after A clear: set=%v err=%v, want set=false", set, err)
	}
}
