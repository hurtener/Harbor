# Phase 83i — runcontext-wiring

## Summary

Phase 83i is the structural foundation Wave 17 needs to make `harbor
dev` actually work end-to-end against a real LLM + real MCP tools.
The §17.5 operator-validation audit pinned four root causes for the
"64 steps, 0 tool calls" failure mode the v1.1 validation hit:

1. `RunContext.Catalog` never populated → planner's
   `<available_tools>` rendered empty → LLM had no tool affordance.
2. `RunContext.Trajectory` never populated AND `steering.RunLoop`
   never appended between steps → planner sent the identical prompt
   forever (multi-step ReAct structurally broken).
3. The runloop's `default:` case dropped every CallTool decision on
   the floor (Phase 53's deliberately-punted scope) — no production
   tool executor existed.
4. `MemoryStore.AddTurn` was never called by any production caller —
   the read path (Phase 83f) returned empty patches forever.

This phase closes all four. The validation against `mcp-youtube`
that motivated Wave 17 now passes: the planner sees the 6 youtube
tools, calls `youtube.get_metadata`, sees the tool result on its
next step's trajectory, and synthesises a Finish — 2 LLM calls, end
to end.

## RFC anchor

- RFC §6.2 — Planner subsystem (`ToolCatalogView`, `RunContext`).
- RFC §6.6 — Memory subsystem (`AddTurn` writeback).
- RFC §6.8 — Runtime engine (steering.RunLoop dispatch layer).

## Briefs informing this phase

- brief 02
- brief 13
- brief 04

## Brief findings incorporated

- brief 02 §6: the runtime owns dispatch + per-run state; the planner
  reads. 83i wires the runtime side that was missing.
- brief 13 §2.4 / §5: heavy tool results must NOT round-trip through
  the LLM verbatim — the runtime promotes via the artifact store.
  83i implements this in the dev binary's executor (D-026 discipline).
- brief 04 §3: memory writeback at FinishGoal is the multi-turn
  affordance.

## Findings I'm departing from (if any)

None. 83i closes consumer gaps against already-shipped primitives.

## Goals

- The dev binary's RunLoop driver populates `RunContext.Catalog`
  (planner-facing schema-only projection of the production
  `tools.ToolCatalog` under the run's identity scope) and
  `RunContext.Trajectory` (per-run object whose Steps the runloop
  appends after every dispatched decision).
- `steering.RunLoop` gains a `ToolExecutor` seam + `RunSpec.ToolExecutor`
  optional field. The default case dispatches via the executor when
  set, appends `trajectory.Step{Action, Observation, LLMObservation}`,
  and re-enters the planner.
- The dev binary's `devToolExecutor` dispatches CallTool against the
  production catalog. CallParallel / SpawnTask / AwaitTask return
  `ErrDecisionShapeUnsupported`; the runloop surfaces the error as
  the step's observation so the planner re-plans.
- D-026 heavy-content discipline lives in the dev executor: tool
  results whose JSON encoding exceeds the configured threshold get
  stored in the artifact store and the planner sees a small summary
  plus ArtifactRef as its `LLMObservation`.
- `MemoryStore.AddTurn` fires on FinishGoal with the user's query
  plus the planner's answer (best-effort; logged but not failure-mode).
- The driver populates `RunContext.Emit` so the planner's
  `planner.decision` / `planner.finish` / `planner.repair_guidance_injected`
  events reach the bus (without this the operator sees only
  `llm.cost.recorded` and the Console reasoning channel is empty).

## Non-goals

- CallParallel / SpawnTask / AwaitTask dispatchers — separate phase
  (the runloop's seam supports them; the dev executor declines).
- A reasoning-trace round-trip onto trajectory.Step.ReasoningTrace
  (the runtime's bus subscription path could pull from
  planner.decision events; deferred to a follow-up).
- Operator-supplied catalog-filter scopes — TODO comment in driver;
  V1.1 ships nil GrantedScopes.

## Acceptance criteria

- [x] `steering.RunLoop.RunSpec.ToolExecutor` field + `steering.ToolExecutor`
      interface land in `internal/runtime/steering/runloop.go`.
- [x] The runloop's `default:` case dispatches via the executor when
      set, captures `(observation, llmObservation)`, and appends a
      `trajectory.Step` to `spec.Base.Trajectory.Steps`.
- [x] `cmd/harbor/cmd_dev_executor.go::devToolExecutor` implements
      `steering.ToolExecutor` against `tools.ToolCatalog`. CallTool
      dispatches; non-CallTool returns `ErrDecisionShapeUnsupported`.
- [x] The dev executor projects heavy results (`json.Marshal(raw) > heavyThreshold`)
      to an `ArtifactStub`-shaped llmObservation via
      `artifacts.ArtifactStore.PutText`; sub-threshold results pass through.
- [x] `cmd/harbor/cmd_dev_catalog_view.go::runtimeCatalogView` adapts
      `tools.ToolCatalog` → `planner.ToolCatalogView` with a per-run
      `CatalogFilter`.
- [x] `cmd/harbor/cmd_dev_runloop.go::runOne` constructs the Trajectory,
      builds the Catalog view, wires the executor + Emit closure, sets
      `Base.Catalog` / `Base.Trajectory` / `Base.Emit` /
      `spec.ToolExecutor` / `spec.MaxSteps` before calling
      `runLoop.Run`.
- [x] After `runLoop.Run` returns `FinishGoal`, the driver calls
      `memory.AddTurn(taskCtx, sessionQ, ConversationTurn{...})` with
      best-effort error logging.
- [x] `harbortest/devstack/devstack.go` mirrors all of the above per
      D-094.
- [x] Operator validation: `harbor scaffold && harbor dev` against the
      mcp-youtube agent reaches a successful `FinishGoal` on a real
      youtube-tool call in ≤3 LLM round-trips. (Verified live during
      83i implementation.)

## Files added or changed

- `internal/runtime/steering/runloop.go` — `ToolExecutor` interface +
  `ErrDecisionShapeUnsupported` + `RunSpec.ToolExecutor` + the
  `default:` case dispatch + trajectory append.
- `cmd/harbor/cmd_dev_executor.go` — new; the dev binary's executor.
- `cmd/harbor/cmd_dev_catalog_view.go` — new; the catalog adapter.
- `cmd/harbor/cmd_dev_runloop.go` — Catalog + Trajectory + Emit +
  ToolExecutor + MaxSteps wiring in runOne; memory writeback;
  `extractAssistantAnswer` helper.
- `cmd/harbor/cmd_dev.go` — `newDevToolExecutor` call site at
  bootDevStack.
- `harbortest/devstack/devstack.go` — D-094 mirror.
- `docs/plans/phase-83i-runcontext-wiring.md` — this plan.
- `docs/plans/README.md` — Phase 83i row + flip to Shipped.
- `docs/decisions.md` — D-152.
- `scripts/smoke/phase-83i.sh` — static-surface assertions.

## Public API surface

- `steering.ToolExecutor` interface — new public seam at the runloop's
  dispatch boundary. Implementations: `cmd/harbor::devToolExecutor`,
  `harbortest/devstack::devStackToolExecutor`. The interface signature
  is `ExecuteDecision(ctx, rc, decision) (observation, llmObservation, error)`.

## Test plan

- **Unit:** runloop dispatch path covered by existing
  `internal/runtime/steering` tests (the new default-case behaviour
  defaults to nil executor → empty observation, matching the prior
  Phase 53 shape so legacy tests pass unchanged).
- **Integration:** the validation against the v1.1 operator agent
  (real bifrost + mcp-youtube + scaffolded sqlite-backed agent) is
  the binding end-to-end coverage. A standalone integration test
  with a stub stdio MCP server lands as part of Phase 83l (real-
  bifrost test backfill).
- **Failure-mode:** dev executor's `tools.ErrToolNotFound` path is
  surfaced as the step's observation; the planner re-plans rather
  than crashing the run. The runloop's `nil executor` path appends
  an empty-observation step (matching the Phase 53 shape).
- **Concurrency / leak:** the executor + view are stateless value
  types; the runloop's append mutates `spec.Base.Trajectory.Steps`
  on a per-run pointer. No new shared mutable state; D-025 trivially
  satisfied.

## Smoke script additions

`scripts/smoke/phase-83i.sh` asserts:

- `steering.ToolExecutor` declared in runloop.go.
- `cmd/harbor/cmd_dev_executor.go::devToolExecutor` exists.
- `cmd/harbor/cmd_dev_catalog_view.go::runtimeCatalogView` exists.
- `runOne` wires `Base.Catalog`, `Base.Trajectory`, `Base.Emit`,
  `ToolExecutor`.
- Memory writeback `memory.AddTurn` is called in `runOne`.
- Devstack mirrors the executor + helpers per D-094.

## Coverage target

- `cmd/harbor`: 80% (existing).
- `internal/runtime/steering`: 85% (existing).

## Dependencies

- Phase 83f (RunContext primitive consumers wiring — 83i extends with
  Catalog/Trajectory/Emit/Memory writeback).
- Phase 83g (MCP tools land in the catalog; 83i is what makes them
  reachable by the planner).
- Phase 83h (dev binary boots cleanly so 83i can be validated).
- Phase 26 (`tools.ToolCatalog`).
- Phase 23 (`memory.MemoryStore.AddTurn`).
- Phase 18 (`artifacts.ArtifactStore.PutText`).

## Risks / open questions

- **Catalog visibility scopes default to nil.** The dev token carries
  `["admin","console:fleet"]` but the driver passes nil GrantedScopes
  to the CatalogFilter. Tools without AuthScopes (the MCP-discovered
  default) pass; tools with AuthScopes would be invisible. Plumbed
  through later (Phase 83m WARN cleanup).
- **Reasoning trace round-trip onto trajectory.Step.ReasoningTrace.**
  The planner emits the reasoning on `planner.decision` events but
  the runloop's trajectory append currently leaves
  `Step.ReasoningTrace` empty. Phase 83e's `ReasoningReplay=text`
  mode is therefore ineffective in production today. Follow-up.
- **`task.tool_count` does not increment** even when CallTool fires —
  the FSM counter is wired separately. Cosmetic; tracked for
  Phase 83m cleanup.

## Glossary additions

- **ToolExecutor** — the runtime-side dispatch surface the RunLoop
  calls when the planner returns a non-Finish, non-RequestPause
  decision.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Concurrent-reuse — N/A: the executor + view are stateless;
      runloop append is per-run-pointer
- [ ] Integration test exists per CLAUDE.md §17 — operator validation
      covers it live; Phase 83l adds a fixture-binary integration
      test for recurrence guard
- [ ] Glossary updated
- [ ] If a brief finding was departed from: justified above
