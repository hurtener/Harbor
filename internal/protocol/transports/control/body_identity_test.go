package control_test

// body_identity_test.go — body-identity reconciliation coverage for the
// MCP Apps (`serveApps`) and MCP-Connections (`serveMCP`) transport
// adapters.
//
// Both adapters accept an IdentityScope on the request body. When the
// auth middleware ran, r.Context() carries the verified identity and the
// adapter reconciles the two:
//
//	(1) body triple empty            → backfilled from the verified triple
//	(2) body triple matches verified → dispatched
//	(3) body tenant differs          → CodeIdentityRequired, never dispatched
//	(4) body user/session differs    → CodeIdentityRequired, never dispatched
//	(5) no verified identity in ctx  → body is authoritative (no-op)
//
// Neither surface has an admin-elevation path that widens the tenant, so
// (3) is a flat refusal — the same shape the start/control adapter
// applies at control.go's assertBodyMatchesAuthedIdentity.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// recordingSurface is a deterministic MCPSurface / AppsSurface that
// records the identity scope the transport handed it. A refusal path
// must leave calls == 0; an accepted path records the reconciled scope
// so the backfill can be asserted.
type recordingSurface struct {
	mu    sync.Mutex
	calls int
	last  types.IdentityScope
}

func (s *recordingSurface) Dispatch(_ context.Context, method methods.Method, req any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	switch v := req.(type) {
	case *types.MCPServersListRequest:
		s.last = v.Identity
	case *types.ReadMCPResourceRequest:
		s.last = v.Identity
	case *types.MCPAppCallToolRequest:
		s.last = v.Identity
	case *types.ToolContextRequest:
		s.last = v.Identity
	}
	switch method {
	case methods.MethodMCPServersList:
		return &types.MCPServersListResponse{ProtocolVersion: types.ProtocolVersion}, nil
	case methods.MethodMCPReadResource:
		return &types.ReadMCPResourceResponse{ProtocolVersion: types.ProtocolVersion}, nil
	default:
		return &types.ToolContextResponse{ProtocolVersion: types.ProtocolVersion}, nil
	}
}

func (s *recordingSurface) snapshot() (int, types.IdentityScope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.last
}

// newBodyIdentityHandler mounts the control handler with both the MCP
// and MCP Apps surfaces wired to surf, optionally behind a middleware
// that attaches verified into ctx (the shape auth.Middleware produces).
// A zero verified means no middleware ran.
func newBodyIdentityHandler(t *testing.T, surf *recordingSurface, verified identity.Identity) http.Handler {
	t.Helper()
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs,
		control.WithMCPSurface(surf),
		control.WithAppsSurface(surf),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	if verified == (identity.Identity{}) {
		mux.Handle(control.RoutePattern, h)
	} else {
		mux.Handle(control.RoutePattern, withIdentity(h, verified))
	}
	return mux
}

// scopeBody renders a request body carrying exactly the given identity
// triple. Every request type on both surfaces embeds the scope under
// `identity`, so one renderer covers them all.
func scopeBody(tenant, user, session string) string {
	return `{"identity":{"tenant":"` + tenant + `","user":"` + user + `","session":"` + session + `"}}`
}

// postMethod drives one Protocol method through the mounted handler and
// returns the status code plus the decoded error envelope (zero-valued
// on a 200).
func postMethod(t *testing.T, h http.Handler, method methods.Method, body string) (int, protoerrors.Error) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/control/"+string(method), strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var perr protoerrors.Error
	if resp.StatusCode != http.StatusOK {
		if jerr := json.Unmarshal(raw, &perr); jerr != nil {
			t.Fatalf("decode error envelope (%d): %v; raw=%s", resp.StatusCode, jerr, raw)
		}
	}
	return resp.StatusCode, perr
}

var bodyIdentityVerified = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

// ---------------------------------------------------------------------
// MCP Apps surface
// ---------------------------------------------------------------------

// TestAppsIdentity_BodyTenantMismatch_Rejected pins the full-triple
// reconciliation: a body naming a tenant other than the verified one is
// refused with CodeIdentityRequired and never reaches the AppsSurface.
func TestAppsIdentity_BodyTenantMismatch_Rejected(t *testing.T) {
	t.Parallel()
	for _, method := range []methods.Method{
		methods.MethodMCPReadResource,
		methods.MethodMCPAppsCallTool,
		methods.MethodMCPAppsToolContext,
	} {
		t.Run(string(method), func(t *testing.T) {
			t.Parallel()
			surf := &recordingSurface{}
			h := newBodyIdentityHandler(t, surf, bodyIdentityVerified)
			status, perr := postMethod(t, h, method, scopeBody("t2", "u1", "s1"))
			if status != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", status)
			}
			if perr.Code != protoerrors.CodeIdentityRequired {
				t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeIdentityRequired)
			}
			if calls, _ := surf.snapshot(); calls != 0 {
				t.Errorf("AppsSurface.Dispatch called %d times, want 0", calls)
			}
		})
	}
}

// TestAppsIdentity_BodyUserMismatch_Rejected keeps the user/session leg
// of the reconciliation covered alongside the tenant leg.
func TestAppsIdentity_BodyUserMismatch_Rejected(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, bodyIdentityVerified)
	status, perr := postMethod(t, h, methods.MethodMCPAppsCallTool, scopeBody("t1", "u2", "s1"))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if perr.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeIdentityRequired)
	}
	if calls, _ := surf.snapshot(); calls != 0 {
		t.Errorf("AppsSurface.Dispatch called %d times, want 0", calls)
	}
}

// TestAppsIdentity_BodyMatchesVerified_Dispatched confirms the matching
// triple is the accepted path.
func TestAppsIdentity_BodyMatchesVerified_Dispatched(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, bodyIdentityVerified)
	status, perr := postMethod(t, h, methods.MethodMCPReadResource, scopeBody("t1", "u1", "s1"))
	if status != http.StatusOK {
		t.Fatalf("status = %d (code %q), want 200", status, perr.Code)
	}
	calls, got := surf.snapshot()
	if calls != 1 {
		t.Fatalf("AppsSurface.Dispatch called %d times, want 1", calls)
	}
	if got.Tenant != "t1" || got.User != "u1" || got.Session != "s1" {
		t.Errorf("dispatched scope = %+v, want t1/u1/s1", got)
	}
}

// TestAppsIdentity_EmptyBodyTriple_BackfilledFromCtx confirms the
// backfill still runs: an empty body triple is filled from the verified
// identity before dispatch.
func TestAppsIdentity_EmptyBodyTriple_BackfilledFromCtx(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, bodyIdentityVerified)
	status, perr := postMethod(t, h, methods.MethodMCPAppsToolContext, `{"identity":{}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (code %q), want 200", status, perr.Code)
	}
	calls, got := surf.snapshot()
	if calls != 1 {
		t.Fatalf("AppsSurface.Dispatch called %d times, want 1", calls)
	}
	if got.Tenant != "t1" || got.User != "u1" || got.Session != "s1" {
		t.Errorf("backfilled scope = %+v, want t1/u1/s1", got)
	}
}

// TestAppsIdentity_NoVerifiedIdentity_BodyAuthoritative pins the
// unchanged no-middleware posture: with no verified identity in ctx the
// body triple is authoritative and the adapter is a no-op.
func TestAppsIdentity_NoVerifiedIdentity_BodyAuthoritative(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, identity.Identity{})
	status, perr := postMethod(t, h, methods.MethodMCPReadResource, scopeBody("t9", "u9", "s9"))
	if status != http.StatusOK {
		t.Fatalf("status = %d (code %q), want 200", status, perr.Code)
	}
	calls, got := surf.snapshot()
	if calls != 1 {
		t.Fatalf("AppsSurface.Dispatch called %d times, want 1", calls)
	}
	if got.Tenant != "t9" {
		t.Errorf("dispatched scope = %+v, want tenant t9", got)
	}
}

// ---------------------------------------------------------------------
// MCP-Connections surface
// ---------------------------------------------------------------------

// TestMCPIdentity_BodyTenantMismatch_Rejected pins the full-triple
// reconciliation on `mcp.servers.*`: a body naming a tenant other than
// the verified one is refused with CodeIdentityRequired and never
// reaches the MCPSurface.
func TestMCPIdentity_BodyTenantMismatch_Rejected(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, bodyIdentityVerified)
	status, perr := postMethod(t, h, methods.MethodMCPServersList, scopeBody("t2", "u1", "s1"))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if perr.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeIdentityRequired)
	}
	if calls, _ := surf.snapshot(); calls != 0 {
		t.Errorf("MCPSurface.Dispatch called %d times, want 0", calls)
	}
}

// TestMCPIdentity_BodySessionMismatch_Rejected keeps the user/session
// leg of the reconciliation covered alongside the tenant leg.
func TestMCPIdentity_BodySessionMismatch_Rejected(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, bodyIdentityVerified)
	status, perr := postMethod(t, h, methods.MethodMCPServersList, scopeBody("t1", "u1", "s2"))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if perr.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeIdentityRequired)
	}
	if calls, _ := surf.snapshot(); calls != 0 {
		t.Errorf("MCPSurface.Dispatch called %d times, want 0", calls)
	}
}

// TestMCPIdentity_BodyMatchesVerified_Dispatched confirms the matching
// triple is the accepted path.
func TestMCPIdentity_BodyMatchesVerified_Dispatched(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, bodyIdentityVerified)
	status, perr := postMethod(t, h, methods.MethodMCPServersList, scopeBody("t1", "u1", "s1"))
	if status != http.StatusOK {
		t.Fatalf("status = %d (code %q), want 200", status, perr.Code)
	}
	calls, got := surf.snapshot()
	if calls != 1 {
		t.Fatalf("MCPSurface.Dispatch called %d times, want 1", calls)
	}
	if got.Tenant != "t1" || got.User != "u1" || got.Session != "s1" {
		t.Errorf("dispatched scope = %+v, want t1/u1/s1", got)
	}
}

// TestMCPIdentity_EmptyBodyTriple_BackfilledFromCtx confirms the
// backfill still runs on the MCP surface.
func TestMCPIdentity_EmptyBodyTriple_BackfilledFromCtx(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, bodyIdentityVerified)
	status, perr := postMethod(t, h, methods.MethodMCPServersList, `{"identity":{}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (code %q), want 200", status, perr.Code)
	}
	calls, got := surf.snapshot()
	if calls != 1 {
		t.Fatalf("MCPSurface.Dispatch called %d times, want 1", calls)
	}
	if got.Tenant != "t1" || got.User != "u1" || got.Session != "s1" {
		t.Errorf("backfilled scope = %+v, want t1/u1/s1", got)
	}
}

// TestMCPIdentity_NoVerifiedIdentity_BodyAuthoritative pins the
// unchanged no-middleware posture on the MCP surface.
func TestMCPIdentity_NoVerifiedIdentity_BodyAuthoritative(t *testing.T) {
	t.Parallel()
	surf := &recordingSurface{}
	h := newBodyIdentityHandler(t, surf, identity.Identity{})
	status, perr := postMethod(t, h, methods.MethodMCPServersList, scopeBody("t9", "u9", "s9"))
	if status != http.StatusOK {
		t.Fatalf("status = %d (code %q), want 200", status, perr.Code)
	}
	calls, got := surf.snapshot()
	if calls != 1 {
		t.Fatalf("MCPSurface.Dispatch called %d times, want 1", calls)
	}
	if got.Tenant != "t9" {
		t.Errorf("dispatched scope = %+v, want tenant t9", got)
	}
}
