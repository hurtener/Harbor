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

func TestProviderRouteClient_UsesRouteModelProfileWithoutStaticProfile(t *testing.T) {
	now := time.Now().UTC()
	route := ProviderRoute{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "fast"}
	selected := SelectedProviderRoute{
		Provider: "openai", Model: "arbitrary/model", KeyName: "route key", RouteID: route.RouteID, RouteGeneration: route.RouteGeneration,
		ProviderConnectionID: route.ProviderConnectionID, ProviderConnectionGeneration: route.ProviderConnectionGeneration,
		CredentialAssetGeneration: route.CredentialAssetGeneration, ModelSelector: route.ModelSelector, ExpiresAt: now.Add(time.Minute),
		ModelProfile: &ProviderModelProfile{ContextWindowTokens: 8192, MaxOutputTokens: 1024, ReasoningEffortLevels: []ReasoningEffort{ReasoningHigh}},
	}
	probe := &routePolicyProbe{profiles: map[string]int{"arbitrary/model": 1000}}
	client := &providerRouteClient{inner: probe, cfg: ProviderRouteConfig{Resolver: routeClientResolver{selected: selected}, RuntimeID: "runtime"}, validator: routeClientValidator{allowed: map[string]bool{"openai": true}}, now: func() time.Time { return now }}
	ctx, err := identity.WithRun(context.Background(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTrustedProviderRoute(ctx, TrustedProviderRouteContext{Route: route, EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", Purpose: ProviderRoutePurposeRun})
	if _, err := client.Complete(ctx, CompleteRequest{Model: "boot/profile", ReasoningEffort: ReasoningHigh}); err != nil {
		t.Fatalf("route profile completion error = %v", err)
	}
	if probe.seen.Model != selected.Model {
		t.Fatalf("downstream model = %q, want %q", probe.seen.Model, selected.Model)
	}
	profile, ok := EffectiveModelProfile(probe.seen, ConfigSnapshot{})
	if !ok || profile.ContextWindowTokens != 8192 || profile.DefaultMaxTokens == nil || *profile.DefaultMaxTokens != 1024 || profile.ReasoningEffort != "" {
		t.Fatalf("downstream effective profile = %#v, ok=%t; want route output ceiling and no policy reasoning default", profile, ok)
	}
}

func TestProviderRouteClient_DefaultsOmittedMaxTokensToRouteCeiling(t *testing.T) {
	now := time.Now().UTC()
	route := ProviderRoute{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "fast"}
	selected := SelectedProviderRoute{
		Provider: "openai", Model: "arbitrary/model", KeyName: "route key", RouteID: route.RouteID, RouteGeneration: route.RouteGeneration,
		ProviderConnectionID: route.ProviderConnectionID, ProviderConnectionGeneration: route.ProviderConnectionGeneration,
		CredentialAssetGeneration: route.CredentialAssetGeneration, ModelSelector: route.ModelSelector, ExpiresAt: now.Add(time.Minute),
		ModelProfile: &ProviderModelProfile{ContextWindowTokens: 8192, MaxOutputTokens: 1024},
	}
	probe := &routePolicyProbe{profiles: map[string]int{"arbitrary/model": 1000}}
	safe := newSafetyClient(probe, ConfigSnapshot{HeavyOutputThreshold: 32 << 10}, Deps{})
	client := &providerRouteClient{inner: safe, cfg: ProviderRouteConfig{Resolver: routeClientResolver{selected: selected}, RuntimeID: "runtime"}, validator: routeClientValidator{allowed: map[string]bool{"openai": true}}, now: func() time.Time { return now }}
	ctx, err := identity.WithRun(context.Background(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTrustedProviderRoute(ctx, TrustedProviderRouteContext{Route: route, EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", Purpose: ProviderRoutePurposeRun})
	text := "hello"
	if _, err := client.Complete(ctx, CompleteRequest{Model: "boot/profile", Messages: []ChatMessage{{Role: RoleUser, Content: Content{Text: &text}}}}); err != nil {
		t.Fatalf("route completion error = %v", err)
	}
	if probe.seen.MaxTokens == nil || *probe.seen.MaxTokens != selected.ModelProfile.MaxOutputTokens {
		t.Fatalf("downstream MaxTokens = %v, want route ceiling %d", probe.seen.MaxTokens, selected.ModelProfile.MaxOutputTokens)
	}
}

func TestProviderRouteClient_RejectsExplicitControlsOutsideRouteCapabilities(t *testing.T) {
	now := time.Now().UTC()
	route := ProviderRoute{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "fast"}
	selected := SelectedProviderRoute{
		Provider: "openai", Model: "arbitrary/model", KeyName: "route key", RouteID: route.RouteID, RouteGeneration: route.RouteGeneration,
		ProviderConnectionID: route.ProviderConnectionID, ProviderConnectionGeneration: route.ProviderConnectionGeneration,
		CredentialAssetGeneration: route.CredentialAssetGeneration, ModelSelector: route.ModelSelector, ExpiresAt: now.Add(time.Minute),
		ModelProfile: &ProviderModelProfile{ContextWindowTokens: 8192, MaxOutputTokens: 1024, ReasoningEffortLevels: []ReasoningEffort{ReasoningMedium}},
	}
	for name, req := range map[string]CompleteRequest{
		"output ceiling":   func() CompleteRequest { n := 1025; return CompleteRequest{MaxTokens: &n} }(),
		"reasoning levels": {ReasoningEffort: ReasoningHigh},
	} {
		t.Run(name, func(t *testing.T) {
			probe := &routePolicyProbe{}
			client := &providerRouteClient{inner: probe, cfg: ProviderRouteConfig{Resolver: routeClientResolver{selected: selected}, RuntimeID: "runtime"}, validator: routeClientValidator{allowed: map[string]bool{"openai": true}}, now: func() time.Time { return now }}
			ctx, err := identity.WithRun(context.Background(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, "run")
			if err != nil {
				t.Fatal(err)
			}
			ctx = WithTrustedProviderRoute(ctx, TrustedProviderRouteContext{Route: route, EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", Purpose: ProviderRoutePurposeRun})
			if _, err := client.Complete(ctx, req); !errors.Is(err, ErrProviderRouteInvalid) {
				t.Fatalf("error = %v, want ErrProviderRouteInvalid", err)
			}
			if probe.calls != 0 {
				t.Fatalf("inner calls = %d, want 0", probe.calls)
			}
		})
	}
}

func TestEffectiveModelProfileCannotCrossModelBoundary(t *testing.T) {
	request := CompleteRequest{Model: "model-a"}
	request = withTrustedModelProfile(request, "model-a", ModelProfile{ContextWindowTokens: 100})
	request.Model = "model-b"
	if _, ok := EffectiveModelProfile(request, ConfigSnapshot{ModelProfiles: map[string]ModelProfile{"model-b": {ContextWindowTokens: 200}}}); !ok {
		t.Fatal("static fallback should be available after a model switch")
	}
	request.Model = "model-c"
	if _, ok := EffectiveModelProfile(request, ConfigSnapshot{}); ok {
		t.Fatal("route profile leaked across a model switch")
	}
}

func TestEffectiveModelProfileRouteOverlayPreservesConfiguredPolicy(t *testing.T) {
	request := CompleteRequest{Model: "configured/model"}
	request = withTrustedModelProfile(request, "configured/model", ModelProfile{
		ContextWindowTokens: 8192,
		ReasoningEffort:     ReasoningHigh,
	})
	maxRetries := 3
	static := ModelProfile{
		ContextWindowTokens: 4096,
		OutputMode:          OutputModeNative,
		DefaultMaxTokens:    &maxRetries,
		ReasoningEffort:     ReasoningLow,
		MaxRetries:          2,
		Corrections:         CorrectionsProfile{ReasoningEffortRouting: ReasoningRouteThinking},
	}
	profile, ok := EffectiveModelProfile(request, ConfigSnapshot{ModelProfiles: map[string]ModelProfile{"configured/model": static}})
	if !ok {
		t.Fatal("route profile was not accepted")
	}
	if profile.ContextWindowTokens != 8192 || profile.OutputMode != OutputModeNative || profile.MaxRetries != 2 ||
		profile.Corrections.ReasoningEffortRouting != ReasoningRouteThinking || profile.ReasoningEffort != ReasoningLow {
		t.Fatalf("route overlay replaced configured policy: %#v", profile)
	}
	if profile.DefaultMaxTokens == nil || *profile.DefaultMaxTokens != maxRetries {
		t.Fatalf("route overlay dropped configured max-token policy: %#v", profile.DefaultMaxTokens)
	}
}
