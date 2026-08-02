package protocol

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

type signedOAuthMCPStaticKeySet struct {
	key crypto.PublicKey
	alg string
}

func (s signedOAuthMCPStaticKeySet) KeyByID(_ string) (crypto.PublicKey, string, error) {
	return s.key, s.alg, nil
}

func TestSignedOAuthMCPCapabilityAuthority_Verify_RejectsKeyAlgorithmMismatchAndOverLifetime(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	binding := agentcfg.SignedOAuthMCPBinding{TenantID: "tenant", AgentID: "agent", Broker: "broker", ProviderName: "provider", CapabilityRevision: "v1", URLDigest: "digest", Audience: "aud", Scopes: []string{"read"}}
	sign := func(expiry time.Time) string {
		t.Helper()
		claims := agentcfg.SignedOAuthMCPAuthorityClaims{TenantID: binding.TenantID, AgentID: binding.AgentID, Broker: binding.Broker, ProviderName: binding.ProviderName, CapabilityRevision: binding.CapabilityRevision, URLDigest: binding.URLDigest, Audience: binding.Audience, Scopes: binding.Scopes, RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: "jti", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expiry)}}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "kid"
		raw, signErr := token.SignedString(key)
		if signErr != nil {
			t.Fatal(signErr)
		}
		return raw
	}
	authority := SignedOAuthMCPCapabilityAuthority{Broker: "broker", Issuer: "issuer", Keys: signedOAuthMCPStaticKeySet{key: &key.PublicKey, alg: jwt.SigningMethodRS256.Alg()}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: 10 * time.Minute}
	if _, err := authority.Verify(sign(now.Add(10*time.Minute)), now, binding); err != nil {
		t.Fatalf("boundary verification: %v", err)
	}
	if _, err := authority.Verify(sign(now.Add(10*time.Minute+time.Second)), now, binding); !errors.Is(err, agentcfg.ErrSignedCapabilityAuthority) {
		t.Fatalf("over-lifetime err = %v, want ErrSignedCapabilityAuthority", err)
	}
	wrongAlg := authority
	wrongAlg.Keys = signedOAuthMCPStaticKeySet{key: &key.PublicKey, alg: jwt.SigningMethodRS384.Alg()}
	if _, err := wrongAlg.Verify(sign(now.Add(time.Minute)), now, binding); !errors.Is(err, agentcfg.ErrSignedCapabilityAuthority) {
		t.Fatalf("algorithm mismatch err = %v, want ErrSignedCapabilityAuthority", err)
	}
}
