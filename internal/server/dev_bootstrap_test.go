package server

import (
	"encoding/json"
	"fmt"
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
