package control_test

// auth_integration_test.go — Phase 61 ctx-identity coverage tests for
// the control handler.
//
// Phase 60's control handler reads identity from the request body's
// IdentityScope. Phase 61 adds a defence-in-depth check: when the
// auth.Middleware ran before us and r.Context() carries a verified
// identity, the body's IdentityScope MUST match the verified one (or
// be empty, in which case the handler backfills from ctx). These tests
// cover the four ctx-identity branches:
//
//   (1) ctx-identity present + body identity empty  → backfill, success
//   (2) ctx-identity present + body identity matches → success
//   (3) ctx-identity present + body identity mismatches → 401
//   (4) ctx-identity absent (Phase 60 trust-based) → no-op, body wins
//
// We exercise these by attaching identity to ctx via identity.With
// (the same shape auth.Middleware uses) and serving the request
// directly through the control.Handler — no transport wiring needed.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func startBody(tenant, user, session string) string {
	return `{"identity":{"tenant":"` + tenant + `","user":"` + user + `","session":"` + session + `"},"query":"q"}`
}

// (1) ctx-identity present + body identity empty → backfill, success.
func TestControl_AuthCtx_BodyEmpty_Backfilled(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx, err := identity.With(t.Context(), identity.Identity{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	body := `{"identity":{},"query":"q"}`
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/control/start", strings.NewReader(body))
	req.SetPathValue("method", string(methods.MethodStart))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var sr types.StartResponse
	if err := json.NewDecoder(w.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr.TaskID == "" {
		t.Errorf("empty TaskID")
	}
}

// (2) ctx-identity present + body identity matches → success.
func TestControl_AuthCtx_BodyMatches_Success(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx, err := identity.With(t.Context(), identity.Identity{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	body := startBody("t1", "u1", "s1")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/control/start", strings.NewReader(body))
	req.SetPathValue("method", string(methods.MethodStart))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// (3) ctx-identity present + body identity mismatch → 401.
func TestControl_AuthCtx_BodyMismatch_Rejected(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx, err := identity.With(t.Context(), identity.Identity{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	body := startBody("t-evil", "u-evil", "s-evil")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/control/start", strings.NewReader(body))
	req.SetPathValue("method", string(methods.MethodStart))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// (3b) Same with a control method (the request type carries an
// IdentityScope too).
func TestControl_AuthCtx_ControlMethod_BodyMismatch_Rejected(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx, err := identity.With(t.Context(), identity.Identity{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	body := `{"identity":{"tenant":"t-evil","user":"u-evil","session":"s-evil","run":"r1","scope":"session_user"}}`
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/control/cancel", strings.NewReader(body))
	req.SetPathValue("method", string(methods.MethodCancel))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// (4) ctx-identity absent → Phase 60 trust-based posture preserved
// (the body identity wins; the assertion is a no-op).
func TestControl_NoAuthCtx_BodyIdentityWins(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := startBody("t1", "u1", "s1")
	req := httptest.NewRequest(http.MethodPost, "/v1/control/start", strings.NewReader(body))
	req.SetPathValue("method", string(methods.MethodStart))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// (5) ctx-identity present + control method body empty → backfill
// (mirrors (1) for the control-request shape).
func TestControl_AuthCtx_ControlMethod_BodyEmpty_Backfilled(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx, err := identity.With(t.Context(), identity.Identity{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	// Empty IdentityScope but a Run is required for a control — the
	// backfill targets the (tenant, user, session) only.
	body := `{"identity":{"run":"r1","scope":"session_user"}}`
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/control/cancel", strings.NewReader(body))
	req.SetPathValue("method", string(methods.MethodCancel))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	// The backfill happens — Dispatch is then called with the (now-
	// matching) identity. The control will fail downstream with
	// CodeNotFound because there is no live inbox for run `r1`, but
	// the auth-related backfill path is still exercised. Either 404
	// or another non-401 status confirms we got past the identity
	// gate.
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("backfill failed: got 401; body=%s", w.Body.String())
	}
}
