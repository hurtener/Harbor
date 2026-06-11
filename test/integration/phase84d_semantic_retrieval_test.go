// Phase 84d (D-191) cross-subsystem integration test per CLAUDE.md §17.
//
// The Embedder seam's two in-wave consumers, exercised end-to-end on
// real drivers:
//
//	Embedder (deterministic test embedder at the PROVIDER edge —
//	the live bifrost leg is the HARBOR_LIVE_LLM probe in
//	internal/embeddings/drivers/bifrost) ──┐
//	                                       ├─▶ memory sqlite driver, retrieval=semantic
//	real audit redactor + events bus       │      (vectors persist through the SQLite
//	real state store (sqlite, durable) ────┤       StateStore — survive a reopen)
//	                                       └─▶ skills localdb driver, retrieval=semantic
//
// Asserts:
//
//   - Semantic memory retrieval composes with rolling_summary: a turn
//     is embedded at AddTurn and retrieved by similarity via
//     SearchTurns while GetLLMContext keeps its strategy patch.
//   - Vector persistence is durable: the memory store is closed, the
//     SQLite-backed StateStore reopened, and SearchTurns still ranks
//     the previously-embedded turns (§9 — vectors live in the
//     existing stores).
//   - Semantic skill retrieval: localdb Search ranks by similarity
//     with Path="semantic".
//   - Identity propagation + isolation: turns/skills embedded under
//     one (tenant, user, session) are never retrieved under another.
//   - Failure mode: enabling a semantic mode without an Embedder
//     fails loudly at construction (memory.Open + skills.Open).
//   - Concurrency stress: N≥10 concurrent sessions add + search on
//     the shared stores with no cross-talk and no goroutine leak.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	_ "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/skills"
	_ "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func phase84dQuad(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	}}
}

func phase84dBus(t *testing.T) events.EventBus {
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

func TestE2E_Phase84d_SemanticMemory_PersistsAndIsolates(t *testing.T) {
	bus := phase84dBus(t)
	dir := t.TempDir()
	stateDSN := filepath.Join(dir, "state.sqlite")
	memDSN := filepath.Join(dir, "memory.sqlite")

	openStores := func() (state.StateStore, memory.MemoryStore) {
		st, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: stateDSN})
		if err != nil {
			t.Fatalf("state.Open: %v", err)
		}
		mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
			Driver:       "sqlite",
			DSN:          memDSN,
			Strategy:     memory.StrategyRollingSummary,
			BudgetTokens: 512,
			Retrieval:    memory.RetrievalSemantic,
		}, memory.Deps{
			State:      st,
			Bus:        bus,
			Summarizer: strategy.EchoSummarizer{},
			Embedder:   embeddingstest.New(),
		})
		if err != nil {
			t.Fatalf("memory.Open: %v", err)
		}
		return st, mem
	}

	st, mem := openStores()
	ctx := context.Background()
	s1 := phase84dQuad("acme", "ana", "sess-1")
	s2 := phase84dQuad("acme", "ana", "sess-2")

	turns := []memory.ConversationTurn{
		{UserMessage: "how do I moor the sailboat at the harbor pier", AssistantResponse: "use the pier cleats and a bowline knot", Timestamp: time.Now()},
		{UserMessage: "what is the train schedule to the city", AssistantResponse: "trains leave every twenty minutes", Timestamp: time.Now()},
		{UserMessage: "recommend a chocolate cake recipe", AssistantResponse: "flour sugar cocoa eggs and an oven", Timestamp: time.Now()},
	}
	for i, turn := range turns {
		if err := mem.AddTurn(ctx, s1, turn); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}

	// Similarity retrieval, identity-scoped, composing with the
	// rolling_summary patch.
	got, err := mem.SearchTurns(ctx, s1, "mooring a boat at the pier", 1)
	if err != nil {
		t.Fatalf("SearchTurns: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Turn.UserMessage, "sailboat") {
		t.Fatalf("semantic retrieval missed the boat turn: %+v", got)
	}
	patch, err := mem.GetLLMContext(ctx, s1)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Strategy != memory.StrategyRollingSummary {
		t.Errorf("semantic mode replaced the strategy patch: %q", patch.Strategy)
	}

	// Cross-session isolation on the vector index.
	other, err := mem.SearchTurns(ctx, s2, "mooring a boat at the pier", 5)
	if err != nil {
		t.Fatalf("SearchTurns s2: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("session 2 retrieved %d turn(s) embedded under session 1", len(other))
	}

	// Durability: close everything, reopen against the same SQLite
	// files, and the vectors are still searchable (§9 — vectors
	// persist in the existing stores; never a single-driver feature).
	if err := mem.Close(ctx); err != nil {
		t.Fatalf("mem.Close: %v", err)
	}
	if err := st.Close(ctx); err != nil {
		t.Fatalf("state.Close: %v", err)
	}
	st2, mem2 := openStores()
	defer func() {
		_ = mem2.Close(ctx)
		_ = st2.Close(ctx)
	}()
	reopened, err := mem2.SearchTurns(ctx, s1, "mooring a boat at the pier", 1)
	if err != nil {
		t.Fatalf("SearchTurns after reopen: %v", err)
	}
	if len(reopened) != 1 || !strings.Contains(reopened[0].Turn.UserMessage, "sailboat") {
		t.Fatalf("vectors did not survive the reopen: %+v", reopened)
	}
}

func TestE2E_Phase84d_SemanticSkills_RankAndIsolate(t *testing.T) {
	bus := phase84dBus(t)
	store, err := skills.Open(context.Background(), skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       filepath.Join(t.TempDir(), "skills.sqlite"),
		Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: bus, Embedder: embeddingstest.New()})
	if err != nil {
		t.Fatalf("skills.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	ctx := context.Background()
	owner := phase84dQuad("acme", "ana", "sess-1")
	seed := func(id identity.Quadruple, name, trigger, desc string) {
		t.Helper()
		if err := store.Upsert(ctx, id, skills.Skill{
			Name: name, Title: name, Trigger: trigger, Description: desc,
			Steps: []string{"step"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
		}); err != nil {
			t.Fatalf("Upsert %s: %v", name, err)
		}
	}
	seed(owner, "moor-boat", "mooring a sailboat at the pier", "tie the sailboat to the harbor pier cleats")
	seed(owner, "bake-cake", "baking a chocolate cake", "mix flour sugar cocoa and bake")

	ranked, err := store.Search(ctx, owner, "how to moor a sailboat at the pier", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ranked) != 1 || ranked[0].Skill.Name != "moor-boat" {
		t.Fatalf("semantic skill ranking missed: %+v", ranked)
	}
	if ranked[0].Path != skills.PathSemantic {
		t.Errorf("Path=%q, want %q", ranked[0].Path, skills.PathSemantic)
	}

	// Identity isolation.
	stranger := phase84dQuad("other-tenant", "ana", "sess-1")
	leak, err := store.Search(ctx, stranger, "how to moor a sailboat at the pier", 5)
	if err != nil {
		t.Fatalf("Search stranger: %v", err)
	}
	if len(leak) != 0 {
		t.Fatalf("cross-tenant semantic search returned %d row(s)", len(leak))
	}
}

// TestE2E_Phase84d_FailureMode_SemanticWithoutEmbedder pins the §17.3
// failure-mode leg: both consumers fail loudly at construction when a
// semantic mode is enabled without an Embedder.
func TestE2E_Phase84d_FailureMode_SemanticWithoutEmbedder(t *testing.T) {
	bus := phase84dBus(t)
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "state.sqlite")})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = st.Close(context.Background()) }()

	_, err = memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:    "sqlite",
		DSN:       filepath.Join(t.TempDir(), "memory.sqlite"),
		Retrieval: memory.RetrievalSemantic,
	}, memory.Deps{State: st, Bus: bus})
	if err == nil || !strings.Contains(err.Error(), "Deps.Embedder") {
		t.Fatalf("memory.Open err=%v, want the fail-loud Deps.Embedder guard", err)
	}

	_, err = skills.Open(context.Background(), skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       filepath.Join(t.TempDir(), "skills.sqlite"),
		Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: bus})
	if err == nil || !strings.Contains(err.Error(), "Deps.Embedder") {
		t.Fatalf("skills.Open err=%v, want the fail-loud Deps.Embedder guard", err)
	}

	// And SearchTurns on a non-semantic store fails loudly — never an
	// empty success.
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "memory.sqlite"),
	}, memory.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open (default mode): %v", err)
	}
	defer func() { _ = mem.Close(context.Background()) }()
	_, err = mem.SearchTurns(context.Background(), phase84dQuad("t", "u", "s"), "anything", 3)
	if !errors.Is(err, memory.ErrSemanticDisabled) {
		t.Fatalf("SearchTurns err=%v, want ErrSemanticDisabled", err)
	}
}

// TestE2E_Phase84d_ConcurrentSessions_NoCrossTalk is the §17.3
// concurrency-stress leg: N concurrent sessions add + search against
// ONE shared memory store; each session retrieves only its own turn;
// goroutine baseline restored after teardown.
func TestE2E_Phase84d_ConcurrentSessions_NoCrossTalk(t *testing.T) {
	bus := phase84dBus(t)
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "state.sqlite")})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:    "sqlite",
		DSN:       filepath.Join(t.TempDir(), "memory.sqlite"),
		Strategy:  memory.StrategyTruncation,
		Retrieval: memory.RetrievalSemantic,
	}, memory.Deps{State: st, Bus: bus, Embedder: embeddingstest.New()})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}

	baseline := runtime.NumGoroutine()
	const sessions = 16
	var wg sync.WaitGroup
	errCh := make(chan error, sessions)
	wg.Add(sessions)
	for i := range sessions {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			q := phase84dQuad("acme", "ana", fmt.Sprintf("sess-%d", i))
			marker := fmt.Sprintf("topic-%d unique payload for session %d", i, i)
			if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
				UserMessage: marker, AssistantResponse: "noted",
			}); err != nil {
				errCh <- fmt.Errorf("session %d AddTurn: %w", i, err)
				return
			}
			got, err := mem.SearchTurns(ctx, q, marker, 5)
			if err != nil {
				errCh <- fmt.Errorf("session %d SearchTurns: %w", i, err)
				return
			}
			if len(got) != 1 || !strings.Contains(got[0].Turn.UserMessage, fmt.Sprintf("session %d", i)) {
				errCh <- fmt.Errorf("session %d cross-talk: %+v", i, got)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	if err := mem.Close(context.Background()); err != nil {
		t.Fatalf("mem.Close: %v", err)
	}
	if err := st.Close(context.Background()); err != nil {
		t.Fatalf("state.Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
