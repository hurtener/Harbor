// Package auth is the Harbor Protocol's JWT validation surface — the
// Phase 61 transport-edge cryptographic identity check that turns the
// Phase 60 wire transports' trust-based identity carriers into verified
// ones (RFC §5.5: "JWT, asymmetric algorithms only ... the triple
// (tenant, user, session) is in the JWT claims; the Protocol rejects any
// request without an identity scope").
//
// # The two-piece surface
//
// auth ships two pieces that compose:
//
//   - Validator (this file) — transport-agnostic. Takes a raw JWT
//     string, parses + verifies it against a configured KeySet, asserts
//     the signing algorithm is in the asymmetric allowlist, extracts
//     the (tenant, user, session) claim triple + scope claims, and
//     returns a Verified struct. Every failure is one of the eight
//     typed sentinels.
//
//   - Middleware (middleware.go) — net/http binding. Reads the
//     `Authorization: Bearer <token>` header, calls Validator.Validate,
//     injects the verified identity + scopes into r.Context() (via
//     identity.With + auth.WithScopes), and calls the wrapped handler.
//     A failure writes a JSON Protocol error body with HTTP 401 (or
//     403 for a scope mismatch) and emits an audit-redacted slog.Warn.
//
// # The asymmetric-algorithm allowlist (CLAUDE.md §7 rule 1)
//
// Six algorithms — three RSA + three ECDSA — are accepted:
//
//	RS256 / RS384 / RS512      ECDSA-P-256/384/521 = ES256 / ES384 / ES512
//
// HS* (HMAC) and `none` are rejected at the **parser level** via
// `jwt.WithValidMethods`, BEFORE the Keyfunc is consulted — so the
// classical algorithm-confusion CVE family (an `HS256` token signed
// with an `RS256` public key as the HMAC secret) is structurally
// impossible. The security_test.go suite pins this.
//
// # The Protocol identity claim shape
//
// JWT claims map onto identity.Identity by name:
//
//	{
//	    "iss":    "https://idp.example.com",  // optional, audited
//	    "sub":    "user-12345",               // optional, audited
//	    "aud":    "harbor-runtime",           // optional, validated
//	    "exp":    1746662400,                 // mandatory
//	    "nbf":    1746576000,                 // optional
//	    "tenant": "tenant-acme",              // mandatory
//	    "user":   "user-12345",               // mandatory
//	    "session":"sess-01HX...",             // mandatory
//	    "scopes": ["admin", "console:fleet"]  // optional
//	}
//
// The triple (tenant, user, session) is mandatory — a missing claim
// returns ErrIdentityClaimMissing, which the middleware surfaces as a
// 401 with the canonical CodeIdentityRequired Protocol code. Scopes
// are optional — a token with no scopes is still authenticated, just
// not entitled to elevated subscriptions.
//
// # Concurrent reuse (D-025)
//
// Validator is a compiled artifact: the KeySet, the parser
// configuration, the clock, and the redactor are set once at
// construction and never mutated. Validate holds no per-call state on
// the struct — every per-call value lives on the function's stack /
// the returned Verified. One Validator is safe to share across N
// concurrent Validate goroutines; concurrent_test.go pins N≥120 under
// -race.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// AllowedAlgorithms is the asymmetric-algorithm allowlist Harbor
// enforces (CLAUDE.md §7 rule 1). Six algorithms — three RSA-PKCS#1v1.5
// (RS256/RS384/RS512) and three ECDSA (ES256/ES384/ES512). HS* and
// `none` are rejected at the parser level via jwt.WithValidMethods.
//
// The list is exported (a) so tests pin it, (b) so an operator
// inspecting the binary can confirm the surface, (c) so a later phase
// adding a JWKS driver inherits the same list.
var AllowedAlgorithms = []string{
	"RS256", "RS384", "RS512",
	"ES256", "ES384", "ES512",
}

// Sentinel errors. Callers compare via errors.Is.
//
// Each rejection path returns exactly one sentinel, wrapped with
// context — so a middleware mapping a Validate error onto a Protocol
// error code branches on the sentinel, not on the wrapped detail.
var (
	// ErrTokenMissing — the request carried no JWT (the Authorization
	// header was absent or empty). Mapped onto CodeIdentityRequired
	// (HTTP 401) by the middleware.
	ErrTokenMissing = errors.New("auth: token missing")
	// ErrTokenMalformed — the JWT was not a valid three-segment string
	// or could not be base64-decoded. Mapped onto CodeAuthRejected
	// (HTTP 401).
	ErrTokenMalformed = errors.New("auth: token malformed")
	// ErrAlgNotAllowed — the JWT's `alg` header was not in the
	// asymmetric allowlist (HS*, `none`, or anything else). The parser
	// rejects this BEFORE the Keyfunc is consulted, so an algorithm-
	// confusion attack is structurally impossible. Mapped onto
	// CodeAuthRejected (HTTP 401).
	ErrAlgNotAllowed = errors.New("auth: signing algorithm not in asymmetric allowlist")
	// ErrSignatureInvalid — the JWT's signature did not verify against
	// the resolved key. Mapped onto CodeAuthRejected (HTTP 401).
	ErrSignatureInvalid = errors.New("auth: signature invalid")
	// ErrTokenExpired — the JWT's `exp` claim is in the past relative
	// to the validator's clock. Mapped onto CodeAuthRejected (HTTP 401).
	ErrTokenExpired = errors.New("auth: token expired")
	// ErrTokenNotYetValid — the JWT's `nbf` claim is in the future
	// relative to the validator's clock. Mapped onto CodeAuthRejected
	// (HTTP 401).
	ErrTokenNotYetValid = errors.New("auth: token not yet valid")
	// ErrUnknownKey — the JWT's `kid` header did not resolve to a
	// public key in the configured KeySet. Mapped onto
	// CodeAuthRejected (HTTP 401).
	ErrUnknownKey = errors.New("auth: kid does not resolve in key set")
	// ErrIdentityClaimMissing — the JWT verified but its claims did not
	// carry the mandatory (tenant, user, session) triple. Mapped onto
	// CodeIdentityRequired (HTTP 401) — RFC §5.5: "the Protocol rejects
	// any request without an identity scope."
	ErrIdentityClaimMissing = errors.New("auth: identity claim missing")
	// ErrAudienceMismatch — the JWT's `aud` claim did not match the
	// validator's configured audience (when WithAudience was supplied).
	// Mapped onto CodeAuthRejected (HTTP 401).
	ErrAudienceMismatch = errors.New("auth: audience mismatch")
	// ErrIssuerMismatch — the JWT's `iss` claim did not match the
	// validator's configured issuer (when WithIssuer was supplied).
	// Mapped onto CodeAuthRejected (HTTP 401).
	ErrIssuerMismatch = errors.New("auth: issuer mismatch")
)

// ErrMisconfigured — NewValidator was called with a nil KeySet. A
// validator without a key source cannot verify any token; fail closed
// rather than building one that rejects everything (CLAUDE.md §5).
var ErrMisconfigured = errors.New("auth: NewValidator missing a mandatory dependency")

// KeySet maps a JWT `kid` header to the public key + algorithm name to
// verify the token's signature with. Implementations MUST be safe for
// concurrent reads — the Validator calls KeyByID on every Validate.
//
// The static implementation suffices for V1 + the `harbor dev`
// dev-token use case. A later phase can ship a JWKS driver that
// auto-refreshes from a URL behind the same interface — additive, no
// reshape.
type KeySet interface {
	// KeyByID returns the public key + the algorithm name for kid.
	// alg MUST be one of AllowedAlgorithms; an alg outside the
	// allowlist is treated as ErrUnknownKey by the Validator (the
	// allowlist gate is at the parser, not the KeySet, but a KeySet
	// that returned an HMAC key would be rejected here as well).
	//
	// Returning a wrapped ErrUnknownKey signals the kid is not known.
	// Any other error is wrapped as ErrUnknownKey by the Validator.
	KeyByID(kid string) (key crypto.PublicKey, alg string, err error)
}

// Verified is the result of a successful Validate call.
type Verified struct {
	// Identity is the (tenant, user, session) triple extracted from the
	// JWT's mandatory claims. Validates clean against identity.Validate
	// — the Validator already ran that check.
	Identity identity.Identity
	// Scopes is the verified scope set the JWT carried. May be empty;
	// a token with no scopes is still authenticated, just not entitled
	// to any elevated subscription. Membership is checked via
	// auth.HasScope.
	Scopes []Scope
	// Subject is the JWT's `sub` claim, if present. Audited; never used
	// as an isolation principal (the triple is the isolation key).
	Subject string
	// Issuer is the JWT's `iss` claim, if present. Audited.
	Issuer string
}

// validatorConfig holds the optional knobs NewValidator threads into
// the Validator. Set once at construction; never mutated after.
type validatorConfig struct {
	issuer   string
	audience string
	now      func() time.Time
	logger   *slog.Logger
	redactor audit.Redactor
	bus      events.EventBus
}

// Option configures NewValidator.
type Option func(*validatorConfig)

// WithIssuer sets the expected JWT `iss` claim. A token whose `iss`
// claim does not match is rejected with ErrIssuerMismatch. An empty
// configured issuer disables the check.
func WithIssuer(iss string) Option {
	return func(c *validatorConfig) { c.issuer = iss }
}

// WithAudience sets the expected JWT `aud` claim. A token whose `aud`
// claim does not contain the expected value is rejected with
// ErrAudienceMismatch. An empty configured audience disables the check.
func WithAudience(aud string) Option {
	return func(c *validatorConfig) { c.audience = aud }
}

// WithClock overrides the validator's clock — used by tests to drive
// expiration / nbf checks deterministically. A nil clock keeps the
// default (time.Now).
func WithClock(now func() time.Time) Option {
	return func(c *validatorConfig) {
		if now != nil {
			c.now = now
		}
	}
}

// WithLogger sets the slog.Logger the validator emits redacted audit
// records to. A nil logger keeps slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *validatorConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithRedactor sets the audit.Redactor the validator runs audit
// payloads through before logging. The redactor is **mandatory** —
// NewValidator fails closed with ErrMisconfigured when this option is
// not supplied (CLAUDE.md §7 rule 6: "every payload goes through
// audit.Redactor"; CLAUDE.md §13 "Test stubs as production defaults
// on operator-facing seams"). A nil redactor is treated as
// "unsupplied" and is rejected by NewValidator the same way.
func WithRedactor(r audit.Redactor) Option {
	return func(c *validatorConfig) {
		if r != nil {
			c.redactor = r
		}
	}
}

// WithEventBus wires an events.EventBus into the Validator so the
// audit emit on every rejection ALSO publishes a canonical
// `auth.rejected` event onto the bus (PR #91 amendment to D-079).
// The bus is OPTIONAL — when not supplied (or nil), rejections still
// emit a structured slog.Warn through the configured Redactor; the
// bus emit is an additive observability surface that lets a Console
// subscribe to auth rejections through the Protocol's canonical
// event channel rather than scraping logs.
//
// Production wiring (the registry-path NewMux path) SHOULD inject
// the bus so the Console sees rejections; the test-only escape
// hatch (a Validator constructed without a bus) keeps the existing
// per-package tests pinning the slog-only contract.
func WithEventBus(b events.EventBus) Option {
	return func(c *validatorConfig) {
		if b != nil {
			c.bus = b
		}
	}
}

// Validator is the JWT validation surface. Construct via NewValidator;
// do not construct directly. One Validator is safe to share across N
// concurrent Validate goroutines (D-025).
type Validator interface {
	// Validate parses + verifies the rawToken JWT and returns the
	// extracted identity + scopes. Every error wraps one of the
	// package's typed sentinels — callers compare via errors.Is.
	Validate(ctx context.Context, rawToken string) (Verified, error)
}

// jwtValidator is the concrete Validator implementation backed by
// golang-jwt/jwt/v5. All fields are immutable after NewValidator
// returns.
type jwtValidator struct {
	keys     KeySet
	parser   *jwt.Parser
	issuer   string
	audience string
	now      func() time.Time
	logger   *slog.Logger
	redactor audit.Redactor
	bus      events.EventBus // nil ⇒ slog-only audit emit
}

// NewValidator builds a JWT Validator over the supplied KeySet.
//
// Both the KeySet AND an audit.Redactor (via WithRedactor) are
// mandatory. A nil KeySet — or omission of WithRedactor — fails loud
// with ErrMisconfigured rather than building a validator that would
// reject every token (KeySet) or log raw payloads unredacted
// (Redactor; CLAUDE.md §7 rule 6 — "every payload goes through
// audit.Redactor" — and CLAUDE.md §13 "Test stubs as production
// defaults on operator-facing seams"). Production callers wire
// `audit/drivers/patterns.New()` as the redactor; tests wire a
// real or test-local Redactor via the `auth_test` package or a
// _test.go-local stub.
//
// The returned Validator is immutable after construction (D-025) and
// safe for concurrent use by N goroutines.
func NewValidator(keys KeySet, opts ...Option) (Validator, error) {
	if keys == nil {
		return nil, fmt.Errorf("%w: KeySet is nil", ErrMisconfigured)
	}

	cfg := validatorConfig{
		now:    time.Now,
		logger: slog.Default(),
		// redactor is intentionally left nil here so the post-opts
		// guard below catches the "WithRedactor not supplied" case.
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.redactor == nil {
		return nil, fmt.Errorf("%w: WithRedactor is required (CLAUDE.md §7 rule 6 — every payload goes through audit.Redactor)", ErrMisconfigured)
	}

	// jwt.WithValidMethods is the load-bearing parser-level allowlist:
	// it rejects HS* and `none` BEFORE the Keyfunc is consulted, so the
	// classical algorithm-confusion CVE family is structurally
	// impossible (a token with `alg: HS256` never reaches a keyfunc
	// that would otherwise hand it the RSA public key as the HMAC
	// secret). CLAUDE.md §7 rule 1 + §13 ban HS* / `none`.
	parser := jwt.NewParser(
		jwt.WithValidMethods(AllowedAlgorithms),
		// We do our own exp/nbf checks against the configurable clock
		// (WithClock), so disable the parser's built-in time gate.
		jwt.WithoutClaimsValidation(),
	)

	return &jwtValidator{
		keys:     keys,
		parser:   parser,
		issuer:   cfg.issuer,
		audience: cfg.audience,
		now:      cfg.now,
		logger:   cfg.logger,
		redactor: cfg.redactor,
		bus:      cfg.bus,
	}, nil
}

// Validate parses + verifies rawToken and returns the extracted
// identity + scopes. Every error wraps a typed sentinel.
//
// The audit emit is fail-loud: every rejection path emits a structured
// slog.Warn through the configured Redactor with `kid`, `iss`, `sub`,
// `reason` — never the raw token, never the claims body verbatim
// (CLAUDE.md §5 + §7 rule 7).
func (v *jwtValidator) Validate(ctx context.Context, rawToken string) (Verified, error) {
	if strings.TrimSpace(rawToken) == "" {
		v.audit(ctx, "", "", "", ErrTokenMissing)
		return Verified{}, ErrTokenMissing
	}

	// keyfunc resolves the kid → public key. The parser has already
	// asserted the alg is in AllowedAlgorithms by the time keyfunc is
	// called, so an HS256 token never reaches keyfunc — the parser
	// rejected it first.
	var kidSeen string
	keyfunc := func(t *jwt.Token) (any, error) {
		// Belt-and-braces: re-check the alg here in case a future
		// jwt.Parser change loosens the gate. CLAUDE.md §7 rule 1.
		if !isAllowedMethod(t.Method) {
			return nil, ErrAlgNotAllowed
		}
		kid, _ := t.Header["kid"].(string)
		kidSeen = kid
		key, alg, err := v.keys.KeyByID(kid)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnknownKey, err)
		}
		// Defence in depth: if the KeySet returns an algorithm name
		// that disagrees with the JWT header's alg, treat it as
		// unknown — a kid that maps to a different alg is a key-
		// confusion vector (CVE-2022-29217-flavored).
		if alg != "" && alg != t.Method.Alg() {
			return nil, fmt.Errorf("%w: kid %q resolves to alg %q, token uses %q",
				ErrUnknownKey, kid, alg, t.Method.Alg())
		}
		// Final structural check: the resolved key MUST be an
		// asymmetric public key shape (RSA or ECDSA). A symmetric
		// HMAC secret would be a confusion vector.
		switch key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey:
			return key, nil
		default:
			return nil, fmt.Errorf("%w: kid %q resolves to a non-asymmetric key type",
				ErrUnknownKey, kid)
		}
	}

	claims := jwt.MapClaims{}
	tok, err := v.parser.ParseWithClaims(rawToken, claims, keyfunc)
	if err != nil {
		mapped := mapParserError(err)
		v.audit(ctx, kidSeen, "", "", mapped)
		return Verified{}, mapped
	}
	if tok == nil || !tok.Valid {
		v.audit(ctx, kidSeen, "", "", ErrSignatureInvalid)
		return Verified{}, ErrSignatureInvalid
	}

	// Pull `iss` + `sub` for audit + optional issuer-match.
	iss, _ := claims["iss"].(string)
	sub, _ := claims["sub"].(string)

	// Issuer / audience checks. Both are optional (empty configured
	// value disables the check). When set, a mismatch fails loud.
	if v.issuer != "" && iss != v.issuer {
		v.audit(ctx, kidSeen, iss, sub, ErrIssuerMismatch)
		return Verified{}, fmt.Errorf("%w: expected %q, got %q", ErrIssuerMismatch, v.issuer, iss)
	}
	if v.audience != "" && !audienceContains(claims["aud"], v.audience) {
		v.audit(ctx, kidSeen, iss, sub, ErrAudienceMismatch)
		return Verified{}, fmt.Errorf("%w: expected %q", ErrAudienceMismatch, v.audience)
	}

	// Time-based claim validation against the configurable clock.
	// `exp` is mandatory (token without an expiry is rejected as
	// expired); `nbf` is optional (absent = "valid since the dawn of
	// time" — standard JWT behaviour).
	now := v.now()
	expFloat, hasExp := claims["exp"].(float64)
	if !hasExp {
		v.audit(ctx, kidSeen, iss, sub, ErrTokenExpired)
		return Verified{}, fmt.Errorf("%w: token has no exp claim", ErrTokenExpired)
	}
	if exp := time.Unix(int64(expFloat), 0); !now.Before(exp) {
		v.audit(ctx, kidSeen, iss, sub, ErrTokenExpired)
		return Verified{}, fmt.Errorf("%w: exp=%s, now=%s", ErrTokenExpired, exp.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if nbfFloat, hasNbf := claims["nbf"].(float64); hasNbf {
		if nbf := time.Unix(int64(nbfFloat), 0); now.Before(nbf) {
			v.audit(ctx, kidSeen, iss, sub, ErrTokenNotYetValid)
			return Verified{}, fmt.Errorf("%w: nbf=%s, now=%s", ErrTokenNotYetValid, nbf.Format(time.RFC3339), now.Format(time.RFC3339))
		}
	}

	// Identity-mandatory at the Protocol edge (RFC §5.5, CLAUDE.md §6
	// rule 9). Each component must be a non-empty string claim.
	tenant, _ := claims["tenant"].(string)
	user, _ := claims["user"].(string)
	session, _ := claims["session"].(string)
	id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
	if err := identity.Validate(id); err != nil {
		v.audit(ctx, kidSeen, iss, sub, ErrIdentityClaimMissing)
		return Verified{}, fmt.Errorf("%w: %v", ErrIdentityClaimMissing, err)
	}

	scopes := extractScopes(claims["scopes"])

	return Verified{
		Identity: id,
		Scopes:   scopes,
		Subject:  sub,
		Issuer:   iss,
	}, nil
}

// audit emits a redacted slog.Warn for a rejection AND (when
// WithEventBus was supplied) publishes the canonical
// `auth.rejected` event onto the bus. The raw token is NEVER passed
// in — only the kid (already a public header), the iss / sub
// (audited identifiers), and the rejection reason. The Redactor
// runs the payload as defence-in-depth in case a custom kid happens
// to match a secret-shaped pattern.
//
// PR #91 / D-082: the bus emit was added per the Wave 10 audit's
// WARN-3 so a Console subscribing to the canonical event bus sees
// auth rejections alongside every other rejection-class signal.
// The bus is optional; the slog emit is unconditional.
func (v *jwtValidator) audit(ctx context.Context, kid, iss, sub string, reason error) {
	payload := map[string]any{
		"kid":    kid,
		"iss":    iss,
		"sub":    sub,
		"reason": reason.Error(),
	}
	red, redErr := v.redactor.Redact(ctx, payload)
	if redErr != nil {
		// Redactor failed loud: emit the bare reason without the
		// payload — never silently degrade. CLAUDE.md §5.
		v.logger.WarnContext(ctx, "auth: jwt rejected (redactor failed; bare reason emitted)",
			slog.String("reason", reason.Error()),
			slog.String("redactor_error", redErr.Error()))
		return
	}
	v.logger.WarnContext(ctx, "auth: jwt rejected", slog.Any("audit", red))

	// Bus emit (optional). The auth.rejected event uses a sentinel
	// identity triple (authEdgeIdentity) because a rejected request
	// has no verified identity yet — events.ValidateEvent requires
	// the full triple, so the event is keyed to the auth-edge
	// surface and a Console subscribes via the Admin filter or via
	// the sentinel tenant directly. The payload IS SafePayload —
	// no caller-controlled bytes — but defence-in-depth, the
	// fields are derived from the redacted view (the redactor's
	// output) rather than the raw inputs.
	if v.bus == nil {
		return
	}
	reasonStr := redactedString(red, "reason", reason.Error())
	kidStr := redactedString(red, "kid", kid)
	issStr := redactedString(red, "iss", iss)
	subStr := redactedString(red, "sub", sub)
	ev := events.Event{
		Type:       EventTypeAuthRejected,
		Identity:   authEdgeIdentity,
		OccurredAt: v.now(),
		Payload: AuthRejectedPayload{
			Reason:  reasonStr,
			KID:     kidStr,
			Issuer:  issStr,
			Subject: subStr,
		},
	}
	// A Publish failure here MUST NOT propagate — auth-edge audit
	// is best-effort observability and the rejection HTTP response
	// has already been determined upstream. We log the failure
	// loudly (CLAUDE.md §5) so an operator sees the bus drift.
	if err := v.bus.Publish(ctx, ev); err != nil {
		v.logger.WarnContext(ctx, "auth: failed to publish auth.rejected event",
			slog.String("publish_error", err.Error()),
			slog.String("reason", reasonStr))
	}
}

// redactedString returns key's string value from the redactor's
// output when present (and a string); falls back to the original
// fallback value otherwise. This protects against a custom redactor
// that returns a wholly-replaced object — the bus emit prefers the
// redacted view, but never crashes when the shape is unexpected.
func redactedString(red any, key, fallback string) string {
	if m, ok := red.(map[string]any); ok {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return fallback
}

// authEdgeIdentity is the sentinel identity triple under which
// `auth.rejected` events publish. A rejected request has no verified
// identity — `events.ValidateEvent` requires the full triple, so the
// audit emit lives under this surface and a Console subscribes by
// matching the tenant (`harbor-auth`) or by using the Admin filter.
// The values are documented constants — no operator data leaks in.
var authEdgeIdentity = identity.Quadruple{
	Identity: identity.Identity{
		TenantID:  "harbor-auth",
		UserID:    "auth-edge",
		SessionID: "auth-edge",
	},
}

// extractScopes pulls the `scopes` JWT claim into the typed Scope set.
// Accepts either a JSON array of strings or a single space-separated
// string (the OAuth 2.0 `scope` convention). An unknown / empty value
// is silently the empty set — a token with no scopes is authenticated
// just not entitled to any elevated subscription.
func extractScopes(raw any) []Scope {
	switch v := raw.(type) {
	case []any:
		out := make([]Scope, 0, len(v))
		for _, s := range v {
			if str, ok := s.(string); ok && str != "" {
				out = append(out, Scope(str))
			}
		}
		return out
	case []string:
		out := make([]Scope, 0, len(v))
		for _, s := range v {
			if s != "" {
				out = append(out, Scope(s))
			}
		}
		return out
	case string:
		fields := strings.Fields(v)
		out := make([]Scope, 0, len(fields))
		for _, s := range fields {
			out = append(out, Scope(s))
		}
		return out
	default:
		return nil
	}
}

// audienceContains reports whether the JWT `aud` claim contains want.
// `aud` may be a string OR an array of strings per RFC 7519 §4.1.3.
func audienceContains(raw any, want string) bool {
	switch v := raw.(type) {
	case string:
		return v == want
	case []any:
		for _, s := range v {
			if str, ok := s.(string); ok && str == want {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if s == want {
				return true
			}
		}
	}
	return false
}

// isAllowedMethod reports whether method's name is in the asymmetric
// allowlist. Used as a belt-and-braces check inside the keyfunc — the
// parser has already enforced this, but a future change to
// jwt.WithValidMethods semantics would not silently weaken Harbor's
// gate.
func isAllowedMethod(method jwt.SigningMethod) bool {
	if method == nil {
		return false
	}
	name := method.Alg()
	for _, allowed := range AllowedAlgorithms {
		if allowed == name {
			return true
		}
	}
	return false
}

// mapParserError translates a golang-jwt/jwt/v5 parse error into one of
// our typed sentinels. The mapping order is deliberate:
//
//  1. Our own sentinels (returned by the keyfunc) come FIRST — the
//     parser wraps them in ErrTokenUnverifiable, but errors.Is sees
//     through the wrap. A keyfunc-returned ErrAlgNotAllowed /
//     ErrUnknownKey must be honoured AS-IS.
//  2. WithValidMethods produces "signing method X is invalid" wrapped
//     in ErrTokenSignatureInvalid; recognise it by message and route
//     to ErrAlgNotAllowed (the underlying cause is "alg outside the
//     allowlist", which is more specific than "signature invalid").
//  3. `alg: none` produces NoneSignatureTypeDisallowedError ("'none'
//     signature type is not allowed") wrapped in ErrTokenUnverifiable;
//     route to ErrAlgNotAllowed.
//  4. Genuine signature failures (ErrTokenSignatureInvalid that did
//     NOT match the WithValidMethods message) → ErrSignatureInvalid.
//  5. Unverifiable / malformed → ErrTokenMalformed (the fall-through
//     for an unparseable JWT).
func mapParserError(err error) error {
	// (1) honour our keyfunc-returned sentinels first — they are
	// wrapped through ErrTokenUnverifiable but errors.Is unwraps.
	if errors.Is(err, ErrAlgNotAllowed) {
		return ErrAlgNotAllowed
	}
	if errors.Is(err, ErrUnknownKey) {
		return ErrUnknownKey
	}
	// (2) WithValidMethods wraps with ErrTokenSignatureInvalid + the
	// "signing method ... is invalid" message — recognise it.
	if strings.Contains(err.Error(), "signing method") && strings.Contains(err.Error(), "is invalid") {
		return ErrAlgNotAllowed
	}
	// (3) `alg: none` rejected by SigningMethodNone.Verify — wrapped
	// in ErrTokenUnverifiable with the canonical message.
	if strings.Contains(err.Error(), "'none' signature type is not allowed") {
		return ErrAlgNotAllowed
	}
	// (4) malformed JWT (cannot split into three parts, base64 decode
	// failure, etc.) — recognised by the canonical sentinel.
	if errors.Is(err, jwt.ErrTokenMalformed) {
		return ErrTokenMalformed
	}
	// (5) genuine signature failure.
	if errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		return ErrSignatureInvalid
	}
	// (6) unverifiable (no keyfunc ran, key shape wrong) — usually a
	// signature-side failure semantically, but if we got here it is
	// not one of our sentinels, so map to ErrSignatureInvalid as the
	// generic "could not verify the cryptographic signature" outcome.
	if errors.Is(err, jwt.ErrTokenUnverifiable) {
		return ErrSignatureInvalid
	}
	return ErrTokenMalformed
}
