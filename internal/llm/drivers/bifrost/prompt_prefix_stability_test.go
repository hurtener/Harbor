package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
)

const prefixFixtureResponse = `{"id":"fx","object":"chat.completion","created":1700000000,"model":"openai/gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// providerOrder renders a captured chat request the way providers lay out
// the cached prefix: tool declarations first, then messages in order.
func providerOrder(t *testing.T, body []byte) []byte {
	t.Helper()
	var req struct {
		Tools    json.RawMessage   `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.Write(req.Tools)
	for _, m := range req.Messages {
		b.Write(m)
	}
	return b.Bytes()
}

func commonPrefix(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func fixtureSchema(fields ...string) json.RawMessage {
	props := make([]string, 0, len(fields))
	for _, f := range fields {
		props = append(props, fmt.Sprintf(`"%s":{"type":"string","description":"The %s value to use for this operation."}`, f, f))
	}
	return json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{` + strings.Join(props, ",") + `},"required":["` + fields[0] + `"]}`)
}

func registerFixtureTool(t *testing.T, cat tools.ToolCatalog, tool tools.Tool) {
	t.Helper()
	if err := cat.Register(tools.ToolDescriptor{Tool: tool, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
}

// TestPromptPrefixStability_RunAndTurns reproduces a multi-step run (tool_search ->
// discovered tool -> agent tool) plus two later turns in new sessions, one
// after a user-owned connection attached, and reports the byte-identical
// provider-order prefix between consecutive calls.
func TestPromptPrefixStability_RunAndTurns(t *testing.T) {
	t.Setenv("HARBOR_PREFIX_FIXTURE_KEY", "synthetic-fixture-key")
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		mu.Lock()
		bodies = append(bodies, buf.Bytes())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(prefixFixtureResponse))
	}))
	defer server.Close()
	driver, err := New(llm.ConfigSnapshot{Provider: "openrouter", Model: "openai/gpt-6-luna", APIKey: "env.HARBOR_PREFIX_FIXTURE_KEY", BaseURL: server.URL, Timeout: 5 * time.Second}, llm.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	cat := tools.NewCatalog()
	const agentSrc = "crm~a-00112233445566778899aabbccddeeff"
	const userSrc = "mail~u-ffeeddccbbaa99887766554433221100"
	for _, tool := range []tools.Tool{
		{Name: "tool_search", Description: "Search the tools available to this run by capability text.", ArgsSchema: fixtureSchema("query", "limit"), Transport: tools.TransportInProcess, Loading: tools.LoadingAlways},
		{Name: "artifact_fetch", Description: "Read a window of a stored result as text.", ArgsSchema: fixtureSchema("ref", "offset"), Transport: tools.TransportInProcess, Loading: tools.LoadingAlways},
		{Name: "memory_recall", Description: "Recall stored facts about the user.", ArgsSchema: fixtureSchema("query"), Transport: tools.TransportInProcess, Loading: tools.LoadingAlways},
		{Name: "docs_search", Description: "Search operator documentation.", ArgsSchema: fixtureSchema("query"), Transport: tools.TransportHTTP, Source: "docs", Loading: tools.LoadingAlways},
		{Name: agentSrc + "_lookup", Description: "Look up a CRM account by name or id.", ArgsSchema: fixtureSchema("account", "fields"), Transport: tools.TransportMCP, Source: agentSrc, Loading: tools.LoadingAlways},
		{Name: agentSrc + "_update", Description: "Update a CRM account field.", ArgsSchema: fixtureSchema("account", "field", "value"), Transport: tools.TransportMCP, Source: agentSrc, Loading: tools.LoadingAlways},
		{Name: agentSrc + "_export", Description: "Export CRM accounts to a file.", ArgsSchema: fixtureSchema("format"), Transport: tools.TransportMCP, Source: agentSrc, Loading: tools.LoadingDeferred},
	} {
		registerFixtureTool(t, cat, tool)
	}

	{
		// Representative catalog size: 40 more agent tools with
		// production-length descriptions and generator-annotated schemas.
		desc := "Retrieve records from the connected business system. Supports filtering by owner, status and date range, returns at most fifty rows per page with a continuation cursor, and never modifies data. Use it before any update to confirm the current values."
		for i := 0; i < 40; i++ {
			registerFixtureTool(t, cat, tools.Tool{Name: fmt.Sprintf("%s_op%02d", agentSrc, i), Description: desc,
				ArgsSchema: json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"filter":{"type":"object","$comment":"generated from the upstream OpenAPI document","properties":{"owner":{"type":"string","description":"Owner id."},"status":{"type":"string","enum":["open","closed"]}}},"cursor":{"type":"string","$comment":"opaque"}},"additionalProperties":false}`),
				Transport:  tools.TransportMCP, Source: agentSrc, Loading: tools.LoadingAlways})
		}
	}
	p := react.New(driver)
	next := func(session, run, goal string, steps []planner.Step) {
		t.Helper()
		q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: session}, RunID: run}
		ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
		if err != nil {
			t.Fatal(err)
		}
		ctx = tools.WithEffectiveAgentConfig(ctx, "general-assistant")
		model := "openai/gpt-6-luna"
		rc := planner.RunContext{Quadruple: q, Goal: goal, Catalog: tools.NewPlannerView(cat, tools.CatalogFilter{}), LLMOverrides: &planner.LLMOverrides{Model: &model}}
		if steps != nil {
			rc.Trajectory = &planner.Trajectory{Steps: steps}
		}
		if _, err := p.Next(ctx, rc); err != nil {
			t.Fatal(err)
		}
	}
	exportName := tools.ModelVisibleToolName(agentSrc + "_export")
	s1 := planner.Step{Action: planner.CallTool{Tool: "tool_search", Args: json.RawMessage(`{"query":"export accounts"}`), CallID: "call-1"},
		Observation: map[string]any{"tools": []any{map[string]any{"name": exportName, "description": "Export CRM accounts to a file."}}}}
	s2 := planner.Step{Action: planner.CallTool{Tool: agentSrc + "_lookup", Args: json.RawMessage(`{"account":"acme"}`), CallID: "call-2"},
		Observation: map[string]any{"account": "acme", "tier": "gold", "owner": "pat"}}
	s3 := planner.Step{Action: planner.CallTool{Tool: agentSrc + "_export", Args: json.RawMessage(`{"format":"csv"}`), CallID: "call-3"},
		Observation: map[string]any{"ref": "abc123", "rows": 42}}

	// Turn 1: four planner calls of one run.
	next("sess-1", "run-1", "Export the gold accounts", nil)
	next("sess-1", "run-1", "Export the gold accounts", []planner.Step{s1})
	next("sess-1", "run-1", "Export the gold accounts", []planner.Step{s1, s2})
	next("sess-1", "run-1", "Export the gold accounts", []planner.Step{s1, s2, s3})
	// Turn 2: new session, same catalog ("hola").
	next("sess-2", "run-2", "hola", nil)
	// The user's own connection attaches lazily.
	registerFixtureTool(t, cat, tools.Tool{Name: userSrc + "_read", Description: "Read the user's mailbox.", ArgsSchema: fixtureSchema("folder"), Transport: tools.TransportMCP, Source: userSrc, Loading: tools.LoadingAlways})
	// Turn 3: new session after the attach; turn 4: steady state.
	next("sess-3", "run-3", "hola", nil)
	next("sess-4", "run-4", "hola", nil)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 7 {
		t.Fatalf("captured %d requests, want 7", len(bodies))
	}
	r := make([][]byte, len(bodies))
	for i, body := range bodies {
		r[i] = providerOrder(t, body)
	}
	toolsLen := make([]int, 0, len(bodies))
	for _, body := range bodies {
		var req struct {
			Tools json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		toolsLen = append(toolsLen, len(req.Tools))
	}
	tierPrefix := func(call int) int {
		// Byte length of the declarations through the last non-user-owned
		// tool: the stable part a user's connection must never disturb.
		var req struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(bodies[call], &req); err != nil {
			t.Fatal(err)
		}
		n := 1 // '['
		for i, d := range req.Tools {
			if strings.Contains(string(d), "_u-") {
				break
			}
			if i > 0 {
				n++ // ','
			}
			n += len(d)
		}
		return n
	}
	check := func(label string, a, b, atLeast int) {
		t.Helper()
		cp := commonPrefix(r[a], r[b])
		t.Logf("%-38s prefix=%6d of %6d bytes (%.1f%%)", label, cp, len(r[a]), 100*float64(cp)/float64(len(r[a])))
		if cp < atLeast {
			t.Errorf("%s: shared prefix %d bytes, want >= %d", label, cp, atLeast)
		}
	}
	// Within one run every call is a byte prefix of the next one, except
	// where tool_search discovers a tool: the declaration is appended after
	// every existing declaration (never reordering them), so the prefix is
	// the whole earlier tool array. This is the v1.36.0 baseline shape.
	check("run step0->step1 (tool_search)", 0, 1, toolsLen[0]-1)
	check("run step1->step2 (discovered tool)", 1, 2, len(r[1]))
	check("run step2->step3", 2, 3, len(r[2]))
	// A new session's first call shares everything up to the user's goal.
	check("turn1 step0 -> turn2 (new session)", 0, 4, len(r[0])-64)
	// A user-owned connection attaching changes only the tail of the
	// declarations: every builtin/shared/agent declaration keeps its bytes
	// and position (v1.36.0 inserted it alphabetically, mid-array).
	check("turn2 -> turn3 (user tool attached)", 4, 5, tierPrefix(5)-1)
	check("turn3 -> turn4 (steady)", 5, 6, len(r[5]))

	// One prompt cache key for the whole agent: constant across the run's
	// steps (discoveries excluded), across sessions, and across a user's
	// connection attaching (user-owned tools excluded).
	var keys []string
	for _, body := range bodies {
		var req struct {
			Key string `json:"prompt_cache_key"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, req.Key)
	}
	for i, k := range keys {
		if k == "" || k != keys[0] {
			t.Errorf("call %d prompt_cache_key = %q, want the run's constant key %q", i, k, keys[0])
		}
	}
}
