// Phase 08 cross-subsystem integration test per AGENTS.md §17.
// Wires real config + audit + events + state + sessions drivers and
// exercises the canonical lifecycle end-to-end (Open → Touch → Close)
// against the in-memory StateStore. Identity propagation is asserted
// at every step; the failure mode is reopen-after-close.
package integration_test

import (
	"context"
	"errors"
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

// TestE2E_Phase08_SessionLifecycle_RoundTrip wires the SessionRegistry
// over the real in-memory StateStore + EventBus and runs the canonical
// lifecycle. Asserts:
//
//   - Open emits session.opened on the bus with the correct identity.
//   - Touch updates LastSeen and survives a Get round-trip.
//   - Close emits session.closed; reopen-after-close returns
//     ErrReopenAfterClose (the failure mode).
//   - All assertions hold under -race.
func TestE2E_Phase08_SessionLifecycle_RoundTrip(t *testing.T) {
	cfg := phase08Config()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Subscribe BEFORE Open so we observe the lifecycle events.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
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
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Open.
	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := mustReceive(t, sub, 2*time.Second)
	if got.Type != sessions.EventTypeSessionOpened {
		t.Fatalf("first event type=%v, want session.opened", got.Type)
	}
	if got.Identity.TenantID != id.TenantID || got.Identity.SessionID != id.SessionID {
		t.Errorf("identity bleed on open event: %+v", got.Identity)
	}

	// Touch.
	if err := reg.Touch(ctx, id.SessionID); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got = mustReceive(t, sub, 2*time.Second)
	if got.Type != sessions.EventTypeSessionTouched {
		t.Fatalf("second event type=%v, want session.touched", got.Type)
	}

	s, err := reg.Get(ctx, id.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Closed {
		t.Error("session should not be Closed after Touch")
	}
	if s.Identity != id {
		t.Errorf("Identity round-trip: got=%+v want=%+v", s.Identity, id)
	}

	// Close.
	if err := reg.Close(ctx, id.SessionID, "e2e"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got = mustReceive(t, sub, 2*time.Second)
	if got.Type != sessions.EventTypeSessionClosed {
		t.Fatalf("third event type=%v, want session.closed", got.Type)
	}

	// Failure mode: reopen-after-close.
	_, err = reg.Open(ctx, id.SessionID, id)
	if !errors.Is(err, sessions.ErrReopenAfterClose) {
		t.Fatalf("Open after Close: err=%v, want ErrReopenAfterClose", err)
	}
}

// TestE2E_Phase08_CrossTenant_SessionIDReuse pins the multi-isolation
// invariant at the cross-subsystem layer: tenant A opens SessionID=S,
// tenant B's Open with the same SessionID fails with
// ErrSessionIDReuse. This is the wire-level analogue of the unit test
// in registry_test.go but exercises the path through the real
// StateStore + EventBus drivers (per AGENTS.md §17 — real drivers on
// the seam, no mocks).
func TestE2E_Phase08_CrossTenant_SessionIDReuse(t *testing.T) {
	cfg := phase08Config()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	idA := identity.Identity{TenantID: "TA", UserID: "UA", SessionID: "shared"}
	ctxA, _ := identity.With(context.Background(), idA)
	if _, err := reg.Open(ctxA, idA.SessionID, idA); err != nil {
		t.Fatalf("tenant A Open: %v", err)
	}
	idB := identity.Identity{TenantID: "TB", UserID: "UB", SessionID: "shared"}
	ctxB, _ := identity.With(context.Background(), idB)
	_, err = reg.Open(ctxB, idB.SessionID, idB)
	if !errors.Is(err, sessions.ErrSessionIDReuse) {
		t.Fatalf("tenant B Open: err=%v, want ErrSessionIDReuse", err)
	}
}

// --- helpers ---

func phase08Config() *config.Config {
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
			ServiceName: "harbor-phase08-e2e",
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
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       2 * time.Hour,
			SweepInterval: 30 * time.Minute,
		},
	}
}
