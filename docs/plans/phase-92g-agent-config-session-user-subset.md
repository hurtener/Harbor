# Phase 92g — agent-config control plane: session-user safe subset

## Summary

Opens the lower tier of the agent-config authorization matrix (D-235): a session-scoped (non-admin) end user gets a SAFE subset of control — set the **user** prompt layer (composes above the operator base, can't weaken it), enable/disable among **already-allowed** sources (a narrowing of the admin-set exposure, never a widening), and manage **ephemeral personal skills**. Everything capability-expanding (base prompt, add/remove servers, widening the tool allowlist) stays admin-only. The composition data model + a scope-derived gate enforce the boundary; authority comes from the verified ctx scope, never the request body.

## RFC anchor

- RFC §6.16 — Agent Registry (fleet observation vs control privilege tiers, D-066 — extended to the config-mutation lower tier).
- RFC §5.5 — JWT scope (the verified scope the gate derives authority from).
- RFC §6.15 — Governance (the desired-state pattern).

## Briefs informing this phase

- brief 11
- brief 09

## Brief findings incorporated

- **brief 11 (console feature surface):** the operator vs end-user surfaces are distinct; an end user gets a constrained, safe set of controls. This phase ships exactly the safe subset (user prompt layer, narrow-only source toggles, ephemeral skills) as a session-scoped Protocol surface, leaving the capability surface admin-only.
- **brief 09 (agent-as-actor / scope):** privilege tiers are scope-claim-derived, not body-derived. This phase derives the session-user tier from the verified ctx scope (no admin claim → the safe subset only), fail-closed, mirroring the steering verified-identity authority model (D-219).

## Findings I'm departing from (if any)

None.

## Goals

- A session-scoped (non-admin) Protocol path for the safe subset: set the `PromptLayers.User` layer (92e), narrow-only source enable/disable within the admin-set allowed set (a subset of 92d's exposure — a session user can only DISABLE among already-allowed, never enable a not-allowed source), and ephemeral personal skills (92c, session-scoped).
- The authorization gate derives the tier from the verified ctx scope: no admin claim → only the safe-subset verbs/fields are accepted; a session user attempting a capability-expanding edit (base prompt, add server, widen allowlist) is rejected with `CodeScopeMismatch`, fail-closed.
- The base/user prompt composition (92e) is the structural enforcement: a session user can only write `User`, never `Base` — enforced by the data model + the gate, both.
- Ephemeral personal skills are session-scoped and do not promote to the agent/tenant level (no cross-scope promotion, §6).

## Non-goals

- The admin path for any of these (admin already has the full surface via 92a/c/d/e).
- A new persistence tier — session-user edits are revisions in the SAME registry, scoped by the session in the identity triple (the safe subset is a scope gate over the existing surface, not a new store).
- The Console rendering of the safe subset for end users (the operator Console is 92h; a white-label end-user UI is downstream, not this repo).
- Widening any capability (explicitly forbidden — the whole point is narrow-only).

## Acceptance criteria

- [ ] A non-admin (session-scoped) caller CAN: set `PromptLayers.User`; disable a source/tool that is within the admin-allowed set (narrow-only); upsert/delete an ephemeral session-scoped personal skill. Each records a revision (or session-scoped state) and emits its event.
- [ ] A non-admin caller CANNOT: set `PromptLayers.Base`; add/remove an MCP connection; enable a source NOT in the admin-allowed set; widen the tool allowlist — each rejected with `CodeScopeMismatch`, fail-closed (authority from verified ctx, D-219/D-235).
- [ ] Narrow-only is enforced: a session disable is intersected with the admin-set exposure; a session attempt to enable beyond the admin set is a no-op-or-reject, never a widening.
- [ ] The base prompt is unwritable by a session user — both the data model (only `User` is in the session-writable shape) and the gate enforce it; a test asserts a session user cannot mutate `Base`.
- [ ] Ephemeral personal skills are session-scoped; they never promote to agent/tenant scope (§6 isolation; a test asserts no cross-session/scope bleed).
- [ ] Identity scoped by the full triple; the tier derives from the verified scope, never the body.
- [ ] TS manifest + typed client + generated docs regenerated; `scripts/smoke/phase-92g.sh` green.

## Files added or changed

- `internal/runtime/agentcfg/protocol/` — the session-user verbs (or a scope-gated branch in the existing methods) + the tier-derivation gate (verified-scope → allowed fields/verbs).
- `internal/protocol/{methods/methods.go,types/agentconfig.go,singlesource,transports/stream/agentconfig_handler.go}` + generator typeindex — any new session-scoped method shapes + the gate (non-admin allowed on the safe verbs).
- `internal/runtime/agentcfg/projection/projection.go` — compose the session user-layer + the narrowed exposure at run start (intersect session narrowing with admin exposure).
- `cmd/harbor/cmd_dev_runloop.go` + `harbortest/devstack/devstack.go` — apply the narrowed projection (D-094 twin).
- `web/console/src/lib/protocol/agentconfig.ts` + `client.ts`; `docs/site/protocol/*`; `docs/skills/...`.
- `scripts/smoke/phase-92g.sh`.

## Public API surface

```go
// The safe subset is a SCOPE-GATED view of the existing agent_config surface:
//   - non-admin: set PromptLayers.User; narrow-only source/tool disable; ephemeral personal skills.
//   - the gate derives the tier from auth scope (verified ctx), fail-closed.
```

## Test plan

- **Unit:** the tier gate (admin → full; non-admin → safe subset only; capability-expanding edit → CodeScopeMismatch); narrow-only intersection (session disable within admin set; enable-beyond rejected); base-prompt unwritable by session user; ephemeral-skill session scoping.
- **Integration:** `test/integration/agentcfg_session_user_test.go` — real registry + bus + handler with admin AND non-admin scoped contexts: admin sets allowed sources + base prompt → a non-admin sets a user layer + disables one allowed source + adds a personal skill → the next run reflects the narrowed exposure + composed user layer → a non-admin base-prompt/add-server attempt is 403 → cross-session isolation of the personal skill; under `-race`.
- **Conformance:** reuses the 92a `agentcfg` conformance.
- **Concurrency / leak:** N≥100 concurrent mixed admin/non-admin edits under `-race`; no cross-session bleed of the safe-subset state.

## Smoke script additions

- `scripts/smoke/phase-92g.sh`: static — the tier-gate symbol + the session-user verbs/branch + the narrow-only projection + typed client + generated-docs rows; live (skip-if-404) — a non-admin token sets a user layer (200) and is rejected setting the base prompt (403); admin sets both (200).

## Coverage target

- `internal/runtime/agentcfg/protocol` (the tier gate + session verbs): 85%

## Dependencies

- 92a (registry), 92c (skills — ephemeral personal skills), 92d (exposure — the narrow-only base), 92e (the layered prompt — the user layer the session writes).

## Risks / open questions

- **Narrow-only must be airtight.** The single most important invariant: a session user can only NARROW (disable within the admin-allowed set), never WIDEN. The projection intersects the session's disable set with the admin exposure; a session "enable" of a not-allowed source is rejected or inert — there is no code path where a session edit grants a capability the admin didn't. Pinned by an adversarial test that tries to widen via every session verb.
- **Base-prompt boundary.** The session-writable shape must not even carry a `Base` field; the gate is defence-in-depth. Both layers tested.
- **Ephemeral vs durable.** Personal skills are session-scoped and ephemeral; the plan pins they live under the session in the identity triple and never promote — no cross-scope leak (§6).

## Glossary additions

- **session-user safe subset** — the lower tier of the agent-config authorization matrix (D-235): a session-scoped, non-admin caller may set the user prompt layer, narrow (never widen) source/tool enablement within the admin-allowed set, and manage ephemeral personal skills — never base prompt, add/remove servers, or allowlist widening.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 against one shared registry under `-race`).**
- [ ] **Integration test exists, real registry + bus + admin/non-admin contexts, identity propagation, the narrow-only + base-unwritable failure modes, `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
