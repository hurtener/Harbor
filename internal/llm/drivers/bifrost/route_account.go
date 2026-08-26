package bifrost

import (
	"context"
	"fmt"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// curatedRouteProviders is the finite Bifrost v1.7.4 chat-capable route set.
// It deliberately excludes non-chat providers (ElevenLabs, Runway, Runware)
// and credential shapes this narrow API-key/typed-endpoint contract cannot
// represent safely (Vertex, Bedrock, Bedrock Mantle).
var curatedRouteProviders = []bfschemas.ModelProvider{
	bfschemas.Anthropic,
	bfschemas.Azure,
	bfschemas.Cerebras,
	bfschemas.Cohere,
	bfschemas.DeepSeek,
	bfschemas.Gemini,
	bfschemas.Groq,
	bfschemas.Mistral,
	bfschemas.Ollama,
	bfschemas.OpencodeGo,
	bfschemas.OpencodeZen,
	bfschemas.OpenAI,
	bfschemas.Parasail,
	bfschemas.Perplexity,
	bfschemas.SGL,
	bfschemas.OpenRouter,
	bfschemas.HuggingFace,
	bfschemas.Nebius,
	bfschemas.XAI,
	bfschemas.Replicate,
	bfschemas.VLLM,
	bfschemas.Fireworks,
	bfschemas.Sarvam,
	bfschemas.Wafer,
}

type routeAccount struct {
	providers []bfschemas.ModelProvider
	configs   map[bfschemas.ModelProvider]*bfschemas.ProviderConfig
}

func newRouteAccount(network llm.NetworkDefaults) (*routeAccount, error) {
	providers := append([]bfschemas.ModelProvider(nil), curatedRouteProviders...)
	configs := make(map[bfschemas.ModelProvider]*bfschemas.ProviderConfig, len(providers))
	for _, provider := range providers {
		if !isKnownProvider(provider) {
			return nil, fmt.Errorf("%w: curated provider %q is unavailable in pinned Bifrost", ErrInvalidProvider, provider)
		}
		config := buildNativeProviderConfig(llm.ConfigSnapshot{NetworkDefaults: network})
		// Routed calls must leave Bifrost after exactly one provider attempt.
		// Harbor's outer retry wrapper then re-enters route resolution, observing
		// credential rotation/revocation before the next attempt.
		config.NetworkConfig.MaxRetries = 0
		configs[provider] = config
	}
	return &routeAccount{providers: providers, configs: configs}, nil
}

func newOpenAICompatibleRouteAccount(network llm.NetworkDefaults, endpoint string) *routeAccount {
	config := buildNativeProviderConfig(llm.ConfigSnapshot{NetworkDefaults: network, BaseURL: endpoint})
	config.NetworkConfig.BaseURL = endpoint
	config.NetworkConfig.MaxRetries = 0
	return &routeAccount{
		providers: []bfschemas.ModelProvider{bfschemas.OpenAI},
		configs:   map[bfschemas.ModelProvider]*bfschemas.ProviderConfig{bfschemas.OpenAI: config},
	}
}

func (a *routeAccount) GetConfiguredProviders() ([]bfschemas.ModelProvider, error) {
	return append([]bfschemas.ModelProvider(nil), a.providers...), nil
}

func (a *routeAccount) GetConfigForProvider(provider bfschemas.ModelProvider) (*bfschemas.ProviderConfig, error) {
	config, ok := a.configs[provider]
	if !ok {
		return nil, fmt.Errorf("%w: provider %q is outside the curated route set", llm.ErrProviderRouteInvalid, provider)
	}
	return config, nil
}

func (a *routeAccount) GetKeysForProvider(ctx context.Context, provider bfschemas.ModelProvider) ([]bfschemas.Key, error) {
	route, ok := llm.ResolvedProviderRouteFrom(ctx)
	if !ok || route.Provider != string(provider) || route.Credential == "" || !route.ExpiresAt.After(time.Now()) {
		return nil, llm.ErrProviderRouteInvalid
	}
	if _, configured := a.configs[provider]; !configured {
		return nil, llm.ErrProviderRouteInvalid
	}
	if err := validateCuratedRouteEndpoint(provider, route.Endpoint); err != nil {
		return nil, err
	}
	key := bfschemas.Key{
		ID:     routeKeyID(route),
		Name:   route.KeyName,
		Value:  bfschemas.SecretVar{Val: route.Credential},
		Models: []string{route.Model},
		Weight: 1,
	}
	if err := applyTypedRouteEndpoint(&key, provider, route); err != nil {
		return nil, err
	}
	return []bfschemas.Key{key}, nil
}

func routeKeyID(route llm.ResolvedProviderRoute) string {
	return fmt.Sprintf("harbor-route-%s-%d-%d", route.RouteID, route.ProviderConnectionGeneration, route.CredentialAssetGeneration)
}

func applyTypedRouteEndpoint(key *bfschemas.Key, provider bfschemas.ModelProvider, route llm.ResolvedProviderRoute) error {
	endpoint := route.Endpoint
	if err := validateCuratedRouteEndpoint(provider, endpoint); err != nil {
		return err
	}
	switch provider {
	case bfschemas.Azure:
		if endpoint == nil || endpoint.Kind != llm.ProviderEndpointAzure {
			return llm.ErrProviderRouteInvalid
		}
		key.AzureKeyConfig = &bfschemas.AzureKeyConfig{Endpoint: bfschemas.SecretVar{Val: endpoint.Value}}
	case bfschemas.VLLM:
		if endpoint == nil || endpoint.Kind != llm.ProviderEndpointVLLM {
			return llm.ErrProviderRouteInvalid
		}
		key.VLLMKeyConfig = &bfschemas.VLLMKeyConfig{URL: bfschemas.SecretVar{Val: endpoint.Value}, ModelName: route.Model}
	case bfschemas.Ollama:
		if endpoint == nil || endpoint.Kind != llm.ProviderEndpointOllama {
			return llm.ErrProviderRouteInvalid
		}
		key.OllamaKeyConfig = &bfschemas.OllamaKeyConfig{URL: bfschemas.SecretVar{Val: endpoint.Value}}
	case bfschemas.SGL:
		if endpoint == nil || endpoint.Kind != llm.ProviderEndpointSGL {
			return llm.ErrProviderRouteInvalid
		}
		key.SGLKeyConfig = &bfschemas.SGLKeyConfig{URL: bfschemas.SecretVar{Val: endpoint.Value}}
	case bfschemas.OpenAI:
		if endpoint != nil && endpoint.Kind != llm.ProviderEndpointOpenAICompatible {
			return llm.ErrProviderRouteInvalid
		}
	default:
		if endpoint != nil {
			return llm.ErrProviderRouteInvalid
		}
	}
	return nil
}

func validateCuratedRouteEndpoint(provider bfschemas.ModelProvider, endpoint *llm.ProviderEndpointBinding) error {
	if endpoint != nil {
		normalized, digest, err := llm.NormalizeProviderEndpoint(endpoint.Value)
		if err != nil || normalized != endpoint.Value || digest != endpoint.Digest {
			return llm.ErrProviderRouteInvalid
		}
	}
	switch provider {
	case bfschemas.Azure:
		if endpoint == nil || endpoint.Kind != llm.ProviderEndpointAzure {
			return llm.ErrProviderRouteInvalid
		}
	case bfschemas.VLLM:
		if endpoint == nil || endpoint.Kind != llm.ProviderEndpointVLLM {
			return llm.ErrProviderRouteInvalid
		}
	case bfschemas.Ollama:
		if endpoint == nil || endpoint.Kind != llm.ProviderEndpointOllama {
			return llm.ErrProviderRouteInvalid
		}
	case bfschemas.SGL:
		if endpoint == nil || endpoint.Kind != llm.ProviderEndpointSGL {
			return llm.ErrProviderRouteInvalid
		}
	case bfschemas.OpenAI:
		if endpoint != nil && endpoint.Kind != llm.ProviderEndpointOpenAICompatible {
			return llm.ErrProviderRouteInvalid
		}
	default:
		if endpoint != nil {
			return llm.ErrProviderRouteInvalid
		}
	}
	return nil
}

func curatedRouteProvider(provider bfschemas.ModelProvider) bool {
	for _, candidate := range curatedRouteProviders {
		if candidate == provider {
			return true
		}
	}
	return false
}
