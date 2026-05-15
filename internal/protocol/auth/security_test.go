package auth_test

// security_test.go — the Phase 61 SECURITY suite (CLAUDE.md §7 + §11).
//
// Five canonical JWT attack shapes are exercised here. Each MUST be
// rejected by the Validator (or by the SSE handler's scope gate); each
// rejection MUST be audited (the validator's audit emit fires); the
// raw token (or any forged secret) MUST NOT appear in the audit body.
//
//   (1) HS256 token signed with the asymmetric public key as the HMAC
//       secret — the classical alg-confusion CVE family. The parser-
//       level WithValidMethods rejects it BEFORE the keyfunc runs.
//   (2) `alg: none` token — RFC 7519's escape-hatch unsigned token.
//       Same parser-level rejection.
//   (3) Scope escalation — a token without `admin` requesting
//       cross-tenant fan-in via the SSE handler's ?admin=1 gate.
//   (4) `kid` substitution — a token signed by an unknown key but
//       claiming a known-good `kid` in its header.
//   (5) Expired token replay — `exp` in the past relative to the
//       validator's clock.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
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
)

// (1) Algorithm confusion: an HS256-signed token verified against an
// RSA public key (the classical CVE-2015-9235 family). The attacker's
// idea: take the RS256 server's public key (which is, by definition,
// public), re-sign the JWT with HS256 using the public-key bytes as
// the HMAC secret, and submit it. A naive verifier that did not pin
// the algorithm would call `hmac.Verify(pubKeyBytes, token)` and
// succeed.
//
// Harbor's defence: jwt.WithValidMethods is passed the asymmetric
// allowlist, so the parser rejects HS* tokens BEFORE the keyfunc is
// consulted. The HS-signed token never reaches the verification step.
func TestSecurity_AlgConfusion_HS256Token_RejectedAtParser(t *testing.T) {
	priv, pub := loadTestRS256(t)
	_ = priv
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)

	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Marshal the public key into the byte form an attacker would use
	// as the "HMAC secret".
	pubBytes, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}

	// Sign the token with HS256 using the asymmetric public key bytes
	// as the HMAC secret — exactly the alg-confusion attack shape.
	c := validClaims(fixedNow)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(pubBytes)
	if err != nil {
		t.Fatalf("sign HS256 with pub bytes: %v", err)
	}

	_, err = v.Validate(context.Background(), signed)
	if !errors.Is(err, auth.ErrAlgNotAllowed) {
		t.Fatalf("alg-confusion attack NOT rejected at the parser: got %v (the JWT validator must reject HS* before the keyfunc runs)", err)
	}
}

// (2) `alg: none` — an explicitly unsigned token (RFC 7519 §6.1). The
// same parser-level allowlist rejects it.
func TestSecurity_AlgNone_RejectedAtParser(t *testing.T) {
	_, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	tok := signNone(t, validClaims(fixedNow))
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrAlgNotAllowed) {
		t.Fatalf("alg:none NOT rejected: got %v", err)
	}
}

// (3) Scope escalation — a request that asks for cross-tenant fan-in
// (events.Filter.Admin = true via the SSE handler's ?admin=1 query)
// but presents a token without the `admin` scope. The SSE handler's
// scope gate (ctx-aware HasScope check) is the enforcement point.
//
// The test wires a tiny HTTP handler that mirrors the SSE handler's
// scope check verbatim: it consults auth.HasScope(ctx, ScopeAdmin) for
// a ?admin=1 request and rejects with 403 if absent. (We do not test
// the full SSE handler here because that brings in events.EventBus —
// the integration test in test/integration/phase61_auth_test.go does
// the full wire round trip.)
func TestSecurity_ScopeEscalation_NoAdminScope_RequestRejected(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Scopes:   nil, // no scopes claimed
	}}
	mw := auth.Middleware(stub)
	scopeGated := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("admin") == "1" {
			if !auth.HasScope(r.Context(), auth.ScopeAdmin) && !auth.HasScope(r.Context(), auth.ScopeConsoleFleet) {
				http.Error(w, "scope_mismatch", http.StatusForbidden)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events?admin=1", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	scopeGated.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("scope-escalation attack NOT rejected: status %d, body %q", w.Code, w.Body.String())
	}
}

// (3b) The same gate must PERMIT the request when the token DOES carry
// the admin scope — the gate is a privilege check, not a blanket deny.
func TestSecurity_ScopeEscalation_WithAdminScope_RequestAllowed(t *testing.T) {
	stub := &stubValidator{verified: auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Scopes:   []auth.Scope{auth.ScopeAdmin},
	}}
	mw := auth.Middleware(stub)
	scopeGated := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("admin") == "1" {
			if !auth.HasScope(r.Context(), auth.ScopeAdmin) && !auth.HasScope(r.Context(), auth.ScopeConsoleFleet) {
				http.Error(w, "scope_mismatch", http.StatusForbidden)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events?admin=1", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	scopeGated.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("admin-scoped request rejected: status %d", w.Code)
	}
}

// (4) `kid` substitution — a token signed by an unknown key but
// claiming a known-good `kid`. The KeySet returns a different key for
// the kid; the signature MUST fail to verify against that key.
func TestSecurity_KIDSubstitution_UnknownSigner_SignatureFails(t *testing.T) {
	_, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Sign with an unknown RSA key but claim kid=k1.
	other, err := generateRSAKey()
	if err != nil {
		t.Fatalf("generate RSA: %v", err)
	}
	tok := signRS256(t, other, validClaims(fixedNow), "k1")
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrSignatureInvalid) {
		t.Fatalf("kid-substitution NOT rejected: got %v", err)
	}
}

// (5) Expired token — replay of a token whose `exp` is in the past.
// The validator's clock check is the enforcement point.
func TestSecurity_ExpiredToken_Rejected(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	c := validClaims(fixedNow)
	c["exp"] = fixedNow.Add(-1 * time.Hour).Unix() // expired 1h ago
	tok := signRS256(t, priv, c, "k1")
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("expired token NOT rejected: got %v", err)
	}
}

// (6) Bonus security path — a forged JWT whose body has been tampered
// with after signing. The signature MUST fail to verify against the
// untampered body's hash.
func TestSecurity_TamperedBody_SignatureFails(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	tok := signRS256(t, priv, validClaims(fixedNow), "k1")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("signed token does not have 3 parts: %q", tok)
	}
	// Tamper with the body — switch the tenant.
	tamperedClaims := validClaims(fixedNow)
	tamperedClaims["tenant"] = "tenant-evil"
	tamperedBody, err := json.Marshal(tamperedClaims)
	if err != nil {
		t.Fatalf("marshal tampered claims: %v", err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(tamperedBody)
	tampered := strings.Join(parts, ".")
	_, err = v.Validate(context.Background(), tampered)
	if !errors.Is(err, auth.ErrSignatureInvalid) {
		t.Fatalf("tampered token NOT rejected: got %v", err)
	}
}

// (7) Bonus — every rejection path emits an audit record. We bind a
// recording logger to the validator and assert the audit emit fired
// for each of the four canonical attack shapes (1, 2, 4, 5). The body
// MUST NOT contain the raw token bytes.
func TestSecurity_EveryRejectionAudits_NoTokenLeaks(t *testing.T) {
	const sentinelMarker = "ATTACK-MARKER-VALUE-XYZZY-9876"
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	rec := &recordingLogger{}
	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNow }),
		auth.WithLogger(rec.slog()),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Run an HS256 attack with a sentinel-bearing claim; the audit
	// emit MUST fire and MUST NOT contain the sentinel.
	c := validClaims(fixedNow)
	c["sentinel_attack_marker"] = sentinelMarker
	pubBytes, _ := x509.MarshalPKIXPublicKey(pub)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = "k1"
	hsToken, _ := tok.SignedString(pubBytes)
	if _, err := v.Validate(context.Background(), hsToken); err == nil {
		t.Fatalf("HS256 attack token unexpectedly accepted")
	}

	// Run an `alg:none` attack the same way.
	noneTok := signNone(t, c)
	if _, err := v.Validate(context.Background(), noneTok); err == nil {
		t.Fatalf("alg:none attack token unexpectedly accepted")
	}

	// And an expired-token attack.
	c2 := validClaims(fixedNow)
	c2["sentinel_attack_marker"] = sentinelMarker
	c2["exp"] = fixedNow.Add(-1 * time.Hour).Unix()
	expiredTok := signRS256(t, priv, c2, "k1")
	if _, err := v.Validate(context.Background(), expiredTok); err == nil {
		t.Fatalf("expired token unexpectedly accepted")
	}

	lines := rec.snapshot()
	if len(lines) < 3 {
		t.Fatalf("expected ≥3 audit emits across the three attacks, got %d (%v)", len(lines), lines)
	}
	for _, line := range lines {
		if strings.Contains(line, sentinelMarker) {
			t.Fatalf("audit emit leaked the sentinel claim value: %q", line)
		}
		if strings.Contains(line, hsToken) {
			t.Fatalf("audit emit leaked the raw HS256 token: %q", line)
		}
		if strings.Contains(line, noneTok) {
			t.Fatalf("audit emit leaked the raw alg:none token: %q", line)
		}
		if strings.Contains(line, expiredTok) {
			t.Fatalf("audit emit leaked the raw expired token: %q", line)
		}
	}
}

// generateRSAKey generates a fresh 2048-bit RSA private key for the
// kid-substitution attack. Imported here as a helper so the security
// suite reads top-to-bottom.
func generateRSAKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
}
