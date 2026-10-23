package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
)

func TestExecutor_TrustedAgentInvocationAcrossShapesAndSessions(t *testing.T) {
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "inspect"}, Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		got, ok := appoperation.AgentInvocationFrom(ctx)
		want, present := tools.EffectiveAgentConfigFrom(ctx)
		if ok != present || got != want {
			t.Errorf("agent bleed: got %q want %q", got, want)
			return tools.ToolResult{}, fmt.Errorf("agent bleed")
		}
		return tools.ToolResult{Value: "ok"}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	exec := NewToolExecutor(cat, nil, nil)
	call := planner.CallTool{Tool: "inspect", CallID: "first"}
	second := planner.CallTool{Tool: "inspect", CallID: "second"}
	decisions := []planner.Decision{call, planner.CallParallel{Branches: []planner.CallTool{call, second}}, planner.Batch{Tools: []planner.CallTool{call, second}}}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			q := dispatchTestQuad(fmt.Sprintf("agent-run-%d", i))
			q.SessionID = fmt.Sprintf("session-%d", i)
			ctx := dispatchTestCtx(t, q)
			if i != 0 {
				ctx = tools.WithEffectiveAgentConfig(ctx, fmt.Sprintf("agent-%d", i))
			}
			for _, decision := range decisions {
				if _, _, err := exec.ExecuteDecision(ctx, dispatchRunContext(cat, q), decision); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}
