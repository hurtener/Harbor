// Semantic recall cross-subsystem integration test per CLAUDE.md §17.
//
// FetchMemoryBlocks is the single run-loop home for the memory step.
// This file proves the cross-package seam — real drivers assembled end-
// to-end — is wired correctly:
//
//   - A turn added via AddTurn is recalled via FetchMemoryBlocks when
//     the query is semantically similar (External tier populated).
//   - A session's turns never appear in another session's External tier
//     (identity isolation, §6).
//   - A SearchTurns error propagates out of FetchMemoryBlocks loudly —
//     no silent fallback to rolling-summary-only.
//   - N≥10 concurrent FetchMemoryBlocks calls against one shared store
//     produce no cross-talk and no goroutine leak (D-025 concurrent-
//     reuse gate at the integration boundary).
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3): real patterns
// audit redactor, real inmem events bus, real inmem state store, real
// inmem memory store with semantic retrieval, deterministic test embedder
// (per D-191, embeddingstest is the only legitimate provider at test
// scope — never registered, never a default).
package integration_test

import (
	"context"
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
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// phase84eQuad builds a test identity quadruple (tenant is always "acme").
func phase84eQuad(user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: "acme", UserID: user, SessionID: session,
	}}
}

// phase84eStores opens a full real-driver stack for the recall seam:
// patterns audit + inmem events + inmem state + inmem memory with
// semantic retrieval enabled.
type phase84eStores struct {
	bus    events.EventBus
	state  state.StateStore
	memory memory.MemoryStore
}

func openPhase84eStores(t *testing.T, strategy memory.Strategy, retrieval memory.RetrievalMode) *phase84eStores {
	t.Helper()
	ctx := context.Background()

	red, err := audit.Open(ctx, config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		ReplayBufferSize:         16,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	st, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	deps := memory.Deps{State: st, Bus: bus}
	if retrieval == memory.RetrievalSemantic {
		deps.Embedder = embeddingstest.New()
	}
	snap := memory.ConfigSnapshot{
		Driver:    "inmem",
		Strategy:  strategy,
		Retrieval: retrieval,
	}
	if strategy == memory.StrategyRollingSummary {
		snap.BudgetTokens = 4096
	}
	mem, err := memory.Open(ctx, snap, deps)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}

	t.Cleanup(func() {
		_ = mem.Close(context.Background())
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return &phase84eStores{bus: bus, state: st, memory: mem}
}

// recall on, MinScore floor low — recalled turn lands in External.
func TestE2E_Phase84e_EarlyTurnRecalled_ExternalTierPopulated(t *testing.T) {
	stores := openPhase84eStores(t, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()
	q := phase84eQuad("bob", "sess-recall")

	// Seed a turn whose content is semantically related to the query
	// we will pass to FetchMemoryBlocks.
	if err := stores.memory.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "how do I moor the sailboat at the harbor pier",
		AssistantResponse: "use the pier cleats and a bowline knot",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	recall := memory.RecallSettings{
		Enabled:  true,
		TopK:     5,
		MinScore: -1.0, // accept everything the embedder scores
	}
	mb, err := runctx.FetchMemoryBlocks(ctx, stores.memory, q, "mooring a boat at the dock", recall, nil)
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	if mb == nil {
		t.Fatal("FetchMemoryBlocks returned nil MemoryBlocks")
	}
	if mb.External == nil {
		t.Fatal("External tier is nil — FetchMemoryBlocks did not populate recalled turns")
	}
	ext, ok := mb.External.(map[string]any)
	if !ok {
		t.Fatalf("External is %T, want map[string]any", mb.External)
	}
	turns, ok := ext["recalled_turns"]
	if !ok {
		t.Fatal("External has no 'recalled_turns' key")
	}
	slice, ok := turns.([]map[string]any)
	if !ok {
		t.Fatalf("recalled_turns is %T, want []map[string]any", turns)
	}
	if len(slice) == 0 {
		t.Fatal("recalled_turns is empty — expected at least one recalled turn")
	}
	// The recalled turn must carry the user message text.
	first, _ := slice[0]["user"].(string)
	if !strings.Contains(first, "sailboat") {
		t.Errorf("recalled turn user=%q, want to contain 'sailboat'", first)
	}
}

// recall off — External tier must remain nil; SearchTurns never called.
func TestE2E_Phase84e_RecallOff_ExternalTierNil(t *testing.T) {
	// Non-semantic store — SearchTurns would fail loud with
	// ErrSemanticDisabled if FetchMemoryBlocks incorrectly called it.
	stores := openPhase84eStores(t, memory.StrategyTruncation, memory.RetrievalMode(""))
	ctx := context.Background()
	q := phase84eQuad("carol", "sess-off")

	if err := stores.memory.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "some question",
		AssistantResponse: "some answer",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	mb, err := runctx.FetchMemoryBlocks(ctx, stores.memory, q, "some question", memory.RecallSettings{Enabled: false}, nil)
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	if mb != nil && mb.External != nil {
		t.Errorf("External tier is non-nil with recall disabled: %+v", mb.External)
	}
}

// MinScore floor — turns below the floor are dropped; External stays nil.
func TestE2E_Phase84e_MinScoreFloor_DropsLowScoringTurns(t *testing.T) {
	stores := openPhase84eStores(t, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()
	q := phase84eQuad("dave", "sess-floor")

	if err := stores.memory.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "completely unrelated topic about cooking pasta",
		AssistantResponse: "boil the pasta for 10 minutes",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	// MinScore = 2.0 is above the cosine similarity ceiling of 1.0;
	// every turn will be filtered out.
	recall := memory.RecallSettings{Enabled: true, TopK: 5, MinScore: 2.0}
	mb, err := runctx.FetchMemoryBlocks(ctx, stores.memory, q, "quantum physics", recall, nil)
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	if mb != nil && mb.External != nil {
		t.Errorf("External should be nil when all turns are below MinScore; got %+v", mb.External)
	}
}

// Cross-session isolation: turns from session A must never appear in
// session B's External tier.
func TestE2E_Phase84e_CrossSession_Isolation(t *testing.T) {
	stores := openPhase84eStores(t, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()
	sessA := phase84eQuad("eve", "sess-a")
	sessB := phase84eQuad("eve", "sess-b")

	// Add a distinctive turn under sessA.
	if err := stores.memory.AddTurn(ctx, sessA, memory.ConversationTurn{
		UserMessage:       "the secret harbor password is xylophone42",
		AssistantResponse: "noted, the password is stored",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn sessA: %v", err)
	}

	recall := memory.RecallSettings{Enabled: true, TopK: 5, MinScore: -1.0}
	// FetchMemoryBlocks for sessB must not see sessA's turn.
	mb, err := runctx.FetchMemoryBlocks(ctx, stores.memory, sessB, "harbor password", recall, nil)
	if err != nil {
		t.Fatalf("FetchMemoryBlocks sessB: %v", err)
	}
	if mb == nil || mb.External == nil {
		return // good: nothing leaked
	}
	ext, ok := mb.External.(map[string]any)
	if !ok {
		return
	}
	raw := ext["recalled_turns"]
	if raw == nil {
		return
	}
	slice, ok := raw.([]map[string]any)
	if !ok || len(slice) == 0 {
		return
	}
	for _, entry := range slice {
		user, _ := entry["user"].(string)
		if strings.Contains(user, "xylophone42") {
			t.Fatalf("cross-session data leak: sessB External contains sessA's turn: %q", user)
		}
	}
}

// SearchTurns error propagates out of FetchMemoryBlocks loudly.
func TestE2E_Phase84e_FailLoud_SearchTurnsError(t *testing.T) {
	stores := openPhase84eStores(t, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()
	q := phase84eQuad("frank", "sess-err")

	injectedErr := errors.New("sentinel: search-turns-unavailable")
	failing := &phase84eFailSearchStore{inner: stores.memory, err: injectedErr}

	recall := memory.RecallSettings{Enabled: true, TopK: 5, MinScore: -1.0}
	_, err := runctx.FetchMemoryBlocks(ctx, failing, q, "any query", recall, nil)
	if err == nil {
		t.Fatal("FetchMemoryBlocks succeeded; expected a propagated SearchTurns error")
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("FetchMemoryBlocks err=%v, want errors.Is to find %v", err, injectedErr)
	}
}

// phase84eFailSearchStore wraps a MemoryStore and injects an error on
// SearchTurns while passing everything else through.
type phase84eFailSearchStore struct {
	inner memory.MemoryStore
	err   error
}

func (s *phase84eFailSearchStore) AddTurn(ctx context.Context, id identity.Quadruple, t memory.ConversationTurn) error {
	return s.inner.AddTurn(ctx, id, t)
}

func (s *phase84eFailSearchStore) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	return s.inner.GetLLMContext(ctx, id)
}

func (s *phase84eFailSearchStore) SearchTurns(ctx context.Context, id identity.Quadruple, query string, topK int) ([]memory.ScoredTurn, error) {
	return nil, s.err
}

func (s *phase84eFailSearchStore) Flush(ctx context.Context, id identity.Quadruple) error {
	return s.inner.Flush(ctx, id)
}

func (s *phase84eFailSearchStore) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	return s.inner.EstimateTokens(ctx, id)
}

func (s *phase84eFailSearchStore) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	return s.inner.Health(ctx, id)
}

func (s *phase84eFailSearchStore) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	return s.inner.Snapshot(ctx, id)
}

func (s *phase84eFailSearchStore) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	return s.inner.Restore(ctx, id, snap)
}

func (s *phase84eFailSearchStore) Close(ctx context.Context) error {
	return s.inner.Close(ctx)
}

// N≥10 concurrent FetchMemoryBlocks calls against a shared store must
// produce no cross-talk and restore the goroutine baseline (D-025
// concurrent-reuse contract at the integration boundary).
func TestE2E_Phase84e_ConcurrentSessions_NoCrossTalk(t *testing.T) {
	stores := openPhase84eStores(t, memory.StrategyNone, memory.RetrievalSemantic)
	ctx := context.Background()

	const n = 16
	baseline := runtime.NumGoroutine()

	// Seed a distinct turn per session.
	for i := range n {
		q := phase84eQuad("grace", fmt.Sprintf("sess-%d", i))
		marker := fmt.Sprintf("unique session %d topic about harbor mooring", i)
		if err := stores.memory.AddTurn(ctx, q, memory.ConversationTurn{
			UserMessage:       marker,
			AssistantResponse: fmt.Sprintf("answer for session %d", i),
			Timestamp:         time.Now(),
		}); err != nil {
			t.Fatalf("AddTurn sess-%d: %v", i, err)
		}
	}

	recall := memory.RecallSettings{Enabled: true, TopK: 5, MinScore: -1.0}
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			q := phase84eQuad("grace", fmt.Sprintf("sess-%d", i))
			query := fmt.Sprintf("harbor mooring session %d", i)
			mb, fetchErr := runctx.FetchMemoryBlocks(ctx, stores.memory, q, query, recall, nil)
			if fetchErr != nil {
				errCh <- fmt.Errorf("sess-%d FetchMemoryBlocks: %w", i, fetchErr)
				return
			}
			if mb == nil || mb.External == nil {
				return // session may have scored too low; not a leak
			}
			ext, ok := mb.External.(map[string]any)
			if !ok {
				return
			}
			raw := ext["recalled_turns"]
			if raw == nil {
				return
			}
			slice, _ := raw.([]map[string]any)
			for _, entry := range slice {
				user, _ := entry["user"].(string)
				// Verify no cross-session turns (other sessions' markers).
				for j := range n {
					if j == i {
						continue
					}
					alien := fmt.Sprintf("unique session %d topic", j)
					if strings.Contains(user, alien) {
						errCh <- fmt.Errorf("sess-%d External contains sess-%d data: %q", i, j, user)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	// Goroutine baseline must restore within a reasonable window.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
