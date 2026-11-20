package agentcfg

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/config"
)

const (
	maxApprovalRules = 64
	maxApprovalGlobs = 32
)

// ApprovalSection holds ordered rules configured for this agent. The
// runtime's boot approval policy applies independently of this section.
type ApprovalSection struct {
	Rules []ApprovalRule `json:"rules,omitempty"`
}

// ApprovalRule selects tools for an approval policy in an agent revision.
type ApprovalRule struct {
	Tools       []string       `json:"tools,omitempty"`
	Sources     []string       `json:"sources,omitempty"`
	Annotations []string       `json:"annotations,omitempty"`
	Approval    ApprovalPolicy `json:"approval"`
}

// ApprovalPolicy names the policy, its reason, and the tags it checks.
type ApprovalPolicy struct {
	Policy      string   `json:"policy"`
	Reason      string   `json:"reason,omitempty"`
	RequireTags []string `json:"require_tags,omitempty"`
}

// Clone copies the rules and their selectors and tags, preserving rule order
// and section presence.
func (a *ApprovalSection) Clone() *ApprovalSection {
	if a == nil {
		return nil
	}
	out := &ApprovalSection{}
	for _, rule := range a.Rules {
		rule.Tools = append([]string(nil), rule.Tools...)
		rule.Sources = append([]string(nil), rule.Sources...)
		rule.Annotations = append([]string(nil), rule.Annotations...)
		rule.Approval.RequireTags = append([]string(nil), rule.Approval.RequireTags...)
		out.Rules = append(out.Rules, rule)
	}
	return out
}

// ToolApprovalRules returns the rules in the shared catalog and validation
// format. The returned selector and tag slices share storage with the section.
func (a *ApprovalSection) ToolApprovalRules() []config.ToolApprovalRuleConfig {
	if a == nil || len(a.Rules) == 0 {
		return nil
	}
	rules := make([]config.ToolApprovalRuleConfig, len(a.Rules))
	for i, rule := range a.Rules {
		rules[i] = config.ToolApprovalRuleConfig{
			Tools:       rule.Tools,
			Sources:     rule.Sources,
			Annotations: rule.Annotations,
			Approval:    config.ToolApprovalConfig(rule.Approval),
		}
	}
	return rules
}

// Validate checks the scope, dynamic revision size limits, and the shared
// approval rule semantics.
func (a *ApprovalSection) Validate(scope ConfigScope) error {
	if a == nil {
		return nil
	}
	if scope != ConfigScopeAgent {
		return fmt.Errorf("%w: approval is available only on agent-scoped revisions", ErrInvalidPayload)
	}
	if len(a.Rules) > maxApprovalRules {
		return fmt.Errorf("%w: approval.rules allows at most %d rules", ErrInvalidPayload, maxApprovalRules)
	}
	for i, rule := range a.Rules {
		if len(rule.Tools) > maxApprovalGlobs {
			return fmt.Errorf("%w: approval.rules[%d].tools allows at most %d globs", ErrInvalidPayload, i, maxApprovalGlobs)
		}
		if len(rule.Sources) > maxApprovalGlobs {
			return fmt.Errorf("%w: approval.rules[%d].sources allows at most %d globs", ErrInvalidPayload, i, maxApprovalGlobs)
		}
	}
	if err := config.ValidateToolApprovalRules("approval.rules", a.ToolApprovalRules()); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPayload, err)
	}
	return nil
}
