package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

type admittedAliasView struct{ target tools.Tool }

func (v admittedAliasView) Resolve(name string) (tools.Tool, bool) {
	return v.target, name == "legacy_echo"
}
func (v admittedAliasView) List() []tools.Tool { return []tools.Tool{v.target} }

func TestDispatch_UsesOnlySealedCanonicalResolution(t *testing.T) {
	cat := tools.NewCatalog()
	target := tools.Tool{Name: "physical_echo", Source: "physical"}
	if err := cat.Register(tools.ToolDescriptor{Tool: target, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatal(err)
	}
	executor := &toolExecutor{cat: cat}
	rc := planner.RunContext{Catalog: admittedAliasView{target: target}}
	desc, ok, err := executor.resolveForRun(context.Background(), rc, "legacy_echo")
	if err != nil || !ok || desc.Tool.Name != target.Name {
		t.Fatalf("sequential alias: %v %v %v", desc.Tool, ok, err)
	}
	resolver := executor.resolverForRun(context.Background(), rc)
	if desc, ok := resolver.Resolve("legacy_echo"); !ok || desc.Tool.Name != target.Name {
		t.Fatalf("parallel alias: %v %v", desc.Tool, ok)
	}
	if _, ok := resolver.Resolve("physical_echo"); ok {
		t.Fatal("global descriptor bypassed sealed view")
	}
}
