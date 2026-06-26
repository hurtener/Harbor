package durable_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// TestDurable_Fence_DropsLateEventsAndEmptiesHistory is the driver-level
// regression for the session-erasure gap: after Fence(triple) the durable
// bus drops the triple's future events (no new entry/head persisted) and
// reads its history as empty — so a task finishing AFTER an erasure sweep
// can never re-create retained history under the erased triple.
func TestDurable_Fence_DropsLateEventsAndEmptiesHistory(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	ctx := context.Background()
	id := quad("t-f", "u-f", "s-f")
	hr := bus.(events.HistoryReplayer)
	fencer := bus.(events.Fencer)

	publishN(t, bus, id, 3)
	if _, _, _, err := hr.Bounds(ctx, filterFor(id)); err != nil {
		t.Fatalf("pre-fence Bounds = %v, want non-empty history", err)
	}

	// Fence + sweep the persisted records (mimics the cascade's DeleteScope).
	if err := fencer.Fence(ctx, id.Identity); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if _, err := store.DeleteScope(ctx, id.Identity); err != nil {
		t.Fatalf("DeleteScope: %v", err)
	}

	// The late event from the defunct task: a graceful, fenced drop that
	// persists NOTHING.
	late := events.Event{Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("late")}
	if err := bus.Publish(ctx, late); err != nil {
		t.Fatalf("late publish should drop gracefully, got: %v", err)
	}

	// History reads empty, and no durable head was re-created.
	if _, _, _, err := hr.Bounds(ctx, filterFor(id)); !errors.Is(err, events.ErrNoHistory) {
		t.Fatalf("post-fence Bounds = %v, want ErrNoHistory", err)
	}
	if evs, err := hr.Window(ctx, 0, 100, filterFor(id)); err != nil || len(evs) != 0 {
		t.Fatalf("post-fence Window = (%d, %v), want (0, nil)", len(evs), err)
	}
	if _, err := store.Load(ctx, id, "events.durable.head"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("late event re-created durable head: %v", err)
	}

	// Unfence restores normal retention (a reused session id).
	if err := fencer.Unfence(ctx, id.Identity); err != nil {
		t.Fatalf("Unfence: %v", err)
	}
	if err := bus.Publish(ctx, events.Event{Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("fresh")}); err != nil {
		t.Fatalf("post-unfence publish: %v", err)
	}
	if _, _, _, err := hr.Bounds(ctx, filterFor(id)); err != nil {
		t.Fatalf("post-unfence Bounds = %v, want retained history", err)
	}
}

// TestDurable_Fence_AfterClose asserts Fence / Unfence fail loud after Close.
func TestDurable_Fence_AfterClose(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fencer := bus.(events.Fencer)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if err := fencer.Fence(context.Background(), id); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Fence after close = %v, want ErrBusClosed", err)
	}
	if err := fencer.Unfence(context.Background(), id); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Unfence after close = %v, want ErrBusClosed", err)
	}
}
