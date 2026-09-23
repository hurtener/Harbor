// Administrative memory integration through the real audit, event, StateStore
// and cumulative-memory drivers. Exercise committed notes, identity isolation,
// deletion and observable identity rejection.
package integration_test

import (
	"context"
	"errors"
	"strings"
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

// TestE2E_Phase23_MemoryStore_RoundTrip exercises the shared administrative owner.
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
	}, memory.Deps{State: store, Bus: bus, Redactor: red})
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

	// Enabled memory stores the note once in the cumulative owner.
	key, err := mem.Put(ctx, idA, memory.ConversationTurn{UserMessage: "keep the north exit open", AssistantResponse: "noted"})
	if err != nil || key == "" {
		t.Fatalf("Put: key=%q err=%v", key, err)
	}
	view, err := mem.Inspect(ctx, idA)
	if err != nil || view.Health != memory.HealthHealthy || len(view.Items) != 1 {
		t.Fatalf("Inspect A: %+v, err=%v", view, err)
	}
	if view.Items[0].Key != key || !strings.Contains(string(view.Items[0].Value), "keep the north exit open") {
		t.Fatalf("committed note missing: %+v", view.Items[0])
	}
	other, err := mem.Inspect(ctx, idB)
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("Inspect B leaked note: %+v, err=%v", other, err)
	}
	if _, err := mem.Delete(ctx, idB, key); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("foreign Delete: %v", err)
	}
	unchanged, err := mem.Inspect(ctx, idA)
	if err != nil || len(unchanged.Items) != 1 || unchanged.Items[0].Key != key {
		t.Fatalf("foreign Delete changed owner: %+v, err=%v", unchanged, err)
	}
	if remaining, err := mem.Delete(ctx, idA, key); err != nil || remaining != 0 {
		t.Fatalf("owner Delete: remaining=%d err=%v", remaining, err)
	}
	deleted, err := mem.Inspect(ctx, idA)
	if err != nil || len(deleted.Items) != 0 {
		t.Fatalf("deleted note survived: %+v, err=%v", deleted, err)
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
	}, memory.Deps{State: store, Bus: bus, Redactor: red})
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
	_, err = mem.Put(context.Background(), bogus, memory.ConversationTurn{
		UserMessage: "x",
	})
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("Put: err=%v, want errors.Is ErrIdentityRequired", err)
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
		if payload.Operation != "Put" {
			t.Errorf("payload.Operation=%q, want %q", payload.Operation, "Put")
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
			Strategy: "rolling_summary",
		},
	}
}
