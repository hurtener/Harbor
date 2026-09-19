package bifrost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/output" // Register the real output wrapper without unrelated production drivers.
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// Both ends are the real pinned Bifrost adapters. Only provider responses are
// scripted. Ordinary JSON/text completion must suffice for portable compaction;
// switching adapters must retain exact native call/result pairs, not opaque state.
func TestPortableProviders_OrdinaryCompactionAndNativeReplay(t *testing.T) {
	for _, source := range []string{"openai", "anthropic"} {
		for _, truncated := range []bool{false, true} {
			t.Run(fmt.Sprintf("source=%s/truncated=%t", source, truncated), func(t *testing.T) {
				t.Setenv("HARBOR_PORTABLE_PROVIDER_KEY", "synthetic-test-key")
				target := "anthropic"
				if source == target {
					target = "openai"
				}
				var mu sync.Mutex
				seen := map[string][]json.RawMessage{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					provider := "openai"
					if r.URL.Path == "/v1/messages" {
						provider = "anthropic"
					} else if r.URL.Path != "/v1/chat/completions" {
						t.Errorf("unexpected provider-owned continuation/compaction endpoint: %s", r.URL.Path)
						http.Error(w, "invalid endpoint", http.StatusBadRequest)
						return
					}
					data, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
					if err != nil {
						t.Error(err)
						http.Error(w, "read failure", 400)
						return
					}
					var body map[string]json.RawMessage
					if err := json.Unmarshal(data, &body); err != nil {
						t.Error(err)
						http.Error(w, "invalid JSON", 400)
						return
					}
					for _, forbidden := range []string{"previous_response_id", "context_management", "cache_control"} {
						if len(body[forbidden]) != 0 {
							t.Errorf("portable path requires %s", forbidden)
						}
					}
					isSummary := strings.Contains(string(data), "You summarize historical agent execution")
					content, reason := "The current source is ready to edit.", "stop"
					if isSummary {
						if provider != source {
							t.Error("summary used an unselected provider")
						}
						if tools := string(body["tools"]); tools != "" && tools != "null" && tools != "[]" {
							t.Error("summary has executable tools")
						}
						if strings.Contains(string(body["response_format"]), "json_schema") || len(body["output_config"]) != 0 {
							t.Error("prompted compaction depends on native structured generation")
						}
						content = `{"goals":["edit document"],"facts":["retain approved navigation"],"pending":["apply exact edit"],"last_output_digest":"older checks complete","note":"portable narrative"}`
						if truncated {
							reason = "length"
						}
					} else if provider != target {
						t.Error("continuation did not switch provider")
					}
					mu.Lock()
					seen[provider] = append(seen[provider], append(json.RawMessage(nil), data...))
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if provider == "anthropic" {
						stop := "end_turn"
						if reason == "length" {
							stop = "max_tokens"
						}
						err = json.NewEncoder(w).Encode(map[string]any{"id": "msg-portable", "type": "message", "role": "assistant", "model": "portable-model", "content": []any{map[string]any{"type": "text", "text": content}}, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 100, "output_tokens": 60}})
					} else {
						err = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-portable", "object": "chat.completion", "created": 1700000000, "model": "portable-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": reason}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 60, "total_tokens": 160}})
					}
					if err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				open := func(provider string) llm.LLMClient {
					t.Helper()
					deps, cleanup := makeCustomProviderTestDeps(t)
					t.Cleanup(cleanup)
					output := 2048
					cfg := llm.ConfigSnapshot{Driver: "bifrost", Provider: provider, Model: "portable-model", APIKey: "env.HARBOR_PORTABLE_PROVIDER_KEY", BaseURL: server.URL, Timeout: 10 * time.Second, ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
						DisableCorrections: true, DisableRetry: true, DisableGovernance: true,
						ModelProfiles: map[string]llm.ModelProfile{"portable-model": {ContextWindowTokens: 32768, DefaultMaxTokens: &output, OutputMode: llm.OutputModePrompted}},
					}
					client, err := llm.Open(t.Context(), cfg, deps)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = client.Close(context.Background()) })
					return client
				}
				first := open(source)
				sum, err := summarizer.NewTrajectorySummariser(first)
				if err != nil {
					t.Fatal(err)
				}
				tr := &planner.Trajectory{Query: "Continue editing the same document"}
				for i := range 2 {
					tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: fmt.Sprint("old-", i), Args: json.RawMessage(`{}`)}, LLMObservation: strings.Repeat("older evidence ", 200)})
				}
				receipt := json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
				if len(receipt) != 14660 {
					t.Fatalf("fixture size=%d", len(receipt))
				}
				tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: "fresh", Args: json.RawMessage(`{"id":"doc-a"}`)}, LLMObservation: receipt})
				q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: source}, RunID: "portable"}
				rc := planner.RunContext{Quadruple: q, Query: tr.Query, Trajectory: tr, Budget: planner.Budget{TokenBudget: 1000}}
				ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
				if err != nil {
					t.Fatal(err)
				}
				err = planner.NewCompressionRunner(sum).MaybeCompress(ctx, rc, tr)
				if truncated {
					if !errors.Is(err, summarizer.ErrTrajectorySummaryIncomplete) || tr.Summary != nil {
						t.Fatalf("truncated checkpoint accepted: %v", err)
					}
					mu.Lock()
					defer mu.Unlock()
					if len(seen[target]) != 0 {
						t.Fatal("continued after incomplete checkpoint")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if tr.Summary == nil || tr.Summary.Coverage == nil || tr.Summary.Coverage.ThroughStep != 2 {
					t.Fatal("summary did not retain fresh exchange")
				}
				second := open(target)
				if _, err := react.New(second).Next(ctx, rc); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				requests := append([]json.RawMessage(nil), seen[target]...)
				sourceRequests := append([]json.RawMessage(nil), seen[source]...)
				mu.Unlock()
				if len(requests) != 1 || len(sourceRequests) != 1 {
					t.Fatalf("unexpected continuation requests source=%d target=%d", len(sourceRequests), len(requests))
				}
				if strings.Contains(string(sourceRequests[0]), "9007199254740993") {
					t.Fatal("fresh result was summarized before exposure")
				}
				if !strings.Contains(string(requests[0]), "retain approved navigation") {
					t.Fatal("portable narrative missing after provider switch")
				}
				var body struct {
					Messages []struct {
						Role       string          `json:"role"`
						Content    json.RawMessage `json:"content"`
						ToolCallID string          `json:"tool_call_id"`
						ToolCalls  []struct {
							ID string `json:"id"`
						} `json:"tool_calls"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(requests[0], &body); err != nil {
					t.Fatal(err)
				}
				call, result := false, false
				for _, msg := range body.Messages {
					if target == "openai" {
						for _, c := range msg.ToolCalls {
							if c.ID == "fresh" && msg.Role == "assistant" {
								call = true
							}
						}
						var text string
						if msg.Role == "tool" && msg.ToolCallID == "fresh" && json.Unmarshal(msg.Content, &text) == nil && strings.Contains(text, string(receipt)) {
							result = true
						}
					} else if len(msg.Content) > 0 && msg.Content[0] == '[' {
						var blocks []struct {
							Type      string          `json:"type"`
							ID        string          `json:"id"`
							ToolUseID string          `json:"tool_use_id"`
							Content   json.RawMessage `json:"content"`
						}
						if err := json.Unmarshal(msg.Content, &blocks); err != nil {
							t.Fatal(err)
						}
						for _, b := range blocks {
							if b.Type == "tool_use" && b.ID == "fresh" && msg.Role == "assistant" {
								call = true
							}
							if b.Type == "tool_result" && b.ToolUseID == "fresh" && msg.Role == "user" {
								var text string
								if json.Unmarshal(b.Content, &text) == nil {
									result = strings.Contains(text, string(receipt))
								} else {
									var parts []struct {
										Text string `json:"text"`
									}
									if err := json.Unmarshal(b.Content, &parts); err != nil {
										t.Fatal(err)
									}
									for _, p := range parts {
										if strings.Contains(p.Text, string(receipt)) {
											result = true
										}
									}
								}
							}
						}
					}
				}
				if !call || !result {
					t.Fatalf("provider switch broke exact native exchange: call=%t result=%t", call, result)
				}
			})
		}
	}
}
