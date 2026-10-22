package serve

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestRunLoopDriver_ClosedTaskToolFenceReachesPlanner(t *testing.T) {
	env := newFailDriverEnv(t)
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "write_document"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: "unused"}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	observed := make(chan struct{}, 1)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Catalog, o.Executor = cat, dispatch.NewToolExecutor(cat, nil, env.reg)
		o.Planner = &driverTestPlanner{finishGoalImmediately: true, onRunContext: func(rc planner.RunContext) {
			if rc.ToolAllowlist == nil || len(*rc.ToolAllowlist) != 0 {
				t.Errorf("closed task tool policy was not passed to planner: %v", rc.ToolAllowlist)
			}
			if _, ok := rc.Catalog.Resolve("write_document"); ok {
				t.Error("closed task planner resolved a registered tool")
			}
			observed <- struct{}{}
		}}
	})
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: runLoopDriverTestID}
	closed := []string{}
	h, err := env.reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground, Query: "discuss document", ToolAllowlist: &closed})
	if err != nil {
		t.Fatal(err)
	}
	if status := waitForTaskStatus(t, env.reg, h.ID, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
		t.Fatalf("status = %s", status)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("planner did not observe the closed task")
	}
}
