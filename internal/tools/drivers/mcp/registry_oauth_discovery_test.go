package mcp

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools/auth"
)

func newDiscoveryRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider:                     &stubProvider{id: "auth-server", toolNames: []string{"call"}},
		Transport:                    "streamable-http",
		URLOrCommand:                 "https://mcp.example.com/rpc",
		InitialState:                 ServerStateOnline,
		OAuthDiscoveryAllowedOrigins: []string{"https://as.example.net"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r
}

func TestRegistry_RecordAuthChallenge_And_DiscoveryTarget(t *testing.T) {
	r := newDiscoveryRegistry(t)
	r.RecordAuthChallenge("auth-server", AuthChallenge{
		Scheme:              "Bearer",
		ResourceMetadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource",
		CapturedAt:          time.Unix(1700000000, 0),
	})
	ch, serverURL, origins, err := r.OAuthDiscoveryTarget("auth-server")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if ch == nil || ch.ResourceMetadataURL != "https://mcp.example.com/.well-known/oauth-protected-resource" {
		t.Fatalf("challenge = %+v", ch)
	}
	if serverURL != "https://mcp.example.com/rpc" {
		t.Errorf("serverURL = %q", serverURL)
	}
	if len(origins) != 1 || origins[0] != "https://as.example.net" {
		t.Errorf("origins = %v", origins)
	}
}

func TestRegistry_RecordAuthChallenge_UnknownServer_NoOp(t *testing.T) {
	r := newDiscoveryRegistry(t)
	// Must not panic; unknown server is a best-effort no-op.
	r.RecordAuthChallenge("missing", AuthChallenge{Scheme: "Bearer"})
}

func TestRegistry_OAuthDiscoveryTarget_UnknownServer(t *testing.T) {
	r := newDiscoveryRegistry(t)
	_, _, _, err := r.OAuthDiscoveryTarget("missing")
	if !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("err = %v, want ErrServerNotFound", err)
	}
}

func TestRegistry_RecordOAuthRequirement_ProjectedOnGet(t *testing.T) {
	r := newDiscoveryRegistry(t)
	req := &auth.OAuthRequirement{
		ResourceMetadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource",
		Source:              "probe",
		DiscoveredAt:        time.Unix(1700000000, 0),
		AuthorizationServers: []auth.AuthorizationServerMeta{{
			Issuer:                "https://as.example.net",
			AuthorizationEndpoint: "https://as.example.net/authorize",
			TokenEndpoint:         "https://as.example.net/token",
		}},
	}
	if err := r.RecordOAuthRequirement("auth-server", req); err != nil {
		t.Fatalf("record: %v", err)
	}
	// The requirement rides the DETAIL read (GetServer), NOT the list row.
	v, err := r.GetServer(idCtx(t), "auth-server")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.OAuthRequirement == nil || len(v.OAuthRequirement.AuthorizationServers) != 1 {
		t.Fatalf("requirement not projected on get: %+v", v.OAuthRequirement)
	}

	views, _, err := r.ListServers(idCtx(t), ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, lv := range views {
		if lv.Name == "auth-server" && lv.OAuthRequirement != nil {
			t.Errorf("requirement leaked onto the hot list row — it must ride detail only")
		}
	}
}

func TestRegistry_RecordOAuthRequirement_UnknownServer(t *testing.T) {
	r := newDiscoveryRegistry(t)
	if err := r.RecordOAuthRequirement("missing", &auth.OAuthRequirement{}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("err = %v, want ErrServerNotFound", err)
	}
}

// Concurrent record + read against one registry under -race (D-025).
func TestRegistry_OAuthDiscovery_ConcurrentReuse(t *testing.T) {
	r := newDiscoveryRegistry(t)
	r.RecordAuthChallenge("auth-server", AuthChallenge{Scheme: "Bearer", ResourceMetadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource"})
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for range n {
		go func() {
			defer wg.Done()
			_ = r.RecordOAuthRequirement("auth-server", &auth.OAuthRequirement{Source: "probe"})
		}()
		go func() {
			defer wg.Done()
			_, _, _, _ = r.OAuthDiscoveryTarget("auth-server")
			_, _ = r.GetServer(idCtx(t), "auth-server")
		}()
	}
	wg.Wait()
}
