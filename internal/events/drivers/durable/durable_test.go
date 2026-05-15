package durable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func durableCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         64,
	}
}

func newInmemStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	return s
}

// newDurableBus builds a durable bus backed by a fresh in-memory
// StateStore and returns the bus, its Replayer view, and the store
// (so tests can simulate a restart by reusing it).
func newDurableBus(t *testing.T, store state.StateStore) (events.EventBus, events.Replayer) {
	t.Helper()
	bus, err := durable.New(durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("durable bus does not implement events.Replayer")
	}
	return bus, rp
}

func quad(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	}}
}

// testPayload is a non-SafePayload external payload (it goes through
// the audit redactor on Publish).
type testPayload struct {
	events.Sealed
	Note string
}

func runtimeWarn(note string) events.EventPayload {
	return testPayload{Note: note}
}

func publishN(t *testing.T, bus events.EventBus, id identity.Quadruple, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ev := events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  runtimeWarn(fmt.Sprintf("ev-%d", i)),
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}
}

func filterFor(id identity.Quadruple) events.Filter {
	return events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

// ---------------------------------------------------------------------------
// Registry + construction
// ---------------------------------------------------------------------------

func TestDurable_RegisteredDriver_IsRegistered(t *testing.T) {
	found := false
	for _, name := range events.RegisteredDrivers() {
		if name == "durable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("durable driver not in registry: %v", events.RegisteredDrivers())
	}
}

// TestDurable_RegistryOpen_EmptyStateDriver_FailsLoud — PR #91
// amended D-074 per CLAUDE.md §13 ("Test stubs as production defaults
// on operator-facing seams"). An operator who selects
// `events.driver = "durable"` but leaves `events.state_driver` empty
// MUST get a fail-loud boot error, not a silent in-memory ring.
func TestDurable_RegistryOpen_EmptyStateDriver_FailsLoud(t *testing.T) {
	cfg := durableCfg()
	cfg.StateDriver = "" // explicit: no state driver
	_, err := events.OpenDriver("durable", cfg, auditpatterns.New())
	if err == nil {
		t.Fatalf("expected fail-loud error for durable+empty StateDriver, got nil")
	}
	if !strings.Contains(err.Error(), "state_driver is required") {
		t.Fatalf("expected error to name the missing config key, got %v", err)
	}
}

// TestDurable_RegistryOpen_WithStateDriver_OpensSuccessfully — the
// configured path: a real StateStore driver name opens cleanly and
// yields a bus that satisfies events.Replayer.
func TestDurable_RegistryOpen_WithStateDriver_OpensSuccessfully(t *testing.T) {
	cfg := durableCfg()
	cfg.StateDriver = "inmem"
	bus, err := events.OpenDriver("durable", cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("OpenDriver(durable, inmem state): %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	if _, ok := bus.(events.Replayer); !ok {
		t.Fatalf("durable bus must implement events.Replayer")
	}
}

func TestDurable_New_RejectsNilRedactor(t *testing.T) {
	if _, err := durable.New(durableCfg(), nil, newInmemStore(t)); err == nil {
		t.Fatalf("expected error for nil redactor")
	}
}

// ---------------------------------------------------------------------------
// Publish -> persist -> replay round-trip
// ---------------------------------------------------------------------------

func TestDurable_PublishPersistsAndReplays(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	id := quad("t1", "u1", "s1")

	publishN(t, bus, id, 5)

	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 replayed events, got %d", len(got))
	}
	for i, ev := range got {
		wantSeq := uint64(i + 1)
		if ev.Sequence != wantSeq {
			t.Fatalf("event %d: expected Sequence %d, got %d", i, wantSeq, ev.Sequence)
		}
		if ev.Identity.SessionID != "s1" {
			t.Fatalf("event %d: expected SessionID s1, got %q", i, ev.Identity.SessionID)
		}
		// Payload rehydrates as RedactedMap (D-074).
		if _, ok := ev.Payload.(events.RedactedMap); !ok {
			t.Fatalf("event %d: expected RedactedMap payload, got %T", i, ev.Payload)
		}
	}
}

func TestDurable_ReplayFromCursor_StrictlyNewer(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	id := quad("t1", "u1", "s1")
	publishN(t, bus, id, 10)

	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1", Sequence: 6}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 events after cursor 6, got %d", len(got))
	}
	if got[0].Sequence != 7 {
		t.Fatalf("expected first replayed Sequence 7, got %d", got[0].Sequence)
	}
}

func TestDurable_ReplayCursorAtHead_ReturnsNil(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	id := quad("t1", "u1", "s1")
	publishN(t, bus, id, 3)

	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1", Sequence: 3}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil replay at head, got %d events", len(got))
	}
}

// ---------------------------------------------------------------------------
// Acceptance: restart-replay-no-gaps
// ---------------------------------------------------------------------------

func TestDurable_ReplayAcrossRestart_NoGaps(t *testing.T) {
	// One StateStore survives across two bus instances — that IS the
	// Runtime-restart scenario.
	store := newInmemStore(t)
	id := quad("t1", "u1", "s1")

	// First Runtime: publish 8 events, then tear down the bus.
	bus1, err := durable.New(durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New (run 1): %v", err)
	}
	publishN(t, bus1, id, 8)
	if err := bus1.Close(context.Background()); err != nil {
		t.Fatalf("bus1.Close: %v", err)
	}

	// Second Runtime: a fresh bus over the SAME store. A late
	// subscriber replays from the beginning and must see all 8 with
	// no gaps.
	bus2, err := durable.New(durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New (run 2): %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close(context.Background()) })
	rp := bus2.(events.Replayer)

	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay after restart: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("expected 8 events replayed after restart, got %d", len(got))
	}
	for i, ev := range got {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("gap detected: event %d has Sequence %d", i, ev.Sequence)
		}
	}
}

// ---------------------------------------------------------------------------
// Multi-isolation
// ---------------------------------------------------------------------------

func TestDurable_Replay_CrossSessionIsolation(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)

	idA := quad("t1", "u1", "sA")
	idB := quad("t1", "u1", "sB")
	idC := quad("t2", "u9", "sC")

	publishN(t, bus, idA, 3)
	publishN(t, bus, idB, 4)
	publishN(t, bus, idC, 2)

	gotA, err := rp.Replay(context.Background(), events.Cursor{SessionID: "sA"}, filterFor(idA))
	if err != nil {
		t.Fatalf("Replay sA: %v", err)
	}
	if len(gotA) != 3 {
		t.Fatalf("session sA: expected 3 events, got %d", len(gotA))
	}
	for _, ev := range gotA {
		if ev.Identity.SessionID != "sA" || ev.Identity.TenantID != "t1" {
			t.Fatalf("cross-session leak: sA replay returned %+v", ev.Identity)
		}
	}

	gotC, err := rp.Replay(context.Background(), events.Cursor{SessionID: "sC"}, filterFor(idC))
	if err != nil {
		t.Fatalf("Replay sC: %v", err)
	}
	if len(gotC) != 2 {
		t.Fatalf("session sC: expected 2 events, got %d", len(gotC))
	}
	for _, ev := range gotC {
		if ev.Identity.TenantID != "t2" {
			t.Fatalf("cross-tenant leak: sC replay returned tenant %q", ev.Identity.TenantID)
		}
	}
}

func TestDurable_Subscribe_RejectsEmptyTripleNonAdmin(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	_, err := bus.Subscribe(context.Background(), events.Filter{Tenant: "t1"})
	if !errors.Is(err, events.ErrIdentityScopeRequired) {
		t.Fatalf("expected ErrIdentityScopeRequired, got %v", err)
	}
}

func TestDurable_Replay_RejectsEmptyTripleNonAdmin(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	_ = bus
	_, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, events.Filter{Session: "s1"})
	if !errors.Is(err, events.ErrIdentityScopeRequired) {
		t.Fatalf("expected ErrIdentityScopeRequired, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Live fan-out
// ---------------------------------------------------------------------------

func TestDurable_Subscribe_LiveFanOut(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	id := quad("t1", "u1", "s1")

	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	publishN(t, bus, id, 3)

	for i := 0; i < 3; i++ {
		select {
		case ev := <-sub.Events():
			if ev.Identity.SessionID != "s1" {
				t.Fatalf("live event %d: wrong session %q", i, ev.Identity.SessionID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for live event %d", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Loud degradation (no StateStore)
// ---------------------------------------------------------------------------

func TestDurable_NoStateStore_DegradesLoudly(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	bus, err := durable.New(durableCfg(), auditpatterns.New(), nil, durable.WithLogger(logger))
	if err != nil {
		t.Fatalf("durable.New (no store): %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	logged := buf.String()
	if !strings.Contains(logged, "best-effort") || !strings.Contains(logged, "level=WARN") {
		t.Fatalf("expected a loud WARN about best-effort degradation, got: %q", logged)
	}

	// Best-effort mode still publishes + replays from the ring.
	id := quad("t1", "u1", "s1")
	rp := bus.(events.Replayer)
	publishN(t, bus, id, 4)
	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, filterFor(id))
	if err != nil {
		t.Fatalf("best-effort Replay: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("best-effort replay: expected 4, got %d", len(got))
	}
}

func TestDurable_NoStateStore_RingZero_ReplayUnavailable(t *testing.T) {
	cfg := durableCfg()
	cfg.ReplayBufferSize = 0
	bus, err := durable.New(cfg, auditpatterns.New(), nil,
		durable.WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rp := bus.(events.Replayer)
	_, err = rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, filterFor(quad("t1", "u1", "s1")))
	if !errors.Is(err, events.ErrReplayUnavailable) {
		t.Fatalf("expected ErrReplayUnavailable, got %v", err)
	}
}

func TestDurable_BestEffort_CursorTooOld(t *testing.T) {
	cfg := durableCfg()
	cfg.ReplayBufferSize = 4 // small ring so older events are evicted
	bus, err := durable.New(cfg, auditpatterns.New(), nil,
		durable.WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := quad("t1", "u1", "s1")
	publishN(t, bus, id, 10) // ring retains seq 7..10
	rp := bus.(events.Replayer)
	_, err = rp.Replay(context.Background(), events.Cursor{SessionID: "s1", Sequence: 2}, filterFor(id))
	if !errors.Is(err, events.ErrCursorTooOld) {
		t.Fatalf("expected ErrCursorTooOld, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fail-loudly: persistence failure surfaces from Publish
// ---------------------------------------------------------------------------

// failingStore is a state.StateStore whose Save always fails. Used to
// prove the durable driver surfaces a persistence failure loudly
// rather than silently dropping the event.
type failingStore struct{ saveErr error }

func (f *failingStore) Save(context.Context, state.StateRecord) error { return f.saveErr }
func (f *failingStore) Load(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
	return state.StateRecord{}, state.ErrNotFound
}
func (f *failingStore) LoadByEventID(context.Context, state.EventID) (state.StateRecord, error) {
	return state.StateRecord{}, state.ErrNotFound
}
func (f *failingStore) Delete(context.Context, identity.Quadruple, string) error { return nil }
func (f *failingStore) Close(context.Context) error                              { return nil }

func TestDurable_PersistFailure_SurfacesLoudly(t *testing.T) {
	sentinel := errors.New("disk on fire")
	bus, err := durable.New(durableCfg(), auditpatterns.New(), &failingStore{saveErr: sentinel})
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := quad("t1", "u1", "s1")
	err = bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  runtimeWarn("doomed"),
	})
	if err == nil {
		t.Fatalf("expected Publish to surface the persistence failure, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Closed-bus behaviour
// ---------------------------------------------------------------------------

func TestDurable_ClosedBus_RejectsOps(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	id := quad("t1", "u1", "s1")
	if err := bus.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("x"),
	}); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Publish after Close: expected ErrBusClosed, got %v", err)
	}
	if _, err := bus.Subscribe(context.Background(), filterFor(id)); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Subscribe after Close: expected ErrBusClosed, got %v", err)
	}
	if _, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, filterFor(id)); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Replay after Close: expected ErrBusClosed, got %v", err)
	}
}

func TestDurable_Close_Idempotent(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close #2 (idempotent): %v", err)
	}
}

// ensure the audit.Redactor interface import is used (compile guard).
var _ audit.Redactor = auditpatterns.New()
