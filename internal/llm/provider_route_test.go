package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

type countingRouteResolver struct {
	calls atomic.Int64
	out   llm.ResolvedProviderRoute
	err   error
}

func (r *countingRouteResolver) ResolveProviderRoute(context.Context, llm.ProviderRouteRequest) (llm.ResolvedProviderRoute, error) {
	r.calls.Add(1)
	return r.out, r.err
}

func (r *countingRouteResolver) SelectProviderRoute(context.Context, llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
	r.calls.Add(1)
	return llm.SelectedProviderRoute{
		Provider: r.out.Provider, Model: r.out.Model, KeyName: r.out.KeyName, RouteID: r.out.RouteID, RouteGeneration: r.out.RouteGeneration,
		ProviderConnectionID: r.out.ProviderConnectionID, ProviderConnectionGeneration: r.out.ProviderConnectionGeneration,
		CredentialAssetGeneration: r.out.CredentialAssetGeneration, ModelSelector: r.out.ModelSelector, Endpoint: r.out.Endpoint, ExpiresAt: r.out.ExpiresAt,
	}, r.err
}

func TestProviderRoute_DefaultIsLatentAndExplicitMissingFails(t *testing.T) {
	if err := llm.ValidateProviderRouteConfig(llm.ProviderRouteConfig{}); err != nil {
		t.Fatalf("zero config: %v", err)
	}
	if err := llm.ValidateProviderRouteConfig(llm.ProviderRouteConfig{RuntimeID: "runtime"}); err == nil {
		t.Fatal("partial injected config accepted")
	}
	if err := llm.ValidateProviderRoute(llm.ProviderRoute{}); err != nil {
		t.Fatalf("empty runtime-default route: %v", err)
	}
	_, err := llm.ResolveProviderRoute(context.Background(), llm.ProviderRouteConfig{}, llm.ProviderRouteRequest{}, time.Now())
	if !errors.Is(err, llm.ErrProviderRouteResolverUnavailable) {
		t.Fatalf("missing resolver error = %v", err)
	}
}

func TestOpen_ProviderRouteFailsBeforeNonBifrostExecution(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	resolver := &countingRouteResolver{}

	deps.ProviderRoute = llm.ProviderRouteConfig{RuntimeID: "runtime-only"}
	if _, err := llm.Open(context.Background(), makeSnapshot("model", 1000), deps); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("partial provider route Open error = %v, want ErrInvalidConfig", err)
	}
	deps.ProviderRoute = llm.ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}
	if _, err := llm.Open(context.Background(), makeSnapshot("model", 1000), deps); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("non-Bifrost provider route Open error = %v, want ErrInvalidConfig", err)
	}
	if resolver.calls.Load() != 0 {
		t.Fatalf("resolver called %d times during refused boot", resolver.calls.Load())
	}
}

func TestResolvedProviderRoute_NeverSerializesCredential(t *testing.T) {
	endpoint := &llm.ProviderEndpointBinding{Kind: llm.ProviderEndpointOpenAICompatible, Value: "https://private-endpoint.example.test", Digest: "digest"}
	body, err := json.Marshal(llm.ResolvedProviderRoute{Provider: "openai", Model: "model", Credential: "do-not-serialize", Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "" || contains(string(body), "do-not-serialize") || contains(string(body), `"credential":`) || contains(string(body), endpoint.Value) {
		t.Fatalf("credential escaped route result: %s", body)
	}
	resolved := llm.ResolvedProviderRoute{Provider: "openai", Model: "model", Credential: "do-not-log", Endpoint: endpoint}
	if formatted := fmt.Sprintf("%+v %#v", resolved, resolved); contains(formatted, "do-not-log") || contains(formatted, endpoint.Value) {
		t.Fatalf("credential escaped formatted diagnostics: %s", formatted)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("route", slog.Any("resolved", resolved))
	if contains(logs.String(), "do-not-log") || contains(logs.String(), endpoint.Value) {
		t.Fatalf("credential escaped slog diagnostics: %s", logs.String())
	}
}

func TestResolveProviderRoute_RejectsStaleOrMismatchedBinding(t *testing.T) {
	now := time.Now().UTC()
	req := llm.ProviderRouteRequest{RouteID: "route", RouteGeneration: 2, ProviderConnectionID: "connection", ProviderConnectionGeneration: 3, CredentialAssetGeneration: 4, ModelSelector: "fast"}
	for name, out := range map[string]llm.ResolvedProviderRoute{
		"expired":             {Provider: "openai", Model: "model", KeyName: "route key", RouteID: "route", RouteGeneration: 2, ProviderConnectionID: "connection", ProviderConnectionGeneration: 3, CredentialAssetGeneration: 4, ModelSelector: "fast", Credential: "key", ExpiresAt: now},
		"generation mismatch": {Provider: "openai", Model: "model", KeyName: "route key", RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 3, CredentialAssetGeneration: 4, ModelSelector: "fast", Credential: "key", ExpiresAt: now.Add(time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := &countingRouteResolver{out: out}
			if _, err := llm.ResolveProviderRoute(context.Background(), llm.ProviderRouteConfig{Resolver: resolver}, req, now); !errors.Is(err, llm.ErrProviderRouteInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestProviderRouteResolverErrorsAreContentFreeAndCancellationIsPreserved(t *testing.T) {
	const canary = "credential-canary-must-not-escape"
	resolver := &countingRouteResolver{err: fmt.Errorf("upstream failed with %s", canary)}
	cfg := llm.ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}
	for _, call := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, err := llm.SelectProviderRoute(ctx, cfg, llm.ProviderRouteRequest{}, time.Now())
			return err
		},
		func(ctx context.Context) error {
			_, err := llm.ResolveProviderRoute(ctx, cfg, llm.ProviderRouteRequest{}, time.Now())
			return err
		},
	} {
		err := call(context.Background())
		if !errors.Is(err, llm.ErrProviderRouteResolutionFailed) || contains(err.Error(), canary) {
			t.Fatalf("resolver error = %q, want fixed content-free sentinel", err)
		}
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := llm.ResolveProviderRoute(cancelled, cfg, llm.ProviderRouteRequest{}, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resolver error = %v, want context.Canceled", err)
	}
	resolver.err = fmt.Errorf("wrapped: %w", context.DeadlineExceeded)
	if _, err := llm.ResolveProviderRoute(context.Background(), cfg, llm.ProviderRouteRequest{}, time.Now()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline resolver error = %v, want context.DeadlineExceeded", err)
	}
}

func TestProviderRouteResolutionMustExactMatchPrePolicySelection(t *testing.T) {
	selected := llm.SelectedProviderRoute{
		Provider: "openai", Model: "selected-model", KeyName: "route key", RouteID: "route", RouteGeneration: 2,
		ProviderConnectionID: "connection", ProviderConnectionGeneration: 3,
		CredentialAssetGeneration: 4, ModelSelector: "fast",
	}
	resolved := llm.ResolvedProviderRoute{
		Provider: selected.Provider, Model: selected.Model, KeyName: selected.KeyName, RouteID: selected.RouteID, RouteGeneration: selected.RouteGeneration,
		ProviderConnectionID: selected.ProviderConnectionID, ProviderConnectionGeneration: selected.ProviderConnectionGeneration,
		CredentialAssetGeneration: selected.CredentialAssetGeneration, ModelSelector: selected.ModelSelector,
		Credential: "attempt-only-key",
	}
	if !llm.ProviderRouteResolutionMatchesSelection(resolved, selected) {
		t.Fatal("exact attempt resolution did not match selection")
	}
	resolved.Model = "different-model"
	if llm.ProviderRouteResolutionMatchesSelection(resolved, selected) {
		t.Fatal("attempt resolution crossed the pre-policy model selection")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
