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
	prev, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerA(), []string{"https://as2.example.net"})
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
	if _, err := r.SetOAuthDiscoveryOrigins(context.Background(), "auth-server", ownerA(), nil); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("err = %v, want ErrRegistryIdentityMissing", err)
	}
}

func TestRegistry_SetOAuthDiscoveryOrigins_UnknownServer(t *testing.T) {
	r := newDiscoveryRegistry(t)
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "missing", ownerA(), nil); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("err = %v, want ErrServerNotFound", err)
	}
}

// TestRegistry_SetOAuthDiscoveryOrigins_OwnerScoped proves the allowance write
// lands on the CALLER'S OWN registration: the owning (tenant, agent) succeeds,
// and a different owner presenting the same bare name gets the same answer it
// would get for a name nobody registered (ErrServerNotFound) with the live
// allow-list untouched.
func TestRegistry_SetOAuthDiscoveryOrigins_OwnerScoped(t *testing.T) {
	r := newDiscoveryRegistry(t) // registered to ownerA with https://as.example.net

	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerB(), []string{"https://other-owner.example.net"}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("write by a non-owning owner: err = %v, want ErrServerNotFound", err)
	}
	_, _, origins, err := r.OAuthDiscoveryTarget("auth-server")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if len(origins) != 1 || origins[0] != "https://as.example.net" {
		t.Fatalf("live origins after a non-owner write = %v, want the owner's [https://as.example.net]", origins)
	}

	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerA(), []string{"https://as2.example.net"}); err != nil {
		t.Fatalf("write by the owning owner: %v", err)
	}
	_, _, origins, err = r.OAuthDiscoveryTarget("auth-server")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if len(origins) != 1 || origins[0] != "https://as2.example.net" {
		t.Fatalf("live origins after the owner's write = %v, want [https://as2.example.net]", origins)
	}
}

// TestRegistry_SetOAuthDiscoveryOrigins_ZeroOwnerOwnsNothing proves the ZERO
// owner resolves to no registration at all — it never matches the boot-declared
// (zero-owner) entries a bare equality check would hand it. The guard lives at
// the single resolution choke point, so a caller that omits its owner is
// refused rather than silently scoped onto boot state.
func TestRegistry_SetOAuthDiscoveryOrigins_ZeroOwnerOwnsNothing(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider:                     &stubProvider{id: "boot-srv", toolNames: []string{"call"}},
		Transport:                    "streamable-http",
		URLOrCommand:                 "https://boot.example.com/rpc",
		InitialState:                 ServerStateOnline,
		OAuthDiscoveryAllowedOrigins: []string{"https://boot-as.example.net"},
		// No Owner — a boot-declared registration carrying the zero owner.
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "boot-srv", auth.Owner{}, []string{"https://zero-owner.example.net"}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("zero-owner write against a zero-owner entry: err = %v, want ErrServerNotFound", err)
	}
	_, _, origins, err := r.OAuthDiscoveryTarget("boot-srv")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if len(origins) != 1 || origins[0] != "https://boot-as.example.net" {
		t.Fatalf("boot allow-list = %v, want the boot-declared [https://boot-as.example.net]", origins)
	}
	// A HALF owner is not the zero owner, and matches neither a boot entry nor a
	// fully-tagged one.
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "boot-srv", auth.Owner{Tenant: "tenant-a"}, nil); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("half-owner write: err = %v, want ErrServerNotFound", err)
	}
}

// TestRegistry_SetOAuthDiscoveryOrigins_BootDeclaredIsOwnerScopedOut proves an
// allowance write never reaches a boot-declared (zero-owner) registration: the
// deployment-wide server's allow-list is boot state, and a runtime owner's
// write resolves to no registration of its own.
func TestRegistry_SetOAuthDiscoveryOrigins_BootDeclaredIsOwnerScopedOut(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider:                     &stubProvider{id: "boot-srv", toolNames: []string{"call"}},
		Transport:                    "streamable-http",
		URLOrCommand:                 "https://boot.example.com/rpc",
		InitialState:                 ServerStateOnline,
		OAuthDiscoveryAllowedOrigins: []string{"https://boot-as.example.net"},
		// No Owner — a boot-declared registration.
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "boot-srv", ownerA(), []string{"https://runtime-chosen.example.net"}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("runtime-owner write against a boot-declared name: err = %v, want ErrServerNotFound", err)
	}
	_, _, origins, err := r.OAuthDiscoveryTarget("boot-srv")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if len(origins) != 1 || origins[0] != "https://boot-as.example.net" {
		t.Fatalf("boot allow-list = %v, want the boot-declared [https://boot-as.example.net]", origins)
	}
}

// TestRegistry_SetOAuthDiscoveryOrigins_ConcurrentOwners pins the owner scope
// under contention (D-025 + §6 rule 10): N goroutines per owner hammer ONE
// shared registry with disjoint allow-lists; every write that lands is the
// owner's own, so the terminal live allow-list is always one of ownerA's values
// and never carries ownerB's origin.
func TestRegistry_SetOAuthDiscoveryOrigins_ConcurrentOwners(t *testing.T) {
	r := newDiscoveryRegistry(t) // owned by ownerA
	const n = 128
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := range n {
		aOrigins := []string{"https://as.example.net"}
		if i%2 == 0 {
			aOrigins = []string{"https://as2.example.net"}
		}
		go func() {
			defer wg.Done()
			if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerA(), aOrigins); err != nil {
				t.Errorf("owner write: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerB(), []string{"https://other-owner.example.net"}); !errors.Is(err, ErrServerNotFound) {
				t.Errorf("non-owner write: err = %v, want ErrServerNotFound", err)
			}
		}()
	}
	wg.Wait()
	_, _, origins, err := r.OAuthDiscoveryTarget("auth-server")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	for _, o := range origins {
		if o == "https://other-owner.example.net" {
			t.Fatalf("live allow-list = %v, want only the owning owner's origins", origins)
		}
	}
}

func TestSetMCPDiscoveryOrigins_RevokePrunesRecordedRequirement(t *testing.T) {
	r := newDiscoveryRegistry(t)
	if err := r.RecordOAuthRequirement("auth-server", recordedRequirement()); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Revoke every origin — the as.example.net-sourced AS entry must be pruned.
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerA(), nil); err != nil {
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
	if _, err := r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerA(), []string{"https://as.example.net"}); err != nil {
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
				_, _ = r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerA(), []string{"https://as.example.net"})
			} else {
				_, _ = r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerA(), nil)
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
			_, _ = r.SetOAuthDiscoveryOrigins(idCtx(t), "auth-server", ownerA(), origins)
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
