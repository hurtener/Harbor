// Integration test for the StateStore-backed durable TaskRegistry
// driver, exercised end-to-end against real production StateStore
// drivers (in-memory and SQLite) with a real events.EventBus on the
// seam.
//
// This is the binding §17 integration test for the durable task
// driver: its Deps span the tasks subsystem (the TaskRegistry surface)
// and the distributed idempotency contract, over the state subsystem.
// Real drivers everywhere on the seam — no mocks. Identity propagation
// is asserted through every layer; the spawn→restart→records-intact
// scenario and the crash-left-Running→recovery scenario are the
// acceptance criteria; a forced StateStore write error is the failure
// mode; the whole suite runs under -race.
//
// SQLite uses a file DSN so a full close+reopen is a TRUE process-style
// restart (the in-memory driver only survives an in-process driver
// reopen, so its store instance is shared across the two driver opens).
package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/drivers/durable"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// restartableStore opens a StateStore over a fixed backing location, so
// the test can close one store and reopen another over the same data.
type restartableStore struct {
	name    string
	open    func(t *testing.T) state.StateStore
	closeIt bool // true when a true close+reopen persists (SQLite); false for in-mem (share the instance)
}

func durableTaskStores(t *testing.T) []restartableStore {
	t.Helper()
	// In-memory: share one instance across reopens (a close would drop
	// the data — in-mem survives only an in-process driver reopen).
	shared, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = shared.Close(context.Background()) })

	dsn := filepath.Join(t.TempDir(), "durable-tasks.db")
	return []restartableStore{
		{
			name:    "inmem",
			open:    func(_ *testing.T) state.StateStore { return shared },
			closeIt: false,
		},
		{
			name: "sqlite",
			open: func(t *testing.T) state.StateStore {
				s, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
				if err != nil {
					t.Fatalf("statesqlite.New(%q): %v", dsn, err)
				}
				return s
			},
			closeIt: true,
		},
	}
}

func mkInmemBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsinmem.New: %v", err)
	}
	return bus
}

func openDurableTasks(t *testing.T, store state.StateStore, bus events.EventBus) tasks.TaskRegistry {
	t.Helper()
	r, err := durable.New(tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "durable"},
	})
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	return r
}

func idCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestE2E_DurableTasks_RestartAndRecovery is the wave-level acceptance
// scenario: across each real StateStore driver, a durable registry
// persists tasks, survives a restart with records intact and identity
// isolation preserved, and recovers a crash-left Running task to
// Failed{runtime_restarted}.
func TestE2E_DurableTasks_RestartAndRecovery(t *testing.T) {
	for _, rc := range durableTaskStores(t) {
		t.Run(rc.name, func(t *testing.T) {
			idA := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"}}
			ctxA := idCtx(t, idA.Identity)

			store1 := rc.open(t)
			bus1 := mkInmemBus(t)
			r1 := openDurableTasks(t, store1, bus1)

			// A completed task, a still-pending task, and a task left
			// Running (the crash victim).
			done, err := r1.Spawn(ctxA, tasks.SpawnRequest{Identity: idA, Kind: tasks.KindBackground, Description: "done"})
			if err != nil {
				t.Fatalf("Spawn done: %v", err)
			}
			_ = r1.MarkRunning(ctxA, done.ID)
			if err := r1.MarkComplete(ctxA, done.ID, tasks.TaskResult{Value: []byte(`{"ok":true}`)}); err != nil {
				t.Fatalf("MarkComplete: %v", err)
			}
			pending, err := r1.Spawn(ctxA, tasks.SpawnRequest{Identity: idA, Kind: tasks.KindForeground, Description: "pending"})
			if err != nil {
				t.Fatalf("Spawn pending: %v", err)
			}
			crashed, err := r1.Spawn(ctxA, tasks.SpawnRequest{Identity: idA, Kind: tasks.KindBackground, Description: "crashed"})
			if err != nil {
				t.Fatalf("Spawn crashed: %v", err)
			}
			if err := r1.MarkRunning(ctxA, crashed.ID); err != nil {
				t.Fatalf("MarkRunning crashed: %v", err)
			}

			// Restart: tear down the registry + bus; for SQLite also close
			// and reopen the store (a true process restart).
			_ = r1.Close(context.Background())
			_ = bus1.Close(context.Background())
			if rc.closeIt {
				_ = store1.Close(context.Background())
			}

			store2 := rc.open(t)
			bus2 := mkInmemBus(t)
			r2 := openDurableTasks(t, store2, bus2)
			t.Cleanup(func() {
				_ = r2.Close(context.Background())
				_ = bus2.Close(context.Background())
				if rc.closeIt {
					_ = store2.Close(context.Background())
				}
			})

			// Completed task survived with its result.
			gotDone, err := r2.Get(ctxA, done.ID)
			if err != nil {
				t.Fatalf("Get(done) after restart: %v", err)
			}
			if gotDone.Status != tasks.StatusComplete || string(gotDone.Result.Value) != `{"ok":true}` {
				t.Errorf("done task = %q result=%s, want Complete {\"ok\":true}", gotDone.Status, gotDone.Result.Value)
			}

			// Pending task survived as Pending.
			if gotPending, err := r2.Get(ctxA, pending.ID); err != nil || gotPending.Status != tasks.StatusPending {
				t.Errorf("pending task after restart: %+v err=%v, want Pending", gotPending, err)
			}

			// Crash-left Running task recovered to Failed{runtime_restarted}.
			gotCrashed, err := r2.Get(ctxA, crashed.ID)
			if err != nil {
				t.Fatalf("Get(crashed) after restart: %v", err)
			}
			if gotCrashed.Status != tasks.StatusFailed {
				t.Errorf("crashed task status = %q, want Failed (recovered)", gotCrashed.Status)
			}
			if gotCrashed.Error == nil || gotCrashed.Error.Code != engine.RecoveryErrorCode {
				t.Errorf("crashed task error = %+v, want code %q", gotCrashed.Error, engine.RecoveryErrorCode)
			}

			// Identity isolation holds after restart: another tenant
			// cannot read tenant-a's task.
			otherCtx := idCtx(t, identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"})
			if _, err := r2.Get(otherCtx, done.ID); err == nil {
				t.Error("cross-tenant Get after restart: want ErrNotFound, got nil")
			}

			// List for the owning session returns all three tasks.
			list, err := r2.List(ctxA, idA.Identity, tasks.TaskFilter{})
			if err != nil {
				t.Fatalf("List after restart: %v", err)
			}
			if len(list) != 3 {
				t.Errorf("List after restart = %d tasks, want 3", len(list))
			}
		})
	}
}

// failingStateStore forces Save to error, to exercise the fail-loud
// persistence path on the seam.
type failingStateStore struct {
	state.StateStore
	err error
}

func (f failingStateStore) Save(context.Context, state.StateRecord) error { return f.err }

func (f failingStateStore) SaveIf(context.Context, []state.SlotExpectation, state.StateRecord) error {
	return f.err
}

// TestE2E_DurableTasks_WriteError_FailsLoud asserts a StateStore write
// failure surfaces from Spawn rather than being silently swallowed.
func TestE2E_DurableTasks_WriteError_FailsLoud(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	defer func() { _ = inner.Close(context.Background()) }()
	bus := mkInmemBus(t)
	defer func() { _ = bus.Close(context.Background()) }()

	r := openDurableTasks(t, failingStateStore{StateStore: inner, err: errors.New("disk full")}, bus)
	defer func() { _ = r.Close(context.Background()) }()

	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx := idCtx(t, id.Identity)
	if _, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "x"}); err == nil {
		t.Error("Spawn against a failing StateStore: want error, got nil")
	}
}
