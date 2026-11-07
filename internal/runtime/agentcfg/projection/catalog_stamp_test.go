package projection_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

type stampServer struct {
	source tools.ToolSourceID
	mu     sync.Mutex
	names  []string
}

func (s *stampServer) SourceID() tools.ToolSourceID { return s.source }
func (s *stampServer) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tools.ToolDescriptor, 0, len(s.names))
	for _, name := range s.names {
		out = append(out, tools.ToolDescriptor{Tool: tools.Tool{Name: string(s.source) + "_" + name, Source: s.source, Transport: tools.TransportMCP}, Invoke: stampInvoke})
	}
	return out, nil
}
func (s *stampServer) DisplayModes() []string { return nil }
func (s *stampServer) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("none")
}
func (s *stampServer) Close(context.Context) error { return nil }

func stampInvoke(context.Context, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{}, nil
}

// TestCatalogStamp_ChangesForEveryInventoryClass is the H3 AC1 table over
// real drivers (StateStore-backed agent config, session overlay, tools
// catalog and MCP registry). Every mutation class the configuration
// projection reads must move the stamp; discovery with an identical snapshot,
// approval-policy writes and tool invocations must not.
func TestCatalogStamp_ChangesForEveryInventoryClass(t *testing.T) {
	ctx := context.Background()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(ctx) })
	reg := newRegistryOver(t, st)
	overlay, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cat := tools.NewCatalog()
	registry := mcp.NewRegistry()
	resolver := projection.CatalogViewResolver{Registry: reg, SessionOverlay: overlay, Catalog: cat, OwnerResolver: registry}
	actor := projID()
	operator := identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: "operator", SessionID: "op"}}

	stamp := func() (string, bool) {
		t.Helper()
		revision, sessionScoped, ok, err := resolver.CatalogStamp(ctx, actor.Identity, projAgent, "configuration")
		if err != nil || !ok {
			t.Fatalf("stamp ok=%v err=%v", ok, err)
		}
		return revision, sessionScoped
	}
	setAgent := func(payload agentcfg.ConfigPayload) agentcfg.Revision {
		t.Helper()
		rev, err := reg.SetRevision(ctx, operator, projAgent, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return rev
	}
	first := setAgent(agentcfg.ConfigPayload{})
	owner := toolauth.Owner{Tenant: projTenant, Agent: projAgent}
	server := &stampServer{source: tools.ToolSourceID(mcp.PhysicalServerName("src", owner)), names: []string{"one"}}
	ownerCtx, err := identity.With(ctx, actor.Identity)
	if err != nil {
		t.Fatal(err)
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(st)
	if err != nil {
		t.Fatal(err)
	}
	var fence agentcfg.SignedOAuthMCPActivationFence
	var userFirst agentcfg.Revision

	steps := []struct {
		name   string
		mutate func()
		moves  bool
	}{
		{"agent revision", func() {
			setAgent(agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"x"}}})
		}, true},
		{"agent tool exposure", func() {
			setAgent(agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{ToolLoadingModes: map[string]string{"x": "deferred"}}})
		}, true},
		{"agent idempotent re-set", func() {
			setAgent(agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{ToolLoadingModes: map[string]string{"x": "deferred"}}})
		}, false},
		{"agent rollback", func() {
			if _, err := reg.Rollback(ctx, operator, projAgent, first.RevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"user revision", func() {
			rev, err := reg.SetRevision(ctx, actor, projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"y"}}}, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			userFirst = rev
		}, true},
		{"user second revision", func() {
			if _, err := reg.SetRevision(ctx, actor, projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"z"}}}, agentcfg.SetOptions{}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"user rollback", func() {
			if _, err := reg.Rollback(ctx, actor, projAgent, userFirst.RevisionID, agentcfg.ConfigScopeUser, agentcfg.SetOptions{}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"user deactivate", func() {
			if changed, err := reg.DeactivateIfActive(ctx, actor, projAgent, userFirst.RevisionID, agentcfg.ConfigScopeUser); err != nil || !changed {
				t.Fatalf("deactivate changed=%v err=%v", changed, err)
			}
		}, true},
		{"fence begin", func() {
			fence, err = fences.Begin(ctx, projTenant, projAgent, "op-1", "fingerprint", "candidate-hash", first.RevisionID)
			if err != nil {
				t.Fatal(err)
			}
		}, true},
		{"fence advance", func() {
			if fence, err = fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceAborted, ""); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"fence pair begin", func() {
			fence, err = fences.BeginForOperation(ctx, projTenant, projAgent, "op-2", "fingerprint-2", "candidate-2", first.RevisionID)
			if err != nil {
				t.Fatal(err)
			}
		}, true},
		{"fence release", func() {
			if err := fences.ReleasePending(ctx, fence); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"session overlay", func() {
			if _, err := overlay.SetSourceDisables(ctx, actor, projAgent, []string{"srv"}, nil); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"catalog register", func() {
			if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "boot_tool"}, Invoke: stampInvoke}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"catalog stage+rollback", func() {
			swap, err := cat.StageSource("staged", []tools.ToolDescriptor{{Tool: tools.Tool{Name: "staged_a", Source: "staged"}, Invoke: stampInvoke}}, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := swap.Rollback(); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"registry register", func() {
			if err := registry.Register(ctx, mcp.ServerRegistration{Provider: server, Transport: "streamable-http", Owner: owner, LogicalName: "src", DescriptorFingerprint: "fp", Catalog: cat}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"discovery publishes tools", func() {
			if _, err := registry.RefreshDiscovery(ownerCtx, string(server.source)); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"discovery identical snapshot", func() {
			if _, err := registry.RefreshDiscovery(ownerCtx, string(server.source)); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"approval policy write", func() {
			policies, err := toolapproval.NewStatePolicyStore(st)
			if err != nil {
				t.Fatal(err)
			}
			if err := policies.SetPolicy(ctx, actor.Identity, "boot_tool", prototypes.ToolApprovalGated); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"tool invocation", func() {
			desc, ok := cat.Resolve("boot_tool")
			if !ok {
				t.Fatal("resolve")
			}
			if _, err := desc.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"discovery changed snapshot", func() {
			server.mu.Lock()
			server.names = []string{"one", "two"}
			server.mu.Unlock()
			if _, err := registry.RefreshDiscovery(ownerCtx, string(server.source)); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"catalog deregister source", func() {
			if n := cat.(tools.CatalogSourceDeregisterer).DeregisterSource(server.source); n == 0 {
				t.Fatal("nothing deregistered")
			}
		}, true},
		{"registry deregister", func() {
			if err := registry.Deregister(ctx, "src", owner); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"agent deactivate", func() {
			active, ok, err := reg.Active(ctx, operator, projAgent, agentcfg.ConfigScopeAgent)
			if err != nil || !ok {
				t.Fatalf("active ok=%v err=%v", ok, err)
			}
			if changed, err := reg.DeactivateIfActive(ctx, operator, projAgent, active.RevisionID, agentcfg.ConfigScopeAgent); err != nil || !changed {
				t.Fatalf("deactivate changed=%v err=%v", changed, err)
			}
		}, true},
		{"agent re-activate", func() { setAgent(agentcfg.ConfigPayload{}) }, true},
		{"lifecycle retire", func() {
			active, _, err := reg.Active(ctx, operator, projAgent, agentcfg.ConfigScopeAgent)
			if err != nil {
				t.Fatal(err)
			}
			retirer, ok := reg.(agentcfg.RetirementRegistry)
			if !ok {
				t.Skip("registry does not implement retirement")
			}
			if _, err := retirer.Retire(ctx, operator, projAgent, agentcfg.RetirementRequest{OperationID: "retire-op", ExpectedContentHash: active.ContentHash}); err != nil {
				t.Fatal(err)
			}
		}, true},
	}

	previous, sessionScoped := stamp()
	if sessionScoped {
		t.Fatal("stamp is session scoped before any overlay exists")
	}
	for _, step := range steps {
		step.mutate()
		if step.name == "agent deactivate" {
			// With no agent pointer the agent lifecycle is inactive: the
			// stamp refuses exactly as an uncached projection does.
			if _, _, _, err := resolver.CatalogStamp(ctx, actor.Identity, projAgent, "configuration"); !errors.Is(err, sessionoverlay.ErrAgentLifecycleInactive) {
				t.Fatalf("stamp with inactive lifecycle err=%v, want ErrAgentLifecycleInactive", err)
			}
			continue
		}
		if step.name == "lifecycle retire" {
			// A retired agent is refused by the stamp, as by the projection.
			if _, _, _, err := resolver.CatalogStamp(ctx, actor.Identity, projAgent, "configuration"); !errors.Is(err, agentcfg.ErrAgentRetired) {
				t.Fatalf("stamp after retirement err=%v, want ErrAgentRetired", err)
			}
			continue
		}
		got, scoped := stamp()
		if moved := got != previous; moved != step.moves {
			t.Fatalf("%s: stamp moved=%v, want %v", step.name, moved, step.moves)
		}
		if step.name == "session overlay" && !scoped {
			t.Fatal("overlay write did not make the stamp session scoped")
		}
		previous = got
	}
}

// TestCatalogStamp_PrincipalBound proves equal inputs never yield one stamp
// for two principals or two sessions with overlays, and that missing
// generation support degrades to ok=false rather than an error.
func TestCatalogStamp_PrincipalBound(t *testing.T) {
	ctx := context.Background()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(ctx) })
	reg := newRegistryOver(t, st)
	overlay, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cat := tools.NewCatalog()
	registry := mcp.NewRegistry()
	resolver := projection.CatalogViewResolver{Registry: reg, SessionOverlay: overlay, Catalog: cat, OwnerResolver: registry}
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	stampFor := func(id identity.Identity) string {
		t.Helper()
		revision, _, ok, err := resolver.CatalogStamp(ctx, id, projAgent, "configuration")
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		return revision
	}
	a := identity.Identity{TenantID: projTenant, UserID: "a", SessionID: "s1"}
	b := identity.Identity{TenantID: projTenant, UserID: "b", SessionID: "s1"}
	if stampFor(a) == stampFor(b) {
		t.Fatal("two users share a stamp")
	}
	a2 := identity.Identity{TenantID: projTenant, UserID: "a", SessionID: "s2"}
	if stampFor(a) != stampFor(a2) {
		t.Fatal("sessions without an overlay must share the user's stamp")
	}
	if _, err := overlay.SetSourceDisables(ctx, identity.Quadruple{Identity: a}, projAgent, []string{"x"}, nil); err != nil {
		t.Fatal(err)
	}
	if stampFor(a) == stampFor(a2) {
		t.Fatal("an overlay session shares a stamp with an overlay-free session")
	}

	// A catalog without generation support cannot back a cache.
	_, _, ok, err := projection.CatalogViewResolver{Registry: reg, Catalog: noGenerationCatalog{cat}, OwnerResolver: registry}.CatalogStamp(ctx, a, projAgent, "configuration")
	if err != nil || ok {
		t.Fatalf("non-reporting catalog ok=%v err=%v", ok, err)
	}
	// Two runtime instances never share a stamp for identical inputs.
	other := projection.CatalogViewResolver{Registry: reg, SessionOverlay: overlay, Catalog: tools.NewCatalog(), OwnerResolver: mcp.NewRegistry()}
	otherStamp, _, _, err := other.CatalogStamp(ctx, b, projAgent, "configuration")
	if err != nil || otherStamp == stampFor(b) {
		t.Fatalf("two instances share a stamp (err=%v)", err)
	}
}

type noGenerationCatalog struct{ tools.ToolCatalog }

func newRegistryOver(t *testing.T, st state.StateStore) agentcfg.Registry {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return reg
}
