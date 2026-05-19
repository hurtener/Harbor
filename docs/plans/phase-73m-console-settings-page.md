# Phase 73m — Console Settings page + `harbor console` subcommand (Protocol + UI + CLI bundled)

## Summary

Ships THREE bundled deliverables: (1) the `harbor console` CLI subcommand per D-091 (binding decomposition lock-in #9 — bundle here because Settings is the first page where the Connected-Runtimes card has visible meaning), (2) the Protocol read surfaces consumed by the Settings page (`runtime.info`, `runtime.storage`, `runtime.llm_posture`, `governance.posture`, plus admin method `auth.rotate_token`), and (3) the Settings page UI with 12 cards + Console DB local schema for preferences / runtime registry / auth profiles / PAT store / notifications routing / keybindings. The heaviest Stage 2.3 phase by surface count.

## RFC anchor

- RFC §5.3 (Protocol versioning)
- RFC §5.5 (Authentication)
- RFC §6.15 (Governance — cost ceilings + rate limits + MaxTokens)
- RFC §7 (Console layer)

## Briefs informing this phase

- brief 11 (Console feature surface — "Settings view", §CC-1 multi-runtime, §CC-2 identity-aware UI, §CC-3 notifications routing, §CC-6 theme / density / accessibility)
- brief 12 (deployment + two-surface model — "Why `harbor console`, not `harbor dev`, serves the Console", "`harbor console` subcommand — what the future phase delivers", auth-storage threat model)

## Brief findings incorporated

- brief 12 §"Why `harbor console`, not `harbor dev`, serves the Console": Settings is the page where the Connected-Runtimes card has visible meaning. Bundling the subcommand here (rather than as a separate Stage-1 phase) keeps the primitive (`harbor console`) with its first user-facing consumer (the Connected-Runtimes table that needs `harbor console` to be running to ATTACH to runtimes).
- brief 12 §"auth-storage threat model": per-runtime auth profiles in the Console DB MUST be encrypted at rest. The Console DB schema in this phase ships with an encrypted-blob column for auth profiles and PATs.
- brief 11 §CC-3: notifications routing is a rules-engine-lite Console-local mapper from event class → transport(s). The matrix UI lives here; the `notification.*` event family itself lands in Phase 72d.

## Findings I'm departing from (if any)

None.

## Goals

- Ship the `harbor console` CLI subcommand per D-091. Serves the SvelteKit build via `embed.FS`. Binds to a local port; respects the same Protocol-auth + identity-scope surface as the Runtime.
- Ship 4 NEW read Protocol methods: `runtime.info`, `runtime.storage`, `runtime.llm_posture`, `governance.posture`.
- Ship 1 NEW admin Protocol method: `auth.rotate_token` (gated by `console.admin` scope claim per D-066).
- Ship the Settings page UI with 12 cards covering connections / auth / preferences / routing / posture / about.
- Ship the Console DB schema for preferences (Appearance, Time & Locale, Keybindings, Notifications Routing), runtime registry, auth profiles (encrypted at rest), PAT store.
- The mock-mode banner per D-089 appears in Governance Posture AND LLM-Provider Posture cards when `HARBOR_DEV_ALLOW_MOCK=1` was set at runtime boot. Text matches the stderr banner verbatim: `DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION`.

## Non-goals

- Authoring governance config (edit `IdentityTiers`). Post-V1 per page-settings.md §10.
- Authoring storage driver bindings. Operator config concern.
- Cross-runtime fleet aggregator. D-091 — post-V1.
- `governance.rotate_key` (Phase 91 Post-V1) and `governance.swap_model` (Phase 92 Post-V1).
- Console-driven LLM provider swap. Post-V1.

## Acceptance criteria

- [ ] `cmd/harbor/cmd_console.go` (or equivalent) implements the `harbor console` subcommand. Serves the SvelteKit build via `embed.FS`. Binds to a configurable port (default `127.0.0.1:18790`). Connects to one or more remote Runtimes via the Connected-Runtimes registry stored in Console DB. The subcommand is NEVER bundled into `harbor dev` per D-091 (binding §13 carve-out).
- [ ] `internal/protocol/methods/methods.go` declares 5 new methods: `runtime.info`, `runtime.storage`, `runtime.llm_posture`, `governance.posture`, `auth.rotate_token`.
- [ ] `internal/protocol/types/runtime_info.go` defines `RuntimeInfo` (build version, Protocol version, deprecated method list per D-077, git commit, uptime, host OS, persistence drivers in use).
- [ ] `internal/protocol/types/runtime_storage.go` defines `RuntimeStorage` (per-subsystem driver name + masked connection string + migration version + last-migrated timestamp).
- [ ] `internal/protocol/types/runtime_llm.go` defines `LLMPosture` (provider name, model id, region/endpoint, `MockMode` boolean per D-089).
- [ ] `internal/protocol/types/governance.go` defines `GovernancePosture` (per-tier cost ceilings, rate limits, MaxTokens caps per D-081's `IdentityTiers`).
- [ ] All 4 read methods enforce identity-mandatory; cross-tenant calls require admin scope.
- [ ] `auth.rotate_token` requires the `console.admin` scope claim per D-066; degrades to 403 without.
- [ ] Settings page UI (`web/console/src/routes/settings/+page.svelte`) renders all 12 cards per mockup with the left sub-nav rail anchor scrolling.
- [ ] Console DB schema (Phase 72h base) extended with Settings-specific tables: `preferences`, `runtime_registry`, `auth_profiles` (encrypted blob column), `pat_store`, `notifications_routing`, `keybindings`. Migration is forward-only per CLAUDE.md §9.
- [ ] Auth profiles in Console DB are encrypted at rest per Brief 12 threat model. Decryption happens in-memory on demand; encrypted blob is the only persisted form.
- [ ] PATs (Personal Access Tokens) are one-time-reveal at creation; the Console NEVER displays the raw token after the create flow closes.
- [ ] Mock-mode banner (`DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION`) renders in Governance Posture + LLM-Provider Posture cards when `LLMPosture.MockMode = true`.
- [ ] Email and webhook notification-routing transports gated by `console.admin`; in-Console toast + browser notification are no-elevation defaults.
- [ ] Design tokens only — no raw color/spacing/type-scale literals (§13).
- [ ] `svelte-check --fail-on-warnings` passes.
- [ ] All data flows go through the typed Protocol client (`web/console/src/lib/protocol.ts`, D-093). NO hand-rolled `fetch`.
- [ ] Per-page Playwright spec at `web/console/tests/settings-page.spec.ts` covers: 12 cards render, sub-nav anchors scroll, `+ Add Runtime` Console-DB round-trip, `Rotate token` scope-claim degradation, mock-mode banner conditional on backend `MockMode = true`.
- [ ] `scripts/smoke/phase-73m.sh` asserts all 5 new methods + the `harbor console` subcommand boot.
- [ ] **Concurrent-reuse test:** N≥100 concurrent reads of `runtime.info` / `runtime.storage` against a shared runtime under `-race` (D-025).
- [ ] **Integration test:** `test/integration/settings_page_test.go` — real runtime + Console DB + Protocol transport + identity scope; mock-mode banner end-to-end; `auth.rotate_token` audit-event assertion; under `-race`.
- [ ] **`harbor console` boot test:** smoke script asserts the subcommand starts + serves the static asset at `/` (200 OK) + binds to the configurable port.

## Files added or changed

```text
cmd/harbor/cmd_console.go                                # NEW: harbor console subcommand
cmd/harbor/cmd_console_test.go
cmd/harbor/console_embed.go                              # NEW: //go:embed web/console/build/* — the SvelteKit static build
internal/protocol/methods/methods.go                      # +5 methods
internal/protocol/types/runtime_info.go                   # +RuntimeInfo
internal/protocol/types/runtime_storage.go                # +RuntimeStorage
internal/protocol/types/runtime_llm.go                    # +LLMPosture (with MockMode boolean)
internal/protocol/types/governance.go                     # +GovernancePosture
internal/protocol/types/auth.go                           # +AuthRotateTokenRequest/Response (admin)
internal/protocol/transports/stream/settings_handler.go
internal/protocol/transports/stream/settings_handler_test.go
internal/runtime/protocol/info.go                         # runtime.info implementation
internal/runtime/protocol/storage.go                      # runtime.storage implementation
internal/runtime/protocol/llm_posture.go                  # runtime.llm_posture implementation
internal/governance/protocol/posture.go                   # governance.posture implementation
internal/protocol/auth/rotate_token.go                    # auth.rotate_token implementation
internal/runtime/protocol/*_test.go
internal/runtime/protocol/concurrent_reuse_test.go
test/integration/settings_page_test.go
test/integration/harbor_console_boot_test.go              # cmd/harbor smoke under -race
web/console/src/routes/settings/+page.svelte
web/console/src/lib/components/settings/SubNavRail.svelte
web/console/src/lib/components/settings/ConnectedRuntimesCard.svelte
web/console/src/lib/components/settings/PerRuntimeAuthCard.svelte
web/console/src/lib/components/settings/APITokensCard.svelte
web/console/src/lib/components/settings/AppearanceCard.svelte
web/console/src/lib/components/settings/TimeLocaleCard.svelte
web/console/src/lib/components/settings/KeybindingsCard.svelte
web/console/src/lib/components/settings/NotificationsRoutingCard.svelte
web/console/src/lib/components/settings/RuntimeInfoCard.svelte
web/console/src/lib/components/settings/GovernancePostureCard.svelte
web/console/src/lib/components/settings/StorageDriversCard.svelte
web/console/src/lib/components/settings/LLMPostureCard.svelte
web/console/src/lib/components/settings/AboutCard.svelte
web/console/src/lib/components/settings/MockModeBanner.svelte
web/console/src/lib/console-db/migrations/00X_settings.sql  # preferences + runtime_registry + auth_profiles (encrypted) + pat_store + notifications_routing + keybindings
web/console/src/lib/console-db/auth_profiles.ts             # encrypt-at-rest helpers (uses Web Crypto API)
web/console/tests/settings-page.spec.ts
web/console/src/lib/protocol.ts                              # REGENERATED via make protocol-ts-gen
scripts/smoke/phase-73m.sh
docs/glossary.md                                            # +runtime.info, +runtime.storage, +runtime.llm_posture, +governance.posture, +auth.rotate_token, +"harbor console subcommand"
README.md                                                    # +pointer to harbor console subcommand (CLAUDE.md §4.2 rule 10)
docs/plans/README.md                                         # flip 73m row from Pending to Shipped on merge
```

## Public API surface

```go
// internal/protocol/types/runtime_info.go
type RuntimeInfo struct {
    BuildVersion     string
    ProtocolVersion  string
    DeprecatedMethods []string  // per D-077
    GitCommit        string
    UptimeSeconds    int64
    HostOS           string
    Drivers          DriversInUse
}

type DriversInUse struct {
    StateStore     string  // driver name (in-mem / sqlite / postgres)
    ArtifactStore  string
    MemoryStore    string
    EventBus       string
}

// internal/protocol/types/runtime_storage.go
type RuntimeStorage struct {
    Subsystems []StorageDriver
}

type StorageDriver struct {
    Subsystem        string  // "StateStore" | "ArtifactStore" | "MemoryStore" | ...
    DriverName       string
    ConnectionString string  // MASKED (passwords replaced with `***`)
    MigrationVersion int64
    LastMigratedAt   time.Time
}

// internal/protocol/types/runtime_llm.go
type LLMPosture struct {
    Provider string  // "openai" | "anthropic" | "mock" | ...
    ModelID  string
    Region   string  // or endpoint
    MockMode bool    // true when HARBOR_DEV_ALLOW_MOCK=1 was set at boot (D-089)
}

// internal/protocol/types/governance.go
type GovernancePosture struct {
    IdentityTiers map[string]GovernanceTierConfig  // per D-081
}

type GovernanceTierConfig struct {
    CostCeiling CostCeiling  // per tier (tenant / user / session)
    RateLimit   RateLimit
    MaxTokens   int
}
```

## Test plan

- **Unit:**
  - Each protocol handler `_test.go` — identity-rejection, scope-claim gating, projection shape.
  - `cmd/harbor/cmd_console_test.go` — subcommand argument parsing, embed.FS asset serving.
- **Integration:**
  - `test/integration/settings_page_test.go` — real runtime + Console DB + Protocol transport; mock-mode banner end-to-end (boot with `HARBOR_DEV_ALLOW_MOCK=1`, assert banner renders); `auth.rotate_token` audit-event assertion; cross-tenant Settings observation requires admin scope.
  - `test/integration/harbor_console_boot_test.go` — start `harbor console` on a test port, assert `/` serves the SvelteKit index, assert `/assets/*` serves the embedded assets, assert the subcommand respects port-override flag.
- **Conformance:**
  - 5 new methods run against the Protocol conformance suite (Phase 62).
- **Concurrency / leak:**
  - `concurrent_reuse_test.go` — N=100 concurrent reads against shared runtime under `-race`.
- **UI (Playwright):**
  - `settings-page.spec.ts` — 12 cards render; sub-nav anchors scroll; `+ Add Runtime` round-trips through Console DB; `Rotate token` button hidden without `console.admin` claim; mock-mode banner conditional on backend `MockMode = true`.

## Smoke script additions

`scripts/smoke/phase-73m.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'runtime/info' '{}'` → returns build + Protocol version + drivers.
- `protocol_call 'runtime/storage' '{}'` → returns per-subsystem driver projection.
- `protocol_call 'runtime/llm_posture' '{}'` → returns provider + model + MockMode boolean.
- `protocol_call 'governance/posture' '{}'` → returns IdentityTiers projection.
- `protocol_call 'auth/rotate_token' '{}'` without `console.admin` → expect 403.
- `harbor console --help` exit 0 (CLI-side smoke); `harbor console --port=18791 --datadir=$TMPDIR/harbor-console-smoke` boots + `/` returns 200 + tear down.

## Coverage target

- `internal/runtime/protocol`: 85%.
- `internal/governance/protocol`: 80%.
- `internal/protocol/auth`: 85% (security-critical).
- `cmd/harbor`: 75% (focus on `cmd_console.go` paths).
- `web/console/src/routes/settings/`: 70%.

## Dependencies

**Same-wave (Wave 13):**

- Phase 72 (events.subscribe scope foundation)
- Phase 72g (`governance.posture` + `llm.posture` — actually I am the phase that ships these per the decomposition; 72g's role is the runtime-side identity-tier consolidator if separate. Coordinator: confirm whether 72g and 73m's `governance.posture` are the same surface — if so, fold 72g into this phase via a §16 deviation note)
- Phase 72h (Console DB schema base — Settings extends with preferences / runtime_registry / auth_profiles / pat_store / notifications_routing / keybindings tables)
- Phase 75 (Playwright harness baseline)

**Already shipped (pre-Wave 13):**

- Phase 36a (Cost accumulator + per-identity ceilings — `Shipped`)
- Phase 36b (Per-identity rate limits + MaxTokens — `Shipped`)
- Phase 59 (Protocol versioning + deprecation policy — `Shipped`)
- Phase 60 (Protocol wire transport — `Shipped`)
- Phase 61 (Protocol auth — `Shipped`)
- Phase 89 (LLM-default + mock escape hatch — `Shipped`; supplies `LLMPosture.MockMode` flag per D-089)

## Risks / open questions

- **Overlap with Phase 72g `governance.posture` + `llm.posture`.** The decomposition doc lists 72g as a Stage-1 phase that ships these two read methods. THIS phase also lists them as deliverables. There are two valid resolutions: (a) 72g ships them as Stage-1 primitives + 73m consumes them (the cleaner shape; matches §17.7 primitive-with-consumer); (b) fold 72g into 73m (the binding §12 lock-in #9 path — "bundle into Settings"). **Coordinator decision required.** Recommended: keep 72g as Stage-1 ships methods, 73m consumes them; that gives every Stage-1 primitive a Stage-2 consumer per §13 and avoids ambiguous ownership. This plan is written under recommendation (a); the duplicated method list above is removed if (a) holds.
- **Encryption-at-rest for Console DB auth profiles.** Console-side encryption via Web Crypto API requires a master key — derived from operator's session token, kept only in memory. If the operator clears the session, encrypted profiles MUST decrypt on next sign-in. Operator may want to flag the threat model (lost session = relogin chain, not data loss).
- **`harbor console` port collision.** Default `127.0.0.1:18790` might conflict with other dev tooling. Default is overridable via `--port` flag; smoke asserts an alternate port works.
- **PAT one-time-reveal UX.** A wrong-click after creation strands the PAT; the user must regenerate. Acceptable; matches industry pattern.

## Glossary additions

- **`runtime.info`** — Protocol method returning the runtime's build version, Protocol version, deprecated method list, git commit, uptime, host OS, and drivers-in-use.
- **`runtime.storage`** — Protocol method returning per-subsystem driver projection.
- **`runtime.llm_posture`** — Protocol method returning the LLM provider posture including the `MockMode` flag per D-089.
- **`governance.posture`** — Protocol method returning the runtime's governance configuration (per-tier ceilings + rate limits + MaxTokens caps per D-081).
- **`auth.rotate_token`** — Admin Protocol method rotating the operator's current Protocol-auth token. Requires `console.admin` claim.
- **`harbor console` subcommand** — CLI subcommand serving the SvelteKit Console build via `embed.FS`. The ONLY supported Console deployment path per D-091. NEVER bundled into `harbor dev`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` passes
- [ ] `svelte-check --fail-on-warnings` passes
- [ ] `npm run lint` passes in `web/console/`
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation paths changed — `auth.rotate_token` touches identity; integration test asserts scope-claim gating
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent reads against shared runtime under `-race` (D-025)
- [ ] **Integration test passes** — `settings_page_test.go` + `harbor_console_boot_test.go` (§17)
- [ ] **Per-page Playwright spec lands in this phase's PR**
- [ ] **`harbor console` subcommand boot tested** — smoke asserts `/` serves; integration asserts asset paths + port-override
- [ ] **README.md updated** — pointer to `harbor console` subcommand (CLAUDE.md §4.2 rule 10 — new CLI subcommand needs README update)
- [ ] **`docs/plans/README.md` row 73m flipped to Shipped on merge** (CLAUDE.md §4.2 rule 11)
- [ ] Glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review
