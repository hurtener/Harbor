package durable_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/state"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/drivers/durable"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// pendingStores runs each stranded-Pending scenario over the in-memory
// store and, when HARBOR_PG_DSN is set, a real PostgreSQL store (fresh
// schema per subtest).
func pendingStores(t *testing.T, fn func(t *testing.T, store state.StateStore)) {
	t.Run("inmem", func(t *testing.T) {
		store := newStore(t)
		t.Cleanup(func() { _ = store.Close(context.Background()) })
		fn(t, store)
	})
	t.Run("postgres", func(t *testing.T) {
		base := os.Getenv("HARBOR_PG_DSN")
		if base == "" {
			t.Skip("HARBOR_PG_DSN not set; skipping real-Postgres pending-task recovery")
		}
		store, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: pendingFreshSchema(t, base)})
		if err != nil {
			t.Fatalf("statepostgres.New: %v", err)
		}
		t.Cleanup(func() { _ = store.Close(context.Background()) })
		fn(t, store)
	})
}

// testClock is a settable engine clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// strandPending spawns a task and simulates the latch-era failure mode:
// the task record persisted but no consumer ever started it.
func strandPending(t *testing.T, store state.StateStore, kind tasks.TaskKind) tasks.TaskID {
	t.Helper()
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	r := openOver(t, store, bus)
	defer func() { _ = r.Close(context.Background()) }()
	h, err := r.Spawn(ctxFor(t, quadA().Identity), tasks.SpawnRequest{Identity: quadA(), Kind: kind, Description: "stranded"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return h.ID
}

// reopenAged reopens the registry over store with a clock advanced by age
// and a subscription opened before the open-time recovery sweep runs.
func reopenAged(t *testing.T, store state.StateStore, age time.Duration) (tasks.TaskRegistry, events.Subscription, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Now().Add(age)}
	bus := mkBus(t)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := quadA().Identity
	sub, err := bus.Subscribe(context.Background(), events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	r, err := durable.NewWithOptions(tasks.Dependencies{
		Store: store, Bus: bus, Redactor: auditpatterns.New(), Cfg: config.TasksConfig{Driver: "durable"},
	}, engine.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, sub, clock
}

// drain collects the task event types for id observed within wait.
func drain(sub events.Subscription, id tasks.TaskID, wait time.Duration) []events.EventType {
	var got []events.EventType
	deadline := time.After(wait)
	for {
		select {
		case ev := <-sub.Events():
			if taskIDOf(ev) == id {
				got = append(got, ev.Type)
			}
		case <-deadline:
			return got
		}
	}
}

func taskIDOf(ev events.Event) tasks.TaskID {
	switch p := ev.Payload.(type) {
	case tasks.TaskSpawnedPayload:
		return p.TaskID
	case tasks.TaskFailedPayload:
		return p.TaskID
	case tasks.TaskStartedPayload:
		return p.TaskID
	}
	return ""
}

func redrive(t *testing.T, r tasks.TaskRegistry) int {
	t.Helper()
	n, err := r.(tasks.PendingRedriver).RedrivePendingTasks(context.Background())
	if err != nil {
		t.Fatalf("RedrivePendingTasks: %v", err)
	}
	return n
}

func TestDurable_PendingRecovery_StaleForegroundFails(t *testing.T) {
	pendingStores(t, func(t *testing.T, store state.StateStore) {
		id := strandPending(t, store, tasks.KindForeground)
		r, sub, _ := reopenAged(t, store, engine.PendingForegroundAbandonAfter+time.Minute)
		got, err := r.Get(ctxFor(t, quadA().Identity), id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != tasks.StatusFailed || got.Error == nil || got.Error.Code != engine.PendingAbandonedErrorCode {
			t.Fatalf("stale foreground = %q %+v, want Failed/%s", got.Status, got.Error, engine.PendingAbandonedErrorCode)
		}
		evs := drain(sub, id, 300*time.Millisecond)
		if len(evs) != 2 || evs[0] != tasks.EventTypeTaskSpawned || evs[1] != tasks.EventTypeTaskFailed {
			t.Fatalf("events = %v, want [task.spawned task.failed] and no task.started", evs)
		}
		// The re-emitted spawn cannot start a terminal task.
		if err := r.MarkRunning(ctxFor(t, quadA().Identity), id); err == nil {
			t.Fatal("MarkRunning succeeded on an abandoned task")
		}
		if n := redrive(t, r); n != 0 {
			t.Fatalf("redrive after abandon handled %d, want 0", n)
		}
	})
}

func TestDurable_PendingRecovery_StaleBackgroundReemittedAndRunsOnce(t *testing.T) {
	pendingStores(t, func(t *testing.T, store state.StateStore) {
		id := strandPending(t, store, tasks.KindBackground)
		r, sub, clock := reopenAged(t, store, engine.PendingForegroundAbandonAfter+time.Minute)
		if evs := drain(sub, id, 100*time.Millisecond); len(evs) != 0 {
			t.Fatalf("open-time sweep emitted %v for a background task, want nothing", evs)
		}
		if n := redrive(t, r); n != 1 {
			t.Fatalf("redrive handled %d, want 1", n)
		}
		if n := redrive(t, r); n != 0 {
			t.Fatalf("immediate second redrive handled %d, want 0 (rate-limited)", n)
		}
		evs := drain(sub, id, 200*time.Millisecond)
		if len(evs) != 1 || evs[0] != tasks.EventTypeTaskSpawned {
			t.Fatalf("events = %v, want exactly one task.spawned", evs)
		}
		// A run loop receiving the spawn (and a duplicate) starts it once.
		ctx := ctxFor(t, quadA().Identity)
		if err := r.MarkRunning(ctx, id); err != nil {
			t.Fatalf("MarkRunning: %v", err)
		}
		if err := r.MarkRunning(ctx, id); err == nil {
			t.Fatal("duplicate MarkRunning succeeded; task would run twice")
		}
		clock.advance(engine.PendingRedriveAfter + time.Second)
		if n := redrive(t, r); n != 0 {
			t.Fatalf("redrive of a running task handled %d, want 0", n)
		}
	})
}

func TestDurable_PendingRecovery_RecentForegroundReemittedNotFailed(t *testing.T) {
	pendingStores(t, func(t *testing.T, store state.StateStore) {
		id := strandPending(t, store, tasks.KindForeground)
		r, sub, _ := reopenAged(t, store, 5*time.Minute)
		if n := redrive(t, r); n != 1 {
			t.Fatalf("redrive handled %d, want 1", n)
		}
		got, err := r.Get(ctxFor(t, quadA().Identity), id)
		if err != nil || got.Status != tasks.StatusPending {
			t.Fatalf("recent foreground = %+v, %v; want still Pending", got, err)
		}
		if evs := drain(sub, id, 200*time.Millisecond); len(evs) != 1 || evs[0] != tasks.EventTypeTaskSpawned {
			t.Fatalf("events = %v, want one task.spawned", evs)
		}
	})
}

func TestDurable_PendingRecovery_FreshPendingUntouched(t *testing.T) {
	pendingStores(t, func(t *testing.T, store state.StateStore) {
		fg := strandPending(t, store, tasks.KindForeground)
		bg := strandPending(t, store, tasks.KindBackground)
		r, sub, _ := reopenAged(t, store, 30*time.Second)
		if n := redrive(t, r); n != 0 {
			t.Fatalf("redrive handled %d fresh tasks, want 0", n)
		}
		for _, id := range []tasks.TaskID{fg, bg} {
			got, err := r.Get(ctxFor(t, quadA().Identity), id)
			if err != nil || got.Status != tasks.StatusPending {
				t.Fatalf("fresh task %s = %+v, %v; want Pending", id, got, err)
			}
			if evs := drain(sub, id, 100*time.Millisecond); len(evs) != 0 {
				t.Fatalf("fresh task %s events = %v, want none", id, evs)
			}
		}
	})
}

func TestDurable_SpawnReusedWhilePendingRepublishes(t *testing.T) {
	pendingStores(t, func(t *testing.T, store state.StateStore) {
		bus := mkBus(t)
		t.Cleanup(func() { _ = bus.Close(context.Background()) })
		r := openOver(t, store, bus)
		t.Cleanup(func() { _ = r.Close(context.Background()) })
		ctx := ctxFor(t, quadA().Identity)
		req := tasks.SpawnRequest{Identity: quadA(), Kind: tasks.KindForeground, Description: "retry", IdempotencyKey: "idem-1"}
		first, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		id := quadA().Identity
		sub, err := bus.Subscribe(context.Background(), events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Cancel()
		again, err := r.Spawn(ctx, req)
		if err != nil || !again.Reused || again.ID != first.ID {
			t.Fatalf("retry = %+v, %v; want Reused same ID", again, err)
		}
		if evs := drain(sub, first.ID, 200*time.Millisecond); len(evs) != 1 || evs[0] != tasks.EventTypeTaskSpawned {
			t.Fatalf("Reused-while-Pending events = %v, want one task.spawned", evs)
		}
		if err := r.MarkRunning(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		drain(sub, first.ID, 50*time.Millisecond) // task.started
		if _, err := r.Spawn(ctx, req); err != nil {
			t.Fatal(err)
		}
		if evs := drain(sub, first.ID, 200*time.Millisecond); len(evs) != 0 {
			t.Fatalf("Reused-while-Running events = %v, want none", evs)
		}
	})
}

func pendingFreshSchema(t *testing.T, base string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "harbor_pending_" + hex.EncodeToString(b[:])
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		drop, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		_, _ = drop.Exec(`DROP SCHEMA "` + schema + `" CASCADE`)
	})
	u, err := url.Parse(base)
	if err != nil || !strings.HasPrefix(base, "postgres") {
		t.Fatalf("HARBOR_PG_DSN must be a postgres:// URL for this test")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
