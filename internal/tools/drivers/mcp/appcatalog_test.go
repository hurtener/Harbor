package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// mutableAppProvider is a serverProvider whose Discover returns the
// CURRENT descriptor set — the seam the refresh test flips to prove the
// App dispatch view rebuilds from a fresh snapshot with no stale entries.
// Guarded by its own mutex; safe for concurrent use.
type mutableAppProvider struct {
	mu    sync.Mutex
	id    tools.ToolSourceID
	descs []tools.ToolDescriptor
}

func (p *mutableAppProvider) SourceID() tools.ToolSourceID { return p.id }

func (p *mutableAppProvider) Close(context.Context) error { return nil }

func (p *mutableAppProvider) DisplayModes() []string { return nil }

func (p *mutableAppProvider) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", nil
}

func (p *mutableAppProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]tools.ToolDescriptor, len(p.descs))
	copy(out, p.descs)
	return out, nil
}

func (p *mutableAppProvider) set(descs []tools.ToolDescriptor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.descs = append([]tools.ToolDescriptor(nil), descs...)
}

// appDesc builds a tool descriptor for the app-catalog fixtures: an
// ordinary tool (AppOnly=false) or an app-only callback (AppOnly=true),
// with an Invoke that counts executions through hits so denial-before-
// execution tests can assert ZERO executions.
func appDesc(name string, source tools.ToolSourceID, appOnly bool, hits *atomic.Int64) tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP, AppOnly: appOnly},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			if hits != nil {
				hits.Add(1)
			}
			return tools.ToolResult{Value: map[string]any{"ok": name}}, nil
		},
	}
}

// stageAppServer publishes one server with the given descriptors through
// the exact publication path (StageRegistration + Publish), mirroring what
// the attach path does — the App dispatch catalog is partitioned from the
// same snapshot at stage time.
func stageAppServer(t *testing.T, reg *Registry, providerID string, descs []tools.ToolDescriptor) {
	t.Helper()
	swap, err := reg.StageRegistration(ServerRegistration{
		Provider:     &mutableAppProvider{id: tools.ToolSourceID(providerID)},
		Transport:    "inmemory",
		InitialState: ServerStateOnline,
	}, descs)
	if err != nil {
		t.Fatalf("StageRegistration(%q): %v", providerID, err)
	}
	if _, err := swap.Publish(context.Background(), nil); err != nil {
		t.Fatalf("Publish(%q): %v", providerID, err)
	}
}

// TestRegistry_ResolveAppTool_OwnServerOnly pins the core dispatch
// property: an app-only callback resolves ONLY through its own server's
// App dispatch catalog. A missing server, another server, an ordinary
// (non-app-only) name, and an unknown name all answer not-found — no
// string prefix or remembered global tool name can select another server's
// callback.
func TestRegistry_ResolveAppTool_OwnServerOnly(t *testing.T) {
	reg := NewRegistry()
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{
		appDesc("srv-a_plain", "srv-a", false, nil),
		appDesc("srv-a_cb", "srv-a", true, nil),
	})
	stageAppServer(t, reg, "srv-b", []tools.ToolDescriptor{
		appDesc("srv-b_cb", "srv-b", true, nil),
	})

	if d, ok := reg.ResolveAppTool("srv-a", "srv-a_cb"); !ok {
		t.Fatal("own-server callback did not resolve through the App dispatch catalog")
	} else if !d.Tool.AppOnly {
		t.Fatalf("resolved descriptor is not marked AppOnly: %+v", d.Tool)
	}

	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_plain"); ok {
		t.Fatal("ordinary (non-app-only) tool resolved as an app-only callback")
	}
	if _, ok := reg.ResolveAppTool("srv-b", "srv-a_cb"); ok {
		t.Fatal("another server's App dispatch catalog resolved a foreign callback")
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-b_cb"); ok {
		t.Fatal("own server resolved a callback that belongs to another server")
	}
	if _, ok := reg.ResolveAppTool("no-such-server", "srv-a_cb"); ok {
		t.Fatal("unknown server resolved an app-only callback")
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_nope"); ok {
		t.Fatal("unknown callback name resolved")
	}
	if _, ok := reg.ResolveAppTool("", "srv-a_cb"); ok {
		t.Fatal("empty server identity resolved an app-only callback")
	}
}

// TestRegistry_RefreshDiscovery_RebuildsAppViewWithNoStale proves a
// catalog refresh rebuilds the App dispatch view from the FRESH discovered
// snapshot: a newly added callback becomes usable (only through its own
// server), a removed callback stops resolving everywhere, and no stale
// entry survives.
func TestRegistry_RefreshDiscovery_RebuildsAppViewWithNoStale(t *testing.T) {
	ctx := idCtx(t)
	reg := NewRegistry()
	provider := &mutableAppProvider{id: "srv-a"}
	provider.set([]tools.ToolDescriptor{appDesc("srv-a_cb-old", "srv-a", true, nil)})
	swap, err := reg.StageRegistration(ServerRegistration{
		Provider: provider, Transport: "inmemory", InitialState: ServerStateOnline,
	}, []tools.ToolDescriptor{appDesc("srv-a_cb-old", "srv-a", true, nil)})
	if err != nil {
		t.Fatalf("StageRegistration: %v", err)
	}
	if _, err := swap.Publish(ctx, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_cb-old"); !ok {
		t.Fatal("initial callback did not resolve")
	}

	// The server's tools/list changes: cb-old is removed, cb-new is added.
	provider.set([]tools.ToolDescriptor{appDesc("srv-a_cb-new", "srv-a", true, nil)})
	if _, err := reg.RefreshDiscovery(ctx, "srv-a"); err != nil {
		t.Fatalf("RefreshDiscovery: %v", err)
	}

	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_cb-old"); ok {
		t.Fatal("removed callback survived the refresh — stale entry in the App dispatch view")
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_cb-new"); !ok {
		t.Fatal("newly added callback did not become usable through its own server after refresh")
	}
	if _, ok := reg.ResolveAppTool("srv-b", "srv-a_cb-new"); ok {
		t.Fatal("newly added callback leaked into another server's App dispatch view")
	}
}

// TestRegistry_RefreshDiscovery_ReconcilesVisibilityTransitions proves that
// one refresh snapshot moves a descriptor between the ordinary and App-only
// views in both directions, without leaving either stale projection behind.
func TestRegistry_RefreshDiscovery_ReconcilesVisibilityTransitions(t *testing.T) {
	ctx := idCtx(t)
	reg := NewRegistry()
	cat := tools.NewCatalog()
	provider := &mutableAppProvider{id: "srv-transition"}
	ordinary := appDesc("srv-transition_tool", "srv-transition", false, nil)
	provider.set([]tools.ToolDescriptor{ordinary})
	swap, err := reg.StageRegistration(ServerRegistration{
		Provider: provider, Transport: "inmemory", InitialState: ServerStateOnline,
		Catalog: cat,
	}, []tools.ToolDescriptor{ordinary})
	if err != nil {
		t.Fatalf("StageRegistration: %v", err)
	}
	if _, err := swap.Publish(ctx, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	initial, err := cat.StageSource("srv-transition", ordinaryDescriptors([]tools.ToolDescriptor{ordinary}), false)
	if err != nil {
		t.Fatalf("initial catalog stage: %v", err)
	}
	initial.Commit()

	provider.set([]tools.ToolDescriptor{appDesc("srv-transition_tool", "srv-transition", true, nil)})
	if _, err := reg.RefreshDiscovery(ctx, "srv-transition"); err != nil {
		t.Fatalf("ordinary-to-app refresh: %v", err)
	}
	if _, ok := cat.Resolve("srv-transition_tool"); ok {
		t.Fatal("ordinary descriptor survived transition to App-only")
	}
	if _, ok := reg.ResolveAppTool("srv-transition", "srv-transition_tool"); !ok {
		t.Fatal("App-only descriptor missing after ordinary-to-app transition")
	}

	provider.set([]tools.ToolDescriptor{ordinary})
	if _, err := reg.RefreshDiscovery(ctx, "srv-transition"); err != nil {
		t.Fatalf("app-to-ordinary refresh: %v", err)
	}
	if _, ok := reg.ResolveAppTool("srv-transition", "srv-transition_tool"); ok {
		t.Fatal("App-only descriptor survived transition to ordinary")
	}
	if _, ok := cat.Resolve("srv-transition_tool"); !ok {
		t.Fatal("ordinary descriptor missing after App-only-to-ordinary transition")
	}
}

// TestRegistry_RefreshDiscovery_ReappliesAttachmentPolicy proves refresh
// cannot resurrect a callback excluded by the attachment allow/deny policy.
func TestRegistry_RefreshDiscovery_ReappliesAttachmentPolicy(t *testing.T) {
	ctx := idCtx(t)
	reg := NewRegistry()
	provider := &mutableAppProvider{id: "srv-policy"}
	allowed := appDesc("srv-policy_allowed", "srv-policy", true, nil)
	denied := appDesc("srv-policy_denied", "srv-policy", true, nil)
	provider.set([]tools.ToolDescriptor{allowed})
	swap, err := reg.StageRegistration(ServerRegistration{
		Provider: provider, Transport: "inmemory", InitialState: ServerStateOnline,
		ToolAllowlist: []string{"allowed"}, ToolDenylist: []string{"denied"},
	}, []tools.ToolDescriptor{allowed})
	if err != nil {
		t.Fatalf("StageRegistration: %v", err)
	}
	if _, err := swap.Publish(ctx, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	provider.set([]tools.ToolDescriptor{allowed, denied})
	if _, err := reg.RefreshDiscovery(ctx, "srv-policy"); err != nil {
		t.Fatalf("policy refresh: %v", err)
	}
	if _, ok := reg.ResolveAppTool("srv-policy", "srv-policy_allowed"); !ok {
		t.Fatal("allowlisted callback missing after policy refresh")
	}
	if _, ok := reg.ResolveAppTool("srv-policy", "srv-policy_denied"); ok {
		t.Fatal("denied callback became callable after refresh")
	}
}

// TestRegistry_Replacement_SwapsAppCallbacksAtomically proves a same-name
// replacement (re-attach / hot reload) swaps the App dispatch view with
// the new generation: the old callbacks stop resolving, the new ones
// resolve, and no stale entry survives.
func TestRegistry_Replacement_SwapsAppCallbacksAtomically(t *testing.T) {
	ctx := idCtx(t)
	reg := NewRegistry()
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{
		appDesc("srv-a_cb-v1", "srv-a", true, nil),
	})

	// Same-name re-attach with a NEW generation.
	provider := &mutableAppProvider{id: "srv-a"}
	provider.set([]tools.ToolDescriptor{appDesc("srv-a_cb-v2", "srv-a", true, nil)})
	swap, err := reg.StageRegistration(ServerRegistration{
		Provider: provider, Transport: "inmemory", InitialState: ServerStateOnline,
	}, []tools.ToolDescriptor{appDesc("srv-a_cb-v2", "srv-a", true, nil)})
	if err != nil {
		t.Fatalf("StageRegistration(replacement): %v", err)
	}
	if _, err := swap.Publish(ctx, nil); err != nil {
		t.Fatalf("Publish(replacement): %v", err)
	}

	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_cb-v1"); ok {
		t.Fatal("v1 callback survived a same-name replacement — stale App dispatch entry")
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_cb-v2"); !ok {
		t.Fatal("v2 callback did not resolve after replacement")
	}
}

// TestRegistry_Deregister_RemovesAppCallbacks proves detach removes the
// server's App dispatch view with the registration: a detached server's
// callbacks stop resolving everywhere.
func TestRegistry_Deregister_RemovesAppCallbacks(t *testing.T) {
	ctx := idCtx(t)
	reg := NewRegistry()
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{
		appDesc("srv-a_cb", "srv-a", true, nil),
	})
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_cb"); !ok {
		t.Fatal("callback did not resolve before detach")
	}
	// The zero owner matches the boot-declared (untagged) registration.
	if err := reg.Deregister(ctx, "srv-a", auth.Owner{}); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_cb"); ok {
		t.Fatal("detached server's callback still resolves — stale App dispatch entry")
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_plain"); ok {
		t.Fatal("detached server still resolves any app tool")
	}
}

// TestRegistry_ResolveAppTool_ConcurrentIsolation runs N concurrent
// ResolveAppTool calls from mixed identities against one shared Registry
// under -race, asserting each goroutine's own-server lookup is isolated
// from every other identity's (no context or authority bleed). The mixed
// call mix exercises both the own-server hit and the cross-server miss
// paths concurrently.
func TestRegistry_ResolveAppTool_ConcurrentIsolation(t *testing.T) {
	reg := NewRegistry()
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{
		appDesc("srv-a_cb", "srv-a", true, nil),
	})
	stageAppServer(t, reg, "srv-b", []tools.ToolDescriptor{
		appDesc("srv-b_cb", "srv-b", true, nil),
	})

	const n = 128
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine carries a DISTINCT session so a cross-talk
			// would surface as a wrong lookup result.
			ctx, _ := identity.With(context.Background(), identity.Identity{
				TenantID: "t-" + string(rune('A'+(i%3))),
				UserID:   "u-1", SessionID: "s-" + string(rune('A'+(i%26))),
			})
			if i%2 == 0 {
				if _, ok := reg.ResolveAppTool("srv-a", "srv-a_cb"); !ok {
					errs <- fmt.Errorf("session %d: own-server callback did not resolve", i)
				}
				if _, ok := reg.ResolveAppTool("srv-b", "srv-a_cb"); ok {
					errs <- fmt.Errorf("session %d: cross-server callback resolved", i)
				}
			} else {
				if _, ok := reg.ResolveAppTool("srv-b", "srv-b_cb"); !ok {
					errs <- fmt.Errorf("session %d: own-server callback did not resolve", i)
				}
				if _, ok := reg.ResolveAppTool("srv-a", "srv-b_cb"); ok {
					errs <- fmt.Errorf("session %d: cross-server callback resolved", i)
				}
			}
			_ = ctx
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
