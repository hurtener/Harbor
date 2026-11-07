package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

// TestExecutor_SeatsRunViewForDiscoveryMetaTools is the SECURITY regression
// for the dispatch seam: the executor seats the run's sealed view on the
// invocation ctx, so tool_search answers from that view — never from the
// process catalog holding another source's same-named tools — for both the
// single-call and parallel shapes.
func TestExecutor_SeatsRunViewForDiscoveryMetaTools(t *testing.T) {
	cat := tools.NewCatalog()
	if err := builtin.Register(cat, []string{"tool_search"}); err != nil {
		t.Fatalf("register tool_search: %v", err)
	}
	for _, src := range []tools.ToolSourceID{"mail~a-own", "mail~a-foreign"} {
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: string(src) + "_send", Description: "mailbox send", Source: src, Loading: tools.LoadingDeferred},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	q := dispatchTestQuad("run-view-seat")
	rc := dispatchRunContext(cat, q)
	// The production exclusion narrowing hides the foreign source from this run.
	rc.Catalog = tools.NewExclusionView(rc.Catalog, []string{"mail~a-foreign"}, nil)
	exec := NewToolExecutor(cat, newTestArtifactStore(t), nil)

	check := func(t *testing.T, obs any) {
		t.Helper()
		body, err := json.Marshal(obs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(body), "foreign") {
			t.Fatalf("tool_search disclosed a tool outside the run view: %s", body)
		}
		if !strings.Contains(string(body), "mail_a-own_send") {
			t.Fatalf("tool_search missed the run's own deferred tool: %s", body)
		}
	}
	args := json.RawMessage(`{"query":"mailbox"}`)
	raw, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), rc, planner.CallTool{Tool: "tool_search", Args: args})
	if err != nil {
		t.Fatalf("ExecuteDecision(CallTool): %v", err)
	}
	check(t, raw)
	raw, _, err = exec.ExecuteDecision(dispatchTestCtx(t, q), rc, planner.CallParallel{Branches: []planner.CallTool{
		{Tool: "tool_search", Args: args},
		{Tool: "tool_search", Args: args},
	}})
	if err != nil {
		t.Fatalf("ExecuteDecision(CallParallel): %v", err)
	}
	check(t, raw)
}
