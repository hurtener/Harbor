package react_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

func TestPortableCompaction_FreshReceiptAndErrorReachNextRequest(t *testing.T) {
	t.Parallel()
	tr := &planner.Trajectory{Query: "edit the document"}
	for _, id := range []string{"first", "second"} {
		tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: id}, LLMObservation: strings.Repeat(id, 1000)})
	}
	receipt := map[string]any{"source": strings.Repeat("source", 2440), "resource_id": "doc-a", "version": 7, "more": false}
	tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: "fresh-read"}, LLMObservation: receipt})
	rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "fresh"}, Trajectory: tr, Query: tr.Query, Budget: planner.Budget{TokenBudget: 100}}
	r := planner.NewCompressionRunner(&staticSummariserIT{summary: &planner.TrajectorySummary{Facts: []string{"older work"}}})
	client := &capturingClient{response: llm.CompleteResponse{Content: "done"}}
	p := react.New(client)
	check := func(id, text string) {
		t.Helper()
		if _, err := p.Next(t.Context(), rc); err != nil {
			t.Fatal(err)
		}
		req := client.lastRequest()
		foundCall, foundResult := false, false
		for _, msg := range req.Messages {
			for _, call := range msg.ToolCalls {
				if call.ID == id {
					foundCall = true
				}
			}
			if msg.ToolCallID != nil && *msg.ToolCallID == id && msg.Content.Text != nil && strings.Contains(*msg.Content.Text, text) {
				foundResult = true
			}
		}
		if !foundCall || !foundResult {
			t.Fatalf("next request omitted call/result %s", id)
		}
	}
	if err := r.MaybeCompress(t.Context(), rc, tr); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(receipt)
	check("fresh-read", string(encoded))
	tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "edit", CallID: "conflict"}, Error: "version conflict; current version is 8"})
	if err := r.MaybeCompress(t.Context(), rc, tr); err != nil {
		t.Fatal(err)
	}
	check("conflict", "version conflict")
	tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: "after-summary"}, LLMObservation: "updated exact source"})
	check("after-summary", "updated exact source")
}
