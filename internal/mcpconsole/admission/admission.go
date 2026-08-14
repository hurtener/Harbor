// Package admission provides the narrow, stateless, sealed
// render-admission authority for MCP App `ui://` render documents.
//
// # What it is
//
// An Authority mints short-lived opaque admission tokens that bind ONE
// render request: the already-authorized identity triple, the effective
// agent, the host server identity, the exact `ui://` resource URI, and
// the current registry descriptor generation fingerprint. The sealed
// claims are strict canonical JSON carrying exactly that tuple plus the
// claim-family schema/version, the issued/expiry instants, and a
// crypto-random 128-bit nonce — nothing else. No tool arguments,
// provider output, tool-context content, callback names, secrets, keys,
// or authority beyond that single tuple ever enters a claim.
//
// # What it is NOT
//
// This package performs NO viewer authorization and NO resource lookup,
// and it never mints on read. The host/App dispatch callers that
// consume admissions own those fresh checks and must request an
// admission explicitly before rendering. Tokens are not persisted:
// neither Mint nor Verify touches storage, the MCP registry, or any
// provider-local binding.
//
// # Failure model
//
// Verify fails closed. Outcomes are typed — Missing, Unavailable,
// Invalid, Expired, Mismatch — and are compared with errors.Is against
// the package sentinels ErrTokenMissing, ErrTokenUnavailable,
// ErrTokenInvalid, ErrTokenExpired, and ErrTokenMismatch. Tamper,
// wrong key/replica, stale descriptor generation, resource/server/
// agent/identity mismatch, and expiry all fail closed, and a Mismatch
// never reveals which tuple dimension differed.
//
// # Concurrent reuse
//
// An Authority is an immutable compiled artifact: sealer, clock, and TTL
// are fixed at construction and never mutate, so N goroutines may share
// one instance without locks and without run-to-run bleed. The wrapped
// auth.Sealer is itself concurrency-safe per its documented contract.
package admission

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// Sentinel errors. Construction and mint failures use ErrNilSealer and
// ErrInvalidMintInput; every Verify failure wraps exactly one of the
// five ErrToken* outcome sentinels so callers can branch with
// errors.Is.
var (
	// ErrNilSealer — construction fails loud when the required seal/
	// open seam is nil.
	ErrNilSealer = errors.New("admission: nil sealer")

	// ErrInvalidMintInput — a mint input (or a verify expectation)
	// violates the render-tuple bounds.
	ErrInvalidMintInput = errors.New("admission: invalid render tuple input")

	// ErrTokenMissing — no admission token was supplied to Verify.
	ErrTokenMissing = errors.New("admission: no render-admission token supplied")

	// ErrTokenUnavailable — the supplied token could not be opened:
	// invalid base64url, an oversized token, envelope tamper, or a
	// different sealing key/replica. The content of the token is
	// unrecoverable by design.
	ErrTokenUnavailable = errors.New("admission: render-admission token could not be opened")

	// ErrTokenInvalid — the token opened but its claims are
	// structurally invalid: unknown schema/version, bound violations,
	// malformed nonce, absurd lifetime, or future issuance.
	ErrTokenInvalid = errors.New("admission: render-admission token is structurally invalid")

	// ErrTokenExpired — the token is well-formed but its expiry is past.
	ErrTokenExpired = errors.New("admission: render-admission token is expired")

	// ErrTokenMismatch — the token is well-formed and time-valid but
	// does not match the requested render tuple. The error deliberately
	// names no dimension, so a caller never learns which of identity/
	// agent/server/resource/generation differed.
	ErrTokenMismatch = errors.New("admission: render-admission token does not match the requested render tuple")
)

// Authority is the immutable, stateless render-admission authority.
// It holds only the seal/open seam, the injected clock, and the bounded
// positive TTL — all fixed at construction. It performs no persistence
// and consumes no provider-local binding.
type Authority struct {
	sealer auth.Sealer
	clock  func() time.Time
	ttl    time.Duration
}

// Option configures an Authority at construction. Options are applied in
// order; the last one wins.
type Option func(*Authority) error

// WithClock injects the clock used for issued/expiry computation and
// verification. Production code uses the time.Now default; tests inject
// a controllable clock for deterministic time-bound tests. A nil clock
// is rejected — fail loud.
func WithClock(now func() time.Time) Option {
	return func(a *Authority) error {
		if now == nil {
			return errors.New("admission: WithClock requires a non-nil clock")
		}
		a.clock = now
		return nil
	}
}

// WithTTL bounds the lifetime minted admissions carry. The TTL must be
// positive and no greater than MaxTTL; any other value fails
// construction loud. The default is DefaultTTL (15 minutes).
func WithTTL(ttl time.Duration) Option {
	return func(a *Authority) error {
		if ttl <= 0 || ttl > MaxTTL {
			return fmt.Errorf("admission: WithTTL %s out of bounds (0 < ttl <= %s)", ttl, MaxTTL)
		}
		a.ttl = ttl
		return nil
	}
}

// New constructs an Authority over the mandatory seal/open seam. A nil
// sealer fails construction loud — there is no unsealed fallback.
func New(sealer auth.Sealer, opts ...Option) (*Authority, error) {
	if sealer == nil {
		return nil, fmt.Errorf("%w: render-admission authority requires a non-nil sealer", ErrNilSealer)
	}
	a := &Authority{
		sealer: sealer,
		clock:  time.Now,
		ttl:    DefaultTTL,
	}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// RenderTuple is the one render admission binds: the already-authorized
// identity triple, the effective agent, the host server identity, the
// exact case-sensitive `ui://` resource URI, and the current registry
// descriptor generation fingerprint. Every component is mandatory and
// bounded; an admission authorizes exactly this tuple and no other.
type RenderTuple struct {
	Identity identity.Identity
	AgentID  string
	ServerID string
	// ResourceURI is the exact `ui://` resource URI of the MCP App
	// document, matched case-sensitively.
	ResourceURI string
	// DescriptorFingerprint is the registry descriptor generation
	// fingerprint (the provider/catalog generation) current at mint
	// time.
	DescriptorFingerprint string
}

// Token is a minted render admission: the opaque base64url token string
// plus its issued/expiry metadata.
type Token struct {
	Value     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// validateTuple applies the render-tuple input bounds at both edges:
// identity must Validate, and every string component must satisfy the
// same string-claim bounds the sealed claims enforce, including the
// reserved `ui://` scheme on the resource URI.
func validateTuple(rt RenderTuple) error {
	if err := identity.Validate(rt.Identity); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMintInput, err)
	}
	fields := []struct {
		name string
		val  string
	}{
		{"agent_id", rt.AgentID},
		{"server_id", rt.ServerID},
		{"resource_uri", rt.ResourceURI},
		{"descriptor_generation", rt.DescriptorFingerprint},
	}
	for _, f := range fields {
		if err := validateBoundString(f.name, f.val); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidMintInput, err)
		}
	}
	if !strings.HasPrefix(rt.ResourceURI, resourceScheme) {
		return fmt.Errorf("%w: resource_uri %q does not carry the ui:// scheme", ErrInvalidMintInput, rt.ResourceURI)
	}
	return nil
}

// Mint seals a new render admission for the exact tuple. The tuple is
// taken as already-authorized by the caller; this package performs no
// viewer authorization or resource lookup and mints only on explicit
// request. The returned token is a bounded base64url string carrying
// the sealed canonical claims plus issued/expiry metadata. Two Mint
// calls for the identical tuple produce distinct tokens (a fresh
// crypto-random 128-bit claim nonce per call, on top of the sealer's
// own fresh envelope nonce).
func (a *Authority) Mint(ctx context.Context, rt RenderTuple) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	if err := validateTuple(rt); err != nil {
		return Token{}, err
	}
	now := a.clock().UTC()
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return Token{}, fmt.Errorf("admission: claim nonce: %w", err)
	}
	claims := Claims{
		Schema:               Schema,
		Version:              SchemaVersion,
		TenantID:             rt.Identity.TenantID,
		UserID:               rt.Identity.UserID,
		SessionID:            rt.Identity.SessionID,
		AgentID:              rt.AgentID,
		ServerID:             rt.ServerID,
		ResourceURI:          rt.ResourceURI,
		DescriptorGeneration: rt.DescriptorFingerprint,
		IssuedAt:             now.Unix(),
		ExpiresAt:            now.Add(a.ttl).Unix(),
		Nonce:                base64.RawURLEncoding.EncodeToString(nonce),
	}
	plaintext, err := canonicalJSON(claims)
	if err != nil {
		return Token{}, fmt.Errorf("admission: canonical claims encode: %w", err)
	}
	if len(plaintext) > MaxClaimJSONBytes {
		return Token{}, fmt.Errorf("admission: claims exceed %d bytes", MaxClaimJSONBytes)
	}
	sealed, err := a.sealer.Seal(plaintext)
	if err != nil {
		return Token{}, fmt.Errorf("admission: seal: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(sealed)
	if len(value) > MaxTokenBytes {
		return Token{}, fmt.Errorf("admission: token exceeds %d bytes", MaxTokenBytes)
	}
	return Token{
		Value:     value,
		IssuedAt:  claims.IssuedAtTime(),
		ExpiresAt: claims.ExpiresAtTime(),
	}, nil
}

// Verify opens token, strictly validates its claims, and requires the
// exact expected tuple — identity, agent, server, resource URI, and the
// CURRENT descriptor fingerprint. It performs no persistence and
// consumes no provider-local binding.
//
// The outcome is typed via errors.Is:
//
//   - ErrTokenMissing when no token is supplied,
//   - ErrTokenUnavailable when the token cannot be opened (bad
//     base64url, oversize, envelope tamper, wrong key/replica),
//   - ErrTokenInvalid when the opened claims are structurally invalid
//     (unknown schema/version, bound violations, malformed nonce,
//     absurd lifetime, future issuance),
//   - ErrTokenExpired when the admission's expiry is past,
//   - ErrTokenMismatch when the claims are valid but the tuple differs
//     (identity/agent/server/resource/descriptor generation) — the
//     mismatch never names which dimension differed.
//
// On success the admitted claims are returned for audit/observation
// purposes. A token minted by another Authority sharing the same sealer
// verifies identically after restart; a token sealed under a different
// key fails as Unavailable.
func (a *Authority) Verify(ctx context.Context, expected RenderTuple, token string) (Claims, error) {
	if err := ctx.Err(); err != nil {
		return Claims{}, err
	}
	if err := validateTuple(expected); err != nil {
		return Claims{}, err
	}
	if token == "" {
		return Claims{}, ErrTokenMissing
	}
	if len(token) > MaxTokenBytes {
		return Claims{}, fmt.Errorf("%w: token is %d bytes, max %d", ErrTokenUnavailable, len(token), MaxTokenBytes)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: token is not valid base64url: %v", ErrTokenUnavailable, err)
	}
	plaintext, err := a.sealer.Open(sealed)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: envelope: %v", ErrTokenUnavailable, err)
	}
	if len(plaintext) > MaxClaimJSONBytes {
		return Claims{}, fmt.Errorf("%w: claims plaintext is %d bytes, max %d", ErrTokenInvalid, len(plaintext), MaxClaimJSONBytes)
	}
	if !utf8ValidBytes(plaintext) {
		return Claims{}, fmt.Errorf("%w: claims plaintext is not valid UTF-8", ErrTokenInvalid)
	}
	var claims Claims
	if err := strictDecode(plaintext, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: claims decode: %v", ErrTokenInvalid, err)
	}
	now := a.clock()
	if err := validateClaimsStructure(&claims, now); err != nil {
		return Claims{}, err
	}
	if err := checkClaimsExpiry(&claims, now); err != nil {
		return Claims{}, err
	}
	if err := matchTuple(&claims, expected); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

// matchTuple compares the admitted claims against the expected tuple
// across every dimension — identity triple, agent, server, resource
// URI, and descriptor generation. Any difference yields a bare
// ErrTokenMismatch: the caller learns only that the admission is not
// for the requested tuple, never which dimension differed.
func matchTuple(c *Claims, expected RenderTuple) error {
	if c.TenantID == expected.Identity.TenantID &&
		c.UserID == expected.Identity.UserID &&
		c.SessionID == expected.Identity.SessionID &&
		c.AgentID == expected.AgentID &&
		c.ServerID == expected.ServerID &&
		c.ResourceURI == expected.ResourceURI &&
		c.DescriptorGeneration == expected.DescriptorFingerprint {
		return nil
	}
	return ErrTokenMismatch
}
