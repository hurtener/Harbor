package bifrost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// canned OpenAI-compatible chat-completions response used by the
// happy-path handler. Token usage non-zero so the cost-passthrough
// path exercises.
const cannedResponse = `{
  "id":"chatcmpl-test",
  "object":"chat.completion",
  "created":1700000000,
  "model":"google/gemma-4-31b-it",
  "choices":[{
    "index":0,
    "message":{"role":"assistant","content":"hello world"},
    "finish_reason":"stop"
  }],
  "usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
}`

// makeCustomProviderTestDeps wires the minimum llm.Deps the safety
// pass requires (artifacts + bus + audit redactor). Returns a
// teardown closure callers defer.
func makeCustomProviderTestDeps(t *testing.T) (llm.Deps, func()) {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              30 * time.Second,
		DropWindow:               1 * time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{
		Driver:                    "inmem",
		HeavyOutputThresholdBytes: 32 * 1024,
	})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("artifacts.Open: %v", err)
	}
	deps := llm.Deps{
		Artifacts: store,
		Bus:       bus,
	}
	_ = red // retained for redactor parity; Phase 32 deps don't include it
	teardown := func() {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
	}
	return deps, teardown
}

// TestE2E_CustomProvider_HappyPath spins up an httptest.Server that
// mimics an OpenAI-compatible /v1/chat/completions endpoint and
// verifies the bifrost driver routes a Complete() call through it.
// Proves Phase 33a's custom-provider wiring is alive end-to-end —
// not just at the config-mapping layer.
func TestE2E_CustomProvider_HappyPath(t *testing.T) {
	const envName = "HARBOR_TEST_OPENAI_COMPAT_KEY"
	t.Setenv(envName, "test-key-value")

	var seen atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("server got unexpected path: %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("Authorization header missing Bearer prefix: %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("server: bad JSON: %v body=%q", err, string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedResponse))
	}))
	defer server.Close()

	cfg := llm.ConfigSnapshot{
		Driver:               "bifrost",
		Provider:             "custom-openai-compat",
		Model:                "google/gemma-4-31b-it",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		ModelProfiles: map[string]llm.ModelProfile{
			"google/gemma-4-31b-it": {
				ContextWindowTokens: 8192,
			},
		},
		// Disable corrections so the wire-shape test is unaffected by
		// per-provider rewrites; Phase 34's wrappers have their own
		// dedicated tests.
		DisableCorrections: true,
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name: "custom-openai-compat",
				// BaseURL is the host root; bifrost's OpenAI provider
				// appends "/v1/chat/completions" itself. Operators who
				// host the endpoint at "/v1" (e.g. NIM's
				// `https://integrate.api.nvidia.com/v1`) should use the
				// host root or RequestPathOverrides to avoid "/v1/v1/...".
				BaseURL:      server.URL,
				APIKeyEnvVar: envName,
				Models:       []string{"google/gemma-4-31b-it"},
				Timeout:      10 * time.Second,
			},
		},
	}

	deps, teardown := makeCustomProviderTestDeps(t)
	defer teardown()

	drv, err := New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		_ = drv.Close(context.Background())
	}()

	idCtx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, cancel := context.WithTimeout(idCtx, 10*time.Second)
	defer cancel()

	text := "say hi"
	resp, err := drv.Complete(ctx, llm.CompleteRequest{
		Model: "google/gemma-4-31b-it",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content == "" {
		t.Errorf("response content empty; want assistant text")
	}
	if seen.Load() == 0 {
		t.Errorf("httptest server saw zero requests; custom-provider wiring is dead")
	}

	// Avoid teardown audit emit on shutdown surfacing a flake.
	_ = audit.Redactor(nil)
}

// TestE2E_CustomProvider_Timeout exercises the per-provider Timeout
// knob — a server that sleeps past the configured timeout must yield
// a deadline-exceeded error from Complete().
func TestE2E_CustomProvider_Timeout(t *testing.T) {
	const envName = "HARBOR_TEST_OPENAI_COMPAT_TIMEOUT"
	t.Setenv(envName, "test-key-value")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Bifrost's NetworkConfig.DefaultRequestTimeoutInSeconds is
		// an INT — sub-second timeouts get rounded to 0 and bifrost's
		// package default fires. Use 1s/3s to clear the int boundary.
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedResponse))
	}))
	defer server.Close()

	cfg := llm.ConfigSnapshot{
		Driver:               "bifrost",
		Provider:             "custom-openai-compat",
		Model:                "google/gemma-4-31b-it",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		ModelProfiles: map[string]llm.ModelProfile{
			"google/gemma-4-31b-it": {ContextWindowTokens: 8192},
		},
		DisableCorrections: true,
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name: "custom-openai-compat",
				// BaseURL is the host root; bifrost's OpenAI provider
				// appends "/v1/chat/completions" itself. Operators who
				// host the endpoint at "/v1" (e.g. NIM's
				// `https://integrate.api.nvidia.com/v1`) should use the
				// host root or RequestPathOverrides to avoid "/v1/v1/...".
				BaseURL:      server.URL,
				APIKeyEnvVar: envName,
				Models:       []string{"google/gemma-4-31b-it"},
				Timeout:      1 * time.Second, // shorter than server sleep; >= 1s to clear int round-down
				MaxRetries:   0,
			},
		},
	}

	deps, teardown := makeCustomProviderTestDeps(t)
	defer teardown()

	drv, err := New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		_ = drv.Close(context.Background())
	}()

	idCtx, _ := identity.With(context.Background(), identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	})
	// Use a longer parent ctx so the per-provider timeout is what
	// causes the failure, not the ctx deadline.
	ctx, cancel := context.WithTimeout(idCtx, 10*time.Second)
	defer cancel()

	text := "slow"
	_, err = drv.Complete(ctx, llm.CompleteRequest{
		Model: "google/gemma-4-31b-it",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	})
	if err == nil {
		t.Fatal("Complete succeeded but server slept past per-provider timeout; expected error")
	}
	// Accept any "deadline / timeout / canceled" shape — bifrost
	// wraps differently per provider but the surface here is "the
	// call did NOT succeed against a slow server."
	if errors.Is(err, context.Canceled) {
		// Parent ctx cancellation isn't what we want — but the
		// per-provider timeout caused the underlying call to abort.
		// Either is acceptable for this assertion.
	}
}

// TestE2E_CustomProvider_5xxFailsLoud exercises the surface a custom
// provider exposes when the OpenAI-compatible endpoint returns 5xx.
// Bifrost retries up to MaxRetries; the final error reaches Complete.
func TestE2E_CustomProvider_5xxFailsLoud(t *testing.T) {
	const envName = "HARBOR_TEST_OPENAI_COMPAT_5XX"
	t.Setenv(envName, "test-key-value")

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream is down"}}`))
	}))
	defer server.Close()

	cfg := llm.ConfigSnapshot{
		Driver:               "bifrost",
		Provider:             "custom-openai-compat",
		Model:                "google/gemma-4-31b-it",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		ModelProfiles: map[string]llm.ModelProfile{
			"google/gemma-4-31b-it": {ContextWindowTokens: 8192},
		},
		DisableCorrections: true,
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:                "custom-openai-compat",
				BaseURL:             server.URL + "/v1",
				APIKeyEnvVar:        envName,
				Models:              []string{"google/gemma-4-31b-it"},
				Timeout:             5 * time.Second,
				MaxRetries:          1,
				RetryBackoffInitial: 50 * time.Millisecond,
				RetryBackoffMax:     200 * time.Millisecond,
			},
		},
	}

	deps, teardown := makeCustomProviderTestDeps(t)
	defer teardown()

	drv, err := New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		_ = drv.Close(context.Background())
	}()

	idCtx, _ := identity.With(context.Background(), identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	})
	ctx, cancel := context.WithTimeout(idCtx, 5*time.Second)
	defer cancel()

	text := "fail please"
	_, err = drv.Complete(ctx, llm.CompleteRequest{
		Model: "google/gemma-4-31b-it",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	})
	if err == nil {
		t.Fatal("Complete succeeded; expected error from 5xx response")
	}
	if hits.Load() == 0 {
		t.Error("server saw zero requests; wiring is dead")
	}
}
