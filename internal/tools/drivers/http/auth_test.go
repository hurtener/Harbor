package http

import (
	"errors"
	nethttp "net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAuthSpec_Validate(t *testing.T) {
	cases := []struct { //nolint:govet // fieldalignment on a test-only struct; field order kept for readability
		name    string
		spec    AuthSpec
		wantErr error
	}{
		{"none ok", AuthSpec{Kind: AuthKindNone}, nil},
		{"api_key header ok", AuthSpec{Kind: AuthKindAPIKey, HeaderName: "X-API-Key"}, nil},
		{"api_key query ok", AuthSpec{Kind: AuthKindAPIKey, QueryParam: "api_key"}, nil},
		{"api_key both rejected", AuthSpec{Kind: AuthKindAPIKey, HeaderName: "X", QueryParam: "y"}, ErrAuthInvalidSpec},
		{"api_key neither rejected", AuthSpec{Kind: AuthKindAPIKey}, ErrAuthInvalidSpec},
		{"bearer ok", AuthSpec{Kind: AuthKindBearer}, nil},
		{"cookie ok", AuthSpec{Kind: AuthKindCookie, CookieName: "session"}, nil},
		{"cookie missing name", AuthSpec{Kind: AuthKindCookie}, ErrAuthInvalidSpec},
		{"unknown kind rejected", AuthSpec{Kind: AuthKind("totp")}, ErrAuthInvalidSpec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected errors.Is %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestApplyAuth_APIKey_Header(t *testing.T) {
	req, _ := nethttp.NewRequest("GET", "https://example.com/v1/now", nil)
	spec := AuthSpec{Kind: AuthKindAPIKey, HeaderName: "X-API-Key"}
	if err := applyAuth(req, spec, "secret-value"); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got := req.Header.Get("X-API-Key"); got != "secret-value" {
		t.Errorf("expected X-API-Key=secret-value, got %q", got)
	}
}

func TestApplyAuth_APIKey_Query(t *testing.T) {
	req, _ := nethttp.NewRequest("GET", "https://example.com/v1/now?city=Lyon", nil)
	spec := AuthSpec{Kind: AuthKindAPIKey, QueryParam: "api_key"}
	if err := applyAuth(req, spec, "secret-value"); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	parsed, _ := url.Parse(req.URL.String())
	if got := parsed.Query().Get("api_key"); got != "secret-value" {
		t.Errorf("expected api_key=secret-value, got %q", got)
	}
	if got := parsed.Query().Get("city"); got != "Lyon" {
		t.Errorf("query param 'city' lost: got %q", got)
	}
}

func TestApplyAuth_Bearer(t *testing.T) {
	req, _ := nethttp.NewRequest("POST", "https://example.com/api", nil)
	spec := AuthSpec{Kind: AuthKindBearer}
	if err := applyAuth(req, spec, "abc123"); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	got := req.Header.Get("Authorization")
	if got != "Bearer abc123" {
		t.Errorf("expected 'Bearer abc123', got %q", got)
	}
}

func TestApplyAuth_Cookie(t *testing.T) {
	req, _ := nethttp.NewRequest("GET", "https://example.com/me", nil)
	spec := AuthSpec{Kind: AuthKindCookie, CookieName: "session"}
	if err := applyAuth(req, spec, "abc123"); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	cookies := req.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	if cookies[0].Name != "session" || cookies[0].Value != "abc123" {
		t.Errorf("unexpected cookie: name=%q value=%q", cookies[0].Name, cookies[0].Value)
	}
}

func TestApplyAuth_None_NoOp(t *testing.T) {
	req, _ := nethttp.NewRequest("GET", "https://example.com/", nil)
	if err := applyAuth(req, AuthSpec{Kind: AuthKindNone}, ""); err != nil {
		t.Fatalf("applyAuth(none): %v", err)
	}
	if len(req.Header) != 0 {
		t.Errorf("expected no headers, got %v", req.Header)
	}
}

func TestApplyAuth_MissingSecret(t *testing.T) {
	req, _ := nethttp.NewRequest("GET", "https://example.com/", nil)
	spec := AuthSpec{Kind: AuthKindBearer}
	err := applyAuth(req, spec, "")
	if !errors.Is(err, ErrAuthMissing) {
		t.Fatalf("expected ErrAuthMissing, got %v", err)
	}
	err = applyAuth(req, spec, "   ")
	if !errors.Is(err, ErrAuthMissing) {
		t.Fatalf("whitespace secret should also fail: got %v", err)
	}
	// Sanity: the error string mentions the kind.
	if !strings.Contains(err.Error(), "bearer") {
		t.Errorf("error should mention kind: %v", err)
	}
}
