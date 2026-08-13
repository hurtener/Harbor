package react

import (
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

func TestProjectResponse_ProgressSchemaHasNoTargetCoordinates(t *testing.T) {
	dec, err := projectResponse(llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "p-1", Name: TaskProgressToolName, Args: json.RawMessage(`{"fraction":0.25,"phase":"indexing","message":"started","tags":["a"]}`)}}}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("projectResponse: %v", err)
	}
	p, ok := dec.(planner.TaskProgress)
	if !ok || p.CallID != "p-1" || p.Phase != "indexing" {
		t.Fatalf("decision = %#v, want task progress", dec)
	}
}
