package inmem_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// TestInmem_Fence_DropsAndEmptiesHistory asserts the inmem driver's
// events.Fencer contract: Fence purges the triple's retained ring entries
// AND drops its future events, so a fenced session reads empty history;
// Unfence restores normal retention for a reused id.
func TestInmem_Fence_DropsAndEmptiesHistory(t *testing.T) {
	bus, _ := newReplayBus(t)
	ctx := context.Background()
	id := mkID(1).Identity
	hr := bus.(events.HistoryReplayer)
	fencer := bus.(events.Fencer)
	f := events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	// Retain a couple of events for the session.
	if err := bus.Publish(ctx, mkEvent(1)); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	if err := bus.Publish(ctx, mkEvent(1)); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if _, _, _, err := hr.Bounds(ctx, f); err != nil {
		t.Fatalf("pre-fence Bounds = %v, want non-empty history", err)
	}

	// Fence: already-retained ring entries are purged + the triple reads
	// empty.
	if err := fencer.Fence(ctx, id); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if _, _, _, err := hr.Bounds(ctx, f); !errors.Is(err, events.ErrNoHistory) {
		t.Fatalf("post-fence Bounds = %v, want ErrNoHistory", err)
	}

	// A late event for the fenced triple is a graceful drop, not retained.
	if err := bus.Publish(ctx, mkEvent(1)); err != nil {
		t.Fatalf("fenced publish should drop gracefully: %v", err)
	}
	if _, _, _, err := hr.Bounds(ctx, f); !errors.Is(err, events.ErrNoHistory) {
		t.Fatalf("post-fence-publish Bounds = %v, want ErrNoHistory", err)
	}
	if evs, err := hr.Window(ctx, 0, 100, f); err != nil || len(evs) != 0 {
		t.Fatalf("post-fence Window = (%d events, %v), want (0, nil)", len(evs), err)
	}

	// A DIFFERENT session is unaffected by the fence (no cross-session bleed).
	other := mkID(2).Identity
	if err := bus.Publish(ctx, mkEvent(2)); err != nil {
		t.Fatalf("publish other: %v", err)
	}
	of := events.Filter{Tenant: other.TenantID, User: other.UserID, Session: other.SessionID}
	if _, _, _, err := hr.Bounds(ctx, of); err != nil {
		t.Fatalf("other-session Bounds = %v, want retained history", err)
	}

	// Unfence: the triple retains events again.
	if err := fencer.Unfence(ctx, id); err != nil {
		t.Fatalf("Unfence: %v", err)
	}
	if err := bus.Publish(ctx, mkEvent(1)); err != nil {
		t.Fatalf("post-unfence publish: %v", err)
	}
	if _, _, _, err := hr.Bounds(ctx, f); err != nil {
		t.Fatalf("post-unfence Bounds = %v, want retained history", err)
	}
}

// TestInmem_Fence_AfterClose asserts Fence / Unfence fail loud after Close.
func TestInmem_Fence_AfterClose(t *testing.T) {
	bus, _ := newReplayBus(t)
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
