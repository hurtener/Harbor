package bifrost

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	bf "github.com/maximhq/bifrost/core"
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/provider"
)

// ProviderCatalog is the runtime-owned adapter for Harbor's provider-neutral
// descriptor and model discovery contract. It uses the same Account and
// Bifrost setup as ordinary LLM execution; it never exposes the resolved key
// or a provider response body.
type ProviderCatalog struct {
	catalog *provider.Catalog
	client  *bf.Bifrost
	mu      sync.RWMutex
	closed  bool
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
	account, err := newAccount(cfg, deps)
	if err != nil {
		return nil, err
	}
	client, err := bf.Init(context.Background(), bfschemas.BifrostConfig{Account: account})
	if err != nil {
		return nil, fmt.Errorf("bifrost provider catalog: init: %w", err)
	}
	descriptors := StaticProviderDescriptors(cfg.CustomProviders)
	active := []string{cfg.Provider}
	manual := make(map[string][]string)
	for _, custom := range cfg.CustomProviders {
		manual[custom.Name] = append([]string(nil), custom.Models...)
	}
	catalog, err := provider.NewCatalog(&bifrostModelLister{client: client}, descriptors, active, manual)
	if err != nil {
		client.Shutdown()
		return nil, fmt.Errorf("bifrost provider catalog: descriptors: %w", err)
	}
	return &ProviderCatalog{catalog: catalog, client: client}, nil
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
	return c.catalog.Validate(ctx, req)
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
	return c.catalog.Discover(ctx, req)
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
	c.client.Shutdown()
	return nil
}

type bifrostModelLister struct {
	client *bf.Bifrost
}

func (l *bifrostModelLister) ListModels(ctx context.Context, providerID string, pageSize int, pageToken string) (provider.ModelPage, error) {
	if err := ctx.Err(); err != nil {
		return provider.ModelPage{}, err
	}
	request := &bfschemas.BifrostListModelsRequest{
		Provider:   bfschemas.ModelProvider(providerID),
		PageSize:   pageSize,
		PageToken:  pageToken,
		Unfiltered: true,
	}
	response, bifrostErr := l.client.ListModelsRequest(bfschemas.NewBifrostContext(ctx, bfschemas.NoDeadline), request)
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
