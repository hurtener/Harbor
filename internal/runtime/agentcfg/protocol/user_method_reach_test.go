package protocol_test

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func TestUserSignedOAuthMCPCapability_MethodReachNeedsOnlyUserVerb(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, key, registry, _, _ := signedCapabilityServiceWithRegistry(t, now)
	id := identity.Identity{TenantID: "t", UserID: "user-method-reach", SessionID: "session-a"}
	wireID := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	request := signedCapabilityRequestFor(t, key, now, wireID, testAgentID,
		"jti-user-method-reach", "aud", "user-method-cap")
	base := verifiedUserCapabilityContext(t, id, true, true)
	registerCtx := auth.WithMethodReach(base, []methods.Method{methods.MethodAgentConfigUserRegisterOAuthMCPCapability})
	registered, err := svc.RegisterUserOAuthMCPCapability(registerCtx, userRegisterRequest(request))
	if err != nil {
		t.Fatalf("user registration must not require its admin sibling method: %v", err)
	}
	removeCtx := auth.WithMethodReach(base, []methods.Method{methods.MethodAgentConfigUserRemoveOAuthMCPCapability})
	if _, err := svc.RemoveUserOAuthMCPCapability(removeCtx, prototypes.AgentConfigUserRemoveOAuthMCPCapabilityRequest{
		Identity: wireID, AgentID: testAgentID, ProviderName: "provider",
		ExpectedContentHash: registered.Revision.ContentHash,
	}); err != nil {
		t.Fatalf("user removal must not require its admin sibling method: %v", err)
	}
	if _, set, err := registry.Active(base, identity.Quadruple{Identity: id}, testAgentID, agentcfg.ConfigScopeAgent); err != nil || set {
		t.Fatalf("user-only method authority changed shared agent scope: set=%v err=%v", set, err)
	}
}
