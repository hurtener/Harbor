# Phase 110a — Tool-executor promotion (`internal/runtime/dispatch`)

## Summary

The only production `steering.ToolExecutor` in the codebase lives in `package main`:
`cmd/harbor/cmd_dev_executor.go` (~660 lines — catalog dispatch, D-026 heavy-result
artifact promotion via `projectForLLM` / `heavyTruncationSummary` / `buildPreview`,
`CallParallel` via `internal/runtime/parallel`, `SpawnTask`/`AwaitTask` driving with
spawn-depth caps). The 2026-06-09 SDK friction audit (`docs/notes/sdk-friction-audit.md`,
§2 P1/P3/P5) pinned the consequences: the D-094 devstack mirror ships a **degraded
CallTool-only executor** that skips D-026 promotion by its own admission
(`harbortest/devstack/devstack.go:2103-2143`), and
`internal/planner/react/prompt.go:1163-1164` cites `cmd_dev_executor.go::heavyTruncationSummary`
as its shape source — an `internal/` package documenting `package main` as its contract.
Phase 110a promotes the executor to a new package `internal/runtime/dispatch` with an
exported constructor, exports the answer-envelope + terminal task-error-code bridge
contract, re-homes the catalog→planner view adapter, and converts `cmd/harbor` AND
`harbortest/devstack` to thin callers — **deleting** the devstack degraded executor and
closing its documented D-026 / parallel / spawn capability drift in the same PR (§13
primitive-with-consumer; §17.6 fix-both-sides). Part of the Wave B re-homing program
(D-193, the program entry that lands with this band's PR); this phase's decision is
**D-194** (logged in `docs/decisions.md` with the shipping PR).

## RFC anchor

- RFC §6.4 — code-level tool dispatch (the runtime, not the LLM, executes tool calls;
  the executor is the runtime's dispatch concrete).
- RFC §6.5 — context-window safety net (the D-026 heavy-result→`ArtifactStub` promotion
  the executor performs before an observation reaches the planner/LLM edge).
- RFC §6.2 — Planner interface + Decision sum (the `CallTool` / `CallParallel` /
  `SpawnTask` / `AwaitTask` shapes the executor dispatches; the `ToolCatalogView` the
  planner reads).

## Briefs informing this phase

- brief 02 — planner + steering + HITL (the planner→runtime execution contract the
  executor implements; the concurrent-reuse posture).
- brief 03 — tools + integrations + LLM client (the two-parallel-modes anti-pattern the
  devstack degraded executor instantiates).

## Brief findings incorporated

- **brief 02 §3 "Planner → runtime".** "A planner returns a `Decision` and the runtime
  executes it. The planner does not call `tool.execute`, does not `spawn` tasks itself."
  The executor IS that runtime half of the contract — production semantics, not CLI
  plumbing. Homing it in `package main` makes the runtime's own execution contract
  unreachable to any consumer that isn't the dev binary; `internal/runtime/dispatch` is
  the honest home.
- **brief 02 §5 item 6 "Thread-safety disclaimers".** "Harbor's interface requires
  planners to be safe to use concurrently across runs." The same posture binds the
  executor: it is a compiled artifact constructed once and shared across every run
  (D-025). The promotion preserves the current immutable-after-construction shape and
  ships the N≥100 concurrent-reuse test the contract mandates.
- **brief 03 §5 "Two parallel LLM modes (the toggle smell)".** "Two modes shipping in
  parallel because one path didn't cover the quirks well enough — Harbor picks one
  architecture and bakes the correction in." The production executor vs. the devstack
  CallTool-only executor is exactly this smell, one layer up: two implementations of the
  same conceptual feature with diverging semantics (an external consumer building on
  devstack hits `ErrContextLeak` on the first >32KB tool result — the *opposite* failure
  of production). This phase deletes the second implementation rather than patching it.

## Findings I'm departing from (if any)

None.

## Goals

- **`internal/runtime/dispatch` package** — the promoted executor:
  - `dispatch.NewToolExecutor(cat tools.ToolCatalog, artifacts artifacts.ArtifactStore, taskReg tasks.TaskRegistry, opts ...Option) steering.ToolExecutor`
    — the constructor mirrors today's `newDevToolExecutor` (`cmd_dev_executor.go:106`);
    `heavyThreshold` and `maxSpawnDepth` move to functional options
    (`WithHeavyThreshold(int)`, `WithMaxSpawnDepth(int)`, `WithLogger(*slog.Logger)`)
    with the same defaults (32 KiB floor, depth 4).
  - Behaviour moves verbatim: `CallTool` dispatch + D-026 `projectForLLM` promotion
    (`cmd_dev_executor.go:457-517`), `CallParallel` via `parallel.Executor` in
    non-atomic mode (`:224-415`, D-169), `SpawnTask`/`AwaitTask` registry driving with
    the depth cap + poll cadence (D-170), and the `taskOutcomeObservation` envelope
    parse (`:417-443`).
- **Answer envelope + terminal error codes exported** (the cmd↔cmd implicit wire
  contract the audit's P3 named): the `{answer, finish_reason, tool_calls_seen}` shape
  the run-loop driver marshals into `tasks.TaskResult.Value`
  (`cmd_dev_runloop.go:777-789`) and the `Finish.Reason`→`TaskError.Code` mapping
  (`cmd_dev_runloop.go:717-817`) become exported types/constants. **Home:
  `internal/planner`** — justified by import direction: the envelope is the projection
  of `planner.Finish` (it carries `FinishReason` verbatim), `internal/planner` already
  exports `Finish`/`FinishReason`, and both producers (run-loop drivers) and consumers
  (`dispatch`, Protocol projectors) already import `internal/planner`. Homing it in
  `internal/tasks` would force a new `tasks`→`planner` import edge (tasks is
  planner-free today) or stringly-typed duplication. Shapes:
  - `planner.AnswerEnvelope{Answer string; FinishReason string; ToolCallsSeen int}` with
    the existing snake_case JSON tags (byte-compatible with what Phase 106 ships —
    pinned by a golden test).
  - `planner.TaskErrorCodeRunLoopError` / `planner.TaskErrorCodeCancelled` constants +
    `planner.TaskErrorCodeForFinish(reason FinishReason) string` (non-goal Finish
    reasons map to their string verbatim, exactly today's behaviour).
- **Catalog→planner view re-homed**: `cmd/harbor/cmd_dev_catalog_view.go:31-70`'s
  adapter becomes `tools.NewPlannerView(cat ToolCatalog, filter CatalogFilter) PlannerView`
  in `internal/tools`. Because `internal/planner` imports `internal/tools`
  (`planner.go:53` — `ToolCatalogView`'s methods return `tools.Tool`), `internal/tools`
  cannot name `planner.ToolCatalogView`; the exported concrete `tools.PlannerView`
  satisfies the interface **structurally** (`Resolve(name) (Tool, bool)` +
  `List() []Tool`), with the compile-time assertion
  `var _ planner.ToolCatalogView = tools.PlannerView{}` living in `internal/planner`'s
  tests (where the import is legal). The per-run, never-cached construction discipline
  and its cross-tenant warning move from the package-main comment into the exported
  godoc.
- **cmd/harbor + devstack become thin callers** (the §13 consumer, same phase):
  `cmd_dev_executor.go` and `cmd_dev_catalog_view.go` are deleted; the boot wiring calls
  `dispatch.NewToolExecutor` + `tools.NewPlannerView`. The devstack degraded executor
  (`devstack.go:2103-2143`) is **deleted** — devstack wires the same
  `dispatch.NewToolExecutor` over its catalog/artifact-store/task-registry, gaining
  D-026 promotion, `CallParallel`, and `SpawnTask`/`AwaitTask` parity in one stroke.
- **React prompt shape-contract re-pointed**: `internal/planner/react/prompt.go`'s
  comment citing `cmd/harbor/cmd_dev_executor.go::heavyTruncationSummary` as its source
  of truth (`prompt.go:1163-1164`) now cites
  `internal/runtime/dispatch` — and the truncation-summary map shape becomes an exported
  identifier the prompt renderer can reference by name, so the dependency arrow points
  the right way (the audit's "savoring" finding).
- **The D-192 E2E shim resolved**: the in-flight HITL-deadlock fix's E2E ships with a
  test-local executor shim and a §17.6 test-gap comment naming the missing production
  executor. This phase switches that E2E to the real promoted `dispatch.NewToolExecutor`
  and deletes the shim + comment.

## Non-goals

- **No run-loop promotion.** The per-task run-loop driver (`cmd_dev_runloop.go`) stays
  in `cmd/harbor` this phase; its population helpers are Phase 110b, the fan-out is
  Phase 110d.
- **No behaviour change.** Dispatch semantics, D-026 thresholds, spawn-depth defaults,
  poll cadence, and the envelope's JSON byte-shape are unchanged — golden tests pin
  parity.
- **No external (`harbortest`-style) facade.** This surface stays module-internal; the
  external-module export program is Wave D (RFC-level), out of scope here.
- No change to `steering.ToolExecutor`'s interface shape (the seam Phase 83i shipped).

## Acceptance criteria

- [ ] `internal/runtime/dispatch` exists; `dispatch.NewToolExecutor(...)` returns a
      `steering.ToolExecutor` covering all four Decision shapes (`CallTool`,
      `CallParallel`, `SpawnTask`, `AwaitTask`) with behaviour-parity to today's
      `devToolExecutor` (golden/unit tests pin: D-026 promotion at threshold, non-atomic
      parallel mode, depth-cap rejection, terminal-status polling).
- [ ] `planner.AnswerEnvelope` + the terminal error-code constants +
      `planner.TaskErrorCodeForFinish` are exported in `internal/planner`; a golden test
      pins the envelope's JSON encoding byte-for-byte against the Phase 106 shape; the
      run-loop driver and `dispatch`'s `taskOutcomeObservation` both consume them (the
      implicit cmd↔cmd wire contract is now one named type).
- [ ] `tools.NewPlannerView` exported in `internal/tools`; compile-time
      `planner.ToolCatalogView` satisfaction asserted from `internal/planner` tests;
      identity/scope filtering behaviour-identical to `newRuntimeCatalogView`
      (including the empty-`granted` rule).
- [ ] **§13 consumer in the same phase:** `cmd/harbor` calls `dispatch.NewToolExecutor`
      + `tools.NewPlannerView`; `cmd_dev_executor.go` and `cmd_dev_catalog_view.go` are
      deleted; `harbortest/devstack` wires the SAME constructor and its degraded
      `devStackToolExecutor` (`devstack.go:2103-2143`) is deleted — no second executor
      implementation survives anywhere (§13 two-implementations rule).
- [ ] `internal/planner/react/prompt.go`'s shape-contract comment cites the
      `internal/runtime/dispatch` exported identifier, not `cmd/harbor`; no `internal/`
      file references `cmd_dev_executor.go` any more (grep-asserted in the smoke).
- [ ] The D-192 HITL E2E exercises the promoted executor (planner-dispatched gated tool
      through `dispatch.NewToolExecutor`); its test-local shim + §17.6 test-gap comment
      are removed.
- [ ] Concurrent-reuse test (§11/D-025): N≥100 concurrent `ExecuteDecision` invocations
      against ONE shared executor under `-race` — no data races, no context bleed
      (per-run identity assertions), no cancellation cross-talk, goroutine baseline
      restored.
- [ ] All prior phase smokes + integration tests pass against the converted binary (no
      regression; preflight green).

## Files added or changed

- `internal/runtime/dispatch/dispatch.go` — the promoted executor + options +
  package doc (new package; lives under the §3 `internal/runtime/` ellipsis).
- `internal/runtime/dispatch/dispatch_test.go` — behaviour-parity units + the D-025
  concurrent-reuse test.
- `internal/planner/answer_envelope.go` (+ `_test.go`) — `AnswerEnvelope`, error-code
  constants, `TaskErrorCodeForFinish`, JSON golden test.
- `internal/tools/planner_view.go` (+ `_test.go`) — `PlannerView` + `NewPlannerView`.
- `internal/planner/react/prompt.go` — shape-contract comment re-pointed; the
  truncation-summary shape referenced by exported name.
- `cmd/harbor/cmd_dev_executor.go` — **deleted**.
- `cmd/harbor/cmd_dev_catalog_view.go` — **deleted**.
- `cmd/harbor/cmd_dev.go` + `cmd/harbor/cmd_dev_runloop.go` — thin-caller conversion
  (constructor call sites + envelope/code consumption).
- `harbortest/devstack/devstack.go` — degraded executor deleted; `dispatch` wired; the
  D-094 mirror shrinks.
- `test/integration/<the D-192 HITL E2E file>` — shim removed; real executor wired.
- `scripts/smoke/phase-110a.sh` — assertions below.
- `docs/glossary.md` — "answer envelope" entry.
- `docs/decisions.md` — D-194 (authored at ship time by the implementor, not this
  planning PR).

## Public API surface

- `dispatch.NewToolExecutor(cat tools.ToolCatalog, store artifacts.ArtifactStore, reg tasks.TaskRegistry, opts ...Option) steering.ToolExecutor`
- `dispatch.WithHeavyThreshold(bytes int) Option` / `dispatch.WithMaxSpawnDepth(n int) Option` / `dispatch.WithLogger(l *slog.Logger) Option`
- `planner.AnswerEnvelope` + `planner.TaskErrorCodeRunLoopError` /
  `planner.TaskErrorCodeCancelled` + `planner.TaskErrorCodeForFinish(FinishReason) string`
- `tools.PlannerView` + `tools.NewPlannerView(cat ToolCatalog, filter CatalogFilter) PlannerView`

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at the
> top level). This surface is stable for in-module consumers (cmd, harbortest,
> examples); external-team embedding needs a future facade/export RFC (the audit's
> Wave D), out of scope for this band.

### SDK-consumer reachability

The lens this phase exists for: a Go consumer embedding the runtime headless (no
`harbor` binary) currently CANNOT obtain a production-grade `ToolExecutor` — the only
full implementation is unexported in `package main`, so a headless run loop either
hand-rolls dispatch (re-implementing D-026, parallel, spawn semantics) or inherits
devstack's degraded copy and trips `ErrContextLeak` on the first heavy result. After
110a, `dispatch.NewToolExecutor(cat, store, reg)` is one constructor call on the same
internal surface `steering.NewRunLoop` already exposes — the runloop/tasks seam moves
from "partial" toward "yes" on the audit's reachability scorecard.

## Test plan

- **Unit:** Decision-shape dispatch parity (all four shapes); D-026 promotion at/below/
  above threshold incl. the artifact-store-failure degradation path (loud Warn, preview
  fallback); spawn-depth-cap rejection; envelope JSON golden (byte-for-byte vs Phase
  106); `TaskErrorCodeForFinish` table test; `PlannerView` filter semantics incl.
  empty-granted-scopes rule.
- **Integration:** the D-192 HITL E2E converted to the promoted executor (real catalog +
  approval gate + pause/resume drivers on the seam; identity propagation; the
  gated-tool-rejected failure mode); plus an existing-runloop integration pass proving
  cmd's converted wiring drives a real tool end-to-end (real `inmem` drivers, identity
  asserted on the stored artifact's scope).
- **Conformance:** N/A — single concrete behind an existing interface
  (`steering.ToolExecutor`), not a §4.4 multi-driver registry.
- **Concurrency / leak:** the mandatory D-025 concurrent-reuse test (N≥100, one shared
  executor, `-race`, goroutine-baseline assertion); parallel-cancel test (cancelling run
  A's ctx mid-`CallParallel` leaves run B unaffected).

## Smoke script additions

`scripts/smoke/phase-110a.sh` (static-only): assert `cmd/harbor/cmd_dev_executor.go`
and `cmd_dev_catalog_view.go` no longer exist; assert
`internal/runtime/dispatch/dispatch.go` exists; grep-assert no file under `internal/`
or `harbortest/` references `cmd_dev_executor.go`; run
`go test ./internal/runtime/dispatch/ ./internal/tools/ ./internal/planner/ -run 'Executor|PlannerView|AnswerEnvelope' -race -count=1`.
Skeleton ships with this plan (standard skip until the phase implements).

## Coverage target

- `internal/runtime/dispatch`: 85%.
- `internal/tools` (the new view file): 90% on the added file; package stays ≥ its
  existing target.
- `internal/planner` (the envelope file): 95% on the added file (pure projection).

## Dependencies

- D-192 — the in-flight steering HITL-deadlock fix (its E2E is the shim this phase
  resolves; merge order: D-192 lands first).
- 107d (`CallParallel` executor dispatch — D-169) and 107e (`SpawnTask`/`AwaitTask`
  dispatch — D-170): the behaviour being promoted.
- 83i (the `steering.ToolExecutor` seam + the original executor — D-152).

## Risks / open questions

- **Merge coordination (staging).** 110a runs in **Stage 1 in parallel with 110c**;
  110b and 110d (Stage 2) both depend on this phase's exported envelope/constructor and
  must not be dispatched until Stage 1 merges. Both Stage-1 phases touch
  `cmd/harbor/cmd_dev.go` and `harbortest/devstack/devstack.go` — small, mechanical
  conflicts are expected at merge time; the coordinator drains them.
- **D-192 in flight.** If the steering fix's E2E shape changes before merge, the
  shim-resolution criterion adapts to whatever the merged test looks like (the binding
  part is "the E2E uses the real promoted executor, no test-local executor shim
  survives").
- **Envelope home (planner vs tasks).** Settled above on import-direction grounds; if
  implementation surfaces a cycle (it should not — `planner` has no `tasks` import on
  this path today), flipping to `internal/tasks` with stringly-typed reasons is the
  documented §4.3 fallback, recorded in D-194.
- **Behaviour-parity risk.** The executor is the hottest production path in the dev
  binary; the golden/parity tests + the unchanged prior-phase smokes are the guard.

## Glossary additions

- **Answer envelope** — the exported `{answer, finish_reason, tool_calls_seen}` JSON
  shape (`planner.AnswerEnvelope`) a run-loop driver marshals into
  `tasks.TaskResult.Value` on `FinishGoal`, and that `tasks.get` projectors and
  `AwaitTask` observations parse back. Phase 106 introduced the shape; Phase 110a names
  and exports it. Add to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — the
      executor + view carry identity per run; the concurrent-reuse test's per-run
      identity assertions cover it.
- [ ] **Concurrent-reuse test passes (D-025)** — the promoted executor is a compiled
      artifact; N≥100 under `-race` as specified above.
- [ ] **Integration test (§17):** the converted E2E + runloop integration pass with real
      drivers, identity propagation, ≥1 failure mode, under `-race`.
- [ ] Glossary updated (answer envelope)
- [ ] If a brief finding was departed from: N/A — none departed.
