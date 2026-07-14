package auth

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// discovery_allowance_ssrf_test.go — the "allowance ≠ SSRF bypass" guarantee: a
// runtime-GRANTED authorization-server origin whose hostname resolves to a
// PRIVATE address is STILL refused at dial time by the post-DNS backstop. The
// allowance opens the cross-origin walk; it never opens the private-network
// dial.

// TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial walks a real
// production discoverer (no test-only private-network bypass) whose
// protected-resource hop advertises an authorization server the caller has
// EXPLICITLY granted, but whose hostname resolves to a private IP. The AS hop
// must fail the dial (never resolve an AS entry), proving the grant is not a
// hole.
func TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial(t *testing.T) {
	// The protected-resource fixture is bound to loopback and advertises a
	// granted AS origin. ServerURL uses the loopback IP literal, so the
	// same-origin PR hop dials it directly (no DNS) and is allowed by the
	// same-origin pin; the cross-origin AS hop below goes through DNS.
	prBody := []byte(`{"resource":"x","authorization_servers":["https://as.granted.example"]}`)
	srv := httptest.NewServer(&recordingHandler{routes: map[string][]byte{prWellKnown: prBody}})
	defer srv.Close()

	// The fake resolver maps every hostname (incl. as.granted.example) to a
	// PRIVATE address, so the granted cross-origin AS dial resolves private.
	res := newFakeResolver(t, "10.0.0.5")
	d := NewDiscoverer(WithDiscoveryTimeout(2*time.Second), WithResolverForTest(res))

	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           srv.URL,
		ResourceMetadataURL: srv.URL + prWellKnown,
		Source:              "probe",
		// The AS origin is EXPLICITLY granted — the allowance is present.
		AllowedOrigins: []string{"https://as.granted.example"},
	})

	// The grant let the walk ATTEMPT the AS hop (not a needs_allowance halt), but
	// the private resolved address is refused at dial — no AS entry resolves.
	if len(req.AuthorizationServers) != 0 {
		t.Fatalf("granted private-resolving AS must not resolve: %+v", req.AuthorizationServers)
	}
	var asHop *DiscoveryStepStatus
	for i := range req.Status {
		if req.Status[i].Step == StepAuthorizationServer {
			asHop = &req.Status[i]
		}
	}
	if asHop == nil {
		t.Fatalf("expected an authorization-server hop status; got %+v", req.Status)
	}
	if asHop.OK {
		t.Fatalf("granted private-resolving AS hop must not succeed: %+v", asHop)
	}
	if asHop.Reason == ReasonNeedsAllowance {
		t.Fatalf("AS hop was granted — refusal must be the private-dial backstop, not needs_allowance: %+v", asHop)
	}
}
