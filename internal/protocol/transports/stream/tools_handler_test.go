package stream_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// toolsHandlerID is a documented dummy identity triple — no secrets.
var toolsHandlerID = identity.Identity{TenantID: "t-th", UserID: "u-th", SessionID: "s-th"}

// newToolsHandler builds a ToolsHandler over an in-memory catalog
// seeded with two tools.
func newToolsHandler(t *testing.T, extra ...stream.ToolsOption) *stream.ToolsHandler {
	t.Helper()
	cat := tools.NewCatalog()
	for _, spec := range []struct {
		name      string
		transport tools.TransportKind
	}{
		{"echo", tools.TransportInProcess},
		{"web_search", tools.TransportHTTP},
	} {
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{
				Name:      spec.name,
				Transport: spec.transport,
				Loading:   tools.LoadingAlways,
			},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", spec.name, err)
		}
	}
	proj, err := toolsprotocol.NewCatalogProjector(cat)
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	svc, err := toolsprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	opts := []stream.ToolsOption{
		stream.WithToolsReachAuthorizer(auth.NewAgentReachAuthorizer()),
		stream.WithToolsAgentResolver(toolsAgentResolverFunc(func(context.Context, identity.Identity, string) (bool, error) {
			return true, nil
		})),
	}
	opts = append(opts, extra...)
	h, err := stream.NewToolsHandler(svc, opts...)
	if err != nil {
		t.Fatalf("NewToolsHandler: %v", err)
	}
	return h
}

func newToolsHandlerWithoutAgentResolver(t *testing.T) *stream.ToolsHandler {
	t.Helper()
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "echo", Transport: tools.TransportInProcess, Loading: tools.LoadingAlways},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("register catalog tool: %v", err)
	}
	proj, err := toolsprotocol.NewCatalogProjector(cat)
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	svc, err := toolsprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewToolsHandler(svc, stream.WithToolsReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("NewToolsHandler: %v", err)
	}
	return h
}

type toolsAgentResolverFunc func(context.Context, identity.Identity, string) (bool, error)

func (f toolsAgentResolverFunc) ResolveAgent(ctx context.Context, id identity.Identity, agentID string) (bool, error) {
	return f(ctx, id, agentID)
}

// doToolsRequest issues a POST /v1/tools/{verb} against the handler.
// id (when non-nil) is carried via the X-Harbor-* headers; scopes
// (when non-nil) are injected into the request context, simulating
// auth.Middleware.
func doToolsRequest(t *testing.T, h http.Handler, verb, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/"+verb, strings.NewReader(body))
	req.SetPathValue("method", verb)
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	if id != nil {
		req = req.WithContext(auth.WithAgentReach(req.Context(), []string{"agent-a"}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestNewToolsHandler_NilService_FailsLoudly(t *testing.T) {
	if _, err := stream.NewToolsHandler(nil); err == nil {
		t.Fatal("NewToolsHandler(nil) did not fail")
	}
}

func TestToolsHandler_List_HappyPath(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "list", "{}", &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.ToolListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Tools) != 2 {
		t.Errorf("tools/list returned %d tools, want 2", len(resp.Tools))
	}
}

func TestToolsHandler_List_MissingIdentity_401(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "list", "{}", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("tools/list without identity status = %d, want 401", status)
	}
	assertErrorCode(t, body, protoerrors.CodeIdentityRequired)
}

func TestToolsHandler_List_TransportFacet(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "list",
		`{"filter":{"transports":["HTTP"]}}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/list facet status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.ToolListResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Tools) != 1 || resp.Tools[0].Name != "web_search" {
		t.Fatalf("HTTP facet returned %d rows, want 1 (web_search)", len(resp.Tools))
	}
}

func TestToolsHandler_List_ExplicitAgentSelectionUsesReachAndResolver(t *testing.T) {
	var resolved atomic.Int64
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(_ context.Context, id identity.Identity, agentID string) (bool, error) {
			if id != toolsHandlerID || agentID != "agent-a" {
				t.Fatalf("resolver input = (%+v,%q)", id, agentID)
			}
			resolved.Add(1)
			return true, nil
		})))
	status, body := doToolsRequest(t, h, "list", `{"agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/list explicit agent status = %d body=%s", status, body)
	}
	if resolved.Load() != 1 {
		t.Fatalf("agent resolver calls = %d, want 1", resolved.Load())
	}
}

func TestToolsHandler_List_ExcludedReachPrecedesResolver(t *testing.T) {
	var resolved atomic.Int64
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(context.Context, identity.Identity, string) (bool, error) {
			resolved.Add(1)
			return true, nil
		})))
	status, body := doToolsRequest(t, h, "list", `{"agent_id":"agent-b"}`, &toolsHandlerID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("tools/list excluded agent status = %d body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeScopeMismatch)
	if resolved.Load() != 0 {
		t.Fatalf("agent resolver calls = %d, want 0 before reach authorization", resolved.Load())
	}
}

func TestToolsHandler_List_UnresolvableAgentFailsClosed(t *testing.T) {
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(context.Context, identity.Identity, string) (bool, error) { return false, nil })))
	status, body := doToolsRequest(t, h, "list", `{"agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("tools/list unresolvable agent status = %d body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeInvalidRequest)
}

func TestToolsHandler_List_ResolverFailureIsLoud(t *testing.T) {
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(context.Context, identity.Identity, string) (bool, error) {
			return false, errors.New("resolver unavailable")
		})))
	status, body := doToolsRequest(t, h, "list", `{"agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("tools/list resolver failure status = %d body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeRuntimeError)
}

func TestToolsHandler_List_ExplicitAgentWithoutResolverFailsClosed(t *testing.T) {
	h := newToolsHandlerWithoutAgentResolver(t)
	status, body := doToolsRequest(t, h, "list", `{"agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("tools/list without resolver status = %d, want 400; body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeInvalidRequest)
}

func TestToolsHandler_List_OmittedAgentPreservesBootView(t *testing.T) {
	var resolved atomic.Int64
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(context.Context, identity.Identity, string) (bool, error) {
			resolved.Add(1)
			return true, nil
		})))
	status, body := doToolsRequest(t, h, "list", `{}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/list omitted agent status = %d body=%s", status, body)
	}
	if resolved.Load() != 0 {
		t.Fatalf("agent resolver calls = %d, want 0 for omitted agent", resolved.Load())
	}
}

func TestToolsHandler_Get_HappyPath(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "get", `{"id":"echo"}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/get status = %d, want 200; body=%s", status, body)
	}
}

func TestToolsHandler_Get_UnknownTool_404(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "get", `{"id":"ghost"}`, &toolsHandlerID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("tools/get unknown status = %d, want 404", status)
	}
	assertErrorCode(t, body, protoerrors.CodeNotFound)
}

func TestToolsHandler_Describe_HappyPath(t *testing.T) {
	h := newToolsHandler(t)
	status, _ := doToolsRequest(t, h, "describe", `{"id":"echo"}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/describe status = %d, want 200", status)
	}
}

func TestToolsHandler_Describe_ExplicitAgentReachOnly(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "describe", `{"id":"echo","agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("allowed explicit projection status = %d body=%s", status, body)
	}
	status, body = doToolsRequest(t, h, "describe", `{"id":"echo","agent_id":"agent-b"}`, &toolsHandlerID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("excluded explicit projection status = %d body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeScopeMismatch)
	// Omission remains the boot-effective projection and therefore does not
	// consume reach; TestToolsHandler_Describe_HappyPath covers that route.
}

func TestToolsHandler_Describe_ExcludedReachPrecedesLifecycleResolver(t *testing.T) {
	var calls atomic.Int64
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(context.Context, identity.Identity, string) (bool, error) {
			calls.Add(1)
			return true, nil
		})))
	status, body := doToolsRequest(t, h, "describe", `{"id":"echo","agent_id":"agent-b"}`, &toolsHandlerID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("excluded explicit projection status = %d body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeScopeMismatch)
	if calls.Load() != 0 {
		t.Fatalf("lifecycle resolver calls = %d, want 0 before reach authorization", calls.Load())
	}
}

func TestToolsHandler_Describe_RetiredAgent409(t *testing.T) {
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(_ context.Context, id identity.Identity, agentID string) (bool, error) {
			if id != toolsHandlerID || agentID != "agent-a" {
				t.Fatalf("resolver input = (%+v,%q)", id, agentID)
			}
			return false, protocol.ErrAgentRetired
		})))
	status, body := doToolsRequest(t, h, "describe", `{"id":"echo","agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusConflict {
		t.Fatalf("retired projection status = %d body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeAgentRetired)
}

func TestToolsHandler_Describe_ResolvableAgentStillReachesService(t *testing.T) {
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(context.Context, identity.Identity, string) (bool, error) { return true, nil })))
	status, body := doToolsRequest(t, h, "describe", `{"id":"echo","agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("resolvable projection status = %d body=%s", status, body)
	}
}

func TestToolsHandler_Describe_UnresolvableAgentPreservesNonOracleRefusal(t *testing.T) {
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(context.Context, identity.Identity, string) (bool, error) { return false, nil })))
	status, body := doToolsRequest(t, h, "describe", `{"id":"echo","agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("unresolvable projection status = %d body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeInvalidRequest)
}

func TestToolsHandler_Describe_LifecycleFault500(t *testing.T) {
	h := newToolsHandler(t, stream.WithToolsAgentResolver(toolsAgentResolverFunc(
		func(context.Context, identity.Identity, string) (bool, error) {
			return false, errors.New("lifecycle unavailable")
		})))
	status, body := doToolsRequest(t, h, "describe", `{"id":"echo","agent_id":"agent-a"}`, &toolsHandlerID, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("lifecycle fault status = %d body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeRuntimeError)
}

func TestToolsHandler_Metrics_HappyPath(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "metrics", `{"id":"echo","window":"1h"}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/metrics status = %d, want 200; body=%s", status, body)
	}
	var m prototypes.ToolMetrics
	_ = json.Unmarshal(body, &m)
	if !prototypes.IsValidToolStatus(m.Status) {
		t.Errorf("tools/metrics status pill = %q, not a valid status", m.Status)
	}
}

func TestToolsHandler_ContentStats_HappyPath(t *testing.T) {
	h := newToolsHandler(t)
	status, _ := doToolsRequest(t, h, "content_stats", `{"id":"echo"}`, &toolsHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/content_stats status = %d, want 200", status)
	}
}

func TestToolsHandler_SetApprovalPolicy_WithoutAdminScope_403(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "set_approval_policy",
		`{"id":"echo","policy":"gated"}`, &toolsHandlerID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("tools/set_approval_policy without admin status = %d, want 403", status)
	}
	assertErrorCode(t, body, protoerrors.CodeIdentityScopeRequired)
}

func TestToolsHandler_SetApprovalPolicy_WithAdminScope_200(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "set_approval_policy",
		`{"id":"echo","policy":"gated"}`, &toolsHandlerID, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("tools/set_approval_policy with admin status = %d, want 200; body=%s", status, body)
	}
}

func TestToolsHandler_RevokeOAuth_WithoutAdminScope_403(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "revoke_oauth", `{"id":"echo"}`, &toolsHandlerID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("tools/revoke_oauth without admin status = %d, want 403", status)
	}
	assertErrorCode(t, body, protoerrors.CodeIdentityScopeRequired)
}

func TestToolsHandler_UnknownMethod_404(t *testing.T) {
	h := newToolsHandler(t)
	status, body := doToolsRequest(t, h, "frobnicate", "{}", &toolsHandlerID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("tools/frobnicate status = %d, want 404", status)
	}
	assertErrorCode(t, body, protoerrors.CodeUnknownMethod)
}

func TestToolsHandler_GetMethod_405(t *testing.T) {
	h := newToolsHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tools/list", nil)
	req.SetPathValue("method", "list")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/tools/list status = %d, want 405", rec.Code)
	}
}
