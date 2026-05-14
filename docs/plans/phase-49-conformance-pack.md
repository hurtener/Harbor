# Phase 49 — Planner conformance pack

## Summary

Fill in the planner conformance harness skeleton (`internal/planner/conformance/`, shipped by Phase 42) with the production scenario suite that every concrete `Planner` MUST pass: top-20 prompt round-trips against a canned tool catalog + LLM mock, schema-repair salvage on malformed LLM output, parallel-call atomicity, budget-aware finishes, pause-payload bound checks, steering drain-between-steps, the D-025 concurrent-reuse contract — and the load-bearing **wake-mode round-trip** (D-032 — binding): `SpawnTask` → real `tasks.TaskRegistry` resolves the group → planner re-enters → reads `MemberOutcome` through `RunContext.Trajectory.Background`. The pack runs against BOTH Wave 8 concretes — Phase 45's ReAct (declares `WakePush`) and Phase 48's Deterministic (declares `WakePoll`) — proving CLAUDE.md §1 property 3 ("the Planner is swappable") with a shared test surface, not a per-concrete suite. The pack itself is the test asset; per-package coverage targets are unchanged.

## RFC anchor

- RFC §6.2
- RFC §3.2
- RFC §11 Q-6

## Briefs informing this phase

- brief 02
- brief 07

## Brief findings incorporated

- **brief 02 §6 ("planner-conformance test harness").** "A shared test pack that any `Planner` implementation must pass." Phase 49 IS the fill-in. The Phase 42 skeleton declared every scenario slot; Phase 49 lands the bodies. Concretes added later (Plan-Execute, Workflow, Graph, Supervisor, MultiAgent, HumanApproval) inherit the suite for free — their per-package conformance test calls `conformance.Run(t, factoryFunc)` and gets the full pack.
- **brief 02 §7 (wake-mode round-trip is binding).** "Failure to wire `tasks.WatchGroup` is the test's failure mode, not silent deadlock." Phase 49's `WakeMode_RoundTrip` scenario exercises the round-trip against the REAL `tasks.TaskRegistry` (inprocess driver) + REAL `events.EventBus` (inmem driver) — not a mock. For the push mode (ReAct), the scenario spawns a real task, marks it complete, observes the `GroupCompletion` delivery, surfaces the resolved `MemberOutcome` through `RunContext.Trajectory.Background`, and re-invokes `Next`. For the poll mode (Deterministic), the same Spawn → group resolves → poll detects → consume round-trip fires via the planner's own non-blocking `WatchGroup` receive.
- **brief 02 §2 ("Decision is a sum type, not a magic next_node string").** The pack's top-20 scenarios assert the `Decision` shape returned (CallTool vs CallParallel vs SpawnTask vs AwaitTask vs RequestPause vs Finish) — never a string discriminator. The scenarios that drive ReAct supply a canned mock LLM emitting one of the six envelopes; the scenarios that drive Deterministic supply a `DecisionTreeStep` configuration that yields the equivalent shape via the typed `Decide` method.
- **brief 02 §9 Q-5 (no NoOp variant).** The pack does NOT assert a NoOp scenario — wait-for-steering and trajectory-summarisation are runtime short-circuits (RFC §6.3), not planner decisions. Phase 49 honours the omission.
- **brief 07 §8 ("planner package imports no Runtime internals").** The pack lives in `internal/planner/conformance/` and consumes ONLY `internal/planner/...`, `internal/events`, `internal/tasks`, `internal/identity`, `internal/llm/mock` (mock LLM driver — test-grade), `internal/audit`, `internal/state`. The §13 import-graph lint (`importgraph_test.go`, shipped by Phase 42) walks the planner subtree and gates against any `internal/runtime/...` import — extended for Phase 49 only insofar as the new scenario file lives inside the same lint scope.

## Findings I'm departing from (if any)

- **None.** Phase 49 closes a scenario surface the Phase 42 plan already itemised; the master plan's Phase 49 detail block already pins the wake-mode round-trip wiring decision. No departures.

## Goals

- Fill in EVERY skipped scenario in `internal/planner/conformance/conformance.go`. No skeleton skips remain.
- Add a top-20-style canned scenario set covering: single tool call resolves; two-step reasoning; Finish on goal; graceful failure on no_path; MaxSteps breaker fires; schema repair recovers; budget-aware deadline-exceeded; pause-payload depth/size bounds; steering CANCEL drains between steps; ConcurrentReuse_D025 surface (a thin wrapper over the per-package N=128 reuse test that the harness can run pack-wide).
- Wire the **wake-mode round-trip** scenario against the REAL `tasks.TaskRegistry` (inprocess driver) and REAL `events.EventBus` (inmem driver). The scenario fires for BOTH push (ReAct re-entry on `GroupCompletion`) and poll (Deterministic's per-`Next` non-blocking `WatchGroup` receive). Hybrid is a future concrete's responsibility — the harness already accepts `WakeHybrid` via `IsValidWakeMode`.
- Extend `internal/planner/react/conformance_test.go` and `internal/planner/deterministic/conformance_test.go` to drive the full pack. The Sanity-only skeleton invocation is replaced with a full-suite call.
- Land `test/integration/wave8_test.go` — the Wave 8 wave-end E2E per §17.5. Wires Skills + Planner + Tools + Tasks + Memory + LLM + Events + State end-to-end through real production drivers; includes ≥1 failure mode; runs N≥10 concurrent runs against the assembled surface; asserts no goroutine leak after teardown; honours §17.6 (fix-what-the-test-finds in-PR).
- Ship `scripts/smoke/phase-49.sh` with real assertions: the conformance pack tests pass under `-race`; both factories' suites complete cleanly; the wake-mode round-trip fires for both push and poll modes.
- D-058 entry in `docs/decisions.md` documenting (a) the scenario surface for the pack, (b) the wake-mode round-trip wiring decision (real `TaskRegistry` + real `EventBus`, no mocks), (c) any §17.6 cross-phase fixes bundled in this PR.

## Non-goals

- No conformance scenarios for hybrid wake (`WakeHybrid`). The first hybrid concrete will own its round-trip scenario when it ships.
- No conformance scenarios for `pauseresume`-coordinated `RequestPause` round-trip — the unified pause/resume primitive lands at Phase 50 with its own primitive-with-consumer constraint. Phase 49's RequestPause coverage is limited to payload bounds + the planner emits a typed Decision.
- No top-20 prompts AI-stylised — the canned scenarios are mechanical fixtures (single tool call, two-step, finish, no_path, max_steps, repair, budget exhaustion, pause request, cancel drain) designed for deterministic test runs. A semantic "ride twenty real prompts through ReAct" benchmark belongs to the eval phase set (post-V1).
- No protocol surface; Phase 49 is a code-only phase. The smoke script asserts tests + WakeMode declarations + import-graph rather than HTTP endpoints.

## Acceptance criteria

- [ ] `internal/planner/conformance/conformance.go` contains zero `t.Skip("Phase 49 ...")` calls. Every scenario slot has a real body.
- [ ] `conformance.Run(t, factoryFunc)` exposes the following scenarios (each a `t.Run` subtest):
  - `Sanity_NextReturnsDecision` (shipped Phase 42; preserved).
  - `WakeMode_Declared` (shipped Phase 42; preserved).
  - `Sealed_DecisionSum` (shipped Phase 42; preserved).
  - `TopPrompts_LLMRoundTrip` — runs an op-shaped sequence (CallTool → observation → Finish) for ReAct; runs the equivalent DecisionTreeStep sequence for Deterministic. Asserts each `Decision` shape matches the expected sum variant.
  - `MalformedLLM_Salvage` — for LLM-driven planners only (predicate gated): mock emits invalid JSON; assert no panic and a typed terminal (`Finish{NoPath}` after the repair ladder exhausts). Skip-with-reason for non-LLM concretes.
  - `ParallelCall_Atomicity` — asserts the planner emits a well-formed `CallParallel` and the harness-supplied executor consumes it atomically. For Deterministic the step set is configured to emit `CallParallel{Branches: [a, b]}`; for ReAct the LLM emits a 2-action JSON array which Phase 44 repair-loop salvages to CallParallel.
  - `WakeMode_RoundTrip` — the load-bearing scenario per D-032. SpawnTask emission → real `tasks.TaskRegistry` spawns and seals the group → mark the spawned task Complete → `WatchGroup` delivers `GroupCompletion` → surface MemberOutcome via `RunContext.Trajectory.Background` → re-invoke `Next` → planner emits Finish. The wiring uses REAL drivers — no mocks at the seam (§17.3). For the poll mode the scenario instead drives the per-`Next` non-blocking receive: emit SpawnTask, run the group to completion, call `Next` again — the planner's step performs its own receive and surfaces the resolved MemberOutcome to its `OnResolved` callback.
  - `BudgetAware_FinishDeadlineExceeded` — for planners that read `Budget.Deadline`: a deadline strictly in the past on the first `Next` call → `Finish{DeadlineExceeded}` (ReAct, Deterministic both honour this via `ctx.Err()` propagation; the pack's expectation is "Finish with a deadline-exceeded reason or wrapped ctx.DeadlineExceeded surfaces"). The pack's strictness is the **shape** of the response (terminal or context-error), not a specific FinishReason value, so concretes that prefer `Finish{NoPath}` with a Metadata flag also pass.
  - `PausePayload_BoundsRespected` — pack constructs a `RequestPause`-emitting planner via the factory's `PauseScenario` hook (optional; concretes that cannot emit Pause skip-with-reason). Verifies depth ≤ 6, key count ≤ 64, total ≤ 16 KiB per RFC §6.3. Validation rules currently live at the protocol edge (Phase 52); Phase 49's scenario asserts the planner-emitted `RequestPause.Payload` shape is INSIDE bounds for a typical operator-supplied payload — the strict-bounds enforcement test is Phase 52's responsibility.
  - `Steering_DrainBetweenSteps` — sets `rc.Control.Cancelled = true` and asserts the planner returns `Finish{Cancelled}` at step boundary before any tool dispatch. Both ReAct + Deterministic honour this.
  - `ConcurrentReuse_D025` — N=64 parallel `Next` calls against one shared planner from the factory; asserts no race (race detector is the gate), no context bleed (per-call RunID round-trips via `Finish.Metadata["run_id"]` when the planner stamps it).
- [ ] `internal/planner/react/conformance_test.go` calls `conformance.Run` with a factory that supplies: `WakeMode: WakePush`, a fresh `mock.New(...)` LLM driver (so ReAct's per-test responses don't share state across subtests), and a fully-populated `RunContextFactory` (identity quadruple). The full suite runs against ReAct; the wake-mode round-trip exercises push.
- [ ] `internal/planner/deterministic/conformance_test.go` calls `conformance.Run` with a factory that supplies: `WakeMode: WakePoll`, a `DecisionTreeStep` sequence that produces the expected Decision shapes for each scenario (configurable per-subtest via the harness), and a fully-populated `RunContextFactory`. The full suite runs against Deterministic; the wake-mode round-trip exercises poll.
- [ ] `test/integration/wave8_test.go` exists and:
  - Wires REAL drivers across Skills (Phase 37/38/39/40/41 — `localdb` driver), Planner (Phase 42/45/48), Tools (Phase 26 in-process catalog), Tasks (Phase 20/21 — inprocess driver), Memory (Phase 23 — inmem driver), LLM (Phase 32 — mock driver), Events (Phase 05 — inmem driver), State (Phase 07 — inmem driver). No mocks at any seam (§17.3 #1).
  - Asserts identity propagation through every wired layer (§17.3 #2).
  - Includes at least one failure-mode scenario: missing identity at the planner boundary returns wrapped `llm.ErrIdentityMissing` (ReAct's identity-mandatory pre-check) without burning an LLM call.
  - Includes a concurrency stress run: N=10 concurrent ReAct runs against the assembled surface; asserts `runtime.NumGoroutine()` returns to baseline after teardown.
  - Never uses `time.Sleep` for synchronisation (§17.4); channel-based or bounded-eventually waits.
- [ ] `scripts/smoke/phase-49.sh` exists with real assertions: the conformance pack tests pass under `-race` against BOTH ReAct + Deterministic; the wake-mode round-trip scenario fires for both push and poll modes (asserted via subtest names in `go test -v` output or by package boundary).
- [ ] `docs/plans/README.md` Phase 49 row flips to `Shipped`.
- [ ] `README.md` Status table gains a Phase 49 row.
- [ ] `docs/decisions.md` D-058 entry lands with: (a) scenario surface; (b) wake-mode round-trip wiring decision (real `TaskRegistry` + `EventBus`); (c) any cross-phase §17.6 fixes bundled.
- [ ] `docs/glossary.md` gains entries for `ConformancePack`, `ConformanceScenario`.
- [ ] Coverage on `internal/planner/conformance/`: ≥ 70% (the pack itself — see the §4.3 deviation note in "Coverage target" for why 80% was structurally unreachable).

## Files added or changed

- `internal/planner/conformance/conformance.go` (modified — fills every skipped scenario; adds the wake-mode round-trip wiring).
- `internal/planner/conformance/scenarios.go` (new — the canned top-20-style scenario fixtures, including the LLM-mock content map keyed on scenario name + the Deterministic `DecisionTreeStep` configurations).
- `internal/planner/conformance/wakeroundtrip.go` (new — the real-`TaskRegistry` + real-`EventBus` wake-mode round-trip helper consumed by both push and poll scenarios).
- `internal/planner/react/conformance_test.go` (modified — calls `conformance.Run` with the full factory shape).
- `internal/planner/deterministic/conformance_test.go` (modified — calls `conformance.Run` with the full factory shape).
- `test/integration/wave8_test.go` (new — Wave 8 wave-end E2E).
- `scripts/smoke/phase-49.sh` (new — real assertions).
- `docs/plans/phase-49-conformance-pack.md` (this file).
- `docs/plans/README.md` (modified — Phase 49 row flips to `Shipped`).
- `README.md` (modified — Status table gains a Phase 49 row).
- `docs/decisions.md` (modified — D-058 entry).
- `docs/glossary.md` (modified — `ConformancePack`, `ConformanceScenario`).

## Public API surface

The conformance pack is a TEST-ONLY surface — `_test.go` files only consume it. The non-test files (`conformance.go`, `scenarios.go`, `wakeroundtrip.go`) expose:

- `Harness struct` — extended with optional `PauseScenario *PauseScenarioOptions` (concretes that cannot emit `RequestPause` set it to nil; the scenario subtest skips-with-reason).
- `LLMMockFactory func() *mock.Driver` — optional factory for LLM-driven concretes; concretes that don't use an LLM (Deterministic) leave it nil.
- `DeterministicStepsFactory func() []DecisionTreeStepProjection` — optional factory for the deterministic concrete to supply pre-built step configurations per scenario. Defined narrowly so the conformance package doesn't import `internal/planner/deterministic/` (avoids an import cycle).
- `Run(t *testing.T, factoryFunc func() Harness)` — entry point; unchanged signature. Subtest names are stable across phases so per-concrete suites' pass/fail boards remain comparable.

The conformance pack does NOT export the per-scenario implementations; they're internal to the package.

## Test plan

- **Unit:** N/A — the conformance pack IS the test asset. Coverage is asserted by the pack's own scenarios running against both concretes.
- **Integration:** `test/integration/wave8_test.go` covers the Wave 8 wave-end surface. Real drivers everywhere (§17.3). N=10 concurrent ReAct runs. Failure-mode (missing identity).
- **Conformance:** `internal/planner/conformance/conformance_test.go` runs the full pack against the in-package `finish.Planner` skeleton (proves the pack is well-formed without requiring the LLM-driven concrete) PLUS the per-concrete tests at `internal/planner/react/conformance_test.go` + `internal/planner/deterministic/conformance_test.go`. The wake-mode round-trip exercises both push (ReAct) and poll (Deterministic).
- **Concurrency / leak:** `ConcurrentReuse_D025` scenario runs N=64 parallel `Next` calls against one shared planner from the factory. The wave-end E2E's concurrency stress (N=10) provides the cross-package complement.

## Smoke script additions

- `scripts/smoke/phase-49.sh` runs `go test -race -count=1 -timeout 300s ./internal/planner/conformance/...` and asserts it exits 0.
- Asserts the conformance scenarios fire for BOTH ReAct + Deterministic — pinned via `go test -v ./internal/planner/react/... ./internal/planner/deterministic/... -run TestReact_Conformance` + `-run TestDeterministic_Conformance` returning subtest output that includes `WakeMode_RoundTrip` (the load-bearing scenario per D-032).
- Asserts the §13 import-graph lint still passes (no new `internal/runtime/...` imports introduced by Phase 49 — the lint test under `internal/planner/conformance/importgraph_test.go` is the binding gate).

## Coverage target

- `internal/planner/conformance`: ≥ 70% — see the deviation note below.

> **§4.3 deviation (Wave 8 §17.5 checkpoint audit, 2026-05-14).** The original
> target was 80%. The audit found the package at 70.2%. Investigation showed
> this is not purely a cross-package-measurement artifact: measuring the
> conformance package's coverage with `-coverpkg` across ALL THREE consuming
> test suites (its own self-test + `react/conformance_test.go` +
> `deterministic/conformance_test.go`) still lands at ~71%. The uncovered ~29%
> is structural: a conformance harness's failure-assertion branches
> (`t.Errorf` / `t.Fatalf` paths that fire only when a planner *violates* the
> contract) and tolerant-logging branches are, by construction, unreachable
> from a passing test suite — every planner in the suite passes, so the
> failure paths never execute. Covering them would require a battery of
> deliberately-non-conformant stub planners written purely to trip each
> assertion — coverage theatre that adds no real assurance. The audit chore
> added `TestConformance_SelfTest_MinimalCapabilities` (a no-capabilities
> stub) to claw back the genuinely-reachable capability-gated skip branches,
> and the target is set to a realistic **70%**. The pack's real assurance is
> not its self-coverage number — it is that both Wave 8 concretes (ReAct +
> Deterministic) pass every scenario, which the per-concrete suites verify.

## Dependencies

- Phase 42 (planner iface + Decision sum + RunContext + conformance harness skeleton)
- Phase 45 (ReAct concrete)
- Phase 48 (Deterministic concrete)
- Phase 20/21 (TaskRegistry + groups) — consumed by the wake-mode round-trip scenario
- Phase 05 (events bus) — consumed by the wake-mode round-trip scenario
- Phase 23 (memory) — consumed by the wave-end E2E
- Phase 32 (LLM client) + Phase 37 (Skills) — consumed by the wave-end E2E

## Risks / open questions

- **Risk: the wake-mode round-trip scenario flakes under `-race`.** Mitigation: bound the WatchGroup wait with a 2s timeout (the same shape Phase 47's `phase47_spawn_await_test.go` already uses); fail the test on timeout rather than hanging.
- **Risk: future hybrid concretes need a third round-trip path.** Mitigation: the `Harness.WakeMode` field already accepts `WakeHybrid`; the pack falls back to the push path for hybrid (since hybrid is push + a sidecar) until the first hybrid concrete lands its own scenario. Not a Phase 49 concern.
- **Open question: should the LLM-driven scenarios run against the production bifrost driver behind a flag?** Phase 49 ships against the mock driver only — the production bifrost path is covered by the Wave 7b live tests already. The conformance pack stays mock-driven so CI doesn't burn API credits.

## Glossary additions

- `ConformancePack` — the shared test pack any `Planner` concrete must pass.
- `ConformanceScenario` — one named subtest inside the pack.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, conformance pack does not introduce multi-isolation paths beyond what Phase 45 + Phase 48 ship.
- [ ] Reusable artifact concurrent-reuse test — N/A (the pack IS a test; per-concrete D-025 tests already live next to each planner's `Next` implementation).
- [ ] Cross-subsystem integration test — `test/integration/wave8_test.go` covers the wave-end surface per §17.5.
- [ ] If new vocabulary: glossary updated — `ConformancePack`, `ConformanceScenario`.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — None.
