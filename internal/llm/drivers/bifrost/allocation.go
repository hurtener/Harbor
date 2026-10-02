package bifrost

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/pricing"
)

// ProviderAttemptBound covers configured network retries plus the pinned
// transport's single encrypted-reasoning fail-soft retry. No fallback list is
// emitted by Harbor's request translator. Unreported intermediate usage remains
// held even when the final attempt returns measured usage.
func (d *Driver) ProviderAttemptBound(ctx context.Context, req llm.CompleteRequest) (int, error) {
	if _, routed := llm.TrustedProviderRouteFrom(ctx); routed {
		return 2, nil
	}
	if d.account == nil {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	cfg, err := d.account.GetConfigForProvider(d.provider)
	if err != nil {
		return 0, fmt.Errorf("allocation provider bounds: %w", err)
	}
	if cfg.NetworkConfig.MaxRetries < 0 || cfg.NetworkConfig.MaxRetries > 998 {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	return cfg.NetworkConfig.MaxRetries + 2, nil
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
