package memory

// Phase 84d (D-191) — registry-boundary tests for the semantic
// retrieval mode: the fail-loud Deps.Embedder guard (mirroring the
// Deps.Summarizer rule) and unknown-mode rejection.

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// stubState / stubBus satisfy the Deps validation without touching
// real drivers — the guard under test fires BEFORE any factory runs.
type stubState struct{ state.StateStore }

type stubBus struct{ events.EventBus }

// stubEmbedder is a never-called placeholder for the unknown-mode
// rejection case.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1}
	}
	return out, nil
}

func TestOpen_Semantic_RequiresEmbedder(t *testing.T) {
	_, err := Open(context.Background(), ConfigSnapshot{
		Driver:    "inmem",
		Retrieval: RetrievalSemantic,
	}, Deps{State: stubState{}, Bus: stubBus{}})
	if err == nil {
		t.Fatal("err=nil, want the registry fail-loud guard")
	}
	if !strings.Contains(err.Error(), "Deps.Embedder") || !strings.Contains(err.Error(), "no stub fallback") {
		t.Errorf("guard error mismatch: %q", err.Error())
	}
}

func TestOpen_UnknownRetrievalMode_Rejected(t *testing.T) {
	_, err := Open(context.Background(), ConfigSnapshot{
		Driver:    "inmem",
		Retrieval: RetrievalMode("vibes"),
	}, Deps{State: stubState{}, Bus: stubBus{}, Embedder: stubEmbedder{}})
	if err == nil || !strings.Contains(err.Error(), "unknown retrieval mode") {
		t.Fatalf("err=%v, want unknown-retrieval-mode rejection", err)
	}
}

func TestOpen_NegativeRetrievalTopK_Rejected(t *testing.T) {
	_, err := Open(context.Background(), ConfigSnapshot{
		Driver:        "inmem",
		Retrieval:     RetrievalSemantic,
		RetrievalTopK: -1,
	}, Deps{State: stubState{}, Bus: stubBus{}, Embedder: stubEmbedder{}})
	if err == nil || !strings.Contains(err.Error(), "RetrievalTopK") {
		t.Fatalf("err=%v, want RetrievalTopK rejection", err)
	}
}

// Compile-time check: the package's Embedder interface stays
// satisfied by the single-method shape (any embeddings.Embedder
// implementation also satisfies it structurally).
var _ Embedder = stubEmbedder{}

// Identity quadruple sanity for the new types (keeps the
// vocabulary's zero values honest).
func TestScoredTurn_ZeroValue(t *testing.T) {
	var st ScoredTurn
	if st.Score != 0 || st.Turn.UserMessage != "" {
		t.Errorf("unexpected zero value: %+v", st)
	}
	var q identity.Quadruple
	if ValidateIdentity(q) == nil {
		t.Error("empty quadruple validated")
	}
}
