package react_test

import (
	"context"
	"encoding/json"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
	"testing"
)

type collisionAgreementClient struct{}

func (collisionAgreementClient) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "c1", Name: "clock_now", Args: json.RawMessage(`{}`)}}}, nil
}
func (collisionAgreementClient) Close(context.Context) error { return nil }

// A residual sanitized-name collision (`clock.now` vs `clock_now`) must
// resolve to the SAME catalog tool on every path: tool_get's description,
// the native declaration the model calls, and declarative_action dispatch.
// Prompt-stability ordering reorders surviving declarations only; it never
// picks a different collision winner than catalog order.
func TestToolCollision_NativeAndDiscoveryMustAgree(t *testing.T) {
	cat := tools.NewCatalog()
	for _, tool := range []tools.Tool{
		{Name: "clock.now", Description: "shared read-only clock", Transport: tools.TransportHTTP, Source: "clock", Loading: tools.LoadingAlways},
		{Name: "clock_now", Description: "different builtin action", Transport: tools.TransportInProcess, Loading: tools.LoadingAlways},
	} {
		tool := tool
		if err := cat.Register(tools.ToolDescriptor{Tool: tool, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: tool.Name}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := builtin.RegisterWith(builtin.RegistryContext{Catalog: cat}, []string{"tool_get", "tool_search", "declarative_action"}); err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.WithRun(t.Context(), id, "r")
	if err != nil {
		t.Fatal(err)
	}
	view := tools.NewPlannerView(cat, tools.CatalogFilter{})
	ctx = tools.WithRunToolView(ctx, view)
	get, _ := cat.Resolve("tool_get")
	result, err := get.Invoke(ctx, json.RawMessage(`{"name":"clock_now"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result.Value)
	if err != nil {
		t.Fatal(err)
	}
	var described builtin.ToolGetOut
	if err := json.Unmarshal(raw, &described); err != nil {
		t.Fatal(err)
	}
	decision, err := react.New(collisionAgreementClient{}).Next(ctx, planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: "r"}, Goal: "read clock", Catalog: view})
	if err != nil {
		t.Fatal(err)
	}
	call, ok := decision.(planner.CallTool)
	if !ok {
		t.Fatalf("decision %T", decision)
	}
	declared, _ := view.Resolve(call.Tool)
	action, _ := cat.Resolve("declarative_action")
	escaped, err := action.Invoke(ctx, json.RawMessage(`{"tool":"clock_now","args":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tool_get description=%q; native target=%q description=%q; escape result=%+v", described.Description, call.Tool, declared.Description, escaped.Value)
	if described.Description != declared.Description {
		t.Errorf("same declared alias describes one tool and natively dispatches another")
	}
	escRaw, err := json.Marshal(escaped.Value)
	if err != nil {
		t.Fatal(err)
	}
	var esc builtin.DeclarativeActionOut
	if err := json.Unmarshal(escRaw, &esc); err != nil {
		t.Fatal(err)
	}
	var escTarget string
	if err := json.Unmarshal(esc.Observation, &escTarget); err != nil {
		t.Fatalf("declarative_action observation %s: %v", esc.Observation, err)
	}
	if escTarget != declared.Name {
		t.Errorf("declarative_action dispatched %q, native dispatch targets %q", escTarget, declared.Name)
	}
}
