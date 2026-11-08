package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

type countingSelectionResolver struct {
	calls    atomic.Int64
	lifetime time.Duration
	fail     bool
}

func (r *countingSelectionResolver) SelectProviderRoute(_ context.Context, req ProviderRouteRequest) (SelectedProviderRoute, error) {
	r.calls.Add(1)
	if r.fail {
		return SelectedProviderRoute{}, errors.New("resolver down")
	}
	return SelectedProviderRoute{
		Provider: "openai", Model: "model-" + req.UserID, KeyName: "key", RouteID: req.RouteID,
		RouteGeneration: req.RouteGeneration, ProviderConnectionID: req.ProviderConnectionID,
		ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration:    req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
		ExpiresAt: time.Now().Add(r.lifetime),
	}, nil
}

func (*countingSelectionResolver) ResolveProviderRoute(context.Context, ProviderRouteRequest) (ResolvedProviderRoute, error) {
	panic("selection cache must never resolve credentials")
}

func cacheTestRequest(tenant, user, agent string, generation uint64) ProviderRouteRequest {
	return ProviderRouteRequest{
		TenantID: tenant, UserID: user, SessionID: "s", LogicalRunID: "run", EffectiveAgentID: agent,
		TaskID: "task", LogicalCallID: "call", RouteID: "route", RouteGeneration: generation,
		ProviderConnectionID: "conn", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1,
		ModelSelector: "planner", ModelProfileSupported: true, Purpose: ProviderRoutePurposeRun,
	}
}

func TestProviderRouteSelectionCache_PrepareThenReuseWithinExpiry(t *testing.T) {
	resolver := &countingSelectionResolver{lifetime: time.Minute}
	cfg := ProviderRouteConfig{Resolver: resolver, SelectionCache: NewProviderRouteSelectionCache(8)}
	req := cacheTestRequest("t1", "u1", "a1", 1)
	cached, err := PrepareProviderRouteSelection(context.Background(), cfg, req)
	if err != nil || cached {
		t.Fatalf("first prepare cached=%v err=%v", cached, err)
	}
	// A later run's request differs only in run/task/call/session IDs.
	runReq := req
	runReq.SessionID, runReq.LogicalRunID, runReq.TaskID, runReq.LogicalCallID = "s2", "run2", "task2", "call2"
	selected, hit, err := selectProviderRouteCached(context.Background(), cfg, runReq, time.Now())
	if err != nil || !hit || selected.Model != "model-u1" {
		t.Fatalf("run selection hit=%v model=%q err=%v", hit, selected.Model, err)
	}
	if resolver.calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls.Load())
	}
}

func TestProviderRouteSelectionCache_ReturnsDefensiveCopies(t *testing.T) {
	cache := NewProviderRouteSelectionCache(4)
	now := time.Now()
	key := ProviderRouteSelectionKeyFor(cacheTestRequest("t", "u", "a", 1))
	stored := SelectedProviderRoute{
		Model:        "m",
		Endpoint:     &ProviderEndpointBinding{Kind: ProviderEndpointOpenAICompatible, Value: "https://x.example", Digest: "d"},
		ModelProfile: &ProviderModelProfile{ContextWindowTokens: 1000, MaxOutputTokens: 100, ReasoningEffortLevels: []ReasoningEffort{"low"}},
		ExpiresAt:    now.Add(time.Minute),
	}
	cache.Store(key, stored, now)
	stored.Endpoint.Value = "mutated-after-store"
	got, ok := cache.Lookup(key, now)
	if !ok || got.Endpoint.Value != "https://x.example" {
		t.Fatalf("store aliased the caller's endpoint: %+v", got.Endpoint)
	}
	got.Endpoint.Value = "mutated"
	got.ModelProfile.MaxOutputTokens = 7
	got.ModelProfile.ReasoningEffortLevels[0] = "high"
	again, _ := cache.Lookup(key, now)
	if again.Endpoint.Value != "https://x.example" || again.ModelProfile.MaxOutputTokens != 100 || again.ModelProfile.ReasoningEffortLevels[0] != "low" {
		t.Fatal("cache entry was aliased by a returned selection")
	}
}

func TestProviderRouteSelectionCache_IsolatesPrincipalsAndGenerations(t *testing.T) {
	resolver := &countingSelectionResolver{lifetime: time.Minute}
	cfg := ProviderRouteConfig{Resolver: resolver, SelectionCache: NewProviderRouteSelectionCache(8)}
	base := cacheTestRequest("t1", "u1", "a1", 1)
	if _, err := PrepareProviderRouteSelection(context.Background(), cfg, base); err != nil {
		t.Fatal(err)
	}
	for _, other := range []ProviderRouteRequest{
		cacheTestRequest("t2", "u1", "a1", 1), // other tenant
		cacheTestRequest("t1", "u2", "a1", 1), // other user
		cacheTestRequest("t1", "u1", "a2", 1), // other agent
		cacheTestRequest("t1", "u1", "a1", 2), // rotated route generation
	} {
		if _, hit := cfg.SelectionCache.Lookup(ProviderRouteSelectionKeyFor(other), time.Now()); hit {
			t.Fatalf("selection leaked to %+v", ProviderRouteSelectionKeyFor(other))
		}
	}
}

func TestProviderRouteSelectionCache_ExpiryMarginAndFailures(t *testing.T) {
	short := &countingSelectionResolver{lifetime: 3 * time.Second} // inside the reuse margin
	cfg := ProviderRouteConfig{Resolver: short, SelectionCache: NewProviderRouteSelectionCache(8)}
	req := cacheTestRequest("t", "u", "a", 1)
	if _, err := PrepareProviderRouteSelection(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if cfg.SelectionCache.Len() != 0 {
		t.Fatal("a selection expiring inside the reuse margin was retained")
	}
	long := &countingSelectionResolver{lifetime: time.Minute}
	cfg = ProviderRouteConfig{Resolver: long, SelectionCache: NewProviderRouteSelectionCache(8)}
	if _, err := PrepareProviderRouteSelection(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if _, hit := cfg.SelectionCache.Lookup(ProviderRouteSelectionKeyFor(req), time.Now().Add(2*time.Minute)); hit {
		t.Fatal("expired selection reused")
	}
	if cfg.SelectionCache.Len() != 0 {
		t.Fatal("expired selection not evicted on lookup")
	}
	failing := &countingSelectionResolver{fail: true}
	cfg = ProviderRouteConfig{Resolver: failing, SelectionCache: NewProviderRouteSelectionCache(8)}
	if _, err := PrepareProviderRouteSelection(context.Background(), cfg, req); !errors.Is(err, ErrProviderRouteResolutionFailed) {
		t.Fatalf("failing resolver err = %v", err)
	}
	if cfg.SelectionCache.Len() != 0 {
		t.Fatal("failure retained")
	}
	if _, err := PrepareProviderRouteSelection(context.Background(), ProviderRouteConfig{Resolver: long}, req); !errors.Is(err, ErrProviderRouteResolverUnavailable) {
		t.Fatalf("no cache err = %v", err)
	}
	posture := req
	posture.Purpose = ProviderRoutePurposePosture
	if _, err := PrepareProviderRouteSelection(context.Background(), ProviderRouteConfig{Resolver: long, SelectionCache: NewProviderRouteSelectionCache(1)}, posture); !errors.Is(err, ErrProviderRouteInvalid) {
		t.Fatalf("posture purpose err = %v", err)
	}
}

func TestProviderRouteSelectionCache_BoundedUnderChurn(t *testing.T) {
	cache := NewProviderRouteSelectionCache(16)
	now := time.Now()
	for i := range 200 {
		req := cacheTestRequest("t", fmt.Sprintf("u%d", i), "a", 1)
		cache.Store(ProviderRouteSelectionKeyFor(req), SelectedProviderRoute{ExpiresAt: now.Add(time.Minute + time.Duration(i)*time.Millisecond)}, now)
		if cache.Len() > 16 {
			t.Fatalf("cache grew to %d entries, cap 16", cache.Len())
		}
	}
	// The newest entries survive; the closest-to-expiry are evicted first.
	if _, hit := cache.Lookup(ProviderRouteSelectionKeyFor(cacheTestRequest("t", "u199", "a", 1)), now); !hit {
		t.Fatal("newest entry evicted")
	}
	var nilCache *ProviderRouteSelectionCache
	nilCache.Store(ProviderRouteSelectionKey{}, SelectedProviderRoute{ExpiresAt: now.Add(time.Hour)}, now)
	if _, hit := nilCache.Lookup(ProviderRouteSelectionKey{}, now); hit || nilCache.Len() != 0 {
		t.Fatal("nil cache is not a no-op")
	}
}

func TestProviderRouteClient_ReusesCachedSelectionAcrossCalls(t *testing.T) {
	resolver := &countingSelectionResolver{lifetime: time.Minute}
	route := ProviderRoute{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "conn",
		ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "planner"}
	probe := &routePolicyProbe{}
	client := &providerRouteClient{
		inner:     probe,
		cfg:       ProviderRouteConfig{Resolver: resolver, SelectionCache: NewProviderRouteSelectionCache(8)},
		validator: routeClientValidator{allowed: map[string]bool{"openai": true}},
		now:       time.Now,
	}
	ctx, err := identity.WithRun(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTrustedProviderRoute(ctx, TrustedProviderRouteContext{RuntimeID: "rt", Route: route,
		EffectiveAgentID: "a", TaskID: "task", Purpose: ProviderRoutePurposeRun})
	for range 3 {
		if _, err := client.Complete(ctx, CompleteRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if resolver.calls.Load() != 1 || probe.calls != 3 {
		t.Fatalf("resolver calls=%d inner calls=%d, want 1 and 3", resolver.calls.Load(), probe.calls)
	}
	if probe.selected.Model != "model-u" {
		t.Fatalf("selected model = %q", probe.selected.Model)
	}
}

func TestProviderRouteSelectionCache_ConcurrentReuse(t *testing.T) {
	resolver := &countingSelectionResolver{lifetime: time.Minute}
	cfg := ProviderRouteConfig{Resolver: resolver, SelectionCache: NewProviderRouteSelectionCache(64)}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := fmt.Sprintf("u%d", i%8)
			req := cacheTestRequest("t", user, "a", 1)
			selected, _, err := selectProviderRouteCached(context.Background(), cfg, req, time.Now())
			if err != nil {
				t.Error(err)
				return
			}
			if selected.Model != "model-"+user {
				t.Errorf("principal %s got %q", user, selected.Model)
			}
		}()
	}
	wg.Wait()
	if cfg.SelectionCache.Len() != 8 {
		t.Fatalf("entries = %d, want 8", cfg.SelectionCache.Len())
	}
}
