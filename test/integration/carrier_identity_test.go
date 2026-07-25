package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// Carrier-identity helpers for the end-to-end wire tests.
//
// The Protocol mux establishes every request's identity before any
// handler runs: with a validator wired that is the bearer token, and on
// the bearer-less opt-in these end-to-end stacks use it is the
// X-Harbor-* carrier headers. Nothing downstream reads the request body
// for authority, so a wire test must supply one carrier or the other.
//
// These helpers supply the carrier headers. Tests that need a scope
// claim as well wrap the mux in a fixture middleware instead — see
// withVerifiedIdentity in artifacts_page_test.go.

// carrierHeaders are the identity-carrier header names. They mirror the
// transport's exported constants; the literals keep this test helper
// free of a transport import it does not otherwise need.
const (
	carrierTenantHeader  = "X-Harbor-Tenant"
	carrierUserHeader    = "X-Harbor-User"
	carrierSessionHeader = "X-Harbor-Session"
)

// postCarrierJSON POSTs body to url with the identity-carrier headers
// set. A component left empty is sent empty, which the transport refuses
// — the shape the identity-mandatory tests rely on.
func postCarrierJSON(url string, body []byte, tenant, user, session string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body)) //nolint:noctx // end-to-end wire fixture; the test's own deadline bounds the run.
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(carrierTenantHeader, tenant)
	req.Header.Set(carrierUserHeader, user)
	req.Header.Set(carrierSessionHeader, session)
	return http.DefaultClient.Do(req)
}

// carrierFromPayload reads the identity triple a request payload names,
// so a wire test can drive the request AS the identity its body scopes
// to. It looks under "scope" (the artifacts shape) then "identity" (the
// shape every other Protocol request carries).
//
// A body that deliberately names a foreign identity is not driven this
// way: those tests wrap the mux so the caller's identity is established
// independently of what the body asks for.
func carrierFromPayload(payload any) (tenant, user, session string) {
	m, ok := payload.(map[string]any)
	if !ok {
		return "", "", ""
	}
	for _, key := range []string{"scope", "identity"} {
		inner, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		tenant, _ = inner["tenant"].(string)
		user, _ = inner["user"].(string)
		session, _ = inner["session"].(string)
		return tenant, user, session
	}
	return "", "", ""
}

// verifiedDiscoveryCtx is the established-identity context for the MCP
// discovery end-to-end tests, matching the triple their request bodies
// carry. The surface gate reconciles the body against it.
func verifiedDiscoveryCtx(t *testing.T) context.Context {
	t.Helper()
	return mustVerifiedCtx(t, identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"})
}

// verifiedMCPPageCtx is the established-identity context for the MCP
// connections page end-to-end tests.
func verifiedMCPPageCtx(t *testing.T) context.Context {
	t.Helper()
	return mustVerifiedCtx(t, identity.Identity{
		TenantID: mcpPageTenant, UserID: mcpPageUser, SessionID: mcpPageSession,
	})
}

// mustVerifiedCtx seats id as the request's established identity — the
// shape a transport hands a surface once the request's identity has been
// resolved.
func mustVerifiedCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return ctx
}
