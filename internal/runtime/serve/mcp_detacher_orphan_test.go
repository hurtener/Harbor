package serve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// publishDuringWithdrawCatalog opens the race window deterministically: the
// first orphan withdrawal starts a concurrent publication of the same name
// and gives it a bounded head start before withdrawing.
type publishDuringWithdrawCatalog struct {
	tools.ToolCatalog
	inner   interface{ DeregisterSource(tools.ToolSourceID) int }
	publish func()
	once    sync.Once
	done    chan struct{}
}

func (c *publishDuringWithdrawCatalog) DeregisterSource(source tools.ToolSourceID) int {
	c.once.Do(func() {
		c.done = make(chan struct{})
		go func() {
			defer close(c.done)
			c.publish()
		}()
		select {
		case <-c.done:
		case <-time.After(300 * time.Millisecond):
		}
	})
	return c.inner.DeregisterSource(source)
}

// TestDetachSource_OrphanCleanupNeverStripsConcurrentPublication: the
// generic detach of a name not registered for the caller withdraws orphaned
// catalog entries only when no registration holds the physical name, decided
// atomically with the withdrawal under the registry lock. A registration
// published concurrently therefore never loses its catalog tools.
func TestDetachSource_OrphanCleanupNeverStripsConcurrentPublication(t *testing.T) {
	bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	owner := toolauth.Owner{Tenant: "tenant", Agent: "agent"}
	physical := mcpdrv.PhysicalServerName("orphan", owner)
	id := identity.Identity{TenantID: "tenant", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	registry := mcpdrv.NewRegistry()
	inner := tools.NewCatalog()
	provider, err := mcpdrv.New(mcpdrv.Config{Name: physical, URL: "http://example.invalid/mcp", Bus: bus, DefaultIdentity: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	deregisterer, ok := inner.(interface{ DeregisterSource(tools.ToolSourceID) int })
	if !ok {
		t.Fatal("catalog cannot deregister sources")
	}
	catalog := &publishDuringWithdrawCatalog{ToolCatalog: inner, inner: deregisterer, publish: func() {
		swap, err := registry.StageRegistration(mcpdrv.ServerRegistration{Provider: provider, Transport: "streamable-http", Owner: owner, LogicalName: "orphan"}, nil)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := swap.Publish(context.Background(), func() error {
			return inner.Register(tools.ToolDescriptor{
				Tool: tools.Tool{Name: physical + "_echo", Source: tools.ToolSourceID(physical), Transport: tools.TransportInProcess,
					SideEffects: tools.SideEffectRead, Loading: tools.LoadingAlways},
				Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil },
			})
		}); err != nil {
			t.Error(err)
		}
	}}
	if err := detachSource(ctx, catalog, registry, "orphan", owner, slog.New(slog.NewTextHandler(io.Discard, nil)), "test"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	<-catalog.done
	if _, _, live := registry.RegistrationIdentity(physical); !live {
		t.Fatal("fixture: concurrent publication did not land")
	}
	if _, ok := inner.Resolve(physical + "_echo"); !ok {
		t.Fatal("orphan cleanup stripped a concurrently published registration's tools")
	}
}
