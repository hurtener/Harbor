// Phase 77 — Goroutine leak conformance harness (RFC §3.5, §6.1).
//
// This file is the master goroutine-leak conformance harness. It
// generalises the per-package leak tests that Phases 10 / 12 / 13 / 50
// / 52 each shipped individually (`TestEngine_*_NoLeak`,
// `TestRegistry_Sweeper_StartsAndStops_NoLeak`, the inmem-bus
// `NoLeak` cases, …) into a single table-driven conformance suite that
// runs in CI on every PR under `-race`.
//
// # The invariant
//
// Every long-lived Runtime component — anything built once that starts
// goroutines and exposes a `Stop` / `Close` / `CloseRegistry` — MUST
// join all of its goroutines on teardown (RFC §5 Go conventions:
// "goroutines started by long-lived components must be cancellable by
// a ctx and joined on shutdown"; RFC §3.5 guarantee #4: "no goroutine
// leaks — each invocation's goroutines are joined before the
// invocation returns"). This harness records a baseline
// `runtime.NumGoroutine()`, then for each component runs N
// construct → start → exercise → teardown cycles and asserts the
// goroutine count returns to baseline.
//
// # Why a table
//
// Adding a future long-lived component is one new row in
// `leakCases` — not a new test function. The row supplies a name and
// an `exercise` closure that constructs the real component with real
// drivers, drives it through a representative workload, and tears it
// down. The harness owns the baseline capture, the bounded poll, and
// the assertion.
//
// # Why N cycles (not one)
//
// A single construct→teardown cycle hides a slow leak (one stray
// goroutine per cycle). Each row runs `leakCycles` (≥10) iterations so
// a per-cycle leak accumulates well above the parked-goroutine
// tolerance and fails the suite loudly.
//
// # Why a bounded poll (not an instant snapshot)
//
// Go does not retire parked goroutines instantly; an instant
// `runtime.NumGoroutine()` check immediately after teardown is flaky
// (CLAUDE.md §17.4). The harness reuses the established bounded-poll
// pattern: a deadline plus a 10ms interval plus `runtime.Gosched`,
// with a small absolute tolerance absorbing the test runner's own
// background goroutines. The harness deliberately does NOT call
// `t.Parallel` — `NumGoroutine` is process-global and a parallel
// sibling test would pollute the count
// (matching `TestRegistry_Sweeper_StartsAndStops_NoLeak`).
//
// # Per CLAUDE.md §17.3
//
//  1. Real drivers everywhere on the seam — the inmem + durable event
//     bus, the inmem state store, the patterns audit redactor, the
//     inprocess task driver. No mocks at the boundary.
//  2. Identity propagation — every workload carries a real
//     `(tenant, user, session)` triple (+ run id) through the
//     component under test.
//  3. ≥1 failure mode — the teardown-without-leak assertion IS the
//     failure mode: a component that abandons a goroutine on
//     `Stop`/`Close` (the predecessor's deadlock-on-shutdown bug,
//     brief 01) fails the suite. The Engine row additionally cancels a
//     run mid-flight so the cancellation-cleanup path is exercised at
//     teardown.
//  4. -race is the CI gate — the dedicated `leak-harness` job in
//     `.github/workflows/ci.yml` runs this file under `-race`.
package integration_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	// Driver self-registration (blank import — §4.4 seam pattern). The
	// harness opens every component through its registry factory; the
	// concrete driver packages register themselves from init().
	_ "github.com/hurtener/Harbor/internal/events/drivers/durable"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// leakCycles is the number of construct → exercise → teardown
// iterations each component row runs. ≥10 amplifies a per-cycle leak
// (one stray goroutine per cycle ⇒ delta ≥ 10) well above the
// parked-goroutine tolerance.
const leakCycles = 12

// leakTolerance is the absolute slack added to the baseline. Go's
// runtime does not retire parked goroutines instantly even after a
// component's `Stop`/`Close` has joined its own; a handful of test
// runner / GC-assist goroutines may also be transiently parked. A
// genuine leak under `leakCycles` iterations produces a delta of at
// least `leakCycles`, so a tolerance of 4 cannot mask one.
const leakTolerance = 4

// leakIdentity is the identity triple every component workload runs
// under. Identity is mandatory (CLAUDE.md §6); the harness drives real
// identity through each component, never an empty triple.
func leakIdentity() identity.Identity {
	return identity.Identity{TenantID: "phase77-tenant", UserID: "phase77-user", SessionID: "phase77-session"}
}

// leakEventsConfig is the EventsConfig shared by the inmem and durable
// bus rows. The durable row additionally sets StateDriver so the
// registry-path factory opens an inmem-backed StateStore (the durable
// driver fails loud at boot when StateDriver is empty — D-074).
func leakEventsConfig(driver string) config.EventsConfig {
	cfg := config.EventsConfig{
		Driver:                   driver,
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}
	if driver == "durable" {
		cfg.StateDriver = "inmem"
	}
	return cfg
}

// leakCase is one row of the conformance table: a named long-lived
// component plus a closure that constructs it with real drivers,
// exercises it, and tears it down. The closure MUST fully tear the
// component down before it returns — the harness measures the
// goroutine count after the closure returns.
type leakCase struct {
	name     string
	exercise func(t *testing.T)
}

// leakCases is the conformance table. Every long-lived Runtime
// component that starts goroutines and exposes a teardown method gets
// a row. A future component is added here — one row, no new test
// function.
var leakCases = []leakCase{
	{
		// Engine — Phase 10/12/13. One goroutine per node + the
		// always-on egress dispatcher + the Phase 13 cancellation TTL
		// sweeper, all joined via the engine's WaitGroup on Stop. The
		// row drives a linear A→B→C graph, emits + fetches envelopes,
		// AND cancels a run mid-flight so the Phase 13 cancellation
		// cleanup path is in-flight at teardown.
		name: "runtime/engine.Engine",
		exercise: func(t *testing.T) {
			t.Helper()
			tag := func(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
				return in, nil
			}
			a := engine.Node{Name: "A", Func: tag}
			b := engine.Node{Name: "B", Func: tag}
			c := engine.Node{Name: "C", Func: tag}
			e, err := engine.New([]engine.Adjacency{
				{From: a, To: []engine.Node{b}},
				{From: b, To: []engine.Node{c}},
				{From: c, To: nil},
			})
			if err != nil {
				t.Fatalf("engine.New: %v", err)
			}
			if err := e.Run(context.Background()); err != nil {
				t.Fatalf("engine.Run: %v", err)
			}

			id := leakIdentity()
			for i, runID := range []string{"R-keep", "R-cancel"} {
				env := messages.Envelope{
					Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID},
					SessionID: id.SessionID,
					RunID:     runID,
					Payload:   "phase77",
				}
				if err := e.Emit(context.Background(), env); err != nil {
					t.Fatalf("engine.Emit run %d: %v", i, err)
				}
			}
			// Cancel one run mid-flight so the cancellation-cleanup
			// goroutines are exercised at teardown.
			if _, err := e.Cancel(context.Background(), "R-cancel"); err != nil {
				t.Fatalf("engine.Cancel: %v", err)
			}
			fetchCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = e.FetchByRun(fetchCtx, "R-keep")
			cancel()

			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			if err := e.Stop(stopCtx); err != nil {
				t.Fatalf("engine.Stop: %v", err)
			}
		},
	},
	{
		// EventBus (inmem) — Phase 04/05. The inmem bus runs
		// per-subscription delivery goroutines reaped on Close. The
		// row subscribes, publishes a round-trip event, and closes.
		name: "events/drivers/inmem.EventBus",
		exercise: func(t *testing.T) {
			t.Helper()
			exerciseBus(t, "inmem")
		},
	},
	{
		// EventBus (durable) — Phase 57. The durable bus wraps an
		// owned StateStore (opened via the registry factory) plus the
		// ring-buffer fan-out goroutines. Close must join them AND
		// close the owned store.
		name: "events/drivers/durable.EventBus",
		exercise: func(t *testing.T) {
			t.Helper()
			exerciseBus(t, "durable")
		},
	},
	{
		// sessions.Registry — Phase 08. Runs a background GC sweeper
		// goroutine; CloseRegistry closes the done channel and joins
		// the sweeper via the registry WaitGroup. brief 05's classic
		// long-lived-sweeper leak source.
		name: "sessions.Registry",
		exercise: func(t *testing.T) {
			t.Helper()
			red := auditpatterns.New()
			bus, err := events.OpenDriver("inmem", leakEventsConfig("inmem"), red)
			if err != nil {
				t.Fatalf("events.OpenDriver(inmem): %v", err)
			}
			store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
			if err != nil {
				_ = bus.Close(context.Background())
				t.Fatalf("state.Open: %v", err)
			}
			// A short SweepInterval guarantees the sweeper ticks at
			// least once during the workload, so its per-tick GC
			// goroutines are in-flight when CloseRegistry runs.
			cfg := config.SessionsConfig{
				IdleTTL:       24 * time.Hour,
				HardCap:       720 * time.Hour,
				SweepInterval: 5 * time.Millisecond,
			}
			reg, err := sessions.New(store, cfg, bus)
			if err != nil {
				_ = bus.Close(context.Background())
				_ = store.Close(context.Background())
				t.Fatalf("sessions.New: %v", err)
			}

			id := leakIdentity()
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				t.Fatalf("identity.With: %v", err)
			}
			if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
				t.Fatalf("sessions Open: %v", err)
			}
			// Let the sweeper tick at least once.
			time.Sleep(15 * time.Millisecond)
			_ = reg.Close(ctx, id.SessionID, "phase77-teardown")

			if err := reg.CloseRegistry(context.Background()); err != nil {
				t.Fatalf("sessions.CloseRegistry: %v", err)
			}
			if err := bus.Close(context.Background()); err != nil {
				t.Fatalf("bus.Close: %v", err)
			}
			if err := store.Close(context.Background()); err != nil {
				t.Fatalf("store.Close: %v", err)
			}
		},
	},
	{
		// TaskRegistry (inprocess) — Phase 20/21. The unified
		// foreground/background task registry; Close tears down the
		// driver. The row spawns a background task before teardown so
		// any continuation goroutines are live at Close.
		name: "tasks/drivers/inprocess.TaskRegistry",
		exercise: func(t *testing.T) {
			t.Helper()
			red := auditpatterns.New()
			bus, err := events.OpenDriver("inmem", leakEventsConfig("inmem"), red)
			if err != nil {
				t.Fatalf("events.OpenDriver(inmem): %v", err)
			}
			store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
			if err != nil {
				_ = bus.Close(context.Background())
				t.Fatalf("state.Open: %v", err)
			}
			reg, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
				Store:    store,
				Bus:      bus,
				Redactor: red,
				Cfg:      config.TasksConfig{Driver: "inprocess"},
			})
			if err != nil {
				_ = bus.Close(context.Background())
				_ = store.Close(context.Background())
				t.Fatalf("tasks.OpenDriver(inprocess): %v", err)
			}

			id := leakIdentity()
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				t.Fatalf("identity.With: %v", err)
			}
			runCtx, err := identity.WithRun(ctx, id, "phase77-run")
			if err != nil {
				t.Fatalf("identity.WithRun: %v", err)
			}
			if _, err := reg.Spawn(runCtx, tasks.SpawnRequest{
				Identity:    identity.Quadruple{Identity: id, RunID: "phase77-run"},
				Kind:        tasks.KindBackground,
				Description: "phase77 leak-harness task",
				Query:       "noop",
			}); err != nil {
				t.Fatalf("tasks.Spawn: %v", err)
			}

			if err := reg.Close(context.Background()); err != nil {
				t.Fatalf("tasks.Close: %v", err)
			}
			if err := bus.Close(context.Background()); err != nil {
				t.Fatalf("bus.Close: %v", err)
			}
			if err := store.Close(context.Background()); err != nil {
				t.Fatalf("store.Close: %v", err)
			}
		},
	},
}

// exerciseBus constructs an EventBus of the named driver via the
// registry factory, subscribes, publishes a round-trip event, and
// closes — shared by the inmem and durable bus rows.
func exerciseBus(t *testing.T, driver string) {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.OpenDriver(driver, leakEventsConfig(driver), red)
	if err != nil {
		t.Fatalf("events.OpenDriver(%s): %v", driver, err)
	}

	id := leakIdentity()
	subCtx, err := identity.With(context.Background(), id)
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("identity.With: %v", err)
	}
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("bus.Subscribe(%s): %v", driver, err)
	}

	evt := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: identity.Quadruple{Identity: id},
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: 1},
	}
	if err := bus.Publish(subCtx, evt); err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("bus.Publish(%s): %v", driver, err)
	}
	// Drain one delivery so the per-subscription delivery goroutine is
	// actively in-flight when Close runs.
	recvCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	select {
	case <-sub.Events():
	case <-recvCtx.Done():
	}
	cancel()

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("bus.Close(%s): %v", driver, err)
	}
}

// TestE2E_Phase77_GoroutineLeakConformance is the Phase 77 conformance
// suite. For every long-lived Runtime component in `leakCases` it runs
// `leakCycles` construct → exercise → teardown iterations against real
// drivers, then asserts `runtime.NumGoroutine()` has returned to its
// pre-workload baseline (within `leakTolerance`) via a bounded poll.
//
// NOT t.Parallel — NumGoroutine is process-global; a parallel sibling
// would pollute the count.
func TestE2E_Phase77_GoroutineLeakConformance(t *testing.T) {
	for _, tc := range leakCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// One warm-up cycle so first-use lazy initialisation
			// (driver registries, sync.Once-guarded globals) is not
			// counted as a leak.
			tc.exercise(t)
			settle()

			baseline := runtime.NumGoroutine()
			for i := 0; i < leakCycles; i++ {
				tc.exercise(t)
			}

			// Bounded eventually-poll: wait for parked goroutines to
			// retire, up to a deadline, never an instant snapshot.
			deadline := time.Now().Add(5 * time.Second)
			for runtime.NumGoroutine() > baseline+leakTolerance && time.Now().Before(deadline) {
				runtime.Gosched()
				time.Sleep(10 * time.Millisecond)
			}
			if delta := runtime.NumGoroutine() - baseline; delta > leakTolerance {
				t.Errorf("goroutine leak in %s: baseline=%d after=%d cycles=%d delta=%d (tolerance=%d)",
					tc.name, baseline, runtime.NumGoroutine(), leakCycles, delta, leakTolerance)
			}
		})
	}
}

// settle gives the runtime a brief, bounded window to retire parked
// goroutines before a baseline is captured. It is NOT a synchronisation
// primitive (CLAUDE.md §17.4) — it only ensures the warm-up cycle's
// goroutines have a chance to retire so the baseline is stable.
func settle() {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}
