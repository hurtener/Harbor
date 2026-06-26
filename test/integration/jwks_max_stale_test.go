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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// maxStaleClock is a controllable clock for the JWKS keyset. It is
// anchored at real time.Now() so the Validator's (real-time) exp/nbf
// checks pass with far-future token claims, while the keyset's notion of
// "now" is advanced independently to drive snapshot staleness — no
// time.Sleep (CLAUDE.md §11).
type maxStaleClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *maxStaleClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *maxStaleClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestE2E_JWKSMaxStale wires the production JWKS-backed validator (real
// audit redactor, real in-memory event bus) behind the auth.Middleware
// HTTP boundary over a real JWKS endpoint, and proves the max-stale
// ceiling end-to-end: a valid token succeeds while the snapshot is
// fresh; once the JWKS endpoint goes unreachable and the keyset clock
// advances past the ceiling, the SAME request is rejected 401 with the
// distinct wire reason `jwks_stale`; once the endpoint recovers the
// request succeeds again. Identity propagation is asserted on the
// success path; the stale rejection is the failure mode. Runs under
// -race in CI.
func TestE2E_JWKSMaxStale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		issuer   = "https://idp.example.com"
		audience = "harbor-runtime"
		kid      = "maxstale-rsa-1"
	)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa: %v", err)
	}

	// A real JWKS endpoint with a flip-to-fail switch (simulates the IdP
	// going unreachable: a 503 the keyset treats as a refresh failure).
	var down atomic.Bool
	jwksBody := encodeRSAJWKS(t, &priv.PublicKey, kid)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			http.Error(w, "idp unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBody)
	}))
	t.Cleanup(jwks.Close)

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

	clk := &maxStaleClock{t: time.Now()}
	validator, err := auth.NewJWKSValidator(ctx, config.IdentityConfig{
		JWTAlgorithms: []string{"RS256"},
		Issuer:        issuer,
		Audience:      audience,
		JWKSURL:       jwks.URL,
		JWKSMaxStale:  2 * time.Minute,
	}, auth.ValidatorDeps{Redactor: red, Bus: bus},
		auth.WithJWKSClock(clk.now),
		auth.WithJWKSMinRefreshInterval(time.Minute),
		auth.WithJWKSRefreshTTL(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

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
		_ = json.NewEncoder(w).Encode(echo{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Scopes: scopes})
	})
	srv := httptest.NewServer(auth.Middleware(validator)(surface))
	t.Cleanup(srv.Close)

	token := func() string {
		now := time.Now()
		claims := jwt.MapClaims{
			"iss": issuer, "aud": audience, "sub": "user-1",
			"exp":     now.Add(72 * time.Hour).Unix(),
			"nbf":     now.Add(-time.Minute).Unix(),
			"tenant":  "tenant-acme",
			"user":    "user-1",
			"session": "sess-default",
			"scopes":  []string{"admin"},
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid
		s, serr := tok.SignedString(priv)
		if serr != nil {
			t.Fatalf("sign: %v", serr)
		}
		return s
	}

	do := func(t *testing.T, tok string) (int, echo, *protoerrors.Error) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, derr := srv.Client().Do(req)
		if derr != nil {
			t.Fatalf("request: %v", derr)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusOK {
			var e echo
			if uerr := json.Unmarshal(body, &e); uerr != nil {
				t.Fatalf("decode echo %q: %v", string(body), uerr)
			}
			return resp.StatusCode, e, nil
		}
		var perr protoerrors.Error
		_ = json.Unmarshal(body, &perr)
		return resp.StatusCode, echo{}, &perr
	}

	// Fresh — verified identity reaches the surface.
	t.Run("fresh_succeeds_with_identity", func(t *testing.T) {
		status, e, _ := do(t, token())
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if e.Tenant != "tenant-acme" || e.User != "user-1" || e.Session != "sess-default" {
			t.Fatalf("identity = %+v", e)
		}
	})

	// Concurrency stress on the fresh path — N≥10 concurrent requests, no
	// identity bleed.
	t.Run("concurrent_fresh_no_bleed", func(t *testing.T) {
		const workers = 16
		var wg sync.WaitGroup
		wg.Add(workers)
		errs := make(chan error, workers)
		for i := range workers {
			go func(i int) {
				defer wg.Done()
				status, e, _ := do(t, token())
				if status != http.StatusOK {
					errs <- fmt.Errorf("worker %d status %d", i, status)
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

	// IdP goes down; advance the keyset clock past the 2m ceiling → the
	// same request is rejected 401 with the distinct jwks_stale reason.
	t.Run("stale_rejected_jwks_stale", func(t *testing.T) {
		down.Store(true)
		clk.advance(3 * time.Minute)
		status, _, perr := do(t, token())
		if status != http.StatusUnauthorized {
			t.Fatalf("stale status = %d, want 401", status)
		}
		if perr == nil || perr.Code != protoerrors.CodeAuthRejected {
			t.Fatalf("stale error = %+v, want code %q", perr, protoerrors.CodeAuthRejected)
		}
		if perr.Message == "" || !strings.Contains(perr.Message, "jwks_stale") {
			t.Fatalf("stale message = %q, want it to carry the jwks_stale reason", perr.Message)
		}
	})

	// IdP recovers; advance past the refresh window → staleness resets and
	// the same token verifies again.
	t.Run("recovered_succeeds", func(t *testing.T) {
		down.Store(false)
		clk.advance(2 * time.Minute)
		status, e, perr := do(t, token())
		if status != http.StatusOK {
			t.Fatalf("recovered status = %d (err %+v), want 200", status, perr)
		}
		if e.User != "user-1" {
			t.Fatalf("recovered identity = %+v", e)
		}
	})
}

// encodeRSAJWKS encodes pub as a single-key JWK Set JSON, produced
// independently of the JWKS parser under test (math/big + base64url).
func encodeRSAJWKS(t *testing.T, pub *rsa.PublicKey, kid string) []byte {
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
	return raw
}
