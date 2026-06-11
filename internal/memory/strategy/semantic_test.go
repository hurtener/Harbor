package strategy_test

// Phase 84d (D-191) — in-package coverage for the semantic retrieval
// wrapper executor: construction guards, embed-failure fail-loud
// paths, ranking + bounds, vector flush, snapshot/restore
// delegation, and the dimension-mismatch (model drift) error. The
// cross-driver behaviour is owned by the memory conformance suite;
// this file pins the executor-level edges the suite reaches only
// indirectly.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/embeddings/embeddingstest"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/strategy"
)

func semanticDeps(t *testing.T, emb memory.Embedder, topK int) strategy.Deps {
	t.Helper()
	deps := buildDeps(t, nil)
	deps.Embedder = emb
	deps.Retrieval = memory.RetrievalSemantic
	deps.RetrievalTopK = topK
	return deps
}

func TestNew_Semantic_GuardsConstruction(t *testing.T) {
	t.Run("missing_embedder", func(t *testing.T) {
		deps := buildDeps(t, nil)
		deps.Retrieval = memory.RetrievalSemantic
		_, err := strategy.New(memory.StrategyNone, deps)
		if err == nil || !strings.Contains(err.Error(), "Deps.Embedder") {
			t.Fatalf("err=%v, want the fail-loud Deps.Embedder guard", err)
		}
	})
	t.Run("unknown_mode", func(t *testing.T) {
		deps := buildDeps(t, nil)
		deps.Retrieval = memory.RetrievalMode("vibes")
		deps.Embedder = embeddingstest.New()
		_, err := strategy.New(memory.StrategyNone, deps)
		if err == nil || !strings.Contains(err.Error(), "unknown retrieval mode") {
			t.Fatalf("err=%v, want unknown-retrieval-mode rejection", err)
		}
	})
	t.Run("negative_topk", func(t *testing.T) {
		deps := buildDeps(t, nil)
		deps.RetrievalTopK = -1
		_, err := strategy.New(memory.StrategyNone, deps)
		if err == nil || !strings.Contains(err.Error(), "RetrievalTopK") {
			t.Fatalf("err=%v, want RetrievalTopK rejection", err)
		}
	})
}

func TestSemantic_SearchTurns_RanksDefaultsAndBounds(t *testing.T) {
	exec, err := strategy.New(memory.StrategyTruncation, semanticDeps(t, embeddingstest.New(), 0))
	if err != nil {
		t.Fatalf("strategy.New: %v", err)
	}
	defer func() { _ = exec.Close(context.Background()) }()
	ctx := context.Background()
	id := tripleA()

	topics := []string{
		"docking the sailboat at the harbor pier",
		"the train schedule into the city",
		"a chocolate cake recipe with cocoa",
		"watering succulents on the balcony",
		"filing the quarterly tax forms",
		"replacing a bicycle inner tube",
	}
	for _, topic := range topics {
		if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: topic, AssistantResponse: "noted " + topic}); err != nil {
			t.Fatalf("AddTurn(%q): %v", topic, err)
		}
	}

	// limit<=0 falls back to the default top-K (5) — one fewer than
	// the corpus, proving the bound applies.
	got, err := exec.SearchTurns(ctx, id, "sailboat pier docking", 0)
	if err != nil {
		t.Fatalf("SearchTurns: %v", err)
	}
	if len(got) != memory.DefaultSemanticTopK {
		t.Fatalf("got %d results, want the DefaultSemanticTopK bound (%d)", len(got), memory.DefaultSemanticTopK)
	}
	if !strings.Contains(got[0].Turn.UserMessage, "sailboat") {
		t.Errorf("top result %q is not the sailboat turn", got[0].Turn.UserMessage)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Errorf("results not sorted at %d: %v < %v", i, got[i-1].Score, got[i].Score)
		}
	}

	// Empty query fails loudly.
	if _, err := exec.SearchTurns(ctx, id, "", 3); !errors.Is(err, embeddings.ErrEmptyInput) {
		t.Errorf("empty query: err=%v, want ErrEmptyInput", err)
	}

	// Flush drops the index.
	if err := exec.Flush(ctx, id); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got, err = exec.SearchTurns(ctx, id, "sailboat pier docking", 3)
	if err != nil {
		t.Fatalf("SearchTurns after Flush: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Flush left %d vector entries", len(got))
	}
}

func TestSemantic_DelegatesStrategySurface(t *testing.T) {
	exec, err := strategy.New(memory.StrategyTruncation, semanticDeps(t, embeddingstest.New(), 2))
	if err != nil {
		t.Fatalf("strategy.New: %v", err)
	}
	defer func() { _ = exec.Close(context.Background()) }()
	ctx := context.Background()
	id := tripleA()

	if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "hello there", AssistantResponse: "hi"}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Strategy != memory.StrategyTruncation || len(patch.RecentTurns) != 1 {
		t.Errorf("strategy patch not delegated: %+v", patch)
	}
	if _, err := exec.EstimateTokens(ctx, id); err != nil {
		t.Errorf("EstimateTokens: %v", err)
	}
	if h, err := exec.Health(ctx, id); err != nil || h != memory.HealthHealthy {
		t.Errorf("Health=(%v,%v)", h, err)
	}
	snap, err := exec.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Strategy != memory.StrategyTruncation {
		t.Errorf("Snapshot.Strategy=%q", snap.Strategy)
	}
	if err := exec.Restore(ctx, id, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
}

func TestSemantic_EmbedFailure_FailsLoudly_BeforeStateMutates(t *testing.T) {
	emb := embeddingstest.New()
	exec, err := strategy.New(memory.StrategyTruncation, semanticDeps(t, emb, 2))
	if err != nil {
		t.Fatalf("strategy.New: %v", err)
	}
	defer func() { _ = exec.Close(context.Background()) }()
	ctx := context.Background()
	id := tripleA()

	if err := emb.Close(ctx); err != nil {
		t.Fatalf("embedder Close: %v", err)
	}
	// AddTurn fails before the inner strategy mutates — the recent
	// window stays empty.
	err = exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "doomed", AssistantResponse: "doomed"})
	if !errors.Is(err, embeddings.ErrClientClosed) {
		t.Fatalf("AddTurn err=%v, want wrapped ErrClientClosed", err)
	}
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if len(patch.RecentTurns) != 0 {
		t.Errorf("inner strategy mutated despite the embed failure: %+v", patch.RecentTurns)
	}
	// SearchTurns surfaces the failure too — never silent.
	if _, err := exec.SearchTurns(ctx, id, "anything", 3); !errors.Is(err, embeddings.ErrClientClosed) {
		t.Errorf("SearchTurns err=%v, want wrapped ErrClientClosed", err)
	}
}

// driftEmbedder returns vectors whose dimension depends on the call
// count — simulating an embedding-model change between indexing and
// querying.
type driftEmbedder struct {
	dims []int
	call int
}

func (d *driftEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	dim := d.dims[d.call%len(d.dims)]
	d.call++
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

func TestSemantic_DimensionMismatch_FailsLoudly(t *testing.T) {
	exec, err := strategy.New(memory.StrategyNone, semanticDeps(t, &driftEmbedder{dims: []int{8, 16}}, 2))
	if err != nil {
		t.Fatalf("strategy.New: %v", err)
	}
	defer func() { _ = exec.Close(context.Background()) }()
	ctx := context.Background()
	id := tripleA()

	if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "indexed under dim 8"}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	_, err = exec.SearchTurns(ctx, id, "query under dim 16", 3)
	if !errors.Is(err, embeddings.ErrDimensionMismatch) {
		t.Fatalf("err=%v, want ErrDimensionMismatch (model drift must fail loudly)", err)
	}
	if !strings.Contains(err.Error(), "re-embed") {
		t.Errorf("error %q should name re-embedding as the migration", err.Error())
	}
}

func TestSemantic_AfterClose_OperationsError(t *testing.T) {
	exec, err := strategy.New(memory.StrategyNone, semanticDeps(t, embeddingstest.New(), 2))
	if err != nil {
		t.Fatalf("strategy.New: %v", err)
	}
	if err := exec.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := exec.AddTurn(context.Background(), tripleA(), memory.ConversationTurn{UserMessage: "x"}); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("AddTurn after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.SearchTurns(context.Background(), tripleA(), "x", 1); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("SearchTurns after Close: err=%v, want ErrStoreClosed", err)
	}
}
