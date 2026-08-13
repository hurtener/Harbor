package protocol

import (
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

func TestAgentPackProposalRecord_RoundTripsExactBinding(t *testing.T) {
	record := agentPackProposalRecord{
		AgentID: "agent-a", ExpectedContentHash: "base-hash", ReviewedHash: "review-hash",
		Provenance: packProposedProvenance("agent-a", "review-hash"),
		Item:       skills.AgentPackItem{Name: "playbook", Trigger: "trigger", Steps: []string{"step"}},
	}
	encoded, err := marshalProposal(record)
	if err != nil {
		t.Fatalf("marshal proposal: %v", err)
	}
	decoded, err := unmarshalProposal(encoded)
	if err != nil {
		t.Fatalf("unmarshal proposal: %v", err)
	}
	if decoded.AgentID != record.AgentID || decoded.ExpectedContentHash != record.ExpectedContentHash || decoded.ReviewedHash != record.ReviewedHash || decoded.Provenance != record.Provenance || decoded.Item.Name != record.Item.Name {
		t.Fatalf("proposal binding changed: got %+v", decoded)
	}
}

func TestAgentPackProposalRecord_NormalizesLegacyEmptyProvenanceForReplay(t *testing.T) {
	record := agentPackProposalRecord{AgentID: "agent-a", ReviewedHash: "review-hash"}
	encoded, err := marshalProposal(record)
	if err != nil {
		t.Fatalf("marshal legacy proposal: %v", err)
	}
	decoded, err := unmarshalProposal(encoded)
	if err != nil {
		t.Fatalf("unmarshal legacy proposal: %v", err)
	}
	if decoded.Provenance != "" {
		t.Fatalf("legacy provenance unexpectedly present: %q", decoded.Provenance)
	}
	if err := normalizePackProposalProvenance(&decoded); err != nil {
		t.Fatalf("normalize legacy proposal: %v", err)
	}
	want := packProposedProvenance(record.AgentID, record.ReviewedHash)
	if decoded.Provenance != want {
		t.Fatalf("normalized provenance: got %q want %q", decoded.Provenance, want)
	}
	rewritten, err := marshalProposal(decoded)
	if err != nil {
		t.Fatalf("marshal normalized proposal: %v", err)
	}
	replayed, err := unmarshalProposal(rewritten)
	if err != nil {
		t.Fatalf("unmarshal normalized proposal: %v", err)
	}
	if replayed.Provenance != want {
		t.Fatalf("replayed provenance: got %q want %q", replayed.Provenance, want)
	}
}
