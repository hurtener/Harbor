# Phase 61 — Protocol auth + identity-scope enforcement

## Summary

Phase 61 turns the Phase 60 wire transports' trust-based identity carriers
into **cryptographically verified** ones. A JWT validator + an
`http.Handler` middleware sit at the Phase 60 transport edge: every
request carries a JWT signed with one of the six asymmetric algorithms
(`RS256`/`RS384`/`RS512`/`ES256`/`ES384`/`ES512`); HS\* and `none` are
rejected at the parser level; the `(tenant, user, session)` triple flows
out of the JWT claims into the request `context.Context`; extended scope
claims (`admin`, `console:fleet`) gate cross-session/cross-tenant
subscriptions. Every rejection emits a redacted audit record. No
identity-downgrading knob.

## RFC anchor

- RFC §5.5
- RFC §4.2

## Briefs informing this phase

- brief 09
- brief 07
- brief 06

## Brief findings incorporated

- brief 09 §"Identity-scoped JWT enforcement on resume" + §"Admin-scope
  authz on agent-bound flows": the OAuth callback handler verifies the
  JWT's identity scope matches the pause record's identity scope before
  resuming; agent-bound flows require admin scope on the agent's tenant.
  Phase 61 ships the JWT-scope verification primitive (the `Validator` +
  the middleware) that Phase 30's resume callback consumes; the verifier
  is identity-scoped (the triple) AND scope-claim-aware (`admin`,
  `console:fleet`).
- brief 07 §"the runtime owns the protocol it speaks": auth is a Protocol
  surface obligation, not an HTTP-framework feature — the Phase 61
  middleware is a thin wrapper over a transport-agnostic `Validator` the
  Protocol owns. The middleware reads from `Authorization: Bearer …`,
  validates via the `Validator`, and injects identity + scopes into
  `r.Context()`. The Phase 60 handlers stay unchanged.
- brief 06 §"server-enforced identity": the SSE filter (`events.Filter`)
  is server-built from the verified identity, NEVER from request payload.
  Cross-tenant fan-in (`Filter.Admin = true`) is gated on the verified
  `admin` / `console:fleet` scope claim — RFC §6.13 admin subscriptions
  plus the existing `events.ErrAdminScopeRequired` sentinel.

## Findings I'm departing from (if any)

None.

## Goals

- A transport-agnostic `Validator` that parses + verifies a JWT against a
  configured public-key set, asserts the signing algorithm is in the
  six-element asymmetric allowlist, extracts the `(tenant, user,
  session)` claim triple, and extracts scope claims (`admin`,
  `console:fleet`). HS\* and `alg:none` are rejected at the parser layer
  via `jwt.WithValidMethods` — never post-parse, never silently coerced.
- An `http.Handler` middleware that reads `Authorization: Bearer <token>`,
  calls the `Validator`, populates `r.Context()` with the verified
  identity (via `identity.With`) + the verified scope set (via
  `auth.WithScopes`), and continues to the wrapped handler. A missing or
  invalid token is rejected with HTTP 401 + a Protocol JSON error body
  (`identity_required` / a new `auth_rejected` code).
- Audit-on-rejection: every rejected request emits a structured `slog.Warn`
  via the Redactor with `kid`, `iss`, `reason`, and the masked token
  prefix only — never the raw token, never the claims body.
- Compose with Phase 60: `transports.NewMux` gains a `WithValidator`
  option; when set, both `control` and `stream` handlers are wrapped in
  the auth middleware. The Phase 60 handlers' identity-from-headers /
  identity-from-body code paths become the unauthenticated-fallback the
  middleware already replaces — so their existing behaviour is preserved
  for tests that still mount them bare.
- The SSE handler honours an optional `?admin=1` query param: when
  present, `events.Filter.Admin = true` is set ONLY if the verified
  scope set carries `admin` or `console:fleet`; otherwise the request is
  rejected `403 scope_mismatch`.
- The `Validator` is a D-025 reusable artifact — one instance is shared
  across N concurrent requests under `-race`.

## Non-goals

- A full JWT issuer / mint surface — Phase 61 is verification-only. Token
  issuance is operator-supplied (an upstream IdP, an OAuth provider,
  Phase 30's flow). The dev-token mint helper for `harbor dev` is Phase
  64.
- JWKS auto-refresh / JWKS-URI fetch. Phase 61 takes a static
  `KeySet` interface (`KeyByID(kid) (crypto.PublicKey, alg, error)`). A
  later phase can ship a `jwks` driver that auto-refreshes from a URL
  via the same interface — it stays additive.
- OAuth callback handler / token-exchange dance — Phase 30 (tool-side
  OAuth) lands its own callback Protocol method. Phase 61 supplies the
  scope-verification primitive that callback uses; the callback's
  Protocol-method wiring is Phase 30's surface.
- Console pages that render the verified identity. Console phases are
  later; Phase 61 only ensures the Protocol surface is verified.
- Bumping `ProtocolVersion`. Phase 61 adds neither a new method name nor
  a breaking wire change — adding the bearer header + a new error code
  is additive within the existing `0.1.0` surface.

## Acceptance criteria

- [ ] `internal/protocol/auth` exposes a `Validator` whose `Validate(ctx,
      raw string) (Verified, error)` parses + verifies a JWT and returns
      the extracted identity + scope set; HS\* and `alg:none` are
      rejected by `jwt.WithValidMethods` at the parser, never reach the
      claim extractor.
- [ ] The validator rejects every of: empty / malformed token; signature
      verification failure; expired token (`exp` past); not-yet-valid
      token (`nbf` future); unknown / missing `kid`; missing tenant /
      user / session claim. Each rejection wraps a typed sentinel
      (`ErrTokenMissing`, `ErrTokenMalformed`, `ErrAlgNotAllowed`,
      `ErrSignatureInvalid`, `ErrTokenExpired`, `ErrTokenNotYetValid`,
      `ErrUnknownKey`, `ErrIdentityClaimMissing`).
- [ ] An `auth.Middleware(next http.Handler) http.Handler` reads
      `Authorization: Bearer <token>`, validates via the `Validator`,
      injects identity + scopes into `r.Context()`, and calls `next`. A
      rejection writes a JSON Protocol error body (`identity_required`
      for missing/incomplete identity claims; the new `auth_rejected`
      code for verification failure) with HTTP 401.
- [ ] Every rejection path passes the rejection reason + `kid` + `iss`
      through `audit.Redactor` and emits a `slog.Warn`; the raw token
      never appears in logs.
- [ ] `internal/protocol/errors` adds `CodeAuthRejected` (the new
      seventh… eighth Protocol error code; status maps to HTTP 401).
      No other package declares it.
- [ ] `internal/protocol/transports.NewMux` gains a `WithValidator`
      option; when supplied, both transport handlers are wrapped in
      `auth.Middleware`.
- [ ] The SSE handler reads scopes from ctx (via `auth.HasScope`) and
      allows `events.Filter.Admin = true` ONLY when the request's
      `?admin=1` query param is set AND the verified scope set carries
      `admin` or `console:fleet`. A `?admin=1` without the scope is
      rejected `403 scope_mismatch`.
- [ ] An integration test (`test/integration/phase61_auth_test.go`)
      wires the REAL `protocol.ControlSurface` + REAL in-mem
      `events.EventBus` behind `httptest.Server`, behind the auth
      middleware, with a real ES256 keypair: a valid token round-trips
      `start` over REST and observes the lifecycle event on its
      identity-scoped SSE stream; a missing-token, an HS256-signed
      token, an `alg:none` token, an expired token, an
      identity-claim-mismatch (token says tenant `t1`, body says
      tenant `t2`), and a `?admin=1` without the scope are each
      rejected with the expected status + code; identity propagation
      through every layer; runs under `-race`.
- [ ] A security suite (`internal/protocol/auth/security_test.go`)
      exercises algorithm-confusion attacks (HS256 token verified
      against an RS public key — the classical "alg confusion" CVE
      family), nested-token attacks, and a scope-escalation attempt
      (token without `admin` requesting `Filter.Admin = true`). Every
      attack is rejected; every rejection is audited.
- [ ] A D-025 concurrent-reuse test: N≥120 concurrent `Validator.Validate`
      calls against one shared validator under `-race`, distinct
      per-goroutine identity quadruples, no data races, no context bleed,
      no goroutine leak.
- [ ] `scripts/smoke/phase-61.sh` exercises the package + integration +
      security tests under `-race`, plus static guards (the `auth`
      sub-package exists; `transports.go` declares `WithValidator`; no
      Protocol error code redefined under `internal/protocol/auth`; the
      `auth` package does not import the Console). FAIL = 0.

## Files added or changed

```text
internal/protocol/auth/
  auth.go                  # Validator (interface + concrete) + KeySet + Verified + the eight sentinels
  auth_test.go             # parser-level alg rejection + the eight sentinels' coverage
  middleware.go            # auth.Middleware + WithScopes / HasScope ctx helpers
  middleware_test.go       # bearer parse, identity injection, audit-on-rejection
  scopes.go                # Scope type + the canonical scope constants (admin, console:fleet)
  scopes_test.go
  security_test.go         # algorithm-confusion, alg:none, scope-escalation, expired/nbf attacks
  concurrent_test.go       # D-025 N>=120 + goroutine-leak
  testdata/
    README.md              # documents the dummy keypairs, why they are dummy, regeneration recipe
    rs256_private.pem      # dummy RS256 keypair (test-only; documented as such)
    rs256_public.pem
    es256_private.pem      # dummy ES256 keypair (test-only)
    es256_public.pem
internal/protocol/errors/errors.go              # add CodeAuthRejected
internal/protocol/errors/errors_test.go         # IsValidCode coverage for the new code
internal/protocol/transports/transports.go      # WithValidator option + middleware composition
internal/protocol/transports/transports_test.go # WithValidator wiring assertions
internal/protocol/transports/control/status.go  # CodeAuthRejected -> 401
internal/protocol/transports/control/status_test.go
internal/protocol/transports/stream/stream.go   # ctx-first identity resolution + admin scope gate
internal/protocol/transports/stream/stream_test.go
test/integration/phase61_auth_test.go           # end-to-end with httptest + a real keypair
scripts/smoke/phase-61.sh
docs/plans/phase-61-protocol-auth.md
docs/decisions.md                               # D-079
docs/glossary.md                                # JWT bearer, asymmetric-algorithm allowlist, key set, scope claim
docs/plans/README.md                            # Phase 61 row Pending -> Shipped
README.md                                       # Status table Phase 61 -> Shipped
```

## Public API surface

```go
// internal/protocol/auth

type Validator interface {
    Validate(ctx context.Context, rawToken string) (Verified, error)
}

type Verified struct {
    Identity identity.Identity
    Scopes   []Scope
    Subject  string
    Issuer   string
}

type KeySet interface {
    KeyByID(kid string) (key crypto.PublicKey, alg string, err error)
}

type Validator interface { ... }
func NewValidator(keys KeySet, opts ...Option) (Validator, error)
type Option func(*validatorConfig)
func WithIssuer(iss string) Option
func WithAudience(aud string) Option
func WithClock(now func() time.Time) Option
func WithLogger(l *slog.Logger) Option
func WithRedactor(r audit.Redactor) Option

func Middleware(v Validator, opts ...MiddlewareOption) func(http.Handler) http.Handler
type MiddlewareOption func(*middlewareConfig)
func MWLogger(l *slog.Logger) MiddlewareOption

type Scope string
const (
    ScopeAdmin        Scope = "admin"
    ScopeConsoleFleet Scope = "console:fleet"
)
func WithScopes(ctx context.Context, scopes []Scope) context.Context
func ScopesFrom(ctx context.Context) []Scope
func HasScope(ctx context.Context, s Scope) bool

var (
    ErrTokenMissing         error
    ErrTokenMalformed       error
    ErrAlgNotAllowed        error
    ErrSignatureInvalid     error
    ErrTokenExpired         error
    ErrTokenNotYetValid     error
    ErrUnknownKey           error
    ErrIdentityClaimMissing error
)

// internal/protocol/transports

func WithValidator(v auth.Validator) Option // wraps both handlers in auth.Middleware
```

## Test plan

- **Unit:** `auth/auth_test.go` — every sentinel exercised against
  hand-built JWTs (HS256 token, alg:none token, expired, nbf-future,
  missing `kid`, missing identity claim, unknown `kid`, malformed token,
  empty token). `auth/middleware_test.go` — bearer parse (no header,
  malformed scheme, missing Bearer prefix), identity injection into
  ctx, the JSON Protocol error body shape on rejection. `auth/scopes_test.go`
  — `WithScopes` / `ScopesFrom` / `HasScope` round-trip + nil-context
  safety.
- **Integration:** `test/integration/phase61_auth_test.go` — real
  `ControlSurface` + real in-mem `EventBus` behind `httptest.Server`,
  middleware wired via `transports.NewMux` + `WithValidator`, real
  ES256 keypair from `testdata/`. Submit a `start` with a valid bearer;
  observe the `task.spawned` event on the identity-scoped SSE stream;
  prove the body's `IdentityScope` is the JWT-derived one (a body
  carrying a different identity is rejected). Failure modes (each
  asserted against a real `httptest.Server` round trip): no token →
  401 `auth_rejected`; HS256-signed token → 401 `auth_rejected`;
  `alg:none` token → 401 `auth_rejected`; expired token → 401
  `auth_rejected`; `?admin=1` without the scope → 403
  `scope_mismatch`. N≥10 concurrency stress.
- **Conformance:** N/A — single-source discipline gated by Phase 58's
  checker, which already covers the new `auth/` tree (no method strings
  / wire types redefined). The new error code `CodeAuthRejected` lands
  ONLY in `internal/protocol/errors/`; the canonical-codes registry
  there is the lockstep gate.
- **Concurrency / leak:** `auth/concurrent_test.go` — D-025 N≥120
  concurrent `Validator.Validate` calls against one shared validator
  under `-race`, with distinct per-goroutine identity quadruples;
  goroutine-leak test asserting `runtime.NumGoroutine` returns to
  baseline after all calls complete.
- **Security suite:** `auth/security_test.go` — five canonical attack
  shapes: (1) HS256 token signed with the RS public key as the HMAC
  secret (the classical alg-confusion CVE); (2) `alg:none` token; (3)
  scope-escalation (token without `admin` requesting cross-tenant
  fan-in); (4) `kid` substitution to a known-good key on a token signed
  by an unknown key; (5) crafted JWT whose `exp` claim is in the past.
  Each rejected; each audit-emitted; the raw token never appears in
  the audit body.

## Smoke script additions

- Runs `go test -race -count=1 ./internal/protocol/auth/...` (unit +
  security + D-025 + leak) and asserts pass.
- Runs `go test -race -count=1 -run TestE2E_Phase61 ./test/integration/...`
  (the REST + SSE + auth-middleware E2E) and asserts pass.
- Static guard: `internal/protocol/auth` exists with `auth.go` +
  `middleware.go` + `scopes.go`.
- Static guard: `transports.go` declares `WithValidator`.
- Single-source guard: no `protoerrors.Code(` constant constructed
  under `internal/protocol/auth/` (defence-in-depth over Phase 58 lint).
- Import-graph guard: the `auth` package does NOT import the Console.
- The live-HTTP assertions skip per the 404/405/501 → SKIP convention —
  `harbor dev` (Phase 64) is the server that mounts the mux + the
  middleware. Until then the auth surface is exercised via `httptest`.

## Coverage target

- `internal/protocol/auth`: 90%
- `internal/protocol/errors` (deepened): 90% (already 100%; preserved)
- `internal/protocol/transports` (deepened): 85% (preserved from Phase 60)
- `internal/protocol/transports/control`: 85% (preserved)
- `internal/protocol/transports/stream`: 85% (preserved)

## Dependencies

- Phase 58 — `internal/protocol` single-source layout + the
  `singlesource` checker that gates the new `auth/` tree.
- Phase 60 — `internal/protocol/transports/{control,stream}` the
  middleware wraps; `transports.NewMux` the validator threads into.
- Phase 01 — `internal/identity` for the `Identity` triple injection
  plus the `identity.With` ctx attachment helper.

## Risks / open questions

- **Algorithm confusion is a known JWT CVE family.** Mitigated by
  passing `jwt.WithValidMethods(["RS256","RS384","RS512","ES256","ES384",
  "ES512"])` to the parser — `golang-jwt/jwt/v5` rejects HS\* and
  `alg:none` at parse time when this option is set, BEFORE the
  `Keyfunc` is consulted. The security suite includes the classical
  HS256-verified-with-RS-public-key shape and asserts rejection.
- **JWT library footprint.** `github.com/golang-jwt/jwt/v5` is already
  in `go.sum` as an indirect dependency (pulled by `aws-sdk-go-v2`).
  Phase 61 promotes it to a direct dependency — no new module added,
  no new license surface, no new transitive footprint. `golang-jwt/jwt`
  is the de-facto Go JWT library, pure-Go, well-maintained, 2.5k stars,
  used by major Go projects (Caddy, Hashicorp Boundary). It satisfies
  RFC §10's "stdlib + the libraries listed in the RFC are the allowed
  surface; additions require RFC update" — and since it is already
  transitively present, the surface addition is treated as a
  documentation-only RFC expansion (noted in the §4.3 deviation block
  of the PR description).
- **Static `KeySet` interface for now.** A real deployment may want
  JWKS auto-refresh from a URL. Phase 61 ships a `KeySet` interface
  whose static implementation suffices for V1 + the `harbor dev`
  dev-token use case; a `jwks` driver behind the same interface stays
  additive for a later phase, no reshape.
- **Trust transfer from Phase 60 carrier headers.** Phase 60 reads
  identity from `X-Harbor-*` headers / request body's IdentityScope
  (trust-based). Phase 61 makes the JWT the source of truth: the
  middleware injects identity into ctx, and the existing handlers
  prefer ctx identity when present. The body's `IdentityScope` is
  validated to MATCH the JWT-derived identity — a body carrying a
  different identity than the JWT is rejected (defense in depth). When
  `WithValidator` is NOT set on `NewMux`, the Phase 60 trust-based
  posture is preserved exactly (so existing tests still pass).
- **No `harbor dev` server yet.** Same posture as Phase 60 — the
  auth surface is exercised via `httptest` in package + integration
  tests; the live-server smoke skips per the 404/405/501 convention
  until Phase 64.

## Glossary additions

- **JWT bearer** — the `Authorization: Bearer <token>` HTTP header
  carrying a Harbor Protocol JWT.
- **Asymmetric-algorithm allowlist** — Harbor's six accepted JWT
  signing algorithms: `RS256` / `RS384` / `RS512` / `ES256` / `ES384` /
  `ES512`. HS\* and `alg:none` are rejected at the parser level.
- **Key set** — the `auth.KeySet` interface mapping a JWT `kid` to its
  public key + algorithm; the static implementation is the V1 default,
  a JWKS-URI-fetching driver stays additive.
- **Scope claim** — a verified JWT scope value (`admin`,
  `console:fleet`) the Protocol consults when granting cross-session /
  cross-tenant subscriptions or fleet-control privileges.
- **Algorithm confusion attack** — a JWT CVE family where a token
  signed with one algorithm is verified against a key intended for
  another (e.g. an HS256 token verified using an RS256 public key as
  the HMAC secret). Mitigated by the parser-level allowlist.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test
      passes — the integration test exercises distinct identity
      quadruples on concurrent requests.
- [x] **If this phase builds a reusable artifact: concurrent-reuse test
      passes — N≥120 concurrent invocations against a single shared
      instance under `-race`.** `auth/concurrent_test.go` covers the
      `Validator`.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam: an integration test exists, wires real
      drivers end-to-end, asserts identity propagation, covers ≥1
      failure mode, runs under `-race`.**
      `test/integration/phase61_auth_test.go`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions
      entry filed — N/A, no departures.
