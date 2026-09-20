package corrections

import (
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestUsagePresence_BackfillDoesNotReplacePresentZero(t *testing.T) {
	t.Parallel()
	var response llm.CompleteResponse
	if err := json.Unmarshal([]byte(`{"Content":"answer","Usage":{"ReportPresent":true},"Cost":{"ReportPresent":true}}`), &response); err != nil {
		t.Fatal(err)
	}
	text := "a valid prompt"
	req := llm.CompleteRequest{Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}}
	got := backfillUsage(response, req, llm.ModelProfile{})
	if got.Usage.TotalTokens != 0 {
		t.Fatal("present zero report replaced by an estimate")
	}
}

func TestUsagePresence_BackfillLabelsEstimates(t *testing.T) {
	t.Parallel()
	text := "prompt"
	got := backfillUsage(llm.CompleteResponse{Content: "answer"}, llm.CompleteRequest{Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}}, llm.ModelProfile{})
	encoded, err := json.Marshal(got.Usage)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["Estimated"] != true || fields["ReportPresent"] == true {
		t.Fatalf("backfilled usage mislabeled: %s", encoded)
	}
}

func TestUsagePresence_CostBackfillRespectsReport(t *testing.T) {
	t.Parallel()
	for _, present := range []bool{false, true} {
		text := "prompt"
		got := backfillUsage(llm.CompleteResponse{Content: "answer", Cost: llm.Cost{ReportPresent: present}}, llm.CompleteRequest{Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}}, llm.ModelProfile{CostOverrides: &llm.CostTable{InputPer1M: 1, OutputPer1M: 2}})
		if !got.Usage.Estimated || got.Cost.ReportPresent != present || got.Cost.Estimated == present {
			t.Fatalf("wrong provenance with present=%t: %+v %+v", present, got.Usage, got.Cost)
		}
		if present && got.Cost.TotalCost != 0 {
			t.Fatal("present zero cost overwritten")
		}
	}
}
