package bifrost

import (
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// promptCacheTarget says how the serving endpoint accepts prompt-cache
// controls.
type promptCacheTarget int

const (
	// promptCacheNone: send nothing (operator-declared OpenAI-compatible
	// endpoints, whose request schema Harbor cannot know).
	promptCacheNone promptCacheTarget = iota
	// promptCacheTyped: bifrost's typed prompt_cache_key / retention
	// fields. Bifrost's per-model capability filter strips them for a
	// provider or model that does not accept them.
	promptCacheTyped
	// promptCacheRouter: an aggregator whose bifrost adapter strips the
	// typed fields. Its documented body fields travel as passthrough
	// extra params: prompt_cache_key (cache and fallback session-affinity
	// key), session_id (opt-in affinity from the first request) and
	// provider{order, allow_fallbacks} (opt-in upstream pinning).
	promptCacheRouter
)

func promptCacheTargetFor(provider bfschemas.ModelProvider, custom, compatibleEndpoint bool) promptCacheTarget {
	switch {
	case custom || compatibleEndpoint:
		return promptCacheNone
	case provider == bfschemas.OpenRouter:
		return promptCacheRouter
	default:
		return promptCacheTyped
	}
}

// applyPromptCache stamps the prompt-cache controls on a translated request.
// It returns true when router passthrough params were added, in which case
// the caller enables bifrost's extra-param passthrough on the call context.
// Every value it sends is derived from the opaque scope or operator config;
// none is a raw identity.
func applyPromptCache(bfReq *bfschemas.BifrostChatRequest, scope string, settings llm.PromptCacheSettings, target promptCacheTarget) bool {
	if bfReq == nil || target == promptCacheNone {
		return false
	}
	key := ""
	if !settings.Disabled {
		key = llm.PromptCacheKey(scope, string(bfReq.Provider), bfReq.Model)
	}
	switch target {
	case promptCacheTyped:
		if key == "" {
			return false
		}
		params := ensureParams(bfReq)
		params.PromptCacheKey = &key
		if settings.Retention != "" {
			retention := settings.Retention
			params.PromptCacheRetention = &retention
		}
		return false
	case promptCacheRouter:
		extra := map[string]interface{}{}
		if key != "" {
			extra["prompt_cache_key"] = key
			if settings.StickyRouting {
				extra["session_id"] = key
			}
		}
		if len(settings.ProviderOrder) > 0 {
			routing := map[string]interface{}{"order": append([]string(nil), settings.ProviderOrder...)}
			if settings.AllowFallbacks != nil {
				routing["allow_fallbacks"] = *settings.AllowFallbacks
			}
			extra["provider"] = routing
		}
		if len(extra) == 0 {
			return false
		}
		params := ensureParams(bfReq)
		if params.ExtraParams == nil {
			params.ExtraParams = make(map[string]interface{}, len(extra))
		}
		for k, v := range extra {
			params.ExtraParams[k] = v
		}
		return true
	}
	return false
}

func ensureParams(bfReq *bfschemas.BifrostChatRequest) *bfschemas.ChatParameters {
	if bfReq.Params == nil {
		bfReq.Params = &bfschemas.ChatParameters{}
	}
	return bfReq.Params
}
