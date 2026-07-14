package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools/auth"
)

// recordedRequirement builds a discovered requirement whose single
// authorization-server entry was fetched from as.example.net (the SourceURL
// origin the revoke-prune matches against the allow-set).
func recordedRequirement() *auth.OAuthRequirement {
	return &auth.OAuthRequirement{
		ResourceMetadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource",
		Source:              "probe",
		DiscoveredAt:        time.Unix(1700000000, 0),
		AuthorizationServers: []auth.AuthorizationServerMeta{{
			Issuer:                "https://as.example.net",
			AuthorizationEndpoint: "https://as.example.net/authorize",
			TokenEndpoint:         "https://as.example.net/token",
			SourceURL:             "https://as.example.net/.well-known/oauth-authorization-server",
		}},
	}
}

func TestRegistry_SetOAuthDiscoveryOrigins_LiveAndReturnsPrior(t *testing.T) {
	r := newDiscoveryRegistry(t) // registered with allowance https://as.example.net
	prev, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", []string{"https://as2.example.net"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(prev) != 1 || prev[0] != "https://as.example.net" {
		t.Fatalf("prev = %v, want [https://as.example.net]", prev)
	}
	_, _, origins, err := r.OAuthDiscoveryTarget("auth-server")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if len(origins) != 1 || origins[0] != "https://as2.example.net" {
		t.Fatalf("live origins = %v, want [https://as2.example.net]", origins)
	}
}

func TestRegistry_SetOAuthDiscoveryOrigins_IdentityMissing(t *testing.T) {
	r := newDiscoveryRegistry(t)
	if _, err := r.SetOAuthDiscoveryOrigins(context.Background(), "auth-server", nil); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("err = %v, want ErrRegistryIdentityMissing", err)
	}
}

func TestRegistry_SetOAuthDiscoveryOrigins_UnknownServer(t *testing.T) {
	r := newDiscoveryRegistry(t)
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "missing", nil); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("err = %v, want ErrServerNotFound", err)
	}
}

func TestSetMCPDiscoveryOrigins_RevokePrunesRecordedRequirement(t *testing.T) {
	r := newDiscoveryRegistry(t)
	if err := r.RecordOAuthRequirement("auth-server", recordedRequirement()); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Revoke every origin — the as.example.net-sourced AS entry must be pruned.
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	v, err := r.GetServer(idCtx(t), "auth-server")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.OAuthRequirement == nil {
		t.Fatalf("requirement pointer dropped entirely; want a fresh requirement with 0 AS entries")
	}
	if len(v.OAuthRequirement.AuthorizationServers) != 0 {
		t.Fatalf("AS entries = %d, want 0 (pruned)", len(v.OAuthRequirement.AuthorizationServers))
	}
}

func TestSetMCPDiscoveryOrigins_RevokePrune_KeepsStillAllowed(t *testing.T) {
	r := newDiscoveryRegistry(t)
	if err := r.RecordOAuthRequirement("auth-server", recordedRequirement()); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Re-grant the SAME origin (port-exact) — nothing is pruned.
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", []string{"https://as.example.net"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, err := r.GetServer(idCtx(t), "auth-server")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.OAuthRequirement == nil || len(v.OAuthRequirement.AuthorizationServers) != 1 {
		t.Fatalf("AS entries = %+v, want 1 retained", v.OAuthRequirement)
	}
}

// TestSetMCPDiscoveryOrigins_RevokePrune_NoPointerRace interleaves the
// pointer-swapping revoke-prune with concurrent detail reads that dereference
// the handed-out requirement pointer. Under -race this fails if the prune
// mutated the pointee in place instead of swapping a fresh pointer.
func TestSetMCPDiscoveryOrigins_RevokePrune_NoPointerRace(t *testing.T) {
	r := newDiscoveryRegistry(t)
	if err := r.RecordOAuthRequirement("auth-server", recordedRequirement()); err != nil {
		t.Fatalf("record: %v", err)
	}
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := range n {
		grant := i%2 == 0
		go func() {
			defer wg.Done()
			if grant {
				_, _ = r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", []string{"https://as.example.net"})
			} else {
				_, _ = r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", nil)
			}
		}()
		go func() {
			defer wg.Done()
			v, err := r.GetServer(idCtx(t), "auth-server")
			if err == nil && v.OAuthRequirement != nil {
				// Dereference the handed-out pointer concurrently with the swap.
				_ = len(v.OAuthRequirement.AuthorizationServers)
			}
		}()
	}
	wg.Wait()
}

// TestRegistry_SetOAuthDiscoveryOrigins_ConcurrentReuse pins the D-025 contract:
// N≥100 concurrent writes / reads / walks / prunes against ONE shared registry
// under -race, no torn slice, no pointer race, no goroutine leak.
func TestRegistry_SetOAuthDiscoveryOrigins_ConcurrentReuse(t *testing.T) {
	r := newDiscoveryRegistry(t)
	r.RecordAuthChallenge("auth-server", AuthChallenge{Scheme: "Bearer", ResourceMetadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource"})
	const n = 128
	var wg sync.WaitGroup
	wg.Add(n * 3)
	for i := range n {
		origins := []string{"https://as.example.net"}
		if i%3 == 0 {
			origins = nil
		}
		go func() {
			defer wg.Done()
			_, _ = r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", origins)
		}()
		go func() {
			defer wg.Done()
			_ = r.RecordOAuthRequirement("auth-server", recordedRequirement())
		}()
		go func() {
			defer wg.Done()
			_, _, _, _ = r.OAuthDiscoveryTarget("auth-server")
			_, _ = r.GetServer(idCtx(t), "auth-server")
		}()
	}
	wg.Wait()
}
