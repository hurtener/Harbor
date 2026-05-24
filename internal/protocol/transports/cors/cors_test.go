package cors_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/transports/cors"
)

// echoHandler always replies 200 with a tiny body so the test can
// distinguish "the middleware emitted the headers" from "the middleware
// did NOT reach next".
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

func TestWrap_NilNextPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil next handler")
		}
	}()
	_ = cors.Wrap(nil, cors.Config{})
}

func TestWrap_SameOrigin_NoHeadersEmitted(t *testing.T) {
	h := cors.Wrap(echoHandler(), cors.Config{
		AllowedOrigins: []string{"https://console.example.com"},
	})
	// No Origin header → same-origin request → middleware is invisible.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("allow-origin emitted on same-origin request: %q", got)
	}
	if got := rec.Header().Get(cors.HeaderVary); got != "" {
		t.Errorf("Vary header emitted on same-origin request: %q", got)
	}
}

func TestWrap_AllowedOrigin_EmitsHeaders(t *testing.T) {
	origin := "https://console.example.com"
	h := cors.Wrap(echoHandler(), cors.Config{
		AllowedOrigins: []string{origin, "https://other.example.com:8443"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/control/foo", nil)
	req.Header.Set(cors.HeaderOrigin, origin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != origin {
		t.Errorf("allow-origin: got %q, want %q", got, origin)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowCredentials); got != "true" {
		t.Errorf("allow-credentials: got %q, want true", got)
	}
	// Never `*` — the credentials posture forbids it.
	if strings.Contains(rec.Header().Get(cors.HeaderAccessControlAllowOrigin), "*") {
		t.Error("allow-origin contained '*' on credentialed response")
	}
	if got := rec.Header().Get(cors.HeaderVary); got != cors.HeaderOrigin {
		t.Errorf("Vary: got %q, want %s", got, cors.HeaderOrigin)
	}
}

func TestWrap_NonAllowedOrigin_EmitsNoAllowHeaders(t *testing.T) {
	h := cors.Wrap(echoHandler(), cors.Config{
		AllowedOrigins: []string{"https://console.example.com"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/control/foo", nil)
	req.Header.Set(cors.HeaderOrigin, "https://attacker.example.org")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// next STILL runs — the browser does the enforcement, not the
	// server. The server just doesn't emit the allow headers, so the
	// browser blocks the response from reaching the script.
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("allow-origin emitted for non-allowed origin: %q", got)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowCredentials); got != "" {
		t.Errorf("allow-credentials emitted for non-allowed origin: %q", got)
	}
	// Vary still set so caches behave correctly.
	if got := rec.Header().Get(cors.HeaderVary); got != cors.HeaderOrigin {
		t.Errorf("Vary: got %q, want %s", got, cors.HeaderOrigin)
	}
}

func TestWrap_PreflightAllowed_Returns204WithHeaders(t *testing.T) {
	origin := "https://console.example.com"
	h := cors.Wrap(echoHandler(), cors.Config{
		AllowedOrigins: []string{origin},
	})
	req := httptest.NewRequest(http.MethodOptions, "/v1/control/foo", nil)
	req.Header.Set(cors.HeaderOrigin, origin)
	req.Header.Set(cors.HeaderAccessControlRequestMethod, "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != origin {
		t.Errorf("allow-origin: got %q, want %q", got, origin)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowMethods); got == "" {
		t.Error("allow-methods missing on preflight match")
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowHeaders); got == "" {
		t.Error("allow-headers missing on preflight match")
	}
	if got := rec.Header().Get(cors.HeaderAccessControlMaxAge); got == "" {
		t.Error("max-age missing on preflight match")
	}
}

func TestWrap_PreflightDenied_Returns204WithNoHeaders(t *testing.T) {
	h := cors.Wrap(echoHandler(), cors.Config{
		AllowedOrigins: []string{"https://console.example.com"},
	})
	req := httptest.NewRequest(http.MethodOptions, "/v1/control/foo", nil)
	req.Header.Set(cors.HeaderOrigin, "https://attacker.example.org")
	req.Header.Set(cors.HeaderAccessControlRequestMethod, "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("allow-origin emitted on denied preflight: %q", got)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowMethods); got != "" {
		t.Errorf("allow-methods emitted on denied preflight: %q", got)
	}
}

func TestWrap_EmptyAllowlist_DefaultDeny(t *testing.T) {
	// The pre-83v posture: no operator config → no CORS. Every
	// cross-origin request is blocked at the browser. The middleware
	// passes the request through to next but emits no CORS headers.
	h := cors.Wrap(echoHandler(), cors.Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/control/foo", nil)
	req.Header.Set(cors.HeaderOrigin, "https://console.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("default-deny posture leaked allow-origin: %q", got)
	}
}

func TestWrap_DevAllowAny_AcceptsAnyOrigin(t *testing.T) {
	// The dev-only escape hatch — accepts any origin. Only reachable
	// after the operator explicitly opts in via the validator-gated
	// `server.cors_dev_allow_any: true` flag + stderr banner.
	h := cors.Wrap(echoHandler(), cors.Config{DevAllowAny: true})
	for _, origin := range []string{
		"http://127.0.0.1:18790",
		"https://random.example.com",
		"http://[::1]:5173",
	} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set(cors.HeaderOrigin, origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != origin {
			t.Errorf("DevAllowAny: allow-origin echo: got %q, want %q", got, origin)
		}
		if got := rec.Header().Get(cors.HeaderAccessControlAllowCredentials); got != "true" {
			t.Errorf("DevAllowAny: allow-credentials: got %q, want true", got)
		}
		// Even with DevAllowAny we NEVER emit `*` — the response would
		// be incompatible with credentialed requests.
		if rec.Header().Get(cors.HeaderAccessControlAllowOrigin) == "*" {
			t.Error("DevAllowAny: emitted '*' — would break credentialed responses")
		}
	}
}

func TestWrap_AllowlistEntries_Whitespace_Tolerated(t *testing.T) {
	// Operator yaml might have stray whitespace; the middleware trims.
	origin := "https://console.example.com"
	h := cors.Wrap(echoHandler(), cors.Config{
		AllowedOrigins: []string{"  " + origin + "  ", "", "   "},
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(cors.HeaderOrigin, origin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin); got != origin {
		t.Errorf("allow-origin after trim: got %q, want %q", got, origin)
	}
}

// TestWrap_ConcurrentReuse_NoCrossTalk pins the D-025 contract: the
// middleware is shared across N concurrent goroutines, the allowlist is
// read-only, no data races, no goroutine leak. CLAUDE.md §11 mandates
// N≥100 against a single instance under -race; we run 200.
func TestWrap_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	t.Parallel()
	h := cors.Wrap(echoHandler(), cors.Config{
		AllowedOrigins: []string{
			"https://console-a.example.com",
			"https://console-b.example.com",
		},
	})

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			var origin string
			var wantMatch bool
			switch i % 3 {
			case 0:
				origin = "https://console-a.example.com"
				wantMatch = true
			case 1:
				origin = "https://console-b.example.com"
				wantMatch = true
			default:
				origin = "https://attacker.example.org"
				wantMatch = false
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/control/x", nil)
			req.Header.Set(cors.HeaderOrigin, origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			got := rec.Header().Get(cors.HeaderAccessControlAllowOrigin)
			if wantMatch && got != origin {
				t.Errorf("goroutine %d: allow-origin: got %q, want %q", i, got, origin)
			}
			if !wantMatch && got != "" {
				t.Errorf("goroutine %d: allow-origin leaked: got %q (origin %q)", i, got, origin)
			}
		}()
	}
	wg.Wait()
}
