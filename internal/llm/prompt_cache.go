package llm

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// Prompt cache retention values. Empty keeps the provider default.
const (
	PromptCacheRetentionInMemory = "in_memory"
	PromptCacheRetention24h      = "24h"
)

// PromptCacheSettings is the operator posture for provider prompt caching.
// The zero value is the default: a prompt cache key is sent on providers
// that accept one, retention is the provider default, and routing is not
// constrained.
type PromptCacheSettings struct {
	// Disabled stops Harbor from sending a prompt cache key (providers that
	// cache automatically still do so).
	Disabled bool
	// Retention is "" (provider default), "in_memory" or "24h". Sent only to
	// providers whose request schema carries a retention field.
	Retention string
	// StickyRouting additionally sends the cache key as the session-affinity
	// key on routers that accept one, so turns sharing a prefix reach the
	// same upstream provider from the first request.
	StickyRouting bool
	// ProviderOrder and AllowFallbacks pin a router's upstream provider
	// choice. Both are opt-in; a nil AllowFallbacks keeps the router
	// default. A manual order can disable a router's own sticky routing.
	ProviderOrder  []string
	AllowFallbacks *bool
}

// Clone returns a deep copy.
func (s PromptCacheSettings) Clone() PromptCacheSettings {
	out := s
	out.ProviderOrder = append([]string(nil), s.ProviderOrder...)
	if len(out.ProviderOrder) == 0 {
		out.ProviderOrder = nil
	}
	if s.AllowFallbacks != nil {
		v := *s.AllowFallbacks
		out.AllowFallbacks = &v
	}
	return out
}

// PromptCacheScopeInput names the stable prefix a prompt cache key covers.
// Every field is hashed; none reaches a provider in clear.
type PromptCacheScopeInput struct {
	TenantID string
	AgentID  string
	// ToolsDigest identifies the stable part of the declared tool catalog
	// (the tiers that do not change between one user's turns).
	ToolsDigest string
	// SystemDigest identifies the static system-prompt spine.
	SystemDigest string
}

// PromptCacheScope derives the opaque scope a planner attaches to
// [CompleteRequest.PromptCacheScope]. Equal inputs give equal scopes; any
// differing field gives a different scope. The scope is a digest, never a
// raw identifier.
func PromptCacheScope(in PromptCacheScopeInput) string {
	h := sha256.New()
	writeDigestField(h, "harbor:prompt-cache-scope:v1")
	writeDigestField(h, in.TenantID)
	writeDigestField(h, in.AgentID)
	writeDigestField(h, in.ToolsDigest)
	writeDigestField(h, in.SystemDigest)
	return hex.EncodeToString(h.Sum(nil))
}

// PromptCacheKey derives the provider-facing prompt cache key from a scope
// and the provider/model that will serve the call. It is empty when scope
// is empty. The key is fixed-length and opaque.
func PromptCacheKey(scope, provider, model string) string {
	if scope == "" {
		return ""
	}
	h := sha256.New()
	writeDigestField(h, "harbor:prompt-cache-key:v1")
	writeDigestField(h, scope)
	writeDigestField(h, provider)
	writeDigestField(h, model)
	return "hpc1-" + hex.EncodeToString(h.Sum(nil))[:40]
}

// DigestParts returns a length-prefixed SHA-256 digest of parts — an
// unambiguous building block for the digests a scope input carries.
func DigestParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		writeDigestField(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeDigestField(h hash.Hash, v string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(v)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(v))
}
