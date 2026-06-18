// Wave-1 (Phases 86 + 87) wave-end integration test (§17.5): the three
// StateStore-backed durable drivers — the durable event log (57), the
// durable TaskService (87), and the durable distributed bus (86) — all
// persist through ONE shared StateStore. This proves their Kind
// prefixes (events.durable.* / task.durable.* / distributed.bus.entry/)
// are disjoint at RUNTIME: each driver, reopened over the shared store
// containing all three subsystems' records, hydrates / reads ONLY its
// own — no cross-contamination. Real drivers everywhere; under -race.
package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"

	_ "github.com/hurtener/Harbor/internal/distributed/drivers/durable"
	_ "github.com/hurtener/Harbor/internal/events/drivers/durable"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/durable"
)

func TestE2E_Wave1_DurableBackendsCoexistInOneStore(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "wave-durable.db")
	openStore := func() state.StateStore {
		s, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatalf("statesqlite.New: %v", err)
		}
		return s
	}
	red := auditpatterns.New()
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, err := identity.With(context.Background(), id.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	store := openStore()

	// 1. Durable event log over the shared store — publish 2 events.
	evlog, err := events.OpenWith(context.Background(), config.EventsConfig{
		Driver: "durable", MaxSubscribersPerSession: 16, SubscriberBufferSize: 256,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 1024,
	}, red, events.Deps{State: store})
	if err != nil {
		t.Fatalf("events durable OpenWith: %v", err)
	}
	for i := range 2 {
		if err := evlog.Publish(ctx, events.Event{Type: events.EventTypeRuntimeWarning, Identity: id, Payload: events.RedactedMap{Data: map[string]any{"i": i}}}); err != nil {
			t.Fatalf("evlog publish: %v", err)
		}
	}

	// 2. Durable TaskService over the same store — spawn 1 task.
	taskBus := mkBusInmem(t)
	treg, err := tasks.OpenDriver("durable", tasks.Dependencies{Store: store, Bus: taskBus, Redactor: red, Cfg: config.TasksConfig{Driver: "durable", RetainTurnTimeout: 5 * time.Minute, ContinuationHopLimit: 8}})
	if err != nil {
		t.Fatalf("tasks durable OpenDriver: %v", err)
	}
	taskH, err := treg.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "wave"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// 3. Durable distributed bus over the same store — publish 1 envelope.
	busEB := mkBusInmem(t)
	dbus, err := distributed.OpenBusDriver("durable", distributed.Dependencies{EventBus: busEB, State: store, Cfg: config.DistributedConfig{BusDriver: "durable", BusPollInterval: 15 * time.Millisecond}})
	if err != nil {
		t.Fatalf("distributed durable OpenBusDriver: %v", err)
	}
	if err := dbus.Publish(ctx, distributed.BusEnvelope{Edge: "wave", Source: "test", Identity: id, TaskID: "t1", EventID: "evt-w", Payload: []byte(`{}`), Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("bus publish: %v", err)
	}

	// Tear the live drivers down (keep the shared store) — a restart.
	_ = evlog.Close(context.Background())
	_ = treg.Close(context.Background())
	_ = taskBus.Close(context.Background())
	_ = dbus.Close(context.Background())
	_ = busEB.Close(context.Background())
	_ = store.Close(context.Background())

	// Reopen the SHARED store (now holding all three subsystems' records).
	store2 := openStore()
	defer func() { _ = store2.Close(context.Background()) }()

	// The durable TaskService reopened over the shared store hydrates
	// EXACTLY its own 1 task — not confused by event-log / bus records.
	taskBus2 := mkBusInmem(t)
	defer func() { _ = taskBus2.Close(context.Background()) }()
	treg2, err := tasks.OpenDriver("durable", tasks.Dependencies{Store: store2, Bus: taskBus2, Redactor: red, Cfg: config.TasksConfig{Driver: "durable", RetainTurnTimeout: 5 * time.Minute, ContinuationHopLimit: 8}})
	if err != nil {
		t.Fatalf("tasks durable reopen: %v", err)
	}
	defer func() { _ = treg2.Close(context.Background()) }()
	list, err := treg2.List(ctx, id.Identity, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List after restart: %v", err)
	}
	if len(list) != 1 || list[0].ID != taskH.ID {
		t.Errorf("durable tasks hydrated %d records over a shared store, want exactly its own 1 (%s)", len(list), taskH.ID)
	}

	// The durable bus reopened over the shared store projects EXACTLY its
	// own 1 envelope — not the task / event-log records.
	busEB2 := mkBusInmem(t)
	defer func() { _ = busEB2.Close(context.Background()) }()
	sub, err := busEB2.Subscribe(context.Background(), events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Types: []events.EventType{distributed.EventTypeDistributedBusEnvelope}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()
	dbus2, err := distributed.OpenBusDriver("durable", distributed.Dependencies{EventBus: busEB2, State: store2, Cfg: config.DistributedConfig{BusDriver: "durable", BusPollInterval: 15 * time.Millisecond}})
	if err != nil {
		t.Fatalf("distributed durable reopen: %v", err)
	}
	defer func() { _ = dbus2.Close(context.Background()) }()

	busCount := 0
	deadline := time.After(2 * time.Second)
	drain := true
	for drain {
		select {
		case ev := <-sub.Events():
			if p, ok := ev.Payload.(distributed.BusEnvelopePayload); ok && p.Envelope.Edge == "wave" {
				busCount++
			} else {
				t.Errorf("durable bus projected a non-bus / foreign record: %T", ev.Payload)
			}
		case <-deadline:
			drain = false
		}
		if busCount > 1 {
			t.Fatalf("durable bus projected %d envelopes over a shared store, want exactly its own 1", busCount)
		}
	}
	if busCount != 1 {
		t.Errorf("durable bus projected %d envelopes after restart, want exactly its own 1", busCount)
	}

	// The durable event log reopened over the shared store replays EXACTLY
	// its own 2 events.
	evlog2, err := events.OpenWith(context.Background(), config.EventsConfig{
		Driver: "durable", MaxSubscribersPerSession: 16, SubscriberBufferSize: 256,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 1024,
	}, red, events.Deps{State: store2})
	if err != nil {
		t.Fatalf("events durable reopen: %v", err)
	}
	defer func() { _ = evlog2.Close(context.Background()) }()
	replayer, ok := evlog2.(events.Replayer)
	if !ok {
		t.Fatal("durable event log is not a Replayer")
	}
	replayed, err := replayer.Replay(ctx, events.Cursor{SessionID: id.SessionID}, events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replayed) != 2 {
		t.Errorf("durable event log replayed %d events over a shared store, want exactly its own 2", len(replayed))
	}
}

func mkBusInmem(t *testing.T) events.EventBus {
	t.Helper()
	eb, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 256, IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 1024}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsinmem.New: %v", err)
	}
	return eb
}
