package auth

// Phase 111b (D-199) — unit tests for the OAuth callback handler:
// param parsing, provider lookup across a multi-provider map, the
// typed error→status mappings, the upstream-denial path, the
// no-secret-material assertions (response bodies AND log lines), the
// replay-is-404 idempotency, and the D-025 concurrent-reuse pin
// (N≥128 concurrent GETs against one shared handler, mixed valid /
// invalid states, no cross-flow bleed, goroutine baseline restored).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
)

// cbGet drives one GET against the handler and returns the recorder.
func cbGet(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// cbErrorBody decodes the typed JSON error body.
func cbErrorBody(t *testing.T, rec *httptest.ResponseRecorder) (code, detail string) {
	t.Helper()
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("error body not JSON: %v (body=%q)", err, rec.Body.String())
	}
	return body.Error, body.Detail
}

func TestCallbackHandler_MissingState_BadRequest(t *testing.T) {
	t.Parallel()
	h := CallbackHandler(nil)
	rec := cbGet(t, h, CallbackPath)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	code, _ := cbErrorBody(t, rec)
	if code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", code)
	}
}

func TestCallbackHandler_NonGET_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := CallbackHandler(nil)
	req := httptest.NewRequest(http.MethodPost, CallbackPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestCallbackHandler_UnknownState_NotFound(t *testing.T) {
	t.Parallel()
	har := newProviderHarness(t)
	h := CallbackHandler(map[string]OAuthProvider{"github": har.provider})
	rec := cbGet(t, h, CallbackPath+"?state=never-issued&code=x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	code, _ := cbErrorBody(t, rec)
	if code != "flow_not_found" {
		t.Fatalf("error code = %q, want flow_not_found", code)
	}
}

func TestCallbackHandler_MissingCode_BadRequest(t *testing.T) {
	t.Parallel()
	har := newProviderHarness(t)
	ctx := mkCtx(t, mkIdentity(t))
	_, err := har.provider.Token(ctx, har.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", err)
	}
	h := CallbackHandler(map[string]OAuthProvider{"github": har.provider})
	rec := cbGet(t, h, CallbackPath+"?state="+authErr.State)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	code, _ := cbErrorBody(t, rec)
	if code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", code)
	}
	// The flow record is NOT consumed by a missing-code request.
	if _, ok, err := har.provider.PendingFlow(context.Background(), authErr.State); err != nil || !ok {
		if err != nil {
			t.Fatalf("PendingFlow: %v", err)
		}
		t.Fatal("flow record consumed by a missing-code request")
	}
}

// TestCallbackHandler_Success_CompletesFlow_ResumesPause_NoSecrets is
// the headline unit test: a real provider + real fake auth server;
// the redirect GET completes the flow, the success page carries no
// token / code material, the pause record resumes with the typed
// DecisionResume marker, and a subsequent Token() resolves from the
// store. Log lines are captured and asserted secret-free (§7).
func TestCallbackHandler_Success_CompletesFlow_ResumesPause_NoSecrets(t *testing.T) {
	t.Parallel()
	har := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	// Subscribe BEFORE the Token() call so the publish is never raced.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := har.bus.Subscribe(subCtx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{EventTypeToolAuthRequired},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	_, terr := har.provider.Token(ctx, har.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(terr, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", terr)
	}
	var pauseToken string
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(ToolAuthRequiredPayload)
		if !ok {
			t.Fatalf("payload type = %T", ev.Payload)
		}
		pauseToken = p.PauseToken
	case <-time.After(30 * time.Second):
		t.Fatal("tool.auth_required never landed")
	}

	code, _, err := har.server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}

	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := CallbackHandler(map[string]OAuthProvider{"github": har.provider},
		WithCallbackLogger(logger))

	rec := cbGet(t, h, CallbackPath+"?state="+authErr.State+"&code="+code)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}

	// Subsequent Token() resolves from the store — the flow is whole.
	tok, err := har.provider.Token(ctx, har.userCfg.Source)
	if err != nil {
		t.Fatalf("Token readback: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("readback AccessToken empty")
	}

	// The pause record resumed.
	st, err := har.coordinator.Status(ctx, pauseresume.Token(pauseToken))
	if err != nil {
		t.Fatalf("coordinator.Status: %v", err)
	}
	if st.State != pauseresume.StatusResumed {
		t.Fatalf("pause state = %q, want resumed", st.State)
	}

	// §7 no-secret assertions: neither the success page nor any log
	// line carries the authorization code or token bytes.
	for name, blob := range map[string]string{
		"success page": rec.Body.String(),
		"log output":   logBuf.String(),
	} {
		for what, secret := range map[string]string{
			"authorization code": code,
			"access token":       tok.AccessToken,
			"refresh token":      tok.RefreshToken,
		} {
			if secret != "" && strings.Contains(blob, secret) {
				t.Errorf("%s leaks the %s", name, what)
			}
		}
	}
	// The success-path log line exists and carries identity attrs.
	if !strings.Contains(logBuf.String(), "flow completed") {
		t.Errorf("success log line missing: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), id.TenantID) {
		t.Errorf("log line missing tenant attr: %q", logBuf.String())
	}
}

// TestCallbackHandler_ReplayWithinRetryHorizon_IdempotentSuccess verifies that
// an ambiguous client response can be retried while the exact-state tombstone
// remains live, without a second authorization-code exchange.
func TestCallbackHandler_ReplayWithinRetryHorizon_IdempotentSuccess(t *testing.T) {
	t.Parallel()
	har := newProviderHarness(t)
	ctx := mkCtx(t, mkIdentity(t))
	_, terr := har.provider.Token(ctx, har.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(terr, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", terr)
	}
	code, _, err := har.server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	h := CallbackHandler(map[string]OAuthProvider{"github": har.provider})
	target := CallbackPath + "?state=" + authErr.State + "&code=" + code

	if rec := cbGet(t, h, target); rec.Code != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", rec.Code)
	}
	rec := cbGet(t, h, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("replayed GET status = %d, want 200", rec.Code)
	}
	if got := har.server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges after replay = %d, want 1", got)
	}
}

// TestCallbackHandler_MultiProviderMap_RoutesToOwner — two distinct
// providers in the map; each state completes against its owner.
func TestCallbackHandler_MultiProviderMap_RoutesToOwner(t *testing.T) {
	t.Parallel()
	harA := newProviderHarness(t)
	harB := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	_, errB := harB.provider.Token(ctx, harB.userCfg.Source)
	var authErrB *ErrAuthRequired
	if !errors.As(errB, &authErrB) {
		t.Fatalf("B Token: want *ErrAuthRequired, got %v", errB)
	}
	codeB, _, err := harB.server.VisitAuthorizeURL(authErrB.AuthorizeURL)
	if err != nil {
		t.Fatalf("B VisitAuthorizeURL: %v", err)
	}

	h := CallbackHandler(map[string]OAuthProvider{
		"provider-a": harA.provider,
		"provider-b": harB.provider,
	})
	rec := cbGet(t, h, CallbackPath+"?state="+authErrB.State+"&code="+codeB)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	// B's flow completed; A never saw a flow.
	if _, err := harB.provider.Token(ctx, harB.userCfg.Source); err != nil {
		t.Fatalf("B Token readback: %v", err)
	}
	if _, err := harA.provider.Token(ctx, harA.userCfg.Source); !errors.Is(err, ErrAuthRequiredSentinel) {
		t.Fatalf("A Token: want ErrAuthRequired (no bleed), got %v", err)
	}
}

// TestCallbackHandler_UpstreamDenial_RejectsPause — error=access_denied
// surfaces 400 + the flow is consumed + the pause resumes with the
// typed DecisionReject marker (D-199's resume-with-rejection call).
func TestCallbackHandler_UpstreamDenial_RejectsPause(t *testing.T) {
	t.Parallel()
	har := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := har.bus.Subscribe(subCtx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{EventTypeToolAuthRequired},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	_, terr := har.provider.Token(ctx, har.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(terr, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", terr)
	}
	var pauseToken string
	select {
	case ev := <-sub.Events():
		pauseToken = ev.Payload.(ToolAuthRequiredPayload).PauseToken
	case <-time.After(30 * time.Second):
		t.Fatal("tool.auth_required never landed")
	}

	h := CallbackHandler(map[string]OAuthProvider{"github": har.provider})
	rec := cbGet(t, h, CallbackPath+"?state="+authErr.State+"&error=access_denied&error_description=user+said+no")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	code, detail := cbErrorBody(t, rec)
	if code != "authorization_denied" || detail != "access_denied" {
		t.Fatalf("body = (%q, %q), want (authorization_denied, access_denied)", code, detail)
	}
	// The flow is consumed (replay → 404)…
	if _, ok, err := har.provider.PendingFlow(context.Background(), authErr.State); err != nil || ok {
		if err != nil {
			t.Fatalf("PendingFlow: %v", err)
		}
		t.Fatal("denied flow record not consumed")
	}
	// …and the pause resumed (with reject — the resume-with-rejection
	// terminal, not a hang-to-TTL).
	st, err := har.coordinator.Status(ctx, pauseresume.Token(pauseToken))
	if err != nil {
		t.Fatalf("coordinator.Status: %v", err)
	}
	if st.State != pauseresume.StatusResumed {
		t.Fatalf("denied pause state = %q, want resumed (DecisionReject)", st.State)
	}
}

func TestCallbackHandler_UnknownDenialCannotCrossContainmentBoundary(t *testing.T) {
	const malicious = "access_denied bearer=secret-like-credential-DO-NOT-PERSIST"
	har := newProviderHarness(t)
	coord := pauseresume.New(pauseresume.WithBus(har.bus))
	provider, err := NewProvider([]OAuthConfig{har.userCfg, har.agentCfg}, ProviderDeps{
		Store: har.store, Flows: har.flows, Bus: har.bus, Redactor: har.redactor,
		Coordinator: coord, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := mkIdentity(t)
	ctx := mkCtx(t, id)
	sub, err := har.bus.Subscribe(ctx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{pauseresume.EventTypePauseResumed},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	_, tokenErr := provider.Token(ctx, har.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(tokenErr, &required) {
		t.Fatalf("Token: want ErrAuthRequired, got %v", tokenErr)
	}
	var logs strings.Builder
	handler := CallbackHandler(map[string]OAuthProvider{"provider": provider},
		WithCallbackLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	query := url.Values{
		"state":             []string{required.State},
		"error":             []string{malicious},
		"error_description": []string{malicious},
	}
	rec := cbGet(t, handler, CallbackPath+"?"+query.Encode())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
	code, detail := cbErrorBody(t, rec)
	if code != "authorization_denied" || detail != string(denialAuthorizationDeniedLocal) {
		t.Fatalf("body=(%q,%q) want=(authorization_denied,%q)", code, detail, denialAuthorizationDeniedLocal)
	}

	var resumed events.Event
	select {
	case resumed = <-sub.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("pause.resumed event not emitted")
	}
	status, err := coord.Status(ctx, pauseresume.Token(required.PauseToken))
	if err != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject {
		t.Fatalf("denied pause status=%+v err=%v", status, err)
	}
	listed, err := coord.List(ctx, pauseresume.ListRequest{
		Identity: id,
		Filter:   pauseresume.ListFilter{States: []pauseresume.State{pauseresume.StatusResumed}},
		Page:     1,
		PageSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	var storedPayload map[string]any
	for _, pause := range listed.Snapshots {
		if pause.Token == pauseresume.Token(required.PauseToken) {
			storedPayload = pause.Payload
			break
		}
	}
	if storedPayload == nil {
		t.Fatalf("resumed pause %q missing from canonical List snapshot", required.PauseToken)
	}
	if got := storedPayload["denied_reason"]; got != string(denialAuthorizationDeniedLocal) {
		t.Fatalf("stored denied_reason=%v want=%q", got, denialAuthorizationDeniedLocal)
	}
	storedJSON, err := json.Marshal(storedPayload)
	if err != nil {
		t.Fatal(err)
	}
	eventJSON, err := json.Marshal(resumed)
	if err != nil {
		t.Fatal(err)
	}
	for name, blob := range map[string]string{
		"callback log": logs.String(),
		"HTTP body":    rec.Body.String(),
		"pause record": string(storedJSON),
		"resume event": string(eventJSON),
	} {
		if strings.Contains(blob, malicious) || strings.Contains(blob, "secret-like-credential") {
			t.Fatalf("%s leaked malicious OAuth denial: %s", name, blob)
		}
	}
	if _, ok, err := provider.PendingFlow(context.Background(), required.State); err != nil || ok {
		t.Fatalf("denied flow cleanup = ok:%v err:%v", ok, err)
	}
}

// TestCallbackHandler_ExpiredFlow_Gone_PauseStillParked — a callback
// after FlowTTL maps to 410 and the pause record stays parked (the
// 111c sweeper / operator owns the eventual cleanup).
func TestCallbackHandler_ExpiredFlow_Gone_PauseStillParked(t *testing.T) {
	t.Parallel()
	server := newFakeAuthServer(t)
	red := mkRedactor(t)
	bus := mkBus(t, red)
	coord := mkCoordinator(t)
	store := mkTokenStore(t)

	var clockMu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}

	cfg := OAuthConfig{
		Source:       toolsToolSource("expiry-source"),
		BindingScope: ScopeUser,
		ServerURL:    server.BaseURL(),
		RedirectURI:  "http://localhost" + CallbackPath,
	}
	prov, err := NewProvider([]OAuthConfig{cfg}, ProviderDeps{
		Store: store, Flows: mkFlowStore(t), Bus: bus, Redactor: red, Coordinator: coord,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Clock:      clock,
		FlowTTL:    time.Minute,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	id := mkIdentity(t)
	ctx := mkCtx(t, id)
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{EventTypeToolAuthRequired},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	_, terr := prov.Token(ctx, cfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(terr, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", terr)
	}
	var pauseToken string
	select {
	case ev := <-sub.Events():
		pauseToken = ev.Payload.(ToolAuthRequiredPayload).PauseToken
	case <-time.After(30 * time.Second):
		t.Fatal("tool.auth_required never landed")
	}
	code, _, err := server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}

	// Advance the provider's clock past the flow TTL.
	clockMu.Lock()
	now = now.Add(2 * time.Minute)
	clockMu.Unlock()

	h := CallbackHandler(map[string]OAuthProvider{"expiry": prov})
	rec := cbGet(t, h, CallbackPath+"?state="+authErr.State+"&code="+code)
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", rec.Code)
	}
	if ec, _ := cbErrorBody(t, rec); ec != "flow_expired" {
		t.Fatalf("error code = %q, want flow_expired", ec)
	}
	// The pause is STILL parked — expiry does not resume it.
	st, err := coord.Status(ctx, pauseresume.Token(pauseToken))
	if err != nil {
		t.Fatalf("coordinator.Status: %v", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("expired flow's pause state = %q, want paused", st.State)
	}
}

// cbStubProvider returns canned sentinels so the error→status mapping
// table is exercised without standing up upstream failures. Test-only
// (CLAUDE.md §13 — never reachable from production code).
type cbStubProvider struct {
	completeErr error
	denyErr     error
	pendingErr  error
	info        PendingFlowInfo
}

func (s *cbStubProvider) Token(context.Context, tools.ToolSourceID) (Token, error) {
	return Token{}, nil
}
func (s *cbStubProvider) InitiateFlow(context.Context, tools.ToolSourceID) (FlowInitiation, error) {
	return FlowInitiation{}, nil
}
func (s *cbStubProvider) CompleteFlow(context.Context, string, string) (Token, error) {
	return Token{}, s.completeErr
}
func (s *cbStubProvider) PendingFlow(context.Context, string) (PendingFlowInfo, bool, error) {
	return s.info, s.pendingErr == nil, s.pendingErr
}
func (s *cbStubProvider) DenyFlow(context.Context, string, string) error {
	return s.denyErr
}
func (s *cbStubProvider) Revoke(context.Context, tools.ToolSourceID) error { return nil }
func (s *cbStubProvider) Close(context.Context) error                      { return nil }
func (s *cbStubProvider) AllowedDownstreamHosts() []string                 { return nil }

func cbStubInfo(t *testing.T) PendingFlowInfo {
	t.Helper()
	return PendingFlowInfo{
		Source:       toolsToolSource("stub-source"),
		BindingScope: ScopeUser,
		Identity:     mkIdentity(t),
		ExpiresAt:    time.Now().Add(time.Minute),
	}
}

func TestCallbackHandler_PendingFlowError_FailsLoud(t *testing.T) {
	t.Parallel()
	stub := &cbStubProvider{pendingErr: errors.New("durable flow lookup failed")}
	h := CallbackHandler(map[string]OAuthProvider{"stub": stub},
		WithCallbackLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	rec := cbGet(t, h, CallbackPath+"?state=s&code=c")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code, _ := cbErrorBody(t, rec); code != "internal_error" {
		t.Fatalf("error code = %q, want internal_error", code)
	}
}

// TestCallbackHandler_ErrorMapping_Table pins the full sentinel →
// HTTP-status mapping (the acceptance criterion's four mappings plus
// the defensive legs).
func TestCallbackHandler_ErrorMapping_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"flow_not_found", ErrFlowNotFound, http.StatusNotFound, "flow_not_found"},
		{"flow_expired", ErrFlowExpired, http.StatusGone, "flow_expired"},
		{"state_mismatch", ErrStateMismatch, http.StatusBadRequest, "state_mismatch"},
		{"exchange_failed", ErrExchangeFailed, http.StatusBadGateway, "exchange_failed"},
		{"discovery_failed", ErrDiscoveryFailed, http.StatusBadGateway, "exchange_failed"},
		{"registration_failed", ErrRegistrationFailed, http.StatusBadGateway, "exchange_failed"},
		{"provider_closed", ErrProviderClosed, http.StatusServiceUnavailable, "provider_closed"},
		{"unknown", errors.New("boom"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &cbStubProvider{
				completeErr: fmt.Errorf("wrapped: %w", tc.err),
				info:        cbStubInfo(t),
			}
			h := CallbackHandler(map[string]OAuthProvider{"stub": stub},
				WithCallbackLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
			rec := cbGet(t, h, CallbackPath+"?state=s&code=c")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if code, _ := cbErrorBody(t, rec); code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

func TestCallbackHandler_CompletionErrorNeverLogsUntrustedSecret(t *testing.T) {
	const secret = "upstream-reflected-access-token-SECRET"
	var logs strings.Builder
	stub := &cbStubProvider{
		completeErr: fmt.Errorf("%w: status 400 body %q", ErrExchangeFailed, secret),
		info:        cbStubInfo(t),
	}
	h := CallbackHandler(map[string]OAuthProvider{"stub": stub},
		WithCallbackLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	rec := cbGet(t, h, CallbackPath+"?state=s&code=c")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusBadGateway)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("callback log leaked untrusted upstream secret: %s", logs.String())
	}
}

// TestCallbackHandler_DenyFlowError_Mapped — a denial whose DenyFlow
// fails (e.g. the flow expired between lookup and deny) maps through
// the same sentinel table.
func TestCallbackHandler_DenyFlowError_Mapped(t *testing.T) {
	t.Parallel()
	stub := &cbStubProvider{denyErr: ErrFlowExpired, info: cbStubInfo(t)}
	h := CallbackHandler(map[string]OAuthProvider{"stub": stub},
		WithCallbackLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	rec := cbGet(t, h, CallbackPath+"?state=s&error=access_denied")
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", rec.Code)
	}
}

// TestCallbackHandler_WithSuccessPage_Override — the operator-supplied
// page is served verbatim.
func TestCallbackHandler_WithSuccessPage_Override(t *testing.T) {
	t.Parallel()
	stub := &cbStubProvider{info: cbStubInfo(t)}
	h := CallbackHandler(map[string]OAuthProvider{"stub": stub},
		WithSuccessPage("<html>custom done page</html>"),
		WithCallbackLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	rec := cbGet(t, h, CallbackPath+"?state=s&code=c")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "custom done page") {
		t.Fatalf("body = %q, want the custom page", rec.Body.String())
	}
}

// TestProvider_DenyFlow_Sentinels pins DenyFlow's own contract:
// unknown state, identity mismatch, and the success path's
// DecisionReject resume.
func TestProvider_DenyFlow_Sentinels(t *testing.T) {
	t.Parallel()
	har := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	if err := har.provider.DenyFlow(ctx, "never-issued", "access_denied"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("DenyFlow unknown state: want ErrFlowNotFound, got %v", err)
	}
	if err := har.provider.DenyFlow(ctx, "", "access_denied"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("DenyFlow empty state: want ErrFlowNotFound, got %v", err)
	}

	_, terr := har.provider.Token(ctx, har.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(terr, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", terr)
	}
	// Cross-identity denial fails closed.
	otherCtx := mkCtx(t, identity.Identity{TenantID: "tenant-B", UserID: "user-bob", SessionID: "session-002"})
	if err := har.provider.DenyFlow(otherCtx, authErr.State, "access_denied"); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("cross-identity DenyFlow: want ErrStateMismatch, got %v", err)
	}
	// The mismatch consumed the record (same consumption semantics as
	// CompleteFlow) — re-initiate for the success leg.
	_, terr = har.provider.Token(ctx, har.userCfg.Source)
	if !errors.As(terr, &authErr) {
		t.Fatalf("re-Token: want *ErrAuthRequired, got %v", terr)
	}
	if err := har.provider.DenyFlow(ctx, authErr.State, "access_denied"); err != nil {
		t.Fatalf("DenyFlow: %v", err)
	}
	if _, ok, err := har.provider.PendingFlow(context.Background(), authErr.State); err != nil || ok {
		if err != nil {
			t.Fatalf("PendingFlow: %v", err)
		}
		t.Fatal("denied flow record not consumed")
	}
}

// TestCallbackHandler_ConcurrentReuse_NoCrossFlowBleed is the D-025
// pin: N≥128 concurrent GETs (mixed valid / invalid states) against
// ONE shared handler instance under -race. Asserts: no data races
// (the gate), no cross-flow bleed (each identity's completion mints a
// token bound to that identity), and the goroutine baseline restores.
func TestCallbackHandler_ConcurrentReuse_NoCrossFlowBleed(t *testing.T) {
	har := newProviderHarness(t)
	h := CallbackHandler(map[string]OAuthProvider{"github": har.provider},
		WithCallbackLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))

	const n = 64 // valid flows; + 64 invalid-state GETs = 128 concurrent calls
	type flow struct {
		id    identity.Identity
		state string
		code  string
	}
	flows := make([]flow, 0, n)
	for i := range n {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%d", i%4),
			UserID:    fmt.Sprintf("user-%d", i),
			SessionID: fmt.Sprintf("session-%d", i),
		}
		ctx := mkCtx(t, id)
		_, terr := har.provider.Token(ctx, har.userCfg.Source)
		var authErr *ErrAuthRequired
		if !errors.As(terr, &authErr) {
			t.Fatalf("flow %d Token: %v", i, terr)
		}
		code, _, err := har.server.VisitAuthorizeURL(authErr.AuthorizeURL)
		if err != nil {
			t.Fatalf("flow %d VisitAuthorizeURL: %v", i, err)
		}
		flows = append(flows, flow{id: id, state: authErr.State, code: code})
	}

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errCh := make(chan error, 2*n)
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			f := flows[i]
			rec := cbGet(t, h, CallbackPath+"?state="+f.state+"&code="+f.code)
			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("flow %d: status %d (body %q)", i, rec.Code, rec.Body.String())
			}
		}()
		go func() {
			defer wg.Done()
			rec := cbGet(t, h, fmt.Sprintf("%s?state=bogus-%d&code=x", CallbackPath, i))
			if rec.Code != http.StatusNotFound {
				errCh <- fmt.Errorf("bogus %d: status %d, want 404", i, rec.Code)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	// No cross-flow bleed: every identity's token readback is bound to
	// that identity.
	for i, f := range flows {
		ctx := mkCtx(t, f.id)
		tok, err := har.provider.Token(ctx, har.userCfg.Source)
		if err != nil {
			t.Fatalf("flow %d readback: %v", i, err)
		}
		if tok.TenantID != f.id.TenantID || tok.UserID != f.id.UserID {
			t.Fatalf("flow %d cross-identity bleed: tok=(%s,%s) want (%s,%s)",
				i, tok.TenantID, tok.UserID, f.id.TenantID, f.id.UserID)
		}
	}

	// Goroutine baseline restored (bounded poll, never a sync sleep).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}
