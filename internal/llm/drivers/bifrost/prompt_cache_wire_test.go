package bifrost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// promptCacheWireRequest drives one real driver call against a local
// provider fixture and returns the decoded request body bifrost sent.
func promptCacheWireRequest(t *testing.T, provider, model string, settings llm.PromptCacheSettings, req llm.CompleteRequest) map[string]json.RawMessage {
	t.Helper()
	// Synthetic fixture credential; traffic stays on the local HTTP server.
	t.Setenv("HARBOR_PROMPT_CACHE_WIRE_KEY", "synthetic-fixture-key")
	seen := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cannedResponse))
	}))
	defer server.Close()
	driver, err := New(llm.ConfigSnapshot{
		Provider: provider, Model: model, APIKey: "env.HARBOR_PROMPT_CACHE_WIRE_KEY",
		BaseURL: server.URL, Timeout: 5 * time.Second, PromptCache: settings,
	}, llm.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = driver.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(withIdentity(t, t.Context(), "prompt-cache-wire"), 5*time.Second)
	defer cancel()
	text := "hola"
	req.Model = model
	req.Messages = []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}
	if _, err := driver.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-seen:
		return body
	case <-ctx.Done():
		t.Fatal("provider fixture received no request")
		return nil
	}
}

func jsonString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("not a JSON string: %s", raw)
	}
	return s
}

func TestDriver_OpenAISendsTypedPromptCacheKeyAndRetention(t *testing.T) {
	const scope = "scope-digest"
	body := promptCacheWireRequest(t, "openai", "gpt-5.6-terra", llm.PromptCacheSettings{Retention: llm.PromptCacheRetention24h},
		llm.CompleteRequest{PromptCacheScope: scope})
	if got, want := jsonString(t, body["prompt_cache_key"]), llm.PromptCacheKey(scope, "openai", "gpt-5.6-terra"); got != want {
		t.Errorf("prompt_cache_key = %q, want %q", got, want)
	}
	if got := jsonString(t, body["prompt_cache_retention"]); got != "24h" {
		t.Errorf("prompt_cache_retention = %q, want 24h", got)
	}
	if _, ok := body["session_id"]; ok {
		t.Error("a native provider received a router session_id")
	}
}

func TestDriver_PromptCacheKeyAbsentWithoutScopeOrWhenDisabled(t *testing.T) {
	for name, tc := range map[string]struct {
		scope    string
		settings llm.PromptCacheSettings
	}{
		"no scope": {},
		"disabled": {scope: "scope-digest", settings: llm.PromptCacheSettings{Disabled: true}},
	} {
		t.Run(name, func(t *testing.T) {
			body := promptCacheWireRequest(t, "openai", "gpt-5.6-terra", tc.settings, llm.CompleteRequest{PromptCacheScope: tc.scope})
			if _, ok := body["prompt_cache_key"]; ok {
				t.Errorf("prompt_cache_key sent: %s", body["prompt_cache_key"])
			}
		})
	}
}

// OpenRouter's bifrost adapter strips the typed cache fields, so the key
// travels as a passthrough body field; session affinity and provider
// pinning are opt-in.
func TestDriver_OpenRouterSendsPromptCacheKeyAndOptInRouting(t *testing.T) {
	const scope, model = "scope-digest", "openai/gpt-6-luna"
	key := llm.PromptCacheKey(scope, "openrouter", model)

	t.Run("default", func(t *testing.T) {
		body := promptCacheWireRequest(t, "openrouter", model, llm.PromptCacheSettings{Retention: llm.PromptCacheRetentionInMemory}, llm.CompleteRequest{PromptCacheScope: scope})
		if got := jsonString(t, body["prompt_cache_key"]); got != key {
			t.Errorf("prompt_cache_key = %q, want %q", got, key)
		}
		for _, absent := range []string{"session_id", "provider", "prompt_cache_retention"} {
			if _, ok := body[absent]; ok {
				t.Errorf("%s sent without opt-in: %s", absent, body[absent])
			}
		}
	})
	t.Run("sticky and pinned", func(t *testing.T) {
		allow := false
		body := promptCacheWireRequest(t, "openrouter", model, llm.PromptCacheSettings{
			StickyRouting: true, ProviderOrder: []string{"openai", "azure"}, AllowFallbacks: &allow,
		}, llm.CompleteRequest{PromptCacheScope: scope})
		if got := jsonString(t, body["session_id"]); got != key {
			t.Errorf("session_id = %q, want the cache key", got)
		}
		var routing struct {
			Order          []string `json:"order"`
			AllowFallbacks *bool    `json:"allow_fallbacks"`
		}
		if err := json.Unmarshal(body["provider"], &routing); err != nil {
			t.Fatalf("provider = %s: %v", body["provider"], err)
		}
		if strings.Join(routing.Order, ",") != "openai,azure" || routing.AllowFallbacks == nil || *routing.AllowFallbacks {
			t.Errorf("provider routing = %s", body["provider"])
		}
	})
}

// Tool schemas reach the provider without $schema / $comment noise.
func TestDriver_ToolSchemaAnnotationsStrippedOnWire(t *testing.T) {
	schema := json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"q":{"type":"string","$comment":"generated"},"opts":{"type":"object","$schema":"x","properties":{"n":{"type":"integer"}}}}}`)
	body := promptCacheWireRequest(t, "openrouter", "openai/gpt-6-luna", llm.PromptCacheSettings{}, llm.CompleteRequest{
		Tools: []llm.ToolDeclaration{{Name: "lookup", Description: "Look something up.", Schema: schema}},
	})
	tools := string(body["tools"])
	if tools == "" {
		t.Fatal("no tools on the wire")
	}
	for _, noise := range []string{"$schema", "$comment", "draft-07"} {
		if strings.Contains(tools, noise) {
			t.Errorf("tools carry %q: %s", noise, tools)
		}
	}
	if !strings.Contains(tools, `"n"`) {
		t.Errorf("nested schema content lost: %s", tools)
	}
}

func TestPromptCacheTargetFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider           bfschemas.ModelProvider
		custom, compatible bool
		want               promptCacheTarget
	}{
		{bfschemas.OpenAI, false, false, promptCacheTyped},
		{bfschemas.Anthropic, false, false, promptCacheTyped},
		{bfschemas.OpenRouter, false, false, promptCacheRouter},
		{"nim", true, false, promptCacheNone},
		{bfschemas.OpenAI, false, true, promptCacheNone},
	} {
		if got := promptCacheTargetFor(tc.provider, tc.custom, tc.compatible); got != tc.want {
			t.Errorf("%s custom=%v compatible=%v: %d, want %d", tc.provider, tc.custom, tc.compatible, got, tc.want)
		}
	}
}

func TestApplyPromptCache_NoTargetOrNothingToSendLeavesRequestUntouched(t *testing.T) {
	t.Parallel()
	req := &bfschemas.BifrostChatRequest{Provider: bfschemas.OpenAI, Model: "m"}
	if applyPromptCache(req, "scope", llm.PromptCacheSettings{}, promptCacheNone) || req.Params != nil {
		t.Error("an unknown-schema endpoint received prompt cache params")
	}
	router := &bfschemas.BifrostChatRequest{Provider: bfschemas.OpenRouter, Model: "m"}
	if applyPromptCache(router, "", llm.PromptCacheSettings{}, promptCacheRouter) || router.Params != nil {
		t.Error("router passthrough enabled with nothing to send")
	}
}
