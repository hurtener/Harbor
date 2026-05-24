package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// echoAuthHandler replies 200 with the request's Authorization header
// verbatim so each test can assert what reached the wrapped handler.
func echoAuthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Authorization", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	})
}

func TestSSEAccessTokenShim_NilNextPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil next handler")
		}
	}()
	_ = auth.SSEAccessTokenShim(nil)
}

func TestSSEAccessTokenShim_PromotesQueryParamToHeader(t *testing.T) {
	h := auth.SSEAccessTokenShim(echoAuthHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/events?access_token=jwt-xyz&type=task.completed", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Echo-Authorization"); got != "Bearer jwt-xyz" {
		t.Errorf("expected synthesized header, got %q", got)
	}
	// Original request must not be mutated — the test guarantees the
	// shim cloned the request rather than mutating in place.
	if req.Header.Get("Authorization") != "" {
		t.Errorf("shim mutated the original request header: %q", req.Header.Get("Authorization"))
	}
}

func TestSSEAccessTokenShim_PrefersExistingAuthorization(t *testing.T) {
	h := auth.SSEAccessTokenShim(echoAuthHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/events?access_token=query-jwt", nil)
	req.Header.Set("Authorization", "Bearer header-jwt")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Echo-Authorization"); got != "Bearer header-jwt" {
		t.Errorf("explicit header must win over query param; got %q", got)
	}
}

func TestSSEAccessTokenShim_NoTokenNoChange(t *testing.T) {
	h := auth.SSEAccessTokenShim(echoAuthHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Echo-Authorization"); got != "" {
		t.Errorf("expected empty Authorization, got %q", got)
	}
}

func TestSSEAccessTokenShim_EmptyQueryNoChange(t *testing.T) {
	h := auth.SSEAccessTokenShim(echoAuthHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/events?access_token=", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Echo-Authorization"); got != "" {
		t.Errorf("empty access_token must not synthesize a Bearer header; got %q", got)
	}
}
