package transports_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// testDeps holds the real drivers behind the mux — no mocks at the seam
// (CLAUDE.md §17.3).
type testDeps struct {
	surface *protocol.ControlSurface
	bus     events.EventBus
	cleanup func()
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	return &testDeps{
		surface: surface,
		bus:     bus,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

func TestNewMux_NilDeps_FailLoud(t *testing.T) {
	if _, err := transports.NewMux(nil, nil); err == nil {
		t.Fatal("NewMux(nil,nil) returned nil error; want ErrMisconfigured")
	}
	deps := newTestDeps(t)
	defer deps.cleanup()
	if _, err := transports.NewMux(deps.surface, nil); err == nil {
		t.Error("NewMux(surface,nil) returned nil error; want ErrMisconfigured")
	}
	if _, err := transports.NewMux(nil, deps.bus); err == nil {
		t.Error("NewMux(nil,bus) returned nil error; want ErrMisconfigured")
	}
}

// TestNewMux_MissingAuthChoice_FailLoud — PR #91 made the auth choice
// mandatory: NewMux returns ErrMisconfigured when neither
// WithValidator nor WithoutValidator is supplied (CLAUDE.md §13
// "Test stubs as production defaults on operator-facing seams").
func TestNewMux_MissingAuthChoice_FailLoud(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()
	_, err := transports.NewMux(deps.surface, deps.bus)
	if err == nil {
		t.Fatal("NewMux without auth choice returned nil error; want ErrMisconfigured")
	}
	if !errors.Is(err, transports.ErrMisconfigured) {
		t.Fatalf("error type = %v, want ErrMisconfigured", err)
	}
}

// TestNewMux_RoutesBothTransports — the composed mux serves the REST
// control route AND the SSE event route.
func TestNewMux_RoutesBothTransports(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	mux, err := transports.NewMux(deps.surface, deps.bus, transports.WithoutValidator())
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// REST control route.
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
	resp, err := http.Post(srv.URL+"/v1/control/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/control/start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control route status = %d, want 200", resp.StatusCode)
	}
	var sr types.StartResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode StartResponse: %v", err)
	}
	if sr.TaskID == "" {
		t.Error("control route returned empty TaskID")
	}

	// SSE event route — open + immediately confirm the stream headers,
	// then close.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("X-Harbor-Tenant", "t1")
	req.Header.Set("X-Harbor-User", "u1")
	req.Header.Set("X-Harbor-Session", "s1")
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	defer func() { _ = sresp.Body.Close() }()
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("stream route status = %d, want 200", sresp.StatusCode)
	}
	if ct := sresp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("stream route Content-Type = %q, want text/event-stream", ct)
	}
}

// TestNewMux_Options — WithLogger + WithKeepalive thread through; a nil
// logger / non-positive keepalive is ignored.
func TestNewMux_Options(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux, err := transports.NewMux(deps.surface, deps.bus,
		transports.WithLogger(logger),
		transports.WithLogger(nil),  // ignored
		transports.WithKeepalive(0), // ignored
		transports.WithKeepalive(time.Second),
		transports.WithoutValidator(),
	)
	if err != nil {
		t.Fatalf("NewMux with options: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The mux still serves the control route with the options applied.
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
	resp, err := http.Post(srv.URL+"/v1/control/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// stubValidator is the minimal Validator the WithValidator wiring tests
// use — it returns either the supplied verified or the supplied error
// without touching a real signing key. The integration test in
// test/integration/phase61_auth_test.go covers the end-to-end real-key
// path.
type stubValidator struct {
	verified auth.Verified
	err      error
}

func (s *stubValidator) Validate(_ context.Context, _ string) (auth.Verified, error) {
	if s.err != nil {
		return auth.Verified{}, s.err
	}
	return s.verified, nil
}

// TestNewMux_WithValidator_AppliesAuthMiddlewareToBothHandlers — the
// Phase 61 wiring assertion: when WithValidator is supplied, both the
// REST control route AND the SSE event route reject a request with no
// `Authorization: Bearer` header (HTTP 401 + identity_required). When
// WithValidator is NOT supplied, the Phase 60 trust-based posture is
// preserved (the identity comes from headers / body and the request
// proceeds without a bearer).
func TestNewMux_WithValidator_AppliesAuthMiddlewareToBothHandlers(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	// A no-op-success Validator (so a valid bearer succeeds) — but we
	// only test the *rejection* paths here; the wiring is the surface
	// under test.
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
	}}

	mux, err := transports.NewMux(deps.surface, deps.bus, transports.WithValidator(stub))
	if err != nil {
		t.Fatalf("NewMux with validator: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// REST control route — no bearer ⇒ 401.
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
	resp, err := http.Post(srv.URL+"/v1/control/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST without bearer: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("REST control without bearer: status %d, want 401", resp.StatusCode)
	}

	// SSE event route — no bearer ⇒ 401.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/events without bearer: %v", err)
	}
	_ = sresp.Body.Close()
	if sresp.StatusCode != http.StatusUnauthorized {
		t.Errorf("SSE event without bearer: status %d, want 401", sresp.StatusCode)
	}
}

// TestNewMux_WithValidator_ValidBearerPasses — when a request DOES
// carry a bearer that the (stub) Validator accepts, the request flows
// through to the underlying Phase 60 handlers normally. The REST
// route's `start` succeeds; the SSE route's stream opens.
func TestNewMux_WithValidator_ValidBearerPasses(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
	}}
	mux, err := transports.NewMux(deps.surface, deps.bus, transports.WithValidator(stub))
	if err != nil {
		t.Fatalf("NewMux with validator: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// REST `start` with bearer — body identity left empty so the
	// middleware backfills from the JWT-verified identity (defence-in-
	// depth: the request body's IdentityScope MUST match the JWT
	// identity if non-empty; an empty IdentityScope is backfilled from
	// the JWT — which IS the authentication source of truth).
	body := `{"identity":{},"query":"q"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer faketoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST with bearer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("REST control with bearer: status %d, want 200; body=%s", resp.StatusCode, string(raw))
	}
	var sr types.StartResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode StartResponse: %v", err)
	}
	if sr.TaskID == "" {
		t.Error("REST control returned empty TaskID")
	}

	// SSE stream with bearer — opens 200 + text/event-stream.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	streamReq.Header.Set("Authorization", "Bearer faketoken")
	sresp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("GET /v1/events with bearer: %v", err)
	}
	defer func() { _ = sresp.Body.Close() }()
	if sresp.StatusCode != http.StatusOK {
		t.Errorf("SSE event with bearer: status %d, want 200", sresp.StatusCode)
	}
	if ct := sresp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("SSE Content-Type: %q, want text/event-stream", ct)
	}
}

// TestNewMux_WithValidator_BodyIdentityMismatch_Rejected — the
// defence-in-depth assertion: when the JWT verifies for tenant T1 but
// the request body's IdentityScope claims T2, the REST control handler
// rejects 401 BEFORE the request reaches Dispatch. Stops a malicious
// caller from "borrowing" another tenant's identity using a valid
// token from their own.
func TestNewMux_WithValidator_BodyIdentityMismatch_Rejected(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
	}}
	mux, err := transports.NewMux(deps.surface, deps.bus, transports.WithValidator(stub))
	if err != nil {
		t.Fatalf("NewMux with validator: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"identity":{"tenant":"t2","user":"u2","session":"s2"},"query":"q"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer faketoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("body-identity mismatch: status %d, want 401", resp.StatusCode)
	}
}

// TestNewMux_WithValidator_NilValidator_FailsLoud — PR #91 changed
// nil-handling on WithValidator: a nil now counts as "WithValidator
// not supplied" and NewMux fails closed unless WithoutValidator is
// also passed (CLAUDE.md §13 "Test stubs as production defaults on
// operator-facing seams").
func TestNewMux_WithValidator_NilValidator_FailsLoud(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	_, err := transports.NewMux(deps.surface, deps.bus, transports.WithValidator(nil))
	if err == nil {
		t.Fatal("NewMux with nil validator (no WithoutValidator) returned nil error; want ErrMisconfigured")
	}
	if !errors.Is(err, transports.ErrMisconfigured) {
		t.Fatalf("error type = %v, want ErrMisconfigured", err)
	}
}

// TestNewMux_WithoutValidator_OptInUnauthenticated — the explicit
// test-only escape hatch lets the Phase 60 trust-based posture run
// without a JWT validator. The body's identity is the source of
// truth (the carrier-header / Dispatch identity-from-body gate).
func TestNewMux_WithoutValidator_OptInUnauthenticated(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	mux, err := transports.NewMux(deps.surface, deps.bus, transports.WithoutValidator())
	if err != nil {
		t.Fatalf("NewMux with WithoutValidator: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
	resp, err := http.Post(srv.URL+"/v1/control/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("WithoutValidator: status %d, want 200 (Phase 60 trust-based posture preserved)", resp.StatusCode)
	}
}

// TestNewMux_WithValidator_AdminScopeQuery_GatedOnVerifiedScope — the
// SSE handler's ?admin=1 gate. A request without the verified `admin`
// scope is rejected 403 even though the bearer is valid.
func TestNewMux_WithValidator_AdminScopeQuery_GatedOnVerifiedScope(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	// Validator says: identity OK, but NO scopes claimed.
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Scopes:   nil,
	}}
	mux, err := transports.NewMux(deps.surface, deps.bus, transports.WithValidator(stub))
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
	req.Header.Set("Authorization", "Bearer faketoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET ?admin=1: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("?admin=1 without scope: status %d, want 403", resp.StatusCode)
	}

	// And with the scope, the same request is permitted (200 + SSE).
	stub.verified.Scopes = []auth.Scope{auth.ScopeAdmin}
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
	req2.Header.Set("Authorization", "Bearer faketoken")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET ?admin=1 with scope: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("?admin=1 with scope: status %d, want 200", resp2.StatusCode)
	}
}

// TestNewMux_WithValidator_BearerRejected_ErrorBodyShape — the JSON
// error body the middleware writes on a rejection is the canonical
// Protocol error shape (CodeAuthRejected for a verification failure).
func TestNewMux_WithValidator_BearerRejected_ErrorBodyShape(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	stub := &stubValidator{err: errors.New("custom rejection")}
	mux, err := transports.NewMux(deps.surface, deps.bus, transports.WithValidator(stub))
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer bad")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	var perr struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != "auth_rejected" {
		t.Errorf("error code: %q, want auth_rejected", perr.Code)
	}
}

// TestNewMux_UnknownRoute_404 — a path the mux does not route 404s.
func TestNewMux_UnknownRoute_404(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()
	mux, err := transports.NewMux(deps.surface, deps.bus, transports.WithoutValidator())
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/nonexistent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route status = %d, want 404", resp.StatusCode)
	}
}
