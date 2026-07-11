package integration

// Windowed-reads honesty pair (D-295 flows.runs.list time window +
// D-296 observed retention horizons). Real drivers across the seam: the
// flow RegistryCatalog + Surface for the bounded run-history read, and
// the real inmem + durable event buses for the observed events retention
// horizon. Identity propagation + a cross-tenant refusal failure mode.
// -race is the gate (go test -race ./test/integration/...).

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsdurable "github.com/hurtener/Harbor/internal/events/drivers/durable"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func phase163Passthrough(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return in, nil
}

var phase163Now = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

func phase163Flow() flow.Definition {
	return flow.Definition{
		Name:  "flow-window",
		Entry: "a",
		Exit:  "a",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Name: "a", Func: phase163Passthrough},
		},
	}
}

// newPhase163FlowSurface builds a real flow Surface seeded with runs at
// day -4..0 under tenant-A (plus one run under tenant-B).
func newPhase163FlowSurface(t *testing.T) *flowprotocol.Surface {
	t.Helper()
	artStore, err := artifacts.Open(t.Context(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })

	registry := flow.NewRegistry()
	if err := registry.Register(phase163Flow(), flow.Metadata{Owner: "team", Version: "v1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	for i := range 5 {
		if err := registry.RecordRun(flow.RunRecord{
			FlowName:  "flow-window",
			RunID:     "runA-" + string(rune('0'+i)),
			Identity:  idA,
			Trigger:   "user",
			Status:    "succeeded",
			StartedAt: phase163Now.AddDate(0, 0, -(4 - i)), // day -4 .. day 0
			Duration:  10 * time.Millisecond,
		}); err != nil {
			t.Fatalf("RecordRun: %v", err)
		}
	}
	if err := registry.RecordRun(flow.RunRecord{
		FlowName: "flow-window", RunID: "runB-0",
		Identity: identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"},
		Trigger:  "user", Status: "succeeded",
		StartedAt: phase163Now, Duration: 10 * time.Millisecond,
	}); err != nil {
		t.Fatalf("RecordRun(B): %v", err)
	}

	cat, err := flowprotocol.NewRegistryCatalog(registry, artStore, 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	inv, err := flowprotocol.NewFuncInvoker(
		func(_ context.Context, id identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
			return "run-" + flowID + "-" + id.TenantID, phase163Now, nil
		}, registry)
	if err != nil {
		t.Fatalf("NewFuncInvoker: %v", err)
	}
	s, err := flowprotocol.NewSurface(cat, inv)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return s
}

// TestE2E_Phase163_FlowRunsWindow_ServerSideMatchesClientFilter proves
// the server-side bounded read equals the client-side walk-and-filter
// interim over the same StartedAt window.
func TestE2E_Phase163_FlowRunsWindow_ServerSideMatchesClientFilter(t *testing.T) {
	s := newPhase163FlowSurface(t)
	idA := prototypes.IdentityScope{Tenant: "tenant-A", User: "u-A", Session: "s-A"}

	// Unbounded read — the walk-and-filter baseline.
	full, err := s.RunsList(t.Context(), prototypes.FlowRunsListRequest{
		Identity: idA, FlowID: "flow-window", PageSize: 100,
	}, false)
	if err != nil {
		t.Fatalf("RunsList(unbounded): %v", err)
	}

	// Closed window [day-3, day-1]: server-side.
	since := phase163Now.AddDate(0, 0, -3)
	until := phase163Now.AddDate(0, 0, -1)
	bounded, err := s.RunsList(t.Context(), prototypes.FlowRunsListRequest{
		Identity: idA, FlowID: "flow-window", PageSize: 100,
		Since: since, Until: until,
	}, false)
	if err != nil {
		t.Fatalf("RunsList(bounded): %v", err)
	}

	// Client-side reference filter over the full set (inclusive both
	// bounds — the closed window matching TaskFilter/sessions).
	want := 0
	for _, r := range full.Runs {
		if !r.StartedAt.Before(since) && !r.StartedAt.After(until) {
			want++
		}
	}
	if bounded.TotalRows != want {
		t.Fatalf("bounded TotalRows = %d, client-side filter = %d", bounded.TotalRows, want)
	}
	if len(bounded.Runs) != want {
		t.Fatalf("bounded rows = %d, want %d", len(bounded.Runs), want)
	}
	for _, r := range bounded.Runs {
		if r.StartedAt.Before(since) || r.StartedAt.After(until) {
			t.Fatalf("run %s at %v outside [%v,%v]", r.RunID, r.StartedAt, since, until)
		}
	}
}

// TestE2E_Phase163_FlowRunsWindow_CrossTenantRefusal is the identity /
// failure-mode leg: a Tenants widening beyond the caller's tenant without
// the admin claim fails closed.
func TestE2E_Phase163_FlowRunsWindow_CrossTenantRefusal(t *testing.T) {
	s := newPhase163FlowSurface(t)
	_, err := s.RunsList(t.Context(), prototypes.FlowRunsListRequest{
		Identity: prototypes.IdentityScope{Tenant: "tenant-A", User: "u-A", Session: "s-A"},
		FlowID:   "flow-window",
		Tenants:  []string{"tenant-B"}, // cross-tenant, no admin
		Since:    phase163Now.AddDate(0, 0, -7),
	}, false)
	if err == nil {
		t.Fatal("cross-tenant windowed read without admin succeeded; want refusal")
	}
}

// TestE2E_Phase163_EventsRetentionHorizon_BothDrivers reads the observed
// events retention horizon through RetentionProvider over BOTH real event
// bus drivers and asserts it matches the actual oldest retained event.
func TestE2E_Phase163_EventsRetentionHorizon_BothDrivers(t *testing.T) {
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     8,
		IdleTimeout:              200 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         16,
	}
	oldest := phase163Now.AddDate(0, 0, -2)
	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	quad := identity.Quadruple{Identity: id}

	makeInmem := func(t *testing.T) events.EventBus {
		bus, err := eventsinmem.New(cfg, auditpatterns.New())
		if err != nil {
			t.Fatalf("inmem.New: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close(context.Background()) })
		return bus
	}
	makeDurable := func(t *testing.T) events.EventBus {
		store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("stateinmem.New: %v", err)
		}
		dcfg := cfg
		dcfg.Driver = "durable"
		bus, err := eventsdurable.New(t.Context(), dcfg, auditpatterns.New(), store)
		if err != nil {
			t.Fatalf("durable.New: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close(context.Background()) })
		return bus
	}

	for _, tc := range []struct {
		name string
		make func(*testing.T) events.EventBus
	}{
		{"inmem", makeInmem},
		{"durable", makeDurable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := tc.make(t)
			// Publish the oldest first, then two newer events.
			for i, at := range []time.Time{oldest, oldest.Add(time.Hour), oldest.Add(2 * time.Hour)} {
				ev := events.Event{
					Type:       events.EventTypeRuntimeWarning,
					Identity:   quad,
					OccurredAt: at,
					Payload:    events.RedactedMap{Data: map[string]any{"i": i}},
				}
				if err := bus.Publish(t.Context(), ev); err != nil {
					t.Fatalf("Publish: %v", err)
				}
			}

			horizons := runtimeposture.RetentionProvider(bus, nil, nil)(t.Context(), id)
			var got time.Time
			var found bool
			for _, h := range horizons {
				if h.Surface == "events" {
					got, found = h.OldestRetainedAt, true
				}
			}
			if !found {
				t.Fatalf("no events retention horizon reported (%+v)", horizons)
			}
			// Cross-check against the driver's own RetentionReporter read.
			rr := bus.(events.RetentionReporter)
			want, present, err := rr.OldestRetainedAt(t.Context())
			if err != nil || !present {
				t.Fatalf("driver OldestRetainedAt: present=%v err=%v", present, err)
			}
			if !got.Equal(want) || !got.Equal(oldest) {
				t.Fatalf("events horizon = %v, want oldest retained %v (driver %v)", got, oldest, want)
			}
		})
	}
}
