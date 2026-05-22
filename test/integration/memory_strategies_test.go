// Phase 24 cross-subsystem integration test per AGENTS.md §17.
// Wires real config + audit + events + state + memory drivers +
// the stub Summarizer (`strategy.EchoSummarizer` / a forced-failing
// one) and exercises the canonical paths end-to-end:
//
//   - `truncation` strategy round-trips a multi-turn conversation
//     through AddTurn → Snapshot → Restore.
//   - `rolling_summary` strategy with a working summariser produces
//     a non-empty summary after the recent-window overflows.
//   - `rolling_summary` strategy with a forced-failing summariser
//     transitions `healthy → retry → degraded` and emits
//     `memory.health_changed` events observable on the bus.
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
	"github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_Phase24_Truncation_RoundTripsBuffer asserts the
// truncation strategy round-trips a multi-turn buffer through real
// drivers. Per AGENTS.md §17: real drivers everywhere on the seam.
func TestE2E_Phase24_Truncation_RoundTripsBuffer(t *testing.T) {
	bus, store := buildE2EDeps(t)
	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyTruncation,
		BudgetTokens: 256,
	}, memory.Deps{State: store, Bus: bus}, inmem.Options{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(context.Background()) })

	id := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	ctx := context.Background()
	for i := range 3 {
		if err := mem.AddTurn(ctx, id, memory.ConversationTurn{
			UserMessage:       "hello",
			AssistantResponse: "world",
		}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	patch, err := mem.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if len(patch.RecentTurns) != 3 {
		t.Errorf("RecentTurns=%d, want 3", len(patch.RecentTurns))
	}
	// Round-trip through Snapshot/Restore.
	snap, err := mem.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := mem.Restore(ctx, id, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	patch2, err := mem.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext after restore: %v", err)
	}
	if len(patch2.RecentTurns) != 3 {
		t.Errorf("RecentTurns after restore=%d, want 3", len(patch2.RecentTurns))
	}
}

// TestE2E_Phase24_RollingSummary_HappyPath wires a working
// EchoSummarizer and asserts: (a) AddTurns trigger summarisation,
// (b) the summary materialises in subsequent GetLLMContext calls,
// (c) Health stays healthy across the lifecycle.
func TestE2E_Phase24_RollingSummary_HappyPath(t *testing.T) {
	bus, store := buildE2EDeps(t)
	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyRollingSummary,
	}, memory.Deps{State: store, Bus: bus}, inmem.Options{
		Summarizer: strategy.EchoSummarizer{},
	})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(context.Background()) })

	id := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	ctx := context.Background()
	for i := range 8 {
		if err := mem.AddTurn(ctx, id, memory.ConversationTurn{
			UserMessage:       "hello",
			AssistantResponse: "world",
		}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	patch, err := mem.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Summary == "" {
		t.Error("happy-path rolling_summary returned empty summary after 8 turns")
	}
	got, err := mem.Health(ctx, id)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got != memory.HealthHealthy {
		t.Errorf("Health=%q, want %q", got, memory.HealthHealthy)
	}
}

// TestE2E_Phase24_RollingSummary_DegradedTransitionObservable wires
// a forced-failing summariser and asserts the failure path produces
// observable `memory.health_changed` events on the bus. Per
// AGENTS.md §17 ≥1 failure mode.
func TestE2E_Phase24_RollingSummary_DegradedTransitionObservable(t *testing.T) {
	bus, store := buildE2EDeps(t)
	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyRollingSummary,
	}, memory.Deps{State: store, Bus: bus}, inmem.Options{
		Summarizer:         &alwaysFailSummarizer{},
		RecoveryBacklogMax: 4,
	})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(context.Background()) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{memory.EventTypeMemoryHealthChanged},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	id := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	ctx := context.Background()
	// Push enough turns to spill into pending repeatedly and
	// exhaust the retry budget.
	for i := range 12 {
		if err := mem.AddTurn(ctx, id, memory.ConversationTurn{
			UserMessage:       "u",
			AssistantResponse: "a",
		}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	got, err := mem.Health(ctx, id)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got != memory.HealthDegraded {
		t.Errorf("Health=%q, want %q", got, memory.HealthDegraded)
	}

	// At least one health_changed event observable on the bus
	// within a bounded deadline.
	deadline := time.After(2 * time.Second)
	sawTransition := false
	for !sawTransition {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before health_changed event")
			}
			if ev.Type != memory.EventTypeMemoryHealthChanged {
				continue
			}
			payload, ok := ev.Payload.(memory.HealthChangedPayload)
			if !ok {
				t.Fatalf("payload type=%T, want HealthChangedPayload", ev.Payload)
			}
			if payload.NewHealth == "" {
				t.Error("payload.NewHealth empty")
			}
			sawTransition = true
		case <-deadline:
			t.Fatal("timed out waiting for health_changed event")
		}
	}
}

// TestE2E_Phase24_FailsClosedOnMissingIdentity is the regression
// for the Phase 23 contract under the new strategies: missing
// identity still fails closed and emits `memory.identity_rejected`
// even when the strategy is `truncation` or `rolling_summary`.
func TestE2E_Phase24_FailsClosedOnMissingIdentity(t *testing.T) {
	bus, store := buildE2EDeps(t)
	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyTruncation,
	}, memory.Deps{State: store, Bus: bus}, inmem.Options{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(context.Background()) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{memory.EventTypeMemoryIdentityRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	bogus := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U"},
	}
	err = mem.AddTurn(context.Background(), bogus, memory.ConversationTurn{UserMessage: "x"})
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("AddTurn: err=%v, want errors.Is ErrIdentityRequired", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != memory.EventTypeMemoryIdentityRejected {
			t.Errorf("event type=%q, want %q", ev.Type, memory.EventTypeMemoryIdentityRejected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rejection event")
	}
}

// alwaysFailSummarizer is the test-grade `memory.Summarizer` that
// always returns an error. Used to drive the failure → degraded
// path under real drivers in the integration test.
type alwaysFailSummarizer struct{}

func (*alwaysFailSummarizer) Summarize(_ context.Context, _ identity.Quadruple, _ memory.SummarizeRequest) (memory.SummarizeResponse, error) {
	return memory.SummarizeResponse{}, errors.New("forced summariser failure")
}

func buildE2EDeps(t *testing.T) (events.EventBus, state.StateStore) {
	t.Helper()
	cfg := phase24Config()
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
	return bus, store
}

func phase24Config() *config.Config {
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
			ServiceName: "harbor-phase24-e2e",
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
			SubscriberBufferSize:     128,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
		},
		Memory: config.MemoryConfig{
			Driver:             "inmem",
			Strategy:           "truncation",
			RecoveryBacklogMax: 4,
		},
	}
}
