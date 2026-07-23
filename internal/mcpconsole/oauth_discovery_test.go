package mcpconsole_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestRegistryAccessor_ProbeTriggersDiscovery proves the D-297 wiring: a probe
// against a connection that captured a `WWW-Authenticate` challenge walks the
// RFC 9728 → RFC 8414 chain and records the verbatim requirement on the
// registry, which mcp.servers.get then projects. The probe row is untouched.
func TestRegistryAccessor_ProbeTriggersDiscovery(t *testing.T) {
	// AS metadata fixture server (loopback).
	asHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"issuer":"https://as.example.net",
			"authorization_endpoint":"https://as.example.net/authorize",
			"token_endpoint":"https://as.example.net/token",
			"scopes_supported":["openid","email"],
			"code_challenge_methods_supported":["S256"],
			"registration_endpoint":"https://as.example.net/register"
		}`))
	})
	asServer := httptest.NewServer(asHandler)
	defer asServer.Close()

	prHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"resource":              "https://mcp.example.com",
			"authorization_servers": []string{asServer.URL},
		})
		_, _ = w.Write(body)
	})
	prServer := httptest.NewServer(prHandler)
	defer prServer.Close()

	reg := mcp.NewRegistry()
	if err := reg.Register(context.Background(), mcp.ServerRegistration{
		Provider:                     &stubProvider{id: "auth-srv"},
		Transport:                    "streamable-http",
		URLOrCommand:                 prServer.URL,
		InitialState:                 mcp.ServerStateOnline,
		OAuthDiscoveryAllowedOrigins: []string{asServer.URL},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Simulate the transport edge having captured a challenge.
	reg.RecordAuthChallenge("auth-srv", mcp.AuthChallenge{
		Scheme:              "Bearer",
		ResourceMetadataURL: prServer.URL + "/.well-known/oauth-protected-resource",
		CapturedAt:          time.Now(),
	})

	acc, err := mcpconsole.NewRegistryAccessor(reg,
		mcpconsole.WithOAuthDiscoverer(auth.NewDiscoverer(auth.WithPrivateNetworkAccessForTest())))
	if err != nil {
		t.Fatalf("accessor: %v", err)
	}

	// Probe triggers discovery. The stub provider has no live transport, so the
	// probe itself may error — discovery still runs off the captured challenge.
	_, _ = acc.Probe(context.Background(), "auth-srv")

	row, err := acc.GetServer(idCtx(t), "auth-srv")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.OAuthRequirement == nil {
		t.Fatalf("get did not project the discovered requirement")
	}
	if len(row.OAuthRequirement.AuthorizationServers) != 1 {
		t.Fatalf("want 1 AS, got %d; status=%+v", len(row.OAuthRequirement.AuthorizationServers), row.OAuthRequirement.Status)
	}
	as := row.OAuthRequirement.AuthorizationServers[0]
	if as.TokenEndpoint != "https://as.example.net/token" {
		t.Errorf("token_endpoint = %q", as.TokenEndpoint)
	}
	if as.RegistrationEndpoint != "https://as.example.net/register" {
		t.Errorf("registration_endpoint = %q (reported, never invoked)", as.RegistrationEndpoint)
	}
	if len(as.CodeChallengeMethodsSupported) != 1 || as.CodeChallengeMethodsSupported[0] != "S256" {
		t.Errorf("pkce = %v", as.CodeChallengeMethodsSupported)
	}
	if row.OAuthRequirement.Source != "probe" {
		t.Errorf("source = %q, want probe", row.OAuthRequirement.Source)
	}
}

// Without a wired discoverer, the probe path stays discovery-free.
func TestRegistryAccessor_ProbeNoDiscoverer_NoRequirement(t *testing.T) {
	reg := mcp.NewRegistry()
	_ = reg.Register(context.Background(), mcp.ServerRegistration{
		Provider:     &stubProvider{id: "plain"},
		Transport:    "streamable-http",
		URLOrCommand: "https://mcp.example.com",
		InitialState: mcp.ServerStateOnline,
	})
	acc, _ := mcpconsole.NewRegistryAccessor(reg)
	_, _ = acc.Probe(context.Background(), "plain")
	row, err := acc.GetServer(idCtx(t), "plain")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.OAuthRequirement != nil {
		t.Fatalf("requirement present without a discoverer: %+v", row.OAuthRequirement)
	}
}
