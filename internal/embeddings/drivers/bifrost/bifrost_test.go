package bifrost

// Phase 84d (D-191) — driver tests against a stubbed gateway client:
// request translation, response ordering, identity fail-closed,
// closed-state, and error translation. The live-provider probe lives
// in embed_conformance_test.go (HARBOR_LIVE_LLM-gated).

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/identity"
)

// stubClient implements embeddingClient deterministically.
type stubClient struct {
	calls   atomic.Int64
	lastReq atomic.Pointer[bfschemas.BifrostEmbeddingRequest]
	respond func(req *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError)
}

func (s *stubClient) EmbeddingRequest(_ *bfschemas.BifrostContext, req *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
	s.calls.Add(1)
	s.lastReq.Store(req)
	return s.respond(req)
}

func newStubDriver(respond func(req *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError)) (*Driver, *stubClient) {
	stub := &stubClient{respond: respond}
	return &Driver{
		client:   stub,
		provider: bfschemas.ModelProvider("openai"),
		model:    "text-embedding-3-small",
	}, stub
}

func okResponse(req *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
	n := len(req.Input.Texts)
	data := make([]bfschemas.EmbeddingData, n)
	for i := range n {
		// Reverse order deliberately — the driver must sort by Index.
		data[n-1-i] = bfschemas.EmbeddingData{
			Index:     i,
			Object:    "embedding",
			Embedding: bfschemas.EmbeddingStruct{EmbeddingArray: []float64{float64(i), 1, 2}},
		}
	}
	return &bfschemas.BifrostEmbeddingResponse{Data: data, Model: req.Model, Object: "list"}, nil
}

func identityCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestEmbed_TranslatesRequestAndOrdersByIndex(t *testing.T) {
	d, stub := newStubDriver(okResponse)
	vecs, err := d.Embed(identityCtx(t), []string{"alpha", "beta", "gamma"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	for i, v := range vecs {
		if float64(v[0]) != float64(i) {
			t.Errorf("vector %d not reordered by Index: leading=%v want %d", i, v[0], i)
		}
	}
	req := stub.lastReq.Load()
	if req.Model != "text-embedding-3-small" {
		t.Errorf("req.Model=%q", req.Model)
	}
	if req.Input == nil || len(req.Input.Texts) != 3 {
		t.Errorf("req.Input did not carry the batched texts: %+v", req.Input)
	}
	if req.Params != nil {
		t.Errorf("req.Params set without Dimensions config: %+v", req.Params)
	}
}

func TestEmbed_SetsDimensionsParam(t *testing.T) {
	d, stub := newStubDriver(okResponse)
	d.dimensions = 256
	if _, err := d.Embed(identityCtx(t), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	req := stub.lastReq.Load()
	if req.Params == nil || req.Params.Dimensions == nil || *req.Params.Dimensions != 256 {
		t.Errorf("Dimensions param not forwarded: %+v", req.Params)
	}
}

func TestEmbed_FailsClosedOnMissingIdentity(t *testing.T) {
	d, stub := newStubDriver(okResponse)
	_, err := d.Embed(context.Background(), []string{"a"})
	if !errors.Is(err, embeddings.ErrIdentityMissing) {
		t.Fatalf("err=%v, want ErrIdentityMissing", err)
	}
	if stub.calls.Load() != 0 {
		t.Error("gateway was reached despite missing identity")
	}
}

func TestEmbed_AfterClose_Errors(t *testing.T) {
	d, _ := newStubDriver(okResponse)
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close (idempotent): %v", err)
	}
	if _, err := d.Embed(identityCtx(t), []string{"a"}); !errors.Is(err, embeddings.ErrClientClosed) {
		t.Fatalf("err=%v, want ErrClientClosed", err)
	}
}

func TestEmbed_TranslatesGatewayError(t *testing.T) {
	status := 429
	d, _ := newStubDriver(func(_ *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
		return nil, &bfschemas.BifrostError{
			StatusCode: &status,
			Error:      &bfschemas.ErrorField{Message: "rate limited"},
		}
	})
	_, err := d.Embed(identityCtx(t), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "status 429") || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err=%v, want a translated status-429 error", err)
	}
}

func TestEmbed_RejectsMalformedResponses(t *testing.T) {
	cases := []struct {
		name    string
		resp    *bfschemas.BifrostEmbeddingResponse
		wantSub string
	}{
		{"nil", nil, "no embedding rows"},
		{"empty", &bfschemas.BifrostEmbeddingResponse{}, "no embedding rows"},
		{"row_count", &bfschemas.BifrostEmbeddingResponse{Data: []bfschemas.EmbeddingData{
			{Index: 0, Embedding: bfschemas.EmbeddingStruct{EmbeddingArray: []float64{1}}},
			{Index: 1, Embedding: bfschemas.EmbeddingStruct{EmbeddingArray: []float64{1}}},
		}}, "2 rows for 1 inputs"},
		{"non_float_encoding", &bfschemas.BifrostEmbeddingResponse{Data: []bfschemas.EmbeddingData{
			{Index: 0, Embedding: bfschemas.EmbeddingStruct{EmbeddingStr: ptr("b64==")}},
		}}, "unsupported encoding"},
		{"out_of_range_index", &bfschemas.BifrostEmbeddingResponse{Data: []bfschemas.EmbeddingData{
			{Index: 5, Embedding: bfschemas.EmbeddingStruct{EmbeddingArray: []float64{1}}},
		}}, "out-of-range index"},
		{"empty_vector", &bfschemas.BifrostEmbeddingResponse{Data: []bfschemas.EmbeddingData{
			{Index: 0, Embedding: bfschemas.EmbeddingStruct{EmbeddingArray: []float64{}}},
		}}, "empty embedding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newStubDriver(func(_ *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
				return tc.resp, nil
			})
			_, err := d.Embed(identityCtx(t), []string{"a"})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err=%v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestNew_FailsClosedOnMisconfiguration(t *testing.T) {
	t.Run("missing_model", func(t *testing.T) {
		_, err := New(embeddings.ConfigSnapshot{Provider: "openai", APIKey: "k"}, embeddings.Deps{})
		if !errors.Is(err, ErrMissingModel) {
			t.Fatalf("err=%v, want ErrMissingModel", err)
		}
	})
	t.Run("missing_provider", func(t *testing.T) {
		_, err := New(embeddings.ConfigSnapshot{Model: "m", APIKey: "k"}, embeddings.Deps{})
		if !errors.Is(err, ErrInvalidProvider) {
			t.Fatalf("err=%v, want ErrInvalidProvider", err)
		}
	})
	t.Run("unknown_provider", func(t *testing.T) {
		_, err := New(embeddings.ConfigSnapshot{Provider: "not-a-provider", Model: "m", APIKey: "k"}, embeddings.Deps{})
		if !errors.Is(err, ErrInvalidProvider) {
			t.Fatalf("err=%v, want ErrInvalidProvider", err)
		}
		if !strings.Contains(err.Error(), "allowed:") {
			t.Errorf("error must list allowed providers, got %q", err.Error())
		}
	})
	t.Run("missing_api_key", func(t *testing.T) {
		_, err := New(embeddings.ConfigSnapshot{Provider: "openai", Model: "m"}, embeddings.Deps{})
		if !errors.Is(err, ErrMissingAPIKey) {
			t.Fatalf("err=%v, want ErrMissingAPIKey", err)
		}
	})
	t.Run("unset_env_key_names_var", func(t *testing.T) {
		_, err := New(embeddings.ConfigSnapshot{Provider: "openai", Model: "m", APIKey: "env.HARBOR_TEST_UNSET_EMBED_KEY"}, embeddings.Deps{})
		if !errors.Is(err, ErrMissingAPIKey) {
			t.Fatalf("err=%v, want ErrMissingAPIKey", err)
		}
		if !strings.Contains(err.Error(), "HARBOR_TEST_UNSET_EMBED_KEY") {
			t.Errorf("error must name the env var, got %q", err.Error())
		}
	})
}

func TestAccount_ResolvesLiteralAndEnvKeys(t *testing.T) {
	// Documented dummy value — never a real credential (AGENTS.md §7).
	t.Setenv("HARBOR_TEST_EMBED_KEY", "dummy-embed-key-for-tests")
	acct, err := newAccount(embeddings.ConfigSnapshot{
		Provider: "openai", Model: "m", APIKey: "env.HARBOR_TEST_EMBED_KEY",
	})
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	keys, err := acct.GetKeysForProvider(context.Background(), bfschemas.ModelProvider("openai"))
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 || keys[0].Value.Val != "dummy-embed-key-for-tests" {
		t.Errorf("env key not resolved: %+v", keys)
	}
	if len(keys[0].Models) != 1 || keys[0].Models[0] != "*" {
		t.Errorf("key Models=%v, want the [*] wildcard", keys[0].Models)
	}
	if _, err := acct.GetKeysForProvider(context.Background(), bfschemas.ModelProvider("anthropic")); err == nil {
		t.Error("GetKeysForProvider for an unconfigured provider must error")
	}
	if _, err := acct.GetConfigForProvider(bfschemas.ModelProvider("anthropic")); err == nil {
		t.Error("GetConfigForProvider for an unconfigured provider must error")
	}
	providers, err := acct.GetConfiguredProviders()
	if err != nil || len(providers) != 1 {
		t.Errorf("GetConfiguredProviders=%v err=%v", providers, err)
	}
}
