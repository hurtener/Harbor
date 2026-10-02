package bifrost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/llm/pricing"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestAllocation_HiddenTransportRetriesRemainFunded(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"synthetic unavailable"}}`))
	}))
	defer server.Close()
	const env = "HARBOR_TEST_ALLOCATION_SYNTHETIC_KEY"
	t.Setenv(env, "test-only")
	cfg := llm.ConfigSnapshot{Driver: "bifrost", Provider: "allocation-local", Model: "model", DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true, ModelProfiles: map[string]llm.ModelProfile{"model": {ContextWindowTokens: 1000}}, CustomProviders: []llm.CustomProviderSpec{{Name: "allocation-local", BaseURL: server.URL + "/v1", APIKeyEnvVar: env, Models: []string{"model"}, MaxRetries: 1, Timeout: 5 * time.Second, RetryBackoffInitial: time.Millisecond, RetryBackoffMax: time.Millisecond}}}
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	mgr := allocation.New(st)
	deps, closeDeps := makeCustomProviderTestDeps(t)
	defer closeDeps()
	deps.Allocations = mgr
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	ctx, _ := identity.WithRun(t.Context(), q.Identity, q.RunID)
	a := llm.InferenceAllocation{AllocationID: "f", Revision: 1, MaxTotalTokens: 3300}
	ctx = llm.WithInferenceAllocation(ctx, &a)
	max := 100
	req := llm.CompleteRequest{Model: "model", MaxTokens: &max, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("hello")}}}}
	if _, err = client.Complete(ctx, req); err == nil {
		t.Fatal("expected synthetic provider failure")
	}
	if hits.Load() != 2 {
		t.Fatalf("physical requests=%d want2", hits.Load())
	}
	snap, err := mgr.Snapshot(ctx, q, a)
	if err != nil || snap.ReservedTokens != 3300 || snap.UnknownTokens != 3300 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatalf("refusal %v", err)
	}
	if hits.Load() != 2 {
		t.Fatal("exhausted allocation reached transport")
	}
}

func TestMonetaryAllocation_BifrostPhysicalRetriesStayReserved(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"synthetic unavailable"}}`))
	}))
	defer server.Close()
	model := "fixture-version-1"
	tariff := pricing.Tariff{EndpointBinding: pricing.EndpointBinding(server.URL + "/v1"), Provider: "openai", Model: model, ModelVersion: model, ImmutableModelVersion: true, IncludesAllCharges: true, InputMicroUSDPerMillion: new(int64(1)), OutputMicroUSDPerMillion: new(int64(1)), CacheReadMicroUSDPerMillion: new(int64(1)), CacheWriteMicroUSDPerMillion: new(int64(1)), ReasoningMicroUSDPerMillion: new(int64(1)), RequestMicroUSD: new(int64(1)), AncillaryMicroUSD: new(int64(1))}
	catalog, err := pricing.New([]pricing.Manifest{{ID: "synthetic-localhost", Revision: 1, Currency: "USD", Tariffs: []pricing.Tariff{tariff}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := catalog.References()[0]
	cfg := llm.ConfigSnapshot{Driver: "bifrost", Provider: "openai", Model: model, APIKey: "test-only-not-a-real-key", BaseURL: server.URL + "/v1", Timeout: 5 * time.Second, NetworkDefaults: llm.NetworkDefaults{MaxRetries: 1, RetryBackoffInitial: time.Millisecond, RetryBackoffMax: time.Millisecond}, DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true, ModelProfiles: map[string]llm.ModelProfile{model: {ContextWindowTokens: 1000}}}
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	mgr := allocation.New(st)
	deps, closeDeps := makeCustomProviderTestDeps(t)
	defer closeDeps()
	deps.Allocations = mgr
	deps.PricingCatalog = catalog
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "money"}
	ctx, _ := identity.WithRun(t.Context(), q.Identity, q.RunID)
	a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 3300, MaxCostMicroUSD: new(int64(21)), PricingManifestID: ref.ID, PricingManifestRevision: ref.Revision, PricingManifestSHA256: ref.SHA256}
	ctx = llm.WithInferenceAllocation(ctx, &a)
	req := llm.CompleteRequest{Model: model, MaxTokens: new(100), Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("hello")}}}}
	if _, err = client.Complete(ctx, req); err == nil {
		t.Fatal("expected fixture failure")
	}
	if hits.Load() != 2 {
		t.Fatalf("physical attempts=%d want2", hits.Load())
	}
	snap, err := mgr.Snapshot(ctx, q, a)
	if err != nil || snap.ReservedCostMicroUSD != 21 || snap.UnknownCostMicroUSD != 21 || snap.ChargedCostMicroUSD != 0 {
		t.Fatalf("%+v %v", snap, err)
	}
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrAllocationExhausted) || hits.Load() != 2 {
		t.Fatal("cap bypass", err, hits.Load())
	}
}

func TestMonetaryAllocation_BifrostRejectsUnboundedShapes(t *testing.T) {
	account, err := newAccount(llm.ConfigSnapshot{Provider: "openai", APIKey: "synthetic-fixture", Model: "fixture-v1"}, llm.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{provider: "openai", account: account}
	p := llm.ModelProfile{ContextWindowTokens: 1000}
	req := llm.CompleteRequest{Model: "fixture-v1", MaxTokens: new(100)}
	if _, err := d.MonetaryTarget(llm.WithTrustedProviderRoute(t.Context(), llm.TrustedProviderRouteContext{}), req, p); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatal("external route priced by native name", err)
	}
	for _, part := range []llm.ContentPart{{Type: llm.PartImage, Image: &llm.ImagePart{URL: "https://example.test/image"}}, {Type: llm.PartFile, File: &llm.FilePart{}}, {Type: llm.PartAudio, Audio: &llm.AudioPart{}}} {
		req.Messages = []llm.ChatMessage{{Content: llm.Content{Parts: []llm.ContentPart{part}}}}
		if _, err := d.MonetaryTarget(t.Context(), req, p); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
			t.Fatal("unbounded multimedia", err)
		}
	}
	req.Messages = nil
	req.Extra = map[string]any{"unbounded": true}
	if _, err := d.MonetaryTarget(t.Context(), req, p); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatal(err)
	}
	req.Extra = nil
	d.provider = "openrouter"
	if _, err := d.MonetaryTarget(t.Context(), req, p); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatal("opaque fallback", err)
	}
	t.Setenv("HARBOR_SYNTHETIC_CUSTOM_MONETARY", "fixture-only")
	account, err = newAccount(llm.ConfigSnapshot{Provider: "openai", CustomProviders: []llm.CustomProviderSpec{{Name: "openai", BaseURL: "http://127.0.0.1:1", APIKeyEnvVar: "HARBOR_SYNTHETIC_CUSTOM_MONETARY", Models: []string{"fixture-v1"}}}}, llm.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	d.account = account
	d.provider = "openai"
	if _, err := d.MonetaryTarget(t.Context(), req, p); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatal("custom provider disguised as native", err)
	}
	d.provider = "anthropic"
	account, err = newAccount(llm.ConfigSnapshot{Provider: "anthropic", APIKey: "synthetic-fixture", Model: "fixture-v1"}, llm.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	d.account = account
	req.ReasoningEffort = llm.ReasoningHigh
	target, err := d.MonetaryTarget(t.Context(), req, p)
	if err != nil || target.OutputTokens <= 100 {
		t.Fatal("unreserved reasoning", target, err)
	}
}
