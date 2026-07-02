# Phase 146 — Per-task structured output: the `answer_payload` per-task producer

## Summary

Extends run-level structured output (Phase 143, D-272) from the embed runner to Protocol-initiated per-task runs: a new ADDITIVE `output_schema` field on the `start` wire request (`types.StartRequest`) rides request → `tasks.SpawnRequest` → the persisted `tasks.Task` record → the per-task RunLoop drivers, which compile it ONCE at run start (the same `planner.CompileOutputSchema` machinery — no second compile path, §13), let the React driver's existing per-turn steering engage untouched, validate the terminal `Finish` payload through ONE promoted edge-validation implementation shared with `RunOnce`, and marshal the validated `answer_payload` onto the task's answer envelope — so `tasks.get`'s `result_inline` and the AwaitTask observation projection surface it. Both godoc reservations that pinned this gap ("per-task Protocol runs do not yet produce it — the per-task run loop has no output-schema plumbing": `internal/runtime/dispatch/dispatch.go` `taskOutcomeObservation` doc, `internal/tasks/tasks.go` `TaskResult` doc) flip to shipped wording. Failure posture: schema-invalid after the retry budget → the task fails LOUD with a new typed terminal code (`output_invalid`, mirroring `planner.ErrOutputInvalid`), never a silent schemaless success. D-276.

## RFC anchor

- RFC §6.5 (the "Run-level structured output (shipped — D-272)" paragraph — this phase adds the per-task producer for the same mechanism; the paragraph gains a per-task sentence in the same PR)
- RFC §6.2 ("schema mode" among the runtime-level run options — this phase carries that option across the Protocol task-start surface)
- RFC §6.8 (tasks — the `TaskResult` answer-envelope contract this phase extends additively; `SpawnRequest` / `Task` shapes)
- RFC §5.2 (what the Protocol exposes: the `start` task-control method gaining the field; `tasks.get` state snapshot surfacing the payload)
- RFC §5.3 (Protocol versioning — the field is additive `omitempty`; no version bump, no breaking change)

## Briefs informing this phase

- brief 05
- brief 07

## Brief findings incorporated

- brief 05 §5 ("Sharp edges to design out"): "Foreground/background identity is split... **Harbor unifies under `TaskID` — runs are tasks of kind `foreground`.**" A run's typed-output contract must therefore be a property of the RUN wherever it is hosted: the per-task path is the same planner run one layer up from `RunOnce`, so it consumes the SAME schema option, compile site, steering, and validator — never a parallel per-task implementation (§13).
- brief 05 §6 ("Tests required"): "**Concurrency tests.** N concurrent sessions × M concurrent tasks each, asserting no cross-talk in events, memory, artifacts, **or task results**." The concurrency gate for this phase interleaves schema-constrained tasks carrying DISTINCT schemas with plain tasks and asserts no schema/payload bleed across task results.
- brief 07 §6: "Optional `response_format` hint: `{"type": "json_object"}` or `{"type": "json_schema", ...}`. **The runtime is responsible for sanitizing/downgrading per provider; the client just forwards.**" The per-task path adds a new PRODUCER for the run-level option; the LLM client contract gains nothing.
- brief 07 §"What a structured-output correction layer actually does": "**bake structured-output correction into the single LLM client from t=0; don't ship two parallel modes.**" This phase adds NO output-shaping strategy, no second validation loop, no second envelope builder — it plumbs the schema to the existing single implementation and PROMOTES the one remaining duplicated seam (the terminal edge validation) into a shared home.

## Findings I'm departing from (if any)

None.

## Goals

- A Protocol client can ask a task-shaped run for a schema-conforming final answer: `POST /v1/control/start` with `output_schema` set → the completed task's envelope carries the VALIDATED `answer_payload`, readable via `tasks.get` (`result_inline`) and via a parent run's AwaitTask observation. This closes the run-level/per-task asymmetry Phase 143 deliberately left (D-272 shipped the embed runner only; the reservations at `dispatch.go::taskOutcomeObservation` and `tasks.go::TaskResult` recorded the gap).
- Zero default-path change. No `output_schema` → byte-identical wire shape (`omitempty`), byte-identical spawn behavior, byte-identical three-key task envelope (golden-pinned on the driver path).
- ONE implementation everywhere (§13). Schema compile: `planner.CompileOutputSchema` (Phase 143's single compile implementation) — called at the Protocol edge (reject-early) and at driver run start (the compile that the run consumes). Generation steering: the React driver's existing `applyOutputSchema` per-turn wiring engages purely by setting `RunSpec.Base.OutputSchema` — zero planner change. Terminal validation: `Stack.RunOnce`'s edge validation + `capturePayloadJSON` (`internal/runtime/assemble/runonce.go:339-344, 393`) are PROMOTED to one shared `runctx` envelope builder that `RunOnce`, the production per-task driver, and the devstack twin all call — because `steering.RunLoop.Run` does NOT validate terminal output (validation is the caller's edge, by D-272 design), each run-loop driver must invoke the same validator, so the validator moves to one home first.
- Fail-loud end-to-end: a bad schema is rejected at the Protocol edge with `CodeInvalidRequest` before a task ever spawns; a schema-invalid answer after the retry budget fails the task with the new typed terminal code `planner.TaskErrorCodeOutputInvalid` (mirroring `planner.ErrOutputInvalid`) — never `MarkComplete` with an unvalidated envelope, never a silent fall-back to schemaless text (§13).
- The D-272 streaming posture carries over: a schema-constrained task suppresses assistant-content and reasoning token deltas on the per-task streaming path (`RunContext.OnChunk` → `llm.completion.chunk`); step boundaries and tool-dispatch events stream as today; the validated answer arrives once, in the envelope. Documented on the wire field's godoc — a decision, not a surprise.
- The heavy-output discipline stays true: `taskOutcomeObservation` already routes through `projectForLLM` (`internal/runtime/dispatch/dispatch.go:386, 418, 518` — D-026), and its godoc already promises a future `answer_payload` "rides the SAME heavy-output offload". This phase makes that promise live and pins it with a test.

## Non-goals

- **Planner-emitted `SpawnTask` decisions carrying schemas.** The `Decision` sum is sealed (D-047); teaching a planner to request typed output from a child task is a future decision with its own wire/emission design — recorded in D-276 so the sequencing is explicit, not silence.
- **Agent-config-level default schemas.** Wrong granularity: the agent-config control plane (D-234) projects per-agent DESIRED STATE next-turn; an output schema is a per-request property of one caller's one run. Settled by the coordinator review + user sign-off; a config-level default would be a new decision against D-276.
- **Partial-object streaming** of the typed payload (issue #444 — the named D-272 follow-up; explicitly out of scope here too).
- Exposing the task's stored schema on `tasks.get` / `TaskDetail`. No consumer exists (D-062/§13 read backwards); additive later with the first Console surface that wants it.
- Any new Protocol method, error code, or event type. One field on one existing wire type.
- Any change to the 143 mechanism itself: `WithOutputSchema`, the compile/validate machinery, the React steering, the `OutputMode` strategy/downgrade chain are consumed as-is.

## Acceptance criteria

- [x] `types.StartRequest` gains `OutputSchema json.RawMessage` with tag `json:"output_schema,omitempty"` (single-source: `internal/protocol/types/control.go` — §8). Absent → wire bytes and spawn behavior byte-identical to v1.9 (round-trip test extended; omit-when-empty pinned like `input_artifact_ids`).
- [x] Edge validation in `ControlSurface.dispatchStart` (`internal/protocol/control.go`): a present-but-empty, non-compiling (via `planner.CompileOutputSchema` — the one compile implementation), or over-cap schema (documented size cap, 64 KiB default) → `CodeInvalidRequest` naming the reason, BEFORE `Spawn` runs. No task is created that the edge could have rejected.
- [x] Plumbing: `tasks.SpawnRequest.OutputSchema` and `tasks.Task.OutputSchema` (`json.RawMessage`, additive). Both task drivers persist it via their existing whole-record JSON marshal (in-process + durable — no migration; old records unmarshal with the field absent). The tasks conformance suite gains a Spawn→Get round-trip of the field so all drivers carry it identically.
- [x] Driver run-start plumbing, BOTH twins (§17.6/D-094): the production per-task driver (`cmd/harbor/cmd_dev_runloop.go` — the `steering.RunSpec{Base: planner.RunContext{...}}` construction) and the devstack twin (`harbortest/devstack/devstack.go`) compile `task.OutputSchema` once at run start and set `RunSpec.Base.OutputSchema` (`planner.RunContext.OutputSchema`, planner.go:388) — the React driver's existing per-turn steering (`applyOutputSchema`: ResponseFormat + tool-call-aware Validator + retry-with-feedback) engages with zero planner change. A run-start compile failure → `MarkFailed{code: output_invalid}` loud; the LLM is never called on a degraded run.
- [x] ONE terminal-validation implementation: the `RunOnce`-edge validation + `capturePayloadJSON` are promoted from `internal/runtime/assemble/runonce.go` into a shared `internal/runtime/runctx` envelope builder (working name `runctx.FinishAnswerEnvelope(fin, traj, schema) (planner.AnswerEnvelope, error)`: FinishGoal + schema → capture exact bytes at the validation site, validate, `AnswerPayload` set, `Answer` = string rendering; no schema or non-goal finish → the plain three-key envelope). `RunOnce` re-bases on it with byte-identical envelopes (existing goldens stay green); both per-task drivers call it after `RunLoop.Run` returns. No third copy of the validation or of envelope construction survives (grep-asserted in the smoke).
- [x] Envelope: on a validated FinishGoal the marshaled `TaskResult.Value` carries `answer_payload` (the exact validated bytes — never a lossy string round-trip); absent schema → the three-key envelope is byte-identical (driver-path golden test). `tasks.get` surfaces it through `result_inline` unchanged (opaque projection; heavy results still flip to `ResultRef`).
- [x] Failure posture: schema-invalid after the retry budget fails the task loud with the new terminal code `planner.TaskErrorCodeOutputInvalid` (`"output_invalid"`), on BOTH failure shapes — a `RunLoop.Run` error whose chain carries the schema-retry exhaustion, and an edge-validation `planner.ErrOutputInvalid` on a returned FinishGoal. No code path `MarkComplete`s an unvalidated envelope when the task carries a schema (asserted by test + smoke grep). The parent's AwaitTask observation carries the failed task's `{code: output_invalid}` error.
- [x] Observation projection: `taskOutcomeObservation` surfaces `answer_payload` to the awaiting parent via its existing generic parse (no shape change); the "reserved... per-task runs do not yet produce it" godoc at `internal/runtime/dispatch/dispatch.go` and `internal/tasks/tasks.go` flips to shipped wording. D-026 stays true: a large `answer_payload` riding an AwaitTask observation goes through `projectForLLM`'s existing offload (test pins the ArtifactRef-shaped observation; no second content-size mechanism).
- [x] Streaming posture: on a schema-constrained task, both drivers suppress assistant-content AND reasoning token deltas at their `OnChunk` seam (step-boundary `done` signals and `tool_dispatched` events still fire) — the D-272 buffered pairing, mirrored; stated in the `output_schema` field godoc and the drivers' comments.
- [x] D-223 lockstep, full dance: `make protocol-ts-gen` regenerates `wire-manifest.gen.json` (StartRequest gains `output_schema`); the typed Console client's `start()` (`web/console/src/lib/protocol/client.ts`) gains an optional `outputSchema` opt mapped to `body.output_schema` (StartRequest stays on the justified untyped allowlist — entry unchanged); `make protocol-ts-gen-check` and `npm run lint`/`check` green. D-209: `make protocol-docs-gen` regenerated pages committed in the same PR; `make protocol-docs-gen-check` green.
- [x] §13 primitive-with-consumer — THE consumer is the AwaitTask round-trip E2E the v1.9 wave-end audit specified: `test/integration/phase146_task_output_schema_test.go` starts a schema-constrained task over the task-start surface (scripted-LLM devstack, real inmem drivers), a parent run awaits it, and the VALIDATED `answer_payload` arrives in the parent's observation; identity propagation asserted; failure mode: a schema-invalid-after-budget task fails loud (`output_invalid`) and the parent observes the error, never a schemaless success; `-race`.
- [x] D-025 concurrency: N≥100 concurrent tasks against one shared driver/stack, interleaving schema-constrained tasks with DISTINCT schemas and plain tasks; no schema/payload bleed across task results (brief 05 §6), no cancellation cross-talk, goroutine baseline restored, `-race`.
- [x] §18 sweep: grep `docs/skills/` for `surface: protocol` / `surface: tasks` playbooks demonstrating `start` and update in the same PR; the docs-site quickstart's executed curl steps are unaffected (additive field) — verified by `scripts/smoke/phase-113a.sh` staying green.
- [x] `scripts/smoke/phase-146.sh` flips from skeleton to real assertions (below).

## Files added or changed

- `internal/protocol/types/control.go` — `StartRequest.OutputSchema` (+ round-trip/omitempty tests)
- `internal/protocol/control.go` — `dispatchStart` edge validation + `SpawnRequest` mapping
- `internal/tasks/tasks.go` — `SpawnRequest.OutputSchema`, `Task.OutputSchema`, `TaskResult` godoc flip
- `internal/tasks/conformancetest/` — field round-trip case
- `internal/planner/` — `TaskErrorCodeOutputInvalid` terminal code
- `internal/runtime/runctx/` — the promoted shared envelope builder (+ tests; `capturePayloadJSON` moves here)
- `internal/runtime/assemble/runonce.go` — re-based on the shared builder (goldens unchanged)
- `internal/runtime/dispatch/dispatch.go` — `taskOutcomeObservation` godoc flip + D-026 offload test
- `cmd/harbor/cmd_dev_runloop.go` — run-start compile, `Base.OutputSchema`, shared-builder envelope, `output_invalid` mapping, token-delta suppression
- `harbortest/devstack/devstack.go` — the §17.6 twin of all of the above
- `web/console/src/lib/protocol/client.ts` — `start()` `outputSchema` opt; `web/console/src/lib/protocol/wire-manifest.gen.json` — regenerated
- `docs/site/protocol/` — regenerated pages (D-209)
- `test/integration/phase146_task_output_schema_test.go`
- `scripts/smoke/phase-146.sh`
- `docs/glossary.md`, `docs/decisions.md` (D-276), `docs/plans/README.md` (row + detail), `RFC-001-Harbor.md` (§6.5 per-task sentence), `docs/skills/` (per §18 sweep)

## Public API surface

- `types.StartRequest.OutputSchema json.RawMessage` (`json:"output_schema,omitempty"`) — the wire field
- `tasks.SpawnRequest.OutputSchema json.RawMessage`; `tasks.Task.OutputSchema json.RawMessage`
- `planner.TaskErrorCodeOutputInvalid` — the typed terminal error code (`"output_invalid"`)
- TS: `HarborClient.start(query, { outputSchema? })` — typed-client parity
- (Internal but load-bearing: the shared `runctx` envelope builder — the ONE terminal-validation + envelope-construction implementation `RunOnce` and both per-task drivers consume.)

## Test plan

- **Unit:** StartRequest JSON round-trip + omit-when-empty; `dispatchStart` edge rejection table (empty / non-compiling / over-cap → `CodeInvalidRequest`; valid → spawned with field); request→`SpawnRequest`→`Task` plumbing; shared envelope builder table (goal+schema valid / invalid / non-JSON / non-goal / no-schema — plus the absent-schema byte-identical golden and a with-payload golden); `RunOnce` goldens unchanged post-promotion; driver `output_invalid` mapping on both failure shapes; token-delta suppression (content + reasoning dropped, step `done` + tool-dispatch preserved).
- **Integration:** the §13 AwaitTask round-trip E2E above (happy + schema-invalid-after-budget failure mode, real inmem drivers, identity propagation, `-race`); a `tasks.get` leg asserting `result_inline` carries `answer_payload`; the D-026 leg (oversized payload → ArtifactRef-shaped parent observation via `projectForLLM`).
- **Conformance:** tasks-driver round-trip of `Task.OutputSchema` (in-process + durable over the state-store triad).
- **Concurrency / leak:** the N≥100 mixed-traffic distinct-schema test above (no bleed, no cancellation cross-talk, goroutine baseline, `-race`).

## Smoke script additions

`scripts/smoke/phase-146.sh` (`PREFLIGHT_REQUIRES: live-server`):

- Live (always-on): `POST /v1/control/start` with a NON-COMPILING `output_schema` → non-200 + `CodeInvalidRequest`-shaped error (exercises the edge gate with no LLM dependency).
- Live (degradable, phase-106 pattern): start a schema-constrained task with a trivial schema + query, poll `tasks.get` to terminal; on `complete` assert `result_inline` parses and `answer_payload` validates against the sent schema; on `failed` under a keyless/mock-LLM preflight env → SKIP with the failure shape logged (the envelope plumbing needs a working LLM; the unit/integration legs carry correctness).
- Static: `output_schema` appears in the committed `wire-manifest.gen.json` under StartRequest AND in the generated `docs/site/protocol/types.md` (D-209 regen landed); grep pins exactly ONE terminal-validation implementation (`ErrOutputInvalid` validation sites = the shared builder only) and no `MarkComplete` on an unvalidated schema path.
- Unit-test leg: `go test -race` over `internal/tasks`, `internal/runtime/runctx`, and `TestE2E_Phase146_*`.

Skeleton parks with `skip` until the surface lands.

## Coverage target

- `internal/protocol` (touched files): no regression below current package coverage (≥85% on touched lines)
- `internal/tasks`: 85%
- `internal/runtime/runctx`: 80%
- `internal/runtime/assemble` (touched lines): no regression below post-143 coverage
- `cmd/harbor` + `harbortest/devstack`: exercised via the E2E + smoke legs (no per-package numeric gate, matching prior run-loop phases)

## Dependencies

- 143 (the structured-output mechanism: compile/validate machinery, `RunContext.OutputSchema`, React steering, `AnswerPayload` — D-272)
- 110a (the canonical `planner.AnswerEnvelope` export — D-194)
- 118 (the D-223 TS lockstep gate this wire change must satisfy)
- 54 (the Protocol task control surface: `start` / `StartRequest`)
- 73d (`tasks.get` + the `result_inline` projection)
- 87 (the durable TaskService backend the persisted schema field rides)
- 107e (SpawnTask/AwaitTask dispatch + `taskOutcomeObservation` — the parent-observation surface the consumer E2E exercises, D-170)

## Risks / open questions

- **Idempotency-key reuse with a differing schema.** `Spawn` dedupes on `(session, IdempotencyKey)`; a genuine retry carries the same body (same schema) and is safe. A REUSED handle with a DIFFERENT `output_schema` is caller misuse: the original spawn's schema governs, documented on the wire field's godoc. A loud mismatch rejection requires a `Get`+compare in `dispatchStart` — implement if cheap, otherwise the documented posture stands (decide at implementation; record in the PR).
- **The drivers do not use `runctx.NewRunContext`.** Both per-task drivers construct `planner.RunContext` literally (via individual `runctx` helpers), so the run-start compile is a direct `planner.CompileOutputSchema` call rather than the `Sources.OutputSchema` threading `RunOnce` uses. Same single compile implementation, two call sites — acceptable; a full driver migration onto `NewRunContext` is a named non-goal-sized refactor for a future hygiene phase, not this one.
- **Preflight LLM availability.** The smoke's happy-path leg degrades to SKIP when the preflight env lacks a working LLM (the phase-106 precedent); the edge-rejection leg + static legs keep the phase's smoke `OK > 0` regardless.
- **Playground UX on schema tasks.** Token suppression means no live assistant tokens for a schema-constrained task in the Console; the answer appears on completion via `result_inline`. This mirrors D-272's documented posture; the field godoc + D-276 state it so it reads as a decision. No Console page change ships (no Console producer sets the field yet).
- **Two-site validation timing.** The schema compiles at the edge (reject-early) and again at run start (the compile the run consumes). A record could in principle pass the edge and fail at run start only through corruption or version skew — handled by the loud `output_invalid` MarkFailed, never a silent skip.

## Glossary additions

- **Task output schema** — the opt-in, per-request JSON Schema a Protocol caller supplies on the `start` request's `output_schema` field; persisted on the task record, compiled once at run start, steered/validated by the run-level structured-output mechanism (D-272), and delivered as the task envelope's `answer_payload`. Schema-invalid after the retry budget → the task fails with the `output_invalid` terminal code. Introduced in Phase 146 (D-276).

(Added to `docs/glossary.md` in the same PR as this plan.)

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes (the schema is identity-scoped task state; the conformance + E2E identity legs cover the seam)
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (The shared drivers + the promoted envelope builder are reusable surfaces — the mixed-traffic distinct-schema test is mandatory.)
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (It consumes 143 + 54 + 87 + 107e and closes the request→task→run→envelope→observation seam — the integration test is mandatory.)
- [x] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed from)
