package agentcfg

import (
	"fmt"
	"strings"
)

// cloneSignedOAuthMCPPair returns a deep canonical copy of immutable pair
// state. Scope and tool policy membership are sets, so ordering cannot create a
// second authority-bearing revision for the same capability.
func cloneSignedOAuthMCPPair(in *SignedOAuthMCPPair) *SignedOAuthMCPPair {
	if in == nil {
		return nil
	}
	out := *in
	out.Scopes = sortDedup(in.Scopes)
	out.Connection.ToolAllowlist = sortDedup(in.Connection.ToolAllowlist)
	out.Connection.ToolDenylist = sortDedup(in.Connection.ToolDenylist)
	out.Connection.Injection = in.Connection.Injection.Clone()
	out.Connection.ArtifactParams = cloneSignedOAuthArtifactParams(in.Connection.ArtifactParams)
	return &out
}

func cloneSignedOAuthMCPPairs(in *SignedOAuthMCPPairs) *SignedOAuthMCPPairs {
	if in == nil || len(*in) == 0 {
		return nil
	}
	out := make(SignedOAuthMCPPairs, len(*in))
	for provider, pair := range *in {
		out[provider] = *cloneSignedOAuthMCPPair(&pair)
	}
	return &out
}

func cloneSignedOAuthArtifactParams(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for tool, params := range in {
		out[tool] = sortDedup(params)
	}
	return out
}

// SignedOAuthMCPPairView returns a defensive copy of the immutable pair.
func (p ConfigPayload) SignedOAuthMCPPairView() (*SignedOAuthMCPPair, bool) {
	if p.SignedOAuthMCPPair == nil {
		return nil, false
	}
	return cloneSignedOAuthMCPPair(p.SignedOAuthMCPPair), true
}

// SignedOAuthMCPPairsView returns a defensive copy of the canonical map slot.
// It does not include the legacy singular pair; use EffectiveSignedOAuthMCPPairs
// when runtime behavior must consume both generations.
func (p ConfigPayload) SignedOAuthMCPPairsView() *SignedOAuthMCPPairs {
	return cloneSignedOAuthMCPPairs(p.SignedOAuthMCPPairs)
}

// EffectiveSignedOAuthMCPPairs returns the strict union of the legacy singular
// slot and the canonical provider-keyed map. A map key mismatch or a provider
// present in both slots is corrupt/ambiguous desired state and fails loud.
func (p ConfigPayload) EffectiveSignedOAuthMCPPairs() (map[string]*SignedOAuthMCPPair, error) {
	size := 1
	if p.SignedOAuthMCPPairs != nil {
		size += len(*p.SignedOAuthMCPPairs)
	}
	out := make(map[string]*SignedOAuthMCPPair, size)
	if p.SignedOAuthMCPPair != nil {
		provider := strings.TrimSpace(p.SignedOAuthMCPPair.ProviderName)
		if provider == "" {
			return nil, fmt.Errorf("%w: legacy signed capability provider_name is empty", ErrInvalidConfig)
		}
		out[provider] = cloneSignedOAuthMCPPair(p.SignedOAuthMCPPair)
	}
	if p.SignedOAuthMCPPairs != nil {
		for key, pair := range *p.SignedOAuthMCPPairs {
			provider := strings.TrimSpace(pair.ProviderName)
			if key == "" || key != provider {
				return nil, fmt.Errorf("%w: signed capability map key %q does not match provider_name %q", ErrInvalidConfig, key, pair.ProviderName)
			}
			if _, duplicate := out[key]; duplicate {
				return nil, fmt.Errorf("%w: signed capability provider %q exists in both legacy and multi-pair state", ErrInvalidConfig, key)
			}
			out[key] = cloneSignedOAuthMCPPair(&pair)
		}
	}
	return out, nil
}

// SignedOAuthMCPPairByProvider resolves one immutable pair from the strict
// effective union.
func (p ConfigPayload) SignedOAuthMCPPairByProvider(provider string) (*SignedOAuthMCPPair, bool, error) {
	pairs, err := p.EffectiveSignedOAuthMCPPairs()
	if err != nil {
		return nil, false, err
	}
	pair, ok := pairs[provider]
	return pair, ok, nil
}

// RemoveSignedOAuthMCPPair removes exactly one provider while preserving the
// storage generation of every sibling. It is used only by the fenced signed
// capability lifecycle.
func (p ConfigPayload) RemoveSignedOAuthMCPPair(provider string) (ConfigPayload, bool, error) {
	if _, _, err := p.SignedOAuthMCPPairByProvider(provider); err != nil {
		return ConfigPayload{}, false, err
	}
	out := NormalizePayload(p)
	removed := false
	if out.SignedOAuthMCPPair != nil && out.SignedOAuthMCPPair.ProviderName == provider {
		out.SignedOAuthMCPPair = nil
		removed = true
	}
	if out.SignedOAuthMCPPairs != nil {
		if _, ok := (*out.SignedOAuthMCPPairs)[provider]; ok {
			delete(*out.SignedOAuthMCPPairs, provider)
			removed = true
		}
		if len(*out.SignedOAuthMCPPairs) == 0 {
			out.SignedOAuthMCPPairs = nil
		}
	}
	return out, removed, nil
}
