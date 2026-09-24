package bifrost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
)

// Exercise the real ReAct declaration/projector and pinned OpenRouter SSE
// decoder together. Similar long catalog keys must not turn a read-only show
// into create, even after earlier calls used the same stream index. Provider
// responses alone are scripted; this is not evidence of model proficiency.
func TestOpenRouter_StreamedToolNamesKeepDeclarationAndDispatch(t *testing.T) {
	// Synthetic fixture credential; traffic stays on the local HTTP server.
	t.Setenv("HARBOR_TOOL_DISPATCH_WIRE_KEY", "synthetic-fixture-key")
	const source = "pc-fixture-source~a-0123456789abcdef0123456789abcdef_workbench_"
	verbs := []string{"create", "show", "edit", "show", "create", "show"}
	schemas := map[string]string{
		"create": `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`,
		"show":   `{"type":"object","properties":{"path":{"type":"string"},"version":{"type":"integer"}},"required":["path"]}`,
		"edit":   `{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}},"required":["path","text"]}`,
	}
	args := map[string]string{
		"create": `{"title":"Synthetic fixture"}`,
		"show":   `{"path":"/me/fixture","version":9007199254740993}`,
		"edit":   `{"path":"/me/fixture","text":"unchanged exact source"}`,
	}
	catalog := tools.NewCatalog()
	for _, verb := range []string{"create", "edit", "show"} {
		if err := catalog.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: source + verb, Description: "Fixture " + verb, ArgsSchema: json.RawMessage(schemas[verb]), Loading: tools.LoadingAlways},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				t.Error("planning dispatched a tool")
				return tools.ToolResult{}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(requests.Add(1)) - 1
		if i >= len(verbs) {
			t.Error("unexpected extra provider request")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Function struct {
					Name        string          `json:"name"`
					Description string          `json:"description"`
					Parameters  json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if !request.Stream {
			t.Error("expected actual streaming request")
		}
		seen := make(map[string]bool)
		for _, declaration := range request.Tools {
			for verb, schema := range schemas {
				if declaration.Function.Name != tools.ModelVisibleToolName(source+verb) {
					continue
				}
				if seen[verb] || declaration.Function.Description != "Fixture "+verb {
					t.Errorf("duplicate or mismatched declaration for %s", verb)
				}
				seen[verb] = true
				var got, want any
				if err := json.Unmarshal(declaration.Function.Parameters, &got); err != nil {
					t.Error(err)
				}
				if err := json.Unmarshal([]byte(schema), &want); err != nil {
					t.Error(err)
				}
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("%s schema = %s, want %s", verb, gotJSON, wantJSON)
				}
			}
		}
		if len(seen) != len(schemas) {
			t.Errorf("distinct catalog declarations = %v", seen)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChunk := func(delta any, finish any) {
			payload := map[string]any{"id": fmt.Sprintf("fixture-%d", i), "object": "chat.completion.chunk", "created": 1700000000, "model": "openai/gpt-5.6-terra", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
			data, err := json.Marshal(payload)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		// Intentionally inconsistent narration: the structured tool name
		// selects the action, never the content/reasoning's claimed intention.
		writeChunk(map[string]any{"reasoning": "I will show the existing artifact.", "content": "Opening the saved artifact."}, nil)
		verb := verbs[i]
		writeChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", i), "type": "function", "function": map[string]any{"name": tools.ModelVisibleToolName(source + verb), "arguments": ""}}}}, nil)
		for _, fragment := range []string{args[verb][:7], args[verb][7:]} {
			writeChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": fragment}}}}, nil)
		}
		writeChunk(map[string]any{}, "tool_calls")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	driver, err := New(llm.ConfigSnapshot{Provider: "openrouter", Model: "openai/gpt-5.6-terra", APIKey: "env.HARBOR_TOOL_DISPATCH_WIRE_KEY", BaseURL: server.URL, Timeout: 5 * time.Second}, llm.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = driver.Close(context.Background()) }()
	p := react.New(driver)
	model, effort, output := "openai/gpt-5.6-terra", "high", 128000
	for i, verb := range verbs {
		q := identity.Quadruple{Identity: identity.Identity{TenantID: "fixture", UserID: "fixture", SessionID: "wire"}, RunID: fmt.Sprintf("run-%d", i)}
		ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
		if err != nil {
			t.Fatal(err)
		}
		var reasoning strings.Builder
		decision, err := p.Next(ctx, planner.RunContext{
			Quadruple: q, Query: "Show the saved artifact", Catalog: tools.NewPlannerView(catalog, tools.CatalogFilter{}),
			LLMOverrides: &planner.LLMOverrides{Model: &model, ReasoningEffort: &effort, MaxTokens: &output},
			OnChunk: func(delta string, _ bool, kind planner.ChunkKind) {
				if kind == planner.ChunkReasoning {
					reasoning.WriteString(delta)
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		call, ok := decision.(planner.CallTool)
		if !ok || call.Tool != source+verb || call.CallID != fmt.Sprintf("call-%d", i) || string(call.Args) != args[verb] {
			t.Fatalf("step %d decision = %#v; want exact %s name, ID and arguments", i, decision, verb)
		}
		if reasoning.String() != "I will show the existing artifact." {
			t.Fatalf("reasoning = %q", reasoning.String())
		}
	}
	if got := int(requests.Load()); got != len(verbs) {
		t.Fatalf("requests = %d, want %d", got, len(verbs))
	}
}
