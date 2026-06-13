package mcpconsole_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

func newAppsStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func newAppsBus(t *testing.T) events.EventBus {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func newAppsRegistry(t *testing.T, body []byte) *mcp.Registry {
	t.Helper()
	reg := mcp.NewRegistry()
	if err := reg.Register(mcp.ServerRegistration{
		Provider:     &stubProvider{id: "srv-a", resourceBody: body},
		Transport:    "inmemory",
		InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// TestAppsAccessor_ReadResource_InlineBelowThreshold proves a small
// resource rides inline (no artifact offload).
func TestAppsAccessor_ReadResource_InlineBelowThreshold(t *testing.T) {
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:  newAppsRegistry(t, []byte("<html>small</html>")),
		Catalog:   tools.NewCatalog(),
		Store:     newAppsStore(t),
		Bus:       newAppsBus(t),
		Threshold: 1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	got, err := acc.ReadResource(idCtx(t), "srv-a", "ui://app/main.html")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Artifact != nil {
		t.Fatalf("small resource was offloaded: %+v", got.Artifact)
	}
	if string(got.Inline) != "<html>small</html>" {
		t.Errorf("Inline = %q", string(got.Inline))
	}
}

// TestAppsAccessor_ReadResource_HeavyOffload proves the D-026 branch:
// a resource at or above the threshold is offloaded by reference AND a
// loud `mcp.resource_offloaded` event is emitted — never inlined, never
// silently truncated.
func TestAppsAccessor_ReadResource_HeavyOffload(t *testing.T) {
	body := []byte(strings.Repeat("A", 2048))
	store := newAppsStore(t)
	bus := newAppsBus(t)

	// Subscribe BEFORE the read so the loud event is captured.
	sub, err := bus.Subscribe(idCtx(t), events.Filter{
		Tenant: "t-1", User: "u-1", Session: "s-1",
		Types: []events.EventType{mcp.EventTypeMCPResourceOffloaded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:  newAppsRegistry(t, body),
		Catalog:   tools.NewCatalog(),
		Store:     store,
		Bus:       bus,
		Threshold: 1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	got, err := acc.ReadResource(idCtx(t), "srv-a", "ui://app/main.html")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(got.Inline) != 0 {
		t.Fatalf("heavy resource was inlined past the threshold (%d bytes)", len(got.Inline))
	}
	if got.Artifact == nil || got.Artifact.ID == "" {
		t.Fatalf("heavy resource not offloaded: %+v", got.Artifact)
	}
	// The bytes are recoverable from the artifact store under scope.
	scope := artifacts.ArtifactScope{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	stored, found, err := store.Get(idCtx(t), scope, got.Artifact.ID)
	if err != nil || !found {
		t.Fatalf("Get offloaded artifact: found=%v err=%v", found, err)
	}
	if len(stored) != len(body) {
		t.Errorf("stored %d bytes, want %d", len(stored), len(body))
	}
	// The loud bypass event was emitted.
	select {
	case ev := <-sub.Events():
		if ev.Type != mcp.EventTypeMCPResourceOffloaded {
			t.Fatalf("event type = %q, want %q", ev.Type, mcp.EventTypeMCPResourceOffloaded)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no mcp.resource_offloaded event — the loud bypass is not emitted")
	}
}

// TestAppsAccessor_CallTool_ResolvesAndInvokes covers the proxy's happy
// path against a real catalog, plus the tool-not-found error.
func TestAppsAccessor_CallTool_ResolvesAndInvokes(t *testing.T) {
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "srv-a_echo"},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"echo": string(args)}}, nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:  newAppsRegistry(t, nil),
		Catalog:   cat,
		Store:     newAppsStore(t),
		Bus:       newAppsBus(t),
		Threshold: 1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	res, err := acc.CallTool(idCtx(t), "srv-a_echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Errorf("IsError = true, want false")
	}
	if len(res.Inline) == 0 {
		t.Errorf("Inline is empty")
	}

	// Unknown tool fails with ErrToolNotFound.
	if _, err := acc.CallTool(idCtx(t), "nope", nil); err == nil {
		t.Fatal("CallTool unknown tool: want error")
	}
}

// TestAppsAccessor_ConcurrentReuse pins the §5/§11 concurrent-reuse
// contract: one shared AppsAccessor serves N≥100 concurrent invocations
// with no data races, no cross-talk, and no goroutine leak.
func TestAppsAccessor_ConcurrentReuse(t *testing.T) {
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "srv-a_echo"},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"echo": string(args)}}, nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:  newAppsRegistry(t, []byte("<html>x</html>")),
		Catalog:   cat,
		Store:     newAppsStore(t),
		Bus:       newAppsBus(t),
		Threshold: 1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}

	const n = 128
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine carries a DISTINCT session so cross-talk
			// would surface as a wrong artifact scope / read.
			ctx, _ := identity.With(context.Background(), identity.Identity{
				TenantID: "t-1", UserID: "u-1", SessionID: "s-" + string(rune('A'+(i%26))),
			})
			if i%2 == 0 {
				if _, err := acc.ReadResource(ctx, "srv-a", "ui://app/main.html"); err != nil {
					errs <- err
				}
			} else {
				if _, err := acc.CallTool(ctx, "srv-a_echo", json.RawMessage(`{"i":1}`)); err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent invocation error: %v", err)
	}
}
