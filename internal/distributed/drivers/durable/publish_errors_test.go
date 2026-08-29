package durable_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// failingSaveStore wraps a real StateStore but forces Save to error.
type failingSaveStore struct {
	state.StateStore
	err error
}

func (f failingSaveStore) Save(context.Context, state.StateRecord) error { return f.err }

func (f failingSaveStore) SaveIf(context.Context, []state.SlotExpectation, state.StateRecord) error {
	return f.err
}

// TestDurable_PublishCtxCancelled asserts a cancelled context fails the
// publish loudly (honours ctx.Err() up front).
func TestDurable_PublishCtxCancelled(t *testing.T) {
	store := newStore(t)
	eb := newEventBus(t)
	b := openOver(t, store, eb)
	defer func() {
		ctx := context.Background()
		_ = b.Close(ctx)
		_ = eb.Close(ctx)
		_ = store.Close(ctx)
	}()

	id := triple()
	ctx, cancel := context.WithCancel(ctxFor(t, id.Identity))
	cancel()
	if err := b.Publish(ctx, envelope("x", id, "evt")); err == nil {
		t.Fatal("publish with cancelled ctx: want error, got nil")
	}
}

// TestDurable_PublishPersistError asserts a StateStore Save failure
// surfaces from Publish (fail-loud, no silent drop) and that the
// projection does not happen on a failed persist.
func TestDurable_PublishPersistError(t *testing.T) {
	inner := newStore(t)
	eb := newEventBus(t)
	b, err := distributed.OpenBusDriver("durable", distributed.Dependencies{
		EventBus: eb,
		State:    failingSaveStore{StateStore: inner, err: errors.New("disk full")},
		Cfg:      config.DistributedConfig{BusDriver: "durable", BusPollInterval: testPollInterval},
	})
	if err != nil {
		t.Fatalf("OpenBusDriver: %v", err)
	}
	defer func() {
		ctx := context.Background()
		_ = b.Close(ctx)
		_ = eb.Close(ctx)
		_ = inner.Close(ctx)
	}()

	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	sub := subscribe(t, eb, id.Identity)
	defer sub.Cancel()

	if err := b.Publish(ctxFor(t, id.Identity), envelope("x", id, "evt")); err == nil {
		t.Fatal("publish with failing store: want error, got nil")
	}
	// No projection should have happened (persist failed before project).
	if edges := collectEdges(sub, 1, 300*time.Millisecond); len(edges) != 0 {
		t.Errorf("failed persist still projected: %v", edges)
	}
}

// failFirstProjectBus wraps an EventBus and fails the first Publish
// (the durable bus's local projection), then delegates — so the
// poller's later re-projection succeeds.
type failFirstProjectBus struct {
	events.EventBus
	failsLeft atomic.Int32
}

func (f *failFirstProjectBus) Publish(ctx context.Context, ev events.Event) error {
	if f.failsLeft.Add(-1) >= 0 {
		return errors.New("transient project failure")
	}
	return f.EventBus.Publish(ctx, ev)
}

// TestDurable_ProjectFailure_PollerRedelivers asserts that when a
// publish's LOCAL projection fails after the envelope is durably
// persisted, the poller re-delivers it (no permanent local-delivery
// gap) — the adversarial F1 fix.
func TestDurable_ProjectFailure_PollerRedelivers(t *testing.T) {
	store := newStore(t)
	inner := newEventBus(t)
	wrapper := &failFirstProjectBus{EventBus: inner}
	wrapper.failsLeft.Store(1) // fail exactly the first (publish-path) projection

	b, err := distributed.OpenBusDriver("durable", distributed.Dependencies{
		EventBus: wrapper,
		State:    store,
		Cfg:      config.DistributedConfig{BusDriver: "durable", BusPollInterval: testPollInterval},
	})
	if err != nil {
		t.Fatalf("OpenBusDriver: %v", err)
	}
	defer func() {
		ctx := context.Background()
		_ = b.Close(ctx)
		_ = inner.Close(ctx)
		_ = store.Close(ctx)
	}()

	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	sub := subscribe(t, wrapper, id.Identity)
	defer sub.Cancel()

	// The publish's local projection fails → Publish returns an error...
	if err := b.Publish(ctxFor(t, id.Identity), envelope("retry", id, "evt-retry")); err == nil {
		t.Fatal("expected publish projection error, got nil")
	}
	// ...but the envelope was persisted, and the poller re-delivers it.
	if edges := collectEdges(sub, 1, 8*time.Second); len(edges) != 1 || edges[0] != "retry" {
		t.Fatalf("poller did not re-deliver after a failed local projection: %v", edges)
	}
}
