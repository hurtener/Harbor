package serve

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

// TestSkillsDirectoryCapability_IncludesDeferredRunTools: the prompt skills
// directory sees the run's deferred tools (reachable through tool_search),
// matching the skill_* tools' capability, while tools outside the run view
// stay out.
func TestSkillsDirectoryCapability_IncludesDeferredRunTools(t *testing.T) {
	cat := tools.NewCatalog()
	for _, tl := range []tools.Tool{
		{Name: "always_tool"},
		{Name: "deferred_tool", Loading: tools.LoadingDeferred},
		{Name: "excluded_tool", Loading: tools.LoadingDeferred},
	} {
		if err := cat.Register(tools.ToolDescriptor{Tool: tl, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	view := tools.NewExclusionView(tools.NewPlannerView(cat, tools.CatalogFilter{}), nil, []string{"excluded_tool"})
	got := map[string]bool{}
	for _, n := range skillsDirectoryCapability(view).AllowedTools {
		got[n] = true
	}
	if !got["always_tool"] || !got["deferred_tool"] || got["excluded_tool"] {
		t.Fatalf("skills directory capability = %v, want always+deferred run tools only", got)
	}
}
