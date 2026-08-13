package packproposer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

type proposalClient struct{ content string }

func (c proposalClient) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{Content: c.content}, nil
}
func (proposalClient) Close(context.Context) error { return nil }

func TestProposer_DraftRejectsAuthorityAndUnknownFields(t *testing.T) {
	for _, field := range []string{"origin", "origin_ref", "content_hash", "membership", "capabilities", "policy", "policy_hash", "provenance", "permissions", "unknown"} {
		t.Run(field, func(t *testing.T) {
			contentBytes, err := json.Marshal(map[string]any{
				"name":    "n",
				"trigger": "t",
				"steps":   []string{"s"},
				field:     nil,
			})
			if err != nil {
				t.Fatal(err)
			}
			content := string(contentBytes)
			p, err := New(proposalClient{content: content})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Draft(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, "agent", "model", "intent", protocol.AgentPackAuthoringPolicy{ProposerSchema: protocol.AgentPackAuthoringProposerSchema()})
			if err == nil {
				t.Fatal("accepted forbidden field")
			}
		})
	}
}

func TestProposer_DraftRejectsTrailingJSON(t *testing.T) {
	baseBytes, err := json.Marshal(map[string]any{
		"name":    "n",
		"trigger": "t",
		"steps":   []string{"s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	trailingJSON := string(baseBytes) + "{}"
	p, err := New(proposalClient{content: trailingJSON})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Draft(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, "agent", "model", "intent", protocol.AgentPackAuthoringPolicy{ProposerSchema: protocol.AgentPackAuthoringProposerSchema()})
	if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("error = %v, want trailing JSON rejection", err)
	}
}

var _ llm.LLMClient = proposalClient{}
