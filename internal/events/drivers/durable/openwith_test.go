// openwith_test.go — Phase 110d (D-197): the deps-aware factory path.
// The durable driver can now SHARE the runtime's StateStore through
// `events.OpenWith` instead of requiring cmd-only direct construction.
package durable_test

import (
	"context"
	"strings"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
)

// TestOpenWith_SharedStore_SurvivesBusReopen — the 110d acceptance
// path: a durable bus opened via OpenWith with the runtime's
// StateStore shares it (caller-owned: the bus's Close leaves the
// store open), so a SECOND bus over the same store replays the first
// bus's events gap-free — the restart scenario through the FACTORY
// path, not direct construction.
func TestOpenWith_SharedStore_SurvivesBusReopen(t *testing.T) {
	store := newInmemStore(t)
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	id := quad("t1", "u1", "s-openwith")

	cfg := durableCfg()
	cfg.Driver = "durable"
	cfg.StateDriver = "" // no dedicated store — share the runtime's

	bus1, err := events.OpenWith(context.Background(), cfg, auditpatterns.New(), events.Deps{State: store})
	if err != nil {
		t.Fatalf("OpenWith (run 1): %v", err)
	}
	publishN(t, bus1, id, 5)
	if err := bus1.Close(context.Background()); err != nil {
		t.Fatalf("bus1.Close: %v", err)
	}

	// The shared store MUST survive the bus's Close (caller owns it):
	// a second factory-path bus over the same store replays history.
	bus2, err := events.OpenWith(context.Background(), cfg, auditpatterns.New(), events.Deps{State: store})
	if err != nil {
		t.Fatalf("OpenWith (run 2 — store must still be open): %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close(context.Background()) })
	rp, ok := bus2.(events.Replayer)
	if !ok {
		t.Fatalf("durable bus must implement events.Replayer")
	}
	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay after factory-path reopen: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 events replayed across the shared store, got %d", len(got))
	}
	for i, ev := range got {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("gap detected: event %d has Sequence %d", i, ev.Sequence)
		}
	}
}

// TestOpenWith_NoStoreNoDriver_FailsLoud — durable selected with
// neither a dedicated `events.state_driver` nor a shared Deps.State
// fails loud naming both ways out (the §13 posture carried into the
// deps-aware path).
func TestOpenWith_NoStoreNoDriver_FailsLoud(t *testing.T) {
	cfg := durableCfg()
	cfg.Driver = "durable"
	cfg.StateDriver = ""
	_, err := events.OpenWith(context.Background(), cfg, auditpatterns.New(), events.Deps{})
	if err == nil {
		t.Fatalf("expected fail-loud error for durable with no store at all")
	}
	if !strings.Contains(err.Error(), "events.state_driver") || !strings.Contains(err.Error(), "Deps.State") {
		t.Fatalf("error must name both ways out, got %v", err)
	}
}

// TestOpenWith_ExplicitStateDriver_WinsOverSharedStore — an operator
// who configured a dedicated event-log store keeps it: the explicit
// `events.state_driver` takes precedence over the ambient shared
// store, and the bus owns (and closes) its private store.
func TestOpenWith_ExplicitStateDriver_WinsOverSharedStore(t *testing.T) {
	shared := newInmemStore(t)
	t.Cleanup(func() { _ = shared.Close(context.Background()) })
	id := quad("t1", "u1", "s-dedicated")

	cfg := durableCfg()
	cfg.Driver = "durable"
	cfg.StateDriver = "inmem"

	bus, err := events.OpenWith(context.Background(), cfg, auditpatterns.New(), events.Deps{State: shared})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	publishN(t, bus, id, 3)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("bus.Close: %v", err)
	}

	// The events went to the bus's PRIVATE store, not the shared one:
	// a fresh bus sharing `shared` sees no history for the session.
	bus2, err := events.OpenWith(context.Background(), durableSharedCfg(), auditpatterns.New(), events.Deps{State: shared})
	if err != nil {
		t.Fatalf("OpenWith over shared store: %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close(context.Background()) })
	got, err := bus2.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected the dedicated-store events NOT to land in the shared store, got %d", len(got))
	}
}

// durableSharedCfg is durableCfg with the durable driver selected and
// no dedicated state driver (the share-the-runtime-store shape).
func durableSharedCfg() config.EventsConfig {
	cfg := durableCfg()
	cfg.Driver = "durable"
	cfg.StateDriver = ""
	return cfg
}
