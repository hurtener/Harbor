package durable

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

func inmemStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	return s
}

func validQuad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
}

// failingStore wraps a StateStore and forces Save to error.
type failingStore struct {
	state.StateStore
	err error
}

func (f failingStore) Save(context.Context, state.StateRecord) error { return f.err }

// TestBackend_SaveNilRecords_FailLoud asserts the nil-record guards.
func TestBackend_SaveNilRecords_FailLoud(t *testing.T) {
	b := &backend{store: inmemStore(t)}
	ctx := context.Background()
	if err := b.SaveTask(ctx, engine.TaskRecord{Task: nil}); err == nil {
		t.Error("SaveTask(nil task): want error")
	}
	if err := b.SaveGroup(ctx, nil); err == nil {
		t.Error("SaveGroup(nil): want error")
	}
	if err := b.SavePatch(ctx, nil); err == nil {
		t.Error("SavePatch(nil): want error")
	}
	if err := b.DeleteTask(ctx, nil); err == nil {
		t.Error("DeleteTask(nil): want error")
	}
}

// TestBackend_SaveStoreError_Propagates asserts a StateStore Save
// failure surfaces from each Save* (fail-loud, no silent swallow).
func TestBackend_SaveStoreError_Propagates(t *testing.T) {
	b := &backend{store: failingStore{StateStore: inmemStore(t), err: errors.New("disk full")}}
	ctx := context.Background()
	id := validQuad()
	if err := b.SaveTask(ctx, engine.TaskRecord{Task: &tasks.Task{ID: "x", Identity: id}}); err == nil {
		t.Error("SaveTask with failing store: want error")
	}
	if err := b.SaveGroup(ctx, &tasks.TaskGroup{ID: "g", SessionID: id.Identity}); err == nil {
		t.Error("SaveGroup with failing store: want error")
	}
	if err := b.SavePatch(ctx, &tasks.Patch{ID: "p", SessionID: id.Identity}); err == nil {
		t.Error("SavePatch with failing store: want error")
	}
}

// TestBackend_Hydrate_CorruptRecord_FailsLoud asserts Hydrate surfaces
// a malformed persisted record instead of booting with partial state.
func TestBackend_Hydrate_CorruptRecord_FailsLoud(t *testing.T) {
	store := inmemStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	// Seed a task slot with invalid JSON.
	if err := store.Save(context.Background(), state.StateRecord{
		ID:       state.NewEventID(),
		Identity: validQuad(),
		Kind:     taskKindPrefix + "broken",
		Bytes:    []byte("{not valid json"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b := &backend{store: store}
	if _, err := b.Hydrate(context.Background()); err == nil {
		t.Error("Hydrate over a corrupt record: want error, got nil")
	}
}

// TestBackend_Hydrate_BadContentHash_FailsLoud asserts a non-hex
// content hash fails Hydrate loudly.
func TestBackend_Hydrate_BadContentHash_FailsLoud(t *testing.T) {
	store := inmemStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	pt := persistedTask{
		Task:        &tasks.Task{ID: "x", Identity: validQuad()},
		ContentHash: "zzzz-not-hex",
	}
	payload, _ := json.Marshal(pt)
	if err := store.Save(context.Background(), state.StateRecord{
		ID:       state.NewEventID(),
		Identity: validQuad(),
		Kind:     taskKindPrefix + "x",
		Bytes:    payload,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b := &backend{store: store}
	if _, err := b.Hydrate(context.Background()); err == nil {
		t.Error("Hydrate with bad content hash: want error, got nil")
	}
}

// TestNew_EngineConstructionError_FailsLoud asserts durable.New wraps
// an engine construction failure (here, a nil bus).
func TestNew_EngineConstructionError_FailsLoud(t *testing.T) {
	_, err := New(tasks.Dependencies{
		Store:    inmemStore(t),
		Bus:      nil, // engine.New rejects a nil bus
		Redactor: auditpatterns.New(),
	})
	if err == nil {
		t.Error("New with nil bus: want error, got nil")
	}
}

// mkInmemBus is a local bus for the internal package.
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
		t.Fatalf("events inmem New: %v", err)
	}
	return bus
}

// TestNew_Succeeds_OverEmptyStore is the happy path: a real store + bus
// builds cleanly and recovers zero tasks.
func TestNew_Succeeds_OverEmptyStore(t *testing.T) {
	store := inmemStore(t)
	bus := mkInmemBus(t)
	defer func() {
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	}()
	r, err := New(tasks.Dependencies{Store: store, Bus: bus, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = r.Close(context.Background())
}

// TestBackend_SaveTask_Unserializable_FailsLoud asserts a task whose
// result holds invalid JSON surfaces tasks.ErrUnserializable rather
// than being silently dropped (CLAUDE.md §5 fail-loud). This is the
// only record kind that can fail to marshal (Result.Value is a
// json.RawMessage the encoder validates).
func TestBackend_SaveTask_Unserializable_FailsLoud(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	b := &backend{store: store}
	bad := &tasks.Task{
		ID:       "01BAD",
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}},
		Kind:     tasks.KindBackground,
		Status:   tasks.StatusComplete,
		Result:   &tasks.TaskResult{Value: json.RawMessage("{not valid json")},
	}

	err = b.SaveTask(context.Background(), engine.TaskRecord{Task: bad})
	if err == nil {
		t.Fatal("SaveTask with unserializable result: want error, got nil")
	}
	if !errors.Is(err, tasks.ErrUnserializable) {
		t.Errorf("error should wrap tasks.ErrUnserializable, got: %v", err)
	}
}
