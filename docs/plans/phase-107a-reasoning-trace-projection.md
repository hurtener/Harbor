# Phase 107a — Reasoning trace projection (tasks.get enricher + Playground accordion)

## Summary

The react planner already captures the LLM's per-step thinking trace on `trajectory.Step.ReasoningTrace` (Phase 83e shipped). The trajectory is never serialized, never projected onto the Protocol surface, and never reaches the Console — the operator's first-five-minutes Playground experience hides what the model was actually thinking. Phase 83e §49 ("Non-goals") explicitly defers Console reasoning display to a separate phase; this is that phase.

Phase 107a extends the `tasks.get` Enricher seam (Phase 73d) with a `Trajectory(...)` method, adds a `Trajectory *TaskTrajectoryRef` field to `TaskDetail`, wires a production enricher that exposes the planner's in-memory trajectory by task id, and renders a collapsible "reasoning" accordion in the Playground chat bubble. Independent of Phase 107 streaming — the two phases can ship in any order; Phase 107 + Phase 107a together set up streaming reasoning (the same chunk-event channel can later carry `OnReasoning` deltas tagged `ChunkReasoning`, populating the accordion live).

## RFC anchor

- RFC §7 — Console as Protocol client (the Playground reads `tasks.get`, never internal planner state).
- RFC §6.5 — LLM client + reasoning trace contract (where reasoning lands on the response per provider).
- RFC §6.8 — task lifecycle + `TaskDetail` projection contract.
- RFC §1 — first-five-minutes adoption guarantee (operators discount agents that look like black boxes; surfacing the thinking trace is the cheapest legibility win available).

## Briefs informing this phase

- brief 11 — Console feature surface (Playground is the load-bearing operator entry; brief 11 §"Playground" specifically lists "reasoning visibility" as the highest-impact non-streaming feature).
- brief 13 — operator UX (reasoning visibility framing).
- brief 06 — events / observability / devx (the trajectory's relationship to `inspect-runs --json` and the broader observability surface).

## Brief findings incorporated

- **brief 11 §"Playground operator UX, reasoning".** "Operators routinely cite `Claude.ai's "thinking" accordion / Cursor's intermediate-step pane` as the feature that converts an agent from a black box into something they trust. Harbor already captures the trace; not surfacing it is the V1 adoption gap most cheaply closed." Phase 107a surfaces it.
- **brief 13 §"Operator UX, perceived legibility".** "A model that produces a one-line answer after 8 seconds of silence reads as luck; a model that produces the same answer with visible reasoning reads as competence. The model is doing the same work in both cases." Direct quote — the reasoning channel is the legibility signal.
- **brief 06 §"Trajectory as the observability spine".** "Every per-task observation the runtime can produce should be reachable from `tasks.get` so a third-party UI / CLI / debugger sees the same data the Console sees. The Enricher interface (Phase 73d) is the seam." Phase 107a extends the seam — no new Protocol method.

## Findings I'm departing from (if any)

None.

## Goals

- `internal/protocol/types/tasks.go::TaskDetail` carries an optional `Trajectory *TaskTrajectoryRef` field. Non-nil for runs whose trajectory the runtime still has in memory; nil otherwise (a long-completed run whose trajectory the runtime evicted — graceful absence, never silent degradation).
- The `tasks.get` Enricher (Phase 73d, `internal/tasks/protocol/registry_projector.go`) gains a `Trajectory(ctx, id, taskID)` method. The production enricher (the one `cmd/harbor` constructs) looks up the trajectory from the run-loop driver's per-run state; a fallback enricher (`internal/tasks/protocol::NoOpEnricher`) returns nil for trajectory.
- The Playground (`web/console/src/routes/(console)/playground/[session_id]/+page.svelte`) renders a "Reasoning (N steps)" toggle on each agent bubble whose `TaskDetail.Trajectory` carries ≥1 step with a non-empty `ReasoningTrace`. Clicking expands the collapsible list of per-step trace strings.
- The chat-bubble component (`web/console/src/lib/chat/`) gets a `reasoningSteps?: ReasoningStep[]` field on `ChatMessage` and a `<ReasoningAccordion>` sub-component that consumes it. The accordion is collapsed by default; the operator opts in per bubble.
- The `task.completed` SSE handler in the Playground (Phase 106 wiring) re-fetches `tasks.get` and now populates BOTH the answer text AND the reasoning steps.

## Non-goals

- **Streaming reasoning.** Phase 107a renders the *post-completion* trajectory. Live per-token reasoning streaming is Phase 107's job (and is set up to flow through the same `OnChunk`/`ChunkKind` channel — see Phase 107 AC-2). Phase 107a's accordion is a static snapshot; Phase 107 + 107a together can be composed to make the accordion populate live.
- **Persistent trajectory storage.** The Enricher reads from the run-loop driver's in-memory trajectory map. A trajectory becomes unavailable when the driver evicts it (currently never within a run's lifetime, but a future GC pass might). Phase 107a does NOT add a durable trajectory table; that's a separate post-V1 phase (the planner-checkpoint store is the natural home — see Phase 51 / D-080).
- **Cross-tenant reasoning visibility.** Per RFC §7, the Fleet view never sees another tenant's reasoning. Identity-mandatory filtering applies; the Enricher rejects cross-identity reads (the same posture `tasks.Get` already enforces).
- **Reasoning redaction.** Reasoning text is the model's own output, not user-input. The audit redactor's default rule set (credentials / PII patterns) still applies — but Phase 107a does NOT add reasoning-specific redaction. A reasoning trace that includes a credential is a model failure, not a Console rendering failure.
- **Multi-trajectory display** (e.g. parallel-planner branches). The Playground renders the linear `Trajectory.Steps[]` — one agent, one trace. Multi-agent / parallel branches are post-V1.

## Acceptance criteria

- [ ] **AC-1** `internal/protocol/types/tasks.go::TaskDetail` gains `Trajectory *TaskTrajectoryRef` field with `json:"trajectory,omitempty"`. `TaskTrajectoryRef` is a new struct: `{ Steps []TaskTrajectoryStep }`. `TaskTrajectoryStep` is `{ Index int; ReasoningTrace string }` — keep the projection narrow (no full action / observation projection in Phase 107a; that's a later expansion).
- [ ] **AC-2** `internal/tasks/protocol::Enricher` interface gains `Trajectory(ctx context.Context, id identity.Identity, taskID string) *prototypes.TaskTrajectoryRef`. The `NoOpEnricher` (the V1 default) returns nil. The production enricher (`cmd/harbor` constructs it) consults the run-loop driver's trajectory map.
- [ ] **AC-3** `cmd/harbor/cmd_dev_runloop.go` exposes a thread-safe `TrajectoryByTaskID(taskID) *planner.Trajectory` method on the run-loop driver (or equivalent — the existing driver already holds per-task state). Concurrent reads are safe under N=100 goroutines per CLAUDE.md §5 / D-025.
- [ ] **AC-4** `cmd/harbor/cmd_dev.go` constructs the production Enricher with `WithTrajectory(driver.TrajectoryByTaskID)` (or equivalent). The Enricher's `Trajectory(...)` method calls into the driver and projects each `*planner.Step` into a `TaskTrajectoryStep`. Steps with empty `ReasoningTrace` are filtered out (no need to render N empty boxes).
- [ ] **AC-5** `internal/tasks/protocol/registry_projector.go::GetTask` calls `p.enricher.Trajectory(...)` after the existing enrichment calls (parent_session, cost, planner_snapshot) and populates `detail.Trajectory`. Identity-mandatory; the projector already passes the caller's identity.
- [ ] **AC-6** Console — `web/console/src/lib/chat/types.ts::ChatMessage` gains `reasoningSteps?: ReasoningStep[]` where `ReasoningStep = { index: number; reasoning_trace: string }` (matches the wire shape).
- [ ] **AC-7** Console — new `web/console/src/lib/chat/ReasoningAccordion.svelte` component. Renders nothing when `reasoningSteps` is empty / undefined. Otherwise renders a collapsible toggle "Reasoning (N steps)" — clicking expands the list. Each step shows `step.index` + `step.reasoning_trace` (text only; no markdown rendering in Phase 107a — plain `<pre>` for legibility).
- [ ] **AC-8** Console — the playground page's `task.completed` handler (Phase 106 wiring) parses the new `trajectory.steps[]` from `TaskDetail` and populates `reasoningSteps` on the matching agent bubble alongside the existing answer-text population.
- [ ] **AC-9** Console — `web/console/src/routes/(console)/playground/[session_id]/answer-envelope.ts` extends with `parseReasoningSteps(detail)` that returns the filtered list of `ReasoningStep[]`. Unit-tested per AC-12.
- [ ] **AC-10** No regression to Phase 106 — `parseAnswerFromDetail` still returns the answer text; the new `parseReasoningSteps` is additive.
- [ ] **AC-11** Console — the chat bubble renderer mounts `<ReasoningAccordion reasoningSteps={message.reasoningSteps} />` above the bubble's main text. Per CONVENTIONS.md §1 the accordion uses design-system tokens only (no raw color / spacing literals).
- [ ] **AC-12** Unit (Svelte) — `answer-envelope.test.ts` extends with `parseReasoningSteps` tests: (a) detail with no trajectory → `[]`; (b) trajectory with two steps, one with empty trace → returns one step; (c) trajectory with three populated steps → returns three steps in index order.
- [ ] **AC-13** Unit (Go) — `internal/tasks/protocol/registry_projector_test.go` extends with `TestProjectDetail_TrajectoryPopulatedFromEnricher` (enricher returns a `*TaskTrajectoryRef` → `detail.Trajectory` populated) and `TestProjectDetail_TrajectoryNilWhenEnricherReturnsNil` (graceful absence — no empty struct, no panic).
- [ ] **AC-14** Unit (Go) — `cmd/harbor/cmd_dev_runloop_test.go::TestTrajectoryByTaskID_Concurrent` runs N=100 concurrent reads + writes under `-race`. Per CLAUDE.md §5 + D-025.
- [ ] **AC-15** Integration (Playwright) — `web/console/tests/reasoning-accordion.spec.ts` against real `harbor dev` (the YouTube agent): send a prompt that elicits ≥1 reasoning step, assert the bubble renders a "Reasoning (N steps)" toggle, click it, assert ≥1 trace string is visible. SKIP when no LLM provider key.
- [ ] **AC-16** Smoke — `scripts/smoke/phase-107a.sh` exercises the wire surface: `tasks.get` against a completed run returns `trajectory` populated with ≥1 step bearing a non-empty `reasoning_trace`. SKIP gracefully on no provider key.
- [ ] **AC-17** No new Protocol method (consumes existing `tasks.get`).
- [ ] **AC-18** No change to `TaskResult.Value` envelope (Phase 106 contract unchanged).

## Files added or changed

### Runtime (Go)

- `internal/protocol/types/tasks.go` — add `TaskDetail.Trajectory`, `TaskTrajectoryRef`, `TaskTrajectoryStep`. ~25 LOC.
- `internal/tasks/protocol/registry_projector.go` — extend Enricher interface + GetTask call site. ~20 LOC.
- `internal/tasks/protocol/registry_projector_test.go` — AC-13 tests. ~80 LOC.
- `cmd/harbor/cmd_dev_runloop.go` — expose `TrajectoryByTaskID(taskID) *planner.Trajectory` thread-safe accessor. ~30 LOC.
- `cmd/harbor/cmd_dev_runloop_test.go` — AC-14 concurrent-reuse test. ~50 LOC.
- `cmd/harbor/cmd_dev.go` — construct enricher with `WithTrajectory(driver.TrajectoryByTaskID)`. ~10 LOC.

### Console (Svelte)

- `web/console/src/lib/chat/types.ts` — add `reasoningSteps?: ReasoningStep[]` on `ChatMessage` + the `ReasoningStep` type. ~10 LOC.
- `web/console/src/lib/chat/ReasoningAccordion.svelte` — **NEW**. ~80 LOC.
- `web/console/src/routes/(console)/playground/[session_id]/answer-envelope.ts` — extend with `parseReasoningSteps(detail)`. ~25 LOC.
- `web/console/src/routes/(console)/playground/[session_id]/answer-envelope.test.ts` — AC-12 tests. ~60 LOC.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — populate `message.reasoningSteps` in the `task.completed` handler; render `<ReasoningAccordion>` above the agent bubble text. ~25 LOC.
- `web/console/src/lib/chat/ChatPanel.svelte` (or chat-bubble component, depending on the existing surface) — mount the accordion. ~10 LOC.

### E2E

- `web/console/tests/reasoning-accordion.spec.ts` — **NEW**. ~100 LOC per AC-15.

### Smoke + drift

- `scripts/smoke/phase-107a.sh` — **NEW**. PREFLIGHT_REQUIRES: live-server. ~60 LOC.

### Docs

- `docs/glossary.md` — new entries: `TaskTrajectoryRef`, `ReasoningAccordion`, `Enricher.Trajectory`. Alphabetised.
- `docs/skills/drive-the-playground/SKILL.md` — short paragraph: "click the 'Reasoning (N steps)' toggle on any agent bubble to see the model's intermediate thinking trace". (CLAUDE.md §18 — same-PR skill update.)

## Public API surface

- `prototypes.TaskDetail.Trajectory *TaskTrajectoryRef` — new optional wire field. JSON tag `trajectory,omitempty`. Backward-compatible additive change.
- `prototypes.TaskTrajectoryRef{Steps []TaskTrajectoryStep}` — new wire type.
- `prototypes.TaskTrajectoryStep{Index int; ReasoningTrace string}` — new wire type.
- `tasksprotocol.Enricher.Trajectory(ctx, id, taskID) *TaskTrajectoryRef` — new interface method. The `NoOpEnricher` (existing default) returns nil; concrete enrichers MAY implement.
- Console `ChatMessage.reasoningSteps?: ReasoningStep[]` — additive TS field.

## Test plan

### Unit (Go)

- `internal/tasks/protocol/registry_projector_test.go::TestProjectDetail_TrajectoryPopulatedFromEnricher`
- `internal/tasks/protocol/registry_projector_test.go::TestProjectDetail_TrajectoryNilWhenEnricherReturnsNil`
- `cmd/harbor/cmd_dev_runloop_test.go::TestTrajectoryByTaskID_Concurrent` (AC-14)
- `cmd/harbor/cmd_dev_runloop_test.go::TestTrajectoryByTaskID_EvictedTaskReturnsNil` (graceful absence)

### Unit (Svelte)

- `answer-envelope.test.ts::parseReasoningSteps_emptyOnNoTrajectory`
- `answer-envelope.test.ts::parseReasoningSteps_filtersEmptyTraces`
- `answer-envelope.test.ts::parseReasoningSteps_preservesIndexOrder`
- (Optional) `ReasoningAccordion` rendering test if `@testing-library/svelte` lands; otherwise covered by Playwright.

### Integration (Playwright)

- `reasoning-accordion.spec.ts::TestE2E_Playground_ReasoningAccordionRendersAfterCompletion` (AC-15)
- `reasoning-accordion.spec.ts::TestE2E_Playground_NoAccordionForTraceLessRun` — a planner config with `reasoning_replay: never` and no reasoning capture should NOT render the toggle.

### Concurrency / leak

- `TestTrajectoryByTaskID_Concurrent` (Go) — AC-14.

### Conformance

- N/A — the Enricher interface extension is additive; the conformance suite for `tasks.get` already covers projection correctness.

## Smoke script additions

`scripts/smoke/phase-107a.sh` — PREFLIGHT_REQUIRES: live-server. Assertions:

1. SKIP when no LLM provider key.
2. Bootstrap a dev token + start a task that elicits ≥1 reasoning step (a multi-step query like "List three primes, then add them").
3. Poll until `task.status === complete` (bounded 30s).
4. Fetch `/v1/tasks/get` for the task.
5. Assert response has `.trajectory.steps` array.
6. Assert `.trajectory.steps[0].reasoning_trace` is non-empty (OR SKIP with rationale if the configured model doesn't emit reasoning).
7. Static: `grep -q 'TaskTrajectoryRef' internal/protocol/types/tasks.go`.
8. Static: `grep -q 'ReasoningAccordion' web/console/src/routes/(console)/playground/[session_id]/+page.svelte`.

## Coverage target

- `internal/protocol/types/`: no test target — pure type declarations.
- `internal/tasks/protocol/`: 85% (existing). The projector edit is small; new tests cover the trajectory branch.
- `cmd/harbor/`: 80% (existing). The accessor + enricher wiring is covered.
- `web/console/src/lib/chat/` + `web/console/src/routes/(console)/playground/`: 80% (existing CONVENTIONS.md §10 target).

## Dependencies

- 73d — `tasks.get` Enricher seam (existing).
- 83e — react reasoning-channel decoupling (existing; Phase 107a is the deferred Console-render side).
- 106 — Playground answer plumbing (the accordion sits in the bubble Phase 106 created).
- (Optional, post-merge) 107 — streaming chunks. Phase 107a does NOT depend on 107; the two compose later when both ship.

## Risks / open questions

- **Trajectory eviction.** Currently the run-loop driver retains the trajectory for the run's lifetime (and post-completion until process exit, since trajectories live in an in-memory map). A long-running `harbor dev` server could grow unbounded. Mitigation deferred: Phase 51's planner-checkpoint store is the natural durability seam; Phase 107a flags the in-memory growth as a known V1.3 limitation and adds a "trajectory unavailable (run too old)" graceful-absence message to the accordion when the projector returns nil for a known task id.
- **Reasoning visibility + audit posture.** A reasoning trace that quotes user input could echo a credential the operator typed in a prompt. The audit redactor's default rule set runs on the trace before it lands on the wire. Test pin: an integration test that prompts with a known fake credential and asserts the redactor's signature appears in the rendered trace.
- **Phase 83e's `ReasoningReplay` knob.** When set to `never` (the default), the planner DOES still capture the trace on `Step.ReasoningTrace` — it just doesn't replay it into subsequent prompts. So the Playground accordion works the same way regardless of replay mode. Test pin verifies this.
- **Future "rich reasoning"**. Some providers (Anthropic extended thinking, Gemini `thought:true`) emit structured / typed reasoning. Phase 107a renders plain text; richer rendering (e.g. nested action chains, mermaid diagrams) is a follow-up phase.
- **Phase 107 streaming hookup**. When Phase 107 ships, the natural extension is: on each `ChunkReasoning` chunk event, append to the accordion's last step's `reasoning_trace`. This requires the accordion to support in-flight updates — easy because `ReasoningAccordion.svelte` already takes `reasoningSteps` as a Svelte 5 reactive prop; the runes mode `$state` propagation handles incremental updates without rework.

## Glossary additions

- **`TaskTrajectoryRef`** — Phase 107a Protocol type carrying the projected planner trajectory's reasoning trace per task. Lives on `TaskDetail.Trajectory`. (Alphabetised under T.)
- **`Enricher.Trajectory`** — Phase 107a addition to the `tasks.get` Enricher interface (Phase 73d). Returns a `*TaskTrajectoryRef` for a given task id, or nil when the trajectory is unavailable. (Alphabetised under E.)
- **`ReasoningAccordion`** — Phase 107a Console component (`web/console/src/lib/chat/ReasoningAccordion.svelte`) rendering a collapsible per-bubble list of the model's reasoning-trace strings. (Alphabetised under R.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Concurrent-reuse test — AC-14
- [ ] Integration test — `reasoning-accordion.spec.ts` against a real LLM-backed `harbor dev`
- [ ] Glossary updated for `TaskTrajectoryRef`, `Enricher.Trajectory`, `ReasoningAccordion`
- [ ] `docs/skills/drive-the-playground/SKILL.md` updated per CLAUDE.md §18

## Implementation order (suggested)

1. **Add the wire types** (`TaskDetail.Trajectory`, `TaskTrajectoryRef`, `TaskTrajectoryStep`). Verify `go build ./...` clean.
2. **Extend the Enricher interface** + `NoOpEnricher` returns nil. Existing projector tests still pass.
3. **Add `TrajectoryByTaskID` accessor** to the run-loop driver. Verify with the AC-14 concurrent-reuse test.
4. **Wire the production enricher** in `cmd_dev.go`. Verify with `cmd_dev_runloop_test.go::TestRunLoop_TaskGetSurfacesTrajectory`.
5. **Extend the projector** to call `enricher.Trajectory(...)`. Verify with `registry_projector_test.go::TestProjectDetail_TrajectoryPopulatedFromEnricher`.
6. **Add `parseReasoningSteps`** to the answer-envelope module + unit tests.
7. **Add `<ReasoningAccordion>`** Svelte component + design-token-only styles.
8. **Mount the accordion** in the Playground page + populate from the `task.completed` handler.
9. **Add the Playwright spec** + the smoke script.
10. **Run `make drift-audit && make preflight`** — both green.
11. **Open PR.**
