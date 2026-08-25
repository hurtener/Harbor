// Package grant implements Harbor's reference external-execution grant
// verifier and opaque credential-binding store. It is deliberately separate
// from the LLM interface so callers can provide another coordinator-backed
// verifier without changing the provider drivers.
package grant

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tools"
)

var (
	// ErrInvalidGrantShape identifies missing or malformed signed claims.
	ErrInvalidGrantShape = errors.New("grant: invalid grant shape")
	// ErrUnknownKey identifies an untrusted signing key id.
	ErrUnknownKey = errors.New("grant: unknown signing key")
	// ErrBindingNotFound identifies an opaque handle that is not registered.
	ErrBindingNotFound = errors.New("grant: credential binding not found")
)

// Signer signs grants with one coordinator-owned Ed25519 key. Private key
// bytes never enter the grant envelope.
type Signer struct {
	keyID    string
	audience string
	private  ed25519.PrivateKey
	clock    func() time.Time
}

// NewSigner constructs a signer. A nil key generates a fresh test/development
// key; production callers should load the coordinator key from their secure
// key source rather than a grant or provider configuration.
func NewSigner(keyID, audience string, private ed25519.PrivateKey, clock func() time.Time) (*Signer, error) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(audience) == "" {
		return nil, fmt.Errorf("%w: key_id and audience are required", ErrInvalidGrantShape)
	}
	if private == nil {
		_, generated, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("grant: generate signing key: %w", err)
		}
		private = generated
	}
	if len(private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: private key length=%d", ErrInvalidGrantShape, len(private))
	}
	if clock == nil {
		clock = time.Now
	}
	return &Signer{keyID: keyID, audience: audience, private: append(ed25519.PrivateKey(nil), private...), clock: clock}, nil
}

// PublicKey returns a copy of the signer's public key for a verifier.
func (s *Signer) PublicKey() ed25519.PublicKey {
	public, ok := s.private.Public().(ed25519.PublicKey)
	if !ok {
		return nil
	}
	return append(ed25519.PublicKey(nil), public...)
}

// Sign creates a signed grant with coordinator-owned key and audience. The
// caller supplies the context-bound claims; the signer stamps the key id and
// issued time and refuses incomplete claims.
func (s *Signer) Sign(claims llm.ExternalGrant) (llm.ExternalGrant, error) {
	claims.KeyID = s.keyID
	claims.Audience = s.audience
	if claims.RouteMode == "" {
		claims.RouteMode = llm.ExternalGrantRouteCoordinatorBound
	}
	if claims.IssuedAt.IsZero() {
		claims.IssuedAt = s.clock().UTC()
	}
	// The coordinator owns logical-call identity. Older callers that did not
	// populate the additive fields are upgraded at this signing boundary from
	// stable grant/run claims; no dispatch caller can choose a different value
	// after the signature is produced.
	if claims.LogicalCallID == "" {
		claims.LogicalCallID = derivedIdentity("call", claims.GrantID, claims.LogicalRunID)
	}
	if claims.AttemptNonce == "" {
		claims.AttemptNonce = derivedIdentity("nonce", claims.GrantID, claims.LogicalRunID)
	}
	if err := validateClaims(claims, false); err != nil {
		return llm.ExternalGrant{}, err
	}
	doc, err := canonicalDocument(claims)
	if err != nil {
		return llm.ExternalGrant{}, err
	}
	claims.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.private, doc))
	return claims, nil
}

// Verifier validates signed grants and binds them to the verified request
// context. The key set and expectations are immutable after construction.
type Verifier struct {
	keys                    map[string]ed25519.PublicKey
	audience                string
	runtimeID               string
	authorizedOrganizations map[string]struct{}
	clock                   func() time.Time
	clockSkew               time.Duration
	routeMode               llm.ExternalGrantRouteMode
}

// VerifierConfig constructs a verifier from a copied key set.
type VerifierConfig struct {
	Audience                string
	RuntimeID               string
	AuthorizedOrganizations []string
	Keys                    map[string]ed25519.PublicKey
	Clock                   func() time.Time
	ClockSkew               time.Duration
	// RouteMode optionally restricts this runtime to one signed route shape.
	// Empty accepts both explicit modes so mixed fleets can migrate without
	// weakening per-grant validation.
	RouteMode llm.ExternalGrantRouteMode
}

// NewVerifier constructs a fail-closed verifier. Public keys are copied so a
// caller cannot mutate trust after construction.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if strings.TrimSpace(cfg.Audience) == "" || strings.TrimSpace(cfg.RuntimeID) == "" || len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("%w: audience, runtime_id, and at least one key are required", ErrInvalidGrantShape)
	}
	if cfg.RouteMode != "" && cfg.RouteMode != llm.ExternalGrantRouteRuntimeDefault && cfg.RouteMode != llm.ExternalGrantRouteCoordinatorBound {
		return nil, fmt.Errorf("%w: unsupported route mode %q", ErrInvalidGrantShape, cfg.RouteMode)
	}
	keys := make(map[string]ed25519.PublicKey, len(cfg.Keys))
	for id, key := range cfg.Keys {
		if strings.TrimSpace(id) == "" || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: key %q has invalid length", ErrInvalidGrantShape, id)
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	skew := cfg.ClockSkew
	if skew <= 0 {
		skew = 30 * time.Second
	}
	organizations := make(map[string]struct{}, len(cfg.AuthorizedOrganizations))
	for _, organization := range cfg.AuthorizedOrganizations {
		organization = strings.TrimSpace(organization)
		if organization == "" {
			return nil, fmt.Errorf("%w: authorized organization ids must not be empty", ErrInvalidGrantShape)
		}
		organizations[organization] = struct{}{}
	}
	return &Verifier{keys: keys, audience: cfg.Audience, runtimeID: cfg.RuntimeID, authorizedOrganizations: organizations, clock: clock, clockSkew: skew, routeMode: cfg.RouteMode}, nil
}

// Verify implements llm.ExternalGrantVerifier. It requires the request-edge
// verified identity, not merely a mutable working identity, and checks the
// exact model/lease/route claims before the driver is reached.
func (v *Verifier) Verify(ctx context.Context, grant llm.ExternalGrant, req llm.CompleteRequest) error {
	if err := validateClaims(grant, true); err != nil {
		return err
	}
	grantMode := normalizedRouteMode(grant.RouteMode)
	if v.routeMode != "" && grantMode != v.routeMode {
		return fmt.Errorf("%w: route mode is not enabled for this runtime", llm.ErrExternalGrantInvalid)
	}
	if grant.Audience != v.audience || grant.RuntimeID != v.runtimeID {
		return fmt.Errorf("%w: audience or runtime mismatch", llm.ErrExternalGrantInvalid)
	}
	key, ok := v.keys[grant.KeyID]
	if !ok {
		return fmt.Errorf("%w: %w", llm.ErrExternalGrantSignature, ErrUnknownKey)
	}
	doc, err := canonicalDocument(grant)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(grant.Signature)
	if err != nil || !ed25519.Verify(key, doc, sig) {
		return fmt.Errorf("%w: key_id=%q", llm.ErrExternalGrantSignature, grant.KeyID)
	}
	now := v.clock().UTC()
	if now.Before(grant.IssuedAt.Add(-v.clockSkew)) || !now.Before(grant.ExpiresAt) {
		return fmt.Errorf("%w: grant expired or issued in the future", llm.ErrExternalGrantInvalid)
	}
	verified, ok := identity.FromVerified(ctx)
	if !ok {
		return fmt.Errorf("%w: request has no verified identity", llm.ErrExternalGrantInvalid)
	}
	working, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("%w: request has no working identity", llm.ErrExternalGrantInvalid)
	}
	quad, ok := identity.QuadrupleFrom(ctx)
	if !ok || quad.RunID == "" {
		return fmt.Errorf("%w: request has no logical run id", llm.ErrExternalGrantInvalid)
	}
	if organizationID, supplied := llm.VerifiedOrganizationFrom(ctx); supplied && organizationID != grant.OrganizationID {
		return fmt.Errorf("%w: request has no matching verified organization", llm.ErrExternalGrantInvalid)
	}
	if len(v.authorizedOrganizations) > 0 {
		if _, allowed := v.authorizedOrganizations[grant.OrganizationID]; !allowed {
			return fmt.Errorf("%w: signed organization is not authorized for this runtime", llm.ErrExternalGrantInvalid)
		}
	}
	if verified.TenantID != grant.TenantID || working != (identity.Identity{TenantID: grant.TenantID, UserID: grant.UserID, SessionID: grant.SessionID}) ||
		quad.RunID != grant.LogicalRunID || grant.OrganizationID == "" {
		return fmt.Errorf("%w: grant identity/run binding mismatch", llm.ErrExternalGrantInvalid)
	}
	if grant.Version == llm.ExternalGrantVersionAgentBound {
		effectiveAgentID, admitted := tools.EffectiveAgentConfigFrom(ctx)
		if !admitted || effectiveAgentID != grant.AgentID {
			return fmt.Errorf("%w: grant agent binding mismatch", llm.ErrExternalGrantInvalid)
		}
	}
	if grantMode == llm.ExternalGrantRouteCoordinatorBound && req.Model != "" && grant.ProviderModelID != req.Model {
		return fmt.Errorf("%w: provider model mismatch", llm.ErrExternalGrantInvalid)
	}
	if req.MaxTokens != nil && grant.MaxOutputTokens > 0 && *req.MaxTokens > grant.MaxOutputTokens {
		return fmt.Errorf("%w: requested output exceeds grant", llm.ErrExternalGrantInvalid)
	}
	if reasoningRank(req.ReasoningEffort) > reasoningRank(grant.MaxReasoning) {
		return fmt.Errorf("%w: requested reasoning exceeds grant", llm.ErrExternalGrantInvalid)
	}
	if grant.Lease.ExpiresAt.IsZero() || !now.Before(grant.Lease.ExpiresAt) || grant.Lease.RemainingTokens() <= 0 {
		return fmt.Errorf("%w: %w", llm.ErrExternalGrantInvalid, llm.ErrExternalGrantLeaseInsufficient)
	}
	if req.MaxTokens != nil && int64(*req.MaxTokens) > grant.Lease.RemainingTokens() {
		return fmt.Errorf("%w: requested output exceeds lease: %w", llm.ErrExternalGrantLeaseInsufficient, llm.ErrExternalGrantLeaseInsufficient)
	}
	return nil
}

func reasoningRank(e llm.ReasoningEffort) int {
	switch e {
	case llm.ReasoningHigh:
		return 4
	case llm.ReasoningMedium:
		return 3
	case llm.ReasoningLow:
		return 2
	case llm.ReasoningOff, "":
		return 1
	default:
		return 99
	}
}

// Binding is an opaque coordinator-selected provider credential asset. The
// secret is held only in this store and returned only at the driver boundary.
type Binding struct {
	Handle                       string
	OrganizationID               string
	RuntimeID                    string
	Provider                     string
	ProviderConnectionID         string
	ProviderConnectionGeneration uint64
	Generation                   uint64
	Secret                       string
	Revoked                      bool
}

// BindingStore is an in-memory reference resolver used by embedded runtimes
// and tests. Production coordinators may replace it with a KMS-backed
// implementation behind llm.CredentialResolver. It never exposes a mutable
// LiveKey and generation changes fence every prior grant immediately.
type BindingStore struct {
	mu       sync.RWMutex
	bindings map[string]Binding
}

// NewBindingStore constructs an empty resolver.
func NewBindingStore() *BindingStore { return &BindingStore{bindings: make(map[string]Binding)} }

// Put installs or replaces a binding at an explicit immutable generation.
func (s *BindingStore) Put(binding Binding) error {
	if strings.TrimSpace(binding.Handle) == "" || strings.TrimSpace(binding.OrganizationID) == "" || strings.TrimSpace(binding.RuntimeID) == "" ||
		strings.TrimSpace(binding.Provider) == "" || strings.TrimSpace(binding.ProviderConnectionID) == "" || binding.ProviderConnectionGeneration == 0 || binding.Generation == 0 || binding.Secret == "" {
		return fmt.Errorf("%w: incomplete credential binding", ErrInvalidGrantShape)
	}
	s.mu.Lock()
	if previous, exists := s.bindings[binding.Handle]; exists {
		if binding.Generation < previous.Generation {
			s.mu.Unlock()
			return fmt.Errorf("%w: binding generation cannot move backwards", ErrInvalidGrantShape)
		}
		if binding.Generation == previous.Generation {
			if binding == previous {
				s.mu.Unlock()
				return nil
			}
			s.mu.Unlock()
			return fmt.Errorf("%w: binding generation is immutable", ErrInvalidGrantShape)
		}
	}
	s.bindings[binding.Handle] = binding
	s.mu.Unlock()
	return nil
}

// Rotate replaces the secret and advances the generation, fencing old grants.
func (s *BindingStore) Rotate(handle, secret string, generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[handle]
	if !ok {
		return ErrBindingNotFound
	}
	if generation <= binding.Generation || secret == "" {
		return fmt.Errorf("%w: generation must advance", ErrInvalidGrantShape)
	}
	binding.Generation, binding.Secret, binding.Revoked = generation, secret, false
	s.bindings[handle] = binding
	return nil
}

// Revoke fences the current generation without deleting audit-relevant
// binding identity.
func (s *BindingStore) Revoke(handle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[handle]
	if !ok {
		return ErrBindingNotFound
	}
	binding.Revoked = true
	s.bindings[handle] = binding
	return nil
}

// Resolve implements llm.CredentialResolver. It rechecks verified identity,
// runtime, organization, provider connection, and generation at resolution
// time so stale/cross-tenant contexts cannot use a valid handle.
func (s *BindingStore) Resolve(ctx context.Context, grant llm.ExternalGrant) (llm.ResolvedCredential, error) {
	verified, ok := identity.FromVerified(ctx)
	if !ok || verified.TenantID != grant.TenantID {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: verified tenant mismatch", llm.ErrExternalGrantRevoked)
	}
	working, ok := identity.From(ctx)
	if !ok || working.TenantID != grant.TenantID || working.UserID != grant.UserID || working.SessionID != grant.SessionID {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: working identity mismatch", llm.ErrExternalGrantRevoked)
	}
	quad, ok := identity.QuadrupleFrom(ctx)
	if !ok || quad.RunID != grant.LogicalRunID {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: logical run mismatch", llm.ErrExternalGrantRevoked)
	}
	if organizationID, supplied := llm.VerifiedOrganizationFrom(ctx); supplied && organizationID != grant.OrganizationID {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: verified organization mismatch", llm.ErrExternalGrantRevoked)
	}
	if verifiedGrant, bound := llm.VerifiedGrantContextFrom(ctx); bound {
		if verifiedGrant.Grant.GrantID != grant.GrantID || verifiedGrant.Grant.OrganizationID != grant.OrganizationID ||
			verifiedGrant.Grant.RuntimeID != grant.RuntimeID || verifiedGrant.Grant.CredentialBindingHandle != grant.CredentialBindingHandle ||
			verifiedGrant.Grant.CredentialAssetGeneration != grant.CredentialAssetGeneration {
			return llm.ResolvedCredential{}, fmt.Errorf("%w: verified grant binding mismatch", llm.ErrExternalGrantRevoked)
		}
	}
	if normalizedRouteMode(grant.RouteMode) != llm.ExternalGrantRouteCoordinatorBound {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: runtime-default grants do not resolve coordinator credentials", llm.ErrExternalGrantInvalid)
	}
	s.mu.RLock()
	binding, ok := s.bindings[grant.CredentialBindingHandle]
	s.mu.RUnlock()
	if !ok {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: %w", llm.ErrExternalGrantRevoked, ErrBindingNotFound)
	}
	if binding.Revoked || binding.Generation != grant.CredentialAssetGeneration || binding.OrganizationID != grant.OrganizationID ||
		binding.RuntimeID != grant.RuntimeID || binding.Provider != grant.Provider || binding.ProviderConnectionID != grant.ProviderConnectionID || binding.ProviderConnectionGeneration != grant.ProviderConnectionGeneration {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: binding generation or scope mismatch", llm.ErrExternalGrantRevoked)
	}
	return llm.ResolvedCredential{
		Provider:                     binding.Provider,
		CredentialBindingHandle:      binding.Handle,
		CredentialAssetGeneration:    binding.Generation,
		ProviderConnectionGeneration: binding.ProviderConnectionGeneration,
		Secret:                       binding.Secret,
	}, nil
}

type grantDocument struct {
	Version                      int                        `json:"version"`
	KeyID                        string                     `json:"kid"`
	Audience                     string                     `json:"aud"`
	GrantID                      string                     `json:"grant_id"`
	RouteMode                    llm.ExternalGrantRouteMode `json:"route_mode,omitempty"`
	OrganizationID               string                     `json:"organization_id"`
	RuntimeID                    string                     `json:"runtime_id"`
	AgentID                      string                     `json:"agent_id,omitempty"`
	TenantID                     string                     `json:"tenant_id"`
	UserID                       string                     `json:"user_id"`
	SessionID                    string                     `json:"session_id"`
	LogicalRunID                 string                     `json:"logical_run_id"`
	LogicalCallID                string                     `json:"logical_call_id"`
	AttemptNonce                 string                     `json:"attempt_nonce"`
	Provider                     string                     `json:"provider,omitempty"`
	ProviderModelID              string                     `json:"provider_model_id,omitempty"`
	ProviderConnectionID         string                     `json:"provider_connection_id,omitempty"`
	ProviderConnectionGeneration uint64                     `json:"provider_connection_generation,omitempty"`
	RouteID                      string                     `json:"route_id,omitempty"`
	CredentialBindingHandle      string                     `json:"credential_binding_handle,omitempty"`
	CredentialAssetGeneration    uint64                     `json:"credential_asset_generation,omitempty"`
	PolicyGeneration             uint64                     `json:"policy_generation"`
	MaxReasoning                 llm.ReasoningEffort        `json:"max_reasoning"`
	MaxOutputTokens              int                        `json:"max_output_tokens"`
	Lease                        llm.ComputeLease           `json:"lease"`
	IssuedAt                     time.Time                  `json:"issued_at"`
	ExpiresAt                    time.Time                  `json:"expires_at"`
}

func toDocument(g llm.ExternalGrant) grantDocument {
	return grantDocument{
		Version: g.Version, KeyID: g.KeyID, Audience: g.Audience, GrantID: g.GrantID,
		RouteMode:      g.RouteMode,
		OrganizationID: g.OrganizationID, RuntimeID: g.RuntimeID, AgentID: g.AgentID, TenantID: g.TenantID,
		UserID: g.UserID, SessionID: g.SessionID, LogicalRunID: g.LogicalRunID,
		LogicalCallID: g.LogicalCallID, AttemptNonce: g.AttemptNonce,
		Provider: g.Provider, ProviderModelID: g.ProviderModelID, ProviderConnectionID: g.ProviderConnectionID, ProviderConnectionGeneration: g.ProviderConnectionGeneration,
		RouteID: g.RouteID, CredentialBindingHandle: g.CredentialBindingHandle,
		CredentialAssetGeneration: g.CredentialAssetGeneration, PolicyGeneration: g.PolicyGeneration,
		MaxReasoning: g.MaxReasoning, MaxOutputTokens: g.MaxOutputTokens, Lease: g.Lease,
		IssuedAt: g.IssuedAt.UTC(), ExpiresAt: g.ExpiresAt.UTC(),
	}
}

func canonicalDocument(g llm.ExternalGrant) ([]byte, error) { return json.Marshal(toDocument(g)) }

func validateClaims(g llm.ExternalGrant, requireSignature bool) error {
	mode := normalizedRouteMode(g.RouteMode)
	switch {
	case g.Version != llm.ExternalGrantVersionLegacy && g.Version != llm.ExternalGrantVersionAgentBound:
		return fmt.Errorf("%w: unsupported version=%d", ErrInvalidGrantShape, g.Version)
	case g.Version == llm.ExternalGrantVersionLegacy && g.AgentID != "":
		return fmt.Errorf("%w: legacy grant cannot carry an unsigned agent binding", ErrInvalidGrantShape)
	case g.Version == llm.ExternalGrantVersionAgentBound && strings.TrimSpace(g.AgentID) == "":
		return fmt.Errorf("%w: version 2 requires an agent binding", ErrInvalidGrantShape)
	case g.KeyID == "", g.Audience == "", g.GrantID == "", g.OrganizationID == "", g.RuntimeID == "":
		return fmt.Errorf("%w: missing signed context claim", ErrInvalidGrantShape)
	case g.TenantID == "", g.UserID == "", g.SessionID == "", g.LogicalRunID == "", g.LogicalCallID == "", g.AttemptNonce == "":
		return fmt.Errorf("%w: missing identity/run claim", ErrInvalidGrantShape)
	case g.PolicyGeneration == 0:
		return fmt.Errorf("%w: missing policy generation", ErrInvalidGrantShape)
	case g.MaxReasoning == "" || reasoningRank(g.MaxReasoning) >= 99:
		return fmt.Errorf("%w: unsupported reasoning ceiling", ErrInvalidGrantShape)
	case g.MaxOutputTokens <= 0 || g.Lease.LeaseID == "" || g.Lease.TokenUnits <= 0 || g.Lease.ConsumedUnits < 0:
		return fmt.Errorf("%w: invalid output or lease", ErrInvalidGrantShape)
	case g.IssuedAt.IsZero() || g.ExpiresAt.IsZero() || !g.ExpiresAt.After(g.IssuedAt):
		return fmt.Errorf("%w: invalid validity interval", ErrInvalidGrantShape)
	case requireSignature && g.Signature == "":
		return fmt.Errorf("%w: missing signature", ErrInvalidGrantShape)
	}
	switch mode {
	case llm.ExternalGrantRouteCoordinatorBound:
		if g.Provider == "" || g.ProviderModelID == "" || g.ProviderConnectionID == "" || g.ProviderConnectionGeneration == 0 || g.RouteID == "" || g.CredentialBindingHandle == "" || g.CredentialAssetGeneration == 0 {
			return fmt.Errorf("%w: missing coordinator-bound provider route claim", ErrInvalidGrantShape)
		}
	case llm.ExternalGrantRouteRuntimeDefault:
		if g.Provider != "" || g.ProviderModelID != "" || g.ProviderConnectionID != "" || g.ProviderConnectionGeneration != 0 || g.RouteID != "" || g.CredentialBindingHandle != "" || g.CredentialAssetGeneration != 0 {
			return fmt.Errorf("%w: runtime-default grant carries coordinator route claim", ErrInvalidGrantShape)
		}
	default:
		return fmt.Errorf("%w: unsupported route mode %q", ErrInvalidGrantShape, g.RouteMode)
	}
	return nil
}

func normalizedRouteMode(mode llm.ExternalGrantRouteMode) llm.ExternalGrantRouteMode {
	if mode == "" {
		return llm.ExternalGrantRouteCoordinatorBound
	}
	return mode
}

func derivedIdentity(prefix, grantID, runID string) string {
	digest := sha256.Sum256([]byte(prefix + "\x00" + grantID + "\x00" + runID))
	return prefix + "/" + hex.EncodeToString(digest[:12])
}

// CanonicalBodyHash returns the stable digest of the unsigned claim document.
// It is useful for audit/debug assertions without exposing grant secrets.
func CanonicalBodyHash(g llm.ExternalGrant) (string, error) {
	doc, err := canonicalDocument(g)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(doc)
	return hex.EncodeToString(digest[:]), nil
}
