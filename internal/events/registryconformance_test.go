package events_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"

	// Blank-import the production driver aggregator so events.RegisteredDrivers()
	// reflects the REAL shipped driver set — not merely what this test binary
	// happened to import. A new events driver that self-registers here but has
	// no conformance harness wired below fails the registry-parity gate.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
)

// registryHarnessClock anchors the method-parity fixture on a fixed instant.
var registryHarnessClock = time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

type registryFixedClock struct{}

func (registryFixedClock) Now() time.Time { return registryHarnessClock }

// eventsDriverHarness constructs a fresh, replay-capable bus for one
// registered events driver. Durable is wired with a REAL inmem StateStore at
// the seam (§17.8) — a generic OpenDriver-only sweep cannot inject it, which
// is exactly why the gate is a per-driver harness map rather than a config
// loop.
type eventsDriverHarness func(t *testing.T) events.EventBus

// registeredEventsHarnesses is the driver-name → harness map the registry
// gate compares against events.RegisteredDrivers(). Every registered driver
// MUST appear here; a driver that ships without a conformance harness cannot
// prove parity and fails the build.
func registeredEventsHarnesses() map[string]eventsDriverHarness {
	return map[string]eventsDriverHarness{
		"inmem": func(t *testing.T) events.EventBus {
			t.Helper()
			bus, err := inmem.New(registryEventsCfg(), auditpatterns.New())
			if err != nil {
				t.Fatalf("inmem.New: %v", err)
			}
			t.Cleanup(func() { _ = bus.Close(context.Background()) })
			return bus
		},
		"durable": func(t *testing.T) events.EventBus {
			t.Helper()
			store := registryInmemStore(t)
			cfg := registryEventsCfg()
			cfg.Driver = "durable"
			bus, err := durable.New(context.Background(), cfg, auditpatterns.New(), store)
			if err != nil {
				t.Fatalf("durable.New: %v", err)
			}
			t.Cleanup(func() { _ = bus.Close(context.Background()) })
			return bus
		},
	}
}

func registryEventsCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         512,
	}
}

func registryInmemStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	return s
}

// TestEventsRegistry_EveryRegisteredDriverHasConformanceHarness is the W3
// registry-parity gate: it blank-imports the production aggregator, so
// events.RegisteredDrivers() is the real shipped set, and fails the build if
// any registered driver has no conformance harness wired. HA-18 shipped
// because events.aggregate was never run against every registered driver;
// this gate makes that class impossible to reintroduce.
func TestEventsRegistry_EveryRegisteredDriverHasConformanceHarness(t *testing.T) {
	t.Parallel()
	harnesses := registeredEventsHarnesses()
	covered := 0
	for _, name := range events.RegisteredDrivers() {
		// RegisterForTest mints per-test sentinel driver names suffixed with
		// "::<test>::<counter>" (they self-clean on t.Cleanup). Those are not
		// production drivers and legitimately have no conformance harness; the
		// gate enforces coverage for the drivers the production aggregator
		// registers (whose names never contain "::").
		if strings.Contains(name, "::") {
			continue
		}
		if _, ok := harnesses[name]; !ok {
			t.Fatalf("events driver %q is registered (via internal/drivers/prod) but has no conformance harness wired in registeredEventsHarnesses() — a new driver cannot ship without proving parity", name)
		}
		covered++
	}
	if covered == 0 {
		t.Fatal("no production events driver harnesses wired — the aggregator registered none")
	}
}

// TestEventsMethodParity_AcrossRegisteredDrivers runs the four event-read
// Protocol surfaces — events.aggregate, events.list, events.subscribe,
// state.history — against EVERY registered driver over an identical fixture,
// asserting each returns the SAME answer (or the same named-sentinel
// difference), never a 500 and never a status fork. This is the executable
// form of the driver-parity thesis: a driver difference may change WHAT a
// method returns, never WHETHER it works.
func TestEventsMethodParity_AcrossRegisteredDrivers(t *testing.T) {
	t.Parallel()
	harnesses := registeredEventsHarnesses()

	// A shared identity fixture, published identically to every driver so the
	// answers are directly comparable (both drivers assign sequences 1..N in
	// publish order).
	idA := identity.Quadruple{Identity: identity.Identity{TenantID: "par-A", UserID: "par-uA", SessionID: "par-sA"}}
	idB := identity.Quadruple{Identity: identity.Identity{TenantID: "par-B", UserID: "par-uB", SessionID: "par-sB"}}
	at := registryHarnessClock.Add(-10 * time.Minute)

	type driverAnswers struct {
		aggregateWidenedTotal  int64
		aggregateTruncated     bool
		listOwnCount           int
		listOwnSeqs            []uint64
		boundsHead, boundsTail uint64
		windowSeqs             []uint64
		subscribeOK            bool
		emptyTripleSentinel    error
	}

	answers := map[string]driverAnswers{}
	aggReq := prototypes.EventAggregateRequest{Window: time.Hour, Bucket: time.Minute}

	for name, build := range harnesses {
		bus := build(t)
		// Seed: 3 events for A, 2 for B (identical across drivers).
		publishParity(t, bus, idA, at, 3)
		publishParity(t, bus, idB, at, 2)

		var da driverAnswers

		// events.aggregate — widened, empty filter (session-less fan-in).
		agg, err := events.NewAggregator(bus, events.WithAggregatorClock(registryFixedClock{}))
		if err != nil {
			t.Fatalf("%s: NewAggregator: %v", name, err)
		}
		resp, err := agg.Aggregate(context.Background(), aggReq, true)
		if err != nil {
			t.Fatalf("%s: events.aggregate must not error on a registered driver: %v", name, err)
		}
		for _, b := range resp.Buckets {
			da.aggregateWidenedTotal += b.Counts[string(events.EventTypeRuntimeError)]
		}
		da.aggregateTruncated = resp.Truncated

		hr, ok := bus.(events.HistoryReplayer)
		if !ok {
			t.Fatalf("%s: registered driver must implement events.HistoryReplayer", name)
		}

		// events.list — own-triple page (non-admin).
		page, err := hr.ListWindow(context.Background(), events.EventListQuery{
			Filter: wireTripleParity(idA), Limit: 50,
		})
		if err != nil {
			t.Fatalf("%s: events.list must not error: %v", name, err)
		}
		da.listOwnCount = len(page.Events)
		da.listOwnSeqs = seqsParity(page.Events)

		// state.history — Bounds + Window for A.
		head, tail, _, err := hr.Bounds(context.Background(), filterTripleParity(idA))
		if err != nil {
			t.Fatalf("%s: state.history Bounds must not error: %v", name, err)
		}
		da.boundsHead, da.boundsTail = head, tail
		win, err := hr.Window(context.Background(), 0, 50, filterTripleParity(idA))
		if err != nil {
			t.Fatalf("%s: state.history Window must not error: %v", name, err)
		}
		da.windowSeqs = seqsParity(win)

		// events.subscribe — a valid own-triple filter is accepted; an
		// empty-triple non-admin filter is rejected with the SAME sentinel on
		// every driver (the named-sentinel parity leg).
		sub, err := bus.Subscribe(context.Background(), filterTripleParity(idA))
		if err != nil {
			t.Fatalf("%s: events.subscribe(valid) must not error: %v", name, err)
		}
		sub.Cancel()
		da.subscribeOK = true
		_, da.emptyTripleSentinel = bus.Subscribe(context.Background(), events.Filter{})

		answers[name] = da
	}

	// Compare every driver's answers to a reference driver's — same answer OR
	// same named sentinel, for every method.
	var refName string
	for n := range answers {
		refName = n
		break
	}
	ref := answers[refName]
	for name, da := range answers {
		if da.aggregateWidenedTotal != ref.aggregateWidenedTotal {
			t.Errorf("aggregate widened total: %s=%d vs %s=%d (drivers disagree on the same request)", name, da.aggregateWidenedTotal, refName, ref.aggregateWidenedTotal)
		}
		if da.aggregateTruncated != ref.aggregateTruncated {
			t.Errorf("aggregate truncated: %s=%v vs %s=%v", name, da.aggregateTruncated, refName, ref.aggregateTruncated)
		}
		if da.listOwnCount != ref.listOwnCount || !equalSeqs(da.listOwnSeqs, ref.listOwnSeqs) {
			t.Errorf("events.list own page: %s=%v vs %s=%v", name, da.listOwnSeqs, refName, ref.listOwnSeqs)
		}
		if da.boundsHead != ref.boundsHead || da.boundsTail != ref.boundsTail {
			t.Errorf("state.history bounds: %s=(%d,%d) vs %s=(%d,%d)", name, da.boundsHead, da.boundsTail, refName, ref.boundsHead, ref.boundsTail)
		}
		if !equalSeqs(da.windowSeqs, ref.windowSeqs) {
			t.Errorf("state.history window: %s=%v vs %s=%v", name, da.windowSeqs, refName, ref.windowSeqs)
		}
		if da.subscribeOK != ref.subscribeOK {
			t.Errorf("events.subscribe acceptance: %s=%v vs %s=%v", name, da.subscribeOK, refName, ref.subscribeOK)
		}
		// Named-sentinel parity: both reject the empty-triple non-admin
		// subscribe with ErrIdentityScopeRequired.
		if !errors.Is(da.emptyTripleSentinel, events.ErrIdentityScopeRequired) {
			t.Errorf("events.subscribe(empty triple) on %s: err=%v, want ErrIdentityScopeRequired (same sentinel on every driver)", name, da.emptyTripleSentinel)
		}
	}
}

func publishParity(t *testing.T, bus events.EventBus, id identity.Quadruple, at time.Time, n int) {
	t.Helper()
	for i := range n {
		ev := events.Event{
			Type:       events.EventTypeRuntimeError,
			Identity:   id,
			OccurredAt: at.UTC(),
			Payload:    events.BusDroppedPayload{FromSeq: 1, ToSeq: 1},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("publishParity %+v #%d: %v", id.Identity, i, err)
		}
	}
}

func wireTripleParity(id identity.Quadruple) prototypes.EventFilter {
	return prototypes.EventFilter{
		TenantIDs:  []string{id.TenantID},
		UserIDs:    []string{id.UserID},
		SessionIDs: []string{id.SessionID},
	}
}

func filterTripleParity(id identity.Quadruple) events.Filter {
	return events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

func seqsParity(evs []events.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Sequence
	}
	return out
}

func equalSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
