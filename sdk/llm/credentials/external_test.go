package credentials_test

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/sdk/llm"
	"github.com/hurtener/Harbor/sdk/llm/credentials"
)

func TestPublicContract_IsUsableWithoutInternalImports(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	grant := llm.ExternalGrant{
		Version: llm.ExternalGrantVersionAgentBound, KeyID: "key", Audience: "runtime", GrantID: "grant",
		RouteMode: llm.ExternalGrantRouteCoordinatorBound, OrganizationID: "org", RuntimeID: "runtime", AgentID: "agent",
		TenantID: "tenant", UserID: "user", SessionID: "session", LogicalRunID: "run", LogicalCallID: "call", AttemptNonce: "nonce",
		Provider: "provider", ProviderModelID: "model", ProviderConnectionID: "connection", ProviderConnectionGeneration: 1,
		RouteID: "route", CredentialBindingHandle: "opaque", CredentialAssetGeneration: 1, PolicyGeneration: 1, MaxOutputTokens: 100,
		Lease:    llm.ComputeLease{LeaseID: "lease", Epoch: 1, TokenUnits: 1000, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute), Signature: "fixture-signature",
	}
	request, err := credentials.NewRequest(grant)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := credentials.MarshalCanonicalRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := credentials.UnmarshalCanonicalRequest(raw)
	if err != nil || parsed.Grant.GrantID != grant.GrantID {
		t.Fatalf("parsed = %#v, %v", parsed, err)
	}
	response := credentials.Response{Version: credentials.Version, Provider: "provider", CredentialBindingHandle: "opaque", CredentialAssetGeneration: 1, ProviderConnectionGeneration: 1, Secret: "fixture-provider-secret", ExpiresAt: grant.ExpiresAt}
	body, err := credentials.MarshalCanonicalResponse(request, response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.ParseCanonicalResponse(request, body); err != nil {
		t.Fatal(err)
	}
}
