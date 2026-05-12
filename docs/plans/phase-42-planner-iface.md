# Phase 42 — Planner interface + Decision sum + RunContext

## Summary

Land the swappable-planner seam (one of Harbor's three non-negotiable product properties — see CLAUDE.md §1 / RFC §3.2). This phase ships the `Planner` interface, the six-shape `Decision` sum-type (`CallTool`, `CallParallel`, `SpawnTask`, `AwaitTask`, `RequestPause`, `Finish`), the planner-side `RunContext` (the ONLY surface the planner sees), the narrow `ToolCatalogView` / `MemoryView` / `SkillLookup` planner-facing views, the wake-mode taxonomy (`push` / `poll` / `hybrid` — D-032) as a planner-side `WakeMode` enum plus an optional `WakeAware` interface, a `Trajectory` skeleton that Phase 43 closes with the fail-loudly `Serialize` contract, a stub `finish.Planner` that returns `Finish{Reason: Goal}` end-to-end, the conformance harness skeleton future concretes (Phase 45 ReAct, Phase 48 Deterministic, Phase 49 conformance pack) consume, and the §13 import-graph lint that gates the planner-runtime decoupling.

## RFC anchor

- RFC §6.2
- RFC §3.2
- RFC §3.4
- RFC §3.5
- RFC §11

## Briefs informing this phase

- brief 02
- brief 07

## Brief findings incorporated

- **brief 02 §2 (`Decision` is a sum type, not a magic `next_node` string).** "Tool call, background task, and subagent are runtime-level concepts here, NOT planner-internal opcodes (the reference implementation overloaded a single `next_node` field with magic strings — Harbor does not)." Phase 42 ships six distinct Go types each tagged by an unexported `isDecision()` marker; no string discriminator.
- **brief 02 §2 (`RunContext` is a read+narrow-write view).** "The planner cannot reach into runtime internals." Phase 42's `RunContext` fields are either value types, narrow read interfaces (`ToolCatalogView`, `MemoryView`, `SkillLookup`), the artifact store interface, or function closures (`Clock`, `Emit`) — never concrete runtime structs.
- **brief 02 §6 (planner-conformance test harness).** "A shared test pack that any `Planner` implementation must pass." Phase 42 ships the harness skeleton in `internal/planner/conformance/`; Phase 49 fills it with the conformance scenarios. The §13 import-graph lint test lives here from t=0 — it's the single most load-bearing assertion the planner package can carry.
- **brief 02 §9 Q-5 (`NoOp` decisions are NOT part of the interface).** Settled in RFC §6.3 — "wait-for-steering and trajectory-summarization are Runtime short-circuits." Phase 42 omits a `NoOp` variant; only six Decision shapes ship.
- **brief 07 §2 (data flow: planner ↔ runtime).** "The planner emits typed `Decision` values; the runtime executes the side effects." Phase 42's `Decision` sum carries `Reasoning` on `CallTool`, the join shape on `CallParallel`, and structured `Payload` maps on `RequestPause` / `Finish` — every shape is round-trippable through `Trajectory.Serialize` (Phase 43 closes the contract).
- **brief 07 §8 (planner package imports no Runtime internals).** The import-graph lint test (`internal/planner/conformance/importgraph_test.go`) walks every Go file under `internal/planner/...` with `go/parser` and fails on any `internal/runtime/...` import path. This is the single most binding rule the phase ships; concretes added in 45 / 48 inherit it for free.

## Findings I'm departing from (if any)

- **brief 02 §2 sketch types `TaskKind` and `TaskSpec` as planner-local.** Departed: `SpawnTask.Kind` is `tasks.TaskKind` and `SpawnTask.Spec` is a planner-package `SpawnSpec` wrapper around the `tasks.SpawnRequest` shape. **Why:** the tasks subsystem already ships `tasks.TaskKind` / `tasks.SpawnRequest` (Phase 20 / 21); duplicating the type in the planner would be a §13 "two parallel implementations of the same conceptual feature" smell. Importing `internal/tasks` is allowed — tasks is NOT a `internal/runtime/...` package. Recorded as D-047.
- **brief 02 §2 sketches `RequestPause.Reason` as a planner-local `PauseReason`.** Followed: the canonical `pauseresume.Reason` lives in a not-yet-shipped phase (the unified pause/resume primitive — see CLAUDE.md §7.4 / RFC §3.3). Phase 42 ships `PauseReason` in the planner package with the four canonical values (`approval_required`, `await_input`, `external_event`, `constraints_conflict`); when the pauseresume primitive lands it MAY canonicalise the enum via a typedef bridge (no shape change). Recorded as D-047.

## Goals

- Ship the `Planner` interface (`Next(ctx, RunContext) (Decision, error)`) as Harbor's swappable-planner contract.
- Ship the six-shape `Decision` sum-type with `isDecision()` marker methods so every variant compiles only where the interface expects it.
- Ship `RunContext` as the planner-only surface — narrow views, no runtime internals.
- Ship `ToolCatalogView` / `MemoryView` / `SkillLookup` narrow view interfaces (planner-side; the runtime adapter from the production catalog/memory/skills lives at the runtime engine phase later in Wave 8 / 9).
- Ship the `WakeMode` enum (`push` / `poll` / `hybrid`) + `WakeAware` optional interface implementing D-032 — planner-side metadata, NOT a TaskRegistry knob.
- Ship a `Trajectory` skeleton (struct + zero-value `Serialize` stub returning `ErrTrajectoryNotImplemented`) so Phase 43 can land the fail-loudly serialise contract without redefining types.
- Ship the `finish.Planner` stub that always returns `Finish{Reason: Goal}`, proving the interface holds and the runtime can execute a Decision end-to-end.
- Ship the `internal/planner/conformance/` harness skeleton — Phase 45 / 48 / 49 fill it; Phase 42 ships the binding §13 import-graph lint test.
- Concurrent-reuse contract per D-025: the stub `finish.Planner` is a reusable artifact; N≥100 concurrent invocations against a single shared instance under `-race` pass without races / context bleed / cancel cross-talk / goroutine leaks.

## Non-goals

- No reference `react` concrete planner — Phase 45.
- No `deterministic` concrete planner — Phase 48.
- No conformance pack scenarios (the test harness is a skeleton in Phase 42; Phase 49 fills the scenarios — top-20 prompts, malformed-LLM-output salvage, parallel-pause atomicity, etc.).
- No `Trajectory.Serialize` real implementation — Phase 43 closes the fail-loudly contract.
- No schema repair pipeline — Phase 44 (`internal/planner/repair/`).
- No `ToolContext` handle-registry — Phase 43 ships the split; the handle re-attach lands with the pauseresume primitive.
- No runtime executor for Decisions — that's the runtime/engine planner-step phase that lands later in Wave 8 / 9.
- No protocol surface — planner is a pure runtime-internal subsystem; the Protocol projection lives in `internal/protocol/...` phases.
- No `pauseresume.Reason` canonicalisation — the unified pause primitive ships separately.

## Acceptance criteria

- [ ] `internal/planner/planner.go` defines `Planner` interface (`Next(ctx context.Context, run RunContext) (Decision, error)`).
- [ ] `internal/planner/planner.go` defines `RunContext` with the field set spec'd in RFC §6.2.
- [ ] `internal/planner/planner.go` defines `ToolCatalogView`, `MemoryView`, `SkillLookup` narrow read interfaces.
- [ ] `internal/planner/planner.go` defines `PlanningHints`, `Budget`, `ControlSignals`, `FinishReason`, `PauseReason`, `JoinSpec`, `SpawnSpec` supporting types.
- [ ] `internal/planner/decision.go` defines the `Decision` sealed interface with `isDecision()` and the six variant types (`CallTool`, `CallParallel`, `SpawnTask`, `AwaitTask`, `RequestPause`, `Finish`).
- [ ] `internal/planner/trajectory.go` defines `Trajectory`, `TrajectoryStep`, `TrajectorySummary`, `ToolContext`, `Source`, `SteeringInjection`, `BackgroundResult`, `ResumeHint`, `FailureRecord`, `StreamChunk` skeletons. `Trajectory.Serialize()` returns `(nil, ErrTrajectoryNotImplemented)` — Phase 43 supersedes.
- [ ] `internal/planner/wake.go` defines `WakeMode` (`WakePush` / `WakePoll` / `WakeHybrid`) and the optional `WakeAware` interface. Godoc cites D-032.
- [ ] `internal/planner/errors.go` defines sentinels: `ErrTrajectoryNotImplemented`, `ErrPlannerClosed`, `ErrInvalidDecision`.
- [ ] `internal/planner/events.go` registers planner-emitted event types: `planner.decision`, `planner.finish`, `planner.error`.
- [ ] `internal/planner/finish/finish.go` ships `New(opts ...Option) planner.Planner` returning a stub that always emits `Finish{Reason: FinishGoal}`. Implements `WakeAware` returning `WakePush` (cheapest valid choice — never spawns, so the mode is structurally irrelevant; the field is exercised by the conformance harness).
- [ ] `internal/planner/conformance/conformance.go` ships the `Harness` struct + `Run(t *testing.T, factory func() Harness)` skeleton. The skeleton declares the slot for each conformance scenario but every scenario is a `t.Run(..., func(t *testing.T) { t.Skip("Phase 49: conformance scenarios") })` until Phase 49 fills them.
- [ ] `internal/planner/conformance/importgraph_test.go` walks `internal/planner/...` Go files via `go/parser` and asserts NO `internal/runtime/...` import. This is the §13-gate test. Lives in conformance/ so concretes added later inherit it.
- [ ] `internal/planner/planner_test.go` covers: `Decision` shapes compile against the sealed interface; `RunContext` zero-value is well-defined; `WakeMode` round-trips through `String()`; `finish.Planner` stub returns `Finish{Reason: FinishGoal}` end-to-end with a context-bound identity quadruple.
- [ ] `internal/planner/finish/finish_test.go` covers the stub planner happy path + cancel propagation + identity propagation.
- [ ] `internal/planner/concurrent_test.go` ships the D-025 concurrent-reuse test: N=128 concurrent `finish.Planner.Next` calls against a single shared instance under `-race`; asserts no races, no context bleed (each call's `RunContext.RunID` round-trips out via `Emit`), no cancellation cross-talk (cancelling one ctx does not affect siblings), no goroutine leaks (baseline `runtime.NumGoroutine()` restored after all calls return).
- [ ] `scripts/smoke/phase-42.sh` exists and `skip`s with the standard "no protocol surface yet" message — Phase 42 is a pure code phase; the executor that runs a `finish.Planner` end-to-end lives at a later phase that DOES have a protocol surface and a smoke check.
- [ ] `docs/decisions.md` D-047 records the two "Findings I'm departing from" lines + the wake-mode planner-side enum decision.
- [ ] `docs/glossary.md` gains entries for: `Planner`, `Decision`, `RunContext`, `Trajectory`, `WakeMode` (push / poll / hybrid), `FinishReason`, `PauseReason`.
- [ ] `docs/plans/README.md` Phase 42 row flips to `Shipped`.
- [ ] `README.md` Status table updated.
- [ ] Coverage on `internal/planner`: ≥ 90%.

## Files added or changed

- `internal/planner/planner.go` (new) — Planner iface + RunContext + views + supporting types.
- `internal/planner/decision.go` (new) — Decision sum + 6 variants.
- `internal/planner/trajectory.go` (new) — Trajectory + ToolContext skeleton.
- `internal/planner/errors.go` (new) — sentinels.
- `internal/planner/events.go` (new) — event-type registrations.
- `internal/planner/wake.go` (new) — WakeMode + WakeAware + D-032 godoc anchor.
- `internal/planner/planner_test.go` (new).
- `internal/planner/concurrent_test.go` (new) — D-025 reuse test.
- `internal/planner/finish/finish.go` (new) — stub planner.
- `internal/planner/finish/finish_test.go` (new).
- `internal/planner/conformance/conformance.go` (new) — harness skeleton.
- `internal/planner/conformance/importgraph_test.go` (new) — §13 lint gate.
- `scripts/smoke/phase-42.sh` (new) — skip-shaped skeleton.
- `docs/plans/phase-42-planner-iface.md` (this file).
- `docs/plans/README.md` (modified — Phase 42 row flips to `Shipped`).
- `docs/decisions.md` (modified — D-047 record).
- `docs/glossary.md` (modified — planner vocabulary).
- `README.md` (modified — Status table).

## Public API surface

```go
package planner

import (
    "context"
    "encoding/json"
    "time"

    "github.com/hurtener/Harbor/internal/artifacts"
    "github.com/hurtener/Harbor/internal/events"
    "github.com/hurtener/Harbor/internal/identity"
    "github.com/hurtener/Harbor/internal/tasks"
    "github.com/hurtener/Harbor/internal/tools"
)

// Planner is the entire reasoning-policy contract.
type Planner interface {
    Next(ctx context.Context, run RunContext) (Decision, error)
}

// RunContext is the only surface the planner sees. All fields are
// either value types, narrow read interfaces, or function closures.
type RunContext struct {
    Quadruple   identity.Quadruple

    Query       string
    Goal        string
    LLMContext  map[string]any
    ToolContext ToolContext
    Trajectory  *Trajectory
    Hints       PlanningHints

    Catalog     ToolCatalogView
    Memory      MemoryView
    Skills      SkillLookup
    Artifacts   artifacts.ArtifactStore

    Control     ControlSignals
    Budget      Budget
    Clock       func() time.Time
    Emit        func(events.Event)
}

// Decision sum-type. Six variants. The unexported isDecision() marker
// makes the interface sealed.
type Decision interface{ isDecision() }

type CallTool      struct { Tool string; Args json.RawMessage; Reasoning string }
type CallParallel  struct { Branches []CallTool; Join *JoinSpec }
type SpawnTask     struct { Kind tasks.TaskKind; Spec SpawnSpec; GroupID tasks.TaskGroupID }
type AwaitTask     struct { TaskID tasks.TaskID }
type RequestPause  struct { Reason PauseReason; Payload map[string]any }
type Finish        struct { Reason FinishReason; Payload any; Metadata map[string]any }

// Narrow planner-facing views.
type ToolCatalogView interface {
    Resolve(name string) (tools.Tool, bool)
    List() []tools.Tool
}

type MemoryView interface {
    Snapshot(ctx context.Context) (map[string]any, error)
}

type SkillLookup interface {
    Search(ctx context.Context, query string, limit int) ([]SkillResult, error)
    Get(ctx context.Context, id string) (*Skill, error)
}

// Wake-mode taxonomy (D-032).
type WakeMode string

const (
    WakePush   WakeMode = "push"
    WakePoll   WakeMode = "poll"
    WakeHybrid WakeMode = "hybrid"
)

// WakeAware is OPTIONAL. Concretes that implement it expose their
// non-retain-turn wake policy to the conformance pack + observability.
type WakeAware interface {
    WakeMode() WakeMode
}
```

The full set of supporting types (`PlanningHints`, `Budget`, `ControlSignals`, `FinishReason`, `PauseReason`, `JoinSpec`, `SpawnSpec`, `Trajectory`, `ToolContext`, etc.) lives in `internal/planner/planner.go` + `trajectory.go`.

## Test plan

- **Unit:** `planner_test.go` covers `Decision` shapes compile against the sealed interface, `RunContext` zero-value semantics, `WakeMode.String()` round-trip, `PauseReason` / `FinishReason` enum exhaustiveness, view-interface zero-value behaviour. `finish/finish_test.go` covers the stub's happy path + ctx cancellation + identity propagation.
- **Integration:** Phase 42 ships no cross-subsystem seam (Deps `09, 13, 26, 32` are TYPE consumers — `tasks.TaskKind`, `tools.Tool`, `events.Event`, identity types — not behavioural consumers). The runtime executor that drives the `finish.Planner` end-to-end lands at the planner-step phase (later in Wave 8 / 9); its PR ships the cross-subsystem integration test. Phase 42's `concurrent_test.go` proves the planner package consumes the type imports cleanly under `-race`.
- **Conformance:** `internal/planner/conformance/conformance.go` ships the harness skeleton; every scenario is a `t.Skip("Phase 49: conformance scenarios")`. The skeleton's `Harness` struct declares `Factory func() planner.Planner` + `WakeMode WakeMode` (for the round-trip assertion Phase 49 fills). `importgraph_test.go` (this PR) is the binding §13 lint — walks every Go file under `internal/planner/...` and asserts no `internal/runtime/...` imports.
- **Concurrency / leak:** `concurrent_test.go` runs N=128 concurrent `Next` calls against a single shared `finish.Planner` under `-race`. Asserts: no races (race detector), no context bleed (each call carries a unique `RunContext.Quadruple.RunID` that surfaces back via `Emit`; per-call assertion), no cancellation cross-talk (cancel one ctx, assert siblings complete), no goroutine leak (baseline `runtime.NumGoroutine()` restored after the WaitGroup join).

## Smoke script additions

- `scripts/smoke/phase-42.sh` ships a `skip "phase 42: planner subsystem has no protocol surface; the runtime executor + protocol smoke land at a later wave"` line. This keeps preflight green and the §4.2 contract honoured. When the planner-step executor lands, it extends ITS phase's smoke script with the end-to-end assertion — Phase 42 ships pure types + a stub + tests.

## Coverage target

- `internal/planner`: 90%.
- `internal/planner/finish`: 90%.
- `internal/planner/conformance`: not graded (skeleton + lint test only).

## Dependencies

- 09 (StateStore generic surface — RunContext-adjacent state shape).
- 13 (events bus — `RunContext.Emit` signature).
- 26 (tools — `ToolCatalogView` resolves `tools.Tool`).
- 32 (LLM client core — the planner does not import the LLM client directly, but the Decision shapes are designed against the `CompleteRequest` / `CompleteResponse` projection ReAct will exercise at Phase 45).

## Risks / open questions

- **Pauseresume primitive deferred.** `PauseReason` lives in the planner package at Phase 42 (four canonical values). When the unified pause/resume primitive phase lands it MAY canonicalise — the planner enum becomes a typedef bridge. **Mitigation:** the enum values match the RFC §6.3 / brief 02 §2 spec exactly; the canonicalisation is a typedef rename, no caller change required.
- **Trajectory.Serialize stubbed.** Phase 43 closes the fail-loudly contract. **Mitigation:** the stub returns `ErrTrajectoryNotImplemented` — no silent-drop path is possible.
- **Conformance pack stubbed.** Phase 49 fills the scenarios. **Mitigation:** the §13 import-graph lint test ships at Phase 42 — it's the most load-bearing assertion and gates every concrete added thereafter.
- **Brief 02 Q-1 (second concrete planner in V1).** Settled: RFC §11 picks `deterministic` (Phase 48). Phase 42 ships the iface; the second concrete validates it at Phase 48.

## Glossary additions

- `Planner` — the swappable reasoning-policy contract: `Next(ctx, RunContext) (Decision, error)`.
- `Decision` — sealed sum-type the planner returns each step. Six shapes: `CallTool`, `CallParallel`, `SpawnTask`, `AwaitTask`, `RequestPause`, `Finish`.
- `RunContext` — the only surface a planner sees. Narrow views over tools / memory / skills / artifacts; identity, budget, control signals, clock, emit closure.
- `Trajectory` — append-only execution log. Phase 43 closes the fail-loudly `Serialize` contract; Phase 42 ships the skeleton.
- `WakeMode` (push / poll / hybrid) — planner-concrete metadata implementing D-032: push wakes the LLM on group resolution, poll re-checks deterministically, hybrid combines push + a status sidecar.
- `WakeAware` — optional interface a planner concrete may implement to expose its wake mode to the conformance pack + observability.
- `FinishReason` — the terminal reason a planner returns: `goal`, `no_path`, `cancelled`, `deadline_exceeded`, `constraints_conflict`.
- `PauseReason` — the four canonical pause reasons: `approval_required`, `await_input`, `external_event`, `constraints_conflict`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. The `finish.Planner` stub IS a reusable artifact; `concurrent_test.go` ships the N=128 test.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. Phase 42 imports TYPE shapes from `tasks` / `tools` / `events` / `identity` / `artifacts`; it does NOT consume their behavioural surface — the runtime executor that drives the stub planner end-to-end ships at a later phase, with its own integration test. Mark N/A: no behavioural consumption, no seam closed in this PR.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-047)
