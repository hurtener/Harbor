package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

type liveProfileRouteSpy struct {
	mu    sync.Mutex
	calls []identity.Quadruple
}

func (s *liveProfileRouteSpy) ReconcileSignedOAuthMCPCapabilityForScope(_ context.Context, q identity.Quadruple, agentID string, scope agentcfg.ConfigScope) error {
	if agentID != acAgent || scope != agentcfg.ConfigScopeUser {
		return context.Canceled
	}
	s.mu.Lock()
	s.calls = append(s.calls, q)
	s.mu.Unlock()
	return nil
}

func TestAgentConfigHandler_UserReconcileLiveProfileUsesUserTierAndVerifiedBody(t *testing.T) {
	fixture := newSessionHandlerFixture(t)
	spy := &liveProfileRouteSpy{}
	svc, err := agentcfgprotocol.NewService(fixture.registry, agentcfgprotocol.WithSignedOAuthMCPUserReconciler(spy))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc, stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `"}`
	code, raw := acReq(t, h, "user/reconcile_live_profile", body, acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusOK {
		t.Fatalf("reconcile status=%d body=%s", code, raw)
	}
	var response prototypes.AgentConfigUserReconcileLiveProfileResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, raw)
	}
	if response.Set {
		t.Fatalf("response Set=true for absent user profile: %+v", response)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.calls) != 1 || spy.calls[0].Identity != *acID() {
		t.Fatalf("reconciler calls=%+v, want verified request identity", spy.calls)
	}
}

func TestAgentConfigHandler_UserReconcileLiveProfileRejectsReachAndBodyMismatch(t *testing.T) {
	fixture := newSessionHandlerFixture(t)
	spy := &liveProfileRouteSpy{}
	svc, err := agentcfgprotocol.NewService(fixture.registry, agentcfgprotocol.WithSignedOAuthMCPUserReconciler(spy))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc, stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `"}`
	code, raw := acReqReach(t, h, "user/reconcile_live_profile", body, acID(), []auth.Scope{auth.ScopeAgentConfigUser}, []string{"other-agent"})
	if code != http.StatusForbidden || errCode(t, raw) != protoerrors.CodeScopeMismatch {
		t.Fatalf("excluded reach=(%d,%s), want 403 scope_mismatch", code, raw)
	}
	foreign := strings.Replace(body, `"user":"u1"`, `"user":"foreign"`, 1)
	code, raw = acReq(t, h, "user/reconcile_live_profile", foreign, acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusUnauthorized || errCode(t, raw) != protoerrors.CodeIdentityRequired {
		t.Fatalf("foreign body=(%d,%s), want 401 identity_required", code, raw)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.calls) != 0 {
		t.Fatalf("reconciler called on rejected requests: %+v", spy.calls)
	}
}

// TestAgentConfigHandler_UserReconcileLiveProfileAdmitsReadReach pins D-510:
// a bearer that may read but not select the agent may restore its OWN saved
// user profile, while every other agent-config method (here user/get and
// user/set_revision) still requires agent_reach, and an agent outside the
// read reach stays refused.
func TestAgentConfigHandler_UserReconcileLiveProfileAdmitsReadReach(t *testing.T) {
	fixture := newSessionHandlerFixture(t)
	spy := &liveProfileRouteSpy{}
	svc, err := agentcfgprotocol.NewService(fixture.registry, agentcfgprotocol.WithSignedOAuthMCPUserReconciler(spy))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc, stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	send := func(route, body string, readReach []string, sessionApps bool) (int, []byte) {
		t.Helper()
		id := acID()
		req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/"+route, strings.NewReader(body))
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
		ctx := auth.WithScopes(req.Context(), []auth.Scope{auth.ScopeAgentConfigUser})
		ctx = auth.WithAgentReadReach(auth.WithAgentReach(ctx, nil), readReach)
		ctx = auth.WithAgentReadSessionApps(ctx, sessionApps)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(ctx))
		return rec.Code, rec.Body.Bytes()
	}
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `"}`
	if code, raw := send("user/reconcile_live_profile", body, []string{acAgent}, true); code != http.StatusOK {
		t.Fatalf("read-reach restore = %d %s", code, raw)
	}
	if code, raw := send("user/reconcile_live_profile", body, []string{"other-agent"}, true); code != http.StatusForbidden || errCode(t, raw) != protoerrors.CodeScopeMismatch {
		t.Fatalf("restore outside read reach = %d %s, want 403", code, raw)
	}
	if code, raw := send("user/reconcile_live_profile", body, []string{acAgent}, false); code != http.StatusForbidden {
		t.Fatalf("history-only read bearer restore = %d %s, want 403 without agent_read_session_apps", code, raw)
	}
	if code, raw := send("user/get", body, []string{acAgent}, true); code != http.StatusForbidden {
		t.Fatalf("read-reach user/get = %d %s, want 403 (only the restore accepts read reach)", code, raw)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.calls) != 1 || spy.calls[0].Identity != *acID() {
		t.Fatalf("reconciler calls = %+v, want exactly the admitted restore", spy.calls)
	}
}
