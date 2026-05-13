# Phase 47 — Parallel-call executor + ReAct CallParallel / SpawnTask / AwaitTask emission

## Summary

Land Harbor's runtime parallel-call executor (the consumer of
`planner.CallParallel`) and upgrade the reference ReAct planner so it
EMITS the three new shapes the executor + task subsystem need
(`CallParallel`, `SpawnTask`, `AwaitTask`). This PR closes three
primitive-with-consumer gaps in one wave — the §13 forbidden practice
rule explicitly forbids shipping a primitive without its first
consumer in the same wave; Phase 42 shipped `SpawnTask` / `AwaitTask`
shapes without an emitter, Phase 45 shipped ReAct without those
emissions, and the master plan's Phase 47 row covers the parallel
executor itself. Phase 47 ships all three consumers in one wave.

## RFC anchor

- RFC §6.2

## Briefs informing this phase

- brief 02
- brief 07

## Brief findings incorporated

- **brief 02 §2 (`Decision` is a sum type; tool call, background task, and subagent are runtime-level concepts).** The Phase 47 ReAct emission upgrade adds two reserved tool names (`_spawn_task`, `_await_task`) whose prompt-time strings translate to typed `planner.SpawnTask{Kind, Spec}` / `planner.AwaitTask{TaskID}` Decisions BEFORE return — the sum stays sealed (no "magic string as next_node" anti-pattern leaks into the Decision shape). The reserved-name convention follows D-051's `_finish` discipline; the leading underscore is a documented convention, never a real tool.
- **brief 02 §2 (parallel-pause atomicity).** RFC §6.2 names the contract surface: "no branch starts side-effecting tools, or all reach checkpointed observation before pause commits." Phase 47 ships the contract-surface stub (`planner.ErrParallelPauseUnsupported`) — the executor fails loud on a mid-execution pause request. The unified pause/resume primitive lands at Phase 50; this phase reserves the seam so Phase 50 fills it rather than introducing a parallel surface.
- **brief 02 §6 (planner-conformance test harness).** The Phase 47 ReAct upgrade integrates the spawn → group resolve → planner re-entry round-trip the conformance pack (Phase 49) consumes. Phase 47's integration test exercises the round-trip end-to-end so Phase 49's harness inherits a working path.
- **brief 07 §8 (planner package imports no Runtime internals).** The Phase 42 import-graph lint test (`internal/planner/conformance/importgraph_test.go`) is the binding §13 surface. Phase 47 places the parallel executor at `internal/runtime/parallel/` (OUTSIDE the planner subtree) so the planner package never depends on a runtime executor; the runtime depends on the planner shape (one-way). The Phase 47 smoke script asserts both directions of the contract.

## Findings I'm departing from (if any)

- **Master plan Phase 47 detail block scope.** The master plan row only lists the runtime parallel executor as scope. Phase 47 EXPANDS scope to bundle three primitive-with-consumer gaps in one wave (CallParallel runtime + ReAct emission upgrade for SpawnTask + AwaitTask). The §13 forbidden practice "shipping a primitive without its first consumer in the same wave" is the binding rule that drives the bundling: SpawnTask + AwaitTask shipped in Phase 42 as decision shapes without an emitter; Phase 45 deferred ReAct emission. The Phase 47 PR closes both the parallel-execution gap AND the spawn/await emission gap by upgrading ReAct to emit all three shapes. Bundling them is intentional and chosen by the operator — splitting into sub-PRs would re-introduce the primitive-with-consumer split the §13 rule forbids.
- **Phase 45 explicit non-goal "No `CallParallel` emission" (D-051).** Phase 45's plan listed `reduceToSingleAction` as the unwind point Phase 47 deletes; this PR honours that hand-off. The deletion is the load-bearing §13 "two parallel implementations of the same conceptual feature" cleanup — Phase 44's repair loop already produces `CallParallel{Join: JoinAll}` when multi-action salvage triggers; the Phase 45 collapse override was a V1 stop-gap that Phase 47 retires.

## Goals

- Ship `internal/runtime/parallel.Executor` with atomic-setup validation, deterministic merge keys (branch index + tool name), and the three join shapes (JoinAll / JoinFirstSuccess / JoinN).
- Pin the `AbsoluteMaxParallel = 50` system cap as a settled constant on the planner package.
- Pin four typed sentinels covering the executor's failure surface (`ErrParallelCapExceeded`, `ErrParallelInvalidJoin`, `ErrParallelBranchInvalidArgs`, `ErrParallelPauseUnsupported`) plus a `JoinN` constant.
- Remove ReAct's Phase 45 D-051 single-tool-call-per-step stop-gap override (the `reduceToSingleAction` method); pass `CallParallel` through `mapDecision` unchanged.
- Add `_spawn_task` / `_await_task` reserved tool names + `mapDecision` translations producing typed `planner.SpawnTask` / `planner.AwaitTask` Decisions.
- Extend the ReAct system prompt to document the four reserved emission shapes (`_finish`, `_spawn_task`, `_await_task`, plus JSON-array fan-out).
- Wire the SpawnTask → `tasks.WatchGroup` → planner re-entry round-trip end-to-end in an integration test using the production TaskRegistry + EventBus + ArtifactStore drivers.
- Honour the §13 primitive-with-consumer rule for all three primitives in one wave.

## Non-goals

- No unified pause/resume primitive. Phase 50 ships the coordinator; Phase 47 reserves the contract surface (`ErrParallelPauseUnsupported`) so Phase 50 fills it rather than introducing a parallel pause path.
- No protocol surface. The parallel executor + emission upgrade are runtime-internal; Phase 60+ exposes the surface through the Protocol.
- No `JoinKeyed` implementation. The constant is reserved for a later runtime phase that ships keyed-merge semantics; Phase 47's executor fails loud on `JoinKeyed` via `ErrParallelInvalidJoin`.
- No retry-on-branch-failure policy. Per-branch retries belong inside the `tools.ToolPolicy` shell (Phase 26+); the parallel executor invokes each descriptor's `Invoke` once and surfaces success/failure unchanged.
- No tool-side caching of branch results. The executor is stateless; per-call state lives on the stack + ctx.
- No multi-action salvage extension beyond Phase 44's loop. The loop already produces `CallParallel{Join: JoinAll}`; Phase 47 passes it through and lets the executor dispatch.

## Acceptance criteria

- [ ] `internal/runtime/parallel/parallel.go` ships `Executor`, `Resolver`, `Result` types + `New(resolver)` constructor.
- [ ] `Executor.Execute(ctx, planner.CallParallel) ([]Result, error)` validates atomically (branch count cap, JoinSpec shape, descriptor resolution, args validation) BEFORE dispatching any branch.
- [ ] JoinAll waits for every branch; results returned in branch-index order.
- [ ] JoinFirstSuccess returns the first successful branch's Result and cancels the remainder via a derived ctx.
- [ ] JoinN returns N successful branches in completion order; setup validates `0 < N ≤ len(Branches)`.
- [ ] `planner.AbsoluteMaxParallel = 50` constant pinned in `internal/planner/errors.go`.
- [ ] Four typed sentinels (`ErrParallelCapExceeded`, `ErrParallelInvalidJoin`, `ErrParallelBranchInvalidArgs`, `ErrParallelPauseUnsupported`) pinned in `internal/planner/errors.go`.
- [ ] `JoinN` JoinKind constant pinned in `internal/planner/decision.go`.
- [ ] `JoinSpec.N` field shipped alongside `JoinSpec.Kind` / `JoinSpec.MergeKeys`.
- [ ] `internal/planner/react/react.go::reduceToSingleAction` is DELETED; the Phase 47 smoke asserts ABSENCE via grep -v.
- [ ] `internal/planner/react/react.go` adds `SpawnTaskToolName = "_spawn_task"` and `AwaitTaskToolName = "_await_task"` constants.
- [ ] `mapDecision` translates `_spawn_task` → `planner.SpawnTask{Kind, Spec, GroupID}` and `_await_task` → `planner.AwaitTask{TaskID}`; fail-loudly on malformed args (wrapped `planner.ErrInvalidDecision`).
- [ ] `mapDecision` passes `planner.CallParallel` through verbatim (Phase 47 pass-through); the reserved-name special-case for the FIRST branch is preserved.
- [ ] `DefaultSystemPrompt` documents the three reserved tool names + the JSON-array fan-out shape; `TestDefaultSystemPrompt_DocumentsAllThreeReservedNames` asserts the contract.
- [ ] `internal/runtime/parallel/parallel_test.go` covers JoinAll happy path; atomic-setup branch rejection; first-success cancellation; AbsoluteMaxParallel cap; missing-identity rejection; ToolNotFound atomic failure; JoinN happy path + invalid threshold; nil-resolver panic; empty-branches rejection; unknown JoinKind rejection; JoinKeyed not-implemented rejection; JoinAll surfaces per-branch failures via Result.Err; nil JoinSpec defaults to JoinAll.
- [ ] `internal/runtime/parallel/concurrent_test.go` ships the D-025 N=128 reuse stress (one shared Executor, per-goroutine identity, no bleed, no leak).
- [ ] `internal/planner/react/react_test.go` adds tests for SpawnTask / AwaitTask emission (happy paths + malformed-args fail-loudly paths).
- [ ] `test/integration/phase47_spawn_await_test.go` wires the real TaskRegistry + EventBus + ArtifactStore end-to-end: ReAct emits SpawnTask → runtime spawns the real task → group resolves → planner re-enters via `RunContext.Trajectory.Background` → planner emits Finish.
- [ ] `scripts/smoke/phase-47.sh` exists and asserts: per-package + integration tests under `-race`; reserved-name constants pinned; `reduceToSingleAction` absent; JoinKind constants pinned; AbsoluteMaxParallel = 50 pinned; sentinels pinned; §13 import-graph contract preserved; system-prompt mentions all three reserved tools.
- [ ] `docs/decisions.md` D-056 records: (a) reserved tool names as V1 emission surface; (b) `reduceToSingleAction` deletion timing; (c) `AbsoluteMaxParallel = 50` cap rationale; (d) `JoinSpec` enum semantics; (e) atomic-setup vs in-flight failure handling; (f) §13 primitive-with-consumer policy compliance.
- [ ] `docs/glossary.md` gains entries: `JoinSpec`, `JoinAll`, `JoinFirstSuccess`, `JoinN`, `ParallelExecutor`, `_spawn_task`, `_await_task`, `AbsoluteMaxParallel`.
- [ ] `docs/plans/README.md` Phase 47 row flips to `Shipped`.
- [ ] `README.md` Status table updated.
- [ ] `docs/plans/phase-45-react-planner.md` Non-goals section reflects Phase 47 closing the SpawnTask/AwaitTask/CallParallel deferrals.
- [ ] Coverage on `internal/runtime/parallel`: ≥ 85% (master-plan target).

## Files added or changed

- `internal/runtime/parallel/parallel.go` (new) — Executor + Resolver + Result + atomic-setup validation + JoinAll/JoinFirstSuccess/JoinN dispatch.
- `internal/runtime/parallel/parallel_test.go` (new) — happy paths + setup-validation + cancellation + cap + sentinels.
- `internal/runtime/parallel/concurrent_test.go` (new) — D-025 N=128 reuse stress.
- `internal/planner/decision.go` (modified) — `JoinN` constant + `JoinSpec.N` field.
- `internal/planner/errors.go` (modified) — `AbsoluteMaxParallel` const + four typed sentinels.
- `internal/planner/react/react.go` (modified) — `reduceToSingleAction` DELETED; `SpawnTaskToolName` / `AwaitTaskToolName` constants; `translateSpawnCall` + `translateAwaitCall`; `mapDecision` signature change to `(Decision, error)`; `Next` propagates the new error path; `DefaultSystemPrompt` rewritten.
- `internal/planner/react/react_test.go` (modified) — replaces the V1 reduction tests with pass-through tests; adds SpawnTask / AwaitTask emission tests; adds system-prompt assertion.
- `test/integration/phase47_spawn_await_test.go` (new) — end-to-end round-trip with real drivers.
- `scripts/smoke/phase-47.sh` (new) — Phase 47 smoke assertions.
- `docs/plans/phase-47-parallel-emission.md` (this file).
- `docs/plans/README.md` (modified — Phase 47 row flips to `Shipped`).
- `docs/plans/phase-45-react-planner.md` (modified — Non-goals reflects Phase 47 closure).
- `docs/decisions.md` (modified — D-056 record).
- `docs/glossary.md` (modified — eight new entries).
- `README.md` (modified — Status table).

## Public API surface

```go
package planner

const AbsoluteMaxParallel = 50

var (
    ErrParallelCapExceeded       = errors.New("planner: CallParallel branch count exceeds absolute_max_parallel")
    ErrParallelInvalidJoin       = errors.New("planner: CallParallel join spec invalid")
    ErrParallelBranchInvalidArgs = errors.New("planner: CallParallel branch failed atomic-setup validation")
    ErrParallelPauseUnsupported  = errors.New("planner: CallParallel pause-mid-execution not supported until Phase 50 unified pause primitive")
)

// JoinSpec extended.
type JoinSpec struct {
    Kind      JoinKind
    MergeKeys []string
    N         int // success threshold for JoinN
}

const JoinN JoinKind = "n"
```

```go
package parallel

type Resolver interface {
    Resolve(name string) (tools.ToolDescriptor, bool)
}

type Executor struct { /* ... */ }
func New(resolver Resolver) *Executor

type Result struct {
    Index  int
    Tool   string
    Result *tools.ToolResult
    Err    error
}

func (e *Executor) Execute(ctx context.Context, call planner.CallParallel) ([]Result, error)
```

```go
package react

const (
    SpawnTaskToolName = "_spawn_task"
    AwaitTaskToolName = "_await_task"
)
```

## Test plan

- **Unit (`internal/runtime/parallel`):**
  - `TestExecute_JoinAll_AllSucceed` — happy path, results in branch-index order.
  - `TestExecute_JoinAll_RejectsInvalidArgsBranchAtomically` — atomic-setup contract: invalid args fail the whole call before any branch executes.
  - `TestExecute_JoinFirstSuccess_CancelsRemainder` — first successful branch wins; slow branches observe cancellation.
  - `TestExecute_AbsoluteMaxParallelCap` — 51-branch input → `ErrParallelCapExceeded`.
  - `TestExecute_AbsoluteMaxParallelExactly50Allowed` — boundary at 50 inclusive.
  - `TestExecute_MissingIdentityFailsClosed` — §6 rule 9.
  - `TestExecute_ToolNotFoundFailsAtomically` — atomic-setup reject on missing tool.
  - `TestExecute_JoinN_WaitsForNSuccesses` — happy path.
  - `TestExecute_JoinN_InvalidThresholdFailsLoudly` — table: zero, negative, exceeds-branches.
  - `TestExecute_NilResolverPanics` — boot-time guard.
  - `TestExecute_EmptyBranchesFailsLoudly` — defensive against Phase 44 loop's empty edge.
  - `TestExecute_JoinKindUnknownFailsLoudly` + `TestExecute_JoinKeyedNotImplementedFailsLoudly`.
  - `TestExecute_JoinAllSurfacesPerBranchFailures` — per-branch failures on Result.Err; call-level err nil.
  - `TestExecute_NilJoinDefaultsToJoinAll`.
- **Unit (`internal/planner/react`):**
  - `TestNext_ParallelPassesThroughVerbatim` — CallParallel from repair loop passes unchanged.
  - `TestNext_ParallelWithFinishFirstStillFinishes` — symmetry preserved.
  - `TestNext_SpawnTaskEmissionMappedToSpawnTaskDecision` — happy path with Kind / Spec / GroupID.
  - `TestNext_SpawnTaskDefaultsKindToBackground`.
  - `TestNext_SpawnTaskMalformedArgsFailsLoudly` — wrapped `ErrInvalidDecision`.
  - `TestNext_SpawnTaskInvalidKindFailsLoudly`.
  - `TestNext_AwaitTaskEmissionMappedToAwaitTaskDecision`.
  - `TestNext_AwaitTaskEmptyIDFailsLoudly`.
  - `TestNext_AwaitTaskMalformedJSONFailsLoudly`.
  - `TestDefaultSystemPrompt_DocumentsAllThreeReservedNames` — drift gate on the prompt schema.
- **Integration (`test/integration/phase47_spawn_await_test.go`):**
  - `TestE2E_Phase47_ReactSpawnTaskWakeRoundTrip` — emit `_spawn_task` → real `tasks.Spawn` → seal → `WatchGroup` → MarkComplete → GroupCompletion arrives → surface MemberOutcome through `RunContext.Trajectory.Background` → re-invoke Next → planner emits Finish.
  - `TestE2E_Phase47_ReactAwaitTaskEmits` — emit `_await_task` produces typed `planner.AwaitTask` Decision.
  - `TestE2E_Phase47_ReactCallParallelEndToEnd` — multi-action salvage → CallParallel pass-through → real `tools.NewCatalog` → `parallel.Execute` → three results in branch-index order.
  - `TestE2E_Phase47_ParallelExecutorAtomicSetup` — real catalog, picky-validator branch fails the whole call BEFORE any branch runs.
  - `TestE2E_Phase47_ParallelExecutorAbsoluteMaxParallel` — real catalog, 51-branch input fails with `ErrParallelCapExceeded`.
- **Concurrency / leak:** `TestConcurrent_ExecutorIsReusableAcrossNCalls` — N=128 against one shared `*parallel.Executor` under `-race`; per-goroutine identity quadruple round-trips without bleed; pre-cancelled ctx on i%17==0 returns ctx.Err() without affecting siblings; goroutine baseline restored within 2s of WaitGroup join.
- **Conformance:** N/A — Phase 49 owns the conformance pack; Phase 47's emission paths land in scope when Phase 49 fills the scenarios.

## Smoke script additions

`scripts/smoke/phase-47.sh` asserts:

- Per-package tests pass under `-race` (planner/react + runtime/parallel).
- Integration tests `TestE2E_Phase47_*` pass under `-race`.
- Reserved tool name constants pinned (`_finish`, `_spawn_task`, `_await_task`).
- `reduceToSingleAction` ABSENT from `internal/planner/react/` (the §13 two-parallel-implementations cleanup; smoke is the drift gate).
- JoinKind constants pinned (`JoinAll`, `JoinFirstSuccess`, `JoinN`).
- `AbsoluteMaxParallel = 50` pinned.
- Four typed sentinels pinned.
- §13 import-graph contract: `internal/planner/react/` does NOT import `internal/runtime/...`; `internal/runtime/parallel/` DOES import `internal/planner` (forward consumer).
- `DefaultSystemPrompt` mentions all three reserved tools.

## Coverage target

- `internal/runtime/parallel`: 85% (master-plan target).
- `internal/planner/react`: unchanged (already at ≥ 85% from Phase 45; the Phase 47 new code lifts touched-line coverage in the same band).

## Dependencies

- 45 (Reference ReAct planner — the upgrade target).
- 14 (concurrency utils — `MapConcurrent` / `JoinK` are the conceptual ancestors; Phase 47 reuses the pattern but ships its own typed executor that consumes `planner.CallParallel` directly).
- 42 (Planner iface — `CallParallel`, `SpawnTask`, `AwaitTask`, `JoinSpec`, `SpawnSpec` shapes).
- 20 + 21 (TaskRegistry + WatchGroup — the runtime side of the spawn → resolve → wake round-trip).

## Risks / open questions

- **Parallel-pause atomicity contract is stub-only at Phase 47.** The unified pause/resume primitive lands at Phase 50; until then the executor fails loud on a mid-execution pause request via `ErrParallelPauseUnsupported`. **Mitigation:** the sentinel surfaces the deferral, and the Phase 50 plan explicitly takes ownership of upgrading this path to a checkpointed atomic pause.
- **JoinKeyed deferral.** The constant exists since Phase 42; Phase 47 fails loud on it via `ErrParallelInvalidJoin` with a "not implemented at Phase 47" message. A future runtime phase ships keyed-merge semantics. **Mitigation:** the error message names the deferral so operators don't think it's a bug.
- **No retry on per-branch failure.** Branches with `tools.ToolPolicy.MaxRetries > 0` will retry inside their own dispatch shell when the runtime engine wires the policy shell at the executor's Invoke seam. The Phase 47 executor invokes `desc.Invoke` once per branch directly; the policy shell composition seam is a follow-up. **Mitigation:** the public surface is `desc.Invoke` (function value), so wrapping it with a policy shell at a later phase is a pure composition exercise — no executor-internal change required.
- **System cap `AbsoluteMaxParallel = 50` is settled but not operator-tunable.** RFC §6.2 lists it as a system cap; tuning would require an RFC PR. **Mitigation:** documented in D-056. Operators tune via `PlanningHints.MaxParallel` (the soft cap); the system cap is defence in depth.

## Glossary additions

- `JoinSpec` — the parallel-merge descriptor on `planner.CallParallel`. Carries `Kind` (the join strategy) + `MergeKeys` (reserved for keyed merges) + `N` (the success threshold for `JoinN`).
- `JoinAll` — the default join strategy: wait for every branch to terminate, return results in branch-index order.
- `JoinFirstSuccess` — return the first successful branch's result; cancel the remainder via a derived ctx. Failures do not cancel until every branch terminates.
- `JoinN` — wait for N branches to succeed, then cancel the remainder. Setup validates `0 < N ≤ len(Branches)`.
- `ParallelExecutor` — the `internal/runtime/parallel.Executor` runtime component that consumes `planner.CallParallel`. Atomic-setup validation; deterministic per-branch merge keys; identity-mandatory at the boundary.
- `_spawn_task` — the reserved ReAct prompt-schema tool name that translates to a `planner.SpawnTask` Decision. D-056 — Phase 47 introduces.
- `_await_task` — the reserved ReAct prompt-schema tool name that translates to a `planner.AwaitTask` Decision. D-056 — Phase 47 introduces.
- `AbsoluteMaxParallel` — the system cap on `planner.CallParallel.Branches` length. Value: 50 (RFC §6.2, settled). The runtime parallel executor rejects above-cap emissions with `planner.ErrParallelCapExceeded` before dispatch.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A — Phase 47 inherits the per-call identity assertion from the parallel executor's `Execute` entry + the integration test's per-tenant ctx wiring; no cross-tenant primitives changed.
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. The `parallel.Executor` IS a reusable artifact; `concurrent_test.go` ships the N=128 test.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. The Phase 47 PR consumes Phase 20/21's TaskRegistry surface + the Phase 26 ToolCatalog descriptor surface. `test/integration/phase47_spawn_await_test.go` wires both ends with real drivers and asserts the spawn → wake → re-entry round-trip plus the atomic-setup contract.
- [ ] If new vocabulary: glossary updated — yes, eight new terms.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-056.
