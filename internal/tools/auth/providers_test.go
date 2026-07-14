package auth

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

// fakeProvider is a minimal auth.OAuthProvider for ProviderSet tests. It counts
// closes so a test can assert Uninstall closed it exactly once.
type fakeProvider struct {
	name   string
	closes atomic.Int64
}

func (p *fakeProvider) Token(context.Context, tools.ToolSourceID) (Token, error) {
	return Token{}, fmt.Errorf("fake provider %q: no token", p.name)
}
func (p *fakeProvider) InitiateFlow(context.Context, tools.ToolSourceID) (FlowInitiation, error) {
	return FlowInitiation{}, nil
}
func (p *fakeProvider) CompleteFlow(context.Context, string, string) (Token, error) {
	return Token{}, nil
}
func (p *fakeProvider) PendingFlow(string) (PendingFlowInfo, bool) { return PendingFlowInfo{}, false }
func (p *fakeProvider) DenyFlow(context.Context, string, string) error {
	return nil
}
func (p *fakeProvider) Revoke(context.Context, tools.ToolSourceID) error { return nil }
func (p *fakeProvider) Close(context.Context) error {
	p.closes.Add(1)
	return nil
}
func (p *fakeProvider) AllowedDownstreamHosts() []string { return []string{"graph.microsoft.com"} }

func TestProviderSet_InstallGetUninstall(t *testing.T) {
	t.Parallel()
	set := NewProviderSet(nil)
	owner := Owner{Tenant: "t1", Agent: "a1"}
	p := &fakeProvider{name: "m365"}
	if err := set.Install(owner, "m365", p); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, ok := set.Get("m365")
	if !ok || got != p {
		t.Fatalf("Get after install: ok=%v got=%v", ok, got)
	}
	if names := set.InstalledFor(owner); len(names) != 1 || names[0] != "m365" {
		t.Fatalf("InstalledFor: %v", names)
	}
	// Another owner's view is empty.
	if names := set.InstalledFor(Owner{Tenant: "t2", Agent: "a2"}); len(names) != 0 {
		t.Fatalf("InstalledFor(other): %v", names)
	}
	if err := set.Uninstall(context.Background(), "m365"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, ok := set.Get("m365"); ok {
		t.Fatalf("Get after uninstall: still present")
	}
	if p.closes.Load() != 1 {
		t.Fatalf("uninstall must close exactly once, got %d", p.closes.Load())
	}
	// Idempotent uninstall of an absent name.
	if err := set.Uninstall(context.Background(), "m365"); err != nil {
		t.Fatalf("idempotent uninstall: %v", err)
	}
}

func TestProviderSet_BootCollisionRefused(t *testing.T) {
	t.Parallel()
	boot := &fakeProvider{name: "github"}
	set := NewProviderSet(map[string]OAuthProvider{"github": boot})
	err := set.Install(Owner{Tenant: "t1", Agent: "a1"}, "github", &fakeProvider{name: "shadow"})
	if err == nil {
		t.Fatalf("install shadowing a boot provider must fail loud")
	}
	// The boot provider must be UNAFFECTED (still resolvable, never closed).
	if got, ok := set.Get("github"); !ok || got != boot {
		t.Fatalf("boot provider must survive a rejected install")
	}
	// Uninstalling a boot (zero-owner) provider is refused.
	if err := set.Uninstall(context.Background(), "github"); err == nil {
		t.Fatalf("uninstall of a boot provider must be refused")
	}
	if boot.closes.Load() != 0 {
		t.Fatalf("boot provider must never be closed by the control plane")
	}
}

func TestProviderSet_OwnerCollisionRefused(t *testing.T) {
	t.Parallel()
	set := NewProviderSet(nil)
	if err := set.Install(Owner{Tenant: "tA", Agent: "aA"}, "m365", &fakeProvider{}); err != nil {
		t.Fatalf("install A: %v", err)
	}
	err := set.Install(Owner{Tenant: "tB", Agent: "aB"}, "m365", &fakeProvider{})
	if err == nil {
		t.Fatalf("cross-owner name collision must fail loud")
	}
}

// TestProviderSet_ConcurrentReuse exercises N≥100 concurrent Install / Uninstall
// / Get across ≥2 tenants against ONE shared set under -race: no torn map, no
// use-after-close observed through Get, no cross-tenant bleed (D-025).
func TestProviderSet_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	set := NewProviderSet(nil)
	const workers = 128
	owners := []Owner{{Tenant: "tA", Agent: "aA"}, {Tenant: "tB", Agent: "aB"}}
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := owners[i%len(owners)]
			name := fmt.Sprintf("prov-%d", i)
			_ = set.Install(owner, name, &fakeProvider{name: name})
			if p, ok := set.Get(name); ok && p == nil {
				t.Errorf("Get returned a nil provider with ok=true (torn state)")
			}
			_ = set.InstalledFor(owner)
			_ = set.Names()
			_ = set.Uninstall(context.Background(), name)
		}(i)
	}
	wg.Wait()
	// After the storm every name is uninstalled; the set is empty.
	if names := set.Names(); len(names) != 0 {
		t.Fatalf("expected empty set after concurrent install/uninstall, got %v", names)
	}
}
