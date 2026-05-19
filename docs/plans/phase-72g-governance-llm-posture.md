# Phase 72g — `governance.posture` + `llm.posture`

## Summary

Adds two Protocol methods that surface read-only operator posture: `governance.posture` returns the active `IdentityTiers` view (D-081) — per-tier `BudgetCeilingUSD` + `RateLimit` (token-bucket) + `MaxTokens` + the `DefaultTier` selector; `llm.posture` returns the bound LLM provider (provider name, model id, region/endpoint, `MockMode` boolean per D-089). Both methods are identity-mandatory; cross-tenant reads require the admin scope claim (D-079). The same-phase consumer is an integration test that boots the real governance subsystem + real LLM registry through the Protocol transport and asserts the payload shapes round-trip cleanly — the Settings page UI consumer (Governance Posture + LLM-Provider Posture cards) lands in Phase 73m Stage 2.3.

## RFC anchor

- RFC §5.5 (Authentication — identity-mandatory + scope claims for cross-tenant)
- RFC §6.15 (Governance subsystem — `IdentityTiers` shape + V1 scope)
- RFC §7 (Console layer — Protocol-client posture)

## Briefs informing this phase

- brief 11 (Console feature surface — §"Settings view" Governance Posture + LLM-Provider Posture cards; §CC-1 read-only-by-default)
- brief 12 (deployment + two-surface model — third-party consoles consume the same posture surface; mock-mode banner must be wire-visible)

## Brief findings incorporated

- brief 11 §"Settings view": Settings exposes "connected runtimes", "API tokens", "theme", "notifications routing", "keybindings", "density", "time zone". The mockup (`docs/design/console/page-settings.md` §3) extended this with **Governance Posture** + **LLM-Provider Posture** cards — both read-only views of runtime configuration. This phase ships the Protocol primitives those cards consume; the UI lands in 73m.
- brief 11 §CC-1 (read-only-by-default): "Console renders runtime state; edits go through explicit Protocol methods, not implicit form-binding." `governance.posture` and `llm.posture` are **read-only** — operator changes to ceilings or LLM provider are restart-required per RFC §6.15's "Hot-reloadable fields" carve-out + RFC §10 default. Post-V1 admin methods (`governance.rotate_key`, `governance.swap_model`, master plan phases 91/92) are NOT in this phase's surface.
- brief 11 §"Settings view" — Mock-mode banner is the integrity boundary between dev iteration and production posture. The banner text is canonical (`[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]` per D-089) and the `LLMPosture.MockMode` flag is the wire-level signal the Console renders verbatim. A Console that hides the banner when `MockMode == true` is a §13 forbidden-practice violation.
- brief 12 §"the two-surface model": the posture shapes MUST be the same one third-party Console implementations consume — wire types live in `internal/protocol/types/`, never a Console-private struct. `IdentityTiers` is already a Go-side internal config struct (`internal/governance/Config.IdentityTiers`); the Protocol projection mirrors but does NOT re-export the internal type (D-002 single-source for wire types preserved).

## Findings I'm departing from (if any)

None.

## Goals

- Ship two Protocol methods (`governance.posture`, `llm.posture`) that surface read-only operator posture for the Settings page (73m Stage 2.3) and for third-party Console implementations.
- `governance.posture` returns the D-081 `IdentityTiers` shape verbatim (per-tier `BudgetCeilingUSD`, `RateLimit{Capacity, RefillTokens, RefillInterval}`, `MaxTokens`) + the `DefaultTier` selector + the resolved-tier-for-caller field.
- `llm.posture` returns the runtime's bound LLM provider name, model id, region/endpoint, and a `MockMode` boolean that is `true` iff the runtime booted with `HARBOR_DEV_ALLOW_MOCK=1` (D-089).
- Both methods are identity-mandatory (RFC §5.5); cross-tenant reads require the `auth.ScopeAdmin` scope claim (D-079).
- The same-phase consumer is `test/integration/phase72g_posture_test.go` — boots real `internal/governance` + real `internal/llm` + real Protocol transport + real auth validator, asserts shape round-trip across two boot modes (production-shaped config + `HARBOR_DEV_ALLOW_MOCK=1`), and exercises the cross-tenant rejection failure mode.
- The §13 primitive-with-consumer rule is satisfied in-PR (test consumer) + in-wave (73m Stage 2.3 UI consumer).

## Non-goals

- **No write methods.** Edits to `IdentityTiers` are restart-required per RFC §6.15 + RFC §10 default. The post-V1 admin methods `governance.rotate_key` (master plan 91), `governance.swap_model` (master plan 92), `governance.set_ceiling` (post-V1) are NOT in this phase's surface.
- **No new LLM driver registry.** The mock vs bifrost driver selection is already settled by D-089 (Phase 64 / `harbor dev` v1). `llm.posture` is a read projection over the already-registered driver — not a new seam.
- **No bypass of the mock banner.** Operators that want to silence the banner must remove the `HARBOR_DEV_ALLOW_MOCK=1` env var (per D-089's "no suppression flag" stance, mirrored in D-081's no-deprecation-warning-quieting).
- **No per-model registry projection.** A future `llm.models.list` (post-V1) lives in its own phase. V1 ships the single bound provider — RFC §6.15 + D-088 "single-provider per Harbor instance".
- **No Console-local persistence.** Posture is runtime state — Console DB (D-061) NEVER caches `IdentityTiers` or `LLMPosture` as a shadow source of truth. The Console renders fresh on every navigation.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares `MethodGovernancePosture Method = "governance.posture"` and `MethodLLMPosture Method = "llm.posture"`. Both registered in `canonicalMethods`; the existing `methods.Methods()` exhaustiveness lint covers them.
- [ ] `internal/protocol/types/governance.go` defines `GovernancePostureRequest`, `GovernancePostureResponse`, `IdentityTierView`, `RateLimitView` — single source of truth (CLAUDE.md §8: wire types live ONLY in `internal/protocol/types/`).
- [ ] `internal/protocol/types/llm.go` defines `LLMPostureRequest`, `LLMPostureResponse`.
- [ ] `governance.posture` returns the `IdentityTiers` shape per D-081: a `map[string]IdentityTierView` keyed by tier name (e.g. `"free"`, `"team"`, `"enterprise"`), each `IdentityTierView` carrying `BudgetCeilingUSD float64`, `RateLimit RateLimitView{Capacity int, RefillTokens int, RefillIntervalMS int64}`, `MaxTokens int`. Plus a top-level `DefaultTier string` and `ResolvedTier string` (the tier the caller's identity resolves to via `TierResolver`).
- [ ] `llm.posture` returns `Provider string` (e.g. `"bifrost"`, `"mock"`), `Model string` (e.g. `"openai/gpt-5.3-chat"`), `Region string` (provider endpoint region or `""` when not applicable), and `MockMode bool` — `true` iff `HARBOR_DEV_ALLOW_MOCK=1` was set at boot (D-089).
- [ ] **Mock-mode contract is binary and structural.** `LLMPosture.MockMode == true` iff the runtime's LLM driver registry resolved `"mock"` via the `HARBOR_DEV_ALLOW_MOCK=1` escape hatch path. The flag is captured at boot via the same call site that prints the stderr banner (`cmd/harbor/devmock.go::registerMockIfDevAllowMock`); the handler reads the captured boolean, never re-checks the env var. The handler does NOT echo the banner text — that lives in `cmd/harbor/devmock.go` per D-089 and is the responsibility of the Console UI to render verbatim.
- [ ] **Identity is mandatory** (CLAUDE.md §6, RFC §5.5). Both handlers call `identity.MustFrom(ctx)` after the auth middleware (Phase 61) has resolved the JWT; missing identity → `ErrIdentityRequired` (already shipped) → 401. There is NO opt-out flag (CLAUDE.md §13 "identity-downgrading knobs" forbidden-practice).
- [ ] **Cross-tenant reads require `auth.ScopeAdmin`** (D-079). The handler reads the request's `tenant_id` field; if non-empty AND different from the caller's identity-resolved tenant, the handler asserts the `auth.HasScope(ctx, auth.ScopeAdmin)` predicate per Phase 61; missing scope → `ErrAdminScopeRequired` → 403. A `tenant_id == ""` request reads the caller's own tenant (the default path, no scope claim needed).
- [ ] **`governance.posture` reads from a typed snapshot, not the live mutable config.** The governance subsystem exposes a `Posture(ctx) (GovernancePostureSnapshot, error)` method that returns a deep-copied view of `Config.IdentityTiers` (the configured map) + the `DefaultTier` (string) + the caller-resolved tier (via the configured `TierResolver`). The internal `governance.Config` struct stays unchanged; the snapshot wrapper lives at `internal/governance/posture.go`.
- [ ] **`llm.posture` reads from a typed snapshot at the LLM registry seam.** The LLM registry exposes a `Posture(ctx) (LLMPostureSnapshot, error)` method backed by the same `Config.LLM` block the binary read at boot + the mock-flag captured at boot. The handler does NOT call `os.Getenv("HARBOR_DEV_ALLOW_MOCK")` at request-time — D-089's boot-time capture is the single source.
- [ ] **Concurrent-reuse test** (D-025, CLAUDE.md §5 + §11). Both handlers are compiled artifacts shared across goroutines; `internal/protocol/transports/stream/posture_handler_concurrent_test.go` runs N≥100 concurrent calls against one shared handler instance under `-race`, asserting no data races, no context bleed (per-goroutine identity preserved), baseline goroutine count restored after teardown.
- [ ] **Integration test** (CLAUDE.md §17). `test/integration/phase72g_posture_test.go` wires real `internal/governance` + real `internal/llm` registry + real `internal/audit` redactor + real `internal/events` bus + real Phase 60 transport + real Phase 61 auth validator (over the canonical ES256 testdata keypair). Asserts: (a) `governance.posture` returns the configured `IdentityTiers` verbatim; (b) `llm.posture` MockMode round-trip — one sub-test boots with `HARBOR_DEV_ALLOW_MOCK=1` and asserts `MockMode == true`, another boots with a production-shaped config (mock disabled) and asserts `MockMode == false`; (c) cross-tenant rejection — caller WITHOUT `auth.ScopeAdmin` requesting `tenant_id == "other"` → 403 + `ErrAdminScopeRequired`; (d) missing identity rejection → 401 + `ErrIdentityRequired`. N≥10 concurrent SSE subscribers / posture-readers stress per §17.3.
- [ ] **Audit emission.** Cross-tenant reads (admin-scoped) emit a `governance.posture_read_admin` audit event through the shipped `audit.Redactor` (CLAUDE.md §7 + RFC §6.15); same for `llm.posture`. Per-tenant own-tenant reads do NOT emit audit (matches the Phase 73 sessions.inspect convention).
- [ ] **No new error codes.** Both rejections reuse the shipped `ErrIdentityRequired` + `ErrAdminScopeRequired` codes from `internal/protocol/errors/errors.go` (D-079 single-source).
- [ ] **`scripts/smoke/phase-72g.sh`** asserts both methods round-trip a 200 with the expected JSON shape, asserts the identity-rejection 401, asserts the cross-tenant 403, and asserts the mock-mode flag round-trip when `HARBOR_DEV_ALLOW_MOCK=1` is exported. Header `# PREFLIGHT_REQUIRES: live-server`.

## Files added or changed

```text
internal/protocol/methods/methods.go              # +MethodGovernancePosture, +MethodLLMPosture
internal/protocol/types/governance.go             # +GovernancePostureRequest/Response, +IdentityTierView, +RateLimitView
internal/protocol/types/llm.go                    # +LLMPostureRequest/Response
internal/protocol/transports/stream/posture_handler.go         # handler dispatch + scope-claim check + audit emission
internal/protocol/transports/stream/posture_handler_test.go    # unit tests (happy + identity-reject + cross-tenant)
internal/protocol/transports/stream/posture_handler_concurrent_test.go  # D-025 N≥100 concurrent invocations
internal/governance/posture.go                    # Posture(ctx) snapshot accessor over Config.IdentityTiers
internal/governance/posture_test.go               # snapshot deep-copy + tier-resolution unit
internal/llm/posture.go                           # Posture(ctx) snapshot over bound driver + MockMode capture
internal/llm/posture_test.go                      # snapshot + mock-mode-flag unit
internal/llm/registry.go                          # MODIFIED: expose RegisterMockModeCaptured(bool) — boot-time capture path
cmd/harbor/devmock.go                             # MODIFIED: registerMockIfDevAllowMock also calls llm.RegisterMockModeCaptured(true)
test/integration/phase72g_posture_test.go         # cross-subsystem E2E + cross-tenant + mock-mode round-trip + N≥10 stress
scripts/smoke/phase-72g.sh                        # protocol_call assertions + identity-reject + mock-mode
docs/glossary.md                                  # +governance.posture, +llm.posture, +GovernancePosture, +LLMPosture
```

## Public API surface

```go
// internal/protocol/types/governance.go
type GovernancePostureRequest struct {
    TenantID string // empty = caller's own tenant; non-empty + different = requires auth.ScopeAdmin
}

type GovernancePostureResponse struct {
    DefaultTier   string                        // the operator-configured default tier name
    ResolvedTier  string                        // the tier name the caller's identity resolves to via TierResolver
    IdentityTiers map[string]IdentityTierView   // tier name → tier configuration (D-081 shape)
}

type IdentityTierView struct {
    BudgetCeilingUSD float64       // per-identity cost ceiling in USD (0 = no ceiling)
    RateLimit        RateLimitView // token-bucket rate-limit configuration
    MaxTokens        int           // per-call MaxTokens (0 = no enforcement)
}

type RateLimitView struct {
    Capacity         int   // bucket capacity (tokens)
    RefillTokens     int   // tokens added per refill tick
    RefillIntervalMS int64 // tick duration in milliseconds (wire-friendly, time.Duration on Go side)
}

// internal/protocol/types/llm.go
type LLMPostureRequest struct {
    TenantID string // empty = caller's own tenant; non-empty + different = requires auth.ScopeAdmin
}

type LLMPostureResponse struct {
    Provider string // e.g. "bifrost", "mock"
    Model    string // e.g. "openai/gpt-5.3-chat"
    Region   string // provider endpoint region; "" when not applicable
    MockMode bool   // true iff runtime booted with HARBOR_DEV_ALLOW_MOCK=1 (D-089)
}

// internal/governance/posture.go
func (s *Subsystem) Posture(ctx context.Context) (Snapshot, error)

// Snapshot is a deep-copied, immutable view of the configured IdentityTiers
// plus the caller-resolved tier (via Config.TierResolver applied to identity.MustFrom(ctx)).
type Snapshot struct {
    DefaultTier   string
    ResolvedTier  string
    IdentityTiers map[string]TierConfig // deep-copied from Config.IdentityTiers
}

// internal/llm/posture.go
type PostureSnapshot struct {
    Provider string
    Model    string
    Region   string
    MockMode bool
}

func (r *Registry) Posture(ctx context.Context) (PostureSnapshot, error)

// internal/llm/registry.go (additive)
// RegisterMockModeCaptured records that the runtime booted with HARBOR_DEV_ALLOW_MOCK=1.
// Called exactly once from cmd/harbor/devmock.go::registerMockIfDevAllowMock at boot;
// the call site that prints the [DEV-ONLY MOCK LLM ...] banner per D-089 ALSO captures
// the boolean so llm.posture surfaces the truth without re-reading the env var at request time.
func RegisterMockModeCaptured(v bool)
```

## Test plan

- **Unit:**
  - `internal/governance/posture_test.go` — `Posture` returns a deep copy (mutating the returned `IdentityTiers` does NOT mutate the underlying `Config`); empty `IdentityTiers` → empty map + zero-value `DefaultTier`; non-empty `Config.TierResolver` resolves the caller's tier; nil resolver → `ResolvedTier == DefaultTier`.
  - `internal/llm/posture_test.go` — `Posture` returns `MockMode == false` by default; after `RegisterMockModeCaptured(true)` (the dev hatch path) → `MockMode == true`. Resetting between test cases via `t.Cleanup`.
  - `internal/protocol/transports/stream/posture_handler_test.go` — happy-path JSON shape; missing identity → 401 + `ErrIdentityRequired`; tenant_id != caller's tenant + missing admin scope → 403 + `ErrAdminScopeRequired`; tenant_id == "" → reads caller's own tenant (no scope claim required); audit emission asserted via in-test bus subscriber.
- **Integration:**
  - `test/integration/phase72g_posture_test.go` — boots `harbortest/devstack.Assemble` with two configurations:
    1. Production-shaped (no `HARBOR_DEV_ALLOW_MOCK`, configured bifrost-like LLM via the test seam) → `LLMPosture.MockMode == false`, `LLMPosture.Provider == "<configured>"`.
    2. `HARBOR_DEV_ALLOW_MOCK=1` set before boot → `LLMPosture.MockMode == true`, `LLMPosture.Provider == "mock"`.
  - Each leg: emit a `GovernancePostureRequest` over the live Protocol transport with the canonical ES256 testdata keypair Bearer token, assert the `IdentityTiers` round-trip matches the configured map verbatim, assert `ResolvedTier` matches the test identity's tier.
  - Cross-tenant: caller without admin scope requests `tenant_id == "other"` → 403; caller WITH admin scope → 200 + the requested tenant's posture (which is the same map in the V1 single-tenant-per-runtime model, but the scope check still fires).
  - Failure mode: missing-identity request (bypassed auth middleware in test setup) → 401.
  - Concurrency: N≥10 concurrent posture readers across N tenants under `-race`; assert no cross-talk (each goroutine sees its own `ResolvedTier`), baseline goroutine count restored after teardown.
- **Conformance:**
  - Both methods are registered in `internal/protocol/conformance.CanonicalWireTypes` so the existing `methods.Methods()` exhaustiveness lint (D-082) and `internal/protocol/singlesource/singlesource_test.go` lockstep check exercise them automatically. No new conformance scenarios required — the read-only-no-mutation shape is covered by the per-method round-trip test.
- **Concurrency / leak:**
  - `internal/protocol/transports/stream/posture_handler_concurrent_test.go::TestPostureHandler_ConcurrentReuse` — N=128 concurrent invocations against one shared handler instance under `-race`, asserting D-025's four guarantees (no data races, no context bleed via per-goroutine identity assertion, no cross-cancellation, baseline goroutine count restored). The handler is a compiled artifact — its dependencies (`governance.Subsystem`, `llm.Registry`, `audit.Redactor`) are immutable after construction.

## Smoke script additions

`scripts/smoke/phase-72g.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- Parse `HARBOR_DEV_TOKEN` from `${HARBOR_DATA_DIR}/server.log` (the existing Phase 64 convention).
- `POST /v1/control/governance.posture` with the Bearer token and an empty-body JSON `{}` (own-tenant read) → assert 200; `assert_json_path '.identity_tiers | type' 'object'`; `assert_json_path '.default_tier | type' 'string'`.
- `POST /v1/control/llm.posture` with the Bearer token + `{}` → assert 200; `assert_json_path '.provider | type' 'string'`; `assert_json_path '.mock_mode | type' 'boolean'`. When the preflight harness boots `harbor dev` with `HARBOR_DEV_ALLOW_MOCK=1` (the Phase 64 convention), additionally `assert_json_path '.mock_mode' 'true' ...`.
- `POST /v1/control/governance.posture` WITHOUT a Bearer token → `assert_status 401` (identity-mandatory rejection).
- `POST /v1/control/llm.posture` WITHOUT a Bearer token → `assert_status 401`.
- `POST /v1/control/governance.posture` with the Bearer token but `{"tenant_id":"other-tenant"}` (non-admin caller) → `assert_status 403` (cross-tenant rejection per D-079).
- `POST /v1/control/llm.posture` with the Bearer token but `{"tenant_id":"other-tenant"}` → `assert_status 403`.
- All assertions honour the 404/405/501 → SKIP convention so the smoke coexists with phase-N builds that have not yet shipped the surface.

## Coverage target

- `internal/governance` (posture additions only): ≥ 85% (matches Phase 36a's existing target).
- `internal/llm` (posture additions only): ≥ 85%.
- `internal/protocol/transports/stream` (posture handler): ≥ 85%.
- `internal/protocol/types` (new shapes): ≥ 90% (struct-level coverage via the per-method round-trip test).

## Dependencies

**Already shipped (pre-Wave 13):**

- Phase 36a (Cost accumulator + per-identity cost ceilings — `Shipped`; supplies the live `IdentityTiers` map on `governance.Config`).
- Phase 36b (Per-identity rate limits + MaxTokens — `Shipped`; extends `IdentityTiers` to its full D-081 shape).
- Phase 32 (LLM client — `Shipped`).
- Phase 33 (Bifrost driver — `Shipped`; the production LLM provider).
- Phase 60 (Protocol wire transport — `Shipped`; supplies `transports.NewMux` the handler mounts onto).
- Phase 61 (Protocol auth — `Shipped`; supplies the `auth.ScopeAdmin` claim + `auth.HasScope` predicate + the `ErrAdminScopeRequired` error).
- Phase 64 (`harbor dev` v1 — `Shipped`, D-089; supplies the `HARBOR_DEV_ALLOW_MOCK=1` boot-time mock escape hatch + the `[DEV-ONLY MOCK LLM ...]` banner. This is the **MockMode supplier** the user's brief flagged as "Phase 89" — the actual decision/phase pair is D-089 / Phase 64.).

**Same-wave (Wave 13, Stage 1) — none.** This phase has no Stage-1 sibling dependencies; it consumes only already-shipped subsystems. Both methods are wave-13-extends primitives whose UI consumer (73m Settings Stage 2.3) lands after Stage-2.2 drains.

## Risks / open questions

- **D-089 mock-mode capture path is shared with the stderr banner emit.** The single source for `MockMode == true` is the boot-time call to `cmd/harbor/devmock.go::registerMockIfDevAllowMock`. If a future PR re-routes the dev-hatch path (e.g. promotes the env var to a CLI flag), it MUST also update `llm.RegisterMockModeCaptured` at the same call site — otherwise `LLMPosture.MockMode` silently desyncs from the banner. The same-PR concurrent-reuse + integration tests assert both paths fire together; a divergence would surface in CI. Documented in this phase's "Files added or changed" so a future grep over `cmd/harbor/devmock.go` finds the cross-reference.
- **Single-tenant-per-runtime V1 posture.** The `tenant_id` field on both request shapes is forward-looking — V1 ships a single tenant per Harbor instance (RFC §6.15 + master plan). The cross-tenant scope claim test is still meaningful because the auth middleware DOES inspect the body's `tenant_id` field (D-079 defence-in-depth: "a body claiming a different `(tenant, user, session)` than the JWT is rejected 401 before Dispatch runs"). The acceptance criterion exercises the scope-claim path so a post-V1 multi-tenant deployment finds the surface ready.
- **`Region` field on `LLMPosture`.** Bifrost-side, "region" is not a first-class concept on every provider — OpenRouter routes through bifrost-internal regions, OpenAI direct has US/EU; the spec sets `Region == ""` when not applicable. The Settings page (73m) renders an em-dash placeholder for the empty case; the test asserts the field is present in the JSON shape, not its value.
- **`governance.posture` for `IdentityTiers == nil` (latent-default boot).** D-044 + Phase 36a allow an empty `IdentityTiers` map → no enforcement. The handler returns `IdentityTiers: {}` (empty map, NOT null) in that case + `DefaultTier == ""` + `ResolvedTier == ""`. The Settings card MUST render an explicit "No tiers configured" state — not a blank panel. Captured in this phase's smoke + the 73m Stage-2.3 acceptance criteria.
- **No write methods raises an Operator expectation.** The Settings page mockup shows the cards as read-only; an operator wanting to change ceilings restarts the runtime with a new YAML. Post-V1 admin methods (`governance.set_ceiling`, master plan slot TBD) are explicitly out of scope for this phase per RFC §6.15's "Hot-reloadable fields" carve-out — that carve-out is operator-facing config, not a Protocol method. The Settings card surface contains a "Configured via harbor.yaml — restart required to change" microcopy.

## Glossary additions

- **`governance.posture`** — Protocol method returning the runtime's read-only governance configuration (the D-081 `IdentityTiers` map + `DefaultTier` + the caller-resolved tier). Identity-mandatory; cross-tenant reads require `auth.ScopeAdmin` (D-079). Added in Phase 72g.
- **`llm.posture`** — Protocol method returning the runtime's read-only LLM provider posture (provider name, model id, region, `MockMode` boolean). `MockMode == true` iff the runtime booted with `HARBOR_DEV_ALLOW_MOCK=1` per D-089. Added in Phase 72g.
- **`GovernancePosture`** — The wire-type response from `governance.posture` (`GovernancePostureResponse` in `internal/protocol/types/governance.go`). Carries `DefaultTier`, `ResolvedTier`, `IdentityTiers map[string]IdentityTierView`. The Console's Settings page Governance Posture card renders this verbatim.
- **`LLMPosture`** — The wire-type response from `llm.posture` (`LLMPostureResponse` in `internal/protocol/types/llm.go`). Carries `Provider`, `Model`, `Region`, `MockMode bool`. The Console's Settings page LLM-Provider Posture card renders this verbatim and displays the canonical `[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]` banner when `MockMode == true` (D-089).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated targets (`internal/governance`, `internal/llm`, `internal/protocol/transports/stream`, `internal/protocol/types`)
- [ ] If multi-isolation paths changed: cross-tenant isolation test passes — the integration test's "non-admin cross-tenant → 403" assertion is binding (both methods touch identity)
- [ ] **Concurrent-reuse test for the posture handler passes** — N≥100 concurrent invocations against a single shared handler under `-race`, asserting no data races, no context bleed, baseline goroutine count restored (D-025; the handler is a compiled artifact)
- [ ] **Integration test exists** — `test/integration/phase72g_posture_test.go` wires real `internal/governance` + real `internal/llm` registry + real Phase 60 transport + real Phase 61 auth validator, covers identity-reject + cross-tenant-reject + mock-mode round-trip, runs N≥10 concurrency stress, all under `-race` (CLAUDE.md §17)
- [ ] **MockMode capture path is reciprocal with the banner emit** — `cmd/harbor/devmock.go::registerMockIfDevAllowMock` calls `llm.RegisterMockModeCaptured(true)` at the SAME call site that prints the `[DEV-ONLY MOCK LLM ...]` stderr banner; the integration test boots with `HARBOR_DEV_ALLOW_MOCK=1` and asserts BOTH the banner AND `LLMPosture.MockMode == true` (§17.6 — fix the production gap, not just the test side)
- [ ] **No write methods, no env-var re-reads at request time, no new error codes** — per the Non-goals section
- [ ] New vocabulary added to `docs/glossary.md` (governance.posture, llm.posture, GovernancePosture, LLMPosture)
- [ ] If a brief finding was departed from: justified + decisions.md entry filed (None for this phase)
