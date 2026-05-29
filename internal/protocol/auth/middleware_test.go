package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// nilValidator is a Validator that always returns the supplied error.
// Used to drive every rejection-path branch of the middleware
// deterministically without needing a real signed token.
type stubValidator struct {
	verified auth.Verified
	err      error
	calls    int
}

func (s *stubValidator) Validate(_ context.Context, _ string) (auth.Verified, error) {
	s.calls++
	if s.err != nil {
		return auth.Verified{}, s.err
	}
	return s.verified, nil
}

// echoHandler reads the verified identity + scopes from ctx and writes
// them back as the response body — used to assert the middleware
// successfully attached identity + scopes to the request context.
func echoHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Helper()
		id, ok := identity.From(r.Context())
		if !ok {
			t.Errorf("echoHandler: no identity on ctx after middleware")
			http.Error(w, "no identity", http.StatusInternalServerError)
			return
		}
		scopes, _ := auth.ScopesFrom(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenant":  id.TenantID,
			"user":    id.UserID,
			"session": id.SessionID,
			"scopes":  scopes,
		})
	})
}

// readErrorBody decodes the JSON Protocol error written by the
// middleware. Used to assert (a) the wire response shape, (b) the
// Code is the one we expect.
func readErrorBody(t *testing.T, w *httptest.ResponseRecorder) *protoerrors.Error {
	t.Helper()
	var perr protoerrors.Error
	if err := json.NewDecoder(w.Body).Decode(&perr); err != nil {
		t.Fatalf("decode error body: %v (raw: %q)", err, w.Body.String())
	}
	return &perr
}

func TestMiddleware_NoAuthorizationHeader_Rejected401_IdentityRequired(t *testing.T) {
	stub := &stubValidator{}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	if stub.calls != 0 {
		t.Errorf("validator was called even though no header was present")
	}
	perr := readErrorBody(t, w)
	if perr.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("error code: got %q, want %q", perr.Code, protoerrors.CodeIdentityRequired)
	}
}

func TestMiddleware_MalformedScheme_Rejected401(t *testing.T) {
	stub := &stubValidator{}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz") // not Bearer
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	if stub.calls != 0 {
		t.Errorf("validator was called for non-Bearer scheme")
	}
}

func TestMiddleware_BearerSchemeNoToken_Rejected401(t *testing.T) {
	stub := &stubValidator{}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer    ")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	if stub.calls != 0 {
		t.Errorf("validator was called for empty token")
	}
}

func TestMiddleware_BearerSchemeIsCaseInsensitive(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
	}}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "bearer faketoken") // lowercase
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (lowercase bearer accepted)", w.Code)
	}
}

func TestMiddleware_ValidatorRejects_AlgNotAllowed_401_AuthRejected(t *testing.T) {
	stub := &stubValidator{err: auth.ErrAlgNotAllowed}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	perr := readErrorBody(t, w)
	if perr.Code != protoerrors.CodeAuthRejected {
		t.Errorf("error code: got %q, want %q", perr.Code, protoerrors.CodeAuthRejected)
	}
	if !strings.Contains(perr.Message, "alg_not_allowed") {
		t.Errorf("message should carry the wire reason, got %q", perr.Message)
	}
}

func TestMiddleware_ValidatorRejects_SignatureInvalid_401_AuthRejected(t *testing.T) {
	stub := &stubValidator{err: auth.ErrSignatureInvalid}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	perr := readErrorBody(t, w)
	if perr.Code != protoerrors.CodeAuthRejected {
		t.Errorf("error code: got %q, want %q", perr.Code, protoerrors.CodeAuthRejected)
	}
}

func TestMiddleware_ValidatorRejects_TokenExpired_401(t *testing.T) {
	stub := &stubValidator{err: auth.ErrTokenExpired}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	perr := readErrorBody(t, w)
	if perr.Code != protoerrors.CodeAuthRejected {
		t.Errorf("expected auth_rejected for expired token, got %q", perr.Code)
	}
}

func TestMiddleware_ValidatorRejects_IdentityClaimMissing_IdentityRequiredCode(t *testing.T) {
	stub := &stubValidator{err: auth.ErrIdentityClaimMissing}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	perr := readErrorBody(t, w)
	if perr.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("error code: got %q, want %q (RFC §5.5: missing identity → identity_required)", perr.Code, protoerrors.CodeIdentityRequired)
	}
}

func TestMiddleware_HappyPath_AttachesIdentityAndScopesToCtx(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "tenant-acme", UserID: "user-12345", SessionID: "sess-01"},
		Scopes:   []auth.Scope{auth.ScopeAdmin},
	}}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Tenant  string       `json:"tenant"`
		User    string       `json:"user"`
		Session string       `json:"session"`
		Scopes  []auth.Scope `json:"scopes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode echo body: %v", err)
	}
	if got.Tenant != "tenant-acme" || got.User != "user-12345" || got.Session != "sess-01" {
		t.Errorf("ctx identity: got %+v", got)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != auth.ScopeAdmin {
		t.Errorf("ctx scopes: got %v, want [admin]", got.Scopes)
	}
	if stub.calls != 1 {
		t.Errorf("validator called %d times, want 1", stub.calls)
	}
}

func TestMiddleware_NilValidator_FailsClosed(t *testing.T) {
	// A middleware built with a nil validator MUST fail every request
	// closed — never silently pass through (CLAUDE.md §5).
	mw := auth.Middleware(nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("nil-validator middleware unexpectedly called the wrapped handler")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", w.Code)
	}
}

// TestMiddleware_AuditOnRejection_DoesNotLeakToken pins the §7 rule 7
// invariant: the raw token never appears in the audit emit. We use a
// recorder logger and a known token suffix that would be unique enough
// to spot in the audit body if it leaked.
func TestMiddleware_AuditOnRejection_DoesNotLeakToken(t *testing.T) {
	const sentinelToken = "VERY-UNIQUE-TOKEN-DO-NOT-LOG-12345abcdef"

	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("RS256", pub)

	// Build a real validator with a recording logger to assert the
	// token never appears in the audit emit.
	rec := &recordingLogger{}
	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC) }),
		auth.WithLogger(rec.slog()),
		withTestRedactor(),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	// Build an EXPIRED token so Validate fails AND audits.
	expired := jwt.MapClaims{
		"iss":     "https://idp.test",
		"exp":     time.Date(2026, 5, 14, 11, 0, 0, 0, time.UTC).Unix(), // 1h ago
		"tenant":  "t",
		"user":    "u",
		"session": "s",
	}
	tok := signRS256(t, priv, expired, "k1")

	mw := auth.Middleware(v)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+tok+sentinelToken) // append a sentinel to make the token uniquely identifiable
	mw(echoHandler(t)).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	for _, line := range rec.lines {
		if strings.Contains(line, sentinelToken) {
			t.Fatalf("audit emit leaked the raw token: %q", line)
		}
	}
	// Also assert the audit emit DID happen — it carried at least the
	// "reason" key, even though the token is absent.
	hasAuditReason := false
	for _, line := range rec.lines {
		if strings.Contains(line, "reason") {
			hasAuditReason = true
			break
		}
	}
	if !hasAuditReason {
		t.Errorf("audit emit did not include the rejection reason — silent degradation? lines=%v", rec.lines)
	}
}

// TestMiddleware_AuditFromValidatorAndMiddleware_NeverPassesToken pins
// the same invariant from a different angle: even when the middleware
// itself logs the rejection (after the validator's audit), the token
// never appears.
func TestMiddleware_MiddlewareLogger_DoesNotLeakToken(t *testing.T) {
	const sentinelToken = "MW-LOG-SENTINEL-TOKEN-VALUE-67890"
	stub := &stubValidator{err: auth.ErrSignatureInvalid}
	rec := &recordingLogger{}
	mw := auth.Middleware(stub, auth.MWLogger(rec.slog()))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+sentinelToken)
	mw(echoHandler(t)).ServeHTTP(w, r)

	for _, line := range rec.lines {
		if strings.Contains(line, sentinelToken) {
			t.Fatalf("middleware logger leaked the raw token: %q", line)
		}
	}
}

// TestMiddleware_StubValidatorReturnsInvalidIdentity_500 — defensive
// path: a buggy Validator returns a Verified with an empty identity
// triple. The middleware must fail closed (500) rather than passing
// the empty identity through to the next handler.
func TestMiddleware_StubValidatorReturnsInvalidIdentity_500(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{}, // empty — fails identity.Validate
	}}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", w.Code)
	}
}

// TestProtocolErrorFor_KnownCodes_StableMapping pins the wire contract:
// the Validate-error → Protocol-code mapping is a fixed table.
func TestProtocolErrorFor_KnownCodes_StableMapping(t *testing.T) {
	cases := []struct {
		err      error
		wantCode protoerrors.Code
	}{
		{auth.ErrTokenMissing, protoerrors.CodeIdentityRequired},
		{auth.ErrIdentityClaimMissing, protoerrors.CodeIdentityRequired},
		{auth.ErrTokenMalformed, protoerrors.CodeAuthRejected},
		{auth.ErrAlgNotAllowed, protoerrors.CodeAuthRejected},
		{auth.ErrSignatureInvalid, protoerrors.CodeAuthRejected},
		{auth.ErrTokenExpired, protoerrors.CodeAuthRejected},
		{auth.ErrTokenNotYetValid, protoerrors.CodeAuthRejected},
		{auth.ErrUnknownKey, protoerrors.CodeAuthRejected},
		{auth.ErrAudienceMismatch, protoerrors.CodeAuthRejected},
		{auth.ErrIssuerMismatch, protoerrors.CodeAuthRejected},
	}
	for _, c := range cases {
		stub := &stubValidator{err: c.err}
		mw := auth.Middleware(stub)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
		r.Header.Set("Authorization", "Bearer faketoken")
		mw(echoHandler(t)).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%v: status %d, want 401", c.err, w.Code)
		}
		body := readErrorBody(t, w)
		if body.Code != c.wantCode {
			t.Errorf("%v: code=%q, want %q", c.err, body.Code, c.wantCode)
		}
	}
}

// TestMiddleware_AnyErrorOutsideKnownSentinels_StillRejected — defensive
// path: a validator that returns an error not wrapping any known
// sentinel still produces a closed-rejection (CodeAuthRejected as the
// fallback).
func TestMiddleware_UnknownValidatorError_FallsBackToAuthRejected(t *testing.T) {
	stub := &stubValidator{err: errors.New("custom validator error")}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	body := readErrorBody(t, w)
	if body.Code != protoerrors.CodeAuthRejected {
		t.Errorf("unknown error fall-through: code=%q, want %q", body.Code, protoerrors.CodeAuthRejected)
	}
}

// decodeEchoBody reads the echoHandler's JSON identity body.
func decodeEchoBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode echo body: %v (body=%q)", err, w.Body.String())
	}
	return m
}

// TestMiddleware_PerRequestSession_HeaderOverridesTokenClaim — D-171:
// the X-Harbor-Session header REPLACES the token's session claim while
// keeping the token-verified tenant + user. The connection token is a
// per-backend credential, not a single-session pin.
func TestMiddleware_PerRequestSession_HeaderOverridesTokenClaim(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "dev", UserID: "dev", SessionID: "default-claim"},
	}}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	r.Header.Set(auth.HeaderSession, "conversation-B")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	body := decodeEchoBody(t, w)
	if body["session"] != "conversation-B" {
		t.Errorf("session: got %v, want conversation-B (header must override claim)", body["session"])
	}
	if body["tenant"] != "dev" || body["user"] != "dev" {
		t.Errorf("tenant/user must stay token-verified: got tenant=%v user=%v", body["tenant"], body["user"])
	}
}

// TestMiddleware_PerRequestSession_FallsBackToTokenClaim — with no
// X-Harbor-Session header, the token's session claim is used as the
// default (back-compat for clients that don't yet send the header).
func TestMiddleware_PerRequestSession_FallsBackToTokenClaim(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "dev", UserID: "dev", SessionID: "default-claim"},
	}}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	body := decodeEchoBody(t, w)
	if body["session"] != "default-claim" {
		t.Errorf("session fallback: got %v, want default-claim", body["session"])
	}
}

// TestMiddleware_PerRequestSession_NoClaimNoHeader_401 — a token with no
// default session claim AND no header is a client error (the caller must
// choose a session): 401 + CodeIdentityRequired, NOT 500.
func TestMiddleware_PerRequestSession_NoClaimNoHeader_401(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "dev", UserID: "dev", SessionID: ""},
	}}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401 (body=%q)", w.Code, w.Body.String())
	}
	body := readErrorBody(t, w)
	if body.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("no-session code: got %q, want %q", body.Code, protoerrors.CodeIdentityRequired)
	}
}

// TestMiddleware_PerRequestSession_HeaderCannotWidenTenantOrUser —
// security: the header only sets session. A request can never widen its
// tenant or user; those stay token-verified. (There is no
// X-Harbor-Tenant / X-Harbor-User override path in the middleware.)
func TestMiddleware_PerRequestSession_HeaderCannotWidenTenantOrUser(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-A", SessionID: "s"},
	}}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	// Even if a client tries to spoof tenant/user via headers, the
	// middleware ignores them — only X-Harbor-Session is honoured.
	r.Header.Set("X-Harbor-Tenant", "tenant-EVIL")
	r.Header.Set("X-Harbor-User", "user-EVIL")
	r.Header.Set(auth.HeaderSession, "s2")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	body := decodeEchoBody(t, w)
	if body["tenant"] != "tenant-A" || body["user"] != "user-A" {
		t.Errorf("header must not widen tenant/user: got tenant=%v user=%v", body["tenant"], body["user"])
	}
	if body["session"] != "s2" {
		t.Errorf("session: got %v, want s2", body["session"])
	}
}
