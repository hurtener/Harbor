package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// authHandlerID is a documented dummy identity triple — no secrets.
var authHandlerID = identity.Identity{TenantID: "t-ah", UserID: "u-ah", SessionID: "s-ah"}

// authTestIssuer is the in-test auth.TokenIssuer for the auth-handler
// tests. It re-mints a deterministic token keyed on the identity.
type authTestIssuer struct{}

func (authTestIssuer) IssueToken(_ context.Context, id identity.Identity, _ []auth.Scope, now time.Time) (string, time.Time, error) {
	return "rotated-" + id.TenantID, now.Add(time.Hour), nil
}

// newAuthHandler builds an AuthHandler over a real RotateSurface.
func newAuthHandler(t *testing.T) *stream.AuthHandler {
	t.Helper()
	surface, err := auth.NewRotateSurface(authTestIssuer{}, auditpatterns.New())
	if err != nil {
		t.Fatalf("NewRotateSurface: %v", err)
	}
	h, err := stream.NewAuthHandler(surface)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}
	return h
}

// doAuthRequest issues a POST /v1/auth/{verb} against the handler.
func doAuthRequest(t *testing.T, h http.Handler, verb, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/"+verb, strings.NewReader(body))
	req.SetPathValue("method", verb)
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

func TestNewAuthHandler_NilSurface_FailsLoudly(t *testing.T) {
	if _, err := stream.NewAuthHandler(nil); err == nil {
		t.Fatal("NewAuthHandler(nil) did not fail")
	}
}

func TestAuthHandler_RotateToken_HappyPath(t *testing.T) {
	h := newAuthHandler(t)
	code, body := doAuthRequest(t, h, "rotate_token", "{}", &authHandlerID,
		[]auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("rotate_token status = %d, want 200; body=%s", code, body)
	}
	var resp prototypes.AuthRotateTokenResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.NewToken != "rotated-"+authHandlerID.TenantID {
		t.Errorf("NewToken = %q, want rotated-%s", resp.NewToken, authHandlerID.TenantID)
	}
}

func TestAuthHandler_RotateToken_RejectsWithoutAdminScope(t *testing.T) {
	h := newAuthHandler(t)
	// Identity present, but no admin scope claim — must be 403.
	code, body := doAuthRequest(t, h, "rotate_token", "{}", &authHandlerID, nil)
	if code != http.StatusForbidden {
		t.Fatalf("rotate_token without admin scope status = %d, want 403; body=%s", code, body)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeIdentityScopeRequired {
		t.Errorf("error code = %q, want %q", e.Code, protoerrors.CodeIdentityScopeRequired)
	}
}

func TestAuthHandler_RotateToken_RejectsMissingIdentity(t *testing.T) {
	h := newAuthHandler(t)
	code, _ := doAuthRequest(t, h, "rotate_token", "{}", nil, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusUnauthorized {
		t.Errorf("rotate_token without identity status = %d, want 401", code)
	}
}

func TestAuthHandler_UnknownMethod_404(t *testing.T) {
	h := newAuthHandler(t)
	code, _ := doAuthRequest(t, h, "bogus", "{}", &authHandlerID, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusNotFound {
		t.Errorf("unknown auth method status = %d, want 404", code)
	}
}

func TestAuthHandler_RejectsNonPOST(t *testing.T) {
	h := newAuthHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/rotate_token", nil)
	req.SetPathValue("method", "rotate_token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}

func TestAuthHandler_MalformedBody_400(t *testing.T) {
	h := newAuthHandler(t)
	code, _ := doAuthRequest(t, h, "rotate_token", "{not json", &authHandlerID,
		[]auth.Scope{auth.ScopeAdmin})
	if code != http.StatusBadRequest {
		t.Errorf("malformed body status = %d, want 400", code)
	}
}

// TestAuthHandler_ConcurrentReuse exercises N≥100 concurrent requests
// against a single shared AuthHandler under -race (D-025).
func TestAuthHandler_ConcurrentReuse(t *testing.T) {
	h := newAuthHandler(t)
	const n = 120
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{TenantID: "t" + itoaAH(i), UserID: "u", SessionID: "s"}
			code, _ := doAuthRequest(t, h, "rotate_token", "{}", &id,
				[]auth.Scope{auth.ScopeAdmin})
			codes[i] = code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("run %d status = %d, want 200", i, c)
		}
	}
}

func itoaAH(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
