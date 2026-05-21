// cmd/harbor/devauth.go — `harbor dev` ephemeral JWT signing.
//
// Phase 64 (D-089) gives `harbor dev` a dev-only identity injection
// path: an ephemeral ES256 keypair generated at boot, a static
// `auth.KeySet` that resolves the boot kid → the generated public
// key, and a default-identity dev token printed to stderr at startup.
//
// The keypair lives in memory for the lifetime of the dev server;
// each boot mints a fresh pair so a leaked token from one run cannot
// be reused against a later run. Operators wiring a real OIDC
// provider for non-local deployments replace this surface entirely
// via `harbor.yaml`'s `identity.jwks_url` (the validator-side wiring
// for that lands in a later release-engineering phase). The dev key
// path is gated behind the `harbor dev` subcommand boundary —
// nothing else in the binary touches it.
//
// # Why ES256, not RS256
//
// ES256 (ECDSA P-256) keypairs are ~10x faster to generate than
// equivalent-strength RS256. `harbor dev` regenerates the key on
// every boot, so generation cost is a real factor for the dev loop's
// startup latency. Both algorithms are on the §7 allowlist; the
// choice between them is purely operational. The Phase 61 Validator
// allows both, so an operator who needs RS256 can wire it via a
// later jwks driver.
//
// # Security posture
//
// The dev key is in-memory only. It is never persisted to disk, never
// printed in cleartext, and never exposed via any Protocol endpoint.
// The matching default-identity dev token IS printed to stderr at
// startup so an operator can `curl -H "Authorization: Bearer ${TOKEN}"
// localhost:18080/v1/events` without writing JWT-signing code. The
// token's expiry is 24 hours by default and the identity triple is
// `(tenant=dev, user=dev, session=dev)` plus the `admin` scope so an
// operator can subscribe to fleet events. The banner makes it clear
// this surface is dev-only.

package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// DevTokenTTL is the dev-token expiry the dev cmd mints with.
const DevTokenTTL = 24 * time.Hour

// DevKID is the kid header stamped on dev tokens. Constant so an
// operator can correlate `kid=harbor-dev` in their request logs with
// the dev-key surface (vs. a real OIDC provider whose kid would
// resolve via JWKS).
const DevKID = "harbor-dev"

// DevTenant / DevUser / DevSession are the default identity triple
// the dev token carries. Operators submitting a request with this
// token are scoped to that triple — every Protocol call is filtered
// through the same multi-isolation seam every other production token
// is. The values are documented constants so they appear in test
// fixtures and never leak through silent defaults.
const (
	DevTenant  = "dev"
	DevUser    = "dev"
	DevSession = "dev"
)

// devKeySet is the auth.KeySet implementation `harbor dev` mounts.
// One kid → one (ecdsa.PublicKey, "ES256") entry. Immutable after
// construction; satisfies the D-025 concurrent-reuse contract by
// being read-only.
type devKeySet struct {
	kid string
	pub *ecdsa.PublicKey
}

// KeyByID implements auth.KeySet. Returns the dev public key when
// kid matches; otherwise ErrUnknownKey-shaped error (the validator
// wraps the error into auth.ErrUnknownKey).
func (s *devKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != s.kid {
		return nil, "", fmt.Errorf("dev key set: kid %q not known", kid)
	}
	return s.pub, "ES256", nil
}

// devSigner pairs the ephemeral ES256 keypair with the JWT-signing
// helper. The private key is held only on this struct; nothing in
// the binary serializes it.
type devSigner struct {
	keys *devKeySet
	priv *ecdsa.PrivateKey
}

// newDevSigner generates a fresh ES256 keypair using crypto/rand and
// returns the signer + the auth.KeySet that resolves the kid. The
// returned signer is owned by the `dev` cmd; it never escapes the
// subcommand boundary.
func newDevSigner() (*devSigner, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("dev signer: generate ES256 key: %w", err)
	}
	return &devSigner{
		keys: &devKeySet{kid: DevKID, pub: &priv.PublicKey},
		priv: priv,
	}, nil
}

// SignDevToken returns a Bearer-shaped JWT for the (tenant, user,
// session) triple with the supplied scopes. Expiry is `DevTokenTTL`
// from `now`. The token carries `kid=harbor-dev` so the matching
// KeySet resolves the public key at validation time.
//
// `now` is parameterised so tests can pin expiry deterministically.
// Production callers pass `time.Now()`.
func (s *devSigner) SignDevToken(now time.Time, tenant, user, session string, scopes []string) (string, error) {
	if s == nil || s.priv == nil {
		return "", errors.New("dev signer: nil signer")
	}
	if tenant == "" || user == "" || session == "" {
		return "", fmt.Errorf("dev signer: identity triple incomplete (tenant=%q, user=%q, session=%q)",
			tenant, user, session)
	}
	claims := jwt.MapClaims{
		"iss":     "harbor-dev",
		"sub":     user,
		"aud":     "harbor",
		"exp":     now.Add(DevTokenTTL).Unix(),
		"nbf":     now.Add(-1 * time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  tenant,
		"user":    user,
		"session": session,
	}
	if len(scopes) > 0 {
		claims["scopes"] = scopes
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = s.keys.kid
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		return "", fmt.Errorf("dev signer: sign: %w", err)
	}
	return signed, nil
}

// KeySet returns the auth.KeySet half of the dev signer — the value
// callers pass to `auth.NewValidator(keys, ...)`.
func (s *devSigner) KeySet() auth.KeySet { return s.keys }

// IssueToken implements auth.TokenIssuer (Phase 73m / D-129). It
// re-mints a Bearer-shaped ES256 JWT for the supplied — already
// verified — identity triple + scope set, used by the
// `auth.rotate_token` Protocol method the Console Settings page calls.
//
// The dev signer is the V1 TokenIssuer implementation: `harbor dev` /
// `harbor console` mint ephemeral tokens themselves. A real deployment
// behind an external OIDC provider wires an RFC 8693 token-exchange
// issuer behind the same auth.TokenIssuer seam in a later
// release-engineering phase — IssueToken's signature is the contract.
//
// IssueToken is read-only over the immutable signer; safe for
// concurrent use by N goroutines (D-025).
func (s *devSigner) IssueToken(_ context.Context, id identity.Identity, scopes []auth.Scope, now time.Time) (string, time.Time, error) {
	strScopes := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		strScopes = append(strScopes, string(sc))
	}
	token, err := s.SignDevToken(now, id.TenantID, id.UserID, id.SessionID, strScopes)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("dev signer: rotate token: %w", err)
	}
	return token, now.Add(DevTokenTTL), nil
}
