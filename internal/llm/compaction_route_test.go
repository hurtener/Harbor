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

type compactionRouteResolver struct {
	selectFn func(context.Context, ProviderRouteRequest) (SelectedProviderRoute, error)
}

func (r compactionRouteResolver) SelectProviderRoute(ctx context.Context, req ProviderRouteRequest) (SelectedProviderRoute, error) {
	return r.selectFn(ctx, req)
}
func (compactionRouteResolver) ResolveProviderRoute(context.Context, ProviderRouteRequest) (ResolvedProviderRoute, error) {
	panic("preparing maintenance must never resolve provider credentials")
}

func compactionRouteSelection(req ProviderRouteRequest) SelectedProviderRoute {
	return SelectedProviderRoute{
		Provider: "openai", Model: "small-compactor", KeyName: "fixture-only-key-label",
		RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
		ExpiresAt:    time.Now().Add(time.Minute),
		ModelProfile: &ProviderModelProfile{ContextWindowTokens: 10000, MaxOutputTokens: 4096},
	}
}

func compactionRouteFixture(t *testing.T, tenant string) (context.Context, ProviderRoute) {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), identity.Identity{TenantID: tenant, UserID: "user", SessionID: "session"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTrustedProviderRoute(ctx, TrustedProviderRouteContext{
		Route:     ProviderRoute{RouteID: "driving-route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "large-model"},
		RuntimeID: "runtime", EffectiveAgentID: "agent", TaskID: "task", Purpose: ProviderRoutePurposeRun,
	})
	output := 128000
	ctx = withCompactionRequest(ctx, CompleteRequest{Model: "large-model", MaxTokens: &output, ReasoningEffort: ReasoningHigh}, ConfigSnapshot{})
	ctx, err = CompactionAttemptContext(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, ProviderRoute{RouteID: "maintenance-route", RouteGeneration: 3, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "small-compactor"}
}

func TestRoutedCompaction_IndependentBudgetIdentityAndConcurrentReuse(t *testing.T) {
	var count atomic.Int32
	resolver := compactionRouteResolver{selectFn: func(_ context.Context, req ProviderRouteRequest) (SelectedProviderRoute, error) {
		if req.RuntimeID != "runtime" || req.EffectiveAgentID != "agent" || req.TaskID != "task" || req.UserID != "user" ||
			req.SessionID != "session" || req.LogicalRunID != "run" || req.LogicalCallID == "" || req.Purpose != ProviderRoutePurposeRun || req.ModelSelector != "small-compactor" {
			t.Errorf("lost route or admitted identity: %+v", req)
		}
		count.Add(1)
		return compactionRouteSelection(req), nil
	}}
	cfg := ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}
	var wg sync.WaitGroup
	for i := range 128 {
		ctx, route := compactionRouteFixture(t, fmt.Sprintf("tenant-%d", i))
		wg.Go(func() {
			output := 2048
			child, req, budget, err := PrepareRoutedCompactionRequest(ctx, CompleteRequest{MaxTokens: &output}, route, cfg, .05)
			if err != nil {
				t.Error(err)
				return
			}
			if req.Model != "small-compactor" || *req.MaxTokens != 2048 || budget.Profile.ContextWindowTokens != 10000 || budget.InputLimit != 7452 || req.ReasoningEffort != "" || !req.ReasoningEffortExplicit {
				t.Errorf("wrong independent controls: %+v %+v", req, budget)
			}
			parentRoute, _ := TrustedProviderRouteFrom(ctx)
			childRoute, _ := TrustedProviderRouteFrom(child)
			if parentRoute.Route.RouteID != "driving-route" || childRoute.Route != route {
				t.Error("parent route mutated or child route missing")
			}
			q, _ := identity.QuadrupleFrom(child)
			if q.TenantID != fmt.Sprintf("tenant-%d", i) {
				t.Error("cross-tenant context")
			}
		})
	}
	wg.Wait()
	if count.Load() != 128 {
		t.Fatal("not all independent selections ran")
	}
}

func TestRoutedCompaction_RefusesStaleMissingProfileAndForeignGrants(t *testing.T) {
	for _, mode := range []string{"revoked", "expired", "wrong-route", "missing-profile", "grant", "parent-grant", "unadmitted"} {
		t.Run(mode, func(t *testing.T) {
			ctx, route := compactionRouteFixture(t, "tenant")
			calls := 0
			resolver := compactionRouteResolver{selectFn: func(_ context.Context, req ProviderRouteRequest) (SelectedProviderRoute, error) {
				calls++
				if mode == "revoked" {
					return SelectedProviderRoute{}, errors.New("private resolver detail")
				}
				r := compactionRouteSelection(req)
				switch mode {
				case "expired":
					r.ExpiresAt = time.Now().Add(-time.Second)
				case "wrong-route":
					r.RouteID = "foreign"
				case "missing-profile":
					r.ModelProfile = nil
				}
				return r, nil
			}}
			output := 2048
			req := CompleteRequest{MaxTokens: &output}
			if mode == "grant" {
				req.ExternalGrant = &ExternalGrant{}
			}
			if mode == "parent-grant" {
				ctx = withCompactionRequest(ctx, CompleteRequest{ExternalGrant: &ExternalGrant{}}, ConfigSnapshot{})
			}
			if mode == "unadmitted" {
				ctx = t.Context()
			}
			_, _, _, err := PrepareRoutedCompactionRequest(ctx, req, route, ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}, .05)
			if err == nil {
				t.Fatal("invalid route accepted")
			}
			if (mode == "grant" || mode == "parent-grant" || mode == "unadmitted") && calls != 0 {
				t.Fatal("dispatch before authority validation")
			}
		})
	}
}

func TestRoutedCompaction_ActualCallRechecksRevocation(t *testing.T) {
	ctx, route := compactionRouteFixture(t, "tenant")
	var revoked atomic.Bool
	resolver := compactionRouteResolver{selectFn: func(_ context.Context, req ProviderRouteRequest) (SelectedProviderRoute, error) {
		if revoked.Load() {
			return SelectedProviderRoute{}, errors.New("revoked")
		}
		return compactionRouteSelection(req), nil
	}}
	cfg := ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}
	output := 2048
	ctx, req, _, err := PrepareRoutedCompactionRequest(ctx, CompleteRequest{MaxTokens: &output}, route, cfg, .05)
	if err != nil {
		t.Fatal(err)
	}
	probe := &routePolicyProbe{}
	client := newProviderRouteClient(probe, cfg, routeClientValidator{allowed: map[string]bool{"openai": true}})
	revoked.Store(true)
	if _, err := client.Complete(ctx, req); !errors.Is(err, ErrProviderRouteResolutionFailed) {
		t.Fatalf("revocation: %v", err)
	}
	if probe.calls != 0 {
		t.Fatal("revoked maintenance reached the provider")
	}
}
