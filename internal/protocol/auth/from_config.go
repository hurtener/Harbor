// from_config.go — the operator-config → production validator
// projection. Mirrors the FromConfig convention the other subsystems
// follow (memory.SnapshotFromConfig, llm.SnapshotFromConfig, …): the
// projection lives next to the validator it builds so a new identity
// config field and its wiring land in the same package.
//
// Import direction: auth imports internal/config additively; config
// stays a leaf (it imports neither auth nor any subsystem), so there is
// no cycle.

package auth

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
)

// ValidatorDeps carries the cross-subsystem dependencies the production
// validator projection wires into the Validator. The Redactor is
// mandatory (NewValidator fails closed without it); Logger and Bus are
// optional (a nil Logger defaults to slog.Default(); a nil Bus disables
// the canonical auth.rejected bus emit, keeping the slog audit).
type ValidatorDeps struct {
	Redactor audit.Redactor
	Logger   *slog.Logger
	Bus      events.EventBus
}

// NewJWKSValidator builds the production JWT Validator from the
// operator's identity config block: a JWKS-backed KeySet (URL or file
// source) behind the existing Validator interface, with the issuer,
// audience, redactor, logger, and event bus wired from the config and
// deps. The initial JWKS fetch runs synchronously inside this call, so
// a misconfigured or unreachable source fails loud here rather than at
// first request.
//
// The config's algorithm allowlist (already validated to be asymmetric)
// narrows which JWK Set entries the keyset accepts. Extra opts are
// threaded into the keyset (tests inject a clock / counting HTTP client;
// production passes none).
func NewJWKSValidator(ctx context.Context, cfg config.IdentityConfig, deps ValidatorDeps, opts ...JWKSOption) (Validator, error) {
	src := JWKSSource{URL: cfg.JWKSURL, File: cfg.JWKSFile}

	// The config's max-stale ceiling is passed straight through — the
	// option owns the single default: a zero/unset field maps to
	// defaultJWKSMaxStale inside WithJWKSMaxStale, a validated positive
	// value is honored verbatim. Re-applying the default here would be a
	// second source of truth.
	keyOpts := append([]JWKSOption{WithJWKSMaxStale(cfg.JWKSMaxStale)}, opts...)
	if deps.Logger != nil {
		keyOpts = append([]JWKSOption{WithJWKSLogger(deps.Logger)}, keyOpts...)
	}

	keys, err := NewJWKSKeySet(ctx, src, cfg.JWTAlgorithms, keyOpts...)
	if err != nil {
		return nil, fmt.Errorf("auth: build JWKS key set: %w", err)
	}

	validator, err := NewValidator(keys,
		WithIssuer(cfg.Issuer),
		WithAudience(cfg.Audience),
		WithRedactor(deps.Redactor),
		WithLogger(deps.Logger),
		WithEventBus(deps.Bus),
	)
	if err != nil {
		return nil, fmt.Errorf("auth: build production validator: %w", err)
	}
	return validator, nil
}
