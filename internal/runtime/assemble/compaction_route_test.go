package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

type assemblyCompactionResolver struct {
	selected llm.SelectedProviderRoute
	selects  atomic.Int64
	resolves atomic.Int64
	revoked  atomic.Bool
}

func (r *assemblyCompactionResolver) SelectProviderRoute(context.Context, llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
	r.selects.Add(1)
	if r.revoked.Load() {
		return llm.SelectedProviderRoute{}, errors.New("revoked fixture route")
	}
	return r.selected, nil
}
func (r *assemblyCompactionResolver) ResolveProviderRoute(context.Context, llm.ProviderRouteRequest) (llm.ResolvedProviderRoute, error) {
	r.resolves.Add(1)
	s := r.selected
	return llm.ResolvedProviderRoute{Provider: s.Provider, Model: s.Model, KeyName: s.KeyName,
		RouteID: s.RouteID, RouteGeneration: s.RouteGeneration, ProviderConnectionID: s.ProviderConnectionID,
		ProviderConnectionGeneration: s.ProviderConnectionGeneration, CredentialAssetGeneration: s.CredentialAssetGeneration,
		ModelSelector: s.ModelSelector, Endpoint: s.Endpoint, ExpiresAt: s.ExpiresAt, ModelProfile: s.ModelProfile,
		Credential: "route-only-fixture-key"}, nil
}

func TestAssemble_IndependentCompactionRouteReachesGovernedBifrost(t *testing.T) {
	const env = "HARBOR_COMPACTION_ROUTE_FIXTURE_KEY"
	t.Setenv(env, "unused-local-fixture-key")
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer route-only-fixture-key" {
			t.Error("compaction did not use the independently resolved credential")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request["model"] != "compact-model" || request["reasoning_effort"] != nil {
			t.Errorf("wrong outgoing model/reasoning: %v", request)
		}
		output := request["max_tokens"]
		if output == nil {
			output = request["max_completion_tokens"]
		}
		if output != float64(2048) {
			t.Errorf("wrong outgoing maintenance allowance: %v", output)
		}
		format, _ := request["response_format"].(map[string]any)
		envelope, _ := format["json_schema"].(map[string]any)
		schema, _ := envelope["schema"].(map[string]any)
		if format["type"] != "json_schema" || envelope["name"] != "harbor_response" || schema["type"] != "object" {
			t.Errorf("maintenance request lacks the provider's named schema envelope: %v", format)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"summary","object":"chat.completion","created":1,"model":"compact-model","choices":[{"index":0,"message":{"role":"assistant","content":"{\"goals\":[\"edit layout\"],\"facts\":[\"keep the original project\"],\"pending\":[],\"last_output_digest\":\"inspected\",\"note\":\"\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`))
	}))
	defer provider.Close()
	endpoint, digest, err := llm.NormalizeProviderEndpoint(provider.URL)
	if err != nil {
		t.Fatal(err)
	}
	route := &config.MemorySummarizerProviderRoute{RouteID: "maintenance", RouteGeneration: 1, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "compact-alias"}
	resolver := &assemblyCompactionResolver{selected: llm.SelectedProviderRoute{
		Provider: "openai", Model: "compact-model", KeyName: "route-fixture-label", RouteID: route.RouteID, RouteGeneration: route.RouteGeneration,
		ProviderConnectionID: route.ProviderConnectionID, ProviderConnectionGeneration: route.ProviderConnectionGeneration,
		CredentialAssetGeneration: route.CredentialAssetGeneration, ModelSelector: route.ModelSelector, ExpiresAt: time.Now().Add(time.Minute),
		Endpoint:     &llm.ProviderEndpointBinding{Kind: llm.ProviderEndpointOpenAICompatible, Value: endpoint, Digest: digest},
		ModelProfile: &llm.ProviderModelProfile{ContextWindowTokens: 16000, MaxOutputTokens: 4096},
	}}
	cfg := minimalCfg(t)
	cfg.Memory.BudgetTokens, cfg.Memory.Summarizer.ProviderRoute = 100, route
	cfg.LLM.Driver, cfg.LLM.Provider, cfg.LLM.Model = "bifrost", "fixture-route", "driving-model"
	cfg.LLM.CustomProviders = []config.LLMCustomProviderConfig{{Name: "fixture-route", BaseURL: provider.URL,
		APIKeyEnvVar: env, Models: []string{"driving-model", "compact-model"}, Timeout: 5 * time.Second}}
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{"driving-model": {ContextWindowTokens: 1000000, TokenEstimator: "chars_div_4"}}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{ProviderRoute: llm.ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stack.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}
	ctx, err := identity.WithRun(t.Context(), id, "run")
	if err != nil {
		t.Fatal(err)
	}
	ctx = llm.WithTrustedProviderRoute(ctx, llm.TrustedProviderRouteContext{RuntimeID: "runtime", EffectiveAgentID: "agent", TaskID: "task", Purpose: llm.ProviderRoutePurposeRun,
		Route: llm.ProviderRoute{RouteID: "driving", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "driving-alias"}})
	rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: "run"}, Query: "edit", Budget: planner.Budget{TokenBudget: 100}}
	trajectory := func() *planner.Trajectory {
		return &planner.Trajectory{Query: "edit", Steps: []planner.Step{{LLMObservation: strings.Repeat("layout detail ", 100)}, {LLMObservation: "fresh result"}}}
	}
	tr := trajectory()
	if err := stack.Compression.MaybeCompressRequest(ctx, rc, tr, 1000); err != nil {
		t.Fatal(err)
	}
	if tr.Summary == nil || calls.Load() != 1 || resolver.selects.Load() != 2 || resolver.resolves.Load() != 1 {
		t.Fatalf("missing actual governed route: summary=%v calls=%d selects=%d resolves=%d", tr.Summary != nil, calls.Load(), resolver.selects.Load(), resolver.resolves.Load())
	}
	resolver.revoked.Store(true)
	if err := stack.Compression.MaybeCompressRequest(ctx, rc, trajectory(), 1000); !errors.Is(err, llm.ErrProviderRouteResolutionFailed) {
		t.Fatalf("revoked maintenance route did not fail closed: %v", err)
	}
	if calls.Load() != 1 || resolver.resolves.Load() != 1 {
		t.Fatal("revoked maintenance fell back to another credential")
	}
}
