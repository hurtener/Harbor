package auth_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// TestNewJWKSValidator_MaxStale_ExplicitValueHonored asserts the
// projection threads cfg.JWKSMaxStale straight through to the keyset: a
// validated positive ceiling (2m) is honored verbatim. The token has a
// far-future exp so the only failing axis is JWKS staleness.
func TestNewJWKSValidator_MaxStale_ExplicitValueHonored(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Now()}
	tr := &staleFetchTransport{body: loadFixture(t, "jwks.json")}

	cfg := config.IdentityConfig{
		JWTAlgorithms: []string{"RS256", "ES256"},
		Issuer:        "https://issuer.example.com",
		Audience:      "harbor",
		JWKSURL:       "http://jwks.test/keys",
		JWKSMaxStale:  2 * time.Minute,
	}
	v, err := auth.NewJWKSValidator(context.Background(), cfg,
		auth.ValidatorDeps{Redactor: testNoopRedactor{}},
		auth.WithJWKSHTTPClient(&http.Client{Transport: tr}),
		auth.WithJWKSClock(clk.now),
		auth.WithJWKSMinRefreshInterval(time.Minute),
		auth.WithJWKSRefreshTTL(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	claims := validIdentityClaims(clk.now())
	claims["exp"] = clk.now().Add(24 * time.Hour).Unix()
	claims["nbf"] = clk.now().Add(-time.Minute).Unix()
	tok := signToken(t, jwt.SigningMethodRS256, loadRSAPriv(t), fixtureRSAKID, claims)

	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("fresh Validate: %v", err)
	}

	tr.setFail(true)
	clk.advance(3 * time.Minute) // past the explicit 2m ceiling
	if _, err := v.Validate(context.Background(), tok); !errors.Is(err, auth.ErrJWKSStale) {
		t.Fatalf("past explicit ceiling Validate err = %v, want ErrJWKSStale", err)
	}
}

// TestNewJWKSValidator_MaxStale_ZeroAppliesDefault asserts an unset
// config field (0) reaches the option, which applies the single package
// default — the ceiling is never disabled. Within the default the cache
// is served; past it (61m) the validator fails closed.
func TestNewJWKSValidator_MaxStale_ZeroAppliesDefault(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Now()}
	tr := &staleFetchTransport{body: loadFixture(t, "jwks.json")}

	cfg := config.IdentityConfig{
		JWTAlgorithms: []string{"RS256", "ES256"},
		Issuer:        "https://issuer.example.com",
		Audience:      "harbor",
		JWKSURL:       "http://jwks.test/keys",
		// JWKSMaxStale unset (0) ⇒ the option applies the safe default.
	}
	v, err := auth.NewJWKSValidator(context.Background(), cfg,
		auth.ValidatorDeps{Redactor: testNoopRedactor{}},
		auth.WithJWKSHTTPClient(&http.Client{Transport: tr}),
		auth.WithJWKSClock(clk.now),
		auth.WithJWKSMinRefreshInterval(time.Minute),
		auth.WithJWKSRefreshTTL(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	claims := validIdentityClaims(clk.now())
	claims["exp"] = clk.now().Add(72 * time.Hour).Unix()
	claims["nbf"] = clk.now().Add(-time.Minute).Unix()
	tok := signToken(t, jwt.SigningMethodRS256, loadRSAPriv(t), fixtureRSAKID, claims)

	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("fresh Validate: %v", err)
	}

	// Within the default (1h) but past the TTL with the IdP down: served.
	tr.setFail(true)
	clk.advance(30 * time.Minute)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("within default ceiling Validate err = %v, want nil", err)
	}

	// Past the default ceiling: fail closed.
	clk.advance(31 * time.Minute) // 61m total ≥ 1h default
	if _, err := v.Validate(context.Background(), tok); !errors.Is(err, auth.ErrJWKSStale) {
		t.Fatalf("past default ceiling Validate err = %v, want ErrJWKSStale", err)
	}
}
