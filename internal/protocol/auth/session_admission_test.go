package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
)

const (
	legacyTestAudience = "harbor-test-runtime"
	scopedTestAudience = "harbor-test-runtime-scoped"
)

func newScopedRSValidator(t *testing.T) (auth.Validator, *rsa.PrivateKey) {
	t.Helper()
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("RS256", pub)
	v, err := auth.NewValidator(keys, withTestRedactor(),
		auth.WithClock(func() time.Time { return fixedNow }),
		auth.WithAudience(legacyTestAudience), auth.WithScopedTokenAudience(scopedTestAudience))
	if err != nil {
		t.Fatal(err)
	}
	return v, priv
}

func admissionClaims() jwt.MapClaims {
	claims := validClaims(fixedNow)
	claims["aud"] = scopedTestAudience
	claims[auth.MethodReachClaim] = []string{string(methods.MethodUserMessage)}
	claims[auth.SessionAdmissionEpochClaim] = uint64(7)
	claims[auth.SessionAdmissionCoordinatorClaim] = "coordinator-a"
	return claims
}

func TestValidator_SessionAdmission_StrictSignedPair(t *testing.T) {
	v, priv := newScopedRSValidator(t)
	for _, tc := range []struct {
		name   string
		change func(jwt.MapClaims)
		bad    bool
		epoch  uint64
	}{
		{name: "valid", epoch: 7},
		{name: "exact above float precision", epoch: 9007199254740993, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = json.Number("9007199254740993") }},
		{name: "max uint64", epoch: ^uint64(0), change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = json.Number("18446744073709551615") }},
		{name: "missing epoch", bad: true, change: func(c jwt.MapClaims) { delete(c, auth.SessionAdmissionEpochClaim) }},
		{name: "missing coordinator", bad: true, change: func(c jwt.MapClaims) { delete(c, auth.SessionAdmissionCoordinatorClaim) }},
		{name: "missing method restriction", bad: true, change: func(c jwt.MapClaims) { delete(c, auth.MethodReachClaim) }},
		{name: "missing issuer", bad: true, change: func(c jwt.MapClaims) { delete(c, "iss") }},
		{name: "nonstring issuer", bad: true, change: func(c jwt.MapClaims) { c["iss"] = 3 }},
		{name: "whitespace issuer", bad: true, change: func(c jwt.MapClaims) { c["iss"] = " " }},
		{name: "zero epoch", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = 0 }},
		{name: "negative epoch", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = -1 }},
		{name: "fraction", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = json.Number("1.5") }},
		{name: "decimal integer", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = json.Number("7.0") }},
		{name: "exponent", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = json.Number("7e0") }},
		{name: "overflow", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = json.Number("18446744073709551616") }},
		{name: "string epoch", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = "7" }},
		{name: "null epoch", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = nil }},
		{name: "boolean epoch", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = true }},
		{name: "object epoch", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionEpochClaim] = map[string]any{} }},
		{name: "null coordinator", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionCoordinatorClaim] = nil }},
		{name: "number coordinator", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionCoordinatorClaim] = 7 }},
		{name: "blank coordinator", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionCoordinatorClaim] = "" }},
		{name: "whitespace coordinator", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionCoordinatorClaim] = " coordinator-a" }},
		{name: "control coordinator", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionCoordinatorClaim] = "coordinator\x00a" }},
		{name: "oversize coordinator", bad: true, change: func(c jwt.MapClaims) { c[auth.SessionAdmissionCoordinatorClaim] = strings.Repeat("a", 129) }},
		{name: "explicit empty method restriction", epoch: 7, change: func(c jwt.MapClaims) { c[auth.MethodReachClaim] = []string{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := admissionClaims()
			if tc.change != nil {
				tc.change(claims)
			}
			verified, err := v.Validate(context.Background(), signRS256(t, priv, claims, "k1"))
			if tc.bad {
				if !errors.Is(err, auth.ErrSessionAdmissionMalformed) {
					t.Fatalf("Validate = %v, want ErrSessionAdmissionMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := &auth.SessionAdmissionAuthority{Epoch: tc.epoch, Coordinator: "coordinator-a", Identity: verified.Identity}
			if !reflect.DeepEqual(verified.SessionAdmission, want) {
				t.Fatalf("admission = %#v, want %#v", verified.SessionAdmission, want)
			}
		})
	}
}

func TestMiddleware_SessionAdmission_BindsOriginalSignedIdentity(t *testing.T) {
	v, priv := newScopedRSValidator(t)
	claims := admissionClaims()
	token := signRS256(t, priv, claims, "k1")
	called := false
	handler := auth.Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		id, ok := identity.FromVerified(r.Context())
		if !ok || id.SessionID != "selected-other-session" {
			t.Fatalf("effective identity = %#v, %v", id, ok)
		}
		admission, present := auth.SessionAdmissionFrom(r.Context())
		if !present || admission.Identity.SessionID != claims["session"] || admission.Identity.TenantID != claims["tenant"] || admission.Identity.UserID != claims["user"] {
			t.Fatalf("signed identity changed with session selector: %#v", admission)
		}
		authority, present := auth.TokenAuthorityFrom(r.Context())
		if !present || authority.Issuer != claims["iss"] || authority.Subject != claims["sub"] {
			t.Fatalf("token authority = %#v, %v", authority, present)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/control/"+string(methods.MethodUserMessage), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(auth.HeaderSession, "selected-other-session")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("middleware = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSessionAdmission_ContextDefensiveCopies(t *testing.T) {
	original := &auth.SessionAdmissionAuthority{Epoch: 1, Coordinator: "coordinator-a", Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx := auth.WithSessionAdmission(context.Background(), original)
	original.Epoch = 2
	copyOne, present := auth.SessionAdmissionFrom(ctx)
	if !present || copyOne.Epoch != 1 {
		t.Fatalf("authority mutated after attach: %#v", copyOne)
	}
	copyOne.Identity.SessionID = "other"
	copyTwo, _ := auth.SessionAdmissionFrom(ctx)
	if copyTwo.Identity.SessionID != "s" {
		t.Fatal("returned authority can mutate context")
	}
	if _, present := auth.SessionAdmissionFrom(auth.WithSessionAdmission(ctx, nil)); present {
		t.Fatal("nil authority did not clear inherited authority")
	}
}

func TestValidator_RejectsDuplicateTopLevelClaims(t *testing.T) {
	v, priv := newScopedRSValidator(t)
	claims := admissionClaims()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	for _, duplicate := range []string{
		`"method_reach":[]`,
		`"method_\u0072each":[]`,
		`"session_admission_epoch":8`,
		`"session_admission_coordinator":"coordinator-b"`,
		`"iss":"other-issuer"`,
		`"sub":"other-subject"`,
		`"tenant":"other-tenant"`,
		`"aud":"other-audience"`,
		`"scopes":[]`,
	} {
		t.Run(duplicate, func(t *testing.T) {
			payload := string(body[:len(body)-1]) + "," + duplicate + "}"
			token := signRawJWTClaims(t, priv, payload)
			if _, err := v.Validate(context.Background(), token); !errors.Is(err, auth.ErrTokenMalformed) {
				t.Fatalf("duplicate authority claim accepted: %v", err)
			}
		})
	}
}

func signRawJWTClaims(t *testing.T, priv *rsa.PrivateKey, payload string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"k1","typ":"JWT"}`))
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	hash := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
