package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// staticSigner is a test-grade BootstrapSigner that returns a stable
// token per identity input so fresh-token assertions are deterministic.
type staticSigner struct {
	mu     sync.Mutex
	calls  int
	prefix string
}

func (s *staticSigner) SignDevToken(_ time.Time, tenant, user, session string, scopes []string) (string, error) {
	s.mu.Lock()
	s.calls++
	c := s.calls
	s.mu.Unlock()
	payload := strings.Repeat("A", 200)
	return s.prefix + "." + payload + ".sig" + strconv.Itoa(c), nil
}

var testLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

func newTestHandler() *BootstrapHandler {
	return NewBootstrapHandler(
		&staticSigner{prefix: "hdr"},
		identity.Identity{TenantID: "dev", UserID: "dev", SessionID: "dev"},
		[]string{"admin", "console:fleet"},
		"http://127.0.0.1:18080",
		testLogger,
	)
}

func mustDecodeBootstrap(t *testing.T, body []byte) BootstrapResponse {
	t.Helper()
	var resp BootstrapResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal BootstrapResponse: %v", err)
	}
	return resp
}

func TestBootstrap_Loopback_127001_Returns200(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := mustDecodeBootstrap(t, rec.Body.Bytes())
	if resp.BaseURL == "" {
		t.Error("base_url empty")
	}
	if resp.Token == "" {
		t.Error("token empty")
	}
	if resp.Identity.Tenant != "dev" || resp.Identity.User != "dev" || resp.Identity.Session != "dev" {
		t.Errorf("identity mismatch: %+v", resp.Identity)
	}
	if len(resp.Scopes) < 2 {
		t.Errorf("scopes too short: %v", resp.Scopes)
	}
	if resp.ProtocolVersion == "" {
		t.Error("protocol_version empty")
	}
}

func TestBootstrap_Loopback_IPv6_Returns200(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", nil)
	req.RemoteAddr = "[::1]:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrap_NonLoopback_Returns403(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", nil)
	req.RemoteAddr = "192.168.1.5:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if body["code"] != "forbidden" {
		t.Errorf("expected code forbidden, got %q", body["code"])
	}
}

func TestBootstrap_SpoofedXForwardedFor_StillReturns403(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", nil)
	req.RemoteAddr = "192.168.1.5:12345"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 despite spoofed header, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrap_TokenIsFreshPerCall(t *testing.T) {
	h := newTestHandler()
	var token1, token2 string

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	token1 = mustDecodeBootstrap(t, rec.Body.Bytes()).Token

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	token2 = mustDecodeBootstrap(t, rec.Body.Bytes()).Token

	if token1 == token2 {
		t.Error("tokens from two calls are identical — expected fresh mint per call")
	}
}

func TestBootstrap_ResponseShape(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	resp := mustDecodeBootstrap(t, rec.Body.Bytes())

	if resp.BaseURL == "" {
		t.Error("base_url is empty")
	}
	if resp.Token == "" {
		t.Error("token is empty")
	}
	if resp.Identity.Tenant == "" {
		t.Error("identity.tenant is empty")
	}
	if resp.Identity.User == "" {
		t.Error("identity.user is empty")
	}
	if resp.Identity.Session == "" {
		t.Error("identity.session is empty")
	}
	if len(resp.Scopes) == 0 {
		t.Error("scopes is empty")
	}
	if resp.ProtocolVersion == "" {
		t.Error("protocol_version is empty")
	}
}

// postBootstrap drives the handler with a loopback peer + the given body
// and returns the recorder.
func postBootstrap(t *testing.T, h *BootstrapHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", rdr)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestBootstrap_DefaultBody_MintsAdmin confirms the existing one-click
// Console-attach flow is preserved: an empty `{}` body mints the
// handler's default admin dev token. This guards the Phase-114 smoke and
// every other caller that posts `-d '{}'`.
func TestBootstrap_DefaultBody_MintsAdmin(t *testing.T) {
	h := newTestHandler()
	rec := postBootstrap(t, h, "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := mustDecodeBootstrap(t, rec.Body.Bytes())
	if resp.Identity.Tenant != "dev" || resp.Identity.User != "dev" || resp.Identity.Session != "dev" {
		t.Errorf("default identity = %+v, want dev/dev/dev", resp.Identity)
	}
	var hasAdmin bool
	for _, s := range resp.Scopes {
		if s == "admin" {
			hasAdmin = true
		}
	}
	if !hasAdmin {
		t.Errorf("default scopes %v missing admin", resp.Scopes)
	}
}

// TestBootstrap_NonAdminScopes_MintsLesserPrivilegedToken is the
// load-bearing dev-mint assertion for the non-admin token contract: an
// explicit empty `scopes` array mints a token with NO scopes (a non-admin
// token), while the identity override targets a chosen principal. The
// response echoes the minted identity + (empty) scope set.
func TestBootstrap_NonAdminScopes_MintsLesserPrivilegedToken(t *testing.T) {
	h := newTestHandler()
	rec := postBootstrap(t, h, `{"tenant":"acme","user":"alice","session":"s1","scopes":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := mustDecodeBootstrap(t, rec.Body.Bytes())
	if resp.Token == "" {
		t.Error("token empty")
	}
	if resp.Identity.Tenant != "acme" || resp.Identity.User != "alice" || resp.Identity.Session != "s1" {
		t.Errorf("identity = %+v, want acme/alice/s1", resp.Identity)
	}
	if len(resp.Scopes) != 0 {
		t.Errorf("scopes = %v, want empty (non-admin token)", resp.Scopes)
	}
}

// TestBootstrap_NonAdminScopes_DefaultIdentity proves the scope override
// is independent of the identity override: an empty `scopes` with no
// identity fields mints a non-admin token for the DEFAULT dev identity.
func TestBootstrap_NonAdminScopes_DefaultIdentity(t *testing.T) {
	h := newTestHandler()
	rec := postBootstrap(t, h, `{"scopes":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := mustDecodeBootstrap(t, rec.Body.Bytes())
	if resp.Identity.User != "dev" {
		t.Errorf("identity = %+v, want default dev", resp.Identity)
	}
	if len(resp.Scopes) != 0 {
		t.Errorf("scopes = %v, want empty", resp.Scopes)
	}
}

// TestBootstrap_PartialIdentity_Rejected proves identity is mandatory: a
// partial triple (tenant only) fails closed with 400 rather than minting
// a token with a half-empty identity.
func TestBootstrap_PartialIdentity_Rejected(t *testing.T) {
	h := newTestHandler()
	rec := postBootstrap(t, h, `{"tenant":"acme"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for partial identity, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if body["code"] != "invalid_request" {
		t.Errorf("code = %q, want invalid_request", body["code"])
	}
}

// TestBootstrap_MalformedBody_Rejected proves a non-JSON body fails closed
// with 400 (no silent fall-through to the default mint).
func TestBootstrap_MalformedBody_Rejected(t *testing.T) {
	h := newTestHandler()
	rec := postBootstrap(t, h, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed body, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrap_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	h := newTestHandler()
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)

	errs := make(chan error, n)
	for range n {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errs <- fmt.Errorf("expected 200, got %d", rec.Code)
				return
			}
			resp := mustDecodeBootstrap(t, rec.Body.Bytes())
			if resp.Token == "" {
				errs <- fmt.Errorf("token empty")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
