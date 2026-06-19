# Phase 92 — console-model-swap

## Summary

Ships **Console-driven mid-session model swap** at **two scopes**, both taking effect on the next run (never mid-flight), so an operator changes the model live with no redeploy:

1. **Session-level** — extend the existing `runs.set_overrides` next-turn override mechanism (which already carries reasoning-effort / temperature / max-tokens / system-prompt) with a `Model` field. The session owner (or an admin acting on one session) swaps the model for that session's next turn. Reuses the proven projection, the verified-identity + cross-session-scope authorization, the TS lockstep client, and the Console Playground consumer.
2. **Tenant-level admin default** — a new **admin-scoped** Protocol method `governance.swap_model` that sets a **tenant-scoped** default model (StateStore-backed, the RFC §6.15 `ModelOverride` governance seam), audited, applied to **every** session in the tenant on its next run. This is the "an administrator selects a new default model for the whole tenant without a deploy" workflow — today the default lives in `cfg.LLM.Model` and requires a redeploy to change.

The effective model for a run is resolved at **run start** (a D-025-clean snapshot, immutable for the run; the planner sees it via `RunContext`): **session override › tenant default › config default (`cfg.LLM.Model`)**. The next run re-resolves, so a swap at either layer lands on the next turn. Both layers validate the target model has a configured `ModelProfile` (fail loud with `ErrUnsupportedModel`) and emit an audit/governance event.

Versioning + diff/rollback + the broader agent-config (prompt / tools / skills) are **out of scope** — they are Phase 92a's mandate; Phase 92 is the model facet, and its tenant-default layer is the first concrete of the 92a "admin-controlled, next-turn, no-deploy desired-state" pattern.

## RFC anchor

- RFC §6.15
- RFC §6.5

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- RFC §6.15: Governance is the identity-scoped middleware between the Runtime and the `LLMClient`; it owns the `ModelOverride` seam (stubbed `type ModelOverride interface { /* mid-session model swap (post-V1) */ }`). The tenant-default layer realises that seam as an admin-scoped, audited, identity-scoped policy. The session layer is a runtime next-turn override (the Playground affordance), not a governance policy — the two scopes have different homes by design.
- D-025 (compiled artifacts are immutable; per-run state in ctx/RunContext): a running planner's LLM client/model is fixed for the run. The swap therefore cannot mutate a live run — it updates a session/tenant desired-state that the **next** run snapshots at start. This is the same next-turn-only invariant `runs.set_overrides` already honours for reasoning-effort/temperature, and that Phase 110a's per-run projection established.
- D-219 (steering authority from the verified ctx, not the request body): the tenant-default method authorizes on the **verified** JWT scope — an admin scope is required to change a tenant-wide default; the session override keeps the existing owner/cross-session-scope check.

## Findings I'm departing from (if any)

The master-plan detail block for Phase 92 names a single new `governance.swap_model` Protocol method. Implementation discovery: the runtime already ships the exact session-scoped **next-turn override** mechanism (`runs.set_overrides` → `RunOverrides` → run-start projection, with verified-identity + cross-session-scope rejection), currently carrying reasoning-effort / temperature / max-tokens / system-prompt. So the **session-level** model swap is a one-field extension of that proven mechanism, NOT a new method (reusing it avoids a §13 duplicate-mechanism smell). The new `governance.swap_model` method is retained for the **tenant-level admin default** scope only — where its governance framing (admin scope, audit, tenant-wide, the `ModelOverride` seam) is exactly right and `runs.set_overrides` (session-scoped) cannot reach. This two-layer split is filed as **D-231**.

## Goals

- `RunOverrides` gains `Model *string`; `runs.set_overrides` validates it (target must have a `ModelProfile`, else `CodeInvalidRequest` wrapping `ErrUnsupportedModel`), stores it in the existing session-scoped pending-override store, and emits the existing `run.overrides_set` event extended with a model flag. Applied next-turn via the existing projection.
- A new `governance.swap_model` Protocol method: admin-scoped, sets a **tenant-scoped** default model through a governance `ModelOverride` policy (StateStore-backed, in-mem / SQLite / Postgres conformance), audited, emits `governance.model_swapped`.
- Run-start resolution of the effective model: **session override › tenant default › `cfg.LLM.Model`** — snapshotted into `RunContext`, immutable for the run; the planner sees it; the next run re-resolves.
- No redeploy: the tenant default lives in the StateStore (not `cfg`), so an admin changes it live and it lands on every session's next run.
- Full `(tenant, user, session)` identity scoping + audit on both layers; admin-scope gate on the tenant layer.

## Non-goals

- **Versioning / diff / rollback** of the model (or any agent-config). Single current value per layer; the versioned, diffable, rollback-able agent-config control plane is Phase 92a.
- **Swapping prompt / tools / skills.** Model only. (`SystemPromptOverride` already exists on `RunOverrides` at the session layer; this phase does not touch it. The tenant-level prompt/tools/skills are 92a.)
- **Mid-run / mid-flight model change.** Next-turn only — a running planner's model is pinned (D-025).
- **Provider / key changes.** Key rotation is Phase 91; this phase swaps the model name (which selects a `ModelProfile`, hence provider/profile), not the credential.
- **A new model-selection runtime.** Reuses the existing `ModelProfile` selection + the `LLMClient`/bifrost path; the swap changes which model name the run requests.

## Acceptance criteria

- [ ] `runs.set_overrides` accepts a `Model` override; an unknown model (no `ModelProfile`) is rejected with `CodeInvalidRequest` (wrapping `ErrUnsupportedModel`); a valid one is applied to the session's NEXT run and the planner's LLM request uses it.
- [ ] `governance.swap_model` sets a tenant-scoped default model: admin scope required (non-admin → `CodeScopeMismatch`/forbidden); unknown model rejected; the change is audited + emits `governance.model_swapped`.
- [ ] **Run-start resolution order** holds: session override wins over tenant default wins over `cfg.LLM.Model`; verified by a test exercising all three precedence cases.
- [ ] **Next-turn-only**: a swap during an in-flight run does NOT change that run's model; the following run picks it up (D-025 snapshot).
- [ ] **No redeploy**: setting a tenant default changes the effective default for a fresh session with no session override, without restarting the runtime.
- [ ] Identity isolation: a tenant-default set for tenant A never affects tenant B; a session override never crosses sessions.
- [ ] Protocol surface: both methods are in `methods.go` (single source), mirrored into the TS lockstep client + the regenerated wire manifest, the generated Protocol docs are regenerated, and `scripts/smoke/phase-92.sh` exercises both.
- [ ] Operator skill(s) updated (§18): the model swap is documented where the Console/Protocol surface is.

## Files added or changed

- `internal/protocol/types/runs.go` — `RunOverrides.Model *string` (single-source wire type).
- `internal/runtime/runs/protocol/overrides.go` — validate + store + event the `Model` override (mirrors the reasoning-effort path).
- `internal/protocol/methods/methods.go` — `MethodGovernanceSwapModel = "governance.swap_model"`.
- `internal/governance/...` — the `ModelOverride` policy (tenant-scoped default model; StateStore-backed; the §4.4 seam) + its conformance.
- `internal/runtime/governance/protocol/` (or the governance protocol home) — the `governance.swap_model` handler (admin-scope gate + validate + set + audit + event).
- the run-start model-resolution point (run-loop / planner runtime LLM-request construction) — resolve session › tenant › config into `RunContext`.
- `internal/protocol/errors/errors.go` — any new error code (if needed; prefer reusing `CodeInvalidRequest` / `CodeScopeMismatch`).
- `web/console/src/lib/protocol/*.ts` + `wire-manifest.gen.json` (regen) — the `Model` field + the new method (D-093/D-223 lockstep).
- `docs/site/protocol/*` (regen via `make protocol-docs-gen`) — the new method + event.
- `web/console/...` — the Playground model picker (session layer) + the admin tenant-default control (consumer; per §13 the Protocol surface lands here, the Console UI consumes it).
- `internal/audit/...` or the event taxonomy — `governance.model_swapped` event registration.
- `scripts/smoke/phase-92.sh`; `docs/CONFIG.md` / `examples` if a config surface is added; `docs/skills/*` (§18); `docs/glossary.md`; `docs/decisions.md` (D-231); `docs/plans/README.md` + `README.md`.

## Public API surface

- `runs.set_overrides`: additive `Model *string` on `RunOverrides` (backward-compatible; nil = no change).
- New Protocol method `governance.swap_model` (request: `{identity, model}`; admin-scoped; tenant-scoped effect). New canonical event `governance.model_swapped`.
- New governance `ModelOverride` policy interface concrete (behind the §4.4 seam). No change to `Subsystem` / `LLMClient` interfaces.

## Test plan

- **Unit:** `RunOverrides.Model` validation (valid / unknown-model / nil); the `ModelOverride` policy (set / get / tenant-scoped isolation / unknown-model reject); the run-start resolution order (3 precedence cases); admin-scope gate on `governance.swap_model`.
- **Integration:** `test/integration/model_swap_test.go` — real StateStore (inmem + sqlite) + real governance + a run-loop; assert next-turn application at both scopes, the resolution order, tenant isolation, no-mid-run-change, and the audit event on the bus. Under `-race`.
- **Conformance:** the governance + tasks/runs conformance suites stay green; the `ModelOverride` policy joins the governance conformance suite (StateStore triad parity).
- **Concurrency / leak:** concurrent swaps + concurrent run-start resolutions under `-race` (the resolver is a reusable artifact).
- **Protocol lockstep:** `make protocol-ts-gen-check` + `make protocol-docs-gen-check` green.

## Smoke script additions

`scripts/smoke/phase-92.sh`:

- `methods.go` declares `governance.swap_model`; `RunOverrides` has a `Model` field (static).
- Live (preflight dev server): `runs.set_overrides` with a `Model` applies on the next run (`tasks.get`/run reflects the model); `governance.swap_model` with admin scope sets a tenant default and a fresh session picks it up; a non-admin `governance.swap_model` is rejected; an unknown model is rejected.

## Coverage target

- Touched governance + runs-overrides packages: 85%.
- `internal/protocol/*` touched lines: no regression.

## Dependencies

- 36a (governance cost accumulator + the Account/policy + StateStore-backed governance state — the `ModelOverride` policy rides this)
- 60 (Protocol transport — how methods reach the runtime)
- 73 (Console attaching — the consumer surface)
- (prerequisite, already shipped) `runs.set_overrides` + `RunOverrides` + the run-start projection; the `ModelProfile` selection + `LLMClient`/bifrost path; D-219 verified-ctx authority.

## Risks / open questions

- **Two surfaces, two scopes (settled with the operator):** session layer = extend `runs.set_overrides` (`Model` field, owner-scoped); tenant layer = new `governance.swap_model` (admin-scoped, tenant default). Confirm the method name `governance.swap_model` for the tenant layer (vs a clearer `governance.set_default_model`) — the master plan uses `swap_model`; pin in D-231.
- **Run-start resolution location.** The effective-model resolution (session › tenant › config) must happen exactly once at run start and pin into `RunContext` (D-025), not per-LLM-call in governance `PreCall` (which would change the model mid-run on the next step — violating next-turn-only). Confirm the run-loop's model-selection point is the projection site; the existing `RunOverrides` projection is the model to follow.
- **Tenant-default store + scoping.** The `ModelOverride` policy record is keyed by **tenant** (not session) — all users/sessions in the tenant inherit it. Confirm tenant-only keying + that it composes with the per-session override + the per-(identity, model) cost/rate accumulators (cost tracked under the effective model — fine, the accumulator already keys per model).
- **Validation timing.** Reject an unknown model at **swap time** (fail loud), not deferred to call time — both layers validate against the configured `ModelProfiles`. (Open: should a tenant default to a model with no `ModelProfile` be rejected, or accepted-and-warned? Recommend reject — fail loud.)
- **92a relationship.** The tenant-default layer is the first concrete of 92a's admin-controlled, next-turn, no-deploy desired-state. 92a generalises it to a **versioned** registry over model + prompt + tools + skills with diff/rollback. Phase 92 ships the un-versioned model facet; 92a may later absorb the tenant-default store into the versioned registry (documented so it is not re-litigated).
- **Console consumer (§13).** The Protocol methods land in this phase; the Console UI (Playground model picker + admin tenant-default control) is the consumer. Confirm whether the Console UI ships in 92 or rides 92a's Console work — at minimum the live test drives the Protocol surface directly.

## Glossary additions

- **mid-session model swap** — changing the model a session (or a whole tenant) uses, taking effect on the **next** run (never mid-flight; a D-025 run-start snapshot). Session scope rides `runs.set_overrides` (`RunOverrides.Model`); tenant scope is the admin-scoped `governance.swap_model` (the RFC §6.15 `ModelOverride` seam). Effective model resolves session › tenant › `cfg.LLM.Model`. Phase 92, D-231.
- **`governance.swap_model`** — the admin-scoped Protocol method that sets a tenant-scoped default model live (no redeploy), audited; the tenant-level half of the mid-session model swap. Phase 92, RFC §6.15, D-231.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (doc-only planning PR may skip the local preflight via `HARBOR_PREFLIGHT_SKIP=1` with justification; CI gates — this plan PR carries no code)
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (at implementation)
- [ ] If multi-isolation paths changed: cross-session + cross-tenant isolation test passes (both swap scopes are identity-scoped — binding at implementation)
- [ ] **If this phase builds a reusable artifact (the model resolver / the ModelOverride policy): concurrent-reuse test passes** — N≥100 under `-race` (binding at implementation).
- [ ] **Integration test exists** — `test/integration/model_swap_test.go`, real StateStore + governance + run-loop, both scopes, resolution order, tenant isolation, next-turn-only, audit event, ≥1 failure mode, under `-race` (Deps names shipped phases — binding at implementation).
- [ ] Protocol changed → `methods.go` single-source + TS lockstep mirrored + `make protocol-ts-gen-check` + `make protocol-docs-gen-check` + a smoke assertion (binding at implementation).
- [ ] Operator skill updated for the changed surface (§18).
- [ ] If new vocabulary: glossary updated (mid-session model swap, governance.swap_model)
- [ ] If a brief finding / master-plan detail was departed from: justified above + decisions.md entry filed (D-231 — the two-layer session/tenant split reusing runs.set_overrides)
