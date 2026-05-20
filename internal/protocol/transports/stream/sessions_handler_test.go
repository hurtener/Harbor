package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
)

// sessionsHandlerID is a documented dummy identity triple — no secrets.
var sessionsHandlerID = identity.Identity{TenantID: "t-sh", UserID: "u-sh", SessionID: "s-sh"}

// sessionsFakeProjector is the in-test Projector backing the handler
// tests. It is the deterministic stand-in a wire-level unit test needs;
// the integration test uses the real registry.
type sessionsFakeProjector struct {
	rows []prototypes.SessionRow
}

func (p *sessionsFakeProjector) ListSessions(_ context.Context, id identity.Identity, f prototypes.SessionFilter, adminScoped bool) ([]prototypes.SessionRow, error) {
	out := []prototypes.SessionRow{}
	for _, r := range p.rows {
		if !adminScoped {
			if r.TenantID != id.TenantID {
				continue
			}
		} else if len(f.TenantIDs) > 0 {
			match := false
			for _, t := range f.TenantIDs {
				if t == r.TenantID {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (p *sessionsFakeProjector) InspectSession(_ context.Context, id identity.Identity, sessionID string, adminScoped bool) (prototypes.SessionsInspectResponse, error) {
	for _, r := range p.rows {
		if r.SessionID != sessionID {
			continue
		}
		if !adminScoped && r.TenantID != id.TenantID {
			continue
		}
		return prototypes.SessionsInspectResponse{
			Row:                 r,
			RecentInterventions: []prototypes.InterventionSummary{},
			RecentArtifacts:     []prototypes.ArtifactRefSummary{},
		}, nil
	}
	return prototypes.SessionsInspectResponse{}, sessionsprotocol.ErrSessionNotFound
}

func newSessionsHandler(t *testing.T) *stream.SessionsHandler {
	t.Helper()
	base := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	rows := []prototypes.SessionRow{
		{
			SessionID: "s-sh", Status: prototypes.SessionStatusRunning,
			UserID: "u-sh", TenantID: "t-sh", StartedAt: base, LastActivityAt: base.Add(time.Minute),
			Identity: prototypes.IdentityScope{Tenant: "t-sh", User: "u-sh", Session: "s-sh"},
		},
		{
			SessionID: "s-other", Status: prototypes.SessionStatusCompleted,
			UserID: "u-x", TenantID: "t-other", StartedAt: base, LastActivityAt: base.Add(time.Minute),
			Identity: prototypes.IdentityScope{Tenant: "t-other", User: "u-x", Session: "s-other"},
		},
	}
	svc, err := sessionsprotocol.NewService(&sessionsFakeProjector{rows: rows})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewSessionsHandler(svc)
	if err != nil {
		t.Fatalf("NewSessionsHandler: %v", err)
	}
	return h
}

// doSessionsRequest issues a POST /v1/sessions/{verb} against the
// handler. id (when non-nil) is carried via the X-Harbor-* headers;
// scopes (when non-nil) are injected into the request context.
func doSessionsRequest(t *testing.T, h http.Handler, verb, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+verb, strings.NewReader(body))
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestNewSessionsHandler_NilService_FailsLoudly(t *testing.T) {
	if _, err := stream.NewSessionsHandler(nil); err == nil {
		t.Fatal("NewSessionsHandler(nil) did not fail")
	}
}

func TestSessionsHandler_List_HappyPath(t *testing.T) {
	h := newSessionsHandler(t)
	code, body := doSessionsRequest(t, h, "list", `{"filter":{}}`, &sessionsHandlerID, nil)
	if code != http.StatusOK {
		t.Fatalf("list: code = %d, body = %s", code, body)
	}
	var resp prototypes.SessionsListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].TenantID != "t-sh" {
		t.Fatalf("list rows = %+v, want a single t-sh row", resp.Rows)
	}
}

func TestSessionsHandler_List_MissingIdentity_401(t *testing.T) {
	h := newSessionsHandler(t)
	code, body := doSessionsRequest(t, h, "list", `{}`, nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("missing-identity list: code = %d, want 401, body = %s", code, body)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(body, &perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeIdentityRequired {
		t.Fatalf("missing-identity code = %q, want %q", perr.Code, protoerrors.CodeIdentityRequired)
	}
}

func TestSessionsHandler_List_CrossTenantWithoutAdmin_403(t *testing.T) {
	h := newSessionsHandler(t)
	code, body := doSessionsRequest(t, h, "list",
		`{"filter":{"tenant_ids":["t-other"]}}`, &sessionsHandlerID, nil)
	if code != http.StatusForbidden {
		t.Fatalf("cross-tenant list without admin: code = %d, want 403, body = %s", code, body)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(body, &perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("cross-tenant code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}
}

func TestSessionsHandler_List_CrossTenantWithAdmin_200(t *testing.T) {
	h := newSessionsHandler(t)
	code, body := doSessionsRequest(t, h, "list",
		`{"filter":{"tenant_ids":["t-other"]}}`, &sessionsHandlerID, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("cross-tenant list with admin: code = %d, want 200, body = %s", code, body)
	}
	var resp prototypes.SessionsListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].TenantID != "t-other" {
		t.Fatalf("admin cross-tenant list = %+v, want a single t-other row", resp.Rows)
	}
}

func TestSessionsHandler_List_MalformedCursor_400(t *testing.T) {
	h := newSessionsHandler(t)
	code, body := doSessionsRequest(t, h, "list",
		`{"cursor":"!!!not-base64!!!"}`, &sessionsHandlerID, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("malformed cursor: code = %d, want 400, body = %s", code, body)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(body, &perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("malformed cursor code = %q, want %q", perr.Code, protoerrors.CodeInvalidRequest)
	}
}

func TestSessionsHandler_List_UnknownField_400(t *testing.T) {
	h := newSessionsHandler(t)
	code, _ := doSessionsRequest(t, h, "list", `{"bogus":true}`, &sessionsHandlerID, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown-field list: code = %d, want 400", code)
	}
}

func TestSessionsHandler_Inspect_HappyPath(t *testing.T) {
	h := newSessionsHandler(t)
	code, body := doSessionsRequest(t, h, "inspect",
		`{"session_id":"s-sh"}`, &sessionsHandlerID, nil)
	if code != http.StatusOK {
		t.Fatalf("inspect: code = %d, body = %s", code, body)
	}
	var resp prototypes.SessionsInspectResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Row.SessionID != "s-sh" {
		t.Fatalf("inspect row = %q, want s-sh", resp.Row.SessionID)
	}
}

func TestSessionsHandler_Inspect_NotFound_404(t *testing.T) {
	h := newSessionsHandler(t)
	code, body := doSessionsRequest(t, h, "inspect",
		`{"session_id":"nope"}`, &sessionsHandlerID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("inspect missing: code = %d, want 404, body = %s", code, body)
	}
}

func TestSessionsHandler_UnknownRoute_404(t *testing.T) {
	h := newSessionsHandler(t)
	code, _ := doSessionsRequest(t, h, "bogus", `{}`, &sessionsHandlerID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown route: code = %d, want 404", code)
	}
}

func TestSessionsHandler_GET_405(t *testing.T) {
	h := newSessionsHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/list", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/sessions/list: code = %d, want 405", rec.Code)
	}
}

// TestSessionsHandler_ConcurrentReuse pins the D-025 contract at the
// wire layer: N≥100 concurrent requests against ONE shared handler.
func TestSessionsHandler_ConcurrentReuse(t *testing.T) {
	h := newSessionsHandler(t)
	const N = 120
	done := make(chan int, N)
	for i := 0; i < N; i++ {
		go func() {
			code, _ := doSessionsRequest(t, h, "list", `{"filter":{}}`, &sessionsHandlerID, nil)
			done <- code
		}()
	}
	for i := 0; i < N; i++ {
		if code := <-done; code != http.StatusOK {
			t.Errorf("concurrent list request %d: code = %d, want 200", i, code)
		}
	}
}
