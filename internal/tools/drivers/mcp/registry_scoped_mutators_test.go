package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// registry_scoped_mutators_test.go — phase 211 (D-355): the sibling connection
// WRITES resolve under the caller's own scope rather than by bare name.
//
// SetRawHTMLTrust is reachable from the wire, where the caller's verified
// identity carries a tenant and no agent id, so it is TENANT-scoped.
// Deregister is not wire-reachable and both its callers hold the (tenant,
// agent) owner, so it is OWNER-scoped. Reads stay bare-name (D-287 / D-301).

// idCtxForTenant returns a ctx carrying a complete triple under the named
// tenant — the identity the tenant-scoped write resolves against.
func idCtxForTenant(t *testing.T, tenant string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID:  tenant,
		UserID:    "u-1",
		SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// newTenantScopedRegistry registers ONE runtime-added server owned by
// (tenant-a, agent-a) plus ONE boot-declared (zero-owner) server.
func newTenantScopedRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider:     &stubProvider{id: "owned-srv", toolNames: []string{"call"}},
		Transport:    "streamable-http",
		URLOrCommand: "https://mcp.example.com/rpc",
		InitialState: ServerStateOnline,
		Owner:        ownerA(),
	}); err != nil {
		t.Fatalf("register owned: %v", err)
	}
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider:     &stubProvider{id: "boot-srv", toolNames: []string{"call"}},
		Transport:    "streamable-http",
		URLOrCommand: "https://boot.example.com/rpc",
		InitialState: ServerStateOnline,
		// No Owner — boot-declared, deployment-global.
	}); err != nil {
		t.Fatalf("register boot: %v", err)
	}
	return r
}

// rawHTMLTrustOf reads the live flag through the read projection (bare-name and
// owner-blind, as the reads stay).
func rawHTMLTrustOf(t *testing.T, r *Registry, name string) bool {
	t.Helper()
	v, err := r.GetServer(idCtx(t), name)
	if err != nil {
		t.Fatalf("GetServer(%q): %v", name, err)
	}
	return v.RawHTMLTrusted
}

// TestRegistry_SetRawHTMLTrust_TenantScoped proves the sandbox-posture write
// lands on a registration the caller's own tenant owns: the owning tenant
// succeeds, and a caller from a DIFFERENT tenant presenting the same bare name
// gets the same answer it would get for a name nobody registered, with the live
// flag untouched.
func TestRegistry_SetRawHTMLTrust_TenantScoped(t *testing.T) {
	r := newTenantScopedRegistry(t) // owned-srv belongs to tenant-a

	if _, err := r.SetRawHTMLTrust(idCtxForTenant(t, "tenant-b"), "owned-srv", true); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("cross-tenant write: err = %v, want ErrServerNotFound", err)
	}
	if got := rawHTMLTrustOf(t, r, "owned-srv"); got {
		t.Fatalf("raw-HTML trust after a cross-tenant write = %v, want false (untouched)", got)
	}

	prev, err := r.SetRawHTMLTrust(idCtxForTenant(t, "tenant-a"), "owned-srv", true)
	if err != nil {
		t.Fatalf("owning-tenant write: %v", err)
	}
	if prev {
		t.Fatalf("prev = %v, want false", prev)
	}
	if got := rawHTMLTrustOf(t, r, "owned-srv"); !got {
		t.Fatalf("raw-HTML trust after the owning tenant's write = %v, want true", got)
	}
}

// TestRegistry_SetRawHTMLTrust_UnknownAndOtherTenantAnswerAlike proves the
// refusal never discloses WHICH case applied: an unregistered name and another
// tenant's registration both answer ErrServerNotFound (§6 — existence is never
// revealed across identities).
func TestRegistry_SetRawHTMLTrust_UnknownAndOtherTenantAnswerAlike(t *testing.T) {
	r := newTenantScopedRegistry(t)
	ctx := idCtxForTenant(t, "tenant-b")

	_, absent := r.SetRawHTMLTrust(ctx, "nobody-registered-this", true)
	_, foreign := r.SetRawHTMLTrust(ctx, "owned-srv", true)
	if !errors.Is(absent, ErrServerNotFound) || !errors.Is(foreign, ErrServerNotFound) {
		t.Fatalf("absent = %v, foreign = %v; want both ErrServerNotFound", absent, foreign)
	}
}

// TestRegistry_SetRawHTMLTrust_BootDeclaredStaysWritable pins the bounded
// guarantee this write makes. A boot-declared server is deployment-global
// infrastructure declared in the deployment's own configuration; its per-server
// admin preferences have no per-owner home and no other door that can set them,
// so refusing the write would delete the preference rather than scope it. The
// guarantee is therefore "the caller's own tenant, or the deployment's own boot
// state" — never another tenant's runtime-added registration.
func TestRegistry_SetRawHTMLTrust_BootDeclaredStaysWritable(t *testing.T) {
	r := newTenantScopedRegistry(t)
	if _, err := r.SetRawHTMLTrust(idCtxForTenant(t, "tenant-b"), "boot-srv", true); err != nil {
		t.Fatalf("boot-declared write: %v", err)
	}
	if got := rawHTMLTrustOf(t, r, "boot-srv"); !got {
		t.Fatalf("boot-declared raw-HTML trust = %v, want true", got)
	}
}

// TestRegistry_SetRawHTMLTrust_IdentityMissing keeps identity mandatory: a ctx
// with no triple is refused before any resolution (§6 rule 9).
func TestRegistry_SetRawHTMLTrust_IdentityMissing(t *testing.T) {
	r := newTenantScopedRegistry(t)
	if _, err := r.SetRawHTMLTrust(context.Background(), "boot-srv", true); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("err = %v, want ErrRegistryIdentityMissing", err)
	}
	if got := rawHTMLTrustOf(t, r, "boot-srv"); got {
		t.Fatalf("identity-less write changed the flag to %v", got)
	}
}

// TestRegistry_TenantEntry_EmptyTenantResolvesNothing pins the fail-closed
// default at the resolution choke point itself. Today requireIdentity refuses an
// identity-less caller before the resolver is reached, so this guard is
// dead-defensive on every LIVE path — which is exactly why it is pinned here
// rather than left to the callers: a future caller that resolves without a
// tenant must not fall back to the whole registry (the mirror of
// [Registry.ownedEntry]'s zero-owner refusal and RuntimeAddedSources' zero-owner
// nil). Called directly because the guard is unreachable through the exported
// surface, and an unreachable guard nobody tests is how an inert one survives.
func TestRegistry_TenantEntry_EmptyTenantResolvesNothing(t *testing.T) {
	r := newTenantScopedRegistry(t)
	for _, name := range []string{"boot-srv", "owned-srv"} {
		if _, err := r.tenantEntry(name, ""); !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("tenantEntry(%q, \"\") = %v, want ErrServerNotFound", name, err)
		}
	}
	// A present tenant still resolves what it may write, so the guard is a
	// refusal and not a blanket denial.
	if _, err := r.tenantEntry("owned-srv", "tenant-a"); err != nil {
		t.Fatalf("tenantEntry(owned-srv, tenant-a): %v", err)
	}
}

// TestRegistry_SetRawHTMLTrust_CompensatingRevertResolvesSymmetrically pins the
// admin-write compensation contract at the registry boundary: the revert leg is
// the SAME call on the SAME ctx, so whenever the apply resolved the revert
// resolves too and restores the prior value exactly. An asymmetric revert would
// leave the toggle observably applied but unrecorded.
func TestRegistry_SetRawHTMLTrust_CompensatingRevertResolvesSymmetrically(t *testing.T) {
	r := newTenantScopedRegistry(t)
	ctx := idCtxForTenant(t, "tenant-a")

	prev, err := r.SetRawHTMLTrust(ctx, "owned-srv", true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The compensating revert the admin-write helper runs on an audit-emit
	// failure: same ctx, prior value.
	if _, rerr := r.SetRawHTMLTrust(ctx, "owned-srv", prev); rerr != nil {
		t.Fatalf("compensating revert: %v", rerr)
	}
	if got := rawHTMLTrustOf(t, r, "owned-srv"); got != prev {
		t.Fatalf("post-revert flag = %v, want the prior %v", got, prev)
	}

	// The refused case never reaches a revert at all: the apply itself fails,
	// so there is no half-applied state to compensate for.
	if _, err := r.SetRawHTMLTrust(idCtxForTenant(t, "tenant-b"), "owned-srv", true); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("cross-tenant apply: err = %v, want ErrServerNotFound", err)
	}
}

// TestRegistry_SetRawHTMLTrust_ConcurrentTenants pins the tenant scope under
// contention (D-025 + §6 rule 10): N goroutines per tenant hammer ONE shared
// registry. Every write that lands is the owning tenant's, so the terminal flag
// is always the owning tenant's chosen value and the other tenant's writes are
// refused without ever touching it.
func TestRegistry_SetRawHTMLTrust_ConcurrentTenants(t *testing.T) {
	r := newTenantScopedRegistry(t)
	const n = 128
	ownCtx := idCtxForTenant(t, "tenant-a")
	otherCtx := idCtxForTenant(t, "tenant-b")

	var wg sync.WaitGroup
	wg.Add(n * 2)
	for range n {
		go func() {
			defer wg.Done()
			if _, err := r.SetRawHTMLTrust(ownCtx, "owned-srv", true); err != nil {
				t.Errorf("owning-tenant write: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := r.SetRawHTMLTrust(otherCtx, "owned-srv", false); !errors.Is(err, ErrServerNotFound) {
				t.Errorf("cross-tenant write: err = %v, want ErrServerNotFound", err)
			}
		}()
	}
	wg.Wait()

	if got := rawHTMLTrustOf(t, r, "owned-srv"); !got {
		t.Fatalf("terminal flag = %v, want the owning tenant's true — a cross-tenant write landed", got)
	}
}

// TestRegistry_Deregister_OwnerScoped proves removal lands only on the
// registration carrying the caller's own (tenant, agent) tag: another owner's
// removal is refused with ErrServerNotFound, the entry survives and its
// transport is never closed; the owning owner's removal succeeds.
func TestRegistry_Deregister_OwnerScoped(t *testing.T) {
	r := NewRegistry()
	prov := &stubProvider{id: "owned-srv", toolNames: []string{"call"}}
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider: prov, Transport: "stdio", InitialState: ServerStateOnline, Owner: ownerA(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := r.Deregister(idCtx(t), "owned-srv", ownerB()); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("cross-owner deregister: err = %v, want ErrServerNotFound", err)
	}
	if got := r.SourceIDs(); len(got) != 1 {
		t.Fatalf("SourceIDs after a cross-owner deregister = %v, want the entry retained", got)
	}
	prov.mu.Lock()
	closed := prov.closed
	prov.mu.Unlock()
	if closed != 0 {
		t.Fatalf("cross-owner deregister closed the transport %d times, want 0", closed)
	}

	if err := r.Deregister(idCtx(t), "owned-srv", ownerA()); err != nil {
		t.Fatalf("owning deregister: %v", err)
	}
	if got := r.SourceIDs(); len(got) != 0 {
		t.Fatalf("SourceIDs after the owning deregister = %v, want empty", got)
	}
	prov.mu.Lock()
	closed = prov.closed
	prov.mu.Unlock()
	if closed != 1 {
		t.Fatalf("provider Close called %d times, want 1", closed)
	}
}

// TestRegistry_Deregister_ZeroOwnerMatchesOnlyBootDeclared pins the exact-match
// semantics: the ZERO owner is the boot loader's own tag, so it removes a
// boot-declared registration (the same-name hot-reload replace) and NOTHING
// else — a runtime-added entry is untouched by an owner-less removal.
func TestRegistry_Deregister_ZeroOwnerMatchesOnlyBootDeclared(t *testing.T) {
	r := newTenantScopedRegistry(t)

	if err := r.Deregister(idCtx(t), "owned-srv", auth.Owner{}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("zero-owner deregister of a runtime-added entry: err = %v, want ErrServerNotFound", err)
	}
	if err := r.Deregister(idCtx(t), "boot-srv", auth.Owner{}); err != nil {
		t.Fatalf("zero-owner deregister of a boot-declared entry: %v", err)
	}
	ids := r.SourceIDs()
	if len(ids) != 1 || ids[0] != "owned-srv" {
		t.Fatalf("SourceIDs = %v, want only the retained [owned-srv]", ids)
	}
	// A half owner is neither the boot tag nor the runtime tag.
	if err := r.Deregister(idCtx(t), "owned-srv", auth.Owner{Tenant: "tenant-a"}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("half-owner deregister: err = %v, want ErrServerNotFound", err)
	}
}

// TestRegistry_Deregister_ConcurrentOwners hammers ONE shared registry with
// N≥128 removals per owner (D-025). Exactly one removal can win the entry, it
// must be the OWNING owner's, and the transport closes exactly once — the owner
// comparison runs under the same write lock as the delete, so a cross-owner
// removal can never slip between a resolve and a delete.
func TestRegistry_Deregister_ConcurrentOwners(t *testing.T) {
	r := NewRegistry()
	prov := &stubProvider{id: "owned-srv", toolNames: []string{"call"}}
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider: prov, Transport: "stdio", InitialState: ServerStateOnline, Owner: ownerA(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	const n = 128
	var wg sync.WaitGroup
	var mu sync.Mutex
	var owningWins int
	wg.Add(n * 2)
	for range n {
		go func() {
			defer wg.Done()
			err := r.Deregister(idCtx(t), "owned-srv", ownerA())
			switch {
			case err == nil:
				mu.Lock()
				owningWins++
				mu.Unlock()
			case errors.Is(err, ErrServerNotFound):
				// Another goroutine already removed it — idempotent.
			default:
				t.Errorf("owning deregister: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := r.Deregister(idCtx(t), "owned-srv", ownerB()); !errors.Is(err, ErrServerNotFound) {
				t.Errorf("cross-owner deregister: err = %v, want ErrServerNotFound", err)
			}
		}()
	}
	wg.Wait()

	if owningWins != 1 {
		t.Fatalf("owning removals that landed = %d, want exactly 1", owningWins)
	}
	prov.mu.Lock()
	closed := prov.closed
	prov.mu.Unlock()
	if closed != 1 {
		t.Fatalf("provider Close called %d times, want exactly 1", closed)
	}
}

// TestRegistry_ReadsStayBareName re-affirms D-287 / D-301: this phase scopes
// the WRITES only. Every read projection still resolves a registration owned by
// a different tenant, and a boot-declared one, by bare name from any session.
func TestRegistry_ReadsStayBareName(t *testing.T) {
	r := newTenantScopedRegistry(t)
	foreign := idCtxForTenant(t, "tenant-b")

	for _, name := range []string{"owned-srv", "boot-srv"} {
		if _, err := r.GetServer(foreign, name); err != nil {
			t.Errorf("GetServer(%q) from another tenant: %v", name, err)
		}
		if _, err := r.ListResources(foreign, name); err != nil {
			t.Errorf("ListResources(%q) from another tenant: %v", name, err)
		}
		if _, err := r.Health(foreign, name, 0); err != nil {
			t.Errorf("Health(%q) from another tenant: %v", name, err)
		}
		// RefreshDiscovery and Probe are classified as READS: they record only
		// what their own round-trip observed, so bare-name resolution stays.
		if _, err := r.RefreshDiscovery(foreign, name); err != nil {
			t.Errorf("RefreshDiscovery(%q) from another tenant: %v", name, err)
		}
		if _, err := r.Probe(foreign, name); err != nil {
			t.Errorf("Probe(%q) from another tenant: %v", name, err)
		}
	}
	rows, _, err := r.ListServers(foreign, ListFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListServers from another tenant = %d rows, err %v; want 2 rows", len(rows), err)
	}
}
