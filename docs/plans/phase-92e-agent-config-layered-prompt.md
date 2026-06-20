# Phase 92e — agent-config control plane: layered system prompt (operator base + user layer)

## Summary

Wires the `PromptLayers` section of the agent-config registry: an operator-owned, versioned **base** prompt layer plus an optional **user** layer that composes ABOVE the base without mutating it. A run resolves the active revision's prompt layers at run start (next-turn, immutable per-run snapshot). The base/user composition order is the structural security boundary (D-235) — a higher layer can add guidance but cannot weaken or replace the operator base. Diff + rollback come free from the 92a registry.

## RFC anchor

- RFC §6.2 — Planner / ReAct prompt (the structured prompt sections this layers into).
- RFC §6.16 — Agent Registry (the prompt set is part of the agent's content-hashed config; a prompt edit is a config revision).
- RFC §6.15 — Governance (the admin-verb desired-state pattern).

## Briefs informing this phase

- brief 13
- brief 11

## Brief findings incorporated

- **brief 13 (ReAct prompt engineering):** the system prompt is depth-engineered into structured sections (tool-usage, additional-guidance, …); UNTRUSTED content (memory, user input) is framed distinctly from operator guidance. This phase keeps the operator base as the trusted spine and composes the user layer in the section reserved for lower-trust guidance — the user layer can extend, never silently override the operator's guardrails.
- **brief 11 (console feature surface):** prompt configuration is an operator surface rendered from canonical events + a registry snapshot. This phase emits `agent.config.revised` on a base/user edit and exposes the layers via the 92a read surface, so the Console (92h) renders + diffs them without holding the prompt itself (D-061).

## Findings I'm departing from (if any)

None.

## Goals

- Admin-scoped Protocol surface to set the prompt layers (base and/or user) as a config revision — REPLACING only the `PromptLayers` section, preserving Skills + ToolExposure (the bidirectional section-merge invariant 92d established).
- Run-start projection: resolve the active revision's `PromptLayers.Base` as the run's base system prompt (overriding the agent's configured default base when set) and append `PromptLayers.User` above it, in the lower-trust guidance position. Next-turn only (D-025).
- A defined, documented precedence with the 92b per-run overrides (see Risks).
- The base/user composition is the security boundary: the user layer is always composed ABOVE (appended), never a base replace — enforced by the data model, not only a runtime check.

## Non-goals

- The session-user authoring path for the user layer (that gating is 92g; this phase ships the admin path that can set both layers).
- The Console prompt editor (92h).
- Changing the ReAct section taxonomy (83a–f) — this phase composes into the existing sections, it does not re-author them.
- Per-run one-shot prompt replace (that is 92b's session `SystemPromptOverride`, already shipped).

## Acceptance criteria

- [ ] Admin-scoped Protocol method (e.g. `agent_config.set_prompt_layers`) on `/v1/agent_config/` sets base and/or user as a revision; REPLACES only `PromptLayers`, preserving Skills + ToolExposure (a test pins all-direction preservation).
- [ ] Run-start projection resolves `PromptLayers.Base` as the base system prompt and composes `PromptLayers.User` above it; an unset base inherits the agent's configured default (backward compatible); no active revision / no PromptLayers section → unchanged.
- [ ] Precedence with 92b overrides is explicit and tested: the durable base layer is the system prompt the per-run session `SystemPromptOverride` (one-shot replace) and tenant/session `ExtraInstructions` (additive) then compose on top per the documented order.
- [ ] The user layer composes ABOVE the base (append); it can never replace or precede the base (the security boundary) — covered by a test asserting base guidance survives a user-layer set.
- [ ] `agent.config.revised` emitted; diff (92a) renders the base/user text delta; rollback repoints.
- [ ] Identity scoped by the triple; `agent_id` a key, never an isolation filter (§6); admin authority from verified ctx (D-219/D-235).
- [ ] TS manifest + typed client + generated docs regenerated; `scripts/smoke/phase-92e.sh` green.

## Files added or changed

- `internal/runtime/agentcfg/protocol/promptlayers.go` — the set-prompt-layers service method (compose a revision, preserve siblings, emit event).
- `internal/protocol/{types/agentconfig.go,methods/methods.go,singlesource/singlesource.go,transports/stream/agentconfig_handler.go}` + the two generator typeindex files — the new method + wire types.
- `internal/runtime/agentcfg/projection/projection.go` — `ActivePromptLayers` (resolve base+user from the active revision), shared by cmd/harbor + devstack.
- `internal/planner/react/prompt.go` (or the run-loop RunContext population) — compose the resolved base/user into the run's system prompt at the right precedence relative to the 92b overrides.
- `cmd/harbor/cmd_dev_runloop.go` + `harbortest/devstack/devstack.go` — apply the prompt-layer projection at run start (D-094 twin).
- `web/console/src/lib/protocol/agentconfig.ts` + `client.ts`; `docs/site/protocol/*`; `docs/skills/use-the-harbor-protocol/SKILL.md`.
- `scripts/smoke/phase-92e.sh`.

## Public API surface

```go
// Reuses agentcfg.PromptLayers{Base, User *string} (already declared).
// Protocol: agent_config.set_prompt_layers → records a revision.
// projection.ActivePromptLayers(ctx, reg, agentID, id) (base string, user string, ok bool, err error)
```

## Test plan

- **Unit:** set_prompt_layers records a revision preserving Skills + ToolExposure; the projection composes base+user; the user layer appends below the base (boundary); precedence with 92b session override + tenant ExtraInstructions; event emitted; identity/admin rejections.
- **Integration:** `test/integration/agentcfg_prompt_layers_test.go` — real registry + real ReAct prompt builder + bus: set base+user via Protocol → revision → run-start projection composes them into the LLM request → 92b session override still replaces for one message → diff/rollback round-trip; non-admin 403; under `-race`.
- **Conformance:** reuses the 92a `agentcfg` driver conformance.
- **Concurrency / leak:** N≥100 concurrent set/project against one shared registry under `-race`; no cross-run bleed.

## Smoke script additions

- `scripts/smoke/phase-92e.sh`: static — the set_prompt_layers method + the projection symbol + the typed client + generated-docs rows; live (skip-if-404) — set base via the admin-gated route then get it back; non-admin rejected.

## Coverage target

- `internal/runtime/agentcfg/protocol` (prompt-layers methods): 85%
- `internal/runtime/agentcfg/projection` (prompt projection): 85%

## Dependencies

- 92a (registry + projection seam + diff/rollback), 83a (the structured prompt sections), 92b (the per-run override precedence this composes with).

## Risks / open questions

- **Precedence with 92b overrides.** The durable layered base (92e) is the agent's base system prompt; the per-run session `SystemPromptOverride` (92b) is a one-shot full REPLACE of that for a single message; tenant + session `ExtraInstructions` (92b) are additive below. The plan pins the order: **durable base layer → durable user layer → tenant additive → session additive**, with the session `SystemPromptOverride` replacing the whole base+user spine for its one message. This must be documented in the prompt-builder godoc and pinned by a test so the two override systems don't silently fight.
- **User-layer trust framing.** The user layer composes in the lower-trust guidance position (brief 13) so it cannot impersonate operator guardrails; the data model (separate `User` field, always appended) is the enforcement.

## Glossary additions

- **layered system prompt** — the agent-config prompt model: an operator-owned, versioned `Base` layer + an optional `User` layer composed above it without mutating it; the composition order is the security boundary. Distinct from the per-run session `SystemPromptOverride` (one-shot replace) and `ExtraInstructions` (additive).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 against one shared registry under `-race`).**
- [ ] **Integration test exists, real registry + ReAct builder + bus, identity propagation, ≥1 failure mode, `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
