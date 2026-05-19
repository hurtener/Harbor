# Phase 73m — Console Settings page + `harbor console` subcommand

## Summary

Ships TWO bundled deliverables: (1) the `harbor console` CLI subcommand per D-091 + decomposition §12 lock-in #9 — Settings is the first page where the Connected-Runtimes card has visible meaning, so the subcommand bundles here, and (2) the Settings page UI with 12 cards rendering the operator's preferences, runtime connections, auth posture, governance posture, and LLM posture. The page is a pure **consumer** of upstream surfaces: 72f's runtime posture methods (`runtime.info`, `runtime.health`, `runtime.counters`, `runtime.drivers`, `metrics.snapshot`), 72g's governance + LLM posture methods (`governance.posture`, `llm.posture`), and 72h's Console DB schema (`profiles`, `runtime_registry`, `auth_profiles`, `pat_store`, `notifications_routing`, `keybindings`, `saved_filters`, `saved_views`). The ONLY net-new Protocol method this phase ships is `auth.rotate_token` (admin, gated by `console.admin` per D-066).

## RFC anchor

- RFC §5.3 (Protocol versioning)
- RFC §5.5 (Authentication)
- RFC §6.15 (Governance — cost ceilings + rate limits + MaxTokens)
- RFC §7 (Console layer)

## Briefs informing this phase

- brief 11 (Console feature surface — "Settings view", §CC-1 multi-runtime, §CC-2 identity-aware UI, §CC-3 notifications routing, §CC-6 theme / density / accessibility)
- brief 12 (deployment + two-surface model — "Why `harbor console`, not `harbor dev`, serves the Console", "`harbor console` subcommand — what the future phase delivers", auth-storage threat model)

## Brief findings incorporated

- brief 12 §"Why `harbor console`, not `harbor dev`, serves the Console": Settings is the page where the Connected-Runtimes card has visible meaning. Bundling the subcommand here (rather than as a separate Stage-1 phase) keeps the primitive (`harbor console`) with its first user-facing consumer (the Connected-Runtimes table that needs `harbor console` to be running to ATTACH to runtimes). The bundling is binding per decomposition §12 lock-in #9.
- brief 12 §"auth-storage threat model": per-runtime auth profiles in the Console DB are encrypted at rest. The encryption helpers + the schema for `auth_profiles` + `pat_store` are owned by Phase 72h (the Console DB phase); 73m consumes them via 72h's exported encrypt/decrypt helpers — 73m never re-implements crypto.
- brief 11 §CC-3: notifications routing is a rules-engine-lite Console-local mapper from event class → transport(s). The routing matrix UI lives in this phase; the table that persists the routing rows (`notifications_routing`) is owned by 72h; the `notification.*` event family the mapper consumes is owned by Phase 72d.

## Findings I'm departing from (if any)

None.

## Goals

- Ship the `harbor console` CLI subcommand per D-091. Serves the SvelteKit build via `embed.FS`. Binds to a local port; respects the same Protocol-auth + identity-scope surface as the Runtime. NEVER bundled into `harbor dev` per D-091.
- Ship 1 NEW admin Protocol method: `auth.rotate_token` (gated by `console.admin` scope claim per D-066).
- Ship the Settings page UI with 12 cards — Connected Runtimes / Per-Runtime Auth / API Tokens / Appearance / Time & Locale / Keybindings / Notifications Routing / Runtime Info / Governance Posture / Storage Drivers / LLM-Provider Posture / About.
- Settings page is a **pure consumer** of upstream surfaces: 72f (runtime posture methods), 72g (governance + LLM posture methods), 72h (Console DB schema for preferences / runtime registry / encrypted auth profiles / PAT store / notifications routing / keybindings). 73m introduces NO new Protocol methods beyond `auth.rotate_token` and adds NO new Console DB tables.
- The mock-mode banner per D-089 appears in Governance Posture AND LLM-Provider Posture cards when `LLMPosture.MockMode = true` (from 72g's `llm.posture` response). Text matches the stderr banner verbatim: `DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION`.

## Non-goals

- Authoring governance config (edit `IdentityTiers`). Post-V1 per page-settings.md §10.
- Authoring storage driver bindings. Operator config concern (restart-required per RFC §10).
- Cross-runtime fleet aggregator. D-091 — post-V1.
- `governance.rotate_key` (Phase 91 Post-V1) and `governance.swap_model` (Phase 92 Post-V1).
- Console-driven LLM provider swap. Post-V1.
- Re-shipping `runtime.info`, `runtime.health`, `runtime.counters`, `runtime.drivers`, `metrics.snapshot`, `governance.posture`, or `llm.posture` (those belong to 72f / 72g — this phase CONSUMES them).
- Re-shipping Console DB tables `profiles`, `runtime_registry`, `auth_profiles`, `pat_store`, `notifications_routing`, `keybindings`, `saved_filters`, `saved_views` (those belong to 72h — this phase CONSUMES them; if a column extension is needed on 72h's `profiles` table, it lands as an ADDITIVE forward migration owned by 72h's plan amendment, NOT a new table here).
- Re-implementing encryption-at-rest for auth profiles / PATs (72h ships the AES-GCM + PBKDF2 helpers per Brief 12; 73m calls them).

## Acceptance criteria

- [ ] `cmd/harbor/cmd_console.go` (or equivalent) implements the `harbor console` subcommand. Serves the SvelteKit build via `embed.FS`. Binds to a configurable port (default `127.0.0.1:18790`). Connects to one or more remote Runtimes via the Connected-Runtimes registry stored in 72h's Console DB (`runtime_registry` table). The subcommand is NEVER bundled into `harbor dev` per D-091 (binding §13 carve-out — verified by a smoke assertion that `harbor dev --help` does NOT advertise a console-serving flag).
- [ ] `internal/protocol/methods/methods.go` declares ONE new method: `auth.rotate_token`. The 4 runtime posture methods (`runtime.info`, `runtime.health`, `runtime.counters`, `runtime.drivers`, `metrics.snapshot`) are SHIPPED by Phase 72f; the 2 governance / LLM posture methods (`governance.posture`, `llm.posture`) are SHIPPED by Phase 72g. 73m's `internal/protocol/methods/methods.go` diff adds exactly one method name.
- [ ] `internal/protocol/types/auth.go` defines `AuthRotateTokenRequest` / `AuthRotateTokenResponse` (admin).
- [ ] `auth.rotate_token` requires the `console.admin` scope claim per D-066; degrades to 403 without; audit-emit on every successful rotation.
- [ ] Settings page UI (`web/console/src/routes/settings/+page.svelte`) renders all 12 cards per `docs/rfc/assets/console-settings-page.png` mockup with the left sub-nav rail anchor scrolling.
- [ ] Each Settings card reads its upstream source through the typed Protocol client at `web/console/src/lib/protocol.ts` (D-093):
  - Connected Runtimes → 72h's `runtime_registry` (Console DB).
  - Per-Runtime Auth → 72h's `auth_profiles` (Console DB; encrypted at rest via 72h's exported `decrypt(blob, kek)` helper).
  - API Tokens (Console-local PAT) → 72h's `pat_store` (Console DB; encrypted at rest).
  - Appearance / Time & Locale / Keybindings → 72h's `profiles` + `keybindings` (Console DB).
  - Notifications Routing → 72h's `notifications_routing` (Console DB) + 72d's `notification.*` event taxonomy (the mapper subscribes).
  - Runtime Info → 72f's `runtime.info` Protocol method.
  - Governance Posture → 72g's `governance.posture` Protocol method (read-only; edit deferred per §10).
  - Storage Drivers → 72f's `runtime.drivers` Protocol method (driver name + masked DSN + migration version).
  - LLM-Provider Posture → 72g's `llm.posture` Protocol method (provider / model / region / `MockMode`).
  - About → static Console content.
- [ ] PATs (Personal Access Tokens) are one-time-reveal at creation; the Console NEVER displays the raw token after the create flow closes. The persisted form is 72h's encrypted blob; the in-memory plaintext is dropped when the create modal closes.
- [ ] Mock-mode banner (`DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION`) renders in Governance Posture + LLM-Provider Posture cards when 72g's `llm.posture` response has `MockMode = true`.
- [ ] Email and webhook notification-routing transports gated by `console.admin`; in-Console toast + browser notification are no-elevation defaults. The transport-elevation check happens in the Console UI; the runtime never accepts a webhook-fan-out call from a non-admin operator.
- [ ] Design tokens only — no raw color/spacing/type-scale literals (§13).
- [ ] `svelte-check --fail-on-warnings` passes.
- [ ] All data flows go through the typed Protocol client. NO hand-rolled `fetch` in `.svelte` files.
- [ ] Per-page Playwright spec at `web/console/tests/settings-page.spec.ts` covers: 12 cards render, sub-nav anchors scroll, `+ Add Runtime` Console-DB round-trip, `Rotate token` scope-claim degradation, mock-mode banner conditional on backend `MockMode = true`.
- [ ] `scripts/smoke/phase-73m.sh` asserts (a) `auth.rotate_token` requires `console.admin` (the only net-new Protocol method this phase ships), (b) the `harbor console` subcommand boots + serves the embedded asset at `/` (200 OK).
- [ ] **Concurrent-reuse test:** N/A for the Go-side surface (the single net-new method `auth.rotate_token` has no per-request shared mutable state; the harbor console subcommand binds an HTTP handler whose concurrency is covered by Go's `net/http`). The corresponding test for the underlying primitives lives in 72f/72g/72h.
- [ ] **Integration test:** `test/integration/settings_page_test.go` — real runtime + real Console DB (via 72h's driver) + Protocol transport + identity scope; mock-mode banner end-to-end (boot runtime with `HARBOR_DEV_ALLOW_MOCK=1`, assert 72g's `llm.posture` returns `MockMode = true`, assert 73m's banner component renders); `auth.rotate_token` audit-event assertion; under `-race`.
- [ ] **`harbor console` boot test:** `test/integration/harbor_console_boot_test.go` asserts the subcommand starts + serves the static asset at `/` (200 OK) + binds to the configurable port + responds to `--help` exit 0.

## Files added or changed

```text
cmd/harbor/cmd_console.go                                # NEW: harbor console subcommand
cmd/harbor/cmd_console_test.go
cmd/harbor/console_embed.go                              # NEW: //go:embed web/console/build/* — the SvelteKit static build
internal/protocol/methods/methods.go                      # +1 method (auth.rotate_token)
internal/protocol/types/auth.go                           # +AuthRotateTokenRequest/Response (admin)
internal/protocol/transports/stream/auth_handler.go       # +rotate_token dispatch (extends existing auth handler)
internal/protocol/transports/stream/auth_handler_test.go
internal/protocol/auth/rotate_token.go                    # auth.rotate_token implementation
internal/protocol/auth/rotate_token_test.go
test/integration/settings_page_test.go
test/integration/harbor_console_boot_test.go              # cmd/harbor smoke under -race
web/console/src/routes/settings/+page.svelte
web/console/src/lib/components/settings/SubNavRail.svelte
web/console/src/lib/components/settings/ConnectedRuntimesCard.svelte   # consumes 72h's runtime_registry
web/console/src/lib/components/settings/PerRuntimeAuthCard.svelte       # consumes 72h's auth_profiles + 72h's decrypt() helper
web/console/src/lib/components/settings/APITokensCard.svelte            # consumes 72h's pat_store
web/console/src/lib/components/settings/AppearanceCard.svelte           # consumes 72h's profiles
web/console/src/lib/components/settings/TimeLocaleCard.svelte           # consumes 72h's profiles (tz + locale columns)
web/console/src/lib/components/settings/KeybindingsCard.svelte          # consumes 72h's keybindings
web/console/src/lib/components/settings/NotificationsRoutingCard.svelte # consumes 72h's notifications_routing + 72d's notification.* events
web/console/src/lib/components/settings/RuntimeInfoCard.svelte          # consumes 72f's runtime.info
web/console/src/lib/components/settings/GovernancePostureCard.svelte    # consumes 72g's governance.posture
web/console/src/lib/components/settings/StorageDriversCard.svelte       # consumes 72f's runtime.drivers
web/console/src/lib/components/settings/LLMPostureCard.svelte           # consumes 72g's llm.posture
web/console/src/lib/components/settings/AboutCard.svelte
web/console/src/lib/components/settings/MockModeBanner.svelte
web/console/tests/settings-page.spec.ts
web/console/src/lib/protocol.ts                                          # REGENERATED via make protocol-ts-gen (auth.rotate_token type added)
scripts/smoke/phase-73m.sh
docs/glossary.md                                                         # +auth.rotate_token, +"harbor console subcommand"
README.md                                                                # +pointer to harbor console subcommand (CLAUDE.md §4.2 rule 10)
docs/plans/README.md                                                     # flip 73m row from Pending to Shipped on merge
```

**Files NOT in this phase (owned by upstream phases):**

- `internal/protocol/types/runtime_info.go` — Phase 72f.
- `internal/protocol/types/runtime_storage.go` — Phase 72f.
- `internal/protocol/types/runtime_llm.go` — Phase 72g.
- `internal/protocol/types/governance.go` — Phase 72g.
- `internal/runtime/protocol/info.go`, `runtime.health`, `runtime.counters`, `runtime.drivers`, `metrics.snapshot` implementations — Phase 72f.
- `internal/governance/protocol/posture.go`, `llm_posture.go` implementations — Phase 72g.
- `web/console/src/lib/db/migrations/00X_*.sql` for `profiles` / `runtime_registry` / `auth_profiles` / `pat_store` / `notifications_routing` / `keybindings` / `saved_filters` / `saved_views` — Phase 72h.
- `web/console/src/lib/db/auth_profiles.ts` encrypt-at-rest helpers — Phase 72h.

## Public API surface

```go
// internal/protocol/types/auth.go (NEW additions only; existing types stay)
type AuthRotateTokenRequest struct {
    // Token rotation requires no body fields — the caller's identity is the JWT.
}

type AuthRotateTokenResponse struct {
    NewToken  string    `json:"new_token"`  // one-time-revealed; the operator copies this once.
    ExpiresAt time.Time `json:"expires_at"`
}
```

## Test plan

- **Unit:**
  - `internal/protocol/auth/rotate_token_test.go` — identity-rejection, `console.admin` scope-claim gating, audit-event emission shape.
  - `cmd/harbor/cmd_console_test.go` — subcommand argument parsing, embed.FS asset serving.
- **Integration:**
  - `test/integration/settings_page_test.go` — real runtime + 72h's real Console DB driver + Protocol transport; mock-mode banner end-to-end (boot with `HARBOR_DEV_ALLOW_MOCK=1`, assert 72g's `llm.posture` returns `MockMode = true`); `auth.rotate_token` audit-event assertion; cross-tenant Settings observation requires admin scope.
  - `test/integration/harbor_console_boot_test.go` — start `harbor console` on a test port, assert `/` serves the SvelteKit index, assert `/assets/*` serves the embedded assets, assert the subcommand respects port-override flag.
- **Conformance:**
  - The 1 new method (`auth.rotate_token`) runs against the Protocol conformance suite (Phase 62).
- **Concurrency / leak:**
  - N/A on the Go side for this phase (no new reusable artifact). The `cmd/harbor/cmd_console` HTTP handler relies on `net/http` concurrency which is already covered by stdlib tests.
- **UI (Playwright):**
  - `settings-page.spec.ts` — 12 cards render; sub-nav anchors scroll; `+ Add Runtime` round-trips through Console DB; `Rotate token` button hidden without `console.admin` claim; mock-mode banner conditional on backend `MockMode = true`.

## Smoke script additions

`scripts/smoke/phase-73m.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'auth/rotate_token' '{}'` without `console.admin` → expect 403.
- `harbor console --help` exit 0 (CLI-side smoke); `harbor console --port=18791 --datadir=$TMPDIR/harbor-console-smoke` boots + `/` returns 200 + tear down.
- Static assertion (grep): `harbor dev --help` output MUST NOT advertise a console-serving flag (D-091 binding rule).

## Coverage target

- `internal/protocol/auth/rotate_token`: 85% (security-critical).
- `cmd/harbor` (focus on `cmd_console.go` paths): 75%.
- `web/console/src/routes/settings/`: 70%.

## Dependencies

**Same-wave (Wave 13):**

- Phase 72 (events.subscribe scope foundation)
- Phase 72d (`notification.*` event family — consumed by Notifications Routing card's mapper)
- **Phase 72f** (runtime posture methods — `runtime.info`, `runtime.health`, `runtime.counters`, `runtime.drivers`, `metrics.snapshot` — consumed by Runtime Info / Storage Drivers cards)
- **Phase 72g** (`governance.posture` + `llm.posture` — consumed by Governance Posture + LLM-Provider Posture cards)
- **Phase 72h** (Console DB schema — `profiles` / `runtime_registry` / `auth_profiles` / `pat_store` / `notifications_routing` / `keybindings` tables consumed; encrypt/decrypt helpers consumed)
- Phase 75 (Playwright harness baseline)

**Already shipped (pre-Wave 13):**

- Phase 36a (Cost accumulator + per-identity ceilings — `Shipped`)
- Phase 36b (Per-identity rate limits + MaxTokens — `Shipped`)
- Phase 59 (Protocol versioning + deprecation policy — `Shipped`)
- Phase 60 (Protocol wire transport — `Shipped`)
- Phase 61 (Protocol auth — `Shipped`)
- Phase 64 (`harbor dev` v1, with mock-mode banner per D-089 — supplies the boot-time `HARBOR_DEV_ALLOW_MOCK=1` capture path that 72g's `LLMPosture.MockMode` reads)

## Risks / open questions

- **`harbor console` port collision.** Default `127.0.0.1:18790` might conflict with other dev tooling. Default is overridable via `--port` flag; smoke asserts an alternate port works.
- **PAT one-time-reveal UX.** A wrong-click after creation strands the PAT; the operator must regenerate. Acceptable; matches industry pattern.
- **Notifications-routing transports beyond `in_app`.** V1 wires the `in_app` transport only per page-settings.md §10; `email` / `webhook` / `web_push` rows render in the matrix as disabled-with-tooltip ("Transport delivery is post-V1"). The 72h `notifications_routing` table schema accepts all 4 transport values (forward-compat) but no deliverer code lands in V1 — that's a post-V1 phase.
- **Cross-page `Rotate token` ↔ `Per-Runtime Auth` row sync.** A successful `auth.rotate_token` invalidates the operator's current 72h-persisted auth profile for the connected runtime; the Per-Runtime Auth card MUST refresh its row immediately and force the operator to re-enter the new token. Implemented as a client-side cache-bust on the rotate-success event.

## Glossary additions

- **`auth.rotate_token`** — Admin Protocol method rotating the operator's current Protocol-auth token. Requires `console.admin` claim. One-time-reveal response; the encrypted persistence (the operator re-saving the new token into 72h's `auth_profiles`) is the Console's job, not the Runtime's.
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
- [ ] **Concurrent-reuse test:** N/A — this phase ships no new reusable Go artifact (mark with one-line reason)
- [ ] **Integration test passes** — `settings_page_test.go` + `harbor_console_boot_test.go` (§17)
- [ ] **Per-page Playwright spec lands in this phase's PR**
- [ ] **`harbor console` subcommand boot tested** — smoke asserts `/` serves; integration asserts asset paths + port-override
- [ ] **`harbor dev` does NOT advertise console-serving** — smoke greps `harbor dev --help` (D-091 binding rule)
- [ ] **README.md updated** — pointer to `harbor console` subcommand (CLAUDE.md §4.2 rule 10 — new CLI subcommand needs README update)
- [ ] **`docs/plans/README.md` row 73m flipped to Shipped on merge** (CLAUDE.md §4.2 rule 11)
- [ ] Glossary updated
- [ ] **No duplicate ownership with 72f / 72g / 72h** — 73m's `internal/protocol/methods/methods.go` diff adds exactly one method name (`auth.rotate_token`); no Console DB migrations live in 73m; no runtime / governance / LLM posture types live in 73m
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review
