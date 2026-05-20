package control_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// withIdentity wraps h so r.Context() carries the verified identity —
// it simulates the auth middleware having run, so the posture handler's
// identity-backfill path is exercised.
func withIdentity(h http.Handler, id identity.Identity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := identity.With(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// stubPosture is a minimal PostureSurface used to pin the
// posture_handler wiring path. Production tests of the posture surface
// live in internal/protocol/posture_test.go and
// test/integration/runtime_posture_test.go.
type stubPosture struct {
	resp any
	err  error
}

func (s *stubPosture) Dispatch(_ context.Context, _ methods.Method, _ any) (any, error) {
	return s.resp, s.err
}

func newPostureHandler(t *testing.T, surf control.PostureSurface) http.Handler {
	t.Helper()
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs, control.WithPostureSurface(surf))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	return mux
}

func TestPostureHandler_HappyPath_DispatchesToPostureSurface(t *testing.T) {
	t.Parallel()
	resp := &types.RuntimeInfo{
		InstanceID:      "inst-1",
		ProtocolVersion: "0.1.0",
		BuildVersion:    "v0.0.0-test",
		BuildGoVersion:  "go1.26",
		UptimeSeconds:   42,
	}
	srv := httptest.NewServer(newPostureHandler(t, &stubPosture{resp: resp}))
	defer srv.Close()

	body, _ := json.Marshal(types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
	})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/runtime.info", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(httpResp.Body)
		t.Fatalf("status: got %d, want 200, body=%s", httpResp.StatusCode, raw)
	}
	var got types.RuntimeInfo
	if err := json.NewDecoder(httpResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.InstanceID != "inst-1" {
		t.Errorf("InstanceID = %q, want inst-1", got.InstanceID)
	}
}

func TestPostureHandler_AllFiveMethodsRoute(t *testing.T) {
	t.Parallel()
	for _, m := range []methods.Method{
		methods.MethodRuntimeInfo, methods.MethodRuntimeHealth,
		methods.MethodRuntimeCounters, methods.MethodRuntimeDrivers,
		methods.MethodMetricsSnapshot,
	} {
		method := string(m)
		t.Run(method, func(t *testing.T) {
			srv := httptest.NewServer(newPostureHandler(t, &stubPosture{
				resp: &types.RuntimeInfo{ProtocolVersion: "0.1.0"},
			}))
			defer srv.Close()
			body, _ := json.Marshal(types.RuntimeInfoRequest{
				Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
			})
			r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/"+method, bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			httpResp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = httpResp.Body.Close() }()
			if httpResp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(httpResp.Body)
				t.Fatalf("%s: status %d, want 200, body=%s", method, httpResp.StatusCode, raw)
			}
		})
	}
}

// TestPostureHandler_NoSurface_RejectsUnknownMethod pins the 404 → SKIP
// path: a handler built WITHOUT WithPostureSurface rejects posture
// calls with CodeUnknownMethod (HTTP 404).
func TestPostureHandler_NoSurface_RejectsUnknownMethod(t *testing.T) {
	t.Parallel()
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs) // no WithPostureSurface
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
	})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/runtime.info", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusNotFound {
		t.Fatalf("no-posture-surface: status %d, want 404", httpResp.StatusCode)
	}
}

func TestPostureHandler_SurfaceError_MapsToWire(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newPostureHandler(t, &stubPosture{
		err: protoerrors.Newf(protoerrors.CodeScopeMismatch, "cross-tenant posture read requires admin"),
	}))
	defer srv.Close()

	body, _ := json.Marshal(types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
	})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/runtime.counters", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	// CodeScopeMismatch maps to HTTP 403.
	if httpResp.StatusCode != http.StatusForbidden {
		t.Fatalf("posture surface error: status %d, want 403", httpResp.StatusCode)
	}
	var perr protoerrors.Error
	if err := json.NewDecoder(httpResp.Body).Decode(&perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("wire error code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}
}

// TestPostureHandler_BackfillsEmptyBodyIdentity pins that when the
// auth middleware ran, an empty body identity is backfilled from the
// verified JWT identity and the request reaches the surface.
func TestPostureHandler_BackfillsEmptyBodyIdentity(t *testing.T) {
	t.Parallel()
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs, control.WithPostureSurface(&stubPosture{
		resp: &types.RuntimeInfo{ProtocolVersion: "0.1.0", InstanceID: "inst-bf"},
	}))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	verified := identity.Identity{TenantID: "t-bf", UserID: "u-bf", SessionID: "s-bf"}
	srv := httptest.NewServer(withIdentity(mux, verified))
	defer srv.Close()

	// Empty body identity — the handler backfills it from the JWT.
	body, _ := json.Marshal(types.RuntimeInfoRequest{})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/runtime.info", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(httpResp.Body)
		t.Fatalf("backfill: status %d, want 200; body=%s", httpResp.StatusCode, raw)
	}
}

// TestPostureHandler_BodyUserMismatch_RejectsClosed pins that a body
// whose user/session disagrees with the verified JWT identity is
// rejected 401 (the tenant may differ — that is the cross-tenant
// admin path — but user/session may not).
func TestPostureHandler_BodyUserMismatch_RejectsClosed(t *testing.T) {
	t.Parallel()
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs, control.WithPostureSurface(&stubPosture{
		resp: &types.RuntimeInfo{ProtocolVersion: "0.1.0"},
	}))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	verified := identity.Identity{TenantID: "t-mm", UserID: "u-mm", SessionID: "s-mm"}
	srv := httptest.NewServer(withIdentity(mux, verified))
	defer srv.Close()

	// Body claims a different user than the verified JWT.
	body, _ := json.Marshal(types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "t-mm", User: "someone-else", Session: "s-mm"},
	})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/runtime.info", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("body user mismatch: status %d, want 401", httpResp.StatusCode)
	}
}

// TestPostureHandler_MatchingBodyIdentity_Accepted pins that a body
// whose identity matches the verified JWT (every component) is
// accepted — the non-backfill path of backfillPostureIdentity.
func TestPostureHandler_MatchingBodyIdentity_Accepted(t *testing.T) {
	t.Parallel()
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs, control.WithPostureSurface(&stubPosture{
		resp: &types.RuntimeInfo{ProtocolVersion: "0.1.0"},
	}))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	verified := identity.Identity{TenantID: "t-ok", UserID: "u-ok", SessionID: "s-ok"}
	srv := httptest.NewServer(withIdentity(mux, verified))
	defer srv.Close()

	body, _ := json.Marshal(types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "t-ok", User: "u-ok", Session: "s-ok"},
	})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/runtime.health", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(httpResp.Body)
		t.Fatalf("matching body identity: status %d, want 200; body=%s", httpResp.StatusCode, raw)
	}
}

// TestPostureHandler_NonProtocolError_MapsToRuntimeError pins that a
// PostureSurface returning a non-*protoerrors.Error is wrapped as
// CodeRuntimeError (HTTP 500) — no raw runtime detail on the wire.
func TestPostureHandler_NonProtocolError_MapsToRuntimeError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newPostureHandler(t, &stubPosture{
		err: errPlain("a bare runtime error"),
	}))
	defer srv.Close()

	body, _ := json.Marshal(types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
	})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/runtime.info", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("non-Protocol error: status %d, want 500", httpResp.StatusCode)
	}
	var perr protoerrors.Error
	if err := json.NewDecoder(httpResp.Body).Decode(&perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeRuntimeError {
		t.Errorf("wire error code = %q, want %q", perr.Code, protoerrors.CodeRuntimeError)
	}
}

// errPlain is a bare error type — not a *protoerrors.Error — used to
// exercise the writePostureError non-Protocol fallback.
type errPlain string

func (e errPlain) Error() string { return string(e) }

func TestPostureHandler_MalformedBody_RejectsInvalidRequest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newPostureHandler(t, &stubPosture{
		resp: &types.RuntimeInfo{},
	}))
	defer srv.Close()

	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/runtime.info", bytes.NewReader([]byte("not json")))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body: status %d, want 400", httpResp.StatusCode)
	}
}
