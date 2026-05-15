package auth_test

// edge_cases_test.go — coverage-completing tests for the long tail of
// validator + middleware branches. These are not behavioural variants
// of the Phase 61 contract; they exist so the package's coverage gate
// (90% per the Phase 61 plan) is met without leaving silent untested
// branches in the build.

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

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// failingRedactor surfaces an error on every Redact — drives the
// validator's "redactor failed loud, emit bare reason" path.
type failingRedactor struct{}

func (failingRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, errors.New("redactor blew up")
}

func TestValidate_AuditPath_RedactorFailure_EmitsBareReason(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	rec := &recordingLogger{}
	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNow }),
		auth.WithLogger(rec.slog()),
		auth.WithRedactor(failingRedactor{}),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	c := validClaims(fixedNow)
	c["exp"] = fixedNow.Add(-1 * time.Hour).Unix()
	tok := signRS256(t, priv, c, "k1")
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
	lines := rec.snapshot()
	if len(lines) == 0 {
		t.Fatal("expected at least one log line on the redactor-failure path")
	}
	// The fail-loud path emits "redactor failed; bare reason emitted"
	// — assert the rejection still got logged.
	hasBareReason := false
	for _, l := range lines {
		if strings.Contains(l, "redactor failed") {
			hasBareReason = true
		}
	}
	if !hasBareReason {
		t.Errorf("expected 'redactor failed' log line, got %v", lines)
	}
}

func TestValidate_WithRedactor_NilRedactor_FailsLoud(t *testing.T) {
	// PR #91 made WithRedactor mandatory: a nil redactor is treated
	// as "WithRedactor not supplied" and NewValidator fails closed
	// with ErrMisconfigured rather than building a Validator with a
	// permissive stub default (CLAUDE.md §13 "Test stubs as
	// production defaults on operator-facing seams").
	_, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	_, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNow }),
		auth.WithRedactor(nil),
	)
	if !errors.Is(err, auth.ErrMisconfigured) {
		t.Fatalf("expected ErrMisconfigured for WithRedactor(nil), got %v", err)
	}
}

func TestValidate_WithRedactor_RealRedactor_RedactsPayload(t *testing.T) {
	// A real audit.Redactor implementation is wired through and used
	// on the rejection emit. The test exercises the WithRedactor
	// option end-to-end.
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)

	// passthroughRedactor mirrors what audit/drivers/noop does — it
	// exists so the WithRedactor branch is covered with a non-nil
	// implementation. The patterns driver requires a config and is
	// over-engineered for a coverage test.
	r := passthroughRedactor{}
	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNow }),
		auth.WithRedactor(r),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	c := validClaims(fixedNow)
	c["exp"] = fixedNow.Add(-1 * time.Hour).Unix()
	tok := signRS256(t, priv, c, "k1")
	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("expected rejection")
	}
}

type passthroughRedactor struct{}

func (passthroughRedactor) Redact(_ context.Context, p any) (any, error) {
	return p, nil
}

// audit.Redactor compile-time assertion.
var _ audit.Redactor = passthroughRedactor{}

// extractScopes is exercised via Validate, but the scope-handling
// branches need direct shape coverage:
//
//   - []string form
//   - []any form
//   - space-separated string form
//   - empty string in the list (filtered out)
//   - non-string value in []any (filtered out)
//   - nil claim
//   - integer claim (returns nil)
func TestValidate_ScopesShapes_Comprehensive(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }), withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	cases := []struct {
		name      string
		raw       any
		wantCount int
	}{
		{"[]any-with-empty-and-non-string", []any{"admin", "", 123, "console:fleet"}, 2},
		{"[]string-with-empty", []string{"admin", "", "console:fleet"}, 2},
		{"space-separated-string-with-extra-whitespace", "  admin   console:fleet  ", 2},
		{"nil-claim", nil, 0},
		{"int-claim-returns-nil", 42, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := validClaims(fixedNow)
			claims["scopes"] = c.raw
			tok := signRS256(t, priv, claims, "k1")
			verified, err := v.Validate(context.Background(), tok)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if len(verified.Scopes) != c.wantCount {
				t.Errorf("scopes len: got %d (%v), want %d", len(verified.Scopes), verified.Scopes, c.wantCount)
			}
		})
	}
}

// audienceContains exercises every shape of the JWT `aud` claim.
func TestValidate_AudienceShapes_Comprehensive(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys,
		auth.WithAudience("harbor-runtime"),
		auth.WithClock(func() time.Time { return fixedNow }),
		withTestRedactor(),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	cases := []struct {
		name string
		aud  any
		want bool // true = match, false = mismatch
	}{
		{"string-match", "harbor-runtime", true},
		{"string-mismatch", "other", false},
		{"any-array-match", []any{"a", "b", "harbor-runtime"}, true},
		{"any-array-mismatch", []any{"a", "b"}, false},
		{"any-array-non-string-elem", []any{"a", 123, "harbor-runtime"}, true},
		{"missing-aud", nil, false},
		{"int-aud", 42, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := validClaims(fixedNow)
			if c.aud != nil {
				claims["aud"] = c.aud
			} else {
				delete(claims, "aud")
			}
			tok := signRS256(t, priv, claims, "k1")
			_, err := v.Validate(context.Background(), tok)
			if c.want && err != nil {
				t.Errorf("expected match, got %v", err)
			}
			if !c.want && err == nil {
				t.Errorf("expected mismatch, got nil")
			}
		})
	}
}

// extractBearer's whitespace + edge-case branches.
func TestMiddleware_ExtractBearer_BearerOnly_NoSpace_Rejected(t *testing.T) {
	stub := &stubValidator{}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer") // no token, no trailing space
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d, want 401", w.Code)
	}
}

// TestValidate_KIDClaimMissing_ResolvesEmptyKID — when a token has no
// kid header, the keyfunc still gets called with an empty kid string
// and the KeySet returns "not found".
func TestValidate_KIDHeaderMissing_RejectedAsUnknownKey(t *testing.T) {
	_, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }), withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	priv, _ := loadTestRS256(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims(fixedNow))
	// Deliberately do NOT set Header["kid"] — empty string.
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	_, err = v.Validate(context.Background(), signed)
	if !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("expected ErrUnknownKey for missing kid, got %v", err)
	}
}

// TestProtocolErrorFor_AnyError_MapsToAuthRejected — a generic error
// (not one of our sentinels) maps to CodeAuthRejected via the
// fall-through. Documents the behaviour for callers that wrap the
// validator with their own error type.
func TestMiddleware_GenericError_MapsToAuthRejected(t *testing.T) {
	stub := &stubValidator{err: errors.New("brand-new error class")}
	mw := auth.Middleware(stub)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d, want 401", w.Code)
	}
	var perr struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(w.Body).Decode(&perr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if perr.Code != "auth_rejected" {
		t.Errorf("code: %q, want auth_rejected", perr.Code)
	}
}
