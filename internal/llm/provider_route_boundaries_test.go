package llm_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestProviderRoute_EndpointNormalizationBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ raw, want string }{
		{" https://API.example.test/v1/// ", "https://api.example.test/v1"},
		{"https://api.example.test/", "https://api.example.test"},
		{"http://localhost:8080/v1/", "http://localhost:8080/v1"},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080"},
		{"http://[::1]:8080/", "http://[::1]:8080"},
	} {
		raw, want := tc.raw, tc.want
		t.Run(raw, func(t *testing.T) {
			got, digest, err := llm.NormalizeProviderEndpoint(raw)
			if err != nil || got != want || digest != fmt.Sprintf("%x", sha256.Sum256([]byte(want))) {
				t.Fatalf("normalization=(%q,%q,%v), want %q and exact digest", got, digest, err, want)
			}
			again, secondDigest, err := llm.NormalizeProviderEndpoint(got)
			if err != nil || again != got || secondDigest != digest {
				t.Fatal("normalization is not stable")
			}
		})
	}
	for _, raw := range []string{"", "/v1", "https:///v1", "https://example.test/%zz", "https://user:private@example.test", "https://example.test?secret=private", "https://example.test/#private", "http://remote.example.test", "http://192.0.2.1", "ftp://localhost", "https://example.test\n"} {
		t.Run(raw, func(t *testing.T) {
			endpoint, digest, err := llm.NormalizeProviderEndpoint(raw)
			if !errors.Is(err, llm.ErrProviderRouteInvalid) || endpoint != "" || digest != "" {
				t.Fatal("invalid credential destination accepted")
			}
		})
	}
}

func TestProviderRoute_EndpointMustMatchProviderAndDigest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	endpoint, digest, err := llm.NormalizeProviderEndpoint("https://private.example.test/v1")
	if err != nil {
		t.Fatal(err)
	}
	req := llm.ProviderRouteRequest{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "model"}
	for kind, provider := range map[llm.ProviderEndpointKind]string{
		llm.ProviderEndpointAzure: "azure", llm.ProviderEndpointVLLM: "vllm", llm.ProviderEndpointOllama: "ollama", llm.ProviderEndpointSGL: "sgl", llm.ProviderEndpointOpenAICompatible: "openai",
	} {
		t.Run(string(kind), func(t *testing.T) {
			binding := &llm.ProviderEndpointBinding{Kind: kind, Value: endpoint, Digest: digest}
			resolver := &countingRouteResolver{out: llm.ResolvedProviderRoute{Provider: provider, Model: "model", KeyName: "fixture key", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration, ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration, CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector, Endpoint: binding, ExpiresAt: now.Add(time.Minute), Credential: "test-fixture-not-a-real-key"}}
			cfg := llm.ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}
			selected, err := llm.SelectProviderRoute(t.Context(), cfg, req, now)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := llm.ResolveProviderRoute(t.Context(), cfg, req, now)
			if err != nil || !llm.ProviderRouteResolutionMatchesSelection(resolved, selected) {
				t.Fatalf("valid exact route refused: %v", err)
			}
			var logs bytes.Buffer
			slog.New(slog.NewJSONHandler(&logs, nil)).Info("route", "selected", selected, "endpoint", *binding)
			formatted := fmt.Sprintf("%s %+v %#v %s %#v", selected, selected, selected, *binding, *binding)
			if strings.Contains(logs.String()+formatted, endpoint) || !strings.Contains(logs.String(), digest) {
				t.Fatal("endpoint privacy or diagnostic digest lost")
			}
			for name, mutate := range map[string]func(){
				"wrong provider": func() { resolver.out.Provider = "wrong" },
				"wrong kind":     func() { binding.Kind = "unknown" },
				"wrong digest":   func() { binding.Digest = "wrong" },
				"not canonical":  func() { binding.Value = endpoint + "/" },
				"unsafe url":     func() { binding.Value = "http://remote.example.test" },
			} {
				t.Run(name, func(t *testing.T) {
					resolver.out.Provider = provider
					*binding = llm.ProviderEndpointBinding{Kind: kind, Value: endpoint, Digest: digest}
					mutate()
					if _, err := llm.SelectProviderRoute(t.Context(), cfg, req, now); !errors.Is(err, llm.ErrProviderRouteInvalid) {
						t.Fatalf("selection accepted changed binding: %v", err)
					}
					if _, err := llm.ResolveProviderRoute(t.Context(), cfg, req, now); !errors.Is(err, llm.ErrProviderRouteInvalid) {
						t.Fatalf("attempt accepted changed binding: %v", err)
					}
				})
			}
		})
	}
}

func TestProviderRoute_ModelProfileMustRemainExactAtAttempt(t *testing.T) {
	t.Parallel()
	profile := llm.ProviderModelProfile{ContextWindowTokens: 1000000, MaxOutputTokens: 128000, ReasoningEffort: llm.ReasoningHigh, ReasoningEffortLevels: []llm.ReasoningEffort{llm.ReasoningLow, llm.ReasoningHigh}}
	selected := llm.SelectedProviderRoute{ModelProfile: &profile}
	for name, mutate := range map[string]func(*llm.ProviderModelProfile){
		"context":           func(p *llm.ProviderModelProfile) { p.ContextWindowTokens++ },
		"output":            func(p *llm.ProviderModelProfile) { p.MaxOutputTokens++ },
		"default reasoning": func(p *llm.ProviderModelProfile) { p.ReasoningEffort = llm.ReasoningLow },
		"level count":       func(p *llm.ProviderModelProfile) { p.ReasoningEffortLevels = []llm.ReasoningEffort{llm.ReasoningHigh} },
		"level value":       func(p *llm.ProviderModelProfile) { p.ReasoningEffortLevels[0] = llm.ReasoningMedium },
	} {
		t.Run(name, func(t *testing.T) {
			copy := profile
			copy.ReasoningEffortLevels = append([]llm.ReasoningEffort(nil), profile.ReasoningEffortLevels...)
			resolved := llm.ResolvedProviderRoute{ModelProfile: &copy}
			if !llm.ProviderRouteResolutionMatchesSelection(resolved, selected) {
				t.Fatal("identical profile refused")
			}
			mutate(&copy)
			if llm.ProviderRouteResolutionMatchesSelection(resolved, selected) {
				t.Fatal("attempt changed admitted capacity or reasoning")
			}
		})
	}
	unspecified := llm.ProviderModelProfile{ContextWindowTokens: 1000000, MaxOutputTokens: 128000}
	explicitNone := unspecified
	explicitNone.ReasoningEffortLevels = []llm.ReasoningEffort{}
	if llm.ProviderRouteResolutionMatchesSelection(llm.ResolvedProviderRoute{ModelProfile: &explicitNone}, llm.SelectedProviderRoute{ModelProfile: &unspecified}) {
		t.Fatal("unknown reasoning support collapsed into explicit no-support")
	}
	if llm.ProviderRouteResolutionMatchesSelection(llm.ResolvedProviderRoute{}, selected) {
		t.Fatal("attempt dropped selected model profile")
	}
}

func TestProviderRoute_ModelFactsRejectInvalidCapacityAndReasoning(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*llm.ProviderModelProfile){
		"zero context":           func(p *llm.ProviderModelProfile) { p.ContextWindowTokens = 0 },
		"context overflow":       func(p *llm.ProviderModelProfile) { p.ContextWindowTokens = (1 << 30) + 1 },
		"zero output":            func(p *llm.ProviderModelProfile) { p.MaxOutputTokens = 0 },
		"output overflow":        func(p *llm.ProviderModelProfile) { p.MaxOutputTokens = (1 << 30) + 1 },
		"output exceeds context": func(p *llm.ProviderModelProfile) { p.MaxOutputTokens = p.ContextWindowTokens + 1 },
		"unknown default":        func(p *llm.ProviderModelProfile) { p.ReasoningEffort = "unknown" },
		"undeclared support":     func(p *llm.ProviderModelProfile) { p.ReasoningEffortLevels = nil },
		"too many levels":        func(p *llm.ProviderModelProfile) { p.ReasoningEffortLevels = make([]llm.ReasoningEffort, 5) },
		"unknown level":          func(p *llm.ProviderModelProfile) { p.ReasoningEffortLevels = []llm.ReasoningEffort{"unknown"} },
		"empty level":            func(p *llm.ProviderModelProfile) { p.ReasoningEffortLevels = []llm.ReasoningEffort{""} },
		"duplicate level": func(p *llm.ProviderModelProfile) {
			p.ReasoningEffortLevels = []llm.ReasoningEffort{llm.ReasoningHigh, llm.ReasoningHigh}
		},
		"unsupported default": func(p *llm.ProviderModelProfile) { p.ReasoningEffortLevels = []llm.ReasoningEffort{llm.ReasoningLow} },
	} {
		t.Run(name, func(t *testing.T) {
			profile := llm.ProviderModelProfile{ContextWindowTokens: 1000000, MaxOutputTokens: 128000, ReasoningEffort: llm.ReasoningHigh, ReasoningEffortLevels: []llm.ReasoningEffort{llm.ReasoningHigh}}
			if err := llm.ValidateProviderModelProfile(profile); err != nil {
				t.Fatal(err)
			}
			mutate(&profile)
			if err := llm.ValidateProviderModelProfile(profile); !errors.Is(err, llm.ErrProviderRouteInvalid) {
				t.Fatalf("invalid provider facts accepted: %v", err)
			}
		})
	}
}

func TestProviderRoute_AttemptContextDoesNotLeakToParentOrSiblings(t *testing.T) {
	t.Parallel()
	parent := t.Context()
	if _, ok := llm.ResolvedProviderRouteFrom(parent); ok {
		t.Fatal("unbound parent has a route")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Dummy per-attempt values; no credential resolver or provider call.
			want := llm.ResolvedProviderRoute{RouteID: fmt.Sprintf("route-%d", i), Credential: fmt.Sprintf("fixture-only-%d", i)}
			ctx := llm.WithResolvedProviderRoute(parent, want)
			<-start
			got, ok := llm.ResolvedProviderRouteFrom(ctx)
			if !ok || got.RouteID != want.RouteID || got.Credential != want.Credential {
				t.Error("attempt context read another sibling's route")
			}
			if _, ok := llm.ResolvedProviderRouteFrom(parent); ok {
				t.Error("attempt route escaped into parent")
			}
		}()
	}
	close(start)
	wg.Wait()
}
