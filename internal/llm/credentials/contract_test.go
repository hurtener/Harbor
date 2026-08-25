package credentials

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

func contractGrant() llm.ExternalGrant {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	return llm.ExternalGrant{
		Version: llm.ExternalGrantVersionAgentBound, KeyID: "key-1", Audience: "runtime",
		GrantID: "grant-1", RouteMode: llm.ExternalGrantRouteCoordinatorBound,
		OrganizationID: "org-1", RuntimeID: "runtime-1", AgentID: "agent-1",
		TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", LogicalRunID: "run-1",
		LogicalCallID: "call-1", AttemptNonce: "nonce-1", Provider: "provider-1",
		ProviderModelID: "model-1", ProviderConnectionID: "connection-1",
		ProviderConnectionGeneration: 2, RouteID: "route-1",
		CredentialBindingHandle: "opaque-1", CredentialAssetGeneration: 3,
		PolicyGeneration: 4, MaxOutputTokens: 1024,
		Lease:    llm.ComputeLease{LeaseID: "lease-1", Epoch: 1, TokenUnits: 2048, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute), Signature: "fixture-signature",
	}
}

func TestContract_CanonicalRoundTripAndExactBinding(t *testing.T) {
	request, err := NewRequest(contractGrant())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalCanonicalRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := UnmarshalCanonicalRequest(raw)
	if err != nil || parsed.Grant.OrganizationID != "org-1" {
		t.Fatalf("parsed request = %#v, %v", parsed, err)
	}
	response := Response{Version: Version, Provider: "provider-1", CredentialBindingHandle: "opaque-1", CredentialAssetGeneration: 3, ProviderConnectionGeneration: 2, Secret: "fixture-provider-secret", ExpiresAt: request.Grant.ExpiresAt}
	body, err := MarshalCanonicalResponse(request, response)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCanonicalResponse(request, body)
	if err != nil || got.Secret != response.Secret {
		t.Fatalf("parsed response = %#v, %v", got, err)
	}
	if bytes.Contains(raw, []byte(response.Secret)) {
		t.Fatal("request leaked provider secret")
	}
}

func TestContract_RejectsNoncanonicalAndMismatchedMaterial(t *testing.T) {
	request, _ := NewRequest(contractGrant())
	raw, _ := MarshalCanonicalRequest(request)
	noncanonical := append([]byte{' '}, raw...)
	if _, err := UnmarshalCanonicalRequest(noncanonical); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("noncanonical request error = %v", err)
	}
	duplicate := strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1)
	if _, err := UnmarshalCanonicalRequest([]byte(duplicate)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("duplicate request error = %v", err)
	}
	response := Response{Version: Version, Provider: "provider-1", CredentialBindingHandle: "opaque-1", CredentialAssetGeneration: 3, ProviderConnectionGeneration: 2, Secret: "fixture-provider-secret", ExpiresAt: request.Grant.ExpiresAt}
	for name, mutate := range map[string]func(*Response){
		"provider":              func(r *Response) { r.Provider = "other" },
		"handle":                func(r *Response) { r.CredentialBindingHandle = "other" },
		"asset generation":      func(r *Response) { r.CredentialAssetGeneration++ },
		"connection generation": func(r *Response) { r.ProviderConnectionGeneration++ },
		"secret":                func(r *Response) { r.Secret = "" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := response
			mutate(&candidate)
			if _, err := MarshalCanonicalResponse(request, candidate); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestContract_RefusesRuntimeDefault(t *testing.T) {
	grant := contractGrant()
	grant.RouteMode = llm.ExternalGrantRouteRuntimeDefault
	grant.Provider, grant.ProviderModelID, grant.ProviderConnectionID = "", "", ""
	grant.RouteID, grant.CredentialBindingHandle = "", ""
	grant.ProviderConnectionGeneration, grant.CredentialAssetGeneration = 0, 0
	if _, err := NewRequest(grant); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
}

func TestContract_RejectsBoundsVersionsUnknownAndTrailingJSON(t *testing.T) {
	request, _ := NewRequest(contractGrant())
	if _, err := MarshalCanonicalRequest(Request{Version: Version + 1, Grant: request.Grant}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("request version error = %v", err)
	}
	for name, raw := range map[string][]byte{
		"empty":     nil,
		"overbound": bytes.Repeat([]byte{'x'}, MaxRequestBytes+1),
		"unknown":   []byte(`{"version":1,"grant":{},"extra":true}`),
		"trailing":  []byte(`{"version":1,"grant":{}} {}`),
	} {
		t.Run("request_"+name, func(t *testing.T) {
			if _, err := UnmarshalCanonicalRequest(raw); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	response := Response{Version: Version, Provider: request.Grant.Provider, CredentialBindingHandle: request.Grant.CredentialBindingHandle, CredentialAssetGeneration: request.Grant.CredentialAssetGeneration, ProviderConnectionGeneration: request.Grant.ProviderConnectionGeneration, Secret: "fixture-provider-secret", ExpiresAt: request.Grant.ExpiresAt}
	body, _ := MarshalCanonicalResponse(request, response)
	unknown := bytes.Replace(body, []byte(`"expires_at"`), []byte(`"extra":true,"expires_at"`), 1)
	duplicate := bytes.Replace(body, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
	for name, raw := range map[string][]byte{
		"empty":        nil,
		"overbound":    bytes.Repeat([]byte{'x'}, MaxResponseBytes+1),
		"noncanonical": append([]byte{' '}, body...),
		"unknown":      unknown,
		"duplicate":    duplicate,
		"trailing":     append(append([]byte(nil), body...), []byte(` {}`)...),
	} {
		t.Run("response_"+name, func(t *testing.T) {
			if _, err := ParseCanonicalResponse(request, raw); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	response.Version++
	if _, err := MarshalCanonicalResponse(request, response); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("response version error = %v", err)
	}
}
