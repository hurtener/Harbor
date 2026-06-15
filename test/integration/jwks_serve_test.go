package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// TestE2E_JWKSVerifiedRequest_ReachesSurfaceWithIdentity wires the
// production JWKS-backed validator (real keyset from a file source, real
// audit redactor, real in-memory event bus) behind the auth.Middleware
// HTTP boundary and proves that a request bearing a JWT signed by the
// IdP's private key flows through the wire transport to a downstream
// surface carrying the verified (tenant, user, session) identity + scopes
// on ctx. It also exercises the failure modes (foreign kid, HS256) and a
// concurrency stress run (no identity bleed across N concurrent
// requests).
//
// Real drivers across the seam, identity propagation, ≥1 failure mode,
// concurrency stress — the §17 integration-test contract.
func TestE2E_JWKSVerifiedRequest_ReachesSurfaceWithIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		issuer   = "https://idp.example.com"
		audience = "harbor-runtime"
		kid      = "serve-test-rsa-1"
	)

	// An IdP keypair: the private key signs tokens; its public half is
	// published as a JWK Set the Runtime fetches. The JWK is encoded
	// independently of the parser-under-test (math/big + base64url).
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa: %v", err)
	}
	jwksPath := writeRSAJWKS(t, &priv.PublicKey, kid)

	red := patterns.New()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	validator, err := auth.NewJWKSValidator(ctx, config.IdentityConfig{
		JWTAlgorithms: []string{"RS256"},
		Issuer:        issuer,
		Audience:      audience,
		JWKSFile:      jwksPath,
	}, auth.ValidatorDeps{Redactor: red, Bus: bus})
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	// The downstream surface: reads the verified identity + scopes from
	// ctx (exactly what every real Protocol surface does) and echoes them.
	type echo struct {
		Tenant  string   `json:"tenant"`
		User    string   `json:"user"`
		Session string   `json:"session"`
		Scopes  []string `json:"scopes"`
	}
	surface := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := identity.From(r.Context())
		if !ok {
			http.Error(w, "no identity on ctx", http.StatusInternalServerError)
			return
		}
		var scopes []string
		if sc, ok := auth.ScopesFrom(r.Context()); ok {
			for _, s := range sc {
				scopes = append(scopes, string(s))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(echo{
			Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Scopes: scopes,
		})
	})

	srv := httptest.NewServer(auth.Middleware(validator)(surface))
	t.Cleanup(srv.Close)

	signRS256 := func(claims jwt.MapClaims, useKID string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = useKID
		s, serr := tok.SignedString(priv)
		if serr != nil {
			t.Fatalf("sign: %v", serr)
		}
		return s
	}
	baseClaims := func() jwt.MapClaims {
		now := time.Now()
		return jwt.MapClaims{
			"iss": issuer, "aud": audience, "sub": "user-1",
			"exp":     now.Add(time.Hour).Unix(),
			"nbf":     now.Add(-time.Minute).Unix(),
			"tenant":  "tenant-acme",
			"user":    "user-1",
			"session": "sess-default",
			"scopes":  []string{"admin", "console:fleet"},
		}
	}

	get := func(t *testing.T, token, sessionHeader string) (int, echo) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if sessionHeader != "" {
			req.Header.Set("X-Harbor-Session", sessionHeader)
		}
		resp, derr := srv.Client().Do(req)
		if derr != nil {
			t.Fatalf("request: %v", derr)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var e echo
		if resp.StatusCode == http.StatusOK {
			if uerr := json.Unmarshal(body, &e); uerr != nil {
				t.Fatalf("decode body %q: %v", string(body), uerr)
			}
		}
		return resp.StatusCode, e
	}

	// Happy path — verified identity + scopes reach the surface.
	t.Run("verified_identity_reaches_surface", func(t *testing.T) {
		status, e := get(t, signRS256(baseClaims(), kid), "")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if e.Tenant != "tenant-acme" || e.User != "user-1" || e.Session != "sess-default" {
			t.Fatalf("identity = %+v", e)
		}
		if len(e.Scopes) != 2 {
			t.Fatalf("scopes = %v, want 2", e.Scopes)
		}
	})

	// Failure mode 1 — a token whose kid is not in the JWK set is rejected.
	t.Run("foreign_kid_rejected", func(t *testing.T) {
		status, _ := get(t, signRS256(baseClaims(), "not-the-kid"), "")
		if status != http.StatusUnauthorized {
			t.Fatalf("foreign kid status = %d, want 401", status)
		}
	})

	// Failure mode 2 — an HS256 token (algorithm-confusion shape) is
	// rejected at the parser allowlist before the keyfunc runs.
	t.Run("hs256_rejected", func(t *testing.T) {
		hsTok := jwt.NewWithClaims(jwt.SigningMethodHS256, baseClaims())
		hsTok.Header["kid"] = kid
		signed, herr := hsTok.SignedString([]byte("symmetric-secret"))
		if herr != nil {
			t.Fatalf("sign hs256: %v", herr)
		}
		status, _ := get(t, signed, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("hs256 status = %d, want 401", status)
		}
	})

	// Failure mode 3 — no token at all.
	t.Run("missing_token_rejected", func(t *testing.T) {
		status, _ := get(t, "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("missing token status = %d, want 401", status)
		}
	})

	// Concurrency stress — N concurrent requests, each pinning a distinct
	// session via X-Harbor-Session, must each see ITS OWN session (no
	// cross-request identity bleed). The (tenant, user) stay
	// token-verified; only the session is per-request.
	t.Run("concurrent_no_identity_bleed", func(t *testing.T) {
		const workers = 32
		var wg sync.WaitGroup
		wg.Add(workers)
		errs := make(chan error, workers)
		for i := range workers {
			go func(i int) {
				defer wg.Done()
				want := fmt.Sprintf("sess-%03d", i)
				token := signRS256(baseClaims(), kid)
				status, e := get(t, token, want)
				if status != http.StatusOK {
					errs <- fmt.Errorf("worker %d status %d", i, status)
					return
				}
				if e.Session != want {
					errs <- fmt.Errorf("worker %d session=%q want %q (bleed)", i, e.Session, want)
					return
				}
				if e.Tenant != "tenant-acme" || e.User != "user-1" {
					errs <- fmt.Errorf("worker %d identity widened: %+v", i, e)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})
}

// writeRSAJWKS encodes pub as a single-key JWK Set JSON and writes it to
// a temp file, returning the path. The base64url n/e encoding is done
// directly (math/big) so the fixture is produced independently of the
// JWKS parser under test.
func writeRSAJWKS(t *testing.T, pub *rsa.PublicKey, kid string) string {
	t.Helper()
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	set := map[string]any{
		"keys": []any{
			map[string]any{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
				"n": b64(pub.N.Bytes()),
				"e": b64(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	return path
}
