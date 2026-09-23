package summarizer_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

func TestTrajectoryHistorical_ContextReachesOrdinarySummarization(t *testing.T) {
	t.Parallel()
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatal(err)
	}
	history, err := planner.RetainStep(planner.Step{
		Action:         planner.CallTool{Tool: "read", CallID: "c1", Args: json.RawMessage(`{"id":"doc-a"}`)},
		LLMObservation: json.RawMessage(`{"source":"日本語\nexact source","version":9007199254740993,"more":false}`),
		ReasoningTrace: "PRIVATE",
	}, "previous-run", 0)
	if err != nil {
		t.Fatal(err)
	}
	tr := &planner.Trajectory{Query: "continue", Steps: []planner.Step{history}}
	if _, err := s.Summarise(context.Background(), trajRC("history"), tr); err != nil {
		t.Fatal(err)
	}
	payload := *client.seenCalls()[0].req.Messages[1].Content.Text
	if !strings.HasSuffix(payload, "\n") {
		t.Fatal("historical exchange lost its chronological line boundary")
	}
	for _, expected := range []string{`9007199254740993`, `"more":false`, `日本語\nexact source`, "call_tool"} {
		if !strings.Contains(payload, expected) {
			t.Fatalf("summary omitted %q", expected)
		}
	}
	if strings.Contains(payload, "PRIVATE") {
		t.Fatal("reasoning entered retained compaction")
	}
}
