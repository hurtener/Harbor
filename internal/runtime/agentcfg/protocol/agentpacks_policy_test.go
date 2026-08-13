package protocol

import "testing"

func TestVerifyProposalPolicy_RejectsTamperingAndVersionDrift(t *testing.T) {
	policy := AgentPackAuthoringPolicy{ID: agentPackAuthoringPolicyID, Version: agentPackAuthoringPolicyVersion, Instructions: agentPackAuthoringInstructions, PermittedTools: []string{"tool"}}
	hash := canonicalPolicyHash(policy)
	for name, tampered := range map[string]AgentPackAuthoringPolicy{
		"instructions": {ID: policy.ID, Version: policy.Version, Instructions: "changed", PermittedTools: policy.PermittedTools},
		"capabilities": {ID: policy.ID, Version: policy.Version, Instructions: policy.Instructions, PermittedTools: []string{"other"}},
		"version":      {ID: policy.ID, Version: "old", Instructions: policy.Instructions, PermittedTools: policy.PermittedTools},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyProposalPolicy(tampered, policy, hash); err != ErrAgentPackProposalInvalid {
				t.Fatalf("error = %v, want invalid proposal", err)
			}
		})
	}
	if err := verifyProposalPolicy(policy, policy, "sha256:tampered"); err != ErrAgentPackProposalInvalid {
		t.Fatalf("tampered hash error = %v", err)
	}
	if err := verifyProposalPolicy(AgentPackAuthoringPolicy{}, policy, ""); err != ErrAgentPackProposalInvalid {
		t.Fatalf("legacy policy error = %v", err)
	}
}
