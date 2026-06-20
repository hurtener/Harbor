# Phase 92c — agent-config control plane: skills control (the first consumer)

## Summary

The first consumer of the desired-state registry primitive (92a): a Protocol surface for live-managing an agent's skills — list / upsert / delete — recorded as agent-config revisions so skills inherit the unified diff + rollback. The `SkillStore` (RFC §6.7) is already durable, identity-scoped, content-hashed, and event-emitting but has **no Protocol surface**; this phase adds the admin-scoped surface and wires skill membership into the registry's `ConfigPayload` so a skills change is a versioned, next-turn-applied revision. This is the same-wave consumer that satisfies CLAUDE.md §13 for the 92a primitive.

## RFC anchor

- RFC §6.7 — Skills subsystem (the `SkillStore` this exposes over the Protocol; identity-scoped, content-hashed, last-write-wins with pack-overwrite refusal).
- RFC §6.16 — Agent Registry (skills are part of the agent's content-hashed config surface; a skills edit is a config revision).
- RFC §6.15 — Governance (the admin-verb pattern the skills surface mirrors).

## Briefs informing this phase

- brief 04
- brief 11

## Brief findings incorporated

- **brief 04 (memory + skills):** skills are queried at reasoning time (not baked into a compiled artifact), are identity-scoped, and carry an `Origin` (pack / generated / imported) with a pack-overwrite-refusal conflict policy. This phase preserves that conflict policy at the Protocol edge (a pack-origin skill is never silently overwritten — the `ErrPackOverwriteRefused` / `skill.pack_overwrite_refused` path surfaces as a typed Protocol error) and projects the agent's active skills-set at run start, not into a frozen artifact.
- **brief 11 (console feature surface):** skill management is an operator surface; the Console renders it as a lens over `skill.*` events + a registry snapshot. This phase emits the existing `skill.upserted`/`skill.deleted` events AND records the membership change as an `agent.config.revised` revision, so the Console can show both the live skill event and the versioned config diff without holding the skills itself (D-061).

## Findings I'm departing from (if any)

None.

## Goals

- Admin-scoped Protocol surface `agent_config.skills.{list,upsert,delete}` (or a `skills.*` family — naming settled at impl against the single-source method set), driving the existing `SkillStore` (Upsert/List/Delete) with identity validation at the edge.
- Record the agent's skills membership in the registry `ConfigPayload.Skills` so a skills change produces a config revision (diff + rollback via 92a).
- The pack-overwrite-refusal conflict policy is honoured at the Protocol edge and returns a typed error (no silent overwrite, no silent degrade).
- Run-start projection: the agent's active skills-set is resolved from the active revision at run start (extends the 92a projection seam) so skills changes apply next-turn.

## Non-goals

- The registry primitive itself (92a).
- MCP pause/per-tool policy (92d), the layered prompt (92e), add-connection (92f), session-user safe subset (92g).
- The Skills.md importer / in-runtime generator surfaces (already shipped; this phase exposes management, not authoring pipelines).
- Cross-identity skill promotion (skills stay identity-scoped per §6).

## Acceptance criteria

- [ ] `agent_config.skills.list` / `.upsert` / `.delete` Protocol methods exist (single-source) on the `/v1/agent_config/` family, admin-gated (D-235), nil-safe to 501 when not wired.
- [ ] `upsert` drives `SkillStore.Upsert` and returns the typed `ErrPackOverwriteRefused` as a Protocol error when refused — never a silent overwrite (§13).
- [ ] A successful skills mutation records an `agent.config.revised` revision (membership delta in `ConfigPayload.Skills`) AND emits the existing `skill.upserted`/`skill.deleted` event.
- [ ] `agent_config.diff` (from 92a) renders a structured set-diff of the skills-set across two revisions; rollback repoints the skills membership.
- [ ] Run-start projection resolves the active skills-set from the active revision; concurrent runs keep their snapshot (D-025).
- [ ] Identity scoped by the triple; `agent_id` keys the registry, never an isolation filter (§6).
- [ ] TS manifest + typed module regenerated; generated docs regenerated; `scripts/smoke/phase-92c.sh` green.

## Files added or changed

- `internal/agentcfg/agentcfg.go` — extend `ConfigPayload` with `SkillsSelection`.
- `internal/agentcfg/protocol/skills.go` — the skills service methods (drive `SkillStore`, record the revision, emit events).
- `internal/protocol/methods/methods.go` — the three skills method constants + sub-set membership.
- `internal/protocol/types/agentconfig.go` — skills wire request/response types.
- `internal/protocol/singlesource/singlesource.go` + `cmd/harbor-protocol-ts-lockstep/manifest.go` — entries.
- `internal/protocol/transports/stream/agentconfig_handler.go` — dispatch the skills verbs.
- `web/console/src/lib/protocol/agentconfig.ts` + `client.ts` — typed skills methods.
- `cmd/harbor/cmd_dev_runloop.go` + `harbortest/devstack` — project the active skills-set at run start (D-094 twin).
- `docs/skills/use-the-harbor-protocol/SKILL.md` + `docs/site/protocol/*` — regenerated.
- `scripts/smoke/phase-92c.sh`.

## Public API surface

```go
// agentcfg.ConfigPayload extension:
type SkillsSelection struct {
    // Names is the membership set of skill names active for the agent in
    // this revision. Resolved at run start; applied next-turn.
    Names []string `json:"names"`
}
```

Protocol: `agent_config.skills.list` → `{skills: []SkillSummary}`; `.upsert` → records a revision; `.delete` → records a revision. (Wire shapes single-sourced in `types/agentconfig.go`.)

## Test plan

- **Unit:** the skills service drives `SkillStore` correctly; pack-overwrite refusal surfaces as a typed Protocol error (not silent); a successful mutation records a revision delta + emits the skill event; identity-required rejection.
- **Integration:** extend `test/integration/agentcfg_control_plane_test.go` (or a sibling) — real `SkillStore` (localdb driver) + real registry + real bus: upsert a skill via Protocol → `agent.config.revised` + `skill.upserted` observed → `agent_config.diff` shows the skills set-diff → rollback repoints membership → run-start projection reflects the active skills-set; pack-overwrite refusal path; non-admin rejected; under `-race`.
- **Conformance:** N/A — reuses the 92a `agentcfg` driver conformance + the existing `SkillStore` conformance suite.
- **Concurrency / leak:** N≥100 concurrent skills upserts/lists against one shared service + registry under `-race`; no cross-identity bleed, baseline goroutines restored.

## Smoke script additions

- `scripts/smoke/phase-92c.sh`: static — the three skills method constants + the `SkillsSelection` payload section + the typed module entries + generated-docs rows; live (skip-if-404) — upsert a skill through the admin-gated route then list it back, a non-admin token is rejected, an upsert that would overwrite a pack-origin skill returns the typed refusal error.

## Coverage target

- `internal/agentcfg/protocol` (skills methods): 85%

## Dependencies

- 92a (the registry primitive + the run-start projection seam + diff/rollback), 37 (the `SkillStore`).

## Risks / open questions

- **Skill payload size in revisions.** The revision records skill *membership* (names), not full skill bodies — the bodies stay in the `SkillStore`. This keeps revisions small and avoids duplicating the content-hashed skill content; the diff is a set-diff over names. Flagged so a later phase does not accidentally inline bodies.
- **Origin/conflict semantics across rollback.** Rolling back a skills membership repoints the active set but does not resurrect a deleted skill's body if it was hard-deleted from the `SkillStore`. The plan pins: rollback restores membership; a body absent from the store surfaces as a loud "skill not found in store" at projection, never a silent drop.

## Glossary additions

- (Reuses the 92a terms.) No new vocabulary; the **skills-set** is the membership projection of skills into an agent-config revision.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 against one shared service under `-race`).**
- [ ] **Integration test exists, real `SkillStore` + registry + bus, identity propagation, ≥1 failure mode (pack-overwrite refusal / non-admin), `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
