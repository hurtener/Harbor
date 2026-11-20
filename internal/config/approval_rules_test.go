package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestValidateTools_ApprovalRules pins the tools.approval_rules[]
// validator: a selector is mandatory, globs and annotation selectors
// are checked, and the approval block follows the entries[] rules.
func TestValidateTools_ApprovalRules(t *testing.T) {
	deny := config.ToolApprovalConfig{Policy: "deny-all"}
	cases := []struct {
		name    string
		rule    config.ToolApprovalRuleConfig
		wantSub string
	}{
		{name: "name glob passes", rule: config.ToolApprovalRuleConfig{Tools: []string{"crm_delete_*"}, Approval: deny}},
		{name: "source + annotation passes", rule: config.ToolApprovalRuleConfig{Sources: []string{"files"}, Annotations: []string{"not_read_only"}, Approval: deny}},
		{name: "tagged with tags passes", rule: config.ToolApprovalRuleConfig{Annotations: []string{"destructive"}, Approval: config.ToolApprovalConfig{Policy: "tagged", RequireTags: []string{"write"}}}},
		{name: "no selector rejected", rule: config.ToolApprovalRuleConfig{Approval: deny}, wantSub: "tools.approval_rules[0]"},
		{name: "malformed glob rejected", rule: config.ToolApprovalRuleConfig{Tools: []string{"crm_["}, Approval: deny}, wantSub: "tools.approval_rules[0].tools[0]"},
		{name: "empty source rejected", rule: config.ToolApprovalRuleConfig{Sources: []string{" "}, Approval: deny}, wantSub: "tools.approval_rules[0].sources[0]"},
		{name: "unknown annotation rejected", rule: config.ToolApprovalRuleConfig{Annotations: []string{"dangerous"}, Approval: deny}, wantSub: "tools.approval_rules[0].annotations[0]"},
		{name: "unknown policy rejected", rule: config.ToolApprovalRuleConfig{Tools: []string{"x_*"}, Approval: config.ToolApprovalConfig{Policy: "bogus"}}, wantSub: "tools.approval_rules[0].approval.policy"},
		{name: "tagged without tags rejected", rule: config.ToolApprovalRuleConfig{Tools: []string{"x_*"}, Approval: config.ToolApprovalConfig{Policy: "tagged"}}, wantSub: "tools.approval_rules[0].approval.require_tags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			cfg.Tools.ApprovalRules = []config.ToolApprovalRuleConfig{tc.rule}
			err := cfg.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("expected ok, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}
