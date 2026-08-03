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
	binding := SignedOAuthMCPBinding{TenantID: "tenant", UserID: "user", SessionID: "session", AgentID: "agent", Broker: "boot-broker", ProviderName: "github", CapabilityRevision: "1", URLDigest: "digest", SinkDigest: "sink-digest", Audience: "api", Scopes: []string{"repo:read", "user:read"}, Connection: SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://mcp.example.test/mcp", ToolAllowlist: []string{"read"}, ConnectTimeoutMS: 1000, ArtifactByteEligible: true, ArtifactParams: map[string][]string{"knowledge.ingest": {"content_base64"}}}}
	claims := SignedOAuthMCPAuthorityClaims{TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID, AgentID: binding.AgentID, Broker: binding.Broker, ProviderName: binding.ProviderName, CapabilityRevision: binding.CapabilityRevision, URLDigest: binding.URLDigest, SinkDigest: binding.SinkDigest, Audience: binding.Audience, Scopes: []string{"user:read", "repo:read"}, Connection: binding.Connection, RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: "jti", IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}}
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
	mutatedPolicy := binding
	mutatedPolicy.Connection.ToolAllowlist = []string{"write"}
	if _, err := VerifySignedOAuthMCPAuthority(raw, "issuer", "key-1", &key.PublicKey, now, mutatedPolicy, []string{"repo:read", "user:read"}); !errors.Is(err, ErrSignedCapabilityBinding) {
		t.Fatalf("connection policy mutation err = %v, want binding refusal", err)
	}
	mutatedEgress := binding
	mutatedEgress.Connection.ArtifactParams = map[string][]string{"knowledge.ingest": {"widened"}}
	if _, err := VerifySignedOAuthMCPAuthority(raw, "issuer", "key-1", &key.PublicKey, now, mutatedEgress, []string{"repo:read", "user:read"}); !errors.Is(err, ErrSignedCapabilityBinding) {
		t.Fatalf("artifact egress mutation err = %v, want binding refusal", err)
	}
}

func TestVerifySignedOAuthMCPAuthority_ArtifactMappingCanonicalEquivalence(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	binding := SignedOAuthMCPBinding{
		TenantID: "tenant", UserID: "user", SessionID: "session", AgentID: "agent", Broker: "broker",
		ProviderName: "provider", CapabilityRevision: "1", URLDigest: "url", SinkDigest: "sink", Audience: "api",
		Connection: SignedOAuthMCPConnectionDescriptor{Name: "knowledge", URL: "https://example.test/mcp", ArtifactByteEligible: true,
			ArtifactParams: map[string][]string{"knowledge.ingest": {"content_base64", "metadata"}}},
	}
	whitespaceTool := " " + binding.Connection.Name + ".ingest "
	claims := SignedOAuthMCPAuthorityClaims{
		TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID, AgentID: binding.AgentID,
		Broker: binding.Broker, ProviderName: binding.ProviderName, CapabilityRevision: binding.CapabilityRevision,
		URLDigest: binding.URLDigest, SinkDigest: binding.SinkDigest, Audience: binding.Audience,
		Connection: SignedOAuthMCPConnectionDescriptor{Name: binding.Connection.Name, URL: binding.Connection.URL, ArtifactByteEligible: true,
			ArtifactParams: map[string][]string{whitespaceTool: {" metadata ", " content_base64 "}}},
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: "jti-canonical", IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "kid"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySignedOAuthMCPAuthority(raw, "issuer", "kid", &key.PublicKey, now, binding, nil); err != nil {
		t.Fatalf("canonical-equivalent signed mapping refused: %v", err)
	}
}

func TestSignedOAuthMCPPairFingerprint_ArtifactEgressIsImmutableBinding(t *testing.T) {
	base := SignedOAuthMCPBinding{TenantID: "t", AgentID: "a", Connection: SignedOAuthMCPConnectionDescriptor{Name: "knowledge", URL: "https://example.test/mcp"}}
	const preExtensionFingerprint = "cdf881db05c3c01691a7277fbb0f8b0bd4d87d34e5916797e45987dba2d52951"
	if got := SignedOAuthMCPPairFingerprint(base); got != preExtensionFingerprint {
		t.Fatalf("omitted extension changed the pre-extension fingerprint: got %s want %s", got, preExtensionFingerprint)
	}
	eligible := base
	eligible.Connection.ArtifactByteEligible = true
	eligible.Connection.ArtifactParams = map[string][]string{"knowledge.ingest": {"content_base64"}}
	widened := eligible
	widened.Connection.ArtifactParams = map[string][]string{"knowledge.ingest": {"content_base64", "metadata"}}
	if SignedOAuthMCPPairFingerprint(base) == SignedOAuthMCPPairFingerprint(eligible) {
		t.Fatal("eligibility and mapping did not change the replay fingerprint")
	}
	if SignedOAuthMCPPairFingerprint(eligible) == SignedOAuthMCPPairFingerprint(widened) {
		t.Fatal("mapping widening did not change the replay fingerprint")
	}
}

func TestVerifySignedOAuthMCPAuthority_RefusesFutureIssuedAtBeyondSkew(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	binding := SignedOAuthMCPBinding{TenantID: "t", UserID: "u", SessionID: "s", AgentID: "a", Broker: "b", ProviderName: "p", CapabilityRevision: "v1", URLDigest: "url", SinkDigest: "sink", Audience: "aud"}
	claims := SignedOAuthMCPAuthorityClaims{
		TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID, AgentID: binding.AgentID,
		Broker: binding.Broker, ProviderName: binding.ProviderName, CapabilityRevision: binding.CapabilityRevision,
		URLDigest: binding.URLDigest, SinkDigest: binding.SinkDigest, Audience: binding.Audience,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: "jti", IssuedAt: jwt.NewNumericDate(now.Add(SignedOAuthMCPAuthorityClockSkew + time.Second)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "kid"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySignedOAuthMCPAuthority(raw, "issuer", "kid", &key.PublicKey, now, binding, nil); !errors.Is(err, ErrSignedCapabilityAuthority) {
		t.Fatalf("future iat error = %v, want authority refusal", err)
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
