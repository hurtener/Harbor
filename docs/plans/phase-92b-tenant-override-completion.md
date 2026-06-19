# Phase 92b — tenant-override-completion

## Summary

Phase 92 (D-231) shipped the **tenant layer** of the next-turn LLM-override mechanism — an admin-set, StateStore-backed tenant default (model + additive extra-instructions + temperature + max-tokens + reasoning-effort), applied to every session's next run via `RunContext.LLMOverrides`, audited, admin-scoped. It deliberately deferred three follow-ups (recorded in the PR #357 closeout and D-231). Phase 92b closes all three so the tenant-override feature is complete end-to-end:

1. **Session-level override apply seam + the model field.** The session next-turn override (`runs.set_overrides` → `RunOverrides` → the in-process `Store`) is **set-only in production** — `Store.Consume` is called from no non-test code, so a recorded session override never reaches a run. Phase 92b wires the **apply seam**: the run loop consumes the session's pending override at run start and resolves it **above** the tenant default (the resolution order D-231 reserved: **session › tenant › config**). It also adds the `Model` field to `RunOverrides` (the originally-planned session model swap), so the session layer carries the same field set as the tenant layer.

2. **Typed Console admin UI control.** Phase 92 shipped the `governance.set_tenant_overrides` / `get_tenant_overrides` Protocol methods + the typed run-loop/planner consumers, but the Console UI consumer is a follow-up: the five wire types are currently in the `protocol-ts-untyped-allow.json` allow-list. Phase 92b ships the typed TS interfaces (removing the allow-list entries) and a Console admin control (a Settings/Governance affordance) that sets + reads a tenant's default overrides — the §13 "Protocol surface gets its Console consumer" closure for this feature.

3. **Multi-replica cache freshness.** `governance.TenantOverridePolicy` lazy-loads each tenant's record once and never re-reads (the cost-accumulator cache model). Within one runtime process the writer (the Protocol handler) and reader (the run loop) share the instance, so a set is immediately visible — but across multiple runtime replicas over a shared Postgres store, a replica that has already loaded a tenant's record serves the prior default until restart. Phase 92b adds a cross-replica freshness path so an admin set on replica A lands on replica B's next run, making D-231's "every session's next run" guarantee hold under multi-replica deployment.

## RFC anchor

- RFC §6.15
- RFC §7

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- RFC §6.15: Governance owns identity-scoped policy between the Runtime and the `LLMClient`; the `ModelOverride` seam (realised tenant-side in Phase 92) is the home for the session arm too — the session override is a runtime next-turn affordance (the Playground), the tenant default is the governance policy, and they compose in ONE run-start resolver (session › tenant › config). The session arm is NOT a second mechanism — it reuses the existing `runs.set_overrides` Store, finally CONSUMED.
- D-231 (Phase 92): the resolver was built to compose a session arm above the tenant arm without a rewrite; 92b adds the `Store.Consume` call + the `Model` field. The policy's documented multi-replica staleness limitation (the cost-accumulator lazy-load model) is the third item 92b closes.
- D-025 (compiled artifacts immutable; per-run state in `ctx`/`RunContext`; next-turn only): the session override stays a run-start snapshot pinned into `RunContext.LLMOverrides` — consuming it at run start preserves next-turn-only. The cross-replica freshness path MUST NOT make the policy a mutable shared artifact (the cache stays internally synchronised; a re-read/invalidation is per-tenant, not global mutable state).
- RFC §7 + D-121 (Console conventions): the admin control is a Console page/affordance built on the shared `HarborClient` + `<PageState>` + tokens; the typed wire client replaces the allow-list entries (D-093/D-223 lockstep — the manifest already carries the types; 92b adds the hand-written TS interfaces and drops the allow-list rows).

## Findings I'm departing from (if any)

None. Phase 92b is the explicit completion of the three follow-ups D-231 deferred; it does not depart from any brief or decision — it discharges them.

## Goals

- `RunOverrides` gains `Model *string`; `runs.set_overrides` validates it against the configured `ModelProfiles` (fail loud on unknown, mirroring the tenant layer) and records it in the existing session Store.
- The run loop **consumes** the session's pending override at run start (the seam built but unwired in Phase 92) and resolves the effective override as **session › tenant › config**, pinned once into `RunContext.LLMOverrides`. The session override is one-shot (consumed) per its existing contract.
- Typed Console TS interfaces for `GovernanceTenantOverrides` + the four request/response types (removing the five `protocol-ts-untyped-allow.json` entries); a Console admin control that calls `governance.set_tenant_overrides` / `get_tenant_overrides`.
- Cross-replica freshness for `TenantOverridePolicy`: an admin set is observed by other replicas' run-start resolution within a bounded window (mechanism chosen at implementation — candidates: a per-read freshness check, a bus/`MessageBus` invalidation signal, or a short per-tenant TTL; the choice is recorded in the implementation decision).
- Full identity scoping + audit preserved on every path; the session arm keeps the owner/cross-session-scope authorization `runs.set_overrides` already enforces.

## Non-goals

- **Versioning / diff / rollback** of overrides — still Phase 92a's mandate.
- **Prompt/tools/skills** desired-state beyond the LLM-parameter set — Phase 92a.
- **Re-architecting the policy off the cost-accumulator cache model** — 92b adds a freshness path, it does not replace the StateStore-backed design.
- **A new model-selection runtime** — reuses the `ModelProfile` selection + the apply path Phase 92 built.

## Acceptance criteria

- [ ] `runs.set_overrides` accepts a `Model` override; an unknown model is rejected (fail loud); a valid one is recorded in the session Store.
- [ ] The run loop consumes the session pending override at run start and applies it; an override recorded for a session changes that session's NEXT run's LLM request, then is gone (one-shot).
- [ ] **Resolution order holds**: a session override wins over a tenant default wins over config; verified by a test exercising all three precedence cases (including session-and-tenant-both-set).
- [ ] Typed Console TS interfaces exist for the five governance tenant-override wire types; the five `protocol-ts-untyped-allow.json` entries are removed and `npm run lint` (the TS lockstep guard) stays green.
- [ ] A Console admin control sets + reads a tenant's default overrides through the typed client (no hand-rolled `fetch`); admin-gated; built on the shared Console foundation (D-121).
- [ ] **Multi-replica freshness**: an admin set on one runtime instance is reflected in another instance's next-run resolution within the bounded window, verified by an integration test over a shared StateStore modelling two instances.
- [ ] Identity isolation preserved on the session + tenant + cross-replica paths; next-turn-only preserved (no mid-run change).

## Files added or changed

- `internal/protocol/types/runs.go` — `RunOverrides.Model *string`.
- `internal/runtime/runs/protocol/overrides.go` — validate + record `Model`; expose the consume path used by the run loop.
- the run-start resolution site (`cmd/harbor/cmd_dev_runloop.go` + the `harbortest/devstack` mirror) — consume the session override + compose session › tenant › config into `RunContext.LLMOverrides`.
- `internal/governance/tenantoverride.go` — the cross-replica freshness mechanism (invalidation / re-read / TTL) behind the existing concurrency contract.
- `internal/protocol/singlesource/singlesource.go` — register `RunOverrides.Model` reflection coverage (the manifest already carries `RunOverrides`); regenerate the wire manifest.
- `web/console/src/lib/protocol/*.ts` — typed governance tenant-override interfaces; `web/console/scripts/protocol-ts-untyped-allow.json` — remove the five entries; `web/console/src/lib/protocol/wire-manifest.gen.json` (regen).
- `web/console/src/routes/(console)/...` + `web/console/src/lib/...` — the admin tenant-override control (consumes the typed client).
- `docs/site/protocol/*` (regen) — `RunOverrides` gains a field; the methods/types pages re-emit.
- `scripts/smoke/phase-92b.sh`; `docs/skills/*` (§18 — the protocol + console skills); `docs/glossary.md`; `docs/decisions.md` (D-232); `docs/plans/README.md`.

## Public API surface

- `runs.set_overrides`: additive `Model *string` on `RunOverrides` (backward-compatible; nil = no change).
- No new Protocol method (the session arm reuses `runs.set_overrides`; the tenant methods already shipped in Phase 92). The typed TS client gains the governance tenant-override interfaces (no wire change — they were allow-listed before).
- No change to the `Subsystem` / `LLMClient` / planner interfaces (the apply primitive `RunContext.LLMOverrides` already exists).

## Test plan

- **Unit:** `RunOverrides.Model` validation (valid / unknown / nil); the session-override consume-at-run-start path; the three-layer resolver precedence (session / tenant / config + both-set); the cross-replica freshness mechanism (a second policy instance over the same store sees a set within the window).
- **Integration:** `test/integration/tenant_override_completion_test.go` — real StateStore + governance + run loop: session-over-tenant application, one-shot consume, multi-replica freshness (two instances over one shared store), identity isolation, next-turn-only. Under `-race`.
- **Console:** the typed client unit test (the governance interfaces round-trip); the admin control's four `<PageState>` branches; `svelte-check --fail-on-warnings` + `npm run lint` (the TS lockstep guard with the allow-list entries removed).
- **Concurrency / leak:** concurrent session+tenant resolutions + the freshness path under `-race`.
- **Protocol lockstep:** `make protocol-ts-gen-check` + `make protocol-docs-gen-check` green (the `RunOverrides.Model` field + the de-allow-listed types).

## Smoke script additions

`scripts/smoke/phase-92b.sh`:

- Static: `RunOverrides` has a `Model` field; the session-override consume seam is wired in the run loop; the five governance types are no longer in the allow-list (typed instead).
- Live (preflight dev server): a `runs.set_overrides` with a `Model` applies on the next run; a session override wins over a set tenant default; the admin Console control route round-trips (admin-gated).

## Coverage target

- Touched governance + runs-overrides packages: 85%.
- `internal/protocol/*` + Console touched lines: no regression; the Console control meets the page coverage bar.

## Dependencies

- 92 (the tenant layer + the apply primitive `RunContext.LLMOverrides` + the resolver scaffold this phase completes)
- 73 (Console attaching — the admin control's host surface)
- (prerequisite, shipped) `runs.set_overrides` + the session Store; the `ModelProfile` selection + the LLM apply edge.

## Risks / open questions

- **Cross-replica mechanism choice.** Per-read freshness check (a StateStore read per run start — simple, adds a read per run), vs a `MessageBus`/bus invalidation signal (lower steady-state cost, needs the durable bus from Phase 86 wired), vs a short per-tenant TTL (bounded staleness, no extra infra). Recommend the per-read check for V1 simplicity (the run loop already does StateStore reads at run start) unless profiling shows it matters; pin the choice in D-232. The mechanism MUST stay inside the policy's concurrency contract (no global mutable state).
- **Session-override one-shot vs tenant persistent.** The session override is consumed (one-shot, per its existing contract); the tenant default persists. The resolver composes a consumed session override over a persistent tenant default — confirm the consume happens exactly once at run start and does not race the tenant read.
- **Console placement.** The admin tenant-override control could live on a Settings/Governance page or a dedicated control; confirm placement against D-121 conventions in the page phase plan section.
- **Allow-list removal is load-bearing.** Removing the five `protocol-ts-untyped-allow.json` entries makes the typed interfaces mandatory — the TS lockstep guard will fail if a field drifts. This is the intended tightening (the §13 "type it once a consumer exists" closure).

## Glossary additions

- **session-level override apply seam** — the run-start consume of a session's pending `runs.set_overrides` record (`Store.Consume`), wired in Phase 92b. Resolves ABOVE the tenant default (session › tenant › config) into `RunContext.LLMOverrides`. One-shot: consumed at the next run, then gone. Phase 92b, D-232.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (doc-only planning PR may skip the local preflight via `HARBOR_PREFLIGHT_SKIP=1` with justification; CI gates — this plan PR carries no code)
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (at implementation)
- [ ] If multi-isolation paths changed: cross-session + cross-tenant isolation test passes (binding at implementation)
- [ ] **If this phase builds a reusable artifact (the freshness path on the policy): concurrent-reuse test passes** — N≥100 under `-race` (binding at implementation)
- [ ] **Integration test exists** — `test/integration/tenant_override_completion_test.go`, real StateStore + governance + run loop, session-over-tenant, multi-replica freshness, isolation, next-turn-only, under `-race` (binding at implementation)
- [ ] Protocol changed (`RunOverrides.Model`) → `make protocol-ts-gen-check` + `make protocol-docs-gen-check` + a smoke assertion (binding at implementation)
- [ ] Console page → `svelte-check --fail-on-warnings` + `npm run lint` green; built on the shared foundation (D-121); no raw literals / hand-rolled fetch (binding at implementation)
- [ ] Operator skill(s) updated for the changed surface (§18 — protocol + console)
- [ ] If new vocabulary: glossary updated (session-level override apply seam)
- [ ] decisions.md entry filed (D-232 — the session apply seam + the cross-replica freshness mechanism)
