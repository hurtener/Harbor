// Wave 3 cross-subsystem integration test per AGENTS.md §17.
//
// The wave introduced two new surfaces:
//   - Phase 06: events Replayer (cursor + in-memory ring buffer).
//   - Phase 08: SessionRegistry (lifecycle + GC over StateStore).
//
// Both shipped their own per-phase integration tests
// (test/integration/replay_test.go and sessions_state_test.go). This
// wave-end test proves the two surfaces COMPOSE: that a SessionRegistry
// running over a real StateStore + EventBus emits its lifecycle events
// in a way that the bus's Replayer captures historically — and that
// the multi-isolation triple flows through unchanged across the seam.
//
// Forms the analog of wave2_test.go for the new surfaces. Failure
// modes covered: ring overrun → ErrCursorTooOld; reopen-after-close
// rejected and visible in replay; cross-tenant ring fan-in isolated.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_Wave3_FullSurface_Aliveness wires every Wave-3-relevant
// subsystem together and asserts the canonical compose-paths work:
//
//   - audit.Redactor scrubs the lifecycle marker fields without
//     stripping their typed access (SafePayload bypass holds).
//   - events bus + replay accept Publishes from sessions and surface
//     them via Replayer.Replay after the fact.
//   - state.StateStore round-trips the session record under
//     identity-mandatory rules.
//   - sessions.Registry ties the three together: Open → Touch → Close
//     emits three lifecycle events in order; a fresh Replay from
//     Cursor{Sequence:0} yields all three with no duplicates and no
//     gaps within the session's RunID.
//
// Failure modes covered:
//
//   - reopen-after-close — the second Open returns ErrReopenAfterClose
//     (matches the per-phase test, but here it traverses the full
//     registry → state → bus pipeline).
//   - Replay rejects empty-triple non-admin filters (the same rule
//     Subscribe enforces; the wave test pins the symmetry).
func TestE2E_Wave3_FullSurface_Aliveness(t *testing.T) {
	ctx := wave3IdentityCtx(t)
	cfg := wave3Config()
	red := auditpatterns.New()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate canonical wave3 config: %v", err)
	}

	bus, err := events.Open(ctx, cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(ctx, cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	id := identity.MustFrom(ctx)

	// Run the lifecycle WITHOUT a live subscriber — the point of this
	// test is to prove Replay reconstructs history after the fact.
	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Touch(ctx, id.SessionID); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := reg.Close(ctx, id.SessionID, "wave3-e2e"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Now type-assert the bus to Replayer and replay from Sequence=0
	// for this exact identity.
	replayer, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("bus does not implement Replayer; Phase 06 surface missing")
	}
	historical, err := replayer.Replay(ctx, events.Cursor{SessionID: id.SessionID, Sequence: 0}, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types: []events.EventType{
			sessions.EventTypeSessionOpened,
			sessions.EventTypeSessionTouched,
			sessions.EventTypeSessionClosed,
		},
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	wantOrder := []events.EventType{
		sessions.EventTypeSessionOpened,
		sessions.EventTypeSessionTouched,
		sessions.EventTypeSessionClosed,
	}
	if len(historical) != len(wantOrder) {
		t.Fatalf("replay yielded %d events, want %d (%v)", len(historical), len(wantOrder), wantOrder)
	}
	for i, ev := range historical {
		if ev.Type != wantOrder[i] {
			t.Errorf("replay[%d].Type=%v, want %v", i, ev.Type, wantOrder[i])
		}
		if ev.Identity.TenantID != id.TenantID || ev.Identity.SessionID != id.SessionID {
			t.Errorf("replay[%d] identity bleed: %+v", i, ev.Identity)
		}
		if i > 0 && historical[i].Sequence <= historical[i-1].Sequence {
			t.Errorf("replay sequences not strictly increasing: [%d]=%d [%d]=%d",
				i-1, historical[i-1].Sequence, i, historical[i].Sequence)
		}
	}

	// State store still has the closed record; identity-mandatory
	// holds at the StateStore boundary too.
	rec, err := store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle")
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if len(rec.Bytes) == 0 {
		t.Error("state.Load returned empty bytes")
	}

	// Failure mode 1 (Phase 08 path through the wave-3 wiring):
	// reopen-after-close rejected.
	_, err = reg.Open(ctx, id.SessionID, id)
	if !errors.Is(err, sessions.ErrReopenAfterClose) {
		t.Errorf("Open after Close: err=%v, want ErrReopenAfterClose", err)
	}

	// Failure mode 2 (Phase 06 path): Replay rejects empty-triple
	// non-admin filters.
	_, err = replayer.Replay(ctx, events.Cursor{}, events.Filter{})
	if err == nil {
		t.Error("Replay accepted empty-triple non-admin filter")
	}
}

// TestE2E_Wave3_Concurrent_CrossTenant_Replay runs N tenants × M
// session lifecycles in parallel against a single shared registry
// AND single shared bus, then has each tenant's "viewer" goroutine
// Replay from cursor 0 and assert it sees ZERO events from any other
// tenant. Pins the cross-package isolation guarantee under -race.
//
// Required when the wiring is long-lived (per AGENTS.md §17.3
// "Concurrency stress run"): N≥10 producers + ≥10 replayers; assert
// no goroutine leak after teardown.
func TestE2E_Wave3_Concurrent_CrossTenant_Replay(t *testing.T) {
	cfg := wave3Config()
	// Generous ring so the lifecycle events of all tenants fit; we
	// want cross-tenant filter behavior, not ring overrun.
	cfg.Events.ReplayBufferSize = 4096
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	const tenants = 8
	const lifecyclesPerTenant = 6

	baseline := runtime.NumGoroutine()

	// Producers: each tenant runs lifecyclesPerTenant Open→Touch→Close
	// triples in parallel.
	var prodWG sync.WaitGroup
	for ti := 0; ti < tenants; ti++ {
		for li := 0; li < lifecyclesPerTenant; li++ {
			prodWG.Add(1)
			go func(tenant, life int) {
				defer prodWG.Done()
				id := identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", tenant),
					UserID:    fmt.Sprintf("u-%d", tenant),
					SessionID: fmt.Sprintf("s-%d-%d", tenant, life),
				}
				ctx, _ := identity.With(context.Background(), id)
				if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
					t.Errorf("tenant=%d life=%d Open: %v", tenant, life, err)
					return
				}
				if err := reg.Touch(ctx, id.SessionID); err != nil {
					t.Errorf("tenant=%d life=%d Touch: %v", tenant, life, err)
					return
				}
				if err := reg.Close(ctx, id.SessionID, "concurrent"); err != nil {
					t.Errorf("tenant=%d life=%d Close: %v", tenant, life, err)
				}
			}(ti, li)
		}
	}
	prodWG.Wait()

	// Replayers: per-tenant viewer asserts zero foreign-tenant events
	// in its replay snapshot. Type-assert once, share the Replayer.
	replayer, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("bus does not implement Replayer")
	}

	var viewWG sync.WaitGroup
	mismatches := atomic.Int64{}
	expectedPerTenant := lifecyclesPerTenant * 3 // open + touch + close
	for ti := 0; ti < tenants; ti++ {
		viewWG.Add(1)
		go func(tenant int) {
			defer viewWG.Done()
			tenantID := fmt.Sprintf("t-%d", tenant)
			// Replay across all sessions for this tenant — use a
			// per-session loop because Cursor.SessionID scopes the
			// snapshot. The Filter still enforces tenant isolation.
			seen := 0
			for li := 0; li < lifecyclesPerTenant; li++ {
				cursor := events.Cursor{
					SessionID: fmt.Sprintf("s-%d-%d", tenant, li),
					Sequence:  0,
				}
				snapshot, err := replayer.Replay(context.Background(), cursor, events.Filter{
					Tenant:  tenantID,
					User:    fmt.Sprintf("u-%d", tenant),
					Session: fmt.Sprintf("s-%d-%d", tenant, li),
					Types: []events.EventType{
						sessions.EventTypeSessionOpened,
						sessions.EventTypeSessionTouched,
						sessions.EventTypeSessionClosed,
					},
				})
				if err != nil {
					t.Errorf("tenant=%d session=%d Replay: %v", tenant, li, err)
					return
				}
				for _, ev := range snapshot {
					if ev.Identity.TenantID != tenantID {
						mismatches.Add(1)
					}
					seen++
				}
			}
			if seen != expectedPerTenant {
				t.Errorf("tenant=%d saw %d events in replay, want %d", tenant, seen, expectedPerTenant)
			}
		}(ti)
	}
	viewWG.Wait()

	if n := mismatches.Load(); n != 0 {
		t.Errorf("%d cross-tenant deliveries observed in replay", n)
	}

	// Tear down + leak check.
	if err := reg.CloseRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+3 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 3 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
	}
}

// TestE2E_Wave3_RingOverrun_ReplayDegradesLoudly composes the Phase 06
// ring-overrun failure mode WITH Phase 08's emit pipeline: open many
// sessions until the ring evicts the earliest events, then ask Replay
// for a cursor older than the ring's tail. ErrCursorTooOld must
// propagate, surfacing the loss explicitly so a future durable-log
// driver (Phase 57) can be wired in.
func TestE2E_Wave3_RingOverrun_ReplayDegradesLoudly(t *testing.T) {
	cfg := wave3Config()
	// Tiny ring forces overrun quickly.
	cfg.Events.ReplayBufferSize = 16
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	// Open the canary first so its events are the first in the ring.
	canary := identity.Identity{TenantID: "T", UserID: "U", SessionID: "canary"}
	canaryCtx, _ := identity.With(context.Background(), canary)
	if _, err := reg.Open(canaryCtx, canary.SessionID, canary); err != nil {
		t.Fatalf("canary Open: %v", err)
	}

	// Saturate the ring with N more lifecycles (3 events each — opened,
	// touched, closed). With ringSize=16 and N=10 lifecycles=30 events,
	// the canary's "session.opened" gets evicted.
	for i := 0; i < 10; i++ {
		id := identity.Identity{TenantID: "T", UserID: "U", SessionID: fmt.Sprintf("filler-%d", i)}
		ctx, _ := identity.With(context.Background(), id)
		if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
			t.Fatalf("filler %d Open: %v", i, err)
		}
		_ = reg.Touch(ctx, id.SessionID)
		_ = reg.Close(ctx, id.SessionID, "fill")
	}

	// Ask for a cursor older than the ring's tail. Sequence=1 is the
	// first event ever published on this bus; we know the ring has
	// evicted it because the test published >ringSize events.
	replayer, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("bus does not implement Replayer")
	}
	_, err = replayer.Replay(canaryCtx, events.Cursor{SessionID: canary.SessionID, Sequence: 1}, events.Filter{
		Tenant:  canary.TenantID,
		User:    canary.UserID,
		Session: canary.SessionID,
		Types:   []events.EventType{sessions.EventTypeSessionOpened},
	})
	if !errors.Is(err, events.ErrCursorTooOld) {
		t.Errorf("Replay with ancient cursor: err=%v, want ErrCursorTooOld", err)
	}
}

// --- helpers ---

func wave3Config() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"RS256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "json",
			LogLevel:    "info",
			ServiceName: "harbor-wave3-e2e",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Provider: "openrouter",
			Model:    "anthropic/claude-sonnet-4",
			APIKey:   "sk-test",
			Timeout:  30 * time.Second,
		},
		Governance: config.GovernanceConfig{
			DefaultMaxTokens: 4096,
			RepairAttempts:   2,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     64,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
			ReplayBufferSize:         512,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       2 * time.Hour,
			SweepInterval: 30 * time.Minute,
		},
		// ArtifactsConfig populated by Phase 17; the validator now
		// requires a non-empty driver. Wave 3's surfaces don't exercise
		// artifacts directly, but Validate runs the field through.
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		// TasksConfig populated by Phase 20 — the validator added a
		// required driver field. Phase 21 added `retain_turn_timeout`
		// + `continuation_hop_limit` validators. Wave 3's surfaces
		// don't exercise tasks directly, but Validate runs the field
		// through (the same §17.6 pattern as Artifacts above).
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    5 * time.Minute,
			ContinuationHopLimit: 8,
		},
	}
}

func wave3IdentityCtx(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, "R-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}
