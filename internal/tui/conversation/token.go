package conversation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

var (
	// ErrTokenExpired reports a signed credential whose exp is not in the future.
	ErrTokenExpired = errors.New("tui: bearer token expired")
	// ErrTokenMalformed reports a token that cannot supply identity and expiry.
	ErrTokenMalformed = errors.New("tui: malformed bearer token")
	// ErrTokenUnavailable reports no credential for the requested identity.
	ErrTokenUnavailable = errors.New("tui: bearer token unavailable")
)

// LifetimeTokenSource reloads a token file for every resolution and supports
// identity-keyed in-memory replacement. Signed credentials are never persisted.
type LifetimeTokenSource struct {
	mu       sync.RWMutex
	path     string
	fallback string
	replaced map[string]string
	now      func() time.Time
}

// NewTokenSource constructs a lifetime-scoped source. file may contain either
// one JWT or a JSON object keyed by tenant/user/session.
func NewTokenSource(path, fallback string) *LifetimeTokenSource {
	return &LifetimeTokenSource{path: path, fallback: strings.TrimSpace(fallback), replaced: map[string]string{}, now: time.Now}
}

// Replace installs a process-memory credential for one complete target scope.
func (s *LifetimeTokenSource) Replace(scope types.IdentityScope, token string) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	if _, err := ParseToken(token, s.now()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaced[scopeKey(scope)] = strings.TrimSpace(token)
	return nil
}

// Replacement returns an isolated in-memory override for rollback handling.
func (s *LifetimeTokenSource) Replacement(scope types.IdentityScope) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.replaced[scopeKey(scope)]
	return value, ok
}

// Clear removes an in-memory override so file rotation or fallback resumes.
func (s *LifetimeTokenSource) Clear(scope types.IdentityScope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.replaced, scopeKey(scope))
}

// Token resolves anew before each REST call and SSE connection.
func (s *LifetimeTokenSource) Token(ctx context.Context, scope types.IdentityScope) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateScope(scope); err != nil {
		return "", err
	}
	s.mu.RLock()
	replacement := s.replaced[scopeKey(scope)]
	path, fallback, now := s.path, s.fallback, s.now
	s.mu.RUnlock()
	if replacement != "" {
		return validateTokenForScope(replacement, scope, now())
	}
	if path != "" {
		body, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) && fallback != "" {
			return validateTokenForScope(fallback, scope, now())
		}
		if err != nil {
			return "", fmt.Errorf("tui: read token file %s: %w", path, err)
		}
		value := strings.TrimSpace(string(body))
		var tokens map[string]string
		if strings.HasPrefix(value, "{") {
			if err := json.Unmarshal(body, &tokens); err != nil {
				return "", fmt.Errorf("%w: token file JSON: %w", ErrTokenMalformed, err)
			}
			value = strings.TrimSpace(tokens[scopeKey(scope)])
		}
		if value == "" {
			return "", fmt.Errorf("%w for %s", ErrTokenUnavailable, scopeKey(scope))
		}
		return validateTokenForScope(value, scope, now())
	}
	if fallback == "" {
		return "", ErrTokenUnavailable
	}
	return validateTokenForScope(fallback, scope, now())
}

// TokenClaims contains the unverified routing claims needed to choose the
// Protocol identity. Authenticity remains exclusively server-verified.
type TokenClaims struct {
	Identity  types.IdentityScope
	ExpiresAt time.Time
}

// ParseToken parses JWT payload claims without treating them as authentication.
// It exists only to route the credential to the identity the server will verify.
func ParseToken(token string, now time.Time) (TokenClaims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return TokenClaims{}, ErrTokenMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, fmt.Errorf("%w: payload encoding", ErrTokenMalformed)
	}
	var claims struct {
		Tenant, User, Session string
		Exp                   json.Number `json:"exp"`
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return TokenClaims{}, fmt.Errorf("%w: payload JSON", ErrTokenMalformed)
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return TokenClaims{}, fmt.Errorf("%w: exp required", ErrTokenMalformed)
	}
	identity := types.IdentityScope{Tenant: claims.Tenant, User: claims.User, Session: claims.Session}
	if err := validateScope(identity); err != nil {
		return TokenClaims{}, fmt.Errorf("%w: identity claims required", ErrTokenMalformed)
	}
	expires := time.Unix(exp, 0)
	if !expires.After(now) {
		return TokenClaims{}, ErrTokenExpired
	}
	return TokenClaims{Identity: identity, ExpiresAt: expires}, nil
}

func validateTokenForScope(token string, scope types.IdentityScope, now time.Time) (string, error) {
	claims, err := ParseToken(token, now)
	if err != nil {
		return "", err
	}
	if claims.Identity.Tenant != scope.Tenant || claims.Identity.User != scope.User || claims.Identity.Session != scope.Session {
		return "", fmt.Errorf("%w: credential is %s, requested %s", ErrTokenUnavailable, scopeKey(claims.Identity), scopeKey(scope))
	}
	return strings.TrimSpace(token), nil
}
func validateScope(scope types.IdentityScope) error {
	if scope.Tenant == "" || scope.User == "" || scope.Session == "" {
		return errors.New("tui: complete identity required")
	}
	return nil
}
func scopeKey(scope types.IdentityScope) string {
	return scope.Tenant + "/" + scope.User + "/" + scope.Session
}
