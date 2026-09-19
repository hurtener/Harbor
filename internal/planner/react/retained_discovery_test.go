package react

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestRetainedDiscovery_RevalidatesCurrentCatalog(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	register := func(name, description string, scopes []string) {
		t.Helper()
		err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: name, Description: description, AuthScopes: scopes, Loading: tools.LoadingDeferred, ArgsSchema: json.RawMessage(`{"type":"object","properties":{"version":{"type":"integer"}}}`)}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			t.Error("discovery dispatched a historical action")
			return tools.ToolResult{}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	register("read_doc", "CURRENT-SCHEMA", nil)
	register("revoked_tool", "PRIVATE-SCHEMA", []string{"restricted"})
	register("disabled_tool", "DISABLED-SCHEMA", nil)
	tr := &planner.Trajectory{}
	prior := planner.Step{Action: planner.CallTool{Tool: "tool_search"}, LLMObservation: json.RawMessage(`{"tools":[{"name":"read_doc","description":"STALE-SCHEMA"},{"name":"revoked_tool"},{"name":"disabled_tool"},{"name":"removed_tool"}]}`)}
	retained, err := planner.RetainStep(prior, "prior-run", 0)
	if err != nil {
		t.Fatal(err)
	}
	tr.Steps = append(tr.Steps, retained)
	names := deriveDiscoveredFromTrajectory(tr)
	if !containsDiscoveryName(names, "read_doc") {
		t.Fatalf("lost retained discovery: %v", names)
	}
	view := tools.NewExclusionView(tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"}), nil, []string{"disabled_tool"})
	decls := buildToolDeclarations(planner.RunContext{Catalog: view}, names)
	found := false
	for _, decl := range decls {
		if decl.Name == "read_doc" {
			found = true
			if decl.Description != "CURRENT-SCHEMA" {
				t.Fatal("persisted schema used instead of catalog")
			}
		}
		if strings.Contains(decl.Description, "PRIVATE") || strings.Contains(decl.Description, "DISABLED") || decl.Name == "removed_tool" {
			t.Fatal("historical discovery restored forbidden authority")
		}
	}
	if !found {
		t.Fatal("authorized deferred tool not declared")
	}
}

func TestRetainedDiscovery_ParallelAndBatch(t *testing.T) {
	t.Parallel()
	search := planner.CallTool{Tool: "tool_search", CallID: "search"}
	cases := []planner.Step{
		{Action: planner.CallParallel{Branches: []planner.CallTool{search, {Tool: "read_doc", CallID: "read"}}}, LLMObservation: planner.ParallelObservation{Branches: []planner.ParallelBranchObservation{{Index: 0, Value: json.RawMessage(`{"tools":[{"name":"parallel_found"}]}`)}, {Index: 1, Value: "done"}}}},
		{Action: planner.Batch{Tools: []planner.CallTool{search}}, LLMObservation: planner.BatchObservation{Tools: []planner.ParallelBranchObservation{{Index: 0, Value: json.RawMessage(`{"tools":[{"name":"batch_found"}]}`)}}}},
	}
	for index, step := range cases {
		retained, err := planner.RetainStep(step, "prior", index)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"parallel_found", "batch_found"}[index]
		for _, input := range []planner.Step{step, retained} {
			got := deriveDiscoveredFromTrajectory(&planner.Trajectory{Steps: []planner.Step{input}})
			if !containsDiscoveryName(got, want) {
				t.Fatalf("aggregate discovery missing: %v", got)
			}
		}
	}
}

func TestRetainedDiscovery_CanonicalInvokedName(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("source_", 12) + "read_document"
	retained, err := planner.RetainStep(planner.Step{Action: planner.CallTool{Tool: name}, LLMObservation: "done"}, "prior", 0)
	if err != nil {
		t.Fatal(err)
	}
	tr := &planner.Trajectory{Steps: []planner.Step{retained}}
	before, _ := tr.Serialize()
	want := deriveDiscoveredFromTrajectory(tr)
	if !containsDiscoveryName(want, name) {
		t.Fatal("canonical tool identity lost")
	}
	var wg sync.WaitGroup
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := deriveDiscoveredFromTrajectory(tr); !reflect.DeepEqual(want, got) {
				t.Error("unstable discovery")
			}
		}()
	}
	wg.Wait()
	after, _ := tr.Serialize()
	if string(before) != string(after) {
		t.Fatal("discovery mutated retained evidence")
	}
}

func containsDiscoveryName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func TestRetainedDiscovery_BoundedRecentSet(t *testing.T) {
	t.Parallel()
	tr := &planner.Trajectory{}
	for i := range maxRetainedDiscoveredTools + 3 {
		name := fmt.Sprintf("tool_%03d", i)
		step, err := planner.RetainStep(planner.Step{Action: planner.CallTool{Tool: name}, LLMObservation: "done"}, "prior", i)
		if err != nil {
			t.Fatal(err)
		}
		tr.Steps = append(tr.Steps, step)
	}
	names := deriveDiscoveredFromTrajectory(tr)
	if len(names) != maxRetainedDiscoveredTools || containsDiscoveryName(names, "tool_000") || !containsDiscoveryName(names, "tool_130") {
		t.Fatal("historical tool names are not a bounded recent set")
	}
	// A current discovery is appended after the bounded historical set.
	tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "tool_search"}, LLMObservation: json.RawMessage(`{"tools":[{"name":"current"}]}`)})
	if !containsDiscoveryName(deriveDiscoveredFromTrajectory(tr), "current") {
		t.Fatal("current discovery lost")
	}
}
