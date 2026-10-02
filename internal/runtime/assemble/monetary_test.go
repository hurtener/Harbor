package assemble_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/llm/pricing"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tasks"
	taskprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

func TestMonetaryAllocation_AssemblyWiresTrustedPricingAndTaskProjection(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","model":"fixture-version-1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	defer server.Close()
	m := pricing.Manifest{ID: "assembled-synthetic", Revision: 1, Currency: "USD", Tariffs: []pricing.Tariff{{EndpointBinding: pricing.EndpointBinding(server.URL + "/v1"), Provider: "openai", Model: "fixture-version-1", ModelVersion: "fixture-version-1", ImmutableModelVersion: true, IncludesAllCharges: true, InputMicroUSDPerMillion: new(int64(1)), OutputMicroUSDPerMillion: new(int64(1)), CacheReadMicroUSDPerMillion: new(int64(0)), CacheWriteMicroUSDPerMillion: new(int64(0)), ReasoningMicroUSDPerMillion: new(int64(0)), RequestMicroUSD: new(int64(1)), AncillaryMicroUSD: new(int64(1))}}}
	catalog, err := pricing.New([]pricing.Manifest{m})
	if err != nil {
		t.Fatal(err)
	}
	ref := catalog.References()[0]
	cfg := minimalCfg(t)
	cfg.LLM = config.LLMConfig{Driver: "bifrost", Provider: "openai", Model: "fixture-version-1", BaseURL: server.URL + "/v1", APIKey: "test-only-not-a-real-key", Timeout: 5 * time.Second, PricingManifests: []pricing.Manifest{m}, ModelProfiles: map[string]config.LLMModelProfileConfig{"fixture-version-1": {ContextWindowTokens: 1000}}, NetworkDefaults: config.LLMNetworkDefaults{MaxRetries: 1}}
	cfg.State = config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "state.db")}
	cfg.Tasks.Driver = "durable"
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stack.Close(context.Background()) }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	a := &llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 3300, MaxCostMicroUSD: new(int64(12)), PricingManifestID: ref.ID, PricingManifestRevision: ref.Revision, PricingManifestSHA256: ref.SHA256}
	h, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "work", InferenceAllocation: a})
	if err != nil {
		t.Fatal("assembly did not wire acceptance", err)
	}
	accepted, err := stack.Tasks.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	callCtx, err := identity.WithRun(ctx, id, string(h.ID))
	if err != nil {
		t.Fatal(err)
	}
	callCtx = llm.WithInferenceAllocationTask(callCtx, accepted.InferenceAllocation, string(h.ID))
	req := llm.CompleteRequest{Model: "fixture-version-1", MaxTokens: new(100), Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("hello")}}}}
	if _, err = stack.LLM.Complete(callCtx, req); err != nil {
		t.Fatal("assembly did not wire provider pricing", err)
	}
	if _, err = stack.LLM.Complete(callCtx, req); !errors.Is(err, llm.ErrAllocationExhausted) || hits.Load() != 1 {
		t.Fatal("cap bypass", err, hits.Load())
	}
	projector, err := taskprotocol.NewRegistryProjector(stack.Tasks, taskprotocol.WithAllocations(allocation.New(stack.State)))
	if err != nil {
		t.Fatal(err)
	}
	detail, err := projector.GetTask(ctx, id, string(h.ID))
	if err != nil || detail.InferenceAllocation == nil || detail.InferenceAllocation.UnknownCostMicroUSD != 12 || detail.InferenceAllocation.PricingManifestSHA256 != ref.SHA256 {
		t.Fatal(detail, err)
	}
	if err = stack.Tasks.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if err = stack.Tasks.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	detail, err = projector.GetTask(ctx, id, string(h.ID))
	if err != nil || !detail.InferenceAllocation.Closed || detail.InferenceAllocation.UnknownCostMicroUSD != 12 {
		t.Fatalf("closed unknown liability: %+v %v", detail, err)
	}
	if _, err = stack.LLM.Complete(callCtx, req); !errors.Is(err, llm.ErrAllocationClosed) || hits.Load() != 1 {
		t.Fatal("late transport escaped closed allocation", err, hits.Load())
	}
	if err = stack.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	*m.Tariffs[0].AncillaryMicroUSD = 9
	cfg.LLM.PricingManifests = []pricing.Manifest{m}
	broken, err := assemble.Assemble(t.Context(), cfg, assemble.Options{})
	if broken != nil {
		_ = broken.Close(context.Background())
	}
	if !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatal("startup accepted changed immutable revision", err)
	}
}
