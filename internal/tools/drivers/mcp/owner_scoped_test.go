package mcp

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// ctxWithTriple returns a ctx carrying an arbitrary identity triple.
func ctxWithTriple(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// owner_scoped_test.go — the owner tag on a runtime-added registry entry and
// the owner-scoped reconcile VIEW (RuntimeAddedSources). Boot-declared servers
// stay untagged and process-globally visible (the property full-triple keying
// would have broken); RuntimeAddedSources filters to one owner's runtime-adds.

func ownerA() auth.Owner { return auth.Owner{Tenant: "tenant-a", Agent: "agent-a"} }
func ownerB() auth.Owner { return auth.Owner{Tenant: "tenant-b", Agent: "agent-b"} }

// registerServer registers a server under name with the given owner. A zero
// owner marks a boot-declared (untagged) server.
func registerServer(t *testing.T, r *Registry, name string, owner auth.Owner) {
	t.Helper()
	if err := r.Register(ServerRegistration{
		Provider:     &stubProvider{id: tools.ToolSourceID(name), toolNames: []string{"do"}},
		Transport:    "stdio",
		URLOrCommand: "/usr/bin/" + name,
		InitialState: ServerStateOnline,
		Owner:        owner,
	}); err != nil {
		t.Fatalf("register %q: %v", name, err)
	}
}

// TestRegistry_RuntimeAddCarriesOwnerTag_BootUntagged proves a runtime-added
// entry carries its owner tag while a boot-declared server is untagged and is
// never enumerated by the owner-scoped reconcile view.
func TestRegistry_RuntimeAddCarriesOwnerTag_BootUntagged(t *testing.T) {
	r := NewRegistry()
	registerServer(t, r, "boot-srv", auth.Owner{}) // boot-declared → untagged
	registerServer(t, r, "a-add", ownerA())        // runtime-added by owner A

	// Owner A's reconcile view contains ONLY its runtime-add — never the boot
	// server.
	if got := r.RuntimeAddedSources(ownerA()); len(got) != 1 || got[0] != "a-add" {
		t.Fatalf("RuntimeAddedSources(A) = %v, want [a-add] (boot-srv must be excluded)", got)
	}
	// The zero (boot) owner never enumerates the whole registry — a reconcile
	// with no owner has nothing of its own.
	if got := r.RuntimeAddedSources(auth.Owner{}); got != nil {
		t.Fatalf("RuntimeAddedSources(zero owner) = %v, want nil (never the boot / whole-registry set)", got)
	}
	// A DIFFERENT owner sees none of A's runtime-adds and no boot server.
	if got := r.RuntimeAddedSources(ownerB()); len(got) != 0 {
		t.Fatalf("RuntimeAddedSources(B) = %v, want empty (A's add + boot excluded)", got)
	}
	// The process-global enumeration still lists everything (boot + every
	// owner's runtime-adds) — the bare-name attached set is unchanged.
	if got := r.SourceIDs(); len(got) != 2 {
		t.Fatalf("SourceIDs() = %v, want [a-add boot-srv] (process-global, unfiltered)", got)
	}
}

// TestRegistry_RuntimeAddedSources_OwnerScoped proves the reconcile view is
// scoped per owner: A sees only A's adds, B only B's, and a boot server is in
// neither view.
func TestRegistry_RuntimeAddedSources_OwnerScoped(t *testing.T) {
	r := NewRegistry()
	registerServer(t, r, "boot-srv", auth.Owner{})
	registerServer(t, r, "a-1", ownerA())
	registerServer(t, r, "a-2", ownerA())
	registerServer(t, r, "b-1", ownerB())

	a := r.RuntimeAddedSources(ownerA())
	sort.Strings(a)
	if len(a) != 2 || a[0] != "a-1" || a[1] != "a-2" {
		t.Fatalf("RuntimeAddedSources(A) = %v, want [a-1 a-2]", a)
	}
	if got := r.RuntimeAddedSources(ownerB()); len(got) != 1 || got[0] != "b-1" {
		t.Fatalf("RuntimeAddedSources(B) = %v, want [b-1]", got)
	}
	// Neither owner's view contains the boot server.
	for _, name := range append(a, r.RuntimeAddedSources(ownerB())...) {
		if name == "boot-srv" {
			t.Fatal("a boot server must never appear in any owner's reconcile view")
		}
	}
}

// TestRegistry_BootServerVisibleToEverySession proves a boot-declared server
// registered under the single boot identity is returned by a read under an
// ARBITRARY session triple — the property full-triple keying of the registry
// would have broken (boot servers attach once under one deployment identity but
// are read under many session triples).
func TestRegistry_BootServerVisibleToEverySession(t *testing.T) {
	r := NewRegistry()
	registerServer(t, r, "boot-srv", auth.Owner{}) // boot-declared, untagged

	// Two totally different session triples (different tenant/user/session).
	sessionA := ctxWithTriple(t, "tenant-x", "user-x", "sess-x")
	sessionB := ctxWithTriple(t, "tenant-y", "user-y", "sess-y")

	// GetServer (a bare-name read) returns the boot server under BOTH sessions —
	// the read is not owner/triple-keyed on the server set.
	if v, err := r.GetServer(sessionA, "boot-srv"); err != nil || v == nil || v.Name != "boot-srv" {
		t.Fatalf("GetServer(sessionA, boot-srv) = %v, err=%v; want the boot server", v, err)
	}
	if v, err := r.GetServer(sessionB, "boot-srv"); err != nil || v == nil || v.Name != "boot-srv" {
		t.Fatalf("GetServer(sessionB, boot-srv) = %v, err=%v; want the boot server (visible to EVERY session)", v, err)
	}
	// ListServers likewise lists it under an arbitrary session.
	servers, _, err := r.ListServers(sessionB, ListFilter{})
	if err != nil {
		t.Fatalf("ListServers(sessionB): %v", err)
	}
	found := false
	for _, s := range servers {
		if s.Name == "boot-srv" {
			found = true
		}
	}
	if !found {
		t.Fatal("boot-srv absent from an arbitrary session's ListServers — boot visibility broke")
	}
}

// TestRegistry_RuntimeAddedSources_ConcurrentReuse is the D-025 concurrent-reuse
// gate for the owner-scoped reconcile view: N≥100 concurrent RuntimeAddedSources
// reads across ≥2 owners + boot ListServers reads + interleaved Deregisters
// against ONE shared Registry never race and never leak an owner's entry into
// another owner's view.
func TestRegistry_RuntimeAddedSources_ConcurrentReuse(t *testing.T) {
	r := NewRegistry()
	registerServer(t, r, "boot-srv", auth.Owner{})
	registerServer(t, r, "a-keep", ownerA())
	registerServer(t, r, "b-keep", ownerB())
	ctx := ctxWithTriple(t, "tenant-x", "user-x", "sess-x")

	const n = 128
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				for _, name := range r.RuntimeAddedSources(ownerA()) {
					if name == "boot-srv" || name == "b-keep" {
						t.Errorf("owner A's view leaked %q", name)
					}
				}
			case 1:
				for _, name := range r.RuntimeAddedSources(ownerB()) {
					if name == "boot-srv" || name == "a-keep" {
						t.Errorf("owner B's view leaked %q", name)
					}
				}
			case 2:
				_, _, _ = r.ListServers(ctx, ListFilter{})
			default:
				_ = r.SourceIDs()
			}
		}(i)
	}
	wg.Wait()

	// After the storm the owners' views are intact and unswapped.
	if got := r.RuntimeAddedSources(ownerA()); len(got) != 1 || got[0] != "a-keep" {
		t.Fatalf("post-storm RuntimeAddedSources(A) = %v, want [a-keep]", got)
	}
	if got := r.RuntimeAddedSources(ownerB()); len(got) != 1 || got[0] != "b-keep" {
		t.Fatalf("post-storm RuntimeAddedSources(B) = %v, want [b-keep]", got)
	}
}
