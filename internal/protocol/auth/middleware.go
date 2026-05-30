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

// HeaderSession is the per-request session selector (D-171). The
// connection token authenticates the (tenant, user, scopes) — it is a
// per-backend credential, like an API key, NOT a single-session pin.
// The session is chosen per-conversation by the client and supplied on
// every request via this header. When present, the middleware REPLACES
// the token's `session` claim with the header value (keeping the
// token's verified tenant + user), so one connection drives many
// isolated sessions. The token's `session` claim is a DEFAULT only:
// when the header is absent, the claim's session is used.
//
// The value MUST be identical to the SSE transport's
// `stream.HeaderSession`; the constant is duplicated here (rather than
// imported) because `stream` imports `auth` and the reverse would be an
// import cycle.
const HeaderSession = "X-Harbor-Session"

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

			// Defensive: the Validator already ran identity.Validate over
			// the JWT claims, so a verified identity with an empty
			// tenant/user is a Validator-implementation bug. Re-check
			// tenant + user here (NOT session — session is per-request
			// below) and fail loud with a 500: this is a server bug, not
			// a client error.
			if verified.Identity.TenantID == "" || verified.Identity.UserID == "" {
				cfg.logger.ErrorContext(r.Context(), "auth: validator returned identity with empty tenant/user",
					slog.String("tenant", verified.Identity.TenantID),
					slog.String("user", verified.Identity.UserID))
				writeProtocolError(w, http.StatusInternalServerError,
					protoerrors.Newf(protoerrors.CodeRuntimeError,
						"validator returned an invalid identity"))
				return
			}

			// D-171: per-request session. The verified token carries
			// (tenant, user) + a default `session` claim. When the
			// request supplies `X-Harbor-Session`, that value REPLACES
			// the claim's session — the connection token is a
			// per-backend credential, not a single-session pin. The
			// tenant + user stay token-verified; only the session is
			// client-chosen, so a request can never widen its tenant or
			// user (multi-isolation §6 stays enforced). An empty header
			// falls back to the token's default session claim.
			effectiveID := verified.Identity
			if hdrSession := strings.TrimSpace(r.Header.Get(HeaderSession)); hdrSession != "" {
				effectiveID.SessionID = hdrSession
			}
			if effectiveID.SessionID == "" {
				// No default session claim on the token AND no
				// X-Harbor-Session header — identity is mandatory and the
				// session is the innermost scope (§6 rule 9). This is a
				// client error (the caller must choose a session), so 401
				// with CodeIdentityRequired, not 500.
				cfg.logger.WarnContext(r.Context(), "auth: request has no resolvable session",
					slog.String("tenant", effectiveID.TenantID),
					slog.String("user", effectiveID.UserID))
				writeProtocolError(w, http.StatusUnauthorized,
					protoerrors.Newf(protoerrors.CodeIdentityRequired,
						"no session: token carries no default session claim and no X-Harbor-Session header supplied"))
				return
			}

			ctx := r.Context()
			ctx, idErr := identity.With(ctx, effectiveID)
			if idErr != nil {
				// Unreachable given the explicit tenant/user + session
				// checks above; kept as a defensive fail-closed (§5).
				cfg.logger.ErrorContext(r.Context(), "auth: identity.With rejected a checked identity",
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
//
//nolint:unparam // (Code, HTTP-status) pair is a deliberate wire-contract shape — every auth error is 401 today, but the status stays explicit so a future 403-mapping error doesn't need a signature change at the call site.
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
	_, _ = w.Write(buf) //nolint:errcheck // response status already committed — a write error cannot be recovered here.
}
