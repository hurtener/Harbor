# Phase 165 — Structured reasoning-steps rehydration

## Summary

Phase 161 (D-293, merged) rehydrates per-turn stats, flat reasoning TEXT, and
tool-call badges on session reopen. But the STRUCTURED reasoning steps — the
ordered per-ReAct-step reasoning that the live view shows as an accordion of
"Step N: <trace>" entries, interleaved in order with the tool calls each step
triggered — do NOT survive reopen: the reopened agent message renders only
161's flat `reasoningText` blob, not the ordered `reasoningSteps`. This phase
reconstructs the ordered reasoning-step sequence on reopen so it renders
IDENTICAL to the live view.

**Verdict: ZERO-WIRE, Console-only** (verified by live probe + code trace,
2026-07-11). The live path's reasoning steps come from the tasks.get
**enricher trajectory projection, which is in-memory-only** (`Enricher.Trajectory`
reads `trajectoryFn(taskID)` — the in-memory trajectory — and returns nil when
it is "unavailable (evicted …)",
`internal/runtime/serve/enricher.go:49-56`): the task record survives a reopen,
but the trajectory projection does NOT — `tasks.get` carries no trajectory
field for a run whose in-memory trajectory has been reaped, so the enricher can
never serve a reopened run's reasoning steps. The ONLY durable source is the
event stream,
which already carries everything needed: `planner.decision` events (one per
trajectory step, ordered by `sequence`, each carrying `ReasoningTrace` +
`DecisionKind` + `Tool`) plus `tool.invoked`/`tool.completed`/`tool.failed`
lifecycle. `state.history` already delivers these rows (Phase 125/D-254; and
161 already reads `planner.decision` for the tool-call badges — it simply
ignores the `ReasoningTrace` key). So `reduceHistoryTurns` reconstructs the
same ordered `ReasoningStep[]` the live `parseReasoningSteps(enriched
tasks.get)` produced, and `hydratePastTurns` sets it on the reopened message —
no runtime change, no wire change, no lockstep churn.

## RFC anchor

- RFC §6.13
- RFC §5.2
- RFC §7
- RFC §6.2

## Briefs informing this phase

- brief 06
- brief 11

## Brief findings incorporated

- brief 06 §5 ("Two-channel split"): the live view and the reopen must both
  reduce the SAME one-bus records. 161 established that discipline for stats +
  tool badges; this phase extends it to the reasoning-step structure — the
  reopen reconstructs from the identical `planner.decision` / `tool.*` events
  the live SSE reduces, never a second store or a second-fetch of a projection
  that no longer exists in memory.
- brief 11 LR-6 (per-task detail pane): historical per-task detail — "tool
  name … identity at invocation time" ordered under a step's reasoning — is
  designed to be sourced from the durable event log, not a live-only
  in-memory projection. This phase makes the reopen path honor that: the
  ordered reasoning↔tool interleaving is reconstructed from the durable log,
  which is exactly where brief 11 said historical per-step detail lives.

## Findings I'm departing from (if any)

- **Corrected source model (the coordinator's original brief called
  `reasoning_trace` the ReAct "Thought:" scratchpad — that is wrong, verified
  against the tree).** Both `reasoningText` and `reasoningSteps` draw from the
  SAME native-model thinking channel, `llm.CompleteResponse.Reasoning`. The
  planner's former textual `Reasoning` action field was DELIBERATELY REMOVED
  (`internal/planner/decision.go:36-38` documents the drop). The chain: the
  ReAct planner threads `resp.Reasoning` → `rc.OnReasoning`
  (`internal/planner/react/react.go:720`, `:730-731`); the runloop copies that
  terminal string onto `trajectory.Step.ReasoningTrace`
  (`internal/runtime/steering/runloop.go:724-725` sets `stepReasoning` from
  `OnReasoning`, `:921` stamps it on the appended `planner.Step`); the enricher
  projects each non-empty step's `{Index: i, ReasoningTrace}` onto the wire
  (`internal/runtime/serve/enricher.go:62-73`). `reasoning_trace` is
  documented as "the provider-side thinking trace via
  `llm.CompleteResponse.Reasoning`" (`internal/planner/events.go` DecisionPayload
  godoc; `internal/planner/trajectory/trajectory.go:112-114`). So
  `reasoningSteps` = native thinking BUCKETED PER ReAct STEP (which thinking
  preceded which tool call) — exactly the UX this phase delivers. This is a
  correction to the motivating brief, not a departure from an RFC/decision.

## Goals

- **Confirm the zero-wire hypothesis (done — evidence in Summary + this
  section).** Byte-equivalence is by construction: `emitDecision(rc, final,
  resp.Reasoning)` (`react.go:720`) and `rc.OnReasoning(resp.Reasoning)` feed
  the SAME `resp.Reasoning` value into (a) the `planner.decision` event's
  `ReasoningTrace` and (b) `stepReasoning` → `trajectory.Step.ReasoningTrace`
  (`runloop.go:725`/`:921`). The enricher projects the trajectory step's trace
  verbatim (`enricher.go:66-67`). So `planner.decision.ReasoningTrace` in the
  durable read-back equals the `reasoning_trace` the live enriched `tasks.get`
  serves — the same bytes. Live probe (session `sess-ca866eb2-ccb`, run
  `01KX7KPCQ0ZCK7EQHXW4FTEEE2`, runtime `127.0.0.1:18163`): the `state.history`
  page carried `planner.decision` at seq 10 (`CallTool`, `Tool:weather.lookup`,
  `ReasoningTrace` key present) → `tool.invoked` seq 11 → `tool.completed`
  seq 12 → `planner.decision` seq 19 (`Finish`) → `task.completed` seq 20 —
  the interleaving preserved by `sequence`. (The traces were empty in the probe
  only because the run used the mock LLM, which emits no native thinking — the
  separate, already-resolved Anthropic-native-thinking question; the STRUCTURE
  is fully reconstructable regardless, and a real-reasoning run rides the same
  key.)
- **The `index` semantics — pinned precisely (corrected: NOT every decision
  is a step).** The live `ReasoningStep.index` is the enricher's `i` from
  `for i, step := range traj.Steps` (`internal/runtime/serve/enricher.go:61`):
  the 0-based position in the FULL trajectory-step sequence, then only
  non-empty-`ReasoningTrace` steps are emitted (`enricher.go:62`,`:66-67`;
  `parseReasoningSteps` applies the same non-empty filter,
  `src/routes/(console)/playground/[session_id]/answer-envelope.ts:62`).
  **The key correction: `emitDecision` fires for EVERY decision — including
  `Finish` and `RequestPause` (`internal/planner/react/react.go:559`,`:720`,
  each writing `resp.Reasoning` verbatim to the event's `ReasoningTrace`) —
  but the runloop appends a `traj.Step` ONLY in the `default` branch of its
  decision switch (CallTool / CallParallel / SpawnTask / AwaitTask,
  `internal/runtime/steering/runloop.go:917-923`); `case planner.Finish:
  return d, nil` (`:795-796`) and `case planner.RequestPause` (`:798`) append
  NO step.** So a `Finish` carrying NON-EMPTY reasoning (common — the final
  answer turn often has the most thinking) has no `traj.Step` and the live
  view never shows it; a mid-run `RequestPause` with reasoning likewise has
  no step. Therefore the reducer must fold a step into `reasoningSteps` ONLY
  for decisions whose `DecisionKind ∈ {CallTool, CallParallel, SpawnTask,
  AwaitTask}` — the step-appending kinds — incrementing the per-run step
  ordinal ONLY on those, and EMITTING only when the trace is non-empty.
  `Finish` / `RequestPause` / `Cancelled` are excluded from BOTH the ordinal
  and emission (mirroring the runloop's `default`-branch gate exactly). This
  is the ONE non-obvious correctness point of the phase.
- **Reducer extension (`web/console/src/lib/sessions/history.ts`).**
  `HistoryTurn` gains `reasoningSteps: HistoryReasoningStep[]` (shape
  `{index: number; reasoning_trace: string}`, matching the wire `ReasoningStep`
  at `web/console/src/lib/chat/types.ts:118-121`). In the existing
  `planner.decision` branch (`history.ts:283-290`, where 161 already reads
  `DecisionKind` + `Tool` for the tool-call badges), `reduceHistoryTurns`
  additionally reads the `ReasoningTrace` key (PascalCase/snake tolerant); it
  increments a per-run STEP ordinal ONLY when `DecisionKind ∈ {CallTool,
  CallParallel, SpawnTask, AwaitTask}` (the step-appending kinds — `Finish` /
  `RequestPause` / `Cancelled` touch neither the ordinal nor the emission),
  and appends `{index, reasoning_trace}` when the trace is non-empty — in
  event (sequence) order. `DecisionKind` is already in the payload and already
  read here, so the fix stays zero-wire. The flat `turn.reasoning` (161) is
  left exactly as-is (it stays the fallback and the non-goal boundary).
- **Bytes are identical — the redactor is a no-op on `ReasoningTrace`
  (verified).** The reopen source is the bus-persisted `planner.decision`
  event; the live enricher reads the raw in-memory
  `trajectory.Step.ReasoningTrace`. These would differ only if the bus
  redactor scrubbed the persisted reasoning — but `DecisionPayload` embeds
  `events.SafeSealed` (`internal/planner/events.go:139-140`), i.e. it is a
  `SafePayload`, and the bus SKIPS the audit redactor entirely for
  SafePayloads (`internal/events/drivers/inmem/inmem.go:369-374`: redact only
  `if _, safe := payload.(events.SafePayload); !safe`). So the persisted
  `ReasoningTrace` is the RAW `resp.Reasoning`, byte-identical to the
  enricher's raw in-memory value. (The `emitDecision` godoc line "the audit
  redactor processes the payload on the bus", `react.go:717-719`, is
  imprecise for this specific payload — SafeSealed makes it a no-op; reasoning
  is model output, not a secret-shaped key the redactor matches. Consequently
  the theoretical "live shows more than reopen" asymmetry does NOT arise: both
  paths carry raw reasoning.)
- **Hydration (`+page.svelte` `hydratePastTurns`, ~`:955-1006`).** The
  hydrated agent message sets `reasoningSteps: turn.reasoningSteps.length > 0 ?
  turn.reasoningSteps : undefined` alongside the existing `reasoningText`
  (161). The message renderer already prefers `reasoningSteps` over
  `reasoningText` (`MessageBubble.svelte:176-178` → `ReasoningAccordion`,
  `:177`), so a reopened turn with reconstructed steps renders the ordered
  accordion identically to a live turn; a turn with no non-empty reasoning
  (e.g. every step's native thinking was empty) cleanly falls back to 161's
  flat text — no regression.
- **Acceptance centerpiece.** On reopen, the ordered reasoning-step accordion
  and the ordered tool-call badges render IDENTICAL to the live view for the same
  run: the reconstructed `reasoningSteps` equals what
  `parseReasoningSteps(enriched tasks.get)` produced live (same indices, same
  traces, same order), and together with 161's already-ordered `toolCalls` they
  convey the same reasoning→tool→reasoning interleaving — not one flat
  undifferentiated reasoning blob.

## Non-goals

- The flat `reasoningText` path — 161 already ships it; it stays as the
  fallback when a run has no non-empty reasoning steps. This phase adds the
  structured steps ON TOP, never removes the flat path.
- The Anthropic-native-thinking-empty question — RESOLVED and SEPARATE: native
  thinking already surfaces on `llm.CompleteResponse.Reasoning` when the
  provider emits it; when it is empty (the mock, or a provider/config with no
  thinking channel) there simply are no non-empty steps to show. Whether a
  given provider populates thinking is a bifrost-layer concern under separate
  investigation and is NOT part of this phase.
- `tasks.get` / trajectory / enricher changes — this phase reconstructs from
  `state.history` events, NOT by calling `tasks.get` per historical run (which
  cannot answer for a reopened run: the enricher's trajectory projection is
  in-memory-only and absent once the trajectory is reaped — the task record
  survives, the trajectory does not, `enricher.go:49-56`). No runtime or wire
  surface is touched.
- No new wire method, wire type, or canonical event type; no `ProtocolVersion`
  bump; therefore no D-223 lockstep and no D-209 docs regen (a manifest or
  generated-docs diff in the implementation PR is a red flag).
- No CallParallel/spawn reasoning-per-branch modeling beyond what the existing
  one-decision-per-step trajectory already expresses (see Risks).

## Acceptance criteria

- [ ] `HistoryTurn.reasoningSteps: {index: number; reasoning_trace: string}[]`
  added (matching the wire `ReasoningStep` shape); `reduceHistoryTurns` folds
  a step ONLY for `planner.decision` events whose `DecisionKind ∈ {CallTool,
  CallParallel, SpawnTask, AwaitTask}` (the step-appending kinds), incrementing
  the per-run 0-based STEP ordinal only on those and emitting `{index,
  reasoning_trace}` only when the trace is non-empty, in sequence order;
  `Finish` / `RequestPause` / `Cancelled` decisions are excluded from both the
  ordinal and emission. The flat `turn.reasoning` fold is unchanged.
- [ ] Ordering + interleaving correct: for a multi-step, multi-tool fixture the
  reconstructed `reasoningSteps` are in step order, their `index` values match
  the enricher's `i`-over-`traj.Steps` (empty-reasoning STEP-appending
  decisions advance the index without emitting; `Finish` / `RequestPause`
  decisions advance NEITHER), and they interleave with the reconstructed
  `toolCalls` in the same order the live view shows.
- [ ] Byte-equivalence pin: for a captured `state.history` window of a real
  reasoning-bearing run, `reduceHistoryTurns(...).reasoningSteps` equals the
  `parseReasoningSteps(...)` output for the same run's enriched `tasks.get`
  detail (same indices, same traces) — a vitest asserts the two producers
  agree. **The fixture MUST include a REASONING-BEARING `Finish` decision AND
  a mid-run `RequestPause` decision** (both carrying non-empty reasoning): a
  fixture without them is the §17.8 rubber-stamp that goes green while
  production diverges — the empty-mock-`Finish` case cannot catch the
  over-emit/mis-index bug. The pin asserts neither the `Finish` nor the
  `RequestPause` produces a `reasoningSteps` entry, and that a step-appending
  decision AFTER the `RequestPause` keeps the correct (un-shifted) index.
- [ ] Page-window-boundary safety: a run whose events span two loaded
  `state.history` pages reconstructs its steps with no duplication and no
  reorder (the reducer already merges pages oldest-first via
  `loadSessionHistory`; the step fold must be order-stable across the merge).
- [ ] `hydratePastTurns` sets the reopened agent message's `reasoningSteps`
  (undefined when empty); `MessageBubble` renders the accordion; a run with no
  non-empty steps falls back to 161's `reasoningText` with no regression.
- [ ] Rehydration regression test: reopen against a recorded event window
  renders the ordered reasoning-step accordion + tool badges; the
  leave-and-return structure equals the live-view structure for the same run
  (the operator's UX requirement).
- [ ] ZERO wire/runtime changes verified: no Go files touched;
  `make protocol-ts-gen-check` + `make protocol-docs-gen-check` produce no
  diff; no new method/type/event.
- [ ] `scripts/smoke/phase-165.sh` OK ≥ 2, FAIL = 0.

## Files added or changed

- `web/console/src/lib/sessions/history.ts` — the `HistoryReasoningStep`
  interface and `HistoryTurn.reasoningSteps`; the `planner.decision` fold in
  `reduceHistoryTurns` extended to read `ReasoningTrace` + maintain the per-run
  decision ordinal + append non-empty steps. (The existing `DecisionKind`/`Tool`
  → tool-row fold at `:283-288` is unchanged; the reasoning read is additive
  in the same `else if (ev.type === 'planner.decision')` branch.)
- `web/console/src/lib/sessions/history.spec.ts` (or the existing vitest home)
  — the ordered-reconstruction fixtures, the byte-equivalence-vs-`parseReasoningSteps`
  pin, the page-boundary fixture.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` —
  `hydratePastTurns` sets `reasoningSteps` on the hydrated agent message
  (beside the existing `reasoningText` at ~`:998`).
- `web/console/src/routes/(console)/playground/[session_id]/*.test.ts` — the
  rehydration regression extended to assert `reasoningSteps` renders on reopen.
- `scripts/smoke/phase-165.sh` (new).
- `docs/plans/README.md` — Phase 165 row + detail block.
- `docs/decisions.md` — D-298.
- `docs/glossary.md` — "reasoning-step rehydration".
- `docs/plans/wave-v113-coordination.md` — Stage 5 = 163 ∥ 165; scope 159–165.

No Go files, no wire types, no generated artifacts.

## Public API surface

- None on the wire — no new methods, types, errors, or event types. The
  reconstruction is Console-internal over already-flowing, already-registered
  events.
- Console-internal: `HistoryTurn.reasoningSteps` (a reducer output type, not a
  wire type) and its `HistoryReasoningStep` shape, which deliberately mirrors
  the existing wire `ReasoningStep` so the hydrated message field is
  assignment-compatible with the live path's.

## Test plan

- **Unit (Console vitest):** the reducer's ordered `reasoningSteps`
  reconstruction — multi-step/multi-tool fixture (interleaving + order correct,
  index matches the enricher's sparse-into-full semantics, empty-reasoning
  steps advance index without emitting); the byte-equivalence pin against
  `parseReasoningSteps` for a captured real-reasoning window; the page-boundary
  fixture (a run split across two windows reconstructs without dup/reorder);
  PascalCase/snake tolerance on the `ReasoningTrace` key.
- **Integration (Console rehydration test):** reopen against a recorded event
  window sets `reasoningSteps` on the message and renders the accordion;
  structure equals the live view; the empty-reasoning fallback to
  `reasoningText` is asserted (no regression).
- **Conformance:** N/A — no driver seam, no Go change.
- **Concurrency / leak:** N/A — no Go change; no new compiled artifact. (If the
  implementer's investigation surfaces ANY Go touch — it should not — its
  unit, `-race`, and 85% coverage obligations apply per §11.)

## Smoke script additions

- live-server, no real LLM needed (the mock path exercises the read-back
  mechanics — 161's precedent; the reasoning traces are empty under the mock,
  which is fine: this smoke proves the STRUCTURE is present, not that a
  provider populated thinking):
  - drive a tool-calling scripted run via the `start` method
    (`POST /v1/control/start`); poll `tasks.get` to terminal;
  - fetch `state.history` for the session; assert the page carries
    `planner.decision` events with a `ReasoningTrace` payload key AND
    `tool.invoked`/`tool.completed` with a `ToolName` key — the exact events
    the reducer folds into ordered steps;
  - assert the `planner.decision` and `tool.*` events interleave by `sequence`
    (a decision precedes its tool's invoke/complete) — the reconstructable
    ordering signal.
- Done-definition: `OK ≥ 2, FAIL = 0`; 404/405/501 → SKIP until the phase
  ships.

## Coverage target

- Console-only phase — no Go coverage gate. The binding bar is the named vitest
  suites (reducer reconstruction, byte-equivalence pin, page-boundary,
  rehydration regression). The frontend CI job (`npm run check && npm run lint
  && npm run test`) gates them.

## Dependencies

- 161 (D-293 — the rehydration foundation this extends: `HistoryTurn`,
  `reduceHistoryTurns`, `hydratePastTurns`, the tool-call fold), 125 (D-254 —
  the `state.history` windowed read that delivers the `planner.decision` /
  `tool.*` rows), 107a (the `ReasoningStep` type + `parseReasoningSteps` +
  the enricher trajectory projection this reconstructs the reopen equivalent
  of), 118 (D-223 lockstep — must stay a no-op; this phase proves zero wire
  diff).

## Risks / open questions

- **The `index` derivation is the one correctness subtlety.** It must match the
  enricher's `i`-over-`traj.Steps`, which counts STEP-APPENDING decisions only
  (CallTool / CallParallel / SpawnTask / AwaitTask). Two ways to get it wrong,
  both caught by the mandated fixture (AC-3): (a) incrementing/emitting on a
  `Finish` or `RequestPause` — those emit a `planner.decision` event with
  reasoning but append NO `traj.Step` (`runloop.go:795-798` return before the
  `default`-branch append at `:917-923`), so folding them phantom-adds a step
  the live view never shows and, for a mid-run `RequestPause`, shifts every
  later index; (b) indexing only the emitted (non-empty) steps — which drifts
  on a step-appending decision that carried empty reasoning (its `i` still
  advances on the enricher side). The guard is the byte-equivalence vitest
  over a captured window containing a reasoning-bearing `Finish`, a mid-run
  `RequestPause`, and at least one empty-reasoning CallTool step.
- **Decision↔step cardinality.** The corrected rule (fold only
  step-appending `DecisionKind`s) makes the mapping exact by construction: the
  runloop's `default` branch appends exactly one `traj.Step` per
  CallTool/CallParallel/SpawnTask/AwaitTask decision (`runloop.go:917-923`),
  which is precisely the set the reducer folds — so one folded decision ↔ one
  `traj.Step`, in order. The probe showed 2 decisions (1 CallTool + 1 Finish)
  ↔ 1 folded step (the Finish excluded). The implementer still verifies the
  ordinal against the enricher's `traj.Steps` for a `CallParallel` /
  `SpawnTask` shape (one decision → one step there too, per the switch) via
  the fixture; this is a test-fixture question, not a wire question.
- **Empty traces under the mock / non-thinking providers.** The reopened view
  shows fewer (or zero) reasoning steps exactly when the live view did — honest
  parity, not a regression. The fallback to flat `reasoningText` covers the
  zero-step case.
- **If the zero-wire hypothesis had failed** (it did not): the escape was a
  small additive surface projecting the per-step reasoning onto the durable
  read-back. It is unnecessary — the `planner.decision.ReasoningTrace` key is
  already in read-back and byte-equivalent — but recorded here so a future
  reviewer sees the path was considered and closed by evidence.

## Glossary additions

- "reasoning-step rehydration" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage: N/A for Go (Console-only); the named vitest suites pass under
      `npm run test` in the frontend CI job.
- [ ] If multi-isolation paths changed: N/A — no Go/identity path touched (the
      `state.history` scoping the reducer reads over is unchanged from 125/161).
- [ ] **Reusable-artifact concurrent-reuse:** N/A — no new compiled Go artifact.
- [ ] **Integration test:** the Console rehydration regression exercises the
      reopen path end-to-end over recorded events (real reducer, real render).
- [ ] Zero wire diff: `make protocol-ts-gen-check` + `make
      protocol-docs-gen-check` unchanged; no Go files in the diff.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: the source-model correction is
      documented above (Findings) and in D-298.
