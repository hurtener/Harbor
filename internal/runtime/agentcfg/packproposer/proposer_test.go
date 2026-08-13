package packproposer

import (
	"context"
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
	const base = `{"name":"n","trigger":"t","steps":["s"]}`
	for _, field := range []string{"origin", "origin_ref", "content_hash", "membership", "capabilities", "policy", "policy_hash", "provenance", "permissions", "unknown"} {
		t.Run(field, func(t *testing.T) {
			content := strings.TrimSuffix(base, "}") + `,"` + field + `":null}`
			p, err := New(proposalClient{content: content})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Draft(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, "agent", "model", "intent", protocol.AgentPackAuthoringPolicy{})
			if err == nil {
				t.Fatal("accepted forbidden field")
			}
		})
	}
}

func TestProposer_DraftRejectsTrailingJSON(t *testing.T) {
	const trailingJSON = `{"name":"n","trigger":"t","steps":["s"]}{}`
	p, err := New(proposalClient{content: trailingJSON})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Draft(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, "agent", "model", "intent", protocol.AgentPackAuthoringPolicy{})
	if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("error = %v, want trailing JSON rejection", err)
	}
}

var _ llm.LLMClient = proposalClient{}
