package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// cross_owner_name_collision_test.go — the instrument for D-301's BOUNDED
// GUARANTEE about runtime-added connection names in a shared runtime.
//
// D-301 (docs/decisions.md) settled that the tool catalog, resolution, and
// dispatch stay PROCESS-GLOBAL and bare-name, and that the (tenant, agent)
// owner tag is a reconcile-VIEW filter — never an isolation, dispatch, or
// storage key. The honest guarantee it states in exchange is:
//
//	"in a shared runtime, runtime-added connection/provider NAMES share a
//	 deployment namespace and a collision fails loud; a shared runtime
//	 therefore TRUSTS its co-tenant admins for runtime-added connections, and
//	 a deployment needing hard isolation runs ONE-RUNTIME-PER-TENANT."
//
// That guarantee had no instrument. `owner_scoped_test.go` pins the
// reconcile-VIEW half of D-301 (RuntimeAddedSources scopes to one owner); the
// NAMESPACE half — that a cross-owner same-name attach fails LOUD rather than
// shadowing, cross-serving, or silently winning — was pinned nowhere. These
// tests close that gap.
//
// # Why the PRE-DIAL assertion is load-bearing
//
// A recurring proposal is to re-key the registry by (owner, name) so two
// owners may hold the same connection name. D-301's context section already
// rejected it and named why: dispatch and bearer injection go through the
// bare-name catalog, so re-keying the registry METADATA leaves same-named
// connections colliding at the catalog (ErrToolDuplicateName) or cross-serving
// another owner's OAuth bearer.
//
// The registry refusal is therefore not merely one of two equivalent gates. It
// is the EARLY one, and it fires BEFORE `provider.Connect` — no subprocess, no
// socket, no handshake. The catalog gate fires only after the transport is live
// (mcpdrv.Attach appends the provider's closer immediately after a successful
// Connect). Removing the registry gate would convert a clean pre-dial refusal
// into a post-dial one that spawns a transport it must then tear down. That
// property is what TestAttach_CrossOwnerSameName_RefusedPreDial pins, and what
// TestAttach_CrossOwnerSameName_CatalogIsTheSecondGate demonstrates the cost of
// losing.
//
// The tests assert BEHAVIOUR (the refusal, its loudness, its timing relative to
// the dial) rather than the map's key shape, so a future refactor that keeps the
// guarantee is free to change how it is implemented.

// collisionServer starts one httptest-hosted MCP server the attach paths dial.
func collisionServer(t *testing.T) *httptest.Server {
	t.Helper()
	mockSrv := newMockServer()
	h := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return mockSrv.server }, nil)
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// catalogNames returns the sorted tool names currently in the catalog.
func catalogNames(cat tools.ToolCatalog) []string {
	listed := cat.List(tools.CatalogFilter{})
	out := make([]string, 0, len(listed))
	for _, tl := range listed {
		out = append(out, tl.Name)
	}
	sort.Strings(out)
	return out
}

// collisionHarness bundles the shared collaborators an attach needs.
type collisionHarness struct {
	url     string
	cat     tools.ToolCatalog
	closers []func(context.Context) error
}

// attach runs one Attach against the harness's shared catalog and the supplied
// registry, under the supplied owner.
func (h *collisionHarness) attach(t *testing.T, ctx context.Context, name string, owner auth.Owner, reg *Registry) error {
	t.Helper()
	return Attach(ctx, config.MCPServerConfig{
		Name:          name,
		TransportMode: string(TransportSSE),
		URL:           h.url,
	}, AttachDeps{
		Catalog:         h.cat,
		Registry:        reg,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &h.closers,
		Owner:           owner,
	})
}

func newCollisionHarness(t *testing.T) *collisionHarness {
	t.Helper()
	h := &collisionHarness{
		url:     collisionServer(t).URL,
		cat:     tools.NewCatalog(),
		closers: []func(context.Context) error{},
	}
	t.Cleanup(func() {
		for i := len(h.closers) - 1; i >= 0; i-- {
			_ = h.closers[i](context.Background())
		}
	})
	return h
}

// TestAttach_CrossOwnerSameName_RefusedPreDial is the D-301 namespace guarantee.
//
// Two owners attach the SAME logical connection name against ONE shared
// registry + catalog (the production shape). The second attach must be refused
// LOUD with the typed ErrConnectionNameOwnerConflict — never shadowing owner A's
// registration, never tearing it down, never silently winning.
//
// And the refusal must be PRE-DIAL: owner B's transport is never spawned, so no
// closer joins the chain and the catalog is byte-identical to its post-owner-A
// state. See the file doc for why that timing is the property worth guarding.
func TestAttach_CrossOwnerSameName_RefusedPreDial(t *testing.T) {
	h := newCollisionHarness(t)
	reg := NewRegistry() // ONE registry — the production shape.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ownerA := auth.Owner{Tenant: "tenant-a", Agent: "agent-1"}
	ownerB := auth.Owner{Tenant: "tenant-b", Agent: "agent-1"}

	if err := h.attach(t, ctx, "github", ownerA, reg); err != nil {
		t.Fatalf("owner A attach must succeed: %v", err)
	}
	// Snapshot the state owner A legitimately produced.
	closersAfterA := len(h.closers)
	catalogAfterA := catalogNames(h.cat)
	if closersAfterA != 1 {
		t.Fatalf("owner A must have spawned exactly 1 transport, got %d", closersAfterA)
	}
	if len(catalogAfterA) == 0 {
		t.Fatalf("owner A must have registered tools; catalog is empty")
	}

	err := h.attach(t, ctx, "github", ownerB, reg)

	// (1) The refusal is loud and typed.
	if err == nil {
		t.Fatalf("D-301 violated: owner B's same-name attach SUCCEEDED — a cross-owner " +
			"name collision must fail loud, never silently shadow or cross-serve")
	}
	if !errors.Is(err, ErrConnectionNameOwnerConflict) {
		t.Fatalf("D-301: the cross-owner collision must surface as the typed "+
			"ErrConnectionNameOwnerConflict so callers can classify it; got %v", err)
	}

	// (2) The refusal is PRE-DIAL — no transport was spawned for owner B.
	// mcpdrv.Attach appends the provider's closer immediately after a successful
	// Connect, so an unchanged closer chain proves the dial never happened.
	if got := len(h.closers); got != closersAfterA {
		t.Fatalf("D-301: the cross-owner refusal must be PRE-DIAL, but owner B spawned a "+
			"transport: closers %d -> %d. A registry re-key that defers the refusal to the "+
			"catalog reintroduces exactly this live-transport cost (see the file doc)",
			closersAfterA, got)
	}

	// (3) Owner A's state is untouched: same catalog, same registration, same owner.
	if got := catalogNames(h.cat); !reflect.DeepEqual(got, catalogAfterA) {
		t.Fatalf("D-301: the refused attach must leave the catalog untouched.\n before: %v\n after:  %v",
			catalogAfterA, got)
	}
	gotOwner, exists := reg.OwnerOf("github")
	if !exists {
		t.Fatalf("D-301: owner A's registration must survive owner B's refused attach")
	}
	if gotOwner != ownerA {
		t.Fatalf("D-301: owner A's registration must not be re-owned by the refused attach; got %+v", gotOwner)
	}
}

// TestAttach_CrossOwnerDistinctNames_BothAttach is the CONTROL arm.
//
// Same harness, same two owners, DISTINCT connection names — both attach. It
// exists so a future breakage of the harness (an unreachable fixture server, a
// mis-wired catalog, a bus that rejects every publish) cannot masquerade as the
// collision guarantee holding: if this test fails, the sibling test's refusal
// proves nothing about names.
//
// It also documents the workaround D-301's namespace guarantee imposes on a
// coordinator attaching one logical server for N owners — encoding the owner
// into the connection id, which is what makes the id long.
func TestAttach_CrossOwnerDistinctNames_BothAttach(t *testing.T) {
	h := newCollisionHarness(t)
	reg := NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.attach(t, ctx, "tenanta_github", auth.Owner{Tenant: "tenant-a", Agent: "agent-1"}, reg); err != nil {
		t.Fatalf("owner A attach (distinct name): %v", err)
	}
	if err := h.attach(t, ctx, "tenantb_github", auth.Owner{Tenant: "tenant-b", Agent: "agent-1"}, reg); err != nil {
		t.Fatalf("CONTROL FAILED — two owners with DISTINCT names must both attach, so the "+
			"sibling collision test is measuring the NAME and not a broken harness: %v", err)
	}
	if got := len(h.closers); got != 2 {
		t.Fatalf("control: both attaches must spawn a transport, got %d closers", got)
	}
}

// TestAttach_CrossOwnerSameName_CatalogIsTheSecondGate pins the fact D-301's
// context section rests on: the registry's owner conflict is not the only thing
// keeping two owners off one name — the process-global bare-name tool catalog
// refuses the collision independently.
//
// The test removes the registry gate BY CONSTRUCTION (one Registry per owner —
// i.e. exactly what a (owner, name) re-key would achieve) and shows the attach
// still fails, now at catalog.Register with ErrToolDuplicateName, and now only
// AFTER owner B's transport is live.
//
// This is the load-bearing evidence against re-keying the registry: doing so
// does not let two owners share a name, it only moves the failure past the dial.
// If this test ever goes green-by-success, the catalog has been re-keyed too and
// D-301 needs revisiting as a whole (RFC PR + superseding decision), not
// piecemeal.
func TestAttach_CrossOwnerSameName_CatalogIsTheSecondGate(t *testing.T) {
	h := newCollisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A separate registry per owner: no registry key can collide.
	if err := h.attach(t, ctx, "github", auth.Owner{Tenant: "tenant-a", Agent: "agent-1"}, NewRegistry()); err != nil {
		t.Fatalf("owner A attach: %v", err)
	}
	closersAfterA := len(h.closers)

	err := h.attach(t, ctx, "github", auth.Owner{Tenant: "tenant-b", Agent: "agent-1"}, NewRegistry())
	if err == nil {
		t.Fatalf("D-301: with the registry gate removed, the process-global bare-name CATALOG " +
			"must still refuse a cross-owner same-name attach. It did not — dispatch may now " +
			"be ambiguous across owners, which D-301 forbids")
	}
	if !errors.Is(err, tools.ErrToolDuplicateName) {
		t.Fatalf("D-301: the catalog gate must surface as ErrToolDuplicateName; got %v", err)
	}
	// The cost of losing the pre-dial registry gate: owner B's transport is live.
	if got := len(h.closers); got != closersAfterA+1 {
		t.Fatalf("expected owner B's transport to be spawned before the catalog refusal "+
			"(closers %d -> %d, want %d) — this is the post-dial cost the registry gate avoids",
			closersAfterA, got, closersAfterA+1)
	}
}
