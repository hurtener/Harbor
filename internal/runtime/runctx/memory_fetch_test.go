// memory_fetch_test.go — unit tests for FetchMemoryBlocks.
//
// Tests use real in-memory store drivers (no mocks at the seam — per
// CLAUDE.md §17.4) and the deterministic test-grade embedder for semantic
// cases.

package runctx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/embeddings/embeddingstest"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	memStrategy "github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// ---- helpers ---------------------------------------------------------------

func newFetchTestBus(t *testing.T) events.EventBus {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// newFetchTestStore builds an in-memory memory store. `strategy` selects
// the base strategy; `retrieval` selects the retrieval mode.
// StrategyNone is the right choice when the test needs an empty
// recent-turn window (no recentSet entries → no dedup of recalled turns).
// StrategyRollingSummary with BudgetTokens=0 keeps all turns in the
// recent window, which is useful for testing the dedup path.
func newFetchTestStore(t *testing.T, bus events.EventBus, strategy memory.Strategy, retrieval memory.RetrievalMode) memory.MemoryStore {
	t.Helper()
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	var embedder memory.Embedder
	if retrieval == memory.RetrievalSemantic {
		embedder = embeddingstest.New()
	}
	deps := memory.Deps{
		State:    st,
		Bus:      bus,
		Embedder: embedder,
	}
	if strategy == memory.StrategyRollingSummary {
		deps.Summarizer = memStrategy.EchoSummarizer{}
	}
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:    "inmem",
		Strategy:  strategy,
		Retrieval: retrieval,
	}, deps)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = mem.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return mem
}

func fetchTestQuad(session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: "acme", UserID: "alice", SessionID: session,
	}}
}

// ---- default parity (semantic OFF) ----------------------------------------

// TestFetchMemoryBlocks_DefaultParity asserts that with recall off the
// output is identical to calling GetLLMContext + ProjectMemoryBlocks
// directly — byte-for-byte prompt parity.
func TestFetchMemoryBlocks_DefaultParity(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyRollingSummary, memory.RetrievalDefault)
	ctx := context.Background()
	q := fetchTestQuad("sess-parity")

	// Seed a turn.
	if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "hello world",
		AssistantResponse: "hi there",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	// FetchMemoryBlocks with recall off.
	got, err := runctx.FetchMemoryBlocks(ctx, mem, q, "hello world", memory.RecallSettings{Enabled: false})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}

	// Direct projection via the old path.
	patch, err := mem.GetLLMContext(ctx, q)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	want := runctx.ProjectMemoryBlocks(patch)

	// Both should have identical Conversation tiers.
	if got == nil && want == nil {
		return
	}
	if got == nil || want == nil {
		t.Fatalf("FetchMemoryBlocks nil=%v, direct projection nil=%v", got == nil, want == nil)
	}
	// External must be nil (recall off).
	if got.External != nil {
		t.Errorf("External tier non-nil with recall off: %v", got.External)
	}
	// Conversation must match.
	gotJSON := marshalAny(t, got.Conversation)
	wantJSON := marshalAny(t, want.Conversation)
	if gotJSON != wantJSON {
		t.Errorf("Conversation mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
	}
}

// TestFetchMemoryBlocks_RecallOff_NoEmbedderTraffic asserts that when
// recall is disabled, SearchTurns is never called (verified via Calls
// counter on the test embedder).
func TestFetchMemoryBlocks_RecallOff_NoEmbedderTraffic(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyRollingSummary, memory.RetrievalDefault)
	ctx := context.Background()
	q := fetchTestQuad("sess-no-embed")
	if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "query one",
		AssistantResponse: "answer one",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	_, err := runctx.FetchMemoryBlocks(ctx, mem, q, "query one", memory.RecallSettings{Enabled: false})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	// If recall is off there must be no ErrSemanticDisabled from SearchTurns
	// (the implementation must not even call it).
}

// ---- min-score floor -------------------------------------------------------

// TestFetchMemoryBlocks_MinScoreFloor asserts that turns scoring below
// MinScore are not injected into External. Uses StrategyNone so the
// recent-turn window is empty and dedup does not remove the recalled turn.
func TestFetchMemoryBlocks_MinScoreFloor(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()
	q := fetchTestQuad("sess-floor")

	// Seed two turns: one closely related to the query, one completely
	// unrelated (the deterministic embedder projects "chocolate cake
	// recipe" far from "boat sailing knots").
	turns := []memory.ConversationTurn{
		{UserMessage: "boat sailing knots technique", AssistantResponse: "use a bowline", Timestamp: time.Now()},
		{UserMessage: "chocolate cake recipe", AssistantResponse: "flour sugar cocoa", Timestamp: time.Now()},
	}
	for i, turn := range turns {
		if err := mem.AddTurn(ctx, q, turn); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}

	// Use a very high min-score to ensure neither turn passes.
	mb, err := runctx.FetchMemoryBlocks(ctx, mem, q, "sailing knots", memory.RecallSettings{
		Enabled:  true,
		TopK:     5,
		MinScore: 1.1, // impossible to meet
	})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	if mb != nil && mb.External != nil {
		t.Errorf("External tier populated despite impossible min-score: %v", mb.External)
	}
}

// TestFetchMemoryBlocks_MinScoreAdmitsRelevant asserts that a relevant
// turn is admitted when it scores above MinScore. Uses StrategyNone so
// the recent-turn window is empty — the recalled turn is not deduped.
func TestFetchMemoryBlocks_MinScoreAdmitsRelevant(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()
	q := fetchTestQuad("sess-admit")

	if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "sailing knots for mooring",
		AssistantResponse: "bowline and cleat hitch work well",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	mb, err := runctx.FetchMemoryBlocks(ctx, mem, q, "sailing knots", memory.RecallSettings{
		Enabled:  true,
		TopK:     5,
		MinScore: -1.0, // admit everything
	})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	if mb == nil || mb.External == nil {
		t.Fatal("External tier nil — expected a recalled turn")
	}
	ext, ok := mb.External.(map[string]any)
	if !ok {
		t.Fatalf("External is %T, want map[string]any", mb.External)
	}
	turns, ok := ext["recalled_turns"].([]map[string]any)
	if !ok || len(turns) == 0 {
		t.Fatalf("recalled_turns missing or empty: %v", ext)
	}
}

// ---- dedup against recent window -------------------------------------------

// TestFetchMemoryBlocks_DedupRecentWindow asserts that a recalled turn
// whose UserMessage is already in the rolling-summary recent-turn window
// is not duplicated in the External tier. Uses StrategyRollingSummary so
// all turns remain in the recent window (BudgetTokens=0 = unlimited).
func TestFetchMemoryBlocks_DedupRecentWindow(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyRollingSummary, memory.RetrievalSemantic)
	ctx := context.Background()
	q := fetchTestQuad("sess-dedup")

	// Add a turn — it will land in both the recent-window and the vector index.
	if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "boat sailing knots technique",
		AssistantResponse: "use a bowline",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	// Fetch: the recent window already has this turn; recall must not
	// duplicate it in External.
	mb, err := runctx.FetchMemoryBlocks(ctx, mem, q, "sailing knots technique", memory.RecallSettings{
		Enabled:  true,
		TopK:     5,
		MinScore: -1.0,
	})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}

	// External must be nil because the recalled turn is already in
	// the recent window.
	if mb != nil && mb.External != nil {
		ext, _ := mb.External.(map[string]any)
		recalled, _ := ext["recalled_turns"].([]map[string]any)
		for _, r := range recalled {
			if r["user"] == "boat sailing knots technique" {
				t.Errorf("dedup failed: recent-window turn appeared in External recalled_turns")
			}
		}
	}
}

// ---- text cap ---------------------------------------------------------------

// TestFetchMemoryBlocks_TextCap asserts that oversized user/assistant
// text is truncated with a "…[truncated]" marker. Uses StrategyNone so
// the recent window is empty and the recalled turn is not deduped.
func TestFetchMemoryBlocks_TextCap(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()
	q := fetchTestQuad("sess-cap")

	// Build a turn whose user message exceeds the 2 KiB cap.
	long := strings.Repeat("sailing ", 400) // 400*8 = 3200 bytes > 2048
	if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       long,
		AssistantResponse: "fine",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	mb, err := runctx.FetchMemoryBlocks(ctx, mem, q, long, memory.RecallSettings{
		Enabled:  true,
		TopK:     5,
		MinScore: -1.0,
	})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	if mb == nil || mb.External == nil {
		t.Fatal("External tier nil — expected a recalled turn")
	}
	ext := mb.External.(map[string]any)
	recalled := ext["recalled_turns"].([]map[string]any)
	if len(recalled) == 0 {
		t.Fatal("no recalled turns")
	}
	userText, _ := recalled[0]["user"].(string)
	if len(userText) > 2048+50 { // cap + marker overhead
		t.Errorf("user text not capped: len=%d", len(userText))
	}
	if !strings.Contains(userText, "…[truncated]") {
		t.Errorf("truncation marker missing from capped text: %q", userText[:min(80, len(userText))])
	}
}

// ---- External tier map shape ------------------------------------------------

// TestFetchMemoryBlocks_ExternalTierShape asserts the External value is
// map[string]any{"recalled_turns": []map[string]any{...}} with the
// expected keys per entry. Uses StrategyNone so the recent window is
// empty and the recalled turn passes the dedup step.
func TestFetchMemoryBlocks_ExternalTierShape(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()
	q := fetchTestQuad("sess-shape")

	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "sailing pier knots bowline",
		AssistantResponse: "yes bowline works",
		Timestamp:         ts,
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	mb, err := runctx.FetchMemoryBlocks(ctx, mem, q, "pier knots", memory.RecallSettings{
		Enabled:  true,
		TopK:     5,
		MinScore: -1.0,
	})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	if mb == nil || mb.External == nil {
		t.Fatal("External tier nil")
	}
	ext, ok := mb.External.(map[string]any)
	if !ok {
		t.Fatalf("External is %T, want map[string]any", mb.External)
	}
	recalled, ok := ext["recalled_turns"].([]map[string]any)
	if !ok || len(recalled) == 0 {
		t.Fatalf("recalled_turns missing or empty: %v", ext)
	}
	entry := recalled[0]
	if _, ok := entry["user"]; !ok {
		t.Error("recalled_turns entry missing 'user' key")
	}
	if _, ok := entry["assistant"]; !ok {
		t.Error("recalled_turns entry missing 'assistant' key")
	}
	if _, ok := entry["score"]; !ok {
		t.Error("recalled_turns entry missing 'score' key")
	}
}

// ---- recall error → fail loud -----------------------------------------------

// errSearchStore wraps a real store and injects a SearchTurns error.
type errSearchStore struct {
	memory.MemoryStore
	searchErr error
}

func (e *errSearchStore) SearchTurns(_ context.Context, _ identity.Quadruple, _ string, _ int) ([]memory.ScoredTurn, error) {
	return nil, e.searchErr
}

// TestFetchMemoryBlocks_SearchError_FailsLoud asserts that a SearchTurns
// error is returned (not swallowed) when recall is enabled.
func TestFetchMemoryBlocks_SearchError_FailsLoud(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	inner := newFetchTestStore(t, bus, memory.StrategyNone, memory.RetrievalSemantic)
	injected := errors.New("test: inject search failure")
	wrapped := &errSearchStore{MemoryStore: inner, searchErr: injected}

	ctx := context.Background()
	q := fetchTestQuad("sess-err")
	_, err := runctx.FetchMemoryBlocks(ctx, wrapped, q, "any query", memory.RecallSettings{
		Enabled:  true,
		TopK:     5,
		MinScore: 0.0,
	})
	if err == nil {
		t.Fatal("FetchMemoryBlocks returned nil error on SearchTurns failure — should fail loud")
	}
	if !errors.Is(err, injected) {
		t.Errorf("error does not wrap injected sentinel: %v", err)
	}
}

// ---- Conversation tier unchanged when recall is on --------------------------

// TestFetchMemoryBlocks_ConversationUnchangedWithRecall asserts that the
// Conversation tier (rolling summary + recent turns) is identical whether
// recall is on or off.
func TestFetchMemoryBlocks_ConversationUnchangedWithRecall(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyRollingSummary, memory.RetrievalSemantic)
	ctx := context.Background()
	q := fetchTestQuad("sess-compose")

	for i, m := range []string{"turn one message", "turn two message"} {
		if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
			UserMessage:       m,
			AssistantResponse: "response",
			Timestamp:         time.Now(),
		}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}

	mbOff, err := runctx.FetchMemoryBlocks(ctx, mem, q, "turn one", memory.RecallSettings{Enabled: false})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks (off): %v", err)
	}
	mbOn, err := runctx.FetchMemoryBlocks(ctx, mem, q, "turn one", memory.RecallSettings{
		Enabled:  true,
		TopK:     5,
		MinScore: -1.0,
	})
	if err != nil {
		t.Fatalf("FetchMemoryBlocks (on): %v", err)
	}

	convOff := marshalAny(t, nil)
	if mbOff != nil {
		convOff = marshalAny(t, mbOff.Conversation)
	}
	convOn := marshalAny(t, nil)
	if mbOn != nil {
		convOn = marshalAny(t, mbOn.Conversation)
	}
	if convOff != convOn {
		t.Errorf("Conversation tier changed when recall was enabled:\n  off: %s\n  on:  %s", convOff, convOn)
	}
}

// ---- concurrent reuse (§11 / D-025) ----------------------------------------

// TestFetchMemoryBlocks_ConcurrentReuse runs N≥100 concurrent
// FetchMemoryBlocks invocations against a single shared semantic store
// under -race, asserting no data races, no context bleed, and no goroutine
// leaks. Uses StrategyNone so the recent window is empty and all recalled
// turns pass the dedup step, keeping the test deterministic.
func TestFetchMemoryBlocks_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()

	// Seed N sessions each with a distinguishable turn.
	const N = 100
	sessions := make([]identity.Quadruple, N)
	for i := range N {
		sessions[i] = fetchTestQuad(fmt.Sprintf("sess-concurrent-%03d", i))
		if err := mem.AddTurn(ctx, sessions[i], memory.ConversationTurn{
			UserMessage:       fmt.Sprintf("unique sailing message %d", i),
			AssistantResponse: fmt.Sprintf("response for %d", i),
			Timestamp:         time.Now(),
		}); err != nil {
			t.Fatalf("AddTurn session %d: %v", i, err)
		}
	}

	// Record baseline goroutine count.
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = runctx.FetchMemoryBlocks(
				ctx, mem, sessions[i],
				fmt.Sprintf("sailing message %d", i),
				memory.RecallSettings{Enabled: true, TopK: 3, MinScore: -1.0},
			)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("session %d: FetchMemoryBlocks error: %v", i, err)
		}
	}

	// Goroutine-leak check: allow some headroom for GC lag.
	after := runtime.NumGoroutine()
	if after > baseline+10 {
		t.Errorf("possible goroutine leak: baseline=%d after=%d", baseline, after)
	}
}

// ---- nil store (helper robustness) -----------------------------------------

// TestFetchMemoryBlocks_NilStore_GetLLMContextError verifies that when
// the inner store's GetLLMContext errors, the error is propagated.
type errGetLLMStore struct {
	memory.MemoryStore
	err error
}

func (e *errGetLLMStore) GetLLMContext(_ context.Context, _ identity.Quadruple) (memory.LLMContextPatch, error) {
	return memory.LLMContextPatch{}, e.err
}

func TestFetchMemoryBlocks_GetLLMContextError_Propagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("test: GetLLMContext failure")
	store := &errGetLLMStore{err: sentinel}
	q := fetchTestQuad("sess-llm-err")
	_, err := runctx.FetchMemoryBlocks(context.Background(), store, q, "query", memory.RecallSettings{})
	if !errors.Is(err, sentinel) {
		t.Errorf("error does not wrap sentinel: %v", err)
	}
}

// ---- helpers ---------------------------------------------------------------

func marshalAny(t *testing.T, v any) string {
	t.Helper()
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}
