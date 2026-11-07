package assemble

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

type hintRecorder struct {
	events.EventBus
	t     *testing.T
	mu    sync.Mutex
	hints []events.Event
}

func (b *hintRecorder) Publish(ctx context.Context, ev events.Event) error {
	if err := identity.Validate(ev.Identity.Identity); err != nil {
		b.t.Errorf("hint published with invalid identity %+v: %v", ev.Identity, err)
	}
	if ev.Type == agentcfg.EventTypeToolsCatalogChanged {
		b.mu.Lock()
		b.hints = append(b.hints, ev)
		b.mu.Unlock()
	}
	return b.EventBus.Publish(ctx, ev)
}

func (b *hintRecorder) take() []events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.hints
	b.hints = nil
	return out
}

type hintServer struct {
	source tools.ToolSourceID
	mu     sync.Mutex
	names  []string
}

func (s *hintServer) SourceID() tools.ToolSourceID { return s.source }
func (s *hintServer) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tools.ToolDescriptor, 0, len(s.names))
	for _, n := range s.names {
		out = append(out, tools.ToolDescriptor{Tool: tools.Tool{Name: string(s.source) + "_" + n, Source: s.source}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }})
	}
	return out, nil
}
func (s *hintServer) DisplayModes() []string { return nil }
func (s *hintServer) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("none")
}
func (s *hintServer) Close(context.Context) error { return nil }

// TestMCPCatalogChangeHint is the H3 AC9 (c)/(d) half: owner-tagged MCP
// publication, discovery changes and withdrawal publish one hint each with a
// valid identity; a no-op discovery and a boot-declared server publish none.
func TestMCPCatalogChangeHint(t *testing.T) {
	inner, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close(context.Background()) })
	bus := &hintRecorder{EventBus: inner, t: t}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := mcpdrv.NewRegistry(mcpdrv.WithRegistryChangeObserver(mcpCatalogChangeHint(bus, logger)))
	catalog := tools.NewCatalog()
	ctx := context.Background()

	expect := func(what, scope, reason string) {
		t.Helper()
		hints := bus.take()
		if len(hints) != 1 {
			t.Fatalf("%s: %d hints, want 1", what, len(hints))
		}
		p := hints[0].Payload.(agentcfg.ToolsCatalogChangedPayload)
		if p.AgentID != "agent" || p.Scope != scope || p.Reason != reason || hints[0].Identity.TenantID != "tenant" {
			t.Fatalf("%s: hint=%+v identity=%+v", what, p, hints[0].Identity)
		}
	}

	owner := toolauth.Owner{Tenant: "tenant", Agent: "agent", User: "user"}
	server := &hintServer{source: tools.ToolSourceID(mcpdrv.PhysicalServerName("src", owner)), names: []string{"one"}}
	if err := registry.Register(ctx, mcpdrv.ServerRegistration{Provider: server, Transport: "streamable-http", Owner: owner, LogicalName: "src", DescriptorFingerprint: "fp", Catalog: catalog}); err != nil {
		t.Fatal(err)
	}
	expect("publish", agentcfg.CatalogChangeScopeUser, agentcfg.CatalogChangeReasonConnection)

	ownerCtx, err := identity.With(ctx, identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RefreshDiscovery(ownerCtx, string(server.source)); err != nil {
		t.Fatal(err)
	}
	expect("discovery change", agentcfg.CatalogChangeScopeUser, agentcfg.CatalogChangeReasonDiscovery)
	if _, err := registry.RefreshDiscovery(ownerCtx, string(server.source)); err != nil {
		t.Fatal(err)
	}
	if hints := bus.take(); len(hints) != 0 {
		t.Fatalf("no-op discovery published %d hints", len(hints))
	}
	if err := registry.Deregister(ctx, "src", owner); err != nil {
		t.Fatal(err)
	}
	expect("withdraw", agentcfg.CatalogChangeScopeUser, agentcfg.CatalogChangeReasonConnection)

	agentOwner := toolauth.Owner{Tenant: "tenant", Agent: "agent"}
	agentServer := &hintServer{source: tools.ToolSourceID(mcpdrv.PhysicalServerName("shared", agentOwner))}
	if err := registry.Register(ctx, mcpdrv.ServerRegistration{Provider: agentServer, Transport: "streamable-http", Owner: agentOwner, LogicalName: "shared", DescriptorFingerprint: "fp2", Catalog: catalog}); err != nil {
		t.Fatal(err)
	}
	expect("agent publish", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConnection)

	boot := &hintServer{source: "boot"}
	if err := registry.Register(ctx, mcpdrv.ServerRegistration{Provider: boot, Transport: "stdio"}); err != nil {
		t.Fatal(err)
	}
	if hints := bus.take(); len(hints) != 0 {
		t.Fatalf("boot-declared server published %d hints (no agent identity)", len(hints))
	}
}
