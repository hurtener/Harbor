package agentcfg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/config"
)

var (
	// ErrSignedCapabilityAuthority rejects an absent, malformed, expired, or
	// non-asymmetrically-signed capability authority envelope.
	ErrSignedCapabilityAuthority = errors.New("agentcfg: signed oauth mcp capability authority rejected")
	// ErrSignedCapabilityBinding rejects a valid envelope that does not bind the
	// exact registration request presented to Harbor.
	ErrSignedCapabilityBinding = errors.New("agentcfg: signed oauth mcp capability binding mismatch")
	// ErrSignedCapabilityScopeWidening rejects a requested scope outside the
	// true boot-declared ceiling. This path never silently intersects scopes.
	ErrSignedCapabilityScopeWidening = errors.New("agentcfg: signed oauth mcp capability scope exceeds boot ceiling")
)

// SignedOAuthMCPBinding is the server-derived, non-secret registration binding
// that must exactly match a verified signed-capability authority envelope. URLDigest must
// come from CanonicalOAuthMCPURL, never raw request text.
type SignedOAuthMCPBinding struct {
	TenantID           string
	UserID             string
	SessionID          string
	AgentID            string
	Broker             string
	ProviderName       string
	CapabilityRevision string
	URLDigest          string
	SinkDigest         string
	Audience           string
	Scopes             []string
	Connection         SignedOAuthMCPConnectionDescriptor
}

// SignedOAuthMCPAuthorityClaims is the JWT payload accepted for signed capability registration. The
// issuer/key selection occurs at the boot trust-anchor boundary; this type only
// verifies the exact bounded claim once that trusted verifier has been chosen.
type SignedOAuthMCPAuthorityClaims struct {
	TenantID           string                             `json:"tenant_id"`
	UserID             string                             `json:"user_id"`
	SessionID          string                             `json:"session_id"`
	AgentID            string                             `json:"agent_id"`
	Broker             string                             `json:"broker"`
	ProviderName       string                             `json:"provider_name"`
	CapabilityRevision string                             `json:"capability_revision"`
	URLDigest          string                             `json:"url_digest"`
	SinkDigest         string                             `json:"sink_digest"`
	Audience           string                             `json:"audience"`
	Scopes             []string                           `json:"scopes"`
	Connection         SignedOAuthMCPConnectionDescriptor `json:"connection"`
	jwt.RegisteredClaims
}

// CanonicalScopes returns a sorted, duplicate-free scope set. Empty or
// duplicate values are invalid because a signer and verifier must have one
// byte-stable interpretation of authority.
func CanonicalScopes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			return nil, fmt.Errorf("%w: empty scope", ErrSignedCapabilityBinding)
		}
		if _, exists := seen[s]; exists {
			return nil, fmt.Errorf("%w: duplicate scope %q", ErrSignedCapabilityBinding, s)
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// VerifySignedOAuthMCPAuthority verifies one JWT against a selected boot
// trust-anchor key, permits only Harbor's asymmetric JWT algorithms, checks
// issuer/key ID/timing, and compares every signed-capability binding field exactly.
//
// key and issuer are construction-time trust-anchor inputs. They are never
// read from the request or its unverified JWT claims.
func VerifySignedOAuthMCPAuthority(raw, issuer, keyID string, key any, now time.Time, expected SignedOAuthMCPBinding, scopeCeiling []string) (SignedOAuthMCPAuthorityClaims, error) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(issuer) == "" || strings.TrimSpace(keyID) == "" || key == nil {
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: boot trust anchor incomplete", ErrSignedCapabilityAuthority)
	}
	claims := SignedOAuthMCPAuthorityClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg(), jwt.SigningMethodRS384.Alg(), jwt.SigningMethodRS512.Alg(), jwt.SigningMethodES256.Alg(), jwt.SigningMethodES384.Alg(), jwt.SigningMethodES512.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	token, err := parser.ParseWithClaims(raw, &claims, func(token *jwt.Token) (any, error) {
		if token.Header["kid"] != keyID {
			return nil, fmt.Errorf("unexpected key id")
		}
		return key, nil
	})
	if err != nil || token == nil || !token.Valid {
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: verify authority: %w", ErrSignedCapabilityAuthority, err)
	}
	if strings.TrimSpace(claims.ID) == "" || claims.IssuedAt == nil || claims.ExpiresAt == nil || !claims.ExpiresAt.After(now) {
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: jti or bounded timing missing", ErrSignedCapabilityAuthority)
	}
	// Expiry is intentionally strict (the parser and the explicit check above
	// apply no leeway). Issued-at permits only a small, documented clock-skew
	// window; a token minted farther in the future is not current authority.
	if claims.IssuedAt.After(now.Add(SignedOAuthMCPAuthorityClockSkew)) {
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: authority issued in the future", ErrSignedCapabilityAuthority)
	}
	if err := matchSignedOAuthMCPBinding(claims, expected); err != nil {
		return SignedOAuthMCPAuthorityClaims{}, err
	}
	requested, err := CanonicalScopes(expected.Scopes)
	if err != nil {
		return SignedOAuthMCPAuthorityClaims{}, err
	}
	ceiling, err := CanonicalScopes(scopeCeiling)
	if err != nil {
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: invalid boot ceiling: %w", ErrSignedCapabilityAuthority, err)
	}
	allowed := make(map[string]struct{}, len(ceiling))
	for _, scope := range ceiling {
		allowed[scope] = struct{}{}
	}
	for _, scope := range requested {
		if _, ok := allowed[scope]; !ok {
			return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: %q", ErrSignedCapabilityScopeWidening, scope)
		}
	}
	return claims, nil
}

// VerifySignedOAuthMCPAuthorityBounded applies the boot trust anchor's
// required maximum envelope lifetime after [VerifySignedOAuthMCPAuthority]
// has verified the signature, issuer, exact binding, and current expiry. The
// ceiling itself is config-only; only iat/exp travel in the signed envelope.
// An exact-boundary lifetime is valid, while an unset/invalid ceiling fails
// closed rather than acquiring a hidden product-wide default.
func VerifySignedOAuthMCPAuthorityBounded(raw, issuer, keyID string, key any, now time.Time, expected SignedOAuthMCPBinding, scopeCeiling []string, maxLifetime time.Duration) (SignedOAuthMCPAuthorityClaims, error) {
	if maxLifetime <= 0 {
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: boot maximum authority lifetime is not positive", ErrSignedCapabilityAuthority)
	}
	claims, err := VerifySignedOAuthMCPAuthority(raw, issuer, keyID, key, now, expected, scopeCeiling)
	if err != nil {
		return SignedOAuthMCPAuthorityClaims{}, err
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil || !claims.ExpiresAt.After(claims.IssuedAt.Time) || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > maxLifetime {
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: authority lifetime exceeds boot maximum", ErrSignedCapabilityAuthority)
	}
	return claims, nil
}

func matchSignedOAuthMCPBinding(claims SignedOAuthMCPAuthorityClaims, expected SignedOAuthMCPBinding) error {
	claimedScopes, err := CanonicalScopes(claims.Scopes)
	if err != nil {
		return err
	}
	expectedScopes, err := CanonicalScopes(expected.Scopes)
	if err != nil {
		return err
	}
	if claims.TenantID != expected.TenantID || claims.UserID != expected.UserID || claims.SessionID != expected.SessionID ||
		claims.AgentID != expected.AgentID || claims.Broker != expected.Broker ||
		claims.ProviderName != expected.ProviderName || claims.CapabilityRevision != expected.CapabilityRevision ||
		claims.URLDigest != expected.URLDigest || claims.SinkDigest != expected.SinkDigest || claims.Audience != expected.Audience ||
		!sameStrings(claimedScopes, expectedScopes) || !sameSignedOAuthMCPConnection(claims.Connection, expected.Connection) {
		return ErrSignedCapabilityBinding
	}
	return nil
}

// SignedOAuthMCPAuthorityClockSkew is the sole issued-at skew allowance. It is
// not applied to expiry or maximum lifetime checks.
const SignedOAuthMCPAuthorityClockSkew = 30 * time.Second

func sameSignedOAuthMCPConnection(left, right SignedOAuthMCPConnectionDescriptor) bool {
	leftAllow, leftAllowErr := CanonicalScopes(left.ToolAllowlist)
	rightAllow, rightAllowErr := CanonicalScopes(right.ToolAllowlist)
	leftDeny, leftDenyErr := CanonicalScopes(left.ToolDenylist)
	rightDeny, rightDenyErr := CanonicalScopes(right.ToolDenylist)
	leftParams, leftParamsErr := config.NormalizeMCPArtifactParams(config.MCPArtifactParams(left.ArtifactParams))
	rightParams, rightParamsErr := config.NormalizeMCPArtifactParams(config.MCPArtifactParams(right.ArtifactParams))
	return leftAllowErr == nil && rightAllowErr == nil && leftDenyErr == nil && rightDenyErr == nil && leftParamsErr == nil && rightParamsErr == nil &&
		left.Name == right.Name && left.URL == right.URL && left.ConnectTimeoutMS == right.ConnectTimeoutMS &&
		left.RequestTimeoutMS == right.RequestTimeoutMS && left.ArtifactByteEligible == right.ArtifactByteEligible &&
		sameStrings(leftAllow, rightAllow) && sameStrings(leftDeny, rightDeny) && sameInjection(left.Injection, right.Injection) && sameArtifactParams(leftParams, rightParams)
}

func sameInjection(left, right *MCPCredentialInjectionDescriptor) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Provider == right.Provider && left.Form == right.Form && left.Header == right.Header &&
		left.BasicUsername == right.BasicUsername && left.MetaKey == right.MetaKey
}

func sameArtifactParams(left, right map[string][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for tool, leftParams := range left {
		rightParams, ok := right[tool]
		if !ok || !sameStrings(leftParams, rightParams) {
			return false
		}
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// SignedOAuthMCPPairFingerprint produces the collision-resistant immutable
// pair identifier used to bind a durable JTI record to exactly one capability.
func SignedOAuthMCPPairFingerprint(binding SignedOAuthMCPBinding) string {
	canonicalScopes, err := CanonicalScopes(binding.Scopes)
	if err != nil {
		// The public fingerprint helper cannot return an error. Preserve the raw
		// values rather than silently dropping an invalid scope from a binding.
		canonicalScopes = append([]string(nil), binding.Scopes...)
	}
	allow, allowErr := CanonicalScopes(binding.Connection.ToolAllowlist)
	if allowErr != nil {
		allow = append([]string(nil), binding.Connection.ToolAllowlist...)
	}
	deny, denyErr := CanonicalScopes(binding.Connection.ToolDenylist)
	if denyErr != nil {
		deny = append([]string(nil), binding.Connection.ToolDenylist...)
	}
	artifactTools := make([]string, 0, len(binding.Connection.ArtifactParams))
	for tool := range binding.Connection.ArtifactParams {
		artifactTools = append(artifactTools, tool)
	}
	sort.Strings(artifactTools)
	parts := make([]string, 0, 22+len(canonicalScopes)+len(allow)+len(deny)+len(artifactTools))
	parts = append(parts, binding.TenantID, binding.UserID, binding.SessionID, binding.AgentID, binding.Broker, binding.ProviderName, binding.CapabilityRevision,
		binding.URLDigest, binding.SinkDigest, binding.Audience, binding.Connection.Name, binding.Connection.URL,
		fmt.Sprintf("%d", binding.Connection.ConnectTimeoutMS), fmt.Sprintf("%d", binding.Connection.RequestTimeoutMS),
		"scopes", fmt.Sprintf("%d", len(canonicalScopes)))
	parts = append(parts, canonicalScopes...)
	parts = append(parts, "tool_allowlist", fmt.Sprintf("%d", len(allow)))
	parts = append(parts, allow...)
	parts = append(parts, "tool_denylist", fmt.Sprintf("%d", len(deny)))
	parts = append(parts, deny...)
	// Preserve every pre-v1.26.9 fingerprint byte-for-byte. Receiver injection
	// contributes only when present, while a signed mapping binds every field so
	// a replay cannot redirect a per-user credential to another provider/form/key.
	if injection := binding.Connection.Injection; injection != nil {
		parts = append(parts, "injection", injection.Provider, injection.Form, injection.Header, injection.BasicUsername, injection.MetaKey)
	}
	// Preserve the pre-extension fingerprint byte-for-byte for every existing
	// signed pair. The additive policy contributes bytes only when declared;
	// an omitted false/nil extension must survive an upgrade and restart under
	// the operation receipt minted by the older binary.
	if binding.Connection.ArtifactByteEligible || len(artifactTools) > 0 {
		parts = append(parts, "artifact_byte_eligible", fmt.Sprintf("%t", binding.Connection.ArtifactByteEligible),
			"artifact_params", fmt.Sprintf("%d", len(artifactTools)))
		for _, tool := range artifactTools {
			params := append([]string(nil), binding.Connection.ArtifactParams[tool]...)
			sort.Strings(params)
			parts = append(parts, tool, fmt.Sprintf("%d", len(params)))
			parts = append(parts, params...)
		}
	}
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SignedOAuthMCPConnectionFingerprint identifies the complete immutable
// signed connection descriptor, including restrictive tool policy and bounds.
func SignedOAuthMCPConnectionFingerprint(connection SignedOAuthMCPConnectionDescriptor) string {
	binding := SignedOAuthMCPBinding{Connection: connection}
	return SignedOAuthMCPPairFingerprint(binding)
}
