package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

type liveProfileReconcilerSpy struct {
	mu     sync.Mutex
	calls  []liveProfileReconcileCall
	err    error
	onCall func(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) error
}

type liveProfileReconcileCall struct {
	q     identity.Quadruple
	agent string
	scope agentcfg.ConfigScope
}

func testNow() time.Time {
	return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
}

func (s *liveProfileReconcilerSpy) ReconcileSignedOAuthMCPCapabilityForScope(ctx context.Context, q identity.Quadruple, agentID string, scope agentcfg.ConfigScope) error {
	return s.reconcile(ctx, q, agentID, scope)
}

func (s *liveProfileReconcilerSpy) reconcile(ctx context.Context, q identity.Quadruple, agentID string, scope agentcfg.ConfigScope) error {
	s.mu.Lock()
	s.calls = append(s.calls, liveProfileReconcileCall{q: q, agent: agentID, scope: scope})
	err := s.err
	onCall := s.onCall
	s.mu.Unlock()
	if onCall != nil {
		if callbackErr := onCall(ctx, q, agentID, scope); callbackErr != nil {
			return callbackErr
		}
	}
	return err
}

func TestUserReconcileLiveProfileUsesVerifiedCurrentSubjectAndIsolatesUsers(t *testing.T) {
	_, _, reg, _, _ := signedCapabilityServiceWithRegistry(t, testNow())
	spy := &liveProfileReconcilerSpy{}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSignedOAuthMCPUserReconciler(spy))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	ids := []identity.Identity{
		{TenantID: "t", UserID: "user-a", SessionID: "session-a"},
		{TenantID: "t", UserID: "user-b", SessionID: "session-b"},
	}
	for _, id := range ids {
		if _, err := reg.SetRevision(context.Background(), identity.Quadruple{Identity: id}, testAgentID, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
			t.Fatalf("seed %s: %v", id.UserID, err)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx := verifiedUserCapabilityContext(t, id, true, true)
			resp, callErr := svc.UserReconcileLiveProfile(ctx, prototypes.AgentConfigUserReconcileLiveProfileRequest{
				Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
				AgentID:  testAgentID,
			})
			if callErr == nil && (!resp.Set || resp.Revision == nil) {
				callErr = errors.New("response omitted the caller's active profile")
			}
			errs <- callErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Fatal(callErr)
		}
	}

	spy.mu.Lock()
	calls := append([]liveProfileReconcileCall(nil), spy.calls...)
	spy.mu.Unlock()
	if len(calls) != len(ids) {
		t.Fatalf("reconciler calls = %d, want %d", len(calls), len(ids))
	}
	seen := make(map[string]liveProfileReconcileCall, len(calls))
	for _, call := range calls {
		if call.agent != testAgentID || call.scope != agentcfg.ConfigScopeUser {
			t.Fatalf("reconcile call = %+v, want user scope and %q", call, testAgentID)
		}
		seen[call.q.UserID] = call
	}
	for _, id := range ids {
		call, ok := seen[id.UserID]
		if !ok || call.q.TenantID != id.TenantID || call.q.UserID != id.UserID || call.q.SessionID != id.SessionID {
			t.Fatalf("reconcile call for %s = %+v, want current verified subject", id.UserID, call)
		}
	}
}

func TestUserReconcileLiveProfileS2UsesCurrentSessionWithoutNewSelectors(t *testing.T) {
	_, _, reg, _, _ := signedCapabilityServiceWithRegistry(t, testNow())
	spy := &liveProfileReconcilerSpy{}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSignedOAuthMCPUserReconciler(spy))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	s1 := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-s1"}
	s2 := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-s2"}
	if _, err := reg.SetRevision(context.Background(), identity.Quadruple{Identity: s1}, testAgentID, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	request := prototypes.AgentConfigUserReconcileLiveProfileRequest{
		Identity: prototypes.IdentityScope{Tenant: s2.TenantID, User: s2.UserID, Session: s2.SessionID}, AgentID: testAgentID,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	for _, forbidden := range []string{"provider", "descriptor", "authority", "jti", "tenant_id", "user_id"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("request contains forbidden selector %q: %s", forbidden, encoded)
		}
	}
	resp, err := svc.UserReconcileLiveProfile(verifiedUserCapabilityContext(t, s2, true, true), request)
	if err != nil {
		t.Fatalf("reconcile S2: %v", err)
	}
	if !resp.Set || resp.Revision == nil {
		t.Fatalf("S2 response = %+v, want active profile", resp)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.calls) != 1 || spy.calls[0].q != (identity.Quadruple{Identity: s2}) {
		t.Fatalf("reconciler call = %+v, want S2 current subject", spy.calls)
	}
}

func TestUserReconcileLiveProfileReturnsFreshRevisionAfterConcurrentRemoval(t *testing.T) {
	now := testNow()
	base, key, reg, _, _ := signedCapabilityServiceWithRegistry(t, now)
	id := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-a"}
	request := signedCapabilityRequestFor(t, key, now,
		prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		testAgentID, "jti-live-reconcile-remove", "aud-live-reconcile", "live-reconcile-cap")
	registered, err := base.RegisterUserOAuthMCPCapability(verifiedUserCapabilityContext(t, id, true, true), userRegisterRequest(request))
	if err != nil {
		t.Fatalf("seed user pair: %v", err)
	}

	removeDone := make(chan error, 1)
	spy := &liveProfileReconcilerSpy{}
	spy.onCall = func(_ context.Context, q identity.Quadruple, agentID string, scope agentcfg.ConfigScope) error {
		if q.Identity != id || agentID != testAgentID || scope != agentcfg.ConfigScopeUser {
			return errors.New("reconcile callback received the wrong subject")
		}
		// Run removal while the live-profile operation is still inside its
		// reconciler call. The wrapper must read the active pointer only after
		// this concurrent revision fence has settled, never return the removed
		// pair's stale revision.
		go func() {
			_, removeErr := base.RemoveUserOAuthMCPCapability(verifiedUserCapabilityContext(t, id, true, true), prototypes.AgentConfigUserRemoveOAuthMCPCapabilityRequest{
				Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
				AgentID:  testAgentID, ProviderName: request.ProviderName, ExpectedContentHash: registered.Revision.ContentHash,
			})
			removeDone <- removeErr
		}()
		select {
		case removeErr := <-removeDone:
			return removeErr
		case <-time.After(5 * time.Second):
			return errors.New("concurrent removal did not complete")
		}
	}
	liveSvc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSignedOAuthMCPUserReconciler(spy))
	if err != nil {
		t.Fatalf("new live-profile service: %v", err)
	}
	resp, err := liveSvc.UserReconcileLiveProfile(verifiedUserCapabilityContext(t, id, true, true), prototypes.AgentConfigUserReconcileLiveProfileRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		AgentID:  testAgentID,
	})
	if err != nil {
		t.Fatalf("live-profile reconcile after concurrent removal: %v", err)
	}
	if !resp.Set || resp.Revision == nil {
		t.Fatalf("response = %+v, want the durable empty post-removal revision", resp)
	}
	if resp.Revision.ContentHash == registered.Revision.ContentHash {
		t.Fatalf("response returned stale removed revision %q", resp.Revision.ContentHash)
	}
	if resp.Revision.Payload.SignedOAuthMCPPair != nil || len(resp.Revision.Payload.SignedOAuthMCPPairs) != 0 {
		t.Fatalf("response retained removed pair: legacy=%+v collection=%+v", resp.Revision.Payload.SignedOAuthMCPPair, resp.Revision.Payload.SignedOAuthMCPPairs)
	}
}

func TestUserReconcileLiveProfileRequiresVerifiedUserScopeAndReach(t *testing.T) {
	_, _, reg, _, _ := signedCapabilityServiceWithRegistry(t, testNow())
	spy := &liveProfileReconcilerSpy{}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSignedOAuthMCPUserReconciler(spy))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	id := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-a"}
	req := prototypes.AgentConfigUserReconcileLiveProfileRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, AgentID: testAgentID,
	}
	if _, err := svc.UserReconcileLiveProfile(context.Background(), req); !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("missing verified identity error = %v, want ErrIdentityRequired", err)
	}
	if _, err := svc.UserReconcileLiveProfile(verifiedUserCapabilityContext(t, id, false, true), req); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityUserAuthorization) {
		t.Fatalf("missing user scope error = %v, want user authorization", err)
	}
	if _, err := svc.UserReconcileLiveProfile(verifiedUserCapabilityContext(t, id, true, false), req); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityUserAuthorization) {
		t.Fatalf("missing reach error = %v, want user authorization", err)
	}
	other := identity.Identity{TenantID: "t", UserID: "user-b", SessionID: "session-b"}
	if _, err := svc.UserReconcileLiveProfile(verifiedUserCapabilityContext(t, other, true, true), req); !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("body/verified mismatch error = %v, want ErrIdentityRequired", err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.calls) != 0 {
		t.Fatalf("reconciler called on rejected requests: %+v", spy.calls)
	}
}

func TestUserReconcileLiveProfileWithoutReconcilerFailsClosed(t *testing.T) {
	_, _, reg, _, _ := signedCapabilityServiceWithRegistry(t, testNow())
	svc, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	id := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-a"}
	req := prototypes.AgentConfigUserReconcileLiveProfileRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, AgentID: testAgentID,
	}
	if _, err := svc.UserReconcileLiveProfile(verifiedUserCapabilityContext(t, id, true, true), req); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityUnavailable) {
		t.Fatalf("missing reconciler error = %v, want ErrSignedCapabilityUnavailable", err)
	}
}
