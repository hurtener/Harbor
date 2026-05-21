package auth

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// Phase 73m (D-129) — the `auth.rotate_token` implementation.
//
// `auth.rotate_token` rotates the operator's current Protocol-auth
// token. The Runtime re-mints a JWT for the caller's already-verified
// `(tenant, user, session)` identity and returns it once
// (one-time-reveal). The encrypted persistence — the operator re-saving
// the new token into the 72h `auth_profiles` table — is the Console's
// job, not the Runtime's.
//
// # Why a TokenIssuer seam (CLAUDE.md §4.4)
//
// A Runtime does not, in general, mint its own tokens — a real
// deployment's tokens are issued by an external OIDC provider, and the
// Runtime only *validates* them. But the V1 `harbor dev` posture mints
// ephemeral ES256 tokens itself (the dev signer), and the Console
// Settings page needs a working "Rotate token" action against that
// posture. The TokenIssuer interface is the §4.4 seam: the dev signer
// is the V1 implementation; a future release-engineering phase wires a
// real OIDC token-exchange issuer (RFC 8693) behind the same shape
// without reshaping this surface.
//
// When no TokenIssuer is wired, the surface fails loudly — it never
// silently degrades to a no-op (CLAUDE.md §13).
//
// # Admin-gated (D-079)
//
// `auth.rotate_token` requires the verified `ScopeAdmin` claim. The
// closed two-scope set (`admin` + `console:fleet`) is the only admit
// surface — there is NO `auth.admin` scope. A request without the
// claim is rejected with ErrRotateScopeRequired, which the wire handler
// maps onto CodeIdentityScopeRequired (HTTP 403).
//
// # Audit (CLAUDE.md §7 rule 6)
//
// Every successful rotation emits a redacted `audit.admin_scope_used`
// event through the wired Redactor + Bus. When the bus is unwired the
// rotation is logged at Info — never fully silent.

// TokenIssuer re-mints a Protocol-auth JWT for an already-verified
// identity. The V1 implementation is the `harbor dev` ephemeral signer;
// a post-V1 release-engineering phase fits an OIDC token-exchange
// issuer (RFC 8693) behind the same shape.
//
// An implementation MUST be safe for concurrent use by N goroutines
// (D-025) — RotateSurface shares one issuer across every request.
type TokenIssuer interface {
	// IssueToken mints a fresh Bearer-shaped JWT for the supplied
	// identity triple + scope set, expiring at `now + TTL`. The
	// returned string is the raw token; expiresAt is its expiry, UTC.
	// The caller has already verified the identity — IssueToken does
	// not re-validate it.
	IssueToken(ctx context.Context, id identity.Identity, scopes []Scope, now time.Time) (token string, expiresAt time.Time, err error)
}

// Rotation-surface sentinel errors. Callers (the wire handler) compare
// via errors.Is and map onto the canonical Protocol Code.
var (
	// ErrRotateMisconfigured — NewRotateSurface was called with a nil
	// TokenIssuer or redactor.
	ErrRotateMisconfigured = stderrors.New("auth: rotate-token surface missing a mandatory dependency")
	// ErrRotateIdentityRequired — the request carried an incomplete
	// identity triple. Maps onto CodeIdentityRequired (HTTP 401).
	ErrRotateIdentityRequired = stderrors.New("auth: rotate-token identity scope incomplete")
	// ErrRotateScopeRequired — the caller lacks the verified
	// `admin` scope claim. Maps onto CodeIdentityScopeRequired (403).
	ErrRotateScopeRequired = stderrors.New("auth: rotate-token requires the verified `admin` scope claim")
	// ErrRotateIdentityMismatch — the request body's identity scope
	// disagreed with the verified-JWT identity. Maps onto
	// CodeIdentityRequired (HTTP 401) — defence-in-depth.
	ErrRotateIdentityMismatch = stderrors.New("auth: rotate-token body identity disagrees with the verified token")
	// ErrRotateIssueFailed — the TokenIssuer failed to mint a token.
	// Maps onto CodeRuntimeError (HTTP 500).
	ErrRotateIssueFailed = stderrors.New("auth: rotate-token issuer failed to mint a token")
)

// RotateSurface is the transport-agnostic `auth.rotate_token` handler.
// It is built once per Runtime process via NewRotateSurface and shared
// across every Protocol request; Rotate is safe for concurrent use by
// N goroutines (D-025) — every field is set once at construction and
// never mutated.
type RotateSurface struct {
	issuer   TokenIssuer
	redactor audit.Redactor
	bus      events.EventBus
	logger   *slog.Logger
}

// RotateOption configures NewRotateSurface.
type RotateOption func(*RotateSurface)

// WithRotateBus wires the events.EventBus the surface publishes the
// `audit.admin_scope_used` event onto on every successful rotation.
// OPTIONAL — when unwired, the rotation is logged at Info instead
// (never fully silent — CLAUDE.md §13). A nil bus is treated as
// "WithRotateBus not supplied".
func WithRotateBus(b events.EventBus) RotateOption {
	return func(s *RotateSurface) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithRotateLogger sets the slog.Logger the surface logs to. A nil
// logger keeps slog.Default().
func WithRotateLogger(l *slog.Logger) RotateOption {
	return func(s *RotateSurface) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewRotateSurface builds the `auth.rotate_token` surface. The
// TokenIssuer and the audit.Redactor are MANDATORY — a nil fails loud
// with ErrRotateMisconfigured rather than building a surface that would
// nil-panic or emit an unredacted audit payload (CLAUDE.md §5, §7
// rule 6, §13).
//
// The returned *RotateSurface is immutable after construction (D-025)
// and safe for concurrent use by N goroutines.
func NewRotateSurface(issuer TokenIssuer, redactor audit.Redactor, opts ...RotateOption) (*RotateSurface, error) {
	if issuer == nil {
		return nil, fmt.Errorf("%w: TokenIssuer is nil", ErrRotateMisconfigured)
	}
	if redactor == nil {
		return nil, fmt.Errorf("%w: audit.Redactor is nil", ErrRotateMisconfigured)
	}
	s := &RotateSurface{
		issuer:   issuer,
		redactor: redactor,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// AuthRotateTokenPayload is the typed SafePayload published on the
// canonical `audit.admin_scope_used` event when an operator rotates
// their token. Phase 73m / D-129.
//
// SafePayload by construction: every field is a bounded identity
// component or a Protocol method name — the re-minted token itself is
// NEVER on the payload (CLAUDE.md §7 "never log secrets").
type AuthRotateTokenPayload struct {
	events.SafeSealed
	// Actor is the verified admin identity at the Protocol edge.
	Actor identity.Identity
	// Method is the Protocol method that carried the action.
	Method string
}

// Rotate handles the `auth.rotate_token` method. `verified` is the
// caller's verified JWT identity + scopes (from auth.Middleware);
// `req` is the decoded wire request. The surface asserts the body
// identity against the verified identity, gates on the `admin` scope,
// re-mints the token, and emits the audit event.
//
// Returns the wire response on success, or one of the package's typed
// sentinels on failure — the wire handler maps each onto a canonical
// Protocol Code.
func (s *RotateSurface) Rotate(ctx context.Context, verified Verified, req types.AuthRotateTokenRequest) (*types.AuthRotateTokenResponse, error) {
	// Identity is mandatory at the Protocol edge (RFC §5.5, CLAUDE.md
	// §6 rule 9). The verified identity already validated clean against
	// identity.Validate — defend anyway in case a non-middleware path
	// hands a zero Verified.
	if err := identity.Validate(verified.Identity); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRotateIdentityRequired, err)
	}

	// Defence-in-depth: the body's identity scope MUST agree with the
	// verified-JWT identity. A caller cannot present a valid token for
	// one identity and a body claiming another.
	if !bodyIdentityMatches(req.Identity, verified.Identity) {
		return nil, ErrRotateIdentityMismatch
	}

	// Admin gate (D-079). The closed two-scope set is the only admit
	// surface; `auth.rotate_token` gates on `admin` specifically.
	if !hasScopeIn(verified.Scopes, ScopeAdmin) {
		return nil, ErrRotateScopeRequired
	}

	now := time.Now().UTC()
	token, expiresAt, err := s.issuer.IssueToken(ctx, verified.Identity, verified.Scopes, now)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRotateIssueFailed, err)
	}

	// Audit the successful rotation (CLAUDE.md §7 rule 6). The emit
	// happens only AFTER the issuer succeeds — a failed mint never
	// reaches the bus.
	s.emitRotationAudit(ctx, verified.Identity)

	return &types.AuthRotateTokenResponse{
		NewToken:  token,
		ExpiresAt: expiresAt.UTC(),
	}, nil
}

// emitRotationAudit publishes the `audit.admin_scope_used` event for a
// successful rotation. The bus is optional; when unwired the rotation
// is logged at Info — never fully silent. The token is NEVER on the
// payload or in any log line (CLAUDE.md §7).
func (s *RotateSurface) emitRotationAudit(ctx context.Context, actor identity.Identity) {
	logAttrs := []any{
		slog.String("method", string(methods.MethodAuthRotateToken)),
		slog.String("tenant_id", actor.TenantID),
		slog.String("user_id", actor.UserID),
		slog.String("session_id", actor.SessionID),
	}
	if s.bus == nil {
		s.logger.InfoContext(ctx, "auth: token rotated (bus not wired — audit logged only)", logAttrs...)
		return
	}
	payload := AuthRotateTokenPayload{
		Actor:  actor,
		Method: string(methods.MethodAuthRotateToken),
	}
	// A redactor error means "do not emit" — log loudly, never publish
	// unredacted (parity with the Phase 73f tools admin-audit site).
	if _, err := s.redactor.Redact(ctx, payload); err != nil {
		s.logger.ErrorContext(ctx, "auth: token-rotation audit redaction failed — event NOT published",
			append(logAttrs, slog.String("error", err.Error()))...)
		return
	}
	ev := events.Event{
		Type: events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{
			Identity: actor,
		},
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.logger.WarnContext(ctx, "auth: token-rotation audit event publish failed",
			append(logAttrs, slog.String("error", err.Error()))...)
		return
	}
	s.logger.InfoContext(ctx, "auth: token rotated", logAttrs...)
}

// bodyIdentityMatches reports whether the wire IdentityScope agrees
// with the verified identity. An empty body component is treated as
// "elided — fill from the verified identity" and matches; a non-empty
// component must equal the verified one exactly.
func bodyIdentityMatches(body types.IdentityScope, verified identity.Identity) bool {
	if body.Tenant != "" && body.Tenant != verified.TenantID {
		return false
	}
	if body.User != "" && body.User != verified.UserID {
		return false
	}
	if body.Session != "" && body.Session != verified.SessionID {
		return false
	}
	return true
}

// hasScopeIn reports whether want is in the verified scope slice.
func hasScopeIn(scopes []Scope, want Scope) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
