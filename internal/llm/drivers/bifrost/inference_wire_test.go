package bifrost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

// Dependency updates must not silently clamp a caller's output budget or drop
// its reasoning control. Inspect the actual request after Bifrost translation.
func TestDriver_OpenRouterPreservesExplicitInferenceControls(t *testing.T) {
	// Synthetic fixture credential; this test never contacts OpenRouter.
	t.Setenv("HARBOR_INFERENCE_WIRE_KEY", "synthetic-fixture-key")
	seen := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		seen <- request
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cannedResponse))
	}))
	defer server.Close()
	driver, err := New(llm.ConfigSnapshot{Provider: "openrouter", Model: "openai/gpt-5.6-terra", APIKey: "env.HARBOR_INFERENCE_WIRE_KEY", BaseURL: server.URL, Timeout: 5 * time.Second}, llm.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = driver.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(withIdentity(t, t.Context(), "inference-wire"), 5*time.Second)
	defer cancel()
	text, budget := "fixture request", 128000
	_, err = driver.Complete(ctx, llm.CompleteRequest{
		Model: "openai/gpt-5.6-terra", MaxTokens: &budget, ReasoningEffort: llm.ReasoningHigh,
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-seen:
		if string(request["max_completion_tokens"]) != "128000" {
			t.Errorf("max_completion_tokens = %s, want 128000", request["max_completion_tokens"])
		}
		// Bifrost's OpenAI-compatible request serializer emits the
		// top-level reasoning_effort field, including for OpenRouter.
		if string(request["reasoning_effort"]) != `"high"` {
			t.Errorf("reasoning_effort = %s, want high", request["reasoning_effort"])
		}
	case <-ctx.Done():
		t.Fatal("provider fixture received no request")
	}
}
