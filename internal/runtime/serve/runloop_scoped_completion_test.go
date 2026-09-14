package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

type completionSourceOwners map[tools.ToolSourceID]toolauth.Owner

func (m completionSourceOwners) OwnerOfSource(source tools.ToolSourceID) (toolauth.Owner, bool) {
	owner, ok := m[source]
	return owner, ok
}

func TestRunLoopDriver_CompletionCatalogConcurrentScopes(t *testing.T) {
	ctx := context.Background()
	reg := acTestRegistry(t)
	cat := tools.NewCatalog()
	owners := completionSourceOwners{}
	for _, tenant := range []string{"t-a", "t-b"} {
		for _, agent := range []string{"a", "b"} {
			q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "author", SessionID: "setup"}}
			_, err := reg.SetRevision(ctx, q, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
				Connections:  &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "capture", URL: "https://example.test/mcp"}}},
				ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"capture_ingest"}},
				Hooks:        &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: "capture_ingest"}},
			}, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			source := tools.ToolSourceID("capture~" + tenant + "-" + agent)
			owners[source] = toolauth.Owner{Tenant: tenant, Agent: agent}
			if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: string(source) + "_ingest", Source: source, Transport: tools.TransportMCP}, Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				q, _ := identity.QuadrupleFrom(ctx)
				if q.TenantID != tenant {
					return tools.ToolResult{}, fmt.Errorf("foreign tenant %s", q.TenantID)
				}
				return tools.ToolResult{Value: string(source)}, nil
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	d := &RunLoopDriver{agentConfig: reg, catalog: cat, connectionDetacher: owners}
	executor := dispatch.NewToolExecutor(cat, nil, nil)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant, agent := []string{"t-a", "t-b"}[i%2], []string{"a", "b"}[(i/2)%2]
			q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: fmt.Sprintf("user-%d", i%5), SessionID: fmt.Sprintf("session-%d", i)}, RunID: fmt.Sprintf("run-%d", i)}
			runCtx, err := identity.WithRun(ctx, q.Identity, q.RunID)
			if err != nil {
				t.Error(err)
				return
			}
			hook, err := d.projectRunCompletionHook(runCtx, agent, q)
			if err != nil {
				t.Error(err)
				return
			}
			if hook.Catalog == nil {
				t.Error("completion catalog absent")
				return
			}
			resolved, ok := hook.Catalog.Resolve(hook.Tool)
			if !ok || resolved.Name != "capture~"+tenant+"-"+agent+"_ingest" {
				t.Errorf("scope leak: %+v", resolved)
				return
			}
			_, _, err = executor.ExecuteDecision(steering.WithTrustedCompletionHook(runCtx), planner.RunContext{Quadruple: q, Catalog: hook.Catalog}, planner.CallTool{Tool: hook.Tool, Args: json.RawMessage(`{}`)})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
func (m completionSourceOwners) LogicalNameOfSource(source tools.ToolSourceID) (string, bool) {
	_, ok := m[source]
	return "capture", ok
}
func (m completionSourceOwners) AttachedSources(context.Context, toolauth.Owner) []string {
	return []string{"capture"}
}
func (m completionSourceOwners) Detach(context.Context, string, toolauth.Owner) error { return nil }

// Exercise the production run driver, real config store and terminal dispatcher.
// The configured logical sink must stay hidden from the planner yet resolve to
// this tenant's attached physical source at completion.
func TestRunLoopDriver_ScopedCompletionDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		wantCall     bool
	}{
		{"logical", "capture_ingest", true},
		{"foreign physical", "capture~foreign_ingest", false},
		{"missing", "missing_ingest", false},
		{"disabled hook", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newFailDriverEnv(t)
			ctx, err := identity.With(context.Background(), runLoopDriverTestID)
			if err != nil {
				t.Fatal(err)
			}
			q := identity.Quadruple{Identity: runLoopDriverTestID}
			reg := acTestRegistry(t)
			const agentID = "completion-agent"
			_, err = reg.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
				Connections:  &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "capture", URL: "https://example.test/mcp"}}},
				ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"capture_ingest"}},
				Hooks:        &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: tc.target, TimeoutMS: 1000}},
			}, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			owners := completionSourceOwners{
				"capture~own":     {Tenant: q.TenantID, Agent: agentID},
				"capture~foreign": {Tenant: "foreign-tenant", Agent: agentID},
			}
			cat := tools.NewCatalog()
			calls := make(chan string, 4)
			for source := range owners {
				name := string(source) + "_ingest"
				err := cat.Register(tools.ToolDescriptor{
					Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP, Loading: tools.LoadingDeferred},
					Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
						var payload steering.RunCompletionPayload
						if err := json.Unmarshal(args, &payload); err != nil {
							return tools.ToolResult{}, err
						}
						if payload.TenantID != q.TenantID || payload.UserID != q.UserID || payload.SessionID != q.SessionID || payload.Outcome != "goal" {
							t.Errorf("completion identity or outcome changed: %+v", payload)
						}
						calls <- name
						return tools.ToolResult{Value: "stored"}, nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			startFailDriver(t, env, func(o *RunLoopDriverOptions) {
				o.Catalog, o.Executor, o.AgentConfig, o.AgentConfigID = cat, dispatch.NewToolExecutor(cat, nil, env.reg), reg, agentID
				o.ConnectionDetacher = owners
				o.Planner = &driverTestPlanner{finishGoalImmediately: true, onRunContext: func(rc planner.RunContext) {
					for _, name := range []string{"capture_ingest", "capture~own_ingest", "capture~foreign_ingest"} {
						if _, ok := rc.Catalog.Resolve(name); ok {
							t.Errorf("planner resolves excluded/foreign sink %q", name)
						}
					}
				}}
			})
			h, err := env.reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground, Query: "completion probe"})
			if err != nil {
				t.Fatal(err)
			}
			if status := waitForTaskStatus(t, env.reg, h.ID, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
				t.Fatalf("status = %s", status)
			}
			select {
			case name := <-calls:
				if !tc.wantCall || name != "capture~own_ingest" {
					t.Fatalf("unexpected sink invocation %s", name)
				}
			case <-time.After(100 * time.Millisecond):
				if tc.wantCall {
					t.Fatal("logical completion sink was never dispatched")
				}
			}
		})
	}
}
