// cmd/harbor/cmd_token_test.go — `harbor token` keygen + mint tests,
// including the §17.8 real-parser round-trip: a keygen-produced JWK Set
// is loaded by the REAL production JWKS keyset + validator, and a
// mint-produced token is accepted iff its iss/aud match — proving the
// self-issued path is wire-compatible with `harbor serve`'s verifier
// rather than self-consistent against a hand fixture.

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// errorsIs is a tiny alias so the crypto error-branch tests read cleanly.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// Test issuer / audience the validator is configured to expect — the
// stand-in for an operator's identity.issuer / identity.audience.
const (
	testIssuer   = "https://issuer.example.com"
	testAudience = "harbor"
)

// realValidator builds the production JWKS-backed validator over a
// keygen-produced jwks.json file, configured with the expected issuer +
// audience — the exact construction `harbor serve` uses.
func realValidator(t *testing.T, jwksPath string) auth.Validator {
	t.Helper()
	ctx := context.Background()
	keys, err := auth.NewJWKSKeySet(ctx, auth.JWKSSource{File: jwksPath}, auth.AllowedAlgorithms)
	if err != nil {
		t.Fatalf("NewJWKSKeySet(file=%s): %v", jwksPath, err)
	}
	v, err := auth.NewValidator(keys,
		auth.WithIssuer(testIssuer),
		auth.WithAudience(testAudience),
		auth.WithRedactor(auditpatterns.New()),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

// keygenInto runs the keygen crypto path into dir and returns the
// jwks.json path + the private.pem path. Uses the real keygen helpers
// (generateSigningKey/writePrivatePEM/publicJWK/writeJWKSet).
func keygenInto(t *testing.T, dir, alg string) (jwksPath, privPath string) {
	t.Helper()
	if err := os.MkdirAll(dir, keyDirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	signer, err := generateSigningKey(alg)
	if err != nil {
		t.Fatalf("generateSigningKey(%s): %v", alg, err)
	}
	privPath = filepath.Join(dir, privateKeyFile)
	jwksPath = filepath.Join(dir, jwksSetFile)
	if err := writePrivatePEM(privPath, signer, false); err != nil {
		t.Fatalf("writePrivatePEM: %v", err)
	}
	jwk, _, err := publicJWK(signer, alg)
	if err != nil {
		t.Fatalf("publicJWK: %v", err)
	}
	if err := writeJWKSet(jwksPath, jwk); err != nil {
		t.Fatalf("writeJWKSet: %v", err)
	}
	return jwksPath, privPath
}

// mintWith loads privPath and mints a token for the supplied claim
// inputs, mirroring runTokenMint's signer/alg/kid derivation.
func mintWith(t *testing.T, privPath, issuer, audience string, scopes []string) string {
	t.Helper()
	signer, err := loadPrivatePEM(privPath)
	if err != nil {
		t.Fatalf("loadPrivatePEM: %v", err)
	}
	alg, err := algForKey(signer)
	if err != nil {
		t.Fatalf("algForKey: %v", err)
	}
	_, kid, err := publicJWK(signer, alg)
	if err != nil {
		t.Fatalf("publicJWK: %v", err)
	}
	claims := harborClaims(time.Now(), time.Hour, issuer, audience, "acme", "alice", "sess-1", scopes)
	signed, err := mintJWT(signer, alg, kid, claims)
	if err != nil {
		t.Fatalf("mintJWT: %v", err)
	}
	return signed
}

func TestTokenRoundTrip_RealParserAcceptsMatchingIssAud(t *testing.T) {
	for _, alg := range []string{algES256, algRS256} {
		t.Run(alg, func(t *testing.T) {
			dir := t.TempDir()
			jwksPath, privPath := keygenInto(t, dir, alg)
			token := mintWith(t, privPath, testIssuer, testAudience, nil)

			v := realValidator(t, jwksPath)
			got, err := v.Validate(context.Background(), token)
			if err != nil {
				t.Fatalf("real validator rejected a matching token: %v", err)
			}
			if got.Identity.TenantID != "acme" || got.Identity.UserID != "alice" || got.Identity.SessionID != "sess-1" {
				t.Fatalf("identity round-trip mismatch: %+v", got.Identity)
			}
			if got.Issuer != testIssuer {
				t.Fatalf("issuer = %q, want %q", got.Issuer, testIssuer)
			}
		})
	}
}

func TestTokenMint_AgentReach_ValidatesAndSignsBoundedClaim(t *testing.T) {
	dir := t.TempDir()
	jwksPath, privPath := keygenInto(t, dir, algES256)
	stdout, _, err := runRoot(t, []string{
		"token", "mint", "--key", privPath,
		"--tenant", "acme", "--user", "alice", "--session", "sess-1",
		"--issuer", testIssuer, "--audience", testAudience,
		"--agent-reach", "agent-a,agent-b",
	})
	if err != nil {
		t.Fatalf("token mint: %v", err)
	}
	verified, err := realValidator(t, jwksPath).Validate(context.Background(), strings.TrimSpace(stdout))
	if err != nil {
		t.Fatalf("validate minted token: %v", err)
	}
	if got, want := strings.Join(verified.AgentReach, ","), "agent-a,agent-b"; got != want {
		t.Fatalf("minted agent reach = %q, want %q", got, want)
	}
	_, _, err = runRoot(t, []string{
		"token", "mint", "--key", privPath,
		"--tenant", "acme", "--user", "alice", "--session", "sess-1",
		"--issuer", testIssuer, "--audience", testAudience,
		"--agent-reach", "agent-a,agent-a",
	})
	if err == nil {
		t.Fatal("duplicate --agent-reach minted a token, want failure")
	}
}

func TestTokenRoundTrip_RealParserRejectsIssuerMismatch(t *testing.T) {
	// iss/aud rejection happens in the validator's claim check, AFTER
	// signature verification succeeds, so it is algorithm-agnostic — ES256
	// alone exercises it (the accept test covers both ES256 and RS256).
	dir := t.TempDir()
	jwksPath, privPath := keygenInto(t, dir, algES256)
	// Token minted with a DIFFERENT issuer than the validator expects.
	token := mintWith(t, privPath, "https://evil.example.com", "harbor", nil)

	v := realValidator(t, jwksPath)
	if _, err := v.Validate(context.Background(), token); err == nil {
		t.Fatal("real validator accepted a token with a mismatched issuer (would 401 at the edge)")
	}
}

func TestTokenRoundTrip_RealParserRejectsAudienceMismatch(t *testing.T) {
	dir := t.TempDir()
	jwksPath, privPath := keygenInto(t, dir, algES256)
	// Token minted with a DIFFERENT audience than the validator expects.
	token := mintWith(t, privPath, "https://issuer.example.com", "wrong-aud", nil)

	v := realValidator(t, jwksPath)
	if _, err := v.Validate(context.Background(), token); err == nil {
		t.Fatal("real validator accepted a token with a mismatched audience (would 401 at the edge)")
	}
}

func TestTokenMint_DefaultIsNonAdmin(t *testing.T) {
	dir := t.TempDir()
	jwksPath, privPath := keygenInto(t, dir, algES256)
	// Default mint: NO scopes passed.
	token := mintWith(t, privPath, testIssuer, testAudience, nil)

	v := realValidator(t, jwksPath)
	got, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(got.Scopes) != 0 {
		t.Fatalf("default mint carried scopes %v, want none (least privilege)", got.Scopes)
	}
	for _, s := range got.Scopes {
		if s == auth.ScopeAdmin {
			t.Fatal("default mint carried the admin scope")
		}
	}
}

func TestTokenMint_ScopesGrantedWhenRequested(t *testing.T) {
	dir := t.TempDir()
	jwksPath, privPath := keygenInto(t, dir, algES256)
	token := mintWith(t, privPath, testIssuer, testAudience, []string{"admin", "console:fleet"})

	v := realValidator(t, jwksPath)
	got, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	var hasAdmin bool
	for _, s := range got.Scopes {
		if s == auth.ScopeAdmin {
			hasAdmin = true
		}
	}
	if !hasAdmin {
		t.Fatalf("requested admin scope absent from verified scopes %v", got.Scopes)
	}
}

func TestTokenKeygen_PrivateKeyIs0600(t *testing.T) {
	dir := t.TempDir()
	_, privPath := keygenInto(t, dir, algES256)
	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != privatePEMMode {
		t.Fatalf("private.pem mode = %o, want %o", perm, privatePEMMode)
	}
}

func TestTokenKeygen_RefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	signer, err := generateSigningKey(algES256)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	p := filepath.Join(dir, privateKeyFile)
	if err := writePrivatePEM(p, signer, false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Second write without force must refuse.
	if err := writePrivatePEM(p, signer, false); err == nil {
		t.Fatal("writePrivatePEM overwrote an existing key without --force")
	}
	// With force it succeeds and stays 0600.
	if err := writePrivatePEM(p, signer, true); err != nil {
		t.Fatalf("forced write: %v", err)
	}
	info, _ := os.Stat(p)
	if perm := info.Mode().Perm(); perm != privatePEMMode {
		t.Fatalf("after --force, mode = %o, want %o", perm, privatePEMMode)
	}
}

func TestTokenThumbprint_IsDeterministicAndKidMatches(t *testing.T) {
	dir := t.TempDir()
	jwksPath, privPath := keygenInto(t, dir, algES256)
	// The kid the mint path derives must equal the kid keygen wrote into
	// jwks.json (so the verifier resolves the key by the token's kid).
	signer, err := loadPrivatePEM(privPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, mintKid, err := publicJWK(signer, algES256)
	if err != nil {
		t.Fatalf("publicJWK: %v", err)
	}
	raw, err := os.ReadFile(jwksPath)
	if err != nil {
		t.Fatalf("read jwks: %v", err)
	}
	// The kid is a base64url thumbprint, unique within the single-key
	// set — a substring check suffices.
	if mintKid == "" || !strings.Contains(string(raw), mintKid) {
		t.Fatalf("jwks.json does not carry the mint-derived kid %q", mintKid)
	}
}

// --- CLI-level tests: exercise the cobra RunE bodies via NewRootCmd ----------

func TestTokenKeygenCmd_WritesFilesAndJSON(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "keys")
	stdout, _, err := runRoot(t, []string{"token", "keygen", "--out", out, "--json"})
	if err != nil {
		t.Fatalf("keygen --json: %v", err)
	}
	if !strings.Contains(stdout, `"private_key"`) || !strings.Contains(stdout, `"kid"`) {
		t.Fatalf("keygen --json stdout missing fields: %q", stdout)
	}
	if _, statErr := os.Stat(filepath.Join(out, privateKeyFile)); statErr != nil {
		t.Fatalf("private.pem not written: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(out, jwksSetFile)); statErr != nil {
		t.Fatalf("jwks.json not written: %v", statErr)
	}
}

func TestTokenKeygenCmd_RS256AndHumanOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "keys")
	stdout, _, err := runRoot(t, []string{"token", "keygen", "--out", out, "--alg", "RS256"})
	if err != nil {
		t.Fatalf("keygen RS256: %v", err)
	}
	if !strings.Contains(stdout, "RS256") {
		t.Fatalf("human output missing alg: %q", stdout)
	}
}

func TestTokenKeygenCmd_RejectsUnknownAlg(t *testing.T) {
	out := filepath.Join(t.TempDir(), "keys")
	_, _, err := runRoot(t, []string{"token", "keygen", "--out", out, "--alg", "HS256"})
	if err == nil {
		t.Fatal("keygen accepted a non-asymmetric --alg")
	}
	cli, ok := asCLIError(err)
	if !ok || cli.Code != CodeTokenUnknownAlg {
		t.Fatalf("want CodeTokenUnknownAlg, got %+v", err)
	}
}

func TestTokenKeygenCmd_RequiresOut(t *testing.T) {
	_, _, err := runRoot(t, []string{"token", "keygen"})
	cli, ok := asCLIError(err)
	if !ok || cli.Code != CodeTokenMissingField {
		t.Fatalf("want CodeTokenMissingField for missing --out, got %+v", err)
	}
}

func TestTokenKeygenCmd_RefusesOverwriteCmd(t *testing.T) {
	out := filepath.Join(t.TempDir(), "keys")
	if _, _, err := runRoot(t, []string{"token", "keygen", "--out", out}); err != nil {
		t.Fatalf("first keygen: %v", err)
	}
	_, _, err := runRoot(t, []string{"token", "keygen", "--out", out})
	cli, ok := asCLIError(err)
	if !ok || cli.Code != CodeTokenKeyExists {
		t.Fatalf("want CodeTokenKeyExists, got %+v", err)
	}
	// --force succeeds.
	if _, _, err := runRoot(t, []string{"token", "keygen", "--out", out, "--force"}); err != nil {
		t.Fatalf("keygen --force: %v", err)
	}
}

func TestTokenMintCmd_RoundTripsThroughCLI(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "keys")
	if _, _, err := runRoot(t, []string{"token", "keygen", "--out", out}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	priv := filepath.Join(out, privateKeyFile)
	jwks := filepath.Join(out, jwksSetFile)

	stdout, _, err := runRoot(t, []string{
		"token", "mint", "--key", priv,
		"--tenant", "acme", "--user", "alice", "--session", "sess-1",
		"--issuer", testIssuer, "--audience", testAudience,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := strings.TrimSpace(stdout)

	v := realValidator(t, jwks)
	got, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("CLI-minted token rejected by real verifier: %v", err)
	}
	if got.Identity.TenantID != "acme" || len(got.Scopes) != 0 {
		t.Fatalf("unexpected verified claims: id=%+v scopes=%v", got.Identity, got.Scopes)
	}
}

func TestTokenMintCmd_JSONMode(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "keys")
	if _, _, err := runRoot(t, []string{"token", "keygen", "--out", out}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	stdout, _, err := runRoot(t, []string{
		"token", "mint", "--json", "--key", filepath.Join(out, privateKeyFile),
		"--tenant", "acme", "--user", "alice", "--session", "sess-1",
		"--issuer", testIssuer, "--audience", testAudience, "--scopes", "console:fleet",
	})
	if err != nil {
		t.Fatalf("mint --json: %v", err)
	}
	if !strings.Contains(stdout, `"token"`) || !strings.Contains(stdout, `"expires_at"`) {
		t.Fatalf("mint --json missing fields: %q", stdout)
	}
}

func TestTokenMintCmd_RequiresIssuerAndAudience(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "keys")
	if _, _, err := runRoot(t, []string{"token", "keygen", "--out", out}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	priv := filepath.Join(out, privateKeyFile)
	base := []string{"token", "mint", "--key", priv, "--tenant", "a", "--user", "b", "--session", "c"}

	// Missing --issuer.
	_, _, err := runRoot(t, append(append([]string{}, base...), "--audience", testAudience))
	if cli, ok := asCLIError(err); !ok || cli.Code != CodeTokenMissingField {
		t.Fatalf("missing --issuer: want CodeTokenMissingField, got %+v", err)
	}
	// Missing --audience.
	_, _, err = runRoot(t, append(append([]string{}, base...), "--issuer", testIssuer))
	if cli, ok := asCLIError(err); !ok || cli.Code != CodeTokenMissingField {
		t.Fatalf("missing --audience: want CodeTokenMissingField, got %+v", err)
	}
}

func TestTokenMintCmd_RejectsBadKeyAndBadTTL(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.pem")
	_, _, err := runRoot(t, []string{
		"token", "mint", "--key", missing,
		"--tenant", "a", "--user", "b", "--session", "c",
		"--issuer", testIssuer, "--audience", testAudience,
	})
	if cli, ok := asCLIError(err); !ok || cli.Code != CodeTokenKeyLoad {
		t.Fatalf("bad --key: want CodeTokenKeyLoad, got %+v", err)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "keys")
	if _, _, err := runRoot(t, []string{"token", "keygen", "--out", out}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	_, _, err = runRoot(t, []string{
		"token", "mint", "--key", filepath.Join(out, privateKeyFile),
		"--tenant", "a", "--user", "b", "--session", "c",
		"--issuer", testIssuer, "--audience", testAudience, "--ttl", "0",
	})
	if cli, ok := asCLIError(err); !ok || cli.Code != CodeTokenMissingField {
		t.Fatalf("--ttl 0: want CodeTokenMissingField, got %+v", err)
	}
}

// --- token_crypto.go error-branch + fallback coverage ------------------------

func TestGenerateSigningKey_RejectsUnknownAlg(t *testing.T) {
	if _, err := generateSigningKey("HS256"); !errorsIs(err, ErrTokenUnknownAlg) {
		t.Fatalf("want ErrTokenUnknownAlg, got %v", err)
	}
}

func TestAlgForKey_RejectsUnsupportedKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 gen: %v", err)
	}
	_ = pub
	if _, err := algForKey(priv); !errorsIs(err, ErrTokenUnknownAlg) {
		t.Fatalf("want ErrTokenUnknownAlg for ed25519, got %v", err)
	}
}

func TestSignMethodForAlg_RejectsUnknown(t *testing.T) {
	if _, err := signMethodForAlg("HS256"); !errorsIs(err, ErrTokenUnknownAlg) {
		t.Fatalf("want ErrTokenUnknownAlg, got %v", err)
	}
	// ES384/ES512 resolve (the non-default EC branches).
	for _, a := range []string{"ES384", "ES512", "RS256"} {
		if _, err := signMethodForAlg(a); err != nil {
			t.Fatalf("signMethodForAlg(%s): %v", a, err)
		}
	}
}

func TestLoadPrivatePEM_ErrorsAndSEC1Fallback(t *testing.T) {
	dir := t.TempDir()

	// Not a PEM file.
	notPEM := filepath.Join(dir, "not.pem")
	if err := os.WriteFile(notPEM, []byte("definitely not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPrivatePEM(notPEM); !errorsIs(err, ErrTokenKeyParse) {
		t.Fatalf("want ErrTokenKeyParse for non-PEM, got %v", err)
	}

	// Missing file.
	if _, err := loadPrivatePEM(filepath.Join(dir, "absent.pem")); err == nil {
		t.Fatal("want error for missing key file")
	}

	// SEC1-encoded EC key (the ParseECPrivateKey fallback).
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	sec1 := filepath.Join(dir, "sec1.pem")
	if err := os.WriteFile(sec1, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := loadPrivatePEM(sec1)
	if err != nil {
		t.Fatalf("SEC1 load: %v", err)
	}
	if alg, err := algForKey(signer); err != nil || alg != "ES256" {
		t.Fatalf("SEC1 key alg = %q, %v", alg, err)
	}
}

func TestCurveName_RejectsUnsupported(t *testing.T) {
	if _, err := curveName(elliptic.P224()); !errorsIs(err, ErrTokenUnknownAlg) {
		t.Fatalf("want ErrTokenUnknownAlg for P-224, got %v", err)
	}
}
