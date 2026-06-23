package durable_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

func triple() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"}}
}

func ctxFor(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func subscribe(t *testing.T, eb events.EventBus, id identity.Identity) events.Subscription {
	t.Helper()
	sub, err := eb.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{distributed.EventTypeDistributedBusEnvelope},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return sub
}

func envelope(edge string, id identity.Quadruple, eventID string) distributed.BusEnvelope {
	return distributed.BusEnvelope{
		Edge:      edge,
		Source:    "test",
		Identity:  id,
		TaskID:    "task-1",
		EventID:   events.EventID(eventID),
		Payload:   []byte(`{"k":"v"}`),
		Timestamp: time.Now().UTC(),
	}
}

// collectEdges reads up to `want` envelope projections from sub within
// the timeout, returning their edges. Fewer than `want` means a timeout.
func collectEdges(sub events.Subscription, want int, timeout time.Duration) []string {
	var edges []string
	deadline := time.After(timeout)
	for len(edges) < want {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return edges
			}
			if p, ok := ev.Payload.(distributed.BusEnvelopePayload); ok {
				edges = append(edges, p.Envelope.Edge)
			}
		case <-deadline:
			return edges
		}
	}
	return edges
}

// TestDurable_RestartReplay asserts envelopes published before a restart
// are re-projected onto a fresh instance's event bus over the same store.
func TestDurable_RestartReplay(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	id := triple()

	// Instance 1: publish 3, then "crash" (close bus + its event bus).
	eb1 := newEventBus(t)
	b1 := openOver(t, store, eb1)
	for i := range 3 {
		if err := b1.Publish(ctxFor(t, id.Identity), envelope(fmt.Sprintf("edge-%d", i), id, fmt.Sprintf("evt-%d", i))); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	_ = b1.Close(context.Background())
	_ = eb1.Close(context.Background())

	// Instance 2: fresh event bus, subscribe BEFORE opening so the
	// poller's replayed projections are captured.
	eb2 := newEventBus(t)
	defer func() { _ = eb2.Close(context.Background()) }()
	sub := subscribe(t, eb2, id.Identity)
	defer sub.Cancel()

	b2 := openOver(t, store, eb2)
	defer func() { _ = b2.Close(context.Background()) }()

	edges := collectEdges(sub, 3, 8*time.Second)
	if len(edges) != 3 {
		t.Fatalf("restart-replay: got %d projected envelopes, want 3 (edges=%v)", len(edges), edges)
	}
}

// TestDurable_CrossInstanceFanout asserts an envelope published on one
// instance is projected onto a second instance's event bus (the two
// share one store), exactly once on each side.
func TestDurable_CrossInstanceFanout(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	id := triple()

	ebA := newEventBus(t)
	defer func() { _ = ebA.Close(context.Background()) }()
	subA := subscribe(t, ebA, id.Identity)
	defer subA.Cancel()
	bA := openOver(t, store, ebA)
	defer func() { _ = bA.Close(context.Background()) }()

	ebB := newEventBus(t)
	defer func() { _ = ebB.Close(context.Background()) }()
	subB := subscribe(t, ebB, id.Identity)
	defer subB.Cancel()
	bB := openOver(t, store, ebB)
	defer func() { _ = bB.Close(context.Background()) }()

	// Publish 3 on A (N>1 exercises the receiver-side dedup across many
	// poll ticks).
	const n = 3
	for i := range n {
		if err := bA.Publish(ctxFor(t, id.Identity), envelope(fmt.Sprintf("cross-%d", i), id, fmt.Sprintf("evt-%d", i))); err != nil {
			t.Fatalf("publish %d on A: %v", i, err)
		}
	}

	// B receives all 3 (cross-instance via the poller) and NOT a single
	// duplicate over subsequent ticks — the poller must not re-project on
	// every tick.
	if edges := collectEdges(subB, n, 8*time.Second); len(edges) != n {
		t.Fatalf("cross-instance: B got %d envelopes, want %d (edges=%v)", len(edges), n, edges)
	}
	if extra := collectEdges(subB, 1, 400*time.Millisecond); len(extra) != 0 {
		t.Errorf("B received a DUPLICATE (poller re-projected across ticks): %v", extra)
	}
	// A received each once (local project); the poller must NOT
	// re-project A's own envelopes.
	if edges := collectEdges(subA, n, 8*time.Second); len(edges) != n {
		t.Fatalf("A self-delivery: got %d, want exactly %d (edges=%v)", len(edges), n, edges)
	}
	if extra := collectEdges(subA, 1, 400*time.Millisecond); len(extra) != 0 {
		t.Errorf("A re-projected its own envelope (self double-project): %v", extra)
	}
}
