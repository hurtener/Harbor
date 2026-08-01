package durable_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// toggleGroupFailStore wraps a StateStore and, when armed, fails Save
// for group records only (task records still persist) — to drive the
// engine's spawn-into-group compensation path through the CONCRETE
// durable backend (its DeleteTask removes the already-persisted task
// slot via store.Delete).
type toggleGroupFailStore struct {
	state.StateStore
	failGroups atomic.Bool
}

func (s *toggleGroupFailStore) Save(ctx context.Context, rec state.StateRecord) error {
	if s.failGroups.Load() && strings.HasPrefix(rec.Kind, "task.durable.group/") {
		return errors.New("group disk full")
	}
	return s.StateStore.Save(ctx, rec)
}

func (s *toggleGroupFailStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if s.failGroups.Load() && strings.HasPrefix(next.Kind, "task.durable.group/") {
		return errors.New("group disk full")
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

// TestDurable_GroupPersistFailure_CompensatesTaskRecord exercises the
// real durable DeleteTask: a spawn whose group persist fails AFTER the
// task was persisted must compensate the task slot, so a restart does
// not resurrect a spawn the caller was told failed (§17.3 real-driver
// coverage of the compensation seam; wave-1 audit WARN).
func TestDurable_GroupPersistFailure_CompensatesTaskRecord(t *testing.T) {
	inner := newStore(t)
	store := &toggleGroupFailStore{StateStore: inner}
	bus1 := mkBus(t)
	r1 := openOver(t, store, bus1)
	id := quadA()
	ctx := ctxFor(t, id.Identity)

	g, err := r1.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: id.Identity})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}

	store.failGroups.Store(true)
	if _, err := r1.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, GroupID: g.ID, IdempotencyKey: "k1"}); err == nil {
		t.Fatal("Spawn with failing group persist: want error, got nil")
	}

	// Reopen over the SAME store: the compensated task must NOT resurrect.
	_ = r1.Close(context.Background())
	_ = bus1.Close(context.Background())
	store.failGroups.Store(false)

	bus2 := mkBus(t)
	r2 := openOver(t, store, bus2)
	defer func() {
		_ = r2.Close(context.Background())
		_ = bus2.Close(context.Background())
		_ = inner.Close(context.Background())
	}()
	list, err := r2.List(ctx, id.Identity, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List after restart: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("durable DeleteTask did not compensate: %d task(s) resurrected after restart", len(list))
	}
}
