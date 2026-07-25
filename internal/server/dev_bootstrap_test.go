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
	"github.com/hurtener/Harbor/internal/protocol/transports/cors"
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

// newBootstrapRequest builds a POST at the bootstrap path addressed to a
// local authority — the shape a co-resident Console / CLI caller sends.
// httptest.NewRequest defaults Host to "example.com", which the
// handler's local-host gate refuses.
func newBootstrapRequest(body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", body)
	req.Host = "127.0.0.1:18080"
	return req
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
	req := newBootstrapRequest(nil)
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
	req := newBootstrapRequest(nil)
	req.RemoteAddr = "[::1]:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrap_NonLoopback_Returns403(t *testing.T) {
	h := newTestHandler()
	req := newBootstrapRequest(nil)
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
	req := newBootstrapRequest(nil)
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

	req := newBootstrapRequest(nil)
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
	req := newBootstrapRequest(nil)
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
	req := newBootstrapRequest(rdr)
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
			req := newBootstrapRequest(nil)
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

// TestBootstrap_LocalHostHeader_Allowed pins the Host allowlist's accept
// side: every authority that names the local machine — the literal name
// "localhost" or a loopback IP literal, with or without a port and with
// IPv6 brackets — is served, so `harbor dev`, the Console's one-click
// attach, and a plain curl all keep working.
func TestBootstrap_LocalHostHeader_Allowed(t *testing.T) {
	for _, host := range []string{
		"localhost",
		"localhost:18080",
		"LOCALHOST:18080",
		"127.0.0.1",
		"127.0.0.1:18080",
		"127.0.0.53:18080",
		"[::1]:18080",
		"::1",
		"localhost.",
		"localhost.:18080",
	} {
		t.Run(host, func(t *testing.T) {
			h := newTestHandler()
			req := newBootstrapRequest(nil)
			req.Host = host
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("Host %q: expected 200, got %d: %s", host, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestBootstrap_ForeignHostHeader_Refused pins the Host allowlist's
// refusal side: a request that arrives over loopback but is addressed to
// an external authority (or carries no Host at all) is not a local
// caller's request and gets 403.
func TestBootstrap_ForeignHostHeader_Refused(t *testing.T) {
	for _, host := range []string{
		"harbor.example.com",
		"harbor.example.com:18080",
		"localhost.example.com:18080",
		"203.0.113.7:18080",
		"",
	} {
		t.Run("host="+host, func(t *testing.T) {
			h := newTestHandler()
			req := newBootstrapRequest(nil)
			req.Host = host
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("Host %q: expected 403, got %d: %s", host, rec.Code, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal error body: %v", err)
			}
			if body["code"] != "forbidden" {
				t.Errorf("expected code forbidden, got %q", body["code"])
			}
		})
	}
}

// TestBootstrap_CrossOriginHeadersStripped composes the handler behind
// the real CORS middleware with the dev-only any-origin flag set — the
// most permissive posture an operator can configure — and asserts the
// bootstrap response still carries no allow-origin / allow-credentials
// headers, so the envelope stays readable to same-origin callers only.
func TestBootstrap_CrossOriginHeadersStripped(t *testing.T) {
	wrapped := cors.Wrap(newTestHandler(), cors.Config{DevAllowAny: true})
	req := newBootstrapRequest(nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set(cors.HeaderOrigin, "https://elsewhere.example.com")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("%s = %q, want empty", cors.HeaderAccessControlAllowOrigin, got)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowCredentials); got != "" {
		t.Errorf("%s = %q, want empty", cors.HeaderAccessControlAllowCredentials, got)
	}
	// The response body is still served — the endpoint is not refused,
	// it is simply not exposed to a cross-origin reader.
	if resp := mustDecodeBootstrap(t, rec.Body.Bytes()); resp.Token == "" {
		t.Error("token empty")
	}
}

// TestBootstrap_CORSWrapped_SameOriginUnaffected proves the strip does
// not change what a same-origin caller sees — the Console's attach flow
// fetches from window.location.origin and never needed the headers.
func TestBootstrap_CORSWrapped_SameOriginUnaffected(t *testing.T) {
	wrapped := cors.Wrap(newTestHandler(), cors.Config{DevAllowAny: true})
	req := newBootstrapRequest(nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := mustDecodeBootstrap(t, rec.Body.Bytes())
	if resp.Token == "" || resp.BaseURL == "" {
		t.Errorf("envelope incomplete: %+v", resp)
	}
}

// TestIsLocalHostHeader_Table covers the authority parser directly,
// including the shapes the handler tests do not drive end-to-end.
func TestIsLocalHostHeader_Table(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"localhost:8080", true},
		{"LocalHost", true},
		// Trailing root dot — the fully-qualified form a browser sends
		// for `http://localhost./`.
		{"localhost.", true},
		{"localhost.:8080", true},
		{"127.0.0.1.", true},
		// Only ONE root label is stripped.
		{"localhost..", false},
		{"example.com.", false},
		{"127.0.0.1", true},
		{"127.0.0.1:1", true},
		{"127.1.2.3", true},
		{"::1", true},
		{"[::1]", true},
		{"[::1]:8080", true},
		{"", false},
		{"example.com", false},
		{"example.com:8080", false},
		{"notlocalhost", false},
		{"192.168.0.1:8080", false},
		{"[2001:db8::1]:8080", false},
	} {
		if got := isLocalHostHeader(tc.host); got != tc.want {
			t.Errorf("isLocalHostHeader(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
