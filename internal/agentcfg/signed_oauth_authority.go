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
)

var (
	// ErrSignedCapabilityAuthority rejects an absent, malformed, expired, or
	// non-asymmetrically-signed D-401 authority envelope.
	ErrSignedCapabilityAuthority = errors.New("agentcfg: signed oauth mcp capability authority rejected")
	// ErrSignedCapabilityBinding rejects a valid envelope that does not bind the
	// exact registration request presented to Harbor.
	ErrSignedCapabilityBinding = errors.New("agentcfg: signed oauth mcp capability binding mismatch")
	// ErrSignedCapabilityScopeWidening rejects a requested scope outside the
	// true boot-declared ceiling. This path never silently intersects scopes.
	ErrSignedCapabilityScopeWidening = errors.New("agentcfg: signed oauth mcp capability scope exceeds boot ceiling")
)

// SignedOAuthMCPBinding is the server-derived, non-secret registration binding
// that must exactly match a verified D-401 authority envelope. URLDigest must
// come from CanonicalOAuthMCPURL, never raw request text.
type SignedOAuthMCPBinding struct {
	TenantID           string
	AgentID            string
	Broker             string
	ProviderName       string
	CapabilityRevision string
	URLDigest          string
	Audience           string
	Scopes             []string
}

// SignedOAuthMCPAuthorityClaims is the JWT payload accepted for D-401. The
// issuer/key selection occurs at the boot trust-anchor boundary; this type only
// verifies the exact bounded claim once that trusted verifier has been chosen.
type SignedOAuthMCPAuthorityClaims struct {
	TenantID           string   `json:"tenant_id"`
	AgentID            string   `json:"agent_id"`
	Broker             string   `json:"broker"`
	ProviderName       string   `json:"provider_name"`
	CapabilityRevision string   `json:"capability_revision"`
	URLDigest          string   `json:"url_digest"`
	Audience           string   `json:"audience"`
	Scopes             []string `json:"scopes"`
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
// issuer/key ID/timing, and compares every D-401 binding field exactly.
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
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: %v", ErrSignedCapabilityAuthority, err)
	}
	if strings.TrimSpace(claims.ID) == "" || claims.IssuedAt == nil || claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(now) {
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: jti or bounded timing missing", ErrSignedCapabilityAuthority)
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
		return SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: invalid boot ceiling: %v", ErrSignedCapabilityAuthority, err)
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
	if claims.IssuedAt == nil || claims.ExpiresAt == nil || claims.ExpiresAt.Time.Before(claims.IssuedAt.Time) || claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time) > maxLifetime {
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
	if claims.TenantID != expected.TenantID || claims.AgentID != expected.AgentID || claims.Broker != expected.Broker ||
		claims.ProviderName != expected.ProviderName || claims.CapabilityRevision != expected.CapabilityRevision ||
		claims.URLDigest != expected.URLDigest || claims.Audience != expected.Audience || !sameStrings(claimedScopes, expectedScopes) {
		return ErrSignedCapabilityBinding
	}
	return nil
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
	canonicalScopes, _ := CanonicalScopes(binding.Scopes)
	parts := []string{binding.TenantID, binding.AgentID, binding.Broker, binding.ProviderName, binding.CapabilityRevision, binding.URLDigest, binding.Audience}
	parts = append(parts, canonicalScopes...)
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
