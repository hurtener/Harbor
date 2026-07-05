# Phase 152 — agentcfg section-setter hooks carry-forward + rebuild-completeness guard

## Summary

Every section-scoped agent-config setter rebuilds the `ConfigPayload` by hand and carries forward only the sibling sections it knew about when it was written — all five of them drop the `Hooks` section added later, so any section edit silently erases a pinned run-completion hook (the exact §13 silent-degradation shape, inside the subsystem whose own godoc promises "the bidirectional section-merge invariant"). This phase adds the `Hooks` carry-forward to all five setters and ships a rebuild-completeness guard test that fails the suite whenever `ConfigPayload` gains a section a setter does not carry, so the omission class is closed permanently, not just for hooks.

## RFC anchor

- RFC §6.16
- RFC §6.17

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 05 §4: durable identity-scoped state edits must be atomic replace-one-section operations — a partial rebuild that loses sibling state is a corruption bug, not a style nit. The five setters' hand-rolled rebuilds are exactly the partial-rebuild risk the brief warns about; the completeness guard mechanizes the invariant.
- brief 06 §3: observability/config surfaces fail loud and visibly — a config mutation that silently drops previously-pinned state is worse than an error, because the operator learns about it only when the downstream behavior (here: conversation auto-save via the run-completion hook) stops.

## Findings I'm departing from (if any)

- None.

## Goals

- `agent_config.set_tool_exposure`, `agent_config.add_mcp_connection`, `agent_config.set_skills`, `agent_config.set_prompt_layers`, and `agent_config.set_llm_params` all preserve the active revision's `Hooks` section when composing the new revision.
- A rebuild-completeness guard test exists that mechanically fails when a future `ConfigPayload` section is added without extending every section-scoped setter's carry-forward.
- The five setters' godoc names the full carried-section list accurately (today several enumerate a stale subset).

## Non-goals

- No new Protocol verb (there is deliberately no section-scoped hooks setter; hooks land via `set_revision` — unchanged).
- No wire-type change, no TS lockstep impact, no protocol-docs regen.
- No change to hook validation, firing semantics, or the D-280 payload (Phase 150's surface is untouched).
- No refactor of the setters onto a shared merge helper beyond what the fix naturally motivates — if the implementor consolidates the carry-forward into one helper used by all five setters (recommended, it makes the guard trivial), that is in scope; redesigning the revision model is not.

## Acceptance criteria

- [x] `Service.SetToolExposure` (`internal/runtime/agentcfg/protocol/mcppolicy.go`) carries `Hooks` forward from the active revision.
- [x] `Service.recordConnectionRevision` (`internal/runtime/agentcfg/protocol/addconnection.go`) carries `Hooks` forward.
- [x] The skills, prompt-layers, and LLM-params setters (`skills.go`, `promptlayers.go`, `llmparams.go`) carry `Hooks` forward.
- [x] A rebuild-completeness guard test seeds an active revision with EVERY `ConfigPayload` section non-nil (via a seed constructor that reflection-asserts it populates every struct field, so a newly added section fails the seed first), invokes each of the five setters, and asserts every non-target section survives byte-identically on the resulting active revision.
- [x] A regression test proves the headline bug end-to-end at the Protocol service layer: `set_revision` with a `Hooks.RunCompletion` → `set_tool_exposure` flipping a loading mode → `get` shows the hook still present; same for `add_mcp_connection`.
- [x] The run-start projection (`projection.ActiveRunCompletionHook`) still resolves the hook after an interleaved section edit (integration-level assertion, real registry + StateStore driver).
- [x] All prior phase smokes pass; `scripts/smoke/phase-152.sh` shows OK ≥ 1, FAIL = 0.

## Files added or changed

- `internal/runtime/agentcfg/protocol/mcppolicy.go` — add `Hooks` carry-forward.
- `internal/runtime/agentcfg/protocol/addconnection.go` — add `Hooks` carry-forward.
- `internal/runtime/agentcfg/protocol/skills.go` — add `Hooks` carry-forward.
- `internal/runtime/agentcfg/protocol/promptlayers.go` — add `Hooks` carry-forward.
- `internal/runtime/agentcfg/protocol/llmparams.go` — add `Hooks` carry-forward.
- `internal/runtime/agentcfg/protocol/rebuild_completeness_test.go` — the guard test (new).
- `scripts/smoke/phase-152.sh` — live assertions.

## Public API surface

- None. Behavior fix inside the existing `agent_config.*` service; no signatures change. (If the implementor extracts a shared carry-forward helper it stays unexported.)

## Test plan

- **Unit:** per-setter carry-forward tests (hook present → section edit → hook survives); the reflection-backed seed-completeness assertion.
- **Integration:** Protocol-service-level round-trip (`set_revision` hooks → `set_tool_exposure` → `get`) on a real registry + real state driver; `projection.ActiveRunCompletionHook` resolves post-edit.
- **Conformance:** N/A — no driver seam changes.
- **Concurrency / leak:** the existing agentcfg D-025 stress remains green; add one interleaving case (concurrent section edits on distinct sections never drop hooks) under `-race`.

## Smoke script additions

- live-server: `agent_config.set_revision` pinning a `hooks.run_completion` → `agent_config.set_tool_exposure` (no-op exposure) → `agent_config.get` asserts `payload.hooks.run_completion.tool` unchanged (skip_if_404 on builds without the surface).

## Coverage target

- `internal/runtime/agentcfg/protocol`: 85%

## Dependencies

- 150 (the `Hooks` section + `set_revision` validation), 151 (loading-mode maps on `set_tool_exposure` — the setter this fixes), 92d/92f (the exposure section + revision registry), 92m (add-connection flow).

## Risks / open questions

- Low risk. One subtlety: the guard's seed constructor must be updated when a section is added — that is the point (the failure message must say exactly that, naming the new field, so the future implementor's fix is obvious).

## Glossary additions

- Rebuild-completeness guard (added to `docs/glossary.md` in this PR).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (85.5% on `internal/runtime/agentcfg/protocol`, target 85%)
- [x] If multi-isolation paths changed: N/A — no `(tenant, user, session)` isolation path changed; the fix is a same-identity sibling-section carry-forward.
- [x] Concurrent-reuse: N/A as a new artifact — no new reusable artifact is built; the existing agentcfg D-025 stress stays green and gains the interleaved-sections case (`TestService_ConcurrentInterleavedSectionWrites_HooksNeverDropped`).
- [x] Integration test wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race` (`test/integration/phase152_hooks_carryforward_test.go`; the failure-mode acceptance criteria on this seam are already covered by the phase-92f/150 integration suites this phase does not touch).
- [x] If new vocabulary: glossary updated ("Rebuild-completeness guard").
- [x] If a brief finding was departed from: N/A — no brief finding was departed from.
