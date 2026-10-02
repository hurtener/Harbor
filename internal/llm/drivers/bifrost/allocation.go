package bifrost

import (
	"context"
	"fmt"

	bfschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/pricing"
)

// ProviderAttemptBound multiplies the two nested retry limits: the SDK's
// configured retries plus one encrypted-reasoning repair, and fasthttp's complete
// physical send cap. A response-header failure may follow a fully consumed POST.
// Neither the SDK's logical attempt trail nor a successful final response proves
// an earlier physical attempt free; settlement retains unreported liability.
func (d *Driver) ProviderAttemptBound(ctx context.Context, req llm.CompleteRequest) (int, error) {
	if !allocationTextRequest(req) {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	if _, routed := llm.TrustedProviderRouteFrom(ctx); routed {
		selected, ok := llm.SelectedProviderRouteFrom(ctx)
		if !ok || selected.Model != req.Model || !allocationBoundedProvider(bfschemas.ModelProvider(selected.Provider)) || d.ValidateProviderRouteSelection(selected) != nil {
			return 0, llm.ErrAllocationBoundUnavailable
		}
		// Both routeAccount factories force SDK MaxRetries=0. The leaf resolves
		// current credentials and exact-matches the provider/model/endpoint back
		// to this pre-policy selection before entering the selected factory.
		return bifrostPhysicalAttemptBound(0)
	}
	if d.account == nil {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	cfg, err := d.account.GetConfigForProvider(d.provider)
	if err != nil {
		return 0, fmt.Errorf("allocation provider bounds: %w", err)
	}
	if cfg == nil {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	provider := d.provider
	if cfg.CustomProviderConfig != nil {
		// Harbor's custom provider factory supports only this exact base.
		// A provider's arbitrary name is not evidence of its transport family.
		if cfg.CustomProviderConfig.BaseProviderType != bfschemas.OpenAI {
			return 0, llm.ErrAllocationBoundUnavailable
		}
		provider = bfschemas.OpenAI
	}
	if !allocationBoundedProvider(provider) {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	return bifrostPhysicalAttemptBound(cfg.NetworkConfig.MaxRetries)
}

func allocationBoundedProvider(provider bfschemas.ModelProvider) bool {
	return provider == bfschemas.OpenAI || provider == bfschemas.Anthropic
}

func bifrostPhysicalAttemptBound(retries int) (int, error) {
	// Pinned Bifrost v1.9.0 OpenAI/Anthropic constructors leave the physical
	// cap at zero (fasthttp default); streaming/large-response clones preserve
	// it. Their text Chat paths call Do once, without redirect following. The
	// stale-socket callback currently allows fewer sends, but the outer cap is
	// the conservative bound. Re-audit the factories when either pin changes.
	const physical = fasthttp.DefaultMaxIdemponentCallAttempts
	if retries < 0 || retries > 1000/physical-2 {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	// The SDK guards its extra encrypted-content retry so it runs at most
	// once. translateRequest emits no fallback list or raw body; opaque Extra
	// and auxiliary file/media work are refused before this declaration.
	return (retries + 2) * physical, nil
}

func allocationTextRequest(req llm.CompleteRequest) bool {
	if len(req.Extra) != 0 {
		return false
	}
	for _, msg := range req.Messages {
		for _, part := range msg.Content.Parts {
			if part.Type != llm.PartText || part.Image != nil || part.Audio != nil || part.File != nil {
				return false
			}
		}
	}
	return true
}

// MonetaryTarget supports bounded text ChatCompletion requests. Provider-native
// file operations and multimodal input can introduce separate physical requests
// or non-token charges and are deliberately refused until they have a bound.
func (d *Driver) MonetaryTarget(ctx context.Context, req llm.CompleteRequest, profile llm.ModelProfile) (llm.MonetaryTarget, error) {
	provider := string(d.provider)
	if _, routed := llm.TrustedProviderRouteFrom(ctx); routed {
		// Credential/connection/endpoint generations need their own pricing
		// authority binding. Do not price an externally selected gateway by
		// the default provider's name alone.
		return llm.MonetaryTarget{}, llm.ErrAllocationPricingUnavailable
	}
	if d.account == nil {
		return llm.MonetaryTarget{}, llm.ErrAllocationPricingUnavailable
	}
	cfg, err := d.account.GetConfigForProvider(d.provider)
	if err != nil || cfg == nil || cfg.CustomProviderConfig != nil {
		return llm.MonetaryTarget{}, llm.ErrAllocationPricingUnavailable
	}
	endpoint := pricing.EndpointBinding(cfg.NetworkConfig.BaseURL)
	// These converters enforce the explicit completion cap. Other provider
	// converters must establish their own complete bound before opting in.
	if provider != "openai" && provider != "anthropic" {
		return llm.MonetaryTarget{}, llm.ErrAllocationPricingUnavailable
	}
	if req.MaxTokens == nil || *req.MaxTokens <= 0 || *req.MaxTokens > 1<<30 || profile.ContextWindowTokens <= 0 || profile.ContextWindowTokens > 1<<30 || len(req.Extra) != 0 {
		return llm.MonetaryTarget{}, llm.ErrAllocationPricingUnavailable
	}
	for _, msg := range req.Messages {
		for _, part := range msg.Content.Parts {
			if part.Type != llm.PartText || part.Image != nil || part.Audio != nil || part.File != nil {
				return llm.MonetaryTarget{}, llm.ErrAllocationPricingUnavailable
			}
		}
	}
	output := int64(*req.MaxTokens)
	effort, err := llm.EffectiveReasoningEffort(req)
	if err != nil {
		return llm.MonetaryTarget{}, err
	}
	if provider == "anthropic" && effort != "" && effort != llm.ReasoningOff {
		// Even when a provider includes thinking inside max_tokens, summing
		// its independently configured thinking budget remains conservative.
		output += int64(anthropicReasoningBudget(effort))
	}
	return llm.MonetaryTarget{EndpointBinding: endpoint, Provider: provider, Model: req.Model, InputTokens: int64(profile.ContextWindowTokens), OutputTokens: output}, nil
}
