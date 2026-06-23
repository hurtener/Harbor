// Cross-subsystem integration test (AGENTS.md §17) for the memory
// context-budget seam: a real SQLite StateStore backing a
// `rolling_summary` memory store, plus the LLM-client safety edge.
//
// It proves the Phase 123 fix end-to-end: a long-lived session that
// previously poisoned itself (the assembled context grew until the
// D-026 byte check tripped at planner step 0) now compacts to within
// its configured token budget, and the assembled CompleteRequest —
// carrying a large rolling summary as CONVERSATION text — passes the
// safety edge without `ErrContextLeak` (the D-241 refinement) while
// staying under the model's context window.
//
// Real drivers everywhere on the seam: real audit redactor, real
// events bus, real SQLite state store, real memory driver, real
// safety-wrapped LLM client (mock provider underneath). Identity
// propagates through every layer; the budget=0 path is the forced
// failure mode (still governed by the token-window guard).
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

const budgetHeavyThreshold = 32 * 1024

// budgetSeam bundles the wired subsystems for a budget integration test.
type budgetSeam struct {
	mem    memory.MemoryStore
	client llm.LLMClient
}

// newBudgetSeam wires a real SQLite StateStore + rolling_summary memory
// + a safety-wrapped LLM client (mock provider) with the given memory
// budget and model context window.
func newBudgetSeam(t *testing.T, budgetTokens, windowTokens int) budgetSeam {
	t.Helper()
	ctx := context.Background()

	red, err := audit.Open(ctx, config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     128,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(ctx) })

	// Real SQLite StateStore — the rolling_summary executor persists
	// its recent window + summary through here.
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := state.Open(ctx, config.StateConfig{Driver: "sqlite", DSN: dbPath})
	if err != nil {
		t.Fatalf("state.Open(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })

	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyRollingSummary,
		BudgetTokens: budgetTokens,
	}, memory.Deps{State: store, Bus: bus, Summarizer: strategy.EchoSummarizer{}}, inmem.Options{Summarizer: strategy.EchoSummarizer{}})
	if err != nil {
		t.Fatalf("memory inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(ctx) })

	artStore, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	client, err := llm.Open(ctx, llm.ConfigSnapshot{
		Driver:               "mock",
		Provider:             "mock",
		Model:                "m",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: budgetHeavyThreshold,
		ModelProfiles: map[string]llm.ModelProfile{
			"m": {ContextWindowTokens: windowTokens, TokenEstimator: "chars_div_4"},
		},
	}, llm.Deps{Artifacts: artStore, Bus: bus})
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(ctx) })

	return budgetSeam{mem: mem, client: client}
}

// patchToRequest assembles a CompleteRequest from a memory patch: the
// rolling summary becomes a system message (conversation context), each
// recent turn becomes a user+assistant pair.
func patchToRequest(patch memory.LLMContextPatch) llm.CompleteRequest {
	var msgs []llm.ChatMessage
	if patch.Summary != "" {
		s := "Conversation so far:\n" + patch.Summary
		msgs = append(msgs, llm.ChatMessage{Role: llm.RoleSystem, Content: llm.Content{Text: &s}})
	}
	for _, turn := range patch.RecentTurns {
		u := turn.UserMessage
		a := turn.AssistantResponse
		msgs = append(msgs,
			llm.ChatMessage{Role: llm.RoleUser, Content: llm.Content{Text: &u}},
			llm.ChatMessage{Role: llm.RoleAssistant, Content: llm.Content{Text: &a}},
		)
	}
	return llm.CompleteRequest{Model: "m", Messages: msgs}
}

func budgetIdentity(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestE2E_Phase123_MemoryLLMBudget_StaysRunnable replays a ≥50-turn
// poisoning sequence into one key with a 32 KiB token budget and a
// 32 KiB heavy threshold; the assembled context fits the budget and
// passes the safety edge (no ErrContextLeak — the large summary is
// conversation text, exempt by D-241 — and within the context window).
func TestE2E_Phase123_MemoryLLMBudget_StaysRunnable(t *testing.T) {
	const budget = 32 * 1024
	// Context window comfortably larger than the budget so a
	// within-budget assembled request passes the token-window guard.
	seam := newBudgetSeam(t, budget, 128*1024)
	ctx := budgetIdentity(t)
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1",
	}}

	for i := range 60 {
		turn := memory.ConversationTurn{
			UserMessage:       fmt.Sprintf("turn-%02d-", i) + strings.Repeat("u", 2000),
			AssistantResponse: strings.Repeat("a", 2000),
		}
		if err := seam.mem.AddTurn(ctx, id, turn); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}

	patch, err := seam.mem.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Tokens > budget {
		t.Fatalf("assembled context not within budget: Tokens=%d, budget=%d", patch.Tokens, budget)
	}
	// Exercise the D-241 refinement: the rolling summary alone exceeds
	// the 32 KiB byte heavy threshold but is legitimate conversation
	// context, so it must NOT trip ErrContextLeak.
	if len(patch.Summary) <= budgetHeavyThreshold {
		t.Fatalf("test precondition: summary (%d bytes) should exceed the heavy threshold (%d) to exercise D-241",
			len(patch.Summary), budgetHeavyThreshold)
	}

	req := patchToRequest(patch)
	resp, err := seam.client.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete on within-budget assembled context failed: %v", err)
	}
	if resp.Content == "" {
		t.Error("mock provider returned empty content")
	}
}

// TestE2E_Phase123_MemoryLLMBudget_ZeroUnboundedTripsTokenGuard is the
// forced failure mode: with budget=0 the rolling_summary strategy is
// unbounded, so a long session assembles an over-window request — the
// token-window guard (unchanged by this phase) still governs
// conversation size and fails loudly with ErrContextWindowExceeded.
func TestE2E_Phase123_MemoryLLMBudget_ZeroUnboundedTripsTokenGuard(t *testing.T) {
	// budget=0 (unbounded memory) + a small model window so the
	// accumulated context overflows it.
	seam := newBudgetSeam(t, 0, 512)
	ctx := budgetIdentity(t)
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1",
	}}

	for i := range 60 {
		turn := memory.ConversationTurn{
			UserMessage:       fmt.Sprintf("turn-%02d-", i) + strings.Repeat("u", 800),
			AssistantResponse: strings.Repeat("a", 800),
		}
		if err := seam.mem.AddTurn(ctx, id, turn); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	patch, err := seam.mem.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	req := patchToRequest(patch)
	_, err = seam.client.Complete(ctx, req)
	if err == nil {
		t.Fatal("expected the token-window guard to fire on an unbounded conversation")
	}
	// The guard is the governor; the leak check must NOT be what fires
	// for conversation text.
	if !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("err=%v, want ErrContextWindowExceeded", err)
	}
}

// TestE2E_Phase123_MemoryLLMBudget_IdentityMandatory proves identity
// propagation across the seam: the memory store rejects a missing
// identity, and the LLM safety edge rejects a request with no identity
// in ctx.
func TestE2E_Phase123_MemoryLLMBudget_IdentityMandatory(t *testing.T) {
	seam := newBudgetSeam(t, 4096, 128*1024)

	// Memory store fails closed on a missing identity component.
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1"}}
	if err := seam.mem.AddTurn(context.Background(), bad, memory.ConversationTurn{UserMessage: "x", AssistantResponse: "y"}); err == nil {
		t.Fatal("memory AddTurn accepted a missing-session identity")
	}

	// LLM edge fails closed when ctx carries no identity.
	text := "hello"
	_, err := seam.client.Complete(context.Background(), llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, llm.ErrIdentityMissing) {
		t.Fatalf("Complete with no identity: err=%v, want ErrIdentityMissing", err)
	}
}
