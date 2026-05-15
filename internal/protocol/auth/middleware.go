package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// bearerPrefix is the standard scheme keyword preceding a JWT in the
// Authorization header (RFC 6750). The check is case-insensitive on
// the scheme.
const bearerPrefix = "Bearer "

// middlewareConfig holds the optional knobs Middleware threads into the
// per-request handler. Set once at construction; never mutated after.
type middlewareConfig struct {
	logger *slog.Logger
}

// MiddlewareOption configures Middleware.
type MiddlewareOption func(*middlewareConfig)

// MWLogger sets the slog.Logger the middleware logs rejection paths
// to. A nil logger keeps slog.Default(). The validator carries its
// own logger for the audit emit; this one logs the wire-side
// rejection (the chosen Protocol error code, the HTTP status).
func MWLogger(l *slog.Logger) MiddlewareOption {
	return func(c *middlewareConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// Middleware returns an http.Handler decorator that enforces JWT-bearer
// auth on every request.
//
// The middleware:
//
//  1. Reads the `Authorization: Bearer <token>` header. A missing or
//     malformed header writes a 401 + CodeIdentityRequired Protocol
//     error body and returns — `next` is never called.
//  2. Calls Validator.Validate(ctx, token). A failure writes a 401 +
//     the appropriate Protocol error code (CodeIdentityRequired for
//     ErrIdentityClaimMissing / ErrTokenMissing; CodeAuthRejected for
//     every other sentinel) and returns.
//  3. On success, attaches the verified identity to r.Context() (via
//     identity.With) and the verified scope set (via WithScopes),
//     then calls next with the augmented request.
//
// Middleware is a compiled artifact: every field is set once at
// construction and never mutated. The decorator is safe to share
// across N concurrent requests (D-025).
func Middleware(v Validator, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	if v == nil {
		// A middleware with no validator could not enforce anything —
		// return a decorator that fails every request closed. Better
		// than silently passing through (CLAUDE.md §5).
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = next
				writeProtocolError(w, http.StatusInternalServerError,
					protoerrors.Newf(protoerrors.CodeRuntimeError,
						"auth middleware misconfigured: validator is nil"))
			})
		}
	}

	cfg := middlewareConfig{logger: slog.Default()}
	for _, opt := range opts {
		opt(&cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := extractBearer(r.Header.Get("Authorization"))
			if err != nil {
				perr := protoerrors.Newf(protoerrors.CodeIdentityRequired,
					"authorization header missing or malformed: %v", err)
				writeProtocolError(w, http.StatusUnauthorized, perr)
				return
			}

			verified, err := v.Validate(r.Context(), token)
			if err != nil {
				code, status := protocolErrorFor(err)
				perr := protoerrors.Newf(code, "jwt rejected: %s", reasonForWire(err))
				cfg.logger.WarnContext(r.Context(), "auth: middleware rejected request",
					slog.String("code", string(code)),
					slog.Int("status", status),
					slog.String("reason_sentinel", reasonForWire(err)))
				writeProtocolError(w, status, perr)
				return
			}

			ctx := r.Context()
			ctx, idErr := identity.With(ctx, verified.Identity)
			if idErr != nil {
				// Defensive — Validate already ran identity.Validate
				// and would have failed with ErrIdentityClaimMissing.
				// If we get here it's a Validator-implementation bug;
				// fail loud.
				cfg.logger.ErrorContext(r.Context(), "auth: validator returned identity that fails Validate",
					slog.String("error", idErr.Error()))
				writeProtocolError(w, http.StatusInternalServerError,
					protoerrors.Newf(protoerrors.CodeRuntimeError,
						"validator returned an invalid identity"))
				return
			}
			ctx = WithScopes(ctx, verified.Scopes)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearer pulls the JWT out of an `Authorization: Bearer <token>`
// header. A missing header, a missing scheme, a non-Bearer scheme, or
// an empty token all return ErrTokenMissing — the wire-side rejection
// path is the same.
func extractBearer(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", fmt.Errorf("%w: header empty", ErrTokenMissing)
	}
	if len(header) <= len(bearerPrefix) {
		return "", fmt.Errorf("%w: header too short for `Bearer <token>`", ErrTokenMissing)
	}
	scheme := header[:len(bearerPrefix)]
	if !strings.EqualFold(scheme, bearerPrefix) {
		return "", fmt.Errorf("%w: scheme %q is not Bearer", ErrTokenMissing, strings.TrimSpace(scheme))
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", fmt.Errorf("%w: bearer scheme but no token", ErrTokenMissing)
	}
	return token, nil
}

// protocolErrorFor maps a Validate error onto a Protocol error Code +
// the HTTP status to write. The mapping is the wire contract: a client
// branches on the Code (the JSON body) but an intermediary branches
// on the HTTP status, so the two must agree.
//
// ErrTokenMissing / ErrIdentityClaimMissing → CodeIdentityRequired
// (the request lacks an identity scope per RFC §5.5). Everything else
// → CodeAuthRejected (the request carried an identity claim but it
// failed verification).
func protocolErrorFor(err error) (protoerrors.Code, int) {
	switch {
	case errors.Is(err, ErrTokenMissing):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized
	case errors.Is(err, ErrIdentityClaimMissing):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized
	default:
		return protoerrors.CodeAuthRejected, http.StatusUnauthorized
	}
}

// reasonForWire returns the rejection reason in a wire-safe form: the
// sentinel name only, never the wrapped detail (which may include the
// `kid` or other operator-specific data we deliberately do not echo to
// an unauthenticated caller). CLAUDE.md §7 rule 7.
func reasonForWire(err error) string {
	switch {
	case errors.Is(err, ErrTokenMissing):
		return "token_missing"
	case errors.Is(err, ErrTokenMalformed):
		return "token_malformed"
	case errors.Is(err, ErrAlgNotAllowed):
		return "alg_not_allowed"
	case errors.Is(err, ErrSignatureInvalid):
		return "signature_invalid"
	case errors.Is(err, ErrTokenExpired):
		return "token_expired"
	case errors.Is(err, ErrTokenNotYetValid):
		return "token_not_yet_valid"
	case errors.Is(err, ErrUnknownKey):
		return "unknown_key"
	case errors.Is(err, ErrIdentityClaimMissing):
		return "identity_claim_missing"
	case errors.Is(err, ErrAudienceMismatch):
		return "audience_mismatch"
	case errors.Is(err, ErrIssuerMismatch):
		return "issuer_mismatch"
	}
	return "verification_failed"
}

// writeProtocolError encodes a *protoerrors.Error as a JSON body with
// the given status. A marshal failure degrades to a bare 500 — never a
// partial body.
func writeProtocolError(w http.ResponseWriter, status int, perr *protoerrors.Error) {
	buf, err := json.Marshal(perr)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
