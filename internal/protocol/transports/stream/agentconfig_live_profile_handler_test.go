package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
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
