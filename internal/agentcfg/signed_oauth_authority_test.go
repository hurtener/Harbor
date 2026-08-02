package agentcfg

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestVerifySignedOAuthMCPAuthority_ExactBindingAndScopeCeiling(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	binding := SignedOAuthMCPBinding{TenantID: "tenant", AgentID: "agent", Broker: "boot-broker", ProviderName: "github", CapabilityRevision: "1", URLDigest: "digest", Audience: "api", Scopes: []string{"repo:read", "user:read"}}
	claims := SignedOAuthMCPAuthorityClaims{TenantID: binding.TenantID, AgentID: binding.AgentID, Broker: binding.Broker, ProviderName: binding.ProviderName, CapabilityRevision: binding.CapabilityRevision, URLDigest: binding.URLDigest, Audience: binding.Audience, Scopes: []string{"user:read", "repo:read"}, RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: "jti", IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "key-1"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySignedOAuthMCPAuthority(raw, "issuer", "key-1", &key.PublicKey, now, binding, []string{"repo:read", "user:read"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := VerifySignedOAuthMCPAuthority(raw, "issuer", "key-1", &key.PublicKey, now, binding, []string{"repo:read"}); !errors.Is(err, ErrSignedCapabilityScopeWidening) {
		t.Fatalf("scope ceiling err = %v, want ErrSignedCapabilityScopeWidening", err)
	}
}

func TestVerifySignedOAuthMCPAuthority_RefusesRequestMismatch(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claims := SignedOAuthMCPAuthorityClaims{TenantID: "t", AgentID: "a", Broker: "b", ProviderName: "p", CapabilityRevision: "1", URLDigest: "one", Audience: "aud", RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: "jti", IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "kid"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifySignedOAuthMCPAuthority(raw, "issuer", "kid", &key.PublicKey, now, SignedOAuthMCPBinding{TenantID: "t", AgentID: "a", Broker: "b", ProviderName: "p", CapabilityRevision: "1", URLDigest: "two", Audience: "aud"}, nil)
	if !errors.Is(err, ErrSignedCapabilityBinding) {
		t.Fatalf("mismatch err = %v, want ErrSignedCapabilityBinding", err)
	}
}

func TestVerifySignedOAuthMCPAuthorityBounded_ExpiryBoundaryAndOverLifetime(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	binding := SignedOAuthMCPBinding{TenantID: "t", AgentID: "a", Broker: "b", ProviderName: "p", CapabilityRevision: "1", URLDigest: "digest", Audience: "aud", Scopes: []string{"read"}}
	sign := func(expiry time.Time) string {
		t.Helper()
		claims := SignedOAuthMCPAuthorityClaims{TenantID: binding.TenantID, AgentID: binding.AgentID, Broker: binding.Broker, ProviderName: binding.ProviderName, CapabilityRevision: binding.CapabilityRevision, URLDigest: binding.URLDigest, Audience: binding.Audience, Scopes: binding.Scopes, RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: "jti", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expiry)}}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "kid"
		raw, signErr := token.SignedString(key)
		if signErr != nil {
			t.Fatal(signErr)
		}
		return raw
	}
	if _, err := VerifySignedOAuthMCPAuthorityBounded(sign(now.Add(10*time.Minute)), "issuer", "kid", &key.PublicKey, now, binding, []string{"read"}, 10*time.Minute); err != nil {
		t.Fatalf("exact expiry boundary must pass: %v", err)
	}
	if _, err := VerifySignedOAuthMCPAuthorityBounded(sign(now.Add(10*time.Minute+time.Second)), "issuer", "kid", &key.PublicKey, now, binding, []string{"read"}, 10*time.Minute); !errors.Is(err, ErrSignedCapabilityAuthority) {
		t.Fatalf("over-lifetime err = %v, want ErrSignedCapabilityAuthority", err)
	}
}
