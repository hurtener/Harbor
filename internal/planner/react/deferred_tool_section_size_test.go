package react

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// largeMCPSchema is a realistic ~3 KB MCP input schema: ten described
// properties, roughly the per-tool average of a large attached catalog.
func largeMCPSchema(tool string) json.RawMessage {
	props := make(map[string]any, 10)
	for i := range 10 {
		props[fmt.Sprintf("field_%d", i)] = map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Parameter %d of %s. ", i, tool) + strings.Repeat("Explains accepted values, defaults and constraints. ", 4),
		}
	}
	raw, err := json.Marshal(map[string]any{"type": "object", "properties": props, "required": []string{"field_0"}})
	if err != nil {
		panic(err)
	}
	return raw
}

// TestDeferredMCPDefault_ShrinksPerCallToolSection measures the per-call
// tool section for a 343-tool MCP catalog (the size of a production
// catalog that motivated `tools.mcp_default_loading_mode`) loaded "always"
// versus "deferred". Deferred keeps only the always-loaded built-ins
// declared; the deferred tools stay listed by name in <deferred_tools>.
func TestDeferredMCPDefault_ShrinksPerCallToolSection(t *testing.T) {
	const sources, perSource = 7, 49 // 343 tools
	measure := func(mode tools.LoadingMode) (llm.RequestTokenSections, llm.CompleteRequest) {
		cat := discoveryAliasCatalog(t) // real tool_search + tool_get built-ins
		var calls atomic.Int64
		for s := range sources {
			src := fmt.Sprintf("server%d", s)
			for i := range perSource {
				name := fmt.Sprintf("%s_operation_%02d", src, i)
				registerDiscoveryAlias(t, cat, tools.Tool{
					Name: name, Description: "Performs operation " + name + " against the connected service and returns structured results.",
					ArgsSchema: largeMCPSchema(name), Loading: mode, Source: tools.ToolSourceID(src),
					SourceTitle: "Service " + src, Transport: tools.TransportMCP,
				}, &calls)
			}
		}
		q := discoveryAliasIdentity()
		req := nextRequest(t, promptCacheCtx(t, q, "agent-1"), planner.RunContext{Quadruple: q, Goal: "g", Catalog: deferredIndexView(cat)})
		return llm.EstimateRequestTokenSections(req, llm.ModelProfile{}), req
	}
	always, alwaysReq := measure(tools.LoadingAlways)
	deferred, deferredReq := measure(tools.LoadingDeferred)
	t.Logf("343 MCP tools: always tools=%d tokens (%d declarations, total %d); deferred tools=%d tokens (%d declarations, total %d)",
		always.Tools, len(alwaysReq.Tools), always.Total(), deferred.Tools, len(deferredReq.Tools), deferred.Total())

	if len(alwaysReq.Tools) < sources*perSource {
		t.Fatalf("always mode declared %d tools, want >= %d", len(alwaysReq.Tools), sources*perSource)
	}
	if deferred.Tools*50 > always.Tools {
		t.Fatalf("deferred tool section %d tokens is not < 2%% of always %d", deferred.Tools, always.Tools)
	}
	if deferred.Total()*10 > always.Total() {
		t.Fatalf("deferred request %d tokens (incl. index) is not < 10%% of always %d", deferred.Total(), always.Total())
	}
	for _, want := range []string{"tool_search", "tool_get"} {
		if _, ok := declarationNamed(deferredReq.Tools, want); !ok {
			t.Fatalf("deferred request lacks always-loaded %s", want)
		}
	}
	index := deferredSection(systemText(t, deferredReq))
	if !strings.Contains(index, "Service server6 (49 tools): ") || strings.Contains(index, "more; use tool_search") {
		t.Fatalf("deferred index does not list every deferred tool by name:\n%s", index)
	}
}
