package auth_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// staticKeySet is the test KeySet: a fixed map of kid → (key, alg).
// Mirrors what a real KeySet driver does without the JWKS dance.
type staticKeySet struct {
	keys map[string]struct {
		key crypto.PublicKey
		alg string
	}
}

func newStaticKeySet() *staticKeySet {
	return &staticKeySet{
		keys: map[string]struct {
			key crypto.PublicKey
			alg string
		}{},
	}
}

func (s *staticKeySet) add(kid, alg string, key crypto.PublicKey) {
	s.keys[kid] = struct {
		key crypto.PublicKey
		alg string
	}{key: key, alg: alg}
}

func (s *staticKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	entry, ok := s.keys[kid]
	if !ok {
		return nil, "", errors.New("kid not found")
	}
	return entry.key, entry.alg, nil
}

// loadTestRS256 reads the dummy RS256 keypair from testdata/. The
// keypair is documented as test-only in testdata/README.md.
func loadTestRS256(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	priv := readPEM(t, "testdata/rs256_private.pem")
	pub := readPEM(t, "testdata/rs256_public.pem")
	rsaPriv, err := x509.ParsePKCS1PrivateKey(priv)
	if err != nil {
		// Try PKCS8.
		k, perr := x509.ParsePKCS8PrivateKey(priv)
		if perr != nil {
			t.Fatalf("parse RS256 private: PKCS1=%v PKCS8=%v", err, perr)
		}
		var ok bool
		rsaPriv, ok = k.(*rsa.PrivateKey)
		if !ok {
			t.Fatalf("PKCS8 key is not *rsa.PrivateKey")
		}
	}
	rsaPubAny, err := x509.ParsePKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("parse RS256 public: %v", err)
	}
	rsaPub, ok := rsaPubAny.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("public key is not *rsa.PublicKey")
	}
	return rsaPriv, rsaPub
}

// loadTestES256 reads the dummy ES256 keypair from testdata/.
func loadTestES256(t *testing.T) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()
	priv := readPEM(t, "testdata/es256_private.pem")
	pub := readPEM(t, "testdata/es256_public.pem")
	ecPriv, err := x509.ParseECPrivateKey(priv)
	if err != nil {
		k, perr := x509.ParsePKCS8PrivateKey(priv)
		if perr != nil {
			t.Fatalf("parse ES256 private: EC=%v PKCS8=%v", err, perr)
		}
		var ok bool
		ecPriv, ok = k.(*ecdsa.PrivateKey)
		if !ok {
			t.Fatalf("PKCS8 key is not *ecdsa.PrivateKey")
		}
	}
	ecPubAny, err := x509.ParsePKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("parse ES256 public: %v", err)
	}
	ecPub, ok := ecPubAny.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is not *ecdsa.PublicKey")
	}
	return ecPriv, ecPub
}

func readPEM(t *testing.T, rel string) []byte {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("abs %q: %v", rel, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %q: %v", rel, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("no PEM block in %q", rel)
	}
	return block.Bytes
}

// signRS256 signs a token with the supplied RS256 private key.
func signRS256(t *testing.T, priv *rsa.PrivateKey, claims jwt.MapClaims, kid string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign RS256: %v", err)
	}
	return signed
}

// signES256 signs a token with the supplied ES256 private key.
func signES256(t *testing.T, priv *ecdsa.PrivateKey, claims jwt.MapClaims, kid string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return signed
}

// signHS256 signs a token with HS256 (a SYMMETRIC algorithm) and a
// shared secret. Used to assert the validator rejects HS* at the
// parser level.
func signHS256(t *testing.T, secret []byte, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	return signed
}

// signNone signs a token with `alg: none` (an explicit unsigned
// token) — RFC 7519 §6.1's escape hatch the validator MUST reject.
func signNone(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	return signed
}

// validClaims builds a baseline-valid claims set for the validator.
// Tests mutate this to exercise each rejection path.
func validClaims(now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     "user-12345",
		"exp":     now.Add(15 * time.Minute).Unix(),
		"nbf":     now.Add(-1 * time.Minute).Unix(),
		"tenant":  "tenant-acme",
		"user":    "user-12345",
		"session": "sess-01HX0000000000000000000000",
		"scopes":  []string{"admin"},
	}
}

func newRSValidator(t *testing.T, fixedNow time.Time) (auth.Validator, *rsa.PrivateKey) {
	t.Helper()
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }), withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v, priv
}

func newESValidator(t *testing.T, fixedNow time.Time) (auth.Validator, *ecdsa.PrivateKey) {
	t.Helper()
	priv, pub := loadTestES256(t)
	keys := newStaticKeySet()
	keys.add("k1", "ES256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }), withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v, priv
}

// fixedNow is the deterministic clock the time-sensitive tests use.
var fixedNow = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

func TestNewValidator_NilKeySet_FailsLoud(t *testing.T) {
	_, err := auth.NewValidator(nil, withTestRedactor())
	if !errors.Is(err, auth.ErrMisconfigured) {
		t.Fatalf("expected ErrMisconfigured, got %v", err)
	}
}

// TestNewValidator_MissingRedactor_FailsLoud — the WithRedactor option
// is mandatory after PR #91 (CLAUDE.md §13 "Test stubs as production
// defaults on operator-facing seams"); construction without it must
// fail closed with ErrMisconfigured rather than building a Validator
// that uses a permissive in-process stub.
func TestNewValidator_MissingRedactor_FailsLoud(t *testing.T) {
	keys := newStaticKeySet()
	_, pub := loadTestRS256(t)
	keys.add("k1", "RS256", pub)
	_, err := auth.NewValidator(keys)
	if !errors.Is(err, auth.ErrMisconfigured) {
		t.Fatalf("expected ErrMisconfigured (missing redactor), got %v", err)
	}
}

func TestValidate_HappyPath_RS256_ReturnsVerified(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	tok := signRS256(t, priv, validClaims(fixedNow), "k1")
	verified, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if verified.Identity.TenantID != "tenant-acme" {
		t.Errorf("tenant: got %q", verified.Identity.TenantID)
	}
	if verified.Identity.UserID != "user-12345" {
		t.Errorf("user: got %q", verified.Identity.UserID)
	}
	if verified.Identity.SessionID == "" {
		t.Errorf("session empty")
	}
	if err := identity.Validate(verified.Identity); err != nil {
		t.Errorf("returned identity does not Validate: %v", err)
	}
	if len(verified.Scopes) != 1 || verified.Scopes[0] != auth.ScopeAdmin {
		t.Errorf("scopes: got %v", verified.Scopes)
	}
	if verified.Issuer != "https://idp.test" || verified.Subject != "user-12345" {
		t.Errorf("iss/sub: %+v", verified)
	}
}

func TestValidate_HappyPath_ES256_ReturnsVerified(t *testing.T) {
	v, priv := newESValidator(t, fixedNow)
	tok := signES256(t, priv, validClaims(fixedNow), "k1")
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_TokenMissing(t *testing.T) {
	v, _ := newRSValidator(t, fixedNow)
	_, err := v.Validate(context.Background(), "")
	if !errors.Is(err, auth.ErrTokenMissing) {
		t.Fatalf("expected ErrTokenMissing, got %v", err)
	}
}

func TestValidate_HS256_RejectedAtParser(t *testing.T) {
	v, _ := newRSValidator(t, fixedNow)
	tok := signHS256(t, []byte("supersecret"), validClaims(fixedNow))
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrAlgNotAllowed) {
		t.Fatalf("expected ErrAlgNotAllowed, got %v", err)
	}
}

func TestValidate_AlgNone_RejectedAtParser(t *testing.T) {
	v, _ := newRSValidator(t, fixedNow)
	tok := signNone(t, validClaims(fixedNow))
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrAlgNotAllowed) {
		t.Fatalf("expected ErrAlgNotAllowed, got %v", err)
	}
}

func TestValidate_TokenMalformed(t *testing.T) {
	v, _ := newRSValidator(t, fixedNow)
	_, err := v.Validate(context.Background(), "not.a.jwt")
	if !errors.Is(err, auth.ErrTokenMalformed) && !errors.Is(err, auth.ErrSignatureInvalid) {
		t.Fatalf("expected ErrTokenMalformed or ErrSignatureInvalid, got %v", err)
	}
}

func TestValidate_SignatureInvalid_DifferentRSAKey(t *testing.T) {
	v, _ := newRSValidator(t, fixedNow)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tok := signRS256(t, other, validClaims(fixedNow), "k1")
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid, got %v", err)
	}
}

func TestValidate_TokenExpired(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	c["exp"] = fixedNow.Add(-1 * time.Minute).Unix() // already expired
	tok := signRS256(t, priv, c, "k1")
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestValidate_TokenWithoutExp_RejectedAsExpired(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	delete(c, "exp")
	tok := signRS256(t, priv, c, "k1")
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired (no exp claim), got %v", err)
	}
}

func TestValidate_TokenNotYetValid(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	c["nbf"] = fixedNow.Add(10 * time.Minute).Unix() // future nbf
	tok := signRS256(t, priv, c, "k1")
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrTokenNotYetValid) {
		t.Fatalf("expected ErrTokenNotYetValid, got %v", err)
	}
}

func TestValidate_UnknownKey_KIDNotInKeySet(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	tok := signRS256(t, priv, validClaims(fixedNow), "k-unknown")
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("expected ErrUnknownKey, got %v", err)
	}
}

func TestValidate_IdentityClaimMissing_NoTenant(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	delete(c, "tenant")
	tok := signRS256(t, priv, c, "k1")
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrIdentityClaimMissing) {
		t.Fatalf("expected ErrIdentityClaimMissing (no tenant), got %v", err)
	}
}

func TestValidate_IdentityClaimMissing_NoSession(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	delete(c, "session")
	tok := signRS256(t, priv, c, "k1")
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrIdentityClaimMissing) {
		t.Fatalf("expected ErrIdentityClaimMissing (no session), got %v", err)
	}
}

func TestValidate_IdentityClaimMissing_NoUser(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	delete(c, "user")
	tok := signRS256(t, priv, c, "k1")
	_, err := v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrIdentityClaimMissing) {
		t.Fatalf("expected ErrIdentityClaimMissing (no user), got %v", err)
	}
}

func TestValidate_IssuerMismatch(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys,
		auth.WithIssuer("https://idp.expected"),
		auth.WithClock(func() time.Time { return fixedNow }),
		withTestRedactor(),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	tok := signRS256(t, priv, validClaims(fixedNow), "k1") // iss = https://idp.test
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrIssuerMismatch) {
		t.Fatalf("expected ErrIssuerMismatch, got %v", err)
	}
}

func TestValidate_AudienceMismatch(t *testing.T) {
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
	c := validClaims(fixedNow)
	c["aud"] = "different-audience"
	tok := signRS256(t, priv, c, "k1")
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrAudienceMismatch) {
		t.Fatalf("expected ErrAudienceMismatch, got %v", err)
	}
}

func TestValidate_AudienceMatch_AcceptsArrayClaim(t *testing.T) {
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
	c := validClaims(fixedNow)
	c["aud"] = []any{"other-aud", "harbor-runtime"}
	tok := signRS256(t, priv, c, "k1")
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_ScopesArrayClaim(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	c["scopes"] = []string{"admin", "console:fleet"}
	tok := signRS256(t, priv, c, "k1")
	verified, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(verified.Scopes) != 2 {
		t.Fatalf("scopes: got %v, want 2", verified.Scopes)
	}
}

func TestValidate_ScopesSpaceSeparatedString(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	c["scopes"] = "admin console:fleet"
	tok := signRS256(t, priv, c, "k1")
	verified, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(verified.Scopes) != 2 {
		t.Fatalf("scopes: got %v, want 2", verified.Scopes)
	}
}

func TestValidate_NoScopes_AuthenticatedButEmptyScopes(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	c := validClaims(fixedNow)
	delete(c, "scopes")
	tok := signRS256(t, priv, c, "k1")
	verified, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(verified.Scopes) != 0 {
		t.Fatalf("expected empty scopes, got %v", verified.Scopes)
	}
}

func TestValidate_KeySetReturnsNonAsymmetric_RejectedAsUnknownKey(t *testing.T) {
	priv, _ := loadTestRS256(t)
	// Build a KeySet that returns a *symmetric* HMAC secret (a non-
	// asymmetric key shape) for kid `k1` — exactly the malicious-KeySet
	// shape an algorithm-confusion attack would need.
	keys := newStaticKeySet()
	keys.add("k1", "HS256", []byte("not-asymmetric"))
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }), withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	tok := signRS256(t, priv, validClaims(fixedNow), "k1")
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("expected ErrUnknownKey (non-asymmetric key shape), got %v", err)
	}
}

func TestValidate_KIDMismatchesAlg_KeySetSaysES256_TokenSaysRS256(t *testing.T) {
	rsaPriv, _ := loadTestRS256(t)
	_, ecPub := loadTestES256(t)
	keys := newStaticKeySet()
	// Lie about the algorithm of kid k1 — the kid resolves to an
	// ECDSA key but the KeySet says ES256, while the token uses RS256.
	keys.add("k1", "ES256", ecPub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }), withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	tok := signRS256(t, rsaPriv, validClaims(fixedNow), "k1")
	_, err = v.Validate(context.Background(), tok)
	if !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("expected ErrUnknownKey (alg disagrees with KeySet), got %v", err)
	}
}

func TestAllowedAlgorithms_ExactlySixEntries(t *testing.T) {
	if len(auth.AllowedAlgorithms) != 6 {
		t.Fatalf("AllowedAlgorithms: expected 6, got %d (%v)", len(auth.AllowedAlgorithms), auth.AllowedAlgorithms)
	}
	want := map[string]bool{
		"RS256": false, "RS384": false, "RS512": false,
		"ES256": false, "ES384": false, "ES512": false,
	}
	for _, a := range auth.AllowedAlgorithms {
		if _, ok := want[a]; !ok {
			t.Errorf("unexpected algorithm in allowlist: %q", a)
		}
		want[a] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing algorithm from allowlist: %q", k)
		}
	}
}

// Sanity check that the test fixtures actually produced a valid keypair
// that signs with the documented algorithm. Catches regenerate-time
// mistakes early.
func TestTestData_KeypairsValid(t *testing.T) {
	rsaPriv, rsaPub := loadTestRS256(t)
	if rsaPriv.PublicKey.N.Cmp(rsaPub.N) != 0 {
		t.Errorf("RS256 testdata keypair mismatched")
	}
	ecPriv, ecPub := loadTestES256(t)
	if ecPriv.PublicKey.X.Cmp(ecPub.X) != 0 {
		t.Errorf("ES256 testdata keypair mismatched")
	}
	if ecPub.Curve != elliptic.P256() {
		t.Errorf("ES256 testdata curve: got %v, want P256", ecPub.Curve)
	}
}
