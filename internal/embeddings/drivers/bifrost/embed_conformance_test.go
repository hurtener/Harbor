package bifrost_test

// Phase 84d (D-191) — the HARBOR_LIVE_LLM-gated embedding
// conformance probe, mirroring the chat driver's live-conformance
// pattern: skipped by default (burns API credits), runs against one
// capable provider when the operator opts in.
//
//	HARBOR_LIVE_LLM=1 OPENAI_API_KEY=... go test -race \
//	    -run TestE2E_BifrostEmbed_LiveConformance ./internal/embeddings/drivers/bifrost/
//
// Asserts: non-empty vectors, one per input, of a consistent
// dimension; the reduced-dimensions knob honoured; cosine of a text
// with itself ≈ 1.

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/hurtener/Harbor/internal/embeddings"
	_ "github.com/hurtener/Harbor/internal/embeddings/drivers/bifrost"
	"github.com/hurtener/Harbor/internal/identity"
)

func TestE2E_BifrostEmbed_LiveConformance(t *testing.T) {
	if os.Getenv("HARBOR_LIVE_LLM") != "1" {
		t.Skip("set HARBOR_LIVE_LLM=1 to run the live embedding conformance (this test burns API credits)")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY is not set — the live embedding probe requires an OpenAI key")
	}

	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "live-conformance", UserID: "harbor", SessionID: "embed-probe",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	emb, err := embeddings.Open(ctx, embeddings.ConfigSnapshot{
		Driver:   "bifrost",
		Provider: "openai",
		Model:    "text-embedding-3-small",
		APIKey:   "env.OPENAI_API_KEY",
	}, embeddings.Deps{})
	if err != nil {
		t.Fatalf("embeddings.Open: %v", err)
	}
	defer func() { _ = emb.Close(context.Background()) }()

	texts := []string{
		"a sailboat moored at the harbor pier",
		"the capital city of France",
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(vecs), len(texts))
	}
	dim := len(vecs[0])
	if dim == 0 {
		t.Fatal("empty vector")
	}
	for i, v := range vecs {
		if len(v) != dim {
			t.Errorf("vector %d dimension %d != %d", i, len(v), dim)
		}
	}
	self, err := embeddings.Cosine(vecs[0], vecs[0])
	if err != nil || math.Abs(self-1) > 1e-3 {
		t.Errorf("self-cosine = %v (err=%v), want ≈ 1", self, err)
	}
	cross, err := embeddings.Cosine(vecs[0], vecs[1])
	if err != nil {
		t.Fatalf("Cosine: %v", err)
	}
	if cross >= self {
		t.Errorf("cross-cosine %v >= self-cosine %v — embeddings carry no signal", cross, self)
	}

	// The reduced-dimensions knob.
	embDim, err := embeddings.Open(ctx, embeddings.ConfigSnapshot{
		Driver:     "bifrost",
		Provider:   "openai",
		Model:      "text-embedding-3-small",
		APIKey:     "env.OPENAI_API_KEY",
		Dimensions: 256,
	}, embeddings.Deps{})
	if err != nil {
		t.Fatalf("embeddings.Open (dimensions): %v", err)
	}
	defer func() { _ = embDim.Close(context.Background()) }()
	small, err := embDim.Embed(ctx, []string{texts[0]})
	if err != nil {
		t.Fatalf("Embed (dimensions): %v", err)
	}
	if len(small[0]) != 256 {
		t.Errorf("reduced dimension = %d, want 256", len(small[0]))
	}
}
