package integration

// mockoidc_test.go — a hermetic, in-test OIDC / OAuth2 issuer used to
// prove the production identity on-ramp end to end.
//
// It is deliberately a TEST FIXTURE, not a user-facing surface: the
// production self-issuing on-ramp for an operator with no IdP is the
// `harbor token` subcommand, and a real deployment fronts a real IdP
// (Auth0 / Okta / Keycloak / Cognito). This issuer exists only so the
// integration + wave-end smokes can drive the OAuth2 client-credentials
// → JWKS-verify → attach round-trip without an external container.
//
// Why it is trustworthy (CLAUDE.md §17.8 — fixtures derive from the real
// spec, not a self-consistent lookalike):
//
//   - It signs with golang-jwt/jwt/v5 (ES256) — the SAME library the
//     production parser internal/protocol/auth verifies with.
//   - It publishes its public key as a JWK Set in the exact shape the
//     production JWKS consumer internal/protocol/auth/jwks.go parses
//     (kty=EC, crv=P-256, base64url x/y, kid). A wrong field name or
//     placement makes `harbor serve`'s verifier reject the token and the
//     round-trip test FAILS — the fixture cannot rubber-stamp a broken
//     wiring.
//
// CGo-free: stdlib crypto/ecdsa + golang-jwt only.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// mockOIDC is a running in-test issuer. It serves a JWKS document and an
// OAuth2 client-credentials token endpoint, and mints ES256 JWTs carrying
// the Harbor identity claim shape the production parser enforces.
type mockOIDC struct {
	server *httptest.Server
	priv   *ecdsa.PrivateKey
	// rogue is a second signing key that is NOT published in the JWKS —
	// a token signed by it must be rejected by the verifier, which the
	// failure-mode assertion exercises.
	rogue *ecdsa.PrivateKey

	kid      string
	issuer   string // the issuer URL == the server base URL
	audience string
	jwksDoc  []byte // the published JWK Set, computed once at construction

	// the identity claims the issuer maps the client onto (the IdP's
	// claim-mapping result in a real deployment).
	tenant  string
	user    string
	session string
}

// mockOIDCConfig parameterizes the fixture.
type mockOIDCConfig struct {
	audience     string
	clientID     string
	clientSecret string
	tenant       string
	user         string
	session      string
}

// startMockOIDC boots the issuer on a loopback httptest server and
// registers cleanup. The returned issuer's URL is both the OIDC issuer
// (`iss`) and the host of the `/.well-known/jwks.json` + `/token`
// endpoints.
func startMockOIDC(t *testing.T, cfg mockOIDCConfig) *mockOIDC {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mockoidc: generate ES256 key: %v", err)
	}
	rogue, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mockoidc: generate rogue key: %v", err)
	}

	m := &mockOIDC{
		priv:     priv,
		rogue:    rogue,
		kid:      "mock-oidc-es256-1",
		audience: cfg.audience,
		tenant:   cfg.tenant,
		user:     cfg.user,
		session:  cfg.session,
	}
	m.jwksDoc = m.buildJWKS(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", m.serveJWKS)
	mux.HandleFunc("/token", m.serveToken(cfg.clientID, cfg.clientSecret, false))
	// /rogue-token mints a structurally-valid token signed by a key the
	// JWKS does not publish — the verifier must reject it.
	mux.HandleFunc("/rogue-token", m.serveToken(cfg.clientID, cfg.clientSecret, true))

	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	m.issuer = m.server.URL
	return m
}

// jwksURL is the published JWK Set endpoint `harbor serve` is pointed at.
func (m *mockOIDC) jwksURL() string { return m.server.URL + "/.well-known/jwks.json" }

// tokenURL is the OAuth2 token endpoint the worked client POSTs to.
func (m *mockOIDC) tokenURL() string { return m.server.URL + "/token" }

// rogueTokenURL mints a token signed by an unpublished key (failure mode).
func (m *mockOIDC) rogueTokenURL() string { return m.server.URL + "/rogue-token" }

// buildJWKS renders the issuer's public signing key as the RFC 7517 JWK
// Set the production consumer parses (EC / P-256 / base64url x,y). The
// coordinates come from the crypto/ecdh uncompressed-point encoding
// (0x04 || X(32) || Y(32)) — the non-deprecated Go 1.26 path — so the x/y
// fields are exactly the 32-byte, spec-shaped coordinates.
func (m *mockOIDC) buildJWKS(t *testing.T) []byte {
	t.Helper()
	ecdhPub, err := m.priv.PublicKey.ECDH()
	if err != nil {
		t.Fatalf("mockoidc: ECDH projection of P-256 key: %v", err)
	}
	raw := ecdhPub.Bytes() // 0x04 || X(32) || Y(32)
	if len(raw) != 65 || raw[0] != 0x04 {
		t.Fatalf("mockoidc: unexpected uncompressed point length %d", len(raw))
	}
	jwk := map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"use": "sig",
		"alg": "ES256",
		"kid": m.kid,
		"x":   base64.RawURLEncoding.EncodeToString(raw[1:33]),
		"y":   base64.RawURLEncoding.EncodeToString(raw[33:65]),
	}
	doc, err := json.Marshal(map[string]any{"keys": []any{jwk}})
	if err != nil {
		t.Fatalf("mockoidc: marshal JWKS: %v", err)
	}
	return doc
}

// serveJWKS publishes the precomputed JWK Set.
func (m *mockOIDC) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(m.jwksDoc)
}

// serveToken returns an OAuth2 client-credentials token endpoint handler.
// When rogue is true it signs with the unpublished key (the verifier must
// then reject the token at attach time).
func (m *mockOIDC) serveToken(clientID, clientSecret string, rogue bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
			return
		}
		if err := r.ParseForm(); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
			return
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only client_credentials is supported")
			return
		}
		// client_secret_basic — credentials in the Authorization header.
		id, secret, ok := r.BasicAuth()
		if !ok || id != clientID || secret != clientSecret {
			oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return
		}

		// The IdP grants the scopes the client requested (space-separated
		// per RFC 6749). A real IdP applies policy; the fixture echoes the
		// request so the smoke can assert a deterministic scope set.
		var scopes []string
		if raw := r.Form.Get("scope"); raw != "" {
			scopes = strings.Fields(raw)
		}

		signer := m.priv
		key := m.kid
		if rogue {
			signer = m.rogue // signed by a key the JWKS does not publish
		}

		now := time.Now()
		claims := jwt.MapClaims{
			"iss":     m.issuer,
			"aud":     m.audience,
			"sub":     m.user,
			"iat":     now.Unix(),
			"exp":     now.Add(time.Hour).Unix(),
			"tenant":  m.tenant,
			"user":    m.user,
			"session": m.session,
			"scopes":  scopes,
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
		tok.Header["kid"] = key
		signed, err := tok.SignedString(signer)
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "mint failed")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": signed,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}
}

// oauthError writes an RFC 6749 §5.2 error response.
func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}
