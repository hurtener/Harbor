package integration_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// Carrier-identity helpers for the end-to-end wire tests in the external
// test package. Mirrors carrier_identity_test.go for the in-package
// half; the two test packages cannot share a symbol.
//
// The Protocol mux establishes every request's identity before any
// handler runs: with a validator wired that is the bearer token, and on
// the bearer-less opt-in these stacks use it is the X-Harbor-* carrier
// headers. Nothing downstream reads the request body for authority, so a
// wire test must supply one carrier or the other.

// postCarrierJSON POSTs body to url with the identity-carrier headers
// set. A component left empty is sent empty, which the transport refuses
// — the shape the identity-mandatory tests rely on.
func postCarrierJSON(url string, body []byte, tenant, user, session string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body)) //nolint:noctx // end-to-end wire fixture; the test's own deadline bounds the run.
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Harbor-Tenant", tenant)
	req.Header.Set("X-Harbor-User", user)
	req.Header.Set("X-Harbor-Session", session)
	return http.DefaultClient.Do(req)
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
