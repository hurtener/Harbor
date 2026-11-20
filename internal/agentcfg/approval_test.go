package agentcfg_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

func TestNormalizePayload_ApprovalCopiesRuleSlicesAndPreservesOrder(t *testing.T) {
	payload := agentcfg.ConfigPayload{Approval: &agentcfg.ApprovalSection{Rules: []agentcfg.ApprovalRule{
		{
			Tools:       []string{"crm_*"},
			Sources:     []string{"crm"},
			Annotations: []string{"destructive"},
			Approval:    agentcfg.ApprovalPolicy{Policy: "tagged", Reason: "review writes", RequireTags: []string{"write"}},
		},
		{Tools: []string{"crm_*"}, Approval: agentcfg.ApprovalPolicy{Policy: "deny-all"}},
	}}}
	normalized := agentcfg.NormalizePayload(payload)
	originalHash, err := agentcfg.ContentHash(normalized)
	if err != nil {
		t.Fatal(err)
	}
	rule := &payload.Approval.Rules[0]
	rule.Tools[0] = "changed"
	rule.Sources[0] = "changed"
	rule.Annotations[0] = "changed"
	rule.Approval.RequireTags[0] = "changed"
	rule.Approval.Reason = "changed"
	copiedHash, err := agentcfg.ContentHash(normalized)
	if err != nil || copiedHash != originalHash {
		t.Fatalf("input mutation changed a normalized revision: %q, %v", copiedHash, err)
	}
	slices.Reverse(normalized.Approval.Rules)
	reversedHash, err := agentcfg.ContentHash(normalized)
	if err != nil || reversedHash == originalHash {
		t.Fatalf("changing first-match order must change the revision hash: %q, %v", reversedHash, err)
	}
}

func TestApprovalSection_RevisionJSON(t *testing.T) {
	section := agentcfg.ApprovalSection{Rules: []agentcfg.ApprovalRule{{
		Tools: []string{"crm_*"}, Sources: []string{"crm"}, Annotations: []string{"destructive"},
		Approval: agentcfg.ApprovalPolicy{Policy: "tagged", RequireTags: []string{"write"}},
	}}}
	data, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"rules":[{"tools":["crm_*"],"sources":["crm"],"annotations":["destructive"],"approval":{"policy":"tagged","require_tags":["write"]}}]}`
	if string(data) != want {
		t.Fatalf("revision JSON = %s, want %s", data, want)
	}
}
