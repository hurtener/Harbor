# Phase 233c — Bifrost reasoning fidelity (HA-51)

## Summary

Deliver HA-51 and D-402: Bifrost reasoning capture preserves the exact bytes
the provider streamed. When raw `delta.Reasoning` was observed, it is the sole
completed source; details-only providers retain a block-aware fallback that
does not invent whitespace. The existing planner, task projection, durable
event, and Console paths prove one trace unchanged from live delivery through
restart reconstruction.

## RFC anchor

- RFC §6.2.
- RFC §6.5.
- RFC §6.8.
- RFC §6.13.

## Briefs informing this phase

- brief 03
- brief 07
- brief 08

## Brief findings incorporated

- brief 03 §5: an LLM transport adapter translates provider data at one
  explicit boundary and reports malformed provider behavior loudly.
- brief 07 §11: a shared runtime artifact is tested under cancellation and
  concurrent calls, not only one happy-path completion.
- brief 08 §4: Bifrost is the provider-normalization layer; Harbor verifies
  its decoded transport behavior at the adapter boundary rather than assuming
  provider-specific response shapes.

## Findings I'm departing from (if any)

- The historical Phase 83e/D-147 capture-source precedence preferred final
  `ReasoningDetails`. D-402 narrows and supersedes only that precedence because
  a streamed raw reasoning channel is evidence of the exact source bytes;
  action-schema narrowing and D-148 replay policy are unchanged.

## Goals

- If any non-nil raw `delta.Reasoning` value is observed, make
  `CompleteResponse.Reasoning` the byte-exact concatenation of every observed
  raw value, including empty and whitespace-only fragments.
- Keep raw reasoning callbacks immediate while preventing synthesized details
  from overriding or duplicating the final raw trace.
- For details-only responses, preserve semantic blocks and exact fragment bytes
  without a trim, normalization, or per-fragment separator.
- Prove byte parity through the adapter, planner decision, live task
  projection, durable `state.history` restart reconstruction, and Console
  history rendering.

## Non-goals

- No Protocol method, wire type, canonical event, Protocol version, or replay
  mode change.
- No consumer-side whitespace repair, presentation-layer workaround, or
  provider-native thinking-block replay.
- No change to D-148's encrypted/content-only block exclusion or to the
  `never` / `text` operator replay modes.

## Acceptance criteria

- [ ] Per completion choice, observing any non-nil raw `delta.Reasoning`
  selects raw-source mode even if that value is empty. The completed reasoning
  is the exact ordered concatenation of all raw values; `ReasoningDetails` do
  not override, append to, trim, or otherwise transform it.
- [ ] Raw reasoning invokes `OnReasoning` immediately with each observed value.
  Details-only reasoning remains final-only, so a later raw delta cannot cause
  duplicate callback delivery.
- [ ] Details-only fragments coalesce by non-empty stable block ID, otherwise
  by `(choice index, type, index)`. An initial ID-bearing fragment aliases its
  fallback identity so later ID-less fragments join it. Within a block bytes
  concatenate exactly; exactly one literal `\n\n` separates distinct emitted
  blocks in first-seen order. No path trims intentional whitespace. Encrypted
  and content-only blocks remain excluded under D-148.
- [ ] A decoded JSON/SSE regression—not directly assembled Go structs—uses
  `["**Preparing to send email**", "\\n\\n", "I", " need", " to", " compose"]`
  and asserts the exact result `**Preparing to send email**\n\nI need to compose`
  in the callback stream and completed response.
- [ ] The same fixture asserts byte-identical reasoning in
  `planner.decision.ReasoningTrace`, the live `tasks.get` trajectory, and
  durable `state.history` after a runtime restart. The restart oracle is the
  durable planner-decision history because live task trajectory is in-memory.
- [ ] Details-only multi-fragment/single-block and multi-block regressions
  remain, including choice separation, ID-to-fallback aliasing, intentional
  whitespace, and excluded encrypted/content blocks.
- [ ] Console history/reopen tests render exactly the persisted newline bytes;
  no CSS or client-side coalescing is accepted as a repair.
- [ ] The shared Bifrost driver passes N>=100 concurrent identity-distinct,
  cancellation-varied calls under `-race`: no race, response/callback byte
  bleed, cancellation cross-talk, or goroutine leak.

## Files added or changed

- `internal/llm/drivers/bifrost/{bifrost,reasoning}.go` and focused tests —
  per-completion accumulator, raw precedence, and details-only block assembly.
- `internal/llm/drivers/bifrost/custom_provider_wire_test.go` — decoded
  JSON/SSE fixture, not direct response structures.
- `test/integration/phase233c_reasoning_fidelity_test.go` — real runtime,
  planner/task/history/restart parity over durable state.
- `web/console/` history/reopen tests — byte-exact reasoning rendering.
- `scripts/smoke/phase-233c.sh` — planning guard, then named live tests when
  implemented.
- `docs/decisions.md`, `docs/plans/README.md`, the historical Phase 83e plan,
  and this plan.

## Public API surface

- None. `llm.CompleteResponse.Reasoning`, planner decision payload, task
  trajectory, and durable history retain their existing shapes; D-402 corrects
  the driver-internal source selection and byte-preserving assembly only.

## Test plan

- **Unit:** raw observed/empty/whitespace precedence; no raw fallback; stable
  ID and `(choice,type,index)` grouping; ID-to-fallback alias; within-block
  exact concatenation; distinct-block separation; encrypted/content exclusion.
- **Integration:** decoded Bifrost JSON/SSE through a real runtime and durable
  state driver, asserting callback/final/planner/live-task/restart-history
  parity plus an identity-distinct negative case.
- **Conformance:** preserve existing Bifrost unary and details-only provider
  fixtures while adding the decoded-stream fixture to the adapter contract.
- **Concurrency / leak:** N>=100 shared driver calls under `-race`, with
  independent identities and cancellation; assert byte isolation and restored
  goroutine baseline.

## Smoke script additions

- Planning-stage static assertions pin HA-51, D-402, raw-byte precedence,
  decoded JSON/SSE fixture, durable restart parity, and zero-wire scope.
- Implementation replaces the planning guard with named adapter/integration
  tests. The completed phase smoke must report `OK > 0` and `FAIL = 0`.

## Coverage target

- `internal/llm/drivers/bifrost`: 90%.
- Planner/runtime task-history paths touched by the integration fixture: 85%.
- Console history/reopen named test suites pass in frontend CI.

## Dependencies

- 33 (Bifrost driver), 83e (reasoning channel), 83m (run-loop reasoning
  projection), and 165 (durable reasoning-step rehydration). Phase 233c is
  independent of 233a/233b after these shipped foundations and gates Phase 235.

## Risks / open questions

- Upstream may expose provider details with incomplete IDs or multiple choices;
  fallback identity and per-choice ownership must stay explicit and must never
  merge unrelated blocks.
- A test that builds delta structs directly can bypass Bifrost's decoder
  synthesis and falsely prove the fix. The decoded JSON/SSE fixture is binding.
- `tasks.get` cannot prove post-restart parity because its trajectory is
  in-memory; durable `state.history` is the restart source of truth.

## Glossary additions

- None — this phase narrows the existing reasoning channel and reasoning trace
  terms rather than introducing a new public concept.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] Focused local race, smoke, lint, mirror, and diff checks pass; cloud
  PR-to-main preflight remains authoritative and is not duplicated locally
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages >= stated target
- [ ] N>=100 driver concurrent-reuse test passes under `-race`
- [ ] Real-driver integration proves identity, restart, and failure behavior
- [ ] Console history byte-parity test passes
- [ ] Zero wire diff verified by protocol lockstep/doc generation checks
- [ ] If a brief finding was departed from: justified above + D-402 filed
