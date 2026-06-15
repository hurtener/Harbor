package auth_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

const (
	fixtureRSAKID = "test-rsa-1"
	fixtureECKID  = "test-ec-1"
)

// --- fixtures ----------------------------------------------------------

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

func loadRSAPriv(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	raw := loadFixture(t, "rs256_private.pem")
	blk, _ := pem.Decode(raw)
	if blk == nil {
		t.Fatal("no PEM block in rs256_private.pem")
	}
	// testdata uses PKCS#1 RSA private key.
	key, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
	if err != nil {
		// fall back to PKCS#8
		k2, err2 := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err2 != nil {
			t.Fatalf("parse rsa private: %v / %v", err, err2)
		}
		return k2.(*rsa.PrivateKey)
	}
	return key
}

func loadECPriv(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	raw := loadFixture(t, "es256_private.pem")
	blk, _ := pem.Decode(raw)
	if blk == nil {
		t.Fatal("no PEM block in es256_private.pem")
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		k2, err2 := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err2 != nil {
			t.Fatalf("parse ec private: %v / %v", err, err2)
		}
		return k2.(*ecdsa.PrivateKey)
	}
	return key
}

func signToken(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func validIdentityClaims(now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://issuer.example.com",
		"sub":     "user-12345",
		"aud":     "harbor",
		"exp":     now.Add(time.Hour).Unix(),
		"nbf":     now.Add(-time.Minute).Unix(),
		"tenant":  "tenant-acme",
		"user":    "user-12345",
		"session": "sess-1",
		"scopes":  []string{"admin"},
	}
}

// countingTransport serves a (possibly rotating) JWK Set body and counts
// the number of fetches — the test seam that proves refresh bounding.
type countingTransport struct {
	mu     sync.Mutex
	count  int
	status int
	body   func() []byte
}

func (ct *countingTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	ct.mu.Lock()
	ct.count++
	st := ct.status
	if st == 0 {
		st = http.StatusOK
	}
	b := ct.body()
	ct.mu.Unlock()
	return &http.Response{
		StatusCode: st,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     make(http.Header),
	}, nil
}

func (ct *countingTransport) fetches() int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.count
}

// --- parsing -----------------------------------------------------------

func TestJWKSKeySet_ResolvesRSAAndEC_FromRealFixture(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "jwks.json")
	ks, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: path}, nil)
	if err != nil {
		t.Fatalf("NewJWKSKeySet: %v", err)
	}

	rsaKey, rsaAlg, err := ks.KeyByID(fixtureRSAKID)
	if err != nil {
		t.Fatalf("KeyByID(rsa): %v", err)
	}
	if _, ok := rsaKey.(*rsa.PublicKey); !ok {
		t.Fatalf("rsa key type = %T, want *rsa.PublicKey", rsaKey)
	}
	if rsaAlg != "RS256" {
		t.Fatalf("rsa alg = %q, want RS256", rsaAlg)
	}

	ecKey, ecAlg, err := ks.KeyByID(fixtureECKID)
	if err != nil {
		t.Fatalf("KeyByID(ec): %v", err)
	}
	if _, ok := ecKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("ec key type = %T, want *ecdsa.PublicKey", ecKey)
	}
	if ecAlg != "ES256" {
		t.Fatalf("ec alg = %q, want ES256", ecAlg)
	}
}

func TestJWKSKeySet_RejectsSymmetricOctKey(t *testing.T) {
	t.Parallel()
	oct := []byte(`{"keys":[{"kty":"oct","kid":"sym-1","alg":"HS256","k":"c2VjcmV0"}]}`)
	dir := t.TempDir()
	p := filepath.Join(dir, "oct.json")
	if err := os.WriteFile(p, oct, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: p}, nil)
	if !errors.Is(err, auth.ErrJWKSNoUsableKeys) {
		t.Fatalf("err = %v, want ErrJWKSNoUsableKeys", err)
	}
}

func TestJWKSKeySet_RejectsMalformedJWK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Malformed JSON entirely.
	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: badJSON}, nil); err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}

	// A single RSA key with non-base64url n → dropped → zero usable.
	badRSA := filepath.Join(dir, "badrsa.json")
	if err := os.WriteFile(badRSA, []byte(`{"keys":[{"kty":"RSA","kid":"r","alg":"RS256","n":"!!notb64!!","e":"AQAB"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: badRSA}, nil); !errors.Is(err, auth.ErrJWKSNoUsableKeys) {
		t.Fatalf("err = %v, want ErrJWKSNoUsableKeys", err)
	}
}

// TestJWKSKeySet_RejectsUndersizedRSAKey pins the RSA-modulus floor: a
// real but weak (1024-bit) RSA key is dropped, so a set containing only
// it is unusable. Defense in depth against a weak/compromised IdP key.
func TestJWKSKeySet_RejectsUndersizedRSAKey(t *testing.T) {
	t.Parallel()
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	n := enc.EncodeToString(priv.N.Bytes())
	e := enc.EncodeToString(big.NewInt(int64(priv.E)).Bytes())
	jwk := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"weak","alg":"RS256","n":%q,"e":%q}]}`, n, e)
	p := filepath.Join(t.TempDir(), "weak.json")
	if err := os.WriteFile(p, []byte(jwk), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: p}, nil); !errors.Is(err, auth.ErrJWKSNoUsableKeys) {
		t.Fatalf("err = %v, want ErrJWKSNoUsableKeys (undersized RSA dropped)", err)
	}
}

func TestJWKSKeySet_AlgOutsideAllowlistRejected(t *testing.T) {
	t.Parallel()
	// The fixture is RS256 + ES256. Restrict the keyset to ES256 only;
	// the RSA key must drop, the EC key must remain.
	path := filepath.Join("testdata", "jwks.json")
	ks, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: path}, []string{"ES256"})
	if err != nil {
		t.Fatalf("NewJWKSKeySet: %v", err)
	}
	if _, _, err := ks.KeyByID(fixtureECKID); err != nil {
		t.Fatalf("ec key should remain: %v", err)
	}
	if _, _, err := ks.KeyByID(fixtureRSAKID); !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("rsa key should have been dropped (alg outside allowlist): %v", err)
	}
}

func TestNewJWKSKeySet_SourceMisconfigured(t *testing.T) {
	t.Parallel()
	cases := []auth.JWKSSource{
		{},                    // neither
		{URL: "x", File: "y"}, // both
	}
	for _, src := range cases {
		if _, err := auth.NewJWKSKeySet(context.Background(), src, nil); !errors.Is(err, auth.ErrJWKSSource) {
			t.Fatalf("src=%+v err=%v, want ErrJWKSSource", src, err)
		}
	}
}

// --- refresh + bounding ------------------------------------------------

func TestJWKSKeySet_UnknownKid_BoundedRefresh_ReturnsUnknownKey(t *testing.T) {
	t.Parallel()
	body := loadFixture(t, "jwks.json")
	ct := &countingTransport{body: func() []byte { return body }}
	client := &http.Client{Transport: ct}

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	ks, err := auth.NewJWKSKeySet(context.Background(),
		auth.JWKSSource{URL: "http://jwks.test/keys"}, nil,
		auth.WithJWKSHTTPClient(client),
		auth.WithJWKSClock(clk.now),
		auth.WithJWKSMinRefreshInterval(time.Minute),
		auth.WithJWKSRefreshTTL(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("NewJWKSKeySet: %v", err)
	}
	if ct.fetches() != 1 {
		t.Fatalf("after construction fetches=%d, want 1", ct.fetches())
	}

	// Unknown kid inside the bound window: no extra fetch, ErrUnknownKey.
	for range 20 {
		if _, _, err := ks.KeyByID("nope"); !errors.Is(err, auth.ErrUnknownKey) {
			t.Fatalf("unknown kid err=%v, want ErrUnknownKey", err)
		}
	}
	if got := ct.fetches(); got != 1 {
		t.Fatalf("fetches after 20 bounded misses = %d, want 1 (rate-limited)", got)
	}

	// Advance past the bound → exactly one refresh fetch on the next miss.
	clk.advance(2 * time.Minute)
	if _, _, err := ks.KeyByID("still-nope"); !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("err=%v, want ErrUnknownKey", err)
	}
	if got := ct.fetches(); got != 2 {
		t.Fatalf("fetches after bound elapsed = %d, want 2", got)
	}

	// Many further misses within the new window → still bounded.
	for range 20 {
		_, _, _ = ks.KeyByID("nope-again")
	}
	if got := ct.fetches(); got != 2 {
		t.Fatalf("fetches after second window misses = %d, want 2 (rate-limited)", got)
	}
}

func TestJWKSKeySet_TTLRefresh_PicksUpRotatedKey(t *testing.T) {
	t.Parallel()
	// Initially serve a set with only the EC key (kid test-ec-1 dropped:
	// instead serve a synthetic single-key set), then after TTL rotate to
	// the full fixture so the RSA kid resolves.
	full := loadFixture(t, "jwks.json")
	ecOnly := singleKeyFixture(t, full, fixtureECKID)

	var rotated atomic.Bool
	ct := &countingTransport{body: func() []byte {
		if rotated.Load() {
			return full
		}
		return ecOnly
	}}
	client := &http.Client{Transport: ct}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}

	ks, err := auth.NewJWKSKeySet(context.Background(),
		auth.JWKSSource{URL: "http://jwks.test/keys"}, nil,
		auth.WithJWKSHTTPClient(client),
		auth.WithJWKSClock(clk.now),
		auth.WithJWKSMinRefreshInterval(time.Minute),
		auth.WithJWKSRefreshTTL(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("NewJWKSKeySet: %v", err)
	}
	// RSA kid absent in the ec-only set.
	if _, _, err := ks.KeyByID(fixtureRSAKID); !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("rsa kid err=%v, want ErrUnknownKey", err)
	}

	// Rotate the IdP body and advance past the TTL → next lookup refreshes.
	rotated.Store(true)
	clk.advance(6 * time.Minute)
	if _, alg, err := ks.KeyByID(fixtureRSAKID); err != nil || alg != "RS256" {
		t.Fatalf("after rotation+TTL: alg=%q err=%v, want RS256/nil", alg, err)
	}
}

// --- validator end-to-end ---------------------------------------------

func TestJWKSValidator_E2E_VerifiesRSAAndEC(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "jwks.json")
	ks, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: path}, nil)
	if err != nil {
		t.Fatalf("NewJWKSKeySet: %v", err)
	}
	v, err := auth.NewValidator(ks,
		withTestRedactor(),
		auth.WithIssuer("https://issuer.example.com"),
		auth.WithAudience("harbor"),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	now := time.Now()
	rsaTok := signToken(t, jwt.SigningMethodRS256, loadRSAPriv(t), fixtureRSAKID, validIdentityClaims(now))
	res, err := v.Validate(context.Background(), rsaTok)
	if err != nil {
		t.Fatalf("validate rsa: %v", err)
	}
	if res.Identity.TenantID != "tenant-acme" || res.Identity.SessionID != "sess-1" {
		t.Fatalf("identity = %+v", res.Identity)
	}
	if len(res.Scopes) != 1 || res.Scopes[0] != "admin" {
		t.Fatalf("scopes = %v", res.Scopes)
	}

	ecTok := signToken(t, jwt.SigningMethodES256, loadECPriv(t), fixtureECKID, validIdentityClaims(now))
	if _, err := v.Validate(context.Background(), ecTok); err != nil {
		t.Fatalf("validate ec: %v", err)
	}
}

func TestJWKSValidator_RejectsForeignKid(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "jwks.json")
	ks, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: path}, nil)
	if err != nil {
		t.Fatalf("NewJWKSKeySet: %v", err)
	}
	v, err := auth.NewValidator(ks, withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	tok := signToken(t, jwt.SigningMethodRS256, loadRSAPriv(t), "foreign-kid", validIdentityClaims(time.Now()))
	if _, err := v.Validate(context.Background(), tok); !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestJWKSValidator_RejectsHS256AndNone(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "jwks.json")
	ks, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: path}, nil)
	if err != nil {
		t.Fatalf("NewJWKSKeySet: %v", err)
	}
	v, err := auth.NewValidator(ks, withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// HS256 — signed with a symmetric secret, kid pointing at the RSA key.
	hsTok := signToken(t, jwt.SigningMethodHS256, []byte("super-secret-hmac-key"), fixtureRSAKID, validIdentityClaims(time.Now()))
	if _, err := v.Validate(context.Background(), hsTok); !errors.Is(err, auth.ErrAlgNotAllowed) {
		t.Fatalf("HS256 err = %v, want ErrAlgNotAllowed", err)
	}

	// alg: none — unsigned token.
	noneTok := signToken(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, fixtureRSAKID, validIdentityClaims(time.Now()))
	if _, err := v.Validate(context.Background(), noneTok); !errors.Is(err, auth.ErrAlgNotAllowed) {
		t.Fatalf("none err = %v, want ErrAlgNotAllowed", err)
	}
}

// --- FromConfig projection --------------------------------------------

func TestNewJWKSValidator_FromConfig(t *testing.T) {
	t.Parallel()
	cfg := config.IdentityConfig{
		JWTAlgorithms: []string{"RS256", "ES256"},
		Issuer:        "https://issuer.example.com",
		Audience:      "harbor",
		JWKSFile:      filepath.Join("testdata", "jwks.json"),
	}
	// Pass a Logger so the projection threads WithJWKSLogger into the
	// keyset (the production wiring path).
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	v, err := auth.NewJWKSValidator(context.Background(), cfg, auth.ValidatorDeps{Redactor: testNoopRedactor{}, Logger: logger})
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}
	tok := signToken(t, jwt.SigningMethodRS256, loadRSAPriv(t), fixtureRSAKID, validIdentityClaims(time.Now()))
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Missing redactor → fail closed.
	if _, err := auth.NewJWKSValidator(context.Background(), cfg, auth.ValidatorDeps{}); !errors.Is(err, auth.ErrMisconfigured) {
		t.Fatalf("missing redactor err = %v, want ErrMisconfigured", err)
	}

	// Bad source (neither URL nor file) → fail closed.
	bad := cfg
	bad.JWKSFile = ""
	if _, err := auth.NewJWKSValidator(context.Background(), bad, auth.ValidatorDeps{Redactor: testNoopRedactor{}}); !errors.Is(err, auth.ErrJWKSSource) {
		t.Fatalf("bad source err = %v, want ErrJWKSSource", err)
	}
}

// --- concurrent reuse (D-025) -----------------------------------------

func TestJWKSKeySet_ConcurrentReuse_NoRaceNoLeak(t *testing.T) {
	path := filepath.Join("testdata", "jwks.json")
	ks, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{File: path}, nil,
		auth.WithJWKSMinRefreshInterval(time.Hour)) // keep misses bounded so no file re-reads
	if err != nil {
		t.Fatalf("NewJWKSKeySet: %v", err)
	}
	v, err := auth.NewValidator(ks, withTestRedactor(),
		auth.WithIssuer("https://issuer.example.com"), auth.WithAudience("harbor"))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	rsaTok := signToken(t, jwt.SigningMethodRS256, loadRSAPriv(t), fixtureRSAKID, validIdentityClaims(time.Now()))
	ecTok := signToken(t, jwt.SigningMethodES256, loadECPriv(t), fixtureECKID, validIdentityClaims(time.Now()))

	baseline := goruntime.NumGoroutine()

	const workers = 150
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			for range 10 {
				switch i % 3 {
				case 0:
					if _, _, err := ks.KeyByID(fixtureRSAKID); err != nil {
						t.Errorf("KeyByID rsa: %v", err)
						return
					}
				case 1:
					if _, err := v.Validate(context.Background(), rsaTok); err != nil {
						t.Errorf("validate rsa: %v", err)
						return
					}
				default:
					if _, err := v.Validate(context.Background(), ecTok); err != nil {
						t.Errorf("validate ec: %v", err)
						return
					}
				}
				// Sprinkle bounded unknown-kid misses to exercise the
				// refresh lock under contention (rate-limited → no I/O).
				_, _, _ = ks.KeyByID("unknown")
			}
		}(i)
	}
	wg.Wait()

	// Allow scheduler to retire any transient goroutines.
	for range 50 {
		if goruntime.NumGoroutine() <= baseline+2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := goruntime.NumGoroutine(); got > baseline+5 {
		t.Fatalf("goroutine leak: baseline=%d after=%d", baseline, got)
	}
}

// --- env-gated live probe (§17.8) -------------------------------------

func TestJWKSKeySet_LiveURL(t *testing.T) {
	url := os.Getenv("HARBOR_LIVE_JWKS_URL")
	if url == "" {
		t.Skip("reason: set HARBOR_LIVE_JWKS_URL to probe a real JWKS endpoint")
	}
	ks, err := auth.NewJWKSKeySet(context.Background(), auth.JWKSSource{URL: url}, nil)
	if err != nil {
		t.Fatalf("live JWKS fetch/parse: %v", err)
	}
	// A real endpoint must yield at least one resolvable key; we cannot
	// know its kid, so just confirm an obviously-absent kid is rejected
	// cleanly (the construction above already proved parse succeeded).
	if _, _, err := ks.KeyByID("definitely-not-a-real-kid"); !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("live unknown kid err = %v, want ErrUnknownKey", err)
	}
}

// --- helpers -----------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// singleKeyFixture returns a JWK Set JSON containing only the entry whose
// kid matches want, derived from the real multi-key fixture (not hand
// authored).
func singleKeyFixture(t *testing.T, full []byte, want string) []byte {
	t.Helper()
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(full, &set); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	var kept []map[string]any
	for _, k := range set.Keys {
		if k["kid"] == want {
			kept = append(kept, k)
		}
	}
	if len(kept) == 0 {
		t.Fatalf("kid %q not in fixture", want)
	}
	out, err := json.Marshal(map[string]any{"keys": kept})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
