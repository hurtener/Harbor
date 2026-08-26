package bifrost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

type rotatingRouteResolver struct {
	resolveCalls atomic.Int64
	revokedAfter int64
	selected     llm.SelectedProviderRoute
}

func (r *rotatingRouteResolver) SelectProviderRoute(context.Context, llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
	return r.selected, nil
}

func (r *rotatingRouteResolver) ResolveProviderRoute(_ context.Context, req llm.ProviderRouteRequest) (llm.ResolvedProviderRoute, error) {
	call := r.resolveCalls.Add(1)
	if r.revokedAfter > 0 && call > r.revokedAfter {
		return llm.ResolvedProviderRoute{}, errors.New("revoked credential canary")
	}
	return llm.ResolvedProviderRoute{
		Provider: r.selected.Provider, Model: r.selected.Model, KeyName: r.selected.KeyName,
		RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
		Credential: "attempt-key", ExpiresAt: time.Now().Add(time.Minute),
	}, nil
}

func TestAccount_ResolvedRouteKeysAreContextConfined(t *testing.T) {
	account, err := newRouteAccount(llm.NetworkDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, credential := range []string{"tenant-a-key", "tenant-b-key"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := llm.WithResolvedProviderRoute(context.Background(), llm.ResolvedProviderRoute{
				Provider: "openai", Model: "model", KeyName: "route key", RouteID: "route", RouteGeneration: 1,
				ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1,
				Credential: credential, ExpiresAt: time.Now().Add(time.Minute),
			})
			keys, err := account.GetKeysForProvider(ctx, bfschemas.OpenAI)
			if err != nil || len(keys) != 1 || keys[0].Value.Val != credential {
				t.Errorf("credential %q keys=%+v err=%v", credential, keys, err)
			}
		}()
	}
	wg.Wait()
	if _, err := account.GetKeysForProvider(context.Background(), bfschemas.OpenAI); err == nil {
		t.Fatal("route account accepted a call without resolved route context")
	}
}

func TestAccount_ExternalRouteCannotInventCustomEndpoint(t *testing.T) {
	account, err := newRouteAccount(llm.NetworkDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if curatedRouteProvider(bfschemas.ModelProvider("caller-controlled-provider")) {
		t.Fatal("undeclared custom provider accepted")
	}
	if !curatedRouteProvider(bfschemas.Anthropic) {
		t.Fatal("boot-allowed standard provider rejected")
	}
	if _, err := account.GetConfigForProvider(bfschemas.ModelProvider("caller-controlled-provider")); err == nil {
		t.Fatal("undeclared custom provider config accepted")
	}
}

func TestRouteAccount_MapsTypedAzureAndLocalEndpoints(t *testing.T) {
	account, err := newRouteAccount(llm.NetworkDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		provider bfschemas.ModelProvider
		kind     llm.ProviderEndpointKind
		check    func(bfschemas.Key) bool
	}{
		{bfschemas.Azure, llm.ProviderEndpointAzure, func(k bfschemas.Key) bool { return k.AzureKeyConfig != nil && k.AzureKeyConfig.Endpoint.Val != "" }},
		{bfschemas.VLLM, llm.ProviderEndpointVLLM, func(k bfschemas.Key) bool { return k.VLLMKeyConfig != nil && k.VLLMKeyConfig.ModelName == "model" }},
		{bfschemas.Ollama, llm.ProviderEndpointOllama, func(k bfschemas.Key) bool { return k.OllamaKeyConfig != nil }},
		{bfschemas.SGL, llm.ProviderEndpointSGL, func(k bfschemas.Key) bool { return k.SGLKeyConfig != nil }},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			value, digest, err := llm.NormalizeProviderEndpoint("http://127.0.0.1:11434")
			if err != nil {
				t.Fatal(err)
			}
			ctx := llm.WithResolvedProviderRoute(context.Background(), llm.ResolvedProviderRoute{
				Provider: string(tc.provider), Model: "model", KeyName: "typed key", RouteID: "route", RouteGeneration: 1,
				ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, Credential: "fixture",
				Endpoint: &llm.ProviderEndpointBinding{Kind: tc.kind, Value: value, Digest: digest}, ExpiresAt: time.Now().Add(time.Minute),
			})
			keys, err := account.GetKeysForProvider(ctx, tc.provider)
			if err != nil || len(keys) != 1 || !tc.check(keys[0]) {
				t.Fatalf("keys=%+v err=%v", keys, err)
			}
		})
	}
}

func TestCuratedRouteProvidersExcludeUnsupportedShapes(t *testing.T) {
	for _, provider := range []bfschemas.ModelProvider{bfschemas.Vertex, bfschemas.Bedrock, bfschemas.BedrockMantle, bfschemas.Elevenlabs, bfschemas.Runway, bfschemas.Runware} {
		if curatedRouteProvider(provider) {
			t.Fatalf("unsupported provider %q entered curated route set", provider)
		}
	}
}

func TestDriver_RoutedAttemptsResolveEveryTimeAndRevocationFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	route := llm.ProviderRoute{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "fast"}
	resolver := &rotatingRouteResolver{revokedAfter: 1, selected: llm.SelectedProviderRoute{
		Provider: "openai", Model: "model", KeyName: "route key", RouteID: route.RouteID, RouteGeneration: route.RouteGeneration,
		ProviderConnectionID: route.ProviderConnectionID, ProviderConnectionGeneration: route.ProviderConnectionGeneration,
		CredentialAssetGeneration: route.CredentialAssetGeneration, ModelSelector: route.ModelSelector, ExpiresAt: now.Add(time.Minute),
	}}
	provider := newStubClient()
	driver := newDriverWithClient(newStubClient(), bfschemas.OpenAI, nil)
	driver.routeClient = provider
	driver.providerRoute = llm.ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}
	ctx, err := identity.WithRun(context.Background(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = llm.WithTrustedProviderRoute(ctx, llm.TrustedProviderRouteContext{Route: route, EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task"})
	ctx = llm.WithSelectedProviderRoute(ctx, resolver.selected)
	if _, err := driver.Complete(ctx, llm.CompleteRequest{Model: "model"}); err != nil {
		t.Fatalf("first routed attempt: %v", err)
	}
	if _, err := driver.Complete(ctx, llm.CompleteRequest{Model: "model"}); !errors.Is(err, llm.ErrProviderRouteResolutionFailed) {
		t.Fatalf("revoked routed attempt error = %v", err)
	}
	if got := resolver.resolveCalls.Load(); got != 2 {
		t.Fatalf("ResolveProviderRoute calls = %d, want one per actual Harbor attempt", got)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider attempts = %d, want revoked second attempt blocked before provider", got)
	}
}

func TestDriver_RoutedProviderFailureIsContentFree(t *testing.T) {
	const (
		endpoint   = "https://resolver.example.invalid/v1"
		credential = "route-provider-credential-canary"
	)
	status := 502
	provider := newStubClient()
	provider.chatHandler = func(_ *bfschemas.BifrostChatRequest) (*bfschemas.BifrostChatResponse, *bfschemas.BifrostError) {
		return nil, &bfschemas.BifrostError{
			StatusCode: &status,
			Error: &bfschemas.ErrorField{
				Message: "upstream rejected endpoint=" + endpoint + " credential=" + credential,
			},
		}
	}

	now := time.Now().UTC()
	route := llm.ProviderRoute{
		RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "fast",
	}
	resolver := &rotatingRouteResolver{selected: llm.SelectedProviderRoute{
		Provider: "openai", Model: "model", KeyName: "route key", RouteID: route.RouteID,
		RouteGeneration: route.RouteGeneration, ProviderConnectionID: route.ProviderConnectionID,
		ProviderConnectionGeneration: route.ProviderConnectionGeneration,
		CredentialAssetGeneration:    route.CredentialAssetGeneration,
		ModelSelector:                route.ModelSelector, ExpiresAt: now.Add(time.Minute),
	}}
	driver := newDriverWithClient(newStubClient(), bfschemas.OpenAI, nil)
	driver.routeClient = provider
	driver.providerRoute = llm.ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}
	ctx, err := identity.WithRun(context.Background(), identity.Identity{
		TenantID: "tenant", UserID: "user", SessionID: "session",
	}, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = llm.WithTrustedProviderRoute(ctx, llm.TrustedProviderRouteContext{
		Route: route, EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task",
	})
	ctx = llm.WithSelectedProviderRoute(ctx, resolver.selected)

	_, err = driver.Complete(ctx, llm.CompleteRequest{Model: "model"})
	if !errors.Is(err, llm.ErrProviderRouteProviderFailed) {
		t.Fatalf("routed provider error = %v, want ErrProviderRouteProviderFailed", err)
	}
	if strings.Contains(err.Error(), endpoint) || strings.Contains(err.Error(), credential) {
		t.Fatalf("terminal error leaked route material: %q", err)
	}

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Error(
		"routed provider failure", slog.String("err", err.Error()),
	)
	if strings.Contains(logs.String(), endpoint) || strings.Contains(logs.String(), credential) {
		t.Fatalf("log-visible error leaked route material: %q", logs.String())
	}
}
