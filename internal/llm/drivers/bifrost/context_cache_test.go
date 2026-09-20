package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
)

// Observe the actual pinned SDK's HTTP bodies, not a prompt-builder imitation.
// Stable prefixes are necessary for caching, not proof of a provider cache hit.
func TestContextCache_LivePrefixesAndExplicitRewriteBoundaries(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("HARBOR_CACHE_FIXTURE_KEY", "synthetic-key")
			var mu sync.Mutex
			var requests []map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/v1/chat/completions"
				if provider == "anthropic" {
					wantPath = "/v1/messages"
				}
				if r.URL.Path != wantPath {
					t.Errorf("unexpected native continuation/compaction endpoint: %s", r.URL.Path)
					http.Error(w, "unexpected endpoint", http.StatusBadRequest)
					return
				}
				body, err := io.ReadAll(io.LimitReader(r.Body, 128*1024))
				if err != nil {
					t.Error(err)
					return
				}
				var request map[string]json.RawMessage
				if err := json.Unmarshal(body, &request); err != nil {
					t.Error(err)
					return
				}
				for _, field := range []string{"previous_response_id", "context_management", "cache_control", "prompt_cache_key"} {
					if len(request[field]) > 0 {
						t.Errorf("cache-disabled portable request requires %s", field)
					}
				}
				content := "Fixture decision."
				if strings.Contains(string(body), "You summarize historical agent execution") {
					content = `{"goals":["edit"],"facts":["older exact reads are complete"],"pending":["verify edit"],"last_output_digest":"older work","note":"portable"}`
				} else {
					mu.Lock()
					requests = append(requests, request)
					mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				if provider == "anthropic" {
					err = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "type": "message", "role": "assistant", "model": "m", "content": []any{map[string]any{"type": "text", "text": content}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 100, "output_tokens": 10}})
				} else {
					err = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "model": "m", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}})
				}
				if err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			deps, cleanup := makeCustomProviderTestDeps(t)
			defer cleanup()
			output := 2048
			profile := llm.ModelProfile{ContextWindowTokens: 32768, DefaultMaxTokens: &output, OutputMode: llm.OutputModePrompted}
			client, err := llm.Open(t.Context(), llm.ConfigSnapshot{
				Driver: "bifrost", Provider: provider, Model: "m", APIKey: "env.HARBOR_CACHE_FIXTURE_KEY", BaseURL: server.URL,
				Timeout: 10 * time.Second, ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
				DisableCorrections: true, DisableRetry: true, DisableGovernance: true,
				ModelProfiles: map[string]llm.ModelProfile{"m": profile, "m2": profile},
			}, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			catalog := tools.NewCatalog()
			// Intentionally not alphabetical: the real catalog owns ordering.
			for _, name := range []string{"read", "edit"} {
				if err := catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: name, Description: "Synthetic fixture", ArgsSchema: json.RawMessage(`{"type":"object"}`), Loading: tools.LoadingAlways}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
					t.Error("request construction dispatched a historical action")
					return tools.ToolResult{}, nil
				}}); err != nil {
					t.Fatal(err)
				}
			}
			tr := &planner.Trajectory{Query: "edit"}
			appendExchange := func(id string) {
				tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: id, Args: json.RawMessage(`{"id":"doc"}`)}, LLMObservation: strings.Repeat("exact evidence ", 200) + id})
			}
			appendExchange("old-one")
			appendExchange("old-two")
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: provider}, RunID: "cache"}
			rc := planner.RunContext{Quadruple: q, Query: tr.Query, Trajectory: tr, Catalog: tools.NewPlannerView(catalog, tools.CatalogFilter{})}
			ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
			if err != nil {
				t.Fatal(err)
			}
			p := react.New(client, react.WithSystemPrompt("Stable operator guidance."))
			next := func() map[string]json.RawMessage {
				t.Helper()
				if _, err := p.Next(ctx, rc); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				defer mu.Unlock()
				return requests[len(requests)-1]
			}
			first := next()
			appendExchange("fresh-three")
			second := next()
			assertWirePrefix(t, first, second)
			repeated := next()
			before, _ := json.Marshal(second)
			after, _ := json.Marshal(repeated)
			if !bytes.Equal(before, after) {
				t.Fatal("rebuilding unchanged evidence changed the outbound request")
			}
			sum, err := summarizer.NewTrajectorySummariser(client)
			if err != nil {
				t.Fatal(err)
			}
			rc.Budget.TokenBudget = 1000
			if err := planner.NewCompressionRunner(sum).MaybeCompress(ctx, rc, tr); err != nil {
				t.Fatal(err)
			}
			compacted := next()
			if bytes.Equal(second["messages"], compacted["messages"]) || !bytes.Contains(compacted["messages"], []byte("older exact reads are complete")) || !bytes.Contains(compacted["messages"], []byte("fresh-three")) {
				t.Fatal("compaction boundary failed to rewrite only covered history")
			}
			appendExchange("fresh-four")
			assertWirePrefix(t, compacted, next())
			rc.Catalog = tools.NewExclusionView(rc.Catalog, nil, []string{"edit"})
			revoked := next()
			if bytes.Equal(compacted["tools"], revoked["tools"]) || bytes.Contains(revoked["tools"], []byte(`"edit"`)) {
				t.Fatal("cache stability preserved revoked tool authority")
			}
			model := "m2"
			rc.LLMOverrides = &planner.LLMOverrides{Model: &model}
			if got := string(next()["model"]); got != `"m2"` {
				t.Fatalf("model-switch boundary was not applied: %s", got)
			}
		})
	}
}

func assertWirePrefix(t *testing.T, before, after map[string]json.RawMessage) {
	t.Helper()
	var oldMessages, newMessages []json.RawMessage
	if err := json.Unmarshal(before["messages"], &oldMessages); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after["messages"], &newMessages); err != nil {
		t.Fatal(err)
	}
	if len(newMessages) <= len(oldMessages) {
		t.Fatal("fixture did not append a complete exchange")
	}
	for i := range oldMessages {
		if !bytes.Equal(oldMessages[i], newMessages[i]) {
			t.Fatalf("outbound prefix changed at message %d", i)
		}
	}
	for _, field := range []string{"tools", "system", "model", "max_tokens"} {
		if !bytes.Equal(before[field], after[field]) {
			t.Fatalf("stable field %s changed", field)
		}
	}
}
