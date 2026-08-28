package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

func verifiedUserCapabilityContext(t *testing.T, id identity.Identity, withScope, withReach bool) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	if withScope {
		ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAgentConfigUser})
	}
	if withReach {
		ctx = auth.WithAgentReach(ctx, []string{testAgentID})
	}
	return ctx
}

func userRegisterRequest(req prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest) prototypes.AgentConfigUserRegisterOAuthMCPCapabilityRequest {
	return prototypes.AgentConfigUserRegisterOAuthMCPCapabilityRequest(req)
}

func TestUserSignedOAuthMCPCapability_TwoUsersHaveIndependentDesiredAndPhysicalOwners(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, key, reg, _, preparer := signedCapabilityServiceWithRegistry(t, now)
	const sharedJTI = "jti-user-shared"

	idA := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-a"}
	idB := identity.Identity{TenantID: "t", UserID: "user-b", SessionID: "session-b"}
	// The signed descriptor is intentionally identical for both users. The
	// owner-derived physical namespace, not a client-selected name, supplies
	// the live isolation boundary.
	const sharedDescriptorName = "shared-cap"
	reqA := signedCapabilityRequestFor(t, key, now, prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID}, testAgentID, sharedJTI, "aud", sharedDescriptorName)
	reqB := signedCapabilityRequestFor(t, key, now, prototypes.IdentityScope{Tenant: idB.TenantID, User: idB.UserID, Session: idB.SessionID}, testAgentID, sharedJTI, "aud", sharedDescriptorName)

	ctxA := verifiedUserCapabilityContext(t, idA, true, true)
	ctxB := verifiedUserCapabilityContext(t, idB, true, true)
	registeredA, err := svc.RegisterUserOAuthMCPCapability(ctxA, userRegisterRequest(reqA))
	if err != nil {
		t.Fatalf("register user A: %v", err)
	}
	preparer.mu.Lock()
	ownerA := preparer.lastReq.Owner
	preparer.mu.Unlock()
	if ownerA != (toolauth.Owner{Tenant: idA.TenantID, Agent: testAgentID, User: idA.UserID}) {
		t.Fatalf("user A physical owner = %+v", ownerA)
	}

	registeredB, err := svc.RegisterUserOAuthMCPCapability(ctxB, userRegisterRequest(reqB))
	if err != nil {
		t.Fatalf("register user B with shared JTI: %v", err)
	}
	preparer.mu.Lock()
	ownerB := preparer.lastReq.Owner
	preparer.mu.Unlock()
	if ownerB != (toolauth.Owner{Tenant: idB.TenantID, Agent: testAgentID, User: idB.UserID}) {
		t.Fatalf("user B physical owner = %+v", ownerB)
	}
	preparer.mu.Lock()
	_, liveA := preparer.userLive[idA.TenantID+"/"+testAgentID+"/"+idA.UserID+"/"+sharedDescriptorName]
	_, liveB := preparer.userLive[idB.TenantID+"/"+testAgentID+"/"+idB.UserID+"/"+sharedDescriptorName]
	preparer.mu.Unlock()
	if !liveA || !liveB {
		t.Fatalf("user physical registrations = A:%t B:%t, want both live", liveA, liveB)
	}

	for _, tc := range []struct {
		name string
		id   identity.Identity
		want string
		hash string
	}{
		{name: "A", id: idA, want: sharedDescriptorName, hash: registeredA.Revision.ContentHash},
		{name: "B", id: idB, want: sharedDescriptorName, hash: registeredB.Revision.ContentHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active, set, err := reg.Active(context.Background(), identity.Quadruple{Identity: tc.id}, testAgentID, agentcfg.ConfigScopeUser)
			if err != nil || !set {
				t.Fatalf("user %s active = (%+v, %t, %v)", tc.name, active, set, err)
			}
			pair, pairSet, err := active.Payload.SignedOAuthMCPPairByProvider("provider")
			if err != nil || !pairSet || pair.Connection.Name != tc.want {
				t.Fatalf("user %s pair = (%+v, %t, %v), want %q", tc.name, pair, pairSet, err, tc.want)
			}
			if active.ContentHash != tc.hash {
				t.Fatalf("user %s content hash = %q, want %q", tc.name, active.ContentHash, tc.hash)
			}
		})
	}
	if _, set, err := reg.Active(context.Background(), identity.Quadruple{Identity: idA}, testAgentID, agentcfg.ConfigScopeAgent); err != nil || set {
		t.Fatalf("user registration changed shared ConfigScopeAgent: set=%t err=%v", set, err)
	}

	// User A can present B's immutable hash, but the lookup is constrained to
	// A's user scope. The failed removal must leave B's desired pair intact.
	if _, err := svc.RemoveUserOAuthMCPCapability(ctxA, prototypes.AgentConfigUserRemoveOAuthMCPCapabilityRequest{
		Identity: prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID}, AgentID: testAgentID,
		ProviderName: "provider", ExpectedContentHash: registeredB.Revision.ContentHash,
	}); err == nil {
		t.Fatal("user A removed user B's pair")
	}
	activeB, setB, err := reg.Active(context.Background(), identity.Quadruple{Identity: idB}, testAgentID, agentcfg.ConfigScopeUser)
	if err != nil || !setB {
		t.Fatalf("user B active after foreign remove = (%+v, %t, %v)", activeB, setB, err)
	}
	if pair, pairSet, pairErr := activeB.Payload.SignedOAuthMCPPairByProvider("provider"); pairErr != nil || !pairSet || pair.Connection.Name != sharedDescriptorName {
		t.Fatalf("user B pair after foreign remove = (%+v, %t, %v)", pair, pairSet, pairErr)
	}

	if _, err := svc.RemoveUserOAuthMCPCapability(ctxA, prototypes.AgentConfigUserRemoveOAuthMCPCapabilityRequest{
		Identity: prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID}, AgentID: testAgentID,
		ProviderName: "provider", ExpectedContentHash: registeredA.Revision.ContentHash,
	}); err != nil {
		t.Fatalf("remove user A: %v", err)
	}
	activeB, setB, err = reg.Active(context.Background(), identity.Quadruple{Identity: idB}, testAgentID, agentcfg.ConfigScopeUser)
	if err != nil || !setB {
		t.Fatalf("user B active after user A removal = (%+v, %t, %v)", activeB, setB, err)
	}
	if pair, pairSet, pairErr := activeB.Payload.SignedOAuthMCPPairByProvider("provider"); pairErr != nil || !pairSet || pair.Connection.Name != sharedDescriptorName {
		t.Fatalf("user B pair after user A removal = (%+v, %t, %v)", pair, pairSet, pairErr)
	}
	preparer.mu.Lock()
	_, liveA = preparer.userLive[idA.TenantID+"/"+testAgentID+"/"+idA.UserID+"/"+sharedDescriptorName]
	_, liveB = preparer.userLive[idB.TenantID+"/"+testAgentID+"/"+idB.UserID+"/"+sharedDescriptorName]
	preparer.mu.Unlock()
	if liveA || !liveB {
		t.Fatalf("user physical registrations after A removal = A:%t B:%t, want A:false B:true", liveA, liveB)
	}
}

func TestUserSignedOAuthMCPCapability_AttachmentSurvivesSessionAndProcessRestart(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	userS1 := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-s1"}
	userS2 := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-s2"}
	request := signedCapabilityRequestFor(t, key, now,
		prototypes.IdentityScope{Tenant: userS1.TenantID, User: userS1.UserID, Session: userS1.SessionID},
		testAgentID, "jti-user-session-restart", "aud", "session-cap")
	registered, err := svc.RegisterUserOAuthMCPCapability(verifiedUserCapabilityContext(t, userS1, true, true), userRegisterRequest(request))
	if err != nil {
		t.Fatalf("register S1: %v", err)
	}
	preparer.mu.Lock()
	delete(preparer.userLive, userS1.TenantID+"/"+testAgentID+"/"+userS1.UserID+"/"+request.Connection.Name)
	preparer.mu.Unlock()

	// A fresh reconciler represents a process restart. The active user
	// revision remains durable, while the process-local physical attachment
	// is intentionally gone and must be rebuilt for S2.
	restarted, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatalf("new reconciler after restart: %v", err)
	}
	if err := restarted.ReconcileSignedOAuthMCPCapabilityForScope(context.Background(), identity.Quadruple{Identity: userS2}, testAgentID, agentcfg.ConfigScopeUser); err != nil {
		t.Fatalf("reconcile S2: %v", err)
	}
	preparer.mu.Lock()
	_, live := preparer.userLive[userS1.TenantID+"/"+testAgentID+"/"+userS1.UserID+"/"+request.Connection.Name]
	preparer.mu.Unlock()
	if !live {
		t.Fatal("S2 reconciliation did not reattach the durable user pair")
	}

	// Removal is also keyed by (tenant, user, agent), not the S1 audit
	// session. S2 may remove the same durable pair without re-authorizing it
	// as a new attachment.
	if _, err := svc.RemoveUserOAuthMCPCapability(verifiedUserCapabilityContext(t, userS2, true, true), prototypes.AgentConfigUserRemoveOAuthMCPCapabilityRequest{
		Identity: prototypes.IdentityScope{Tenant: userS2.TenantID, User: userS2.UserID, Session: userS2.SessionID},
		AgentID:  testAgentID, ProviderName: "provider", ExpectedContentHash: registered.Revision.ContentHash,
	}); err != nil {
		t.Fatalf("remove S2: %v", err)
	}
	preparer.mu.Lock()
	_, live = preparer.userLive[userS1.TenantID+"/"+testAgentID+"/"+userS1.UserID+"/"+request.Connection.Name]
	preparer.mu.Unlock()
	if live {
		t.Fatal("S2 removal left the user physical attachment live")
	}
	active, set, err := reg.Active(context.Background(), identity.Quadruple{Identity: userS2}, testAgentID, agentcfg.ConfigScopeUser)
	if err != nil {
		t.Fatalf("read S2 user revision after removal: %v", err)
	}
	if !set {
		t.Fatal("S2 user revision disappeared after removal; removal should retain the durable empty revision")
	}
	if _, pairSet, pairErr := active.Payload.SignedOAuthMCPPairByProvider("provider"); pairErr != nil || pairSet {
		t.Fatalf("S2 user pair after removal = set:%t err:%v, want absent", pairSet, pairErr)
	}
}

func TestUserSignedOAuthMCPCapability_RequiresVerifiedScopeAndReach(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, key, _, _, _ := signedCapabilityServiceWithRegistry(t, now)
	id := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-a"}
	req := userRegisterRequest(signedCapabilityRequestFor(t, key, now,
		prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, testAgentID, "jti-user-auth", "aud", "user-auth-cap"))

	if _, err := svc.RegisterUserOAuthMCPCapability(context.Background(), req); !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("missing verified identity error = %v, want ErrIdentityRequired", err)
	}
	verified := verifiedUserCapabilityContext(t, id, false, true)
	if _, err := svc.RegisterUserOAuthMCPCapability(verified, req); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityUserAuthorization) {
		t.Fatalf("missing user scope error = %v, want ErrSignedCapabilityUserAuthorization", err)
	}
	noReach := verifiedUserCapabilityContext(t, id, true, false)
	if _, err := svc.RegisterUserOAuthMCPCapability(noReach, req); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityUserAuthorization) {
		t.Fatalf("missing signed reach error = %v, want ErrSignedCapabilityUserAuthorization", err)
	}
}
