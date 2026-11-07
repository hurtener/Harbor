package engine_test

import (
	"context"
	"errors"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// TestGetPersisted_FallsBackToPersistedRecord pins the cross-instance
// read: a task this engine never held in memory (another instance
// sharing the store persisted it) is served from the backend record
// under the exact identity, while Get keeps reporting it absent.
func TestGetPersisted_FallsBackToPersistedRecord(t *testing.T) {
	owner := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	other := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-other"}
	be := &memBackend{persisted: map[tasks.TaskID]*tasks.Task{
		"task-remote": {ID: "task-remote", Identity: owner, Query: "what is the weather", AgentID: "agent-a"},
		"task-forged": {ID: "task-other-id", Identity: owner, Query: "x"},
	}}
	bus := mkBus(t)
	eng, err := engine.New(bus, auditpatterns.New(), be)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()); _ = bus.Close(context.Background()) })

	ctx, _ := identity.With(context.Background(), owner.Identity)
	if _, err := eng.Get(ctx, "task-remote"); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("Get of a remote-only task: %v; want ErrNotFound (in-memory semantics unchanged)", err)
	}
	got, err := eng.GetPersisted(ctx, "task-remote")
	if err != nil || got.Query != "what is the weather" || got.AgentID != "agent-a" {
		t.Fatalf("GetPersisted = %+v, %v; want the persisted record", got, err)
	}
	// A record whose id differs from the requested slot is never served.
	if _, err := eng.GetPersisted(ctx, "task-forged"); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("mismatched record id: %v; want ErrNotFound", err)
	}
	// Another session never sees the record.
	otherCtx, _ := identity.With(context.Background(), other)
	if _, err := eng.GetPersisted(otherCtx, "task-remote"); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("cross-session GetPersisted: %v; want ErrNotFound", err)
	}
	if _, err := eng.GetPersisted(ctx, "task-missing"); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("missing: %v; want ErrNotFound", err)
	}
	// A backend failure is loud, never a silent absence.
	be.mu.Lock()
	be.loadErr = errors.New("store down")
	be.mu.Unlock()
	if _, err := eng.GetPersisted(ctx, "task-remote"); err == nil || errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("backend failure: %v; want a loud non-NotFound error", err)
	}
	be.mu.Lock()
	be.loadErr = nil
	be.mu.Unlock()
	// A task in memory without a persisted record is served from memory.
	h, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: owner, Kind: tasks.KindForeground, Query: "local"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got, err := eng.GetPersisted(ctx, h.ID); err != nil || got.Query != "local" {
		t.Fatalf("GetPersisted of a local task = %+v, %v", got, err)
	}
	// A persisted record a peer advanced wins over the local copy.
	be.mu.Lock()
	be.persisted[h.ID] = &tasks.Task{ID: h.ID, Identity: owner, Query: "local", Status: tasks.StatusComplete}
	be.mu.Unlock()
	if got, err := eng.GetPersisted(ctx, h.ID); err != nil || got.Status != tasks.StatusComplete {
		t.Fatalf("GetPersisted with a newer persisted record = %+v, %v; want the persisted record", got, err)
	}
}
