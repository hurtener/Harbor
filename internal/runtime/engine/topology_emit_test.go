package engine_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// newTestBus opens a real in-mem EventBus for an engine topology-emit
// test. No mocks at the seam (CLAUDE.md §17.3).
func newTestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1000,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func emitTestNode(name string) engine.Node {
	return engine.Node{
		Name: name,
		Func: func(_ context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			return env, nil
		},
	}
}

// TestNew_WithEventBus_PublishesTopologyChanged — engine.New with a bus
// publishes one topology.changed event carrying the initial projection.
// The subscriber opens an Admin-scoped subscription (the emit uses the
// engine's synthetic system identity) and uses the bus's replay-or-live
// stream to catch the construction-time event.
func TestNew_WithEventBus_PublishesTopologyChanged(t *testing.T) {
	bus := newTestBus(t)

	// Subscribe BEFORE constructing the engine so the live stream
	// catches the construction-time emit. Admin filter — the emit
	// carries the engine's synthetic (system/engine/<id>) identity.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Types: []events.EventType{events.EventTypeTopologyChanged},
		Admin: true,
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	in := emitTestNode("ingress")
	out := emitTestNode("egress")
	_, err = engine.New([]engine.Adjacency{{From: in, To: []engine.Node{out}}},
		engine.WithEventBus(bus))
	if err != nil {
		t.Fatalf("engine.New(WithEventBus): %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeTopologyChanged {
			t.Fatalf("event type = %q, want topology.changed", ev.Type)
		}
		payload, ok := ev.Payload.(events.TopologyChangedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want events.TopologyChangedPayload (SafePayload preserved)", ev.Payload)
		}
		proj := payload.Projection
		if proj.EngineID == "" {
			t.Error("projection EngineID is empty")
		}
		if len(proj.Nodes) != 2 {
			t.Errorf("projection Nodes len = %d, want 2", len(proj.Nodes))
		}
		if len(proj.Edges) != 1 {
			t.Errorf("projection Edges len = %d, want 1", len(proj.Edges))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no topology.changed event within 2s of engine.New")
	}
}

// TestNew_WithoutEventBus_PublishesNothing — the Phase 02 default
// (no WithEventBus) emits nothing; a subscriber sees no event.
func TestNew_WithoutEventBus_PublishesNothing(t *testing.T) {
	bus := newTestBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Types: []events.EventType{events.EventTypeTopologyChanged},
		Admin: true,
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	in := emitTestNode("ingress")
	out := emitTestNode("egress")
	if _, err := engine.New([]engine.Adjacency{{From: in, To: []engine.Node{out}}}); err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected event %q — an engine built without WithEventBus must emit nothing", ev.Type)
	case <-time.After(200 * time.Millisecond):
		// expected — no emit.
	}
}

// TestNew_WithEventBus_SecondEngineDiffersByOneEdge — re-constructing
// with one more adjacency emits a second topology.changed whose Edges
// differ by exactly one entry (the acceptance-criteria edge-delta).
func TestNew_WithEventBus_SecondEngineDiffersByOneEdge(t *testing.T) {
	bus := newTestBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Types: []events.EventType{events.EventTypeTopologyChanged},
		Admin: true,
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	a := emitTestNode("a")
	b := emitTestNode("b")
	c := emitTestNode("c")

	// Engine 1: a → b.
	if _, err := engine.New([]engine.Adjacency{{From: a, To: []engine.Node{b}}},
		engine.WithEventBus(bus)); err != nil {
		t.Fatalf("engine.New #1: %v", err)
	}
	// Engine 2: a → b, b → c (one more edge).
	if _, err := engine.New([]engine.Adjacency{
		{From: a, To: []engine.Node{b}},
		{From: b, To: []engine.Node{c}},
	}, engine.WithEventBus(bus)); err != nil {
		t.Fatalf("engine.New #2: %v", err)
	}

	first := waitTopologyEvent(t, sub)
	second := waitTopologyEvent(t, sub)
	if len(first.Edges) != 1 {
		t.Fatalf("engine #1 Edges len = %d, want 1", len(first.Edges))
	}
	if len(second.Edges) != 2 {
		t.Fatalf("engine #2 Edges len = %d, want 2", len(second.Edges))
	}
	if second.EngineID == first.EngineID {
		t.Error("two distinct engines shared an EngineID")
	}
}

// waitTopologyEvent drains one topology.changed event off the
// subscription within a bounded window and returns its projection.
func waitTopologyEvent(t *testing.T, sub events.Subscription) types.TopologyProjection {
	t.Helper()
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(events.TopologyChangedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want events.TopologyChangedPayload", ev.Payload)
		}
		return payload.Projection
	case <-time.After(2 * time.Second):
		t.Fatal("no topology.changed event within 2s")
		return types.TopologyProjection{}
	}
}
