package durable_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestDurable_GetPersisted_SeesOtherInstanceRecord pins the rolling-
// deploy overlap read: two durable registries share one StateStore
// (the old and the new instance). A task spawned on instance A is not
// in instance B's memory — Get reports it absent — but GetPersisted
// serves A's persisted record (query and agent intact) under the exact
// identity, and never across sessions.
func TestDurable_GetPersisted_SeesOtherInstanceRecord(t *testing.T) {
	store := newStore(t)
	busA, busB := mkBus(t), mkBus(t)
	a := openOver(t, store, busA)
	b := openOver(t, store, busB)
	t.Cleanup(func() {
		ctx := context.Background()
		_ = a.Close(ctx)
		_ = b.Close(ctx)
		_ = busA.Close(ctx)
		_ = busB.Close(ctx)
		_ = store.Close(ctx)
	})
	ctx := ctxFor(t, quadA().Identity)
	h, err := a.Spawn(ctx, tasks.SpawnRequest{Identity: quadA(), Kind: tasks.KindForeground, Query: "hello from the new instance", AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("spawn on A: %v", err)
	}
	if _, err := b.Get(ctx, h.ID); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("B.Get: %v; want ErrNotFound (not in B's memory)", err)
	}
	got, err := b.GetPersisted(ctx, h.ID)
	if err != nil {
		t.Fatalf("B.GetPersisted: %v", err)
	}
	if got.ID != h.ID || got.Query != "hello from the new instance" || got.AgentID != "agent-1" {
		t.Fatalf("B.GetPersisted = %+v; want A's record", got)
	}
	other := ctxFor(t, identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-other"})
	if _, err := b.GetPersisted(other, h.ID); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("cross-session GetPersisted: %v; want ErrNotFound", err)
	}
}
