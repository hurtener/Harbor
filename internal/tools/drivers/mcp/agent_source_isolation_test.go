package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

func TestAgentSource_ConcurrentInvocationAndRestoredReference(t *testing.T) {
	h := newCollisionHarness(t)
	reg := NewRegistry()
	owners := []auth.Owner{{Tenant: "tenant-a", Agent: "base"}, {Tenant: "tenant-b", Agent: "base"}, {Tenant: "tenant-a", Agent: "derived"}}
	for _, owner := range owners {
		if err := h.attach(t, context.Background(), "same", owner, reg); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := owners[i%len(owners)]
			ctx, err := identity.With(context.Background(), identity.Identity{TenantID: owner.Tenant, UserID: fmt.Sprintf("user-%d", i), SessionID: fmt.Sprintf("new-session-%d", i)})
			if err != nil {
				t.Error(err)
				return
			}
			ctx = tools.WithEffectiveAgentConfig(ctx, owner.Agent)
			// A persisted pre-upgrade logical reference resolves only under the
			// current admitted tenant/agent after the registry has been reconstructed.
			physical := PhysicalServerName("same", owner)
			if got := reg.CanonicalSourceID(ctx, "same"); got != physical {
				t.Errorf("restored source=%s want %s", got, physical)
			}
			if _, err := reg.ListResources(ctx, "same"); err != nil {
				t.Errorf("restored resources: %v", err)
			}
			for _, target := range owners {
				tool, ok := h.cat.Resolve(PhysicalServerName("same", target) + "_echo")
				if !ok {
					t.Error("missing tool")
					return
				}
				_, err := tool.Invoke(ctx, json.RawMessage(`{"text":"isolated"}`))
				if target == owner {
					if err != nil {
						t.Errorf("own invocation: %v", err)
					}
				} else {
					var denied ErrSourceOwnerDenied
					if !errors.As(err, &denied) {
						t.Errorf("foreign invocation: %v", err)
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestRegistry_RejectsIncompleteSourceOwnership(t *testing.T) {
	for _, owner := range []auth.Owner{{Tenant: "t"}, {Agent: "a"}, {User: "u"}, {Tenant: "t", User: "u"}} {
		reg := NewRegistry()
		if err := reg.Register(context.Background(), ServerRegistration{Provider: &stubProvider{id: tools.ToolSourceID("bad")}, Owner: owner}); err == nil {
			t.Errorf("accepted %+v", owner)
		}
	}
}

func TestAgentSource_RestoredAppReferenceUsesFreshGeneration(t *testing.T) {
	owner := auth.Owner{Tenant: "tenant-a", Agent: "base"}
	physical := PhysicalServerName("old-source", owner)
	reg := NewRegistry()
	provider := &stubProvider{id: tools.ToolSourceID(physical), resources: []string{"ui://app"}, resourceBody: []byte("app"), resourceMime: "text/html"}
	desc := tools.ToolDescriptor{Tool: tools.Tool{Name: physical + "_callback", Source: tools.ToolSourceID(physical), AppVisible: true}}
	swap, err := reg.StageRegistration(ServerRegistration{Provider: provider, Transport: "streamable-http", Owner: owner, LogicalName: "old-source"}, []tools.ToolDescriptor{desc})
	if err != nil {
		t.Fatal(err)
	}
	if err := swap.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: owner.Tenant, UserID: "u", SessionID: "reopened"})
	if err != nil {
		t.Fatal(err)
	}
	ctx = tools.WithEffectiveAgentConfig(ctx, owner.Agent)
	generation, ok, err := reg.CurrentGenerationForIdentity(ctx, "old-source")
	if err != nil || !ok {
		t.Fatalf("current generation %v %v", ok, err)
	}
	if body, _, err := reg.ReadResource(ctx, "old-source", "ui://app"); err != nil || string(body) != "app" {
		t.Fatalf("reopen resource: %s %v", body, err)
	}
	if got, ok, err := reg.ResolveAppToolAtGenerationForIdentity(ctx, "old-source", "old-source_callback", generation); err != nil || !ok || got.Tool.Name != desc.Tool.Name {
		t.Fatalf("reopen callback: %v %v %v", got.Tool, ok, err)
	}
	if _, _, err := reg.ResolveAppToolAtGenerationForIdentity(ctx, "old-source", "old-source_callback", "pre-upgrade-generation"); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("old admission accepted: %v", err)
	}
	other := tools.WithEffectiveAgentConfig(ctx, "derived")
	if _, ok, _ := reg.CurrentGenerationForIdentity(other, "old-source"); ok {
		t.Fatal("legacy reference leaked to derived agent")
	}
}
