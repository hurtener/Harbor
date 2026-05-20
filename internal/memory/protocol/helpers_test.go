package protocol_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// testIdentity is the deterministic identity quadruple every memory/
// protocol unit test scopes against. Documented dummy values (CLAUDE.md
// §7 rule 2 — no real secrets).
func testIdentity() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "t-test",
			UserID:    "u-test",
			SessionID: "s-test",
		},
	}
}

// memHarness bundles the real drivers a memory/protocol test composes
// over — no mocks at the seam (CLAUDE.md §17.3).
type memHarness struct {
	store     memory.MemoryStore
	bus       events.EventBus
	artifacts artifacts.ArtifactStore
}

// newMemHarness builds a real in-mem MemoryStore (on a real StateStore
// + real EventBus) and a real in-mem ArtifactStore. strategy selects
// the memory strategy; budgetTokens bounds the truncation window. The
// EventBus is constructed with a non-zero replay buffer so the events
// Aggregator's Replayer surface works.
func newMemHarness(t *testing.T, strat memory.Strategy, budgetTokens int) memHarness {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	stateStore, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close(context.Background()) })

	store, err := memoryinmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     strat,
		BudgetTokens: budgetTokens,
	}, memory.Deps{State: stateStore, Bus: bus}, memoryinmem.Options{})
	if err != nil {
		t.Fatalf("memoryinmem.New(%q): %v", strat, err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	artStore, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })

	return memHarness{store: store, bus: bus, artifacts: artStore}
}

// seedTurns appends n conversation turns to the harness's memory store
// for the given identity. Each turn carries deterministic user /
// assistant text so a ContentSearch facet test can target a known
// substring.
func seedTurns(t *testing.T, h memHarness, id identity.Quadruple, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		turn := memory.ConversationTurn{
			UserMessage:       "question " + strings.Repeat("q", i),
			AssistantResponse: "answer " + strings.Repeat("a", i),
			Timestamp:         time.Now().UTC().Add(time.Duration(i) * time.Second),
		}
		if err := h.store.AddTurn(context.Background(), id, turn); err != nil {
			t.Fatalf("AddTurn[%d]: %v", i, err)
		}
	}
}

// seedHeavyTurn appends one conversation turn whose value bytes exceed
// sizeBytes (so the memory.get heavy-content bypass fires). The padding
// goes into the assistant response.
func seedHeavyTurn(t *testing.T, h memHarness, id identity.Quadruple, sizeBytes int) {
	t.Helper()
	turn := memory.ConversationTurn{
		UserMessage:       "heavy",
		AssistantResponse: strings.Repeat("X", sizeBytes),
		Timestamp:         time.Now().UTC(),
	}
	if err := h.store.AddTurn(context.Background(), id, turn); err != nil {
		t.Fatalf("AddTurn(heavy): %v", err)
	}
}

// newAggregator builds an events Aggregator over the harness's bus so
// the 24-hour memory.* counter tests have a Replayer surface.
func newAggregator(t *testing.T, h memHarness) *events.Aggregator {
	t.Helper()
	agg, err := events.NewAggregator(h.bus)
	if err != nil {
		t.Fatalf("events.NewAggregator: %v", err)
	}
	return agg
}
