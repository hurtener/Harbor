package bifrost

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	bf "github.com/maximhq/bifrost/core"
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/provider"
)

// ProviderCatalog is the runtime-owned adapter for Harbor's provider-neutral
// descriptor and model discovery contract. It uses the same Account and
// Bifrost setup as ordinary LLM execution; it never exposes the resolved key
// or a provider response body.
type ProviderCatalog struct {
	catalog     *provider.Catalog
	client      *bf.Bifrost
	routeClient *bf.Bifrost
	routePool   *routeClientPool
	mu          sync.RWMutex
	closed      bool
	route       llm.ProviderRouteConfig
}

// NewProviderCatalog constructs a catalog from the exact LLM snapshot used by
// the Bifrost driver. It is retained as a convenient offline/test constructor;
// the serving path uses NewProviderCatalogWithDeps so the catalog shares the
// opened driver's LiveKey and therefore the same broker-pulled credential.
func NewProviderCatalog(cfg llm.ConfigSnapshot) (*ProviderCatalog, error) {
	return NewProviderCatalogWithDeps(cfg, llm.Deps{})
}

// NewProviderCatalogWithDeps constructs the runtime-origin catalog over the
// same dependency seam as ordinary LLM execution. In particular, a shared
// LiveKey means a remote inference broker can seed/rotate one credential holder
// and both provider calls and catalog probes observe the same value without
// copying a secret through Protocol or the catalog surface.
func NewProviderCatalogWithDeps(cfg llm.ConfigSnapshot, deps llm.Deps) (*ProviderCatalog, error) {
	if err := llm.ValidateProviderRouteConfig(deps.ProviderRoute); err != nil {
		return nil, err
	}
	account, err := newAccount(cfg, deps)
	if err != nil {
		return nil, err
	}
	client, err := bf.Init(context.Background(), bfschemas.BifrostConfig{Account: account})
	if err != nil {
		return nil, fmt.Errorf("bifrost provider catalog: init: %w", err)
	}
	var routeClient *bf.Bifrost
	var routePool *routeClientPool
	if deps.ProviderRoute.Resolver != nil {
		routeAccount, routeErr := newRouteAccount(cfg.NetworkDefaults)
		if routeErr != nil {
			client.Shutdown()
			return nil, fmt.Errorf("bifrost provider catalog: initialize curated provider routes: %w", routeErr)
		}
		routeClient, routeErr = bf.Init(context.Background(), bfschemas.BifrostConfig{Account: routeAccount})
		if routeErr != nil {
			client.Shutdown()
			return nil, fmt.Errorf("bifrost provider catalog: initialize curated provider route workers: %w", routeErr)
		}
		routePool = newRouteClientPool(defaultRouteClientPoolCapacity, cfg.NetworkDefaults)
	}
	descriptors := StaticProviderDescriptors(cfg.CustomProviders)
	active := []string{cfg.Provider}
	manual := make(map[string][]string)
	for _, custom := range cfg.CustomProviders {
		manual[custom.Name] = append([]string(nil), custom.Models...)
	}
	catalog, err := provider.NewCatalog(&bifrostModelLister{
		client: client, routeClient: routeClient, routePool: routePool,
	}, descriptors, active, manual)
	if err != nil {
		if routePool != nil {
			routePool.Close()
		}
		if routeClient != nil {
			routeClient.Shutdown()
		}
		client.Shutdown()
		return nil, fmt.Errorf("bifrost provider catalog: descriptors: %w", err)
	}
	return &ProviderCatalog{
		catalog: catalog, client: client, routeClient: routeClient, routePool: routePool,
		route: deps.ProviderRoute,
	}, nil
}

// StaticProviderDescriptors returns the technical descriptor registry for the
// Bifrost standard providers plus any configured OpenAI-compatible providers.
// It returns no endpoint values, environment variable names, or secrets.
func StaticProviderDescriptors(custom []llm.CustomProviderSpec) []provider.ProviderDescriptor {
	out := make([]provider.ProviderDescriptor, 0, len(bfschemas.StandardProviders)+len(custom))
	for _, providerID := range bfschemas.StandardProviders {
		keyless := bf.CanProviderKeyValueBeEmpty(providerID)
		out = append(out, provider.ProviderDescriptor{
			ID:              string(providerID),
			Kind:            "native",
			CredentialModes: credentialModes(keyless),
			CredentialFields: []provider.CredentialField{{
				Name:     "api_key",
				Kind:     provider.FieldSecret,
				Required: !keyless,
				Secret:   true,
			}},
			// Native base_url handling varies by provider. It is exposed
			// as an operator-managed fact, not promised as a portable
			// custom endpoint contract.
			CustomEndpoint: provider.SupportManual,
			Validation: provider.OperationSupport{
				State: provider.SupportSupported, RuntimeOrigin: true, Bounded: true,
			},
			Discovery: provider.OperationSupport{
				State: provider.SupportSupported, RuntimeOrigin: true, Bounded: true,
			},
		})
	}
	for _, customProvider := range custom {
		out = append(out, provider.ProviderDescriptor{
			ID:              customProvider.Name,
			Kind:            "custom",
			CredentialModes: []provider.CredentialMode{provider.CredentialAPIKey},
			CredentialFields: []provider.CredentialField{
				{Name: "api_key", Kind: provider.FieldSecret, Required: true, Secret: true},
				{Name: "base_url", Kind: provider.FieldURL, Required: true},
			},
			CustomEndpoint: provider.SupportSupported,
			Validation: provider.OperationSupport{
				State: provider.SupportSupported, RuntimeOrigin: true, Bounded: true,
			},
			// Bifrost can query an OpenAI-compatible custom endpoint. The
			// declared model list remains the explicit manual fallback when
			// that endpoint does not expose a usable models operation.
			Discovery: provider.OperationSupport{
				State: provider.SupportSupported, RuntimeOrigin: true, Bounded: true,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func credentialModes(keyless bool) []provider.CredentialMode {
	if keyless {
		return []provider.CredentialMode{provider.CredentialAPIKey, provider.CredentialNone}
	}
	return []provider.CredentialMode{provider.CredentialAPIKey}
}

// Descriptors returns a defensive technical descriptor snapshot.
func (c *ProviderCatalog) Descriptors(ctx context.Context) []provider.ProviderDescriptor {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return nil
	}
	return c.catalog.Descriptors(ctx)
}

// Validate executes one bounded runtime-origin provider validation probe.
func (c *ProviderCatalog) Validate(ctx context.Context, req provider.ValidationRequest) provider.ValidationResult {
	if c == nil {
		return provider.ValidationResult{ProviderID: req.ProviderID, Outcome: provider.Outcome{
			State: provider.SupportUnavailable, Code: "provider_catalog_closed", Message: "provider catalog is closed",
		}}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return provider.ValidationResult{ProviderID: req.ProviderID, Outcome: provider.Outcome{
			State: provider.SupportUnavailable, Code: "provider_catalog_closed", Message: "provider catalog is closed",
		}}
	}
	routeCtx, providerID, observation, err := c.resolveCatalogRoute(ctx)
	if err != nil {
		return provider.ValidationResult{ProviderID: req.ProviderID, Route: observation, Outcome: provider.Outcome{
			State: provider.SupportUnavailable, Code: "provider_route_unavailable", Message: "external provider route is unavailable",
		}}
	}
	if observation != nil {
		req.ProviderID, req.ExternalRoute = providerID, true
	}
	result := c.catalog.Validate(routeCtx, req)
	result.Route = observation
	if result.Route != nil {
		result.Route.Ready = result.Outcome.State == provider.SupportSupported || result.Outcome.State == provider.SupportPartial
	}
	return result
}

// Discover executes bounded runtime-origin model discovery.
func (c *ProviderCatalog) Discover(ctx context.Context, req provider.DiscoveryRequest) (provider.DiscoveryResult, error) {
	if c == nil {
		return provider.DiscoveryResult{ProviderID: req.ProviderID, Outcome: provider.Outcome{
			State: provider.SupportUnavailable, Code: "provider_catalog_closed", Message: "provider catalog is closed",
		}}, nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return provider.DiscoveryResult{ProviderID: req.ProviderID, Outcome: provider.Outcome{
			State: provider.SupportUnavailable, Code: "provider_catalog_closed", Message: "provider catalog is closed",
		}}, nil
	}
	routeCtx, providerID, observation, routeErr := c.resolveCatalogRoute(ctx)
	if routeErr != nil {
		return provider.DiscoveryResult{ProviderID: req.ProviderID, Route: observation, Outcome: provider.Outcome{
			State: provider.SupportUnavailable, Code: "provider_route_unavailable", Message: "external provider route is unavailable",
		}}, nil
	}
	if observation != nil {
		req.ProviderID, req.ExternalRoute = providerID, true
	}
	result, err := c.catalog.Discover(routeCtx, req)
	result.Route = observation
	if result.Route != nil {
		result.Route.Ready = result.Outcome.State == provider.SupportSupported || result.Outcome.State == provider.SupportPartial || result.Outcome.State == provider.SupportManual
	}
	return result, err
}

func (c *ProviderCatalog) resolveCatalogRoute(ctx context.Context) (context.Context, string, *provider.RouteObservation, error) {
	trusted, explicit := llm.TrustedProviderRouteFrom(ctx)
	if !explicit {
		return ctx, "", nil, nil
	}
	observation := &provider.RouteObservation{
		RouteID: trusted.Route.RouteID, RouteGeneration: trusted.Route.RouteGeneration,
		ProviderConnectionID:         trusted.Route.ProviderConnectionID,
		ProviderConnectionGeneration: trusted.Route.ProviderConnectionGeneration,
		CredentialAssetGeneration:    trusted.Route.CredentialAssetGeneration,
	}
	if err := llm.ValidateProviderRoute(trusted.Route); err != nil || trusted.Route.RouteID == "" {
		return ctx, "", observation, llm.ErrProviderRouteInvalid
	}
	if c.route.Resolver == nil || trusted.RuntimeID == "" || trusted.RuntimeID != c.route.RuntimeID || trusted.EffectiveAgentID == "" || trusted.TaskID == "" {
		return ctx, "", observation, llm.ErrProviderRouteResolverUnavailable
	}
	id, ok := identity.From(ctx)
	if !ok || id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return ctx, "", observation, llm.ErrProviderRouteInvalid
	}
	logicalRunID := trusted.TaskID
	if q, qOK := identity.QuadrupleFrom(ctx); qOK && q.Identity == id && q.RunID != "" {
		logicalRunID = q.RunID
	}
	callCtx, scope, err := llm.EnsureAttemptScope(ctx)
	if err != nil {
		return ctx, "", observation, err
	}
	resolved, err := llm.ResolveProviderRoute(callCtx, c.route, llm.ProviderRouteRequest{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
		LogicalRunID: logicalRunID, EffectiveAgentID: trusted.EffectiveAgentID, RuntimeID: trusted.RuntimeID,
		TaskID: trusted.TaskID, LogicalCallID: scope.LogicalCallID,
		RouteID: trusted.Route.RouteID, RouteGeneration: trusted.Route.RouteGeneration,
		ProviderConnectionID:         trusted.Route.ProviderConnectionID,
		ProviderConnectionGeneration: trusted.Route.ProviderConnectionGeneration,
		CredentialAssetGeneration:    trusted.Route.CredentialAssetGeneration, ModelSelector: trusted.Route.ModelSelector,
	}, time.Now())
	providerID := bfschemas.ModelProvider(resolved.Provider)
	if err != nil || !curatedRouteProvider(providerID) || validateCuratedRouteEndpoint(providerID, resolved.Endpoint) != nil {
		return ctx, "", observation, llm.ErrProviderRouteInvalid
	}
	return llm.WithResolvedProviderRoute(callCtx, resolved), resolved.Provider, observation, nil
}

// Close shuts down the Bifrost catalog client. Bifrost's Shutdown is
// synchronous and idempotent; the lifecycle lock makes concurrent Close and
// discovery calls safe and prevents a client teardown race.
func (c *ProviderCatalog) Close(_ context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	if c.routePool != nil {
		c.routePool.Close()
	}
	if c.routeClient != nil {
		c.routeClient.Shutdown()
	}
	if c.client != nil {
		c.client.Shutdown()
	}
	return nil
}

type bifrostModelLister struct {
	client      *bf.Bifrost
	routeClient *bf.Bifrost
	routePool   *routeClientPool
}

func (l *bifrostModelLister) ListModels(ctx context.Context, providerID string, pageSize int, pageToken string) (provider.ModelPage, error) {
	if err := ctx.Err(); err != nil {
		return provider.ModelPage{}, err
	}
	client := l.client
	var release func()
	if route, explicit := llm.ResolvedProviderRouteFrom(ctx); explicit {
		trusted, trustedOK := llm.TrustedProviderRouteFrom(ctx)
		if l.routeClient == nil || route.Provider != providerID || !trustedOK || trusted.RuntimeID == "" || trusted.EffectiveAgentID == "" || trusted.TaskID == "" ||
			trusted.Route.RouteID != route.RouteID || trusted.Route.RouteGeneration != route.RouteGeneration ||
			trusted.Route.ProviderConnectionID != route.ProviderConnectionID || trusted.Route.ProviderConnectionGeneration != route.ProviderConnectionGeneration ||
			trusted.Route.CredentialAssetGeneration != route.CredentialAssetGeneration || trusted.Route.ModelSelector != route.ModelSelector ||
			!route.ExpiresAt.After(time.Now()) || validateCuratedRouteEndpoint(bfschemas.ModelProvider(route.Provider), route.Endpoint) != nil {
			return provider.ModelPage{}, llm.ErrProviderRouteInvalid
		}
		client = l.routeClient
		if route.Endpoint != nil && route.Endpoint.Kind == llm.ProviderEndpointOpenAICompatible {
			if l.routePool == nil {
				return provider.ModelPage{}, llm.ErrProviderRouteInvalid
			}
			id, identityOK := identity.From(ctx)
			if !identityOK || id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
				return provider.ModelPage{}, llm.ErrProviderRouteInvalid
			}
			var err error
			client, release, err = l.routePool.acquire(ctx, routeClientPoolKey{
				TenantID: id.TenantID, RuntimeID: trusted.RuntimeID, RouteID: route.RouteID,
				RouteGeneration: route.RouteGeneration, ProviderConnectionID: route.ProviderConnectionID,
				ProviderConnectionGeneration: route.ProviderConnectionGeneration,
				CredentialAssetGeneration:    route.CredentialAssetGeneration,
				Provider:                     route.Provider, EndpointDigest: route.Endpoint.Digest,
			}, route.Endpoint.Value)
			if err != nil {
				return provider.ModelPage{}, err
			}
			defer release()
		}
	}
	if client == nil {
		return provider.ModelPage{}, provider.NewProviderError("provider_unavailable", 0, false)
	}
	request := &bfschemas.BifrostListModelsRequest{
		Provider:   bfschemas.ModelProvider(providerID),
		PageSize:   pageSize,
		PageToken:  pageToken,
		Unfiltered: true,
	}
	response, bifrostErr := client.ListModelsRequest(bfschemas.NewBifrostContext(ctx, bfschemas.NoDeadline), request)
	if bifrostErr != nil {
		return provider.ModelPage{}, classifyBifrostError(bifrostErr)
	}
	if response == nil {
		return provider.ModelPage{}, provider.NewProviderError("provider_empty_response", 0, false)
	}
	page := provider.ModelPage{
		NextPageToken: response.NextPageToken,
		Models:        make([]provider.RawModel, 0, len(response.Data)),
	}
	for _, keyStatus := range response.KeyStatuses {
		if strings.ToLower(string(keyStatus.Status)) != "success" {
			page.KeyFailures++
		}
	}
	for _, model := range response.Data {
		page.Models = append(page.Models, mapBifrostModel(model))
	}
	return page, nil
}

func mapBifrostModel(model bfschemas.Model) provider.RawModel {
	var inputModalities, outputModalities []string
	if model.Architecture != nil {
		inputModalities = append(inputModalities, model.Architecture.InputModalities...)
		outputModalities = append(outputModalities, model.Architecture.OutputModalities...)
	}
	return provider.RawModel{
		ID:                  model.ID,
		ContextLength:       model.ContextLength,
		MaxInputTokens:      model.MaxInputTokens,
		MaxOutputTokens:     model.MaxOutputTokens,
		InputModalities:     inputModalities,
		OutputModalities:    outputModalities,
		SupportedParameters: append([]string(nil), model.SupportedParameters...),
		PricingKnown:        pricingKnown(model.Pricing),
		Deprecated:          model.IsDeprecated,
	}
}

func pricingKnown(pricing *bfschemas.Pricing) bool {
	if pricing == nil {
		return false
	}
	for _, value := range []*string{
		pricing.Prompt, pricing.Completion, pricing.Request, pricing.Image,
		pricing.WebSearch, pricing.InternalReasoning, pricing.InputCacheRead,
		pricing.InputCacheWrite,
	} {
		if value != nil && strings.TrimSpace(*value) != "" {
			return true
		}
	}
	return false
}

func classifyBifrostError(err *bfschemas.BifrostError) error {
	if err == nil {
		return provider.NewProviderError("provider_unavailable", 0, false)
	}
	status := 0
	if err.StatusCode != nil {
		status = *err.StatusCode
	}
	code := "provider_unavailable"
	unsupported := false
	if err.Type != nil {
		switch strings.ToLower(strings.TrimSpace(*err.Type)) {
		case "unsupported", "unsupported_operation", "not_supported":
			code, unsupported = "model_discovery_unsupported", true
		case "provider_blocked":
			code = "provider_blocked"
		case "provider_not_found":
			code = "provider_endpoint_unavailable"
		}
	}
	if status == 401 || status == 403 {
		code = "provider_credential_rejected"
	}
	return provider.NewProviderError(code, status, unsupported)
}
