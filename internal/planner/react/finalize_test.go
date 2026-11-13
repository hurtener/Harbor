package react_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

type finalizeClient struct {
	req     llm.CompleteRequest
	content string
}

func (c *finalizeClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.req = req
	return llm.CompleteResponse{Content: c.content, ToolCalls: []llm.ToolCallStructured{{ID: "x", Name: "web_search"}}}, nil
}

func (*finalizeClient) Close(context.Context) error { return nil }

func finalizeRC() planner.RunContext {
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	rc := withDeclaredTools(rcWith(q, "compare the vendors", nil), "web_search")
	rc.Trajectory = &planner.Trajectory{Query: "compare the vendors", Steps: []planner.Step{
		{Action: planner.CallTool{Tool: "web_search"}, LLMObservation: "vendor A costs 10"},
	}}
	rc.PendingToolCalls = []planner.ToolCallDeferred{{Name: "web_search"}}
	rc.Control.Finalize = &planner.FinalizeRequest{Limit: planner.RunLimitMaxDuration, Instructions: "LIMIT-INSTRUCTION", MaxOutputTokens: 512}
	return rc
}

func TestFinalize_AnswersWithoutToolsAtRunLimit(t *testing.T) {
	client := &finalizeClient{content: "Searched vendor A; vendor B remains."}
	p := react.New(client)
	dec, err := p.Next(context.Background(), finalizeRC())
	if err != nil {
		t.Fatal(err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok || fin.Reason != planner.FinishLimitReached || fin.Payload != "Searched vendor A; vendor B remains." {
		t.Fatalf("decision = %#v; tool calls in the response must be ignored", dec)
	}
	if client.req.ToolChoice != "none" || len(client.req.Tools) == 0 || client.req.MaxTokens == nil || *client.req.MaxTokens != 512 {
		t.Fatalf("final request must keep declarations with tool choice none and a bounded output: choice=%q tools=%d max=%v", client.req.ToolChoice, len(client.req.Tools), client.req.MaxTokens)
	}
	last := client.req.Messages[len(client.req.Messages)-1]
	if last.Role != llm.RoleUser || last.Content.Text == nil || *last.Content.Text != "LIMIT-INSTRUCTION" {
		t.Fatalf("final instruction missing: %+v", last)
	}
}

func TestFinalize_EmptyAnswerIsInvalidDecision(t *testing.T) {
	p := react.New(&finalizeClient{content: "  "})
	if _, err := p.Next(context.Background(), finalizeRC()); !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("err=%v", err)
	}
}
