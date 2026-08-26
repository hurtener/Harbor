package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

type routePolicyProbe struct {
	profiles map[string]int
	seen     CompleteRequest
	selected SelectedProviderRoute
	calls    int
}

func (p *routePolicyProbe) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	p.calls++
	p.seen = req
	p.selected, _ = SelectedProviderRouteFrom(ctx)
	limit := p.profiles[req.Model]
	for _, message := range req.Messages {
		if message.Content.Text != nil && len(*message.Content.Text)/4 >= limit {
			return CompleteResponse{}, ErrContextWindowExceeded
		}
	}
	return CompleteResponse{Content: "ok"}, nil
}

func (*routePolicyProbe) Close(context.Context) error { return nil }

type routeClientResolver struct {
	selected SelectedProviderRoute
}

type routeClientValidator struct {
	allowed map[string]bool
}

func (v routeClientValidator) ValidateProviderRouteSelection(selected SelectedProviderRoute) error {
	if !v.allowed[selected.Provider] {
		return ErrProviderRouteInvalid
	}
	return nil
}

func (r routeClientResolver) SelectProviderRoute(context.Context, ProviderRouteRequest) (SelectedProviderRoute, error) {
	return r.selected, nil
}

func (routeClientResolver) ResolveProviderRoute(context.Context, ProviderRouteRequest) (ResolvedProviderRoute, error) {
	panic("credential resolution belongs to the leaf attempt, not pre-policy selection")
}

func TestProviderRouteClient_SelectedModelReachesModelSensitivePolicy(t *testing.T) {
	now := time.Now().UTC()
	route := ProviderRoute{RouteID: "route", RouteGeneration: 2, ProviderConnectionID: "connection", ProviderConnectionGeneration: 3, CredentialAssetGeneration: 4, ModelSelector: "small"}
	selected := SelectedProviderRoute{
		Provider: "openai", Model: "selected-small", KeyName: "route key", RouteID: route.RouteID, RouteGeneration: route.RouteGeneration,
		ProviderConnectionID: route.ProviderConnectionID, ProviderConnectionGeneration: route.ProviderConnectionGeneration,
		CredentialAssetGeneration: route.CredentialAssetGeneration, ModelSelector: route.ModelSelector, ExpiresAt: now.Add(time.Minute),
	}
	probe := &routePolicyProbe{profiles: map[string]int{"runtime-large": 10_000, "selected-small": 10}}
	client := &providerRouteClient{inner: probe, cfg: ProviderRouteConfig{Resolver: routeClientResolver{selected: selected}, RuntimeID: "runtime"}, validator: routeClientValidator{allowed: map[string]bool{"openai": true}}, now: func() time.Time { return now }}
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}
	ctx, err := identity.WithRun(context.Background(), id, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTrustedProviderRoute(ctx, TrustedProviderRouteContext{Route: route, EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", Purpose: ProviderRoutePurposeRun})
	text := strings.Repeat("x", 80)
	_, err = client.Complete(ctx, CompleteRequest{Model: "runtime-large", Messages: []ChatMessage{{Role: RoleUser, Content: Content{Text: &text}}}})
	if !errors.Is(err, ErrContextWindowExceeded) {
		t.Fatalf("Complete error = %v, want selected model's context-window policy", err)
	}
	if probe.seen.Model != "selected-small" || probe.selected != selected {
		t.Fatalf("downstream saw model=%q selection=%+v, want selected-small and exact route", probe.seen.Model, probe.selected)
	}
}

func TestProviderRouteClient_UnpermittedSelectionNeverReachesPolicyChain(t *testing.T) {
	now := time.Now().UTC()
	route := ProviderRoute{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "fast"}
	selected := SelectedProviderRoute{
		Provider: "arbitrary-provider", Model: "arbitrary-model", KeyName: "route key", RouteID: route.RouteID, RouteGeneration: route.RouteGeneration,
		ProviderConnectionID: route.ProviderConnectionID, ProviderConnectionGeneration: route.ProviderConnectionGeneration,
		CredentialAssetGeneration: route.CredentialAssetGeneration, ModelSelector: route.ModelSelector, ExpiresAt: now.Add(time.Minute),
	}
	probe := &routePolicyProbe{profiles: map[string]int{"arbitrary-model": 1000}}
	client := &providerRouteClient{inner: probe, cfg: ProviderRouteConfig{
		Resolver: routeClientResolver{selected: selected}, RuntimeID: "runtime",
	}, validator: routeClientValidator{allowed: map[string]bool{"openai": true}}, now: func() time.Time { return now }}
	ctx, err := identity.WithRun(context.Background(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTrustedProviderRoute(ctx, TrustedProviderRouteContext{Route: route, EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", Purpose: ProviderRoutePurposeRun})
	if _, err := client.Complete(ctx, CompleteRequest{}); !errors.Is(err, ErrProviderRouteInvalid) {
		t.Fatalf("unpermitted selection error = %v, want ErrProviderRouteInvalid", err)
	}
	if probe.calls != 0 {
		t.Fatalf("model-sensitive inner wrapper called %d times for unpermitted provider", probe.calls)
	}
}
