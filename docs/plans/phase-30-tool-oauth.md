# Phase 30 — Tool-side OAuth + HITL via pause/resume

## Summary

Ships Harbor's tool-side OAuth subsystem: a `TokenStore` typed wrapper plus an `OAuthProvider` that converges on the unified pause/resume primitive (Phase 50). On a missing/expired token, the provider returns an `ErrAuthRequired` typed sentinel; the runtime emits `tool.auth_required` and parks the run; the OAuth callback exchanges the code, persists an encrypted token, and resumes via the Coordinator. Both binding scopes (`ScopeUser` + `ScopeAgent`) ship as first-class peers per brief 09; PKCE + RFC 7591 dynamic client registration + metadata discovery are implemented; agent-bound tokens key on the Agent Registry's `agent_id` (D-059) without entering the isolation tuple.

## RFC anchor

- RFC §6.4
- RFC §3.3

## Briefs informing this phase

- brief 09

## Brief findings incorporated

- **brief 09 §"What to lift from bifrost":** both binding scopes ship as first-class peers (`ScopeUser` ↔ per-user OAuth, `ScopeAgent` ↔ server-level / agent-bound OAuth); the `MCPUserOAuthRequiredError` payload shape becomes Harbor's `ErrAuthRequired` typed sentinel with a `BindingScope` field added; PKCE + RFC 7591 dynamic registration + `.well-known/oauth-authorization-server` discovery are implemented from t=0; the "transparent refresh inside `AccessToken`" pattern is matched via a per-`(scope, subject, source)` single-flight gate.
- **brief 09 §"What to leave behind":** no `virtualKeyID`-style multi-tenancy indirection — Harbor's native identity is the gate; no separate `oauth_configs` table — configs live inline on the per-source attachment; no public `OAuth2TokenExchangeRequest/Response` types — wire-only / unexported; no bifrost-shaped "session token" indirection — Harbor's identity triple is the lookup key.
- **brief 09 §"What Harbor must add":** identity-mandatory enforcement (every method fails closed on a missing triple via `identity.Validate`); audit redaction (`Redactor.Redact` runs on every emission, even though `ToolAuthRequiredPayload` is SafePayload — defence in depth); encryption at rest (AES-256-GCM with a 4-byte version + 12-byte fresh-nonce envelope; KEK is mandatory at construction — empty/wrong-length KEK fails loud); concurrent-reuse contract (D-025) — N=128 concurrent flows under `-race`; single-flight refresh for agent-bound tokens shared across N sessions; admin-scope authz gate on `ScopeAgent` flows via `registry.HasControlScope` (Phase 53a's existing primitive).
- **brief 09 §"How the cycle composes with the unified pause/resume primitive":** the `OAuthProvider`'s job is just to return the typed error with the right payload; the runtime emits `tool.auth_required` (registered into the canonical event registry) and parks via the Phase 50 Coordinator. There is no parallel pause path for OAuth.

## Findings I'm departing from (if any)

- **TokenStore as a typed wrapper over `state.StateStore`, not a fresh §4.4 driver registry.** Brief 09 §"`TokenStore` interface" sketches three V1 drivers in the conventional §4.4 shape. Phase 30 instead follows the Phase 50 / Phase 53a precedent: a single concrete `*stateStoreTokenStore` consumes the existing `state.StateStore` §4.4 seam. The driver pluralism the master plan requires (in-mem / SQLite / Postgres) is inherited from the `state.StateStore` triad; the conformance suite runs the same `TokenStore` assertions against every `state.StateStore` driver to prove parity. This avoids the §13 two-parallel-implementations smell (a token-store driver registry AND a state-store driver registry, both saying "three V1 drivers, three init() blank-imports"). See D-067 (Phase 50 — same approach for pause checkpoints), D-068 (Phase 53a Agent Registry — same approach for agent records), and D-083 (this phase). Documented per §4.3.

## Goals

- A `TokenStore` interface persists OAuth access + refresh tokens with encryption at rest.
- An `OAuthProvider` interface drives the OAuth dance — `Token` / `InitiateFlow` / `CompleteFlow` / `Revoke` / `Close`.
- A typed `ErrAuthRequired` sentinel + audit-redacted `ToolAuthRequiredPayload` event carry "this tool needs OAuth" to the runtime and the Console.
- PKCE + RFC 7591 dynamic registration + metadata discovery exist in the V1 binary — the master-plan acceptance criteria.
- The unified pause/resume primitive (Phase 50) is the ONE pause path: OAuth never invents a parallel coordinator.
- Agent-bound tokens key on `agent_id` (Phase 53a / D-059) without entering the isolation tuple.

## Non-goals

- A live OAuth callback Protocol method — the Protocol-side callback handler lands in a later phase. Phase 30 ships the `CompleteFlow` API call that an HTTP handler will invoke.
- Tool-side approval gates — Phase 31 covers synchronous approve/reject HITL via the same pause coordinator (different payload shape).
- KEK rotation (post-V1 / Phase 91-ish). The encryption envelope carries a version header so future-rotation drivers can decrypt legacy records before re-encrypting, but the V1 binary takes one config-supplied KEK.
- A native A2A `AUTH_REQUIRED` translation in Phase 29's driver code. Phase 30 ships the `ErrAuthRequired` shape; the A2A driver's translation hook lands separately. The integration test asserts structural parity (both transports produce the SAME `ErrAuthRequired` typed sentinel + payload shape).
- Operator-facing config in `internal/config` — Phase 30 ships the `OAuthConfig` Go shape consumed in-code. YAML wiring is a follow-up.

## Acceptance criteria

- [x] `TokenStore` interface ships with three V1 drivers (in-mem / SQLite / Postgres — driver pluralism via the `state.StateStore` triad per §4.3 deviation above) with encryption-at-rest for token material.
- [x] `OAuthProvider` covers both `ScopeUser` and `ScopeAgent` — `BindingScope` is a declared field on `OAuthConfig`, never inferred.
- [x] On a tool-side auth-required event, the tool driver returns a typed `*ErrAuthRequired` carrying `(Source, SourceName, BindingScope, AuthorizeURL, State, Scopes, Message)`; the runtime emits `tool.auth_required` and parks the run via the Phase 50 Coordinator.
- [x] Resume reattaches the token via `CompleteFlow` → `coordinator.Resume`; the parked run resumes with the bearer token reachable through `Provider.Token`.
- [x] A2A `AUTH_REQUIRED` converges on the same primitive — the `ErrAuthRequired` shape is transport-agnostic; the integration test asserts shape parity.
- [x] `ErrAuthRequired` payload is typed (`ToolAuthRequiredPayload` embeds `events.SafeSealed`) and audit-redacted (every emission runs through `Redactor.Redact` for defence in depth) — no raw token material in events.
- [x] PKCE challenge/verifier round-trips against the test authorization server.
- [x] RFC 7591 dynamic client registration + `.well-known/oauth-authorization-server` discovery exercised against the test authorization server.
- [x] Token material is encrypted at rest — the conformance suite asserts raw `StateStore.Bytes` never contain the plaintext marker.
- [x] Admin-scope authz gates protect provider configuration — `ScopeAgent` flows require `registry.HasControlScope(ctx)`; `Token` (the read-side) does not require admin (it produces the prompt; whether admin or user is targeted is the Console's UX decision driven by `BindingScope`).
- [x] Cross-tenant / cross-user / cross-agent isolation conformance — one identity's tokens never resolve for another.
- [x] User-bound and agent-bound tokens coexist for the same tool without collision (the mixed-scope test).
- [x] Initiate-then-cancel emits no goroutine leak (25 cycles → baseline restored ± 5).

## Files added or changed

```text
internal/tools/auth/
  auth.go                       # interfaces + types + errors + glue
  errors_fmt.go                 # wrap helper
  events.go                     # tool.auth_required / tool.auth_completed event types
  sealer.go                     # AES-256-GCM Sealer
  tokenstore.go                 # TokenStore = typed wrapper over state.StateStore
  pkce.go                       # RFC 7636 verifier + S256 challenge
  provider.go                   # *Provider: Token / InitiateFlow / CompleteFlow / Revoke
  sealer_test.go
  tokenstore_test.go
  provider_test.go
  concurrent_test.go            # D-025 N=128 + single-flight-refresh
  conformance_test.go           # in-mem leg of the conformance suite
  testhelpers_test.go           # fakeAuthServer + harness
  runtime_test.go               # NumGoroutine helper
  conformancetest/
    conformancetest.go          # cross-driver TokenStore + Sealer suite

test/integration/
  phase30_tool_oauth_test.go    # cross-driver E2E + concurrency stress + failure-mode
  phase30_pkce_test.go          # local PKCE impl (spec, not a private detail)
  phase30_rand_test.go          # crypto/rand alias

scripts/smoke/phase-30.sh

docs/
  decisions.md                  # D-083 appended
  glossary.md                   # OAuth subsystem vocabulary added
  plans/README.md               # Phase 30 row flipped Pending → Shipped
  plans/phase-30-tool-oauth.md  # this file
README.md                       # Status row Phase 30 → Shipped
```

## Public API surface

```go
// package auth

type BindingScope string
const (ScopeUser BindingScope = "user"; ScopeAgent BindingScope = "agent")

type OAuthConfig struct {
    Source           tools.ToolSourceID
    SourceName       string
    BindingScope     BindingScope
    AgentID          string             // required when BindingScope == ScopeAgent
    ClientID         string             // optional: RFC 7591 dynamic registration if empty
    ClientSecret     string             // optional: PKCE-only if empty
    AuthorizeURL     string             // optional: discovered from ServerURL
    TokenURL         string             // optional: discovered from ServerURL
    RegistrationURL  string             // optional: RFC 7591 endpoint
    ServerURL        string             // required when AuthorizeURL/TokenURL empty
    RedirectURI      string             // required
    Scopes           []string
}

type Token struct {
    Source           tools.ToolSourceID
    BindingScope     BindingScope
    TenantID, UserID, AgentID string
    AccessToken      string             // never logged; encrypted at rest
    RefreshToken     string             // never logged; encrypted at rest
    TokenType        string             // "Bearer"
    ExpiresAt        time.Time
    Scopes           []string
    LastRefreshedAt  time.Time
}

type ErrAuthRequired struct {
    Source           tools.ToolSourceID
    SourceName       string
    BindingScope     BindingScope
    AuthorizeURL     string
    State            string
    Scopes           []string
    Message          string
}

type Sealer interface {
    Seal(plaintext []byte) ([]byte, error)
    Open(ciphertext []byte) ([]byte, error)
}
func NewAESGCMSealer(kek []byte) (Sealer, error)

type TokenStore interface {
    Get(ctx context.Context, scope BindingScope, subjectID string, source tools.ToolSourceID) (Token, bool, error)
    Put(ctx context.Context, t Token) error
    Delete(ctx context.Context, scope BindingScope, subjectID string, source tools.ToolSourceID) error
}
func NewTokenStore(store state.StateStore, sealer Sealer) (TokenStore, error)

type OAuthProvider interface {
    Token(ctx context.Context, source tools.ToolSourceID) (Token, error)
    InitiateFlow(ctx context.Context, source tools.ToolSourceID) (FlowInitiation, error)
    CompleteFlow(ctx context.Context, state, code string) (Token, error)
    Revoke(ctx context.Context, source tools.ToolSourceID) error
    Close(ctx context.Context) error
}
func NewProvider(configs []OAuthConfig, deps ProviderDeps) (*Provider, error)

// Event types (registered via init()):
//   EventTypeToolAuthRequired  = "tool.auth_required"
//   EventTypeToolAuthCompleted = "tool.auth_completed"
```

## Test plan

- **Unit:** `sealer_test.go` (round-trip, fresh-nonce, wrong-KEK, tampered-cipher, short-blob, bad-version); `tokenstore_test.go` (round-trip both scopes, miss-returns-false, delete-idempotency, missing-identity fail-loud, cross-tenant isolation, cross-agent isolation, mixed-scope coexistence, encryption-at-rest assertion against raw `StateStore.Bytes`); `provider_test.go` (Token-no-record returns `*ErrAuthRequired`, full cycle for both binding scopes, agent-bound requires admin scope, `tool.auth_completed` emitted, missing-identity / unknown-source / cross-identity state-swap / unknown-state / closed-provider all fail loud).
- **Integration:** `test/integration/phase30_tool_oauth_test.go` — full cycle for both binding scopes across in-mem + SQLite + Postgres (Postgres skips without `HARBOR_PG_DSN`), real audit redactor + events bus + pauseresume coordinator, real `httptest.Server` authorization server with PKCE + RFC 7591 dynamic registration + metadata discovery. A2A `AUTH_REQUIRED` shape-parity assertion. Initiate-then-cancel goroutine-leak test. Failure mode: cross-identity `CompleteFlow` → `ErrStateMismatch`.
- **Conformance:** `conformancetest/conformancetest.go` — the shared `Run(t, factory)` suite covering Put/Get round-trip both scopes, cross-tenant / cross-user / cross-agent isolation, mixed-scope coexistence, encryption-at-rest, delete idempotency, missing-identity fail-loud, tamper-rejection. Driven by `internal/tools/auth/conformance_test.go` against the in-mem leg; driven by `test/integration/phase30_tool_oauth_test.go` against in-mem + SQLite + Postgres.
- **Concurrency / leak:** `concurrent_test.go::TestProvider_ConcurrentReuse_NoCrossTalk` — D-025, N=128 concurrent flows under `-race`, asserts no cross-identity bleed + baseline goroutine count restored; `TestProvider_ConcurrentReuse_RefreshSingleFlight` — N=32 concurrent refresh calls produce ≤ 4 `/token` round-trips (single-flight gate).

## Smoke script additions

- Runs `go test -race -count=1 ./internal/tools/auth/...` — fails the smoke on any auth-package test regression.
- Runs `go test -race -count=1 -run TestE2E_Phase30 ./test/integration/...` — fails the smoke on any integration regression.
- Static guard: `internal/tools/auth/auth.go` declares the `OAuthProvider` interface with the four methods + `Close`. Catches accidental interface-shape drift.

## Coverage target

- `internal/tools/auth`: 85% (master-plan row 30).
- `internal/tools/auth/conformancetest`: no tests itself; exercised by callers.

## Dependencies

- 26 — tool catalog core (the `tools.ToolSourceID` type)
- 50 — pause/resume Coordinator (the unified primitive)
- 53a — Agent Registry (`agent_id` minted by the registry; `registry.HasControlScope` for admin-scope authz)

## Risks / open questions

- **Cross-session OAuth-token sharing for user-bound tokens.** A user's GitHub token, once issued, is reused across that user's sessions (TokenStore composite key is `(tenant, scope, subject, source)` — no session component). This matches brief 09's recommendation and operator muscle memory ("I connected GitHub once"). The audit trail captures every `tool.auth_required` / `tool.auth_completed` emission so cross-session use is observable.
- **The Phase 30 Provider's HTTP client lives in-package** with a 30s default timeout. Operators wanting a custom `http.Client` (TLS config, proxy, etc.) pass it via `ProviderDeps.HTTPClient`. The §10 config-schema knob lands in a follow-up phase.
- **The Protocol-side callback handler is a follow-up phase.** Phase 30 ships `CompleteFlow(state, code)` as a Go API call; the HTTP-handler wrapping (probably under `internal/protocol/transports/oauth_callback/`) lands when Console's auth-management view ships. Until then, `CompleteFlow` is reachable from in-process tests and from a future REST handler.
- **Single-flight refresh keys on `(scope, subject, source)`.** Brief 09 calls this out as the mitigation for the "concurrent refresh storm on agent-bound tokens shared across N sessions." The test (`TestProvider_ConcurrentReuse_RefreshSingleFlight`) asserts ≤ 4 round-trips for N=32 concurrent callers; a deeper exhaustive single-flight test would require an extra synchronization hook on the provider.

## Glossary additions

See `docs/glossary.md`: `auth.OAuthProvider`, `auth.TokenStore`, `auth.BindingScope`, `auth.ErrAuthRequired`, `tool.auth_required`, `tool.auth_completed`, PKCE, RFC 7591 dynamic client registration.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **Concurrent-reuse test passes** — `TestProvider_ConcurrentReuse_NoCrossTalk` runs N=128 against one shared `*Provider` under `-race`.
- [x] **Integration test exists** — `test/integration/phase30_tool_oauth_test.go` wires real `state.StateStore` + real `events.EventBus` + real `audit.Redactor` + real `pauseresume.Coordinator` + a real `httptest.Server` authorization server.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (D-083)
