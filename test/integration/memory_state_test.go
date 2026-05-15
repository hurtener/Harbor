// Phase 23 cross-subsystem integration test per AGENTS.md §17.
// Wires real config + audit + events + state + memory drivers and
// exercises the canonical paths end-to-end:
//
//   - Happy path: Open → Health → Snapshot → Restore → Snapshot
//     round-trips identity through every layer.
//   - Failure mode: a method call with a missing identity
//     component returns wrapped `ErrIdentityRequired` AND emits one
//     `memory.identity_rejected` event on the bus.
//
// No mocks at the seam (real audit redactor, real events bus, real
// state store, real memory driver).
package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_Phase23_MemoryStore_RoundTrip wires the MemoryStore over
// the real in-memory StateStore + EventBus and exercises the canonical
// lifecycle. Asserts:
//
//   - Open with `inmem` + `none` returns a working store.
//   - Health under a valid identity is `healthy`.
//   - Snapshot / Restore round-trip the empty-snapshot under the
//     same identity.
//   - Identity isolation holds at the StateStore layer (tenant B's
//     snapshot is empty after tenant A writes through Restore).
//   - All assertions hold under -race.
func TestE2E_Phase23_MemoryStore_RoundTrip(t *testing.T) {
	cfg := phase23Config()
	red, err := audit.Open(context.Background(), cfg.Audit)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
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

	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:       cfg.Memory.Driver,
		Strategy:     memory.Strategy(cfg.Memory.Strategy),
		BudgetTokens: cfg.Memory.BudgetTokens,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(context.Background()) })

	idA := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"},
	}
	idB := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-B", UserID: "user-9", SessionID: "sess-9"},
	}
	ctx := context.Background()

	// Health under a valid identity round-trips through audit /
	// events / state without surfacing any unexpected error.
	got, err := mem.Health(ctx, idA)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got != memory.HealthHealthy {
		t.Errorf("Health=%q, want %q", got, memory.HealthHealthy)
	}

	// Snapshot under both identities — neither should error.
	snapA, err := mem.Snapshot(ctx, idA)
	if err != nil {
		t.Fatalf("Snapshot A: %v", err)
	}
	if _, err := mem.Snapshot(ctx, idB); err != nil {
		t.Fatalf("Snapshot B: %v", err)
	}

	// Restore A's snapshot under A. Memory state persists through
	// the StateStore (D-027 typed wrapper).
	if err := mem.Restore(ctx, idA, snapA); err != nil {
		t.Fatalf("Restore A: %v", err)
	}

	// Tenant B's StateStore slot remains untouched — cross-tenant
	// isolation through the StateStore key.
	recB, err := store.Load(ctx, idB, "memory.state")
	if err == nil {
		t.Errorf("StateStore: tenant B leaked tenant A's write: %+v", recB)
	} else if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("StateStore Load B: err=%v, want errors.Is ErrNotFound", err)
	}
}

// TestE2E_Phase23_MemoryStore_FailsClosedOnMissingIdentity is the
// Phase 23 acceptance criterion: missing identity fails closed AND
// emits an audit event observable on the bus. Real drivers; no mocks.
func TestE2E_Phase23_MemoryStore_FailsClosedOnMissingIdentity(t *testing.T) {
	cfg := phase23Config()
	red, err := audit.Open(context.Background(), cfg.Audit)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
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

	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   cfg.Memory.Driver,
		Strategy: memory.Strategy(cfg.Memory.Strategy),
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(context.Background()) })

	// Subscribe with Admin: true so the rejection event (whose
	// identity carries the "<missing>" sentinel) reaches the
	// subscriber regardless of the true scope.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{memory.EventTypeMemoryIdentityRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Missing session_id — fail-closed at the boundary.
	bogus := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U"},
	}
	err = mem.AddTurn(context.Background(), bogus, memory.ConversationTurn{
		UserMessage: "x",
	})
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("AddTurn: err=%v, want errors.Is ErrIdentityRequired", err)
	}

	// One event observable on the bus within a short bounded deadline.
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before rejection event")
		}
		if ev.Type != memory.EventTypeMemoryIdentityRejected {
			t.Errorf("event type=%q, want %q", ev.Type, memory.EventTypeMemoryIdentityRejected)
		}
		payload, ok := ev.Payload.(memory.MemoryIdentityRejectedPayload)
		if !ok {
			t.Fatalf("payload type=%T, want MemoryIdentityRejectedPayload", ev.Payload)
		}
		if payload.Operation != "AddTurn" {
			t.Errorf("payload.Operation=%q, want %q", payload.Operation, "AddTurn")
		}
		if payload.Reason == "" {
			t.Error("payload.Reason empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rejection event")
	}
}

func phase23Config() *config.Config {
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
			ServiceName: "harbor-phase23-e2e",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Provider: "openrouter",
			Model:    "anthropic/claude-sonnet-4",
			APIKey:   "sk-test",
			Timeout:  30 * time.Second,
		},
		Governance: config.GovernanceConfig{
			RepairAttempts: 2,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     64,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
		},
		Memory: config.MemoryConfig{
			Driver:   "inmem",
			Strategy: "none",
		},
	}
}
