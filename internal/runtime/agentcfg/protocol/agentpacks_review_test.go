package protocol

import (
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

func TestAgentPackProposalRecord_RoundTripsExactBinding(t *testing.T) {
	record := agentPackProposalRecord{
		AgentID: "agent-a", ExpectedContentHash: "base-hash", ReviewedHash: "review-hash",
		Item: skills.AgentPackItem{Name: "playbook", Trigger: "trigger", Steps: []string{"step"}},
	}
	encoded, err := marshalProposal(record)
	if err != nil {
		t.Fatalf("marshal proposal: %v", err)
	}
	decoded, err := unmarshalProposal(encoded)
	if err != nil {
		t.Fatalf("unmarshal proposal: %v", err)
	}
	if decoded.AgentID != record.AgentID || decoded.ExpectedContentHash != record.ExpectedContentHash || decoded.ReviewedHash != record.ReviewedHash || decoded.Item.Name != record.Item.Name {
		t.Fatalf("proposal binding changed: got %+v", decoded)
	}
}
