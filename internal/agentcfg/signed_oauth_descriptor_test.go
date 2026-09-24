package agentcfg

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSignedOAuthMCPDescriptor_DecodeIsAtomicAndStrict(t *testing.T) {
	initial := SignedOAuthMCPConnectionDescriptor{Name: "before", URL: "https://before.example.test/mcp"}
	for _, raw := range []string{
		`{"name":"after",`,
		`{"name":"after"} {}`,
		`{"name":"after"} trailing`,
		`{"name":"after","injection":{"unknown":true}}`,
	} {
		descriptor := initial
		if err := descriptor.UnmarshalJSON([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid descriptor %s", raw)
		}
		if !reflect.DeepEqual(descriptor, initial) {
			t.Fatal("failed decode partially changed the destination")
		}
	}
	var decoded SignedOAuthMCPConnectionDescriptor
	if err := decoded.UnmarshalJSON([]byte(`{"name":"after","url":"https://after.example.test/mcp"}`)); err != nil || decoded.Name != "after" {
		t.Fatalf("supported descriptor = %+v, %v", decoded, err)
	}
}

func TestSignedOAuthMCPDescriptor_UnsupportedAuthorityFailsClosed(t *testing.T) {
	for _, field := range []string{`"tool_policies":{"write":{"max_attempts":1}}`, `"future_authority":true`} {
		t.Run(field, func(t *testing.T) {
			raw := []byte(`{"name":"server","url":"https://example.test/mcp",` + field + `}`)
			var descriptor SignedOAuthMCPConnectionDescriptor
			if err := json.Unmarshal(raw, &descriptor); err == nil {
				t.Fatal("unsupported authority was silently stripped")
			}
			var pair SignedOAuthMCPPair
			if err := json.Unmarshal(append(append([]byte(`{"connection":`), raw...), '}'), &pair); err == nil {
				t.Fatal("persisted pair silently lost unsupported authority")
			}
		})
	}
}

func TestVerifySignedOAuthMCPAuthority_UnsupportedDescriptorFailsClosed(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	binding := SignedOAuthMCPBinding{TenantID: "t", UserID: "u", SessionID: "s", AgentID: "a",
		Broker: "b", ProviderName: "p", CapabilityRevision: "1", URLDigest: "url", SinkDigest: "sink", Audience: "api",
		Connection: SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://example.test/mcp"}}
	claims := SignedOAuthMCPAuthorityClaims{
		TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID, AgentID: binding.AgentID,
		Broker: binding.Broker, ProviderName: binding.ProviderName, CapabilityRevision: binding.CapabilityRevision,
		URLDigest: binding.URLDigest, SinkDigest: binding.SinkDigest, Audience: binding.Audience, Connection: binding.Connection,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: "fixture", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))},
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	var extended jwt.MapClaims
	if err := json.Unmarshal(raw, &extended); err != nil {
		t.Fatal(err)
	}
	for _, unsupported := range []bool{false, true} {
		if unsupported {
			extended["connection"].(map[string]any)["tool_policies"] = map[string]any{"write": map[string]any{"max_attempts": 1}}
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, extended)
		token.Header["kid"] = "fixture-key"
		signed, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifySignedOAuthMCPAuthority(signed, "issuer", "fixture-key", &key.PublicKey, now, binding, nil)
		if unsupported && !errors.Is(err, ErrSignedCapabilityAuthority) {
			t.Fatalf("unsupported signed descriptor: %v, want authority rejection", err)
		}
		if !unsupported && err != nil {
			t.Fatalf("unchanged supported descriptor refused: %v", err)
		}
	}
}
