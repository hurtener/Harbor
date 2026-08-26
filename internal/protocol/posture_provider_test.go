package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/provider"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

type providerCatalogFixture struct{}

func (providerCatalogFixture) Descriptors(context.Context) []provider.ProviderDescriptor {
	return []provider.ProviderDescriptor{{ID: "openai", Kind: "native", Validation: provider.OperationSupport{State: provider.SupportSupported, RuntimeOrigin: true, Bounded: true}, Discovery: provider.OperationSupport{State: provider.SupportSupported, RuntimeOrigin: true, Bounded: true}}}
}

type routeAwareCatalogFixture struct{ providerCatalogFixture }

func (routeAwareCatalogFixture) Validate(ctx context.Context, req provider.ValidationRequest) provider.ValidationResult {
	trusted, ok := llm.TrustedProviderRouteFrom(ctx)
	if !ok || trusted.EffectiveAgentID != "agent-a" || trusted.RuntimeID != "runtime-a" || trusted.TaskID == "" || trusted.Purpose != llm.ProviderRoutePurposePosture {
		return provider.ValidationResult{Outcome: provider.Outcome{State: provider.SupportUnavailable}}
	}
	return provider.ValidationResult{ProviderID: "openai", Outcome: provider.Outcome{State: provider.SupportSupported}, Route: &provider.RouteObservation{
		RouteID: trusted.Route.RouteID, RouteGeneration: trusted.Route.RouteGeneration,
		ProviderConnectionID:         trusted.Route.ProviderConnectionID,
		ProviderConnectionGeneration: trusted.Route.ProviderConnectionGeneration,
		CredentialAssetGeneration:    trusted.Route.CredentialAssetGeneration, Ready: true,
	}}
}

func (providerCatalogFixture) Validate(context.Context, provider.ValidationRequest) provider.ValidationResult {
	return provider.ValidationResult{ProviderID: "openai", Outcome: provider.Outcome{State: provider.SupportSupported, Code: "provider_reachable", Message: "provider accepted", RuntimeOrigin: true}}
}

func (providerCatalogFixture) Discover(context.Context, provider.DiscoveryRequest) (provider.DiscoveryResult, error) {
	return provider.DiscoveryResult{ProviderID: "openai", Outcome: provider.Outcome{State: provider.SupportSupported, Code: "models_discovered", Message: "models discovered", RuntimeOrigin: true}, Models: []provider.Model{{ID: "gpt-test", Source: provider.ModelSourceDiscovered}}, Pages: 1, ModelCount: 1}, nil
}

func providerPostureSurface(t *testing.T, catalog provider.CatalogSurface) *protocol.PostureSurface {
	t.Helper()
	bus := newPostureBus(t)
	surface, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Clock: fixedClock, BootedAt: fixedClock().Add(-time.Hour),
		Health:     func(context.Context) []types.SubsystemHealth { return nil },
		Counters:   func(context.Context, identity.Identity) types.RuntimeCounters { return types.RuntimeCounters{} },
		Drivers:    func() []types.SubsystemDriver { return nil },
		Metrics:    func(context.Context) types.MetricsSnapshot { return types.MetricsSnapshot{} },
		Governance: newPostureGovernance(), LLM: newPostureLLM(),
		ProviderCatalog: catalog, ProviderCatalogAvailable: catalog != nil,
		Redactor: patterns.New(), Bus: bus, InstanceID: "provider-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return surface
}

func TestProviderCatalogOperationIsProtectedAndRuntimeOrigin(t *testing.T) {
	surface := providerPostureSurface(t, providerCatalogFixture{})
	req := &types.RuntimeInfoRequest{Identity: types.IdentityScope{Tenant: "tenant-a", User: "user-a", Session: "session-a"}, ProviderOperation: "discover", ProviderID: "openai"}
	if _, err := surface.Dispatch(context.Background(), methods.MethodLLMPosture, req); err == nil {
		t.Fatal("provider operation without admin scope unexpectedly succeeded")
	} else {
		var pe *protoerrors.Error
		if !asProtocolError(err, &pe) || pe.Code != protoerrors.CodeScopeMismatch {
			t.Fatalf("error=%v, want scope_mismatch", err)
		}
	}
	ctx := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	response, err := surface.Dispatch(ctx, methods.MethodLLMPosture, req)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := response.(types.LLMProviderOperationResponse)
	if !ok || !got.RuntimeOrigin || got.Discovery == nil || got.Discovery.Outcome.RuntimeOrigin != true || len(got.Discovery.Models) != 1 {
		t.Fatalf("unexpected provider operation response: %#v", response)
	}
	info, err := surface.Dispatch(ctx, methods.MethodRuntimeInfo, &types.RuntimeInfoRequest{Identity: req.Identity})
	if err != nil {
		t.Fatal(err)
	}
	runtimeInfo := info.(*types.RuntimeInfo)
	seen := false
	for _, capability := range runtimeInfo.Capabilities {
		if capability == types.CapLLMProviderCatalog {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("runtime.info omitted provider catalog capability: %#v", runtimeInfo.Capabilities)
	}
}

func TestProviderCatalogOperationFailsClosedWhenNotWired(t *testing.T) {
	surface := providerPostureSurface(t, nil)
	req := &types.RuntimeInfoRequest{Identity: types.IdentityScope{Tenant: "tenant-a", User: "user-a", Session: "session-a"}, ProviderOperation: "validate", ProviderID: "openai"}
	_, err := surface.Dispatch(auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin}), methods.MethodLLMPosture, req)
	if err == nil {
		t.Fatal("unwired provider operation unexpectedly succeeded")
	}
	var pe *protoerrors.Error
	if !asProtocolError(err, &pe) || pe.Code != protoerrors.CodeRuntimeError {
		t.Fatalf("error=%v, want runtime_error", err)
	}
}

func TestProviderCatalogRouteRequiresSignedAgentReachAndEchoesGenerations(t *testing.T) {
	bus := newPostureBus(t)
	surface, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Clock: fixedClock, Health: func(context.Context) []types.SubsystemHealth { return nil },
		Counters: func(context.Context, identity.Identity) types.RuntimeCounters { return types.RuntimeCounters{} },
		Drivers:  func() []types.SubsystemDriver { return nil }, Metrics: func(context.Context) types.MetricsSnapshot { return types.MetricsSnapshot{} },
		Governance: newPostureGovernance(), LLM: newPostureLLM(), ProviderCatalog: routeAwareCatalogFixture{},
		AgentReach: auth.NewAgentReachAuthorizer(), ProviderRouteRuntimeID: "runtime-a",
		Redactor: patterns.New(), Bus: bus, InstanceID: "provider-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &types.RuntimeInfoRequest{
		Identity:          types.IdentityScope{Tenant: "tenant-a", User: "user-a", Session: "session-a"},
		ProviderOperation: "validate", ProviderAgentID: "agent-a",
		ProviderRoute: &types.LLMProviderRouteSelector{RouteID: "route-a", RouteGeneration: 2,
			ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 3,
			CredentialAssetGeneration: 4, ModelSelector: "fast"},
	}
	admin := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	if _, err := surface.Dispatch(admin, methods.MethodLLMPosture, req); err == nil {
		t.Fatal("route operation without signed Agent reach succeeded")
	}
	fleet := auth.WithAgentReach(auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeConsoleFleet}), []string{"agent-a"})
	if _, err := surface.Dispatch(fleet, methods.MethodLLMPosture, req); err == nil {
		t.Fatal("route operation with fleet-only scope succeeded")
	}
	ctx := auth.WithAgentReach(admin, []string{"agent-a"})
	response, err := surface.Dispatch(ctx, methods.MethodLLMPosture, req)
	if err != nil {
		t.Fatal(err)
	}
	got := response.(types.LLMProviderOperationResponse)
	if got.Route == nil || !got.Route.Ready || got.Route.RouteGeneration != 2 || got.Route.ProviderConnectionGeneration != 3 || got.Route.CredentialAssetGeneration != 4 {
		t.Fatalf("route observation = %#v", got.Route)
	}
}

func asProtocolError(err error, out **protoerrors.Error) bool {
	if err == nil {
		return false
	}
	return errors.As(err, out)
}
