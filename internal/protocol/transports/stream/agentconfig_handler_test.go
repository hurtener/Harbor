package stream_test

import (
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// agentconfig_handler_test.go — the three-tier scope gate on the agent-config
// wire handler. The shared harness (sessionHandler / acReq / acID / acAgent /
// errCode) lives in agentconfig_session_handler_test.go.

const userBody = `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","payload":{"user_prompt":"be terse","disabled_servers":["weather"]}}`

// TestAgentConfigHandler_UserRoute_WithUserScopeAllowed proves a caller
// carrying the agent_config:user scope reaches a user-tier route.
func TestAgentConfigHandler_UserRoute_WithUserScopeAllowed(t *testing.T) {
	h := sessionHandler(t)
	code, resp := acReq(t, h, "user/set_revision", userBody, acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusOK {
		t.Fatalf("user route should allow an agent_config:user caller; got %d body=%s", code, resp)
	}
}

// TestAgentConfigHandler_UserRoute_NoScopeRejected proves a caller WITHOUT the
// user scope is rejected on a user route (CodeScopeMismatch / 403).
func TestAgentConfigHandler_UserRoute_NoScopeRejected(t *testing.T) {
	h := sessionHandler(t)
	code, resp := acReq(t, h, "user/set_revision", userBody, acID(), []auth.Scope{})
	if code != http.StatusForbidden {
		t.Fatalf("user route should reject a no-scope caller; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeScopeMismatch {
		t.Fatalf("no-scope user-route error code = %s, want %s", c, protoerrors.CodeScopeMismatch)
	}
}

// TestAgentConfigHandler_UserRoute_AdminTokenRejected proves an ADMIN token
// (without the user scope) is rejected on a user route — admin does NOT
// implicitly own per-user variants (the tiers are orthogonal).
func TestAgentConfigHandler_UserRoute_AdminTokenRejected(t *testing.T) {
	h := sessionHandler(t)
	code, resp := acReq(t, h, "user/set_revision", userBody, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusForbidden {
		t.Fatalf("user route should reject an admin-only token; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeScopeMismatch {
		t.Fatalf("admin-token user-route error code = %s, want %s", c, protoerrors.CodeScopeMismatch)
	}
}

// TestAgentConfigHandler_AdminRoute_UserTokenRejected proves a user-only token
// is rejected on an ADMIN route (the user scope does not admit admin routes).
func TestAgentConfigHandler_AdminRoute_UserTokenRejected(t *testing.T) {
	h := sessionHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","prompt_layers":{"base":"x"}}`
	code, resp := acReq(t, h, "set_prompt_layers", body, acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusForbidden {
		t.Fatalf("admin route should reject a user-only token; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeScopeMismatch {
		t.Fatalf("user-token admin-route error code = %s, want %s", c, protoerrors.CodeScopeMismatch)
	}
}

// TestSetMCPDiscoveryOrigins_RejectsNonAdminScope proves the discovery-allowance
// write route is admin-gated by inheriting the handler's default arm: a
// valid-identity-but-no-scope caller AND an agent_config:user-scoped caller are
// both rejected with CodeScopeMismatch BEFORE dispatch.
func TestSetMCPDiscoveryOrigins_RejectsNonAdminScope(t *testing.T) {
	h := sessionHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","name":"srv","allowed_origins":["https://as.example.net"]}`
	for _, scopes := range [][]auth.Scope{{}, {auth.ScopeAgentConfigUser}} {
		code, resp := acReq(t, h, "set_mcp_discovery_origins", body, acID(), scopes)
		if code != http.StatusForbidden {
			t.Fatalf("non-admin caller (scopes=%v) should be 403; got %d body=%s", scopes, code, resp)
		}
		if c := errCode(t, resp); c != protoerrors.CodeScopeMismatch {
			t.Fatalf("non-admin error code = %s, want %s", c, protoerrors.CodeScopeMismatch)
		}
	}
}

// TestAgentConfigHandler_UserRoute_IncompleteTriple401 proves a user route
// still fails closed (401) on a missing identity, even with the user scope.
func TestAgentConfigHandler_UserRoute_IncompleteTriple401(t *testing.T) {
	h := sessionHandler(t)
	body := `{"agent_id":"` + acAgent + `","payload":{"user_prompt":"x"}}`
	code, _ := acReq(t, h, "user/set_revision", body, nil, []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusUnauthorized {
		t.Fatalf("missing identity should be 401; got %d", code)
	}
}

// TestAgentConfigHandler_UserRoundTrip proves the user tier set→list works end
// to end through the wire handler.
func TestAgentConfigHandler_UserRoundTrip(t *testing.T) {
	h := sessionHandler(t)
	if code, resp := acReq(t, h, "user/set_revision", userBody, acID(), []auth.Scope{auth.ScopeAgentConfigUser}); code != http.StatusOK {
		t.Fatalf("user set: %d body=%s", code, resp)
	}
	listBody := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `"}`
	code, resp := acReq(t, h, "user/list_revisions", listBody, acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusOK {
		t.Fatalf("user list: %d body=%s", code, resp)
	}
}
