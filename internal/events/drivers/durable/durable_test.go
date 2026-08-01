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
	"github.com/hurtener/Harbor/internal/llm"
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
	bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
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
	for i := range n {
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
	if _, err := durable.New(context.Background(), durableCfg(), nil, newInmemStore(t)); err == nil {
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

// ---------------------------------------------------------------------------
// Acceptance: restart-replay-no-gaps
// ---------------------------------------------------------------------------

func TestDurable_ReplayAcrossRestart_NoGaps(t *testing.T) {
	// One StateStore survives across two bus instances — that IS the
	// Runtime-restart scenario.
	store := newInmemStore(t)
	id := quad("t1", "u1", "s1")

	// First Runtime: publish 8 events, then tear down the bus.
	bus1, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
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
	bus2, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
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

	for i := range 3 {
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

	bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), nil, durable.WithLogger(logger))
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
func (f *failingStore) DeleteScope(context.Context, identity.Identity) (int, error) {
	return 0, nil
}
func (f *failingStore) ListKind(context.Context, state.ListScope, string) ([]state.StateRecord, error) {
	return nil, nil
}
func (f *failingStore) ListKindForIdentity(context.Context, identity.Quadruple, string) ([]state.StateRecord, error) {
	return nil, nil
}
func (f *failingStore) Close(context.Context) error { return nil }

func TestDurable_PersistFailure_SurfacesLoudly(t *testing.T) {
	sentinel := errors.New("disk on fire")
	bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), &failingStore{saveErr: sentinel})
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

// ensure the audit.Redactor interface import is used (compile guard).
var _ audit.Redactor = auditpatterns.New()

// ---------------------------------------------------------------------------
// D-207 — emit-constructor base-ctx threading on the DURABLE driver.
//
// The durable driver persists every event via store.Save under the
// PUBLISH ctx, so the per-run emit closures' base context is
// load-bearing here: pre-D-207 the promoted constructors published
// under context.Background(), which silently outlived the run-loop
// driver's lifetime (D-195's recorded correction). These tests pin
// the restored pre-110b semantics: a live driver-lifetime ctx
// persists; a cancelled one stops persistence and Warns loudly.
// ---------------------------------------------------------------------------

// countDurableEntries counts persisted per-event entry records via the
// StateStore's D-207 maintenance scan.
func countDurableEntries(t *testing.T, store state.StateStore) int {
	t.Helper()
	recs, err := store.ListKind(context.Background(), state.ListScope{MaintenanceScoped: true}, "events.durable.entry/")
	if err != nil {
		t.Fatalf("ListKind: %v", err)
	}
	return len(recs)
}

func TestDurable_IdentityStampingEmitterContext_CancellationBoundsPersistence(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	id := quad("t-emitctx", "u1", "s1")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	driverCtx, cancel := context.WithCancel(context.Background())
	emit := events.IdentityStampingEmitterContext(driverCtx, bus, id, logger)

	// Live driver ctx: the emit persists.
	emit(events.Event{Type: events.EventTypeRuntimeWarning, Payload: runtimeWarn("alive")})
	if n := countDurableEntries(t, store); n != 1 {
		t.Fatalf("persisted entries after live emit = %d, want 1", n)
	}

	// Cancelled driver ctx (the run-loop driver closed): persistence
	// stops — bounded by the caller ctx, the pre-110b subCtx
	// semantics — and the failure is a loud Warn, never silent.
	cancel()
	emit(events.Event{Type: events.EventTypeRuntimeWarning, Payload: runtimeWarn("late")})
	if n := countDurableEntries(t, store); n != 1 {
		t.Fatalf("persisted entries after cancelled emit = %d, want 1 (no write past driver teardown)", n)
	}
	if !strings.Contains(buf.String(), "emitter publish failed") {
		t.Fatalf("cancelled emit did not Warn loudly; log: %q", buf.String())
	}
}

func TestDurable_NewChunkPublisherContext_CancellationBoundsPersistence(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	id := quad("t-chunkctx", "u1", "s1")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	driverCtx, cancel := context.WithCancel(context.Background())
	pub := llm.NewChunkPublisherContext(driverCtx, bus, id, "task-ctx-1", logger)

	pub("hello", false, "answer")
	if n := countDurableEntries(t, store); n != 1 {
		t.Fatalf("persisted entries after live chunk = %d, want 1", n)
	}

	cancel()
	pub("late", true, "answer")
	if n := countDurableEntries(t, store); n != 1 {
		t.Fatalf("persisted entries after cancelled chunk = %d, want 1 (no write past driver teardown)", n)
	}
	if !strings.Contains(buf.String(), "completion-chunk publish failed") {
		t.Fatalf("cancelled chunk publish did not Warn loudly; log: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// HistoryReplayer — Bounds + Window (the state.history substrate)
// ---------------------------------------------------------------------------

func historyReplayer(t *testing.T, bus events.EventBus) events.HistoryReplayer {
	t.Helper()
	hr, ok := bus.(events.HistoryReplayer)
	if !ok {
		t.Fatalf("durable bus does not implement events.HistoryReplayer")
	}
	return hr
}

func seqs(evs []events.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Sequence
	}
	return out
}

func TestDurable_HistoryReplayer_BestEffortRing(t *testing.T) {
	// store=nil ⇒ best-effort in-memory ring (loud-degraded). The
	// HistoryReplayer methods read the ring.
	cfg := durableCfg()
	cfg.ReplayBufferSize = 32
	bus, err := durable.New(context.Background(), cfg, auditpatterns.New(), nil,
		durable.WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err != nil {
		t.Fatalf("durable.New (best-effort): %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	hr := historyReplayer(t, bus)
	id := quad("t1", "u1", "s1")
	publishN(t, bus, id, 6)

	head, tail, _, err := hr.Bounds(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("best-effort Bounds: %v", err)
	}
	if head != 1 || tail != 6 {
		t.Fatalf("best-effort Bounds = (%d,%d), want (1,6)", head, tail)
	}
	win, err := hr.Window(context.Background(), 0, 2, filterFor(id))
	if err != nil {
		t.Fatalf("best-effort Window: %v", err)
	}
	if len(win) != 2 || win[0].Sequence != 5 || win[1].Sequence != 6 {
		t.Fatalf("best-effort Window = %v, want seqs 5,6", seqs(win))
	}
	// A session with no matching events ⇒ ErrNoHistory from Bounds.
	if _, _, _, err := hr.Bounds(context.Background(), filterFor(quad("t1", "u1", "other"))); !errors.Is(err, events.ErrNoHistory) {
		t.Fatalf("best-effort Bounds(no-match) = %v, want ErrNoHistory", err)
	}

	// By-id scope under Admin: a second session in the SAME ring (different
	// tenant) must NOT bleed into an admin read of the first. Before the fix
	// the best-effort ring scan used Matches, whose Admin mode returned the
	// whole ring across tenants.
	publishN(t, bus, quad("t2", "u2", "sB"), 4)
	adminS1 := events.Filter{Admin: true, Tenant: "t1", User: "u1", Session: "s1"}
	win2, err := hr.Window(context.Background(), 0, 100, adminS1)
	if err != nil {
		t.Fatalf("best-effort admin Window: %v", err)
	}
	if len(win2) != 6 {
		t.Fatalf("admin Window for s1 returned %d events, want 6 (no cross-session bleed): %v", len(win2), seqs(win2))
	}
	for _, ev := range win2 {
		if ev.Identity.SessionID != "s1" || ev.Identity.TenantID != "t1" {
			t.Fatalf("best-effort admin bleed: %+v", ev.Identity)
		}
	}
}

func TestDurable_HistoryReplayer_ErrorBranches(t *testing.T) {
	bus, _ := newDurableBus(t, newInmemStore(t))
	hr := historyReplayer(t, bus)

	// Window with a non-positive limit ⇒ (nil, nil).
	win, err := hr.Window(context.Background(), 0, 0, filterFor(quad("t1", "u1", "s1")))
	if err != nil || win != nil {
		t.Fatalf("Window(limit=0) = (%v,%v), want (nil,nil)", win, err)
	}
	// Window with an empty-triple non-admin filter ⇒ ErrIdentityScopeRequired.
	if _, err := hr.Window(context.Background(), 0, 5, events.Filter{Tenant: "t1"}); !errors.Is(err, events.ErrIdentityScopeRequired) {
		t.Fatalf("Window(partial triple) = %v, want ErrIdentityScopeRequired", err)
	}
	// An admin filter that names only a session (no tenant/user) cannot
	// resolve the storage key ⇒ ErrIdentityScopeRequired.
	if _, _, _, err := hr.Bounds(context.Background(), events.Filter{Admin: true, Session: "s1"}); !errors.Is(err, events.ErrIdentityScopeRequired) {
		t.Fatalf("Bounds(admin, no triple) = %v, want ErrIdentityScopeRequired", err)
	}

	// After Close, the methods report the bus is closed.
	_ = bus.Close(context.Background())
	if _, _, _, err := hr.Bounds(context.Background(), filterFor(quad("t1", "u1", "s1"))); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Bounds(closed) = %v, want ErrBusClosed", err)
	}
	if _, err := hr.Window(context.Background(), 0, 5, filterFor(quad("t1", "u1", "s1"))); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Window(closed) = %v, want ErrBusClosed", err)
	}
}
