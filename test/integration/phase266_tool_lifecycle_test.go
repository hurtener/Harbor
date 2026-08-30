// Phase 266 integration gate for universal tool lifecycle publication.
//
// The test keeps the real Catalog and durable EventBus in the path. Its only
// test seam blocks the real StateStore transaction so caller/barrier behavior
// is observable without inventing a production fake.
package integration_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

type phase266ToolBlockingStore struct {
	state.StateStore
	armed     atomic.Bool
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	freeOnce  sync.Once
}

func (s *phase266ToolBlockingStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	if s.armed.Load() {
		s.enterOnce.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

func (s *phase266ToolBlockingStore) free() {
	s.freeOnce.Do(func() { close(s.release) })
}

func phase266ToolEventsConfig() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Hour,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
		LegacyWritersDrained:     true,
	}
}

func phase266ToolID() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "phase266-tool-tenant",
			UserID:    "phase266-tool-user",
			SessionID: "phase266-tool-session",
		},
		RunID: "phase266-tool-run",
	}
}

func phase266ToolFilter(id identity.Quadruple) events.Filter {
	return events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Run:     id.RunID,
	}
}

func phase266ToolReplayEventually(t *testing.T, rp events.Replayer, id identity.Quadruple, want int) []events.Event {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, phase266ToolFilter(id))
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		if len(got) >= want {
			return got
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("Replay returned %d events after 2s, want at least %d", len(got), want)
		}
	}
}

// TestE2E_Phase266_UniversalToolLifecycle_DoesNotWaitForSlowDurableStore
// exercises the real Catalog lifecycle wrapper. Once Phase 266 routes
// tool.invoked/tool.completed through the ordered async lane, invoking a real
// registered tool must return while the durable write is blocked, then replay
// both lifecycle records in canonical order. The pre-Phase-266 baseline is
// expected to fail this gate because its lifecycle wrapper publishes inline.
func TestE2E_Phase266_UniversalToolLifecycle_DoesNotWaitForSlowDurableStore(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close(context.Background()) })
	store := &phase266ToolBlockingStore{
		StateStore: inner,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	bus, err := durable.New(context.Background(), phase266ToolEventsConfig(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("durable bus does not implement events.Replayer")
	}

	id := phase266ToolID()
	ctx, err := identity.With(context.Background(), id.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id.Identity, id.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	catalog := tools.NewCatalog(tools.WithCatalogBus(bus))
	if err := catalog.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:      "phase266-tool",
			Transport: tools.TransportInProcess,
		},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"ok": true}}, nil
		},
	}); err != nil {
		t.Fatalf("catalog.Register: %v", err)
	}
	desc, ok := catalog.Resolve("phase266-tool")
	if !ok {
		t.Fatal("catalog.Resolve did not find phase266-tool")
	}

	store.armed.Store(true)
	invokeDone := make(chan error, 1)
	go func() {
		_, invokeErr := desc.Invoke(ctx, json.RawMessage("{}"))
		invokeDone <- invokeErr
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		store.free()
		t.Fatal("tool lifecycle did not reach the blocked durable write")
	}
	select {
	case invokeErr := <-invokeDone:
		if invokeErr != nil {
			store.free()
			t.Fatalf("tool invocation failed before barrier release: %v", invokeErr)
		}
		// The caller returned while the durable write remained blocked. Release
		// the write before checking the eventual durable lifecycle records.
		store.free()
	case <-time.After(250 * time.Millisecond):
		store.free()
		select {
		case <-invokeDone:
			t.Fatal("tool lifecycle invocation remained coupled to durable persistence")
		case <-time.After(2 * time.Second):
			t.Fatal("tool invocation did not finish after durable release")
		}
		return
	}

	got := phase266ToolReplayEventually(t, rp, id, 2)
	if len(got) != 2 {
		t.Fatalf("replay returned %d events, want exactly 2: %#v", len(got), got)
	}
	if got[0].Type != tools.EventTypeToolInvoked || got[0].Sequence != 1 {
		t.Errorf("first lifecycle event = (%d,%q), want (1,%q)", got[0].Sequence, got[0].Type, tools.EventTypeToolInvoked)
	}
	if got[1].Type != tools.EventTypeToolCompleted || got[1].Sequence != 2 {
		t.Errorf("second lifecycle event = (%d,%q), want (2,%q)", got[1].Sequence, got[1].Type, tools.EventTypeToolCompleted)
	}
	for i, event := range got {
		if event.Identity != id {
			t.Errorf("lifecycle event %d identity = %+v, want %+v", i, event.Identity, id)
		}
	}
}
