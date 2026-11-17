package protocol

import (
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

func TestSignedCapabilityExchangeBinding_AgentScopeRequiresTrustedScopeAndReplayKey(t *testing.T) {
	for _, tc := range []struct {
		name          string
		scope         agentcfg.ConfigScope
		user, session string
		want          bool
	}{
		{"agent", agentcfg.ConfigScopeAgent, "", "", true},
		{"user", agentcfg.ConfigScopeUser, "user", "session", false},
		{"user_empty_replay", agentcfg.ConfigScopeUser, "", "", false},
		{"agent_user_replay", agentcfg.ConfigScopeAgent, "user", "", false},
		{"agent_session_replay", agentcfg.ConfigScopeAgent, "", "session", false},
		{"invalid_scope", agentcfg.ConfigScope(255), "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := agentcfg.SignedOAuthMCPOperation{ReplayKey: agentcfg.SignedOAuthMCPReplayKey{UserID: tc.user, SessionID: tc.session}}
			binding := agentcfg.SignedOAuthMCPBinding{UserID: "service", SessionID: "registration-session"}
			got := signedCapabilityExchangeBinding(binding, tc.scope, "", "", op, nil)
			if got.AgentScoped != tc.want {
				t.Fatalf("AgentScoped = %t, want %t", got.AgentScoped, tc.want)
			}
		})
	}
}
