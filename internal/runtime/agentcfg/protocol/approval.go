package protocol

import (
	"errors"

	"github.com/hurtener/Harbor/internal/agentcfg"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ErrDynamicApprovalRulesUnavailable rejects rules this runtime cannot enforce.
var ErrDynamicApprovalRulesUnavailable = errors.New("agentcfg/protocol: dynamic approval rules are not wired on this runtime")

// WithDynamicApprovalRules enables approval rule writes when the run driver can
// enforce them.
func WithDynamicApprovalRules(available bool) Option {
	return func(s *Service) { s.dynamicApprovalRulesAvailable = available }
}

func approvalToDomain(a *prototypes.AgentConfigApproval) *agentcfg.ApprovalSection {
	// The wire clear operation is an explicit empty rules array. An absent
	// or null rules field leaves the active section in place.
	if a == nil || a.Rules == nil {
		return nil
	}
	out := &agentcfg.ApprovalSection{}
	for _, rule := range a.Rules {
		out.Rules = append(out.Rules, agentcfg.ApprovalRule{
			Tools:       append([]string(nil), rule.Tools...),
			Sources:     append([]string(nil), rule.Sources...),
			Annotations: append([]string(nil), rule.Annotations...),
			Approval: agentcfg.ApprovalPolicy{
				Policy:      rule.Approval.Policy,
				Reason:      rule.Approval.Reason,
				RequireTags: append([]string(nil), rule.Approval.RequireTags...),
			},
		})
	}
	return out
}

func approvalToWire(a *agentcfg.ApprovalSection) *prototypes.AgentConfigApproval {
	if a == nil {
		return nil
	}
	out := &prototypes.AgentConfigApproval{}
	for _, rule := range a.Rules {
		out.Rules = append(out.Rules, prototypes.AgentConfigApprovalRule{
			Tools:       append([]string(nil), rule.Tools...),
			Sources:     append([]string(nil), rule.Sources...),
			Annotations: append([]string(nil), rule.Annotations...),
			Approval: prototypes.AgentConfigApprovalPolicy{
				Policy:      rule.Approval.Policy,
				Reason:      rule.Approval.Reason,
				RequireTags: append([]string(nil), rule.Approval.RequireTags...),
			},
		})
	}
	return out
}
