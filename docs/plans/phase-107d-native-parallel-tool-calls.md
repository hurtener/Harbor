# Phase 107d — Native parallel tool-calls (executor CallParallel branch + default flip)

## Summary

Direct follow-up to Phase 107c (D-167). Phase 107c shipped native provider tool-calling for the React planner but kept a **serialization fallback** for the N>1 case: when the LLM emits several `tool_calls` in one response, the planner dispatches the head as a `CallTool` and queues the tail on `RunContext.PendingToolCalls`, draining one per step. The dev executor still rejects `planner.CallParallel` with `ErrDecisionShapeUnsupported` (`cmd/harbor/cmd_dev_executor.go:101`). This phase closes that carve-out: the dev executor grows a `CallParallel` branch backed by the **already-shipped** `internal/runtime/parallel.Executor` (Phase 47 / D-056), the React projector emits a native `CallParallel{Branches, Join: JoinAll}` for N>1 ToolCalls, and the default flips from serialization to native parallel. Serialization survives as a single-knob opt-out (`planner.react.parallel_tool_calls: false`).

The concurrent dispatch engine is **not** new work — `parallel.Executor.dispatchAll` already does goroutine fanout, branch-count cap, identity propagation, and the per-branch `Result` shape, with an N≥128 `-race` reuse test. The real new code is concentrated in two places: (1) the **trajectory → prompt round-trip**, which today assumes exactly one `CallTool` per step and must learn to emit one assistant message carrying N `tool_calls` followed by N `RoleTool` messages keyed by each branch's `CallID`; and (2) **per-branch D-026 heavy-output projection** in the executor's merge layer, so an aggregate parallel observation can't leak raw heavy content past the `ErrContextLeak` guard.

This phase also pins a semantic adaptation the 107c cutover forced: **on the native path `JoinKind` collapses to `JoinAll`.** Native provider tool-calling gives the model no channel to request `JoinFirstSuccess` / `JoinN`, and the provider wire contract makes "every `tool_call_id` is answered exactly once before the next assistant turn" a *correctness* requirement, not a policy choice. The other join kinds stay in-tree, re-scoped as the surface for **programmatic** planners (Deterministic / future Workflow / Graph) that author a `CallParallel` directly rather than via an LLM round-trip. See D-169.

**Carried-over fix from 107c — the silent reserved-name tail-drop (AC-21).** 107c's `projectResponse` (`internal/planner/react/projector.go:30-39`) switches on `resp.ToolCalls[0].Name`, and the reserved planner-control names (`_finish` / `_spawn_task` / `_await_task`) `return` the translated `Finish` / `SpawnTask` / `AwaitTask` decision **before** the `len(resp.ToolCalls) > 1` tail-queueing block at `:47-55`. So when the LLM emits a control meta-tool alongside other tool-calls in one response (e.g. three `_spawn_task` calls — "create three background tasks at once"), only the first is honoured and the remaining `ToolCalls[1:]` are **silently discarded** — no error, no event. That is exactly the silent-degradation pattern CLAUDE.md §13 forbids. The symmetric case is just as wrong: a reserved name in the *tail* gets queued to `PendingToolCalls` and later drained by `drainPending` as a literal `CallTool{Tool:"_spawn_task"}` (`:60-71`), which is never re-routed through the reserved-name switch and so hits the catalog as an unknown tool. Reserved planner-control tools are **terminal/standalone — they are not parallelisable branches** (`CallParallel.Branches` is `[]CallTool`, and the dev executor would try to `Resolve` them as catalog tools). Phase 107d makes this fail loudly: any response where a reserved control name co-occurs with another tool-call is rejected with `planner.ErrInvalidDecision` naming the constraint. This is bundled here (not a separate hotfix) because it is the same projector seam 107d already reshapes for the N>1 → `CallParallel` mapping, and the two changes must agree on what a "batchable" tool-call is.

## RFC anchor

- RFC §6.2 — Planner interface + parallel executor. The `Decision` sum is unchanged; `CallParallel` + `JoinSpec` already exist. This phase wires the existing `parallel.Executor` into the dev `ToolExecutor` and flips the React planner's emission default.
- RFC §6.5 — LLM client tool-calling contract. The native `ToolCalls[]` round-trip (107c / D-167) extends to the multi-call case: an assistant turn carrying N `tool_calls` is answered by N `RoleTool` messages.

## Briefs informing this phase

- brief 02 — planner + steering + HITL (the `Decision` sum's `CallParallel` shape; the runloop's `ToolExecutor` dispatch seam).
- brief 03 — tools + integrations + LLM client (the native multi-tool-call wire shape the round-trip must honour).
- brief 15 — native tool-calling + deferred loading + tag scoping (§6 "Decision-sum invariance": `N native ToolCalls → CallParallel`; this phase implements the second half 107c deferred).

## Brief findings incorporated

- **brief 15 §6 "Decision-sum invariance".** "Mapping is straightforward: 1 native ToolCall → CallTool; N native ToolCalls → CallParallel; 0 ToolCalls + Content → Finish." Phase 107c implemented the 1-and-0 cases and serialised the N case; Phase 107d completes the mapping by emitting the real `CallParallel` and dispatching it.
- **brief 15 §6 (continued).** "The Runtime's per-branch goroutine fanout + `JoinSpec` + `MaxParallel` cap (RFC §6.2) are unchanged." Phase 107d consumes the existing `parallel.Executor` rather than building a second dispatcher — §13 "no two parallel implementations of one feature."
- **brief 02 §"Decision sum is orthogonal to parsing".** The planner→runtime contract is the `Decision` sum, independent of how the planner derived it. Phase 107d changes only which `Decision` the React projector emits for N>1 ToolCalls (CallParallel instead of head-CallTool); the executor consumes a shape it was always meant to handle.

## Findings I'm departing from (if any)

- **Departure from Phase 47 / D-056's atomic-setup-validation posture, on the native path only.** `parallel.Executor.Execute` today fails the *whole* call if any one branch's args fail the descriptor's `Validate` (`internal/runtime/parallel/parallel.go:214-221`), before any branch dispatches. That all-or-nothing posture was designed for programmatic planners that author a plan and want side-effect safety. It is **wrong-shaped on the native path**: the provider returned N `tool_calls` and the wire contract requires all N `tool_call_id`s be answered — aborting the whole call orphans them and malforms the next request. Phase 107d adds a per-call **non-atomic** execution mode where a branch's resolve/validate failure surfaces as that branch's `Result.Err` (→ one error `RoleTool` message) instead of aborting the call. The native React path uses non-atomic mode; programmatic callers keep the atomic default. Documented in D-169.
- **Departure from D-056's "planner emits the JoinKind it wants".** Under native tool-calling the model has no channel to express a join strategy, so the React projector always emits `Join: JoinAll` (or nil → normalises to `JoinAll`). `JoinFirstSuccess` / `JoinN` / `JoinKeyed` are NOT removed — they are re-scoped as the surface for programmatic planners. Documented in D-169.

## Goals

- **Executor `CallParallel` branch.** `cmd/harbor/cmd_dev_executor.go` dispatches `planner.CallParallel` through the existing `parallel.Executor` instead of returning `ErrDecisionShapeUnsupported`. The aggregate observation carries per-branch `{CallID, Tool, Value | Err}` so the prompt builder can round-trip N `RoleTool` messages.
- **Per-branch heavy-output (D-026).** The executor's parallel merge applies the existing `projectForLLM` discipline to each branch result independently, so a parallel observation with several heavy results never trips `ErrContextLeak`.
- **Native `CallParallel` emission.** The React projector (`internal/planner/react/projector.go`) emits `CallParallel{Branches, Join: JoinAll}` for N>1 ToolCalls when native parallel is enabled (the new default), instead of head-CallTool + `PendingToolCalls` tail.
- **Trajectory + prompt round-trip for N calls.** `renderNativeStepPair` (`internal/planner/react/prompt.go`) gains a `CallParallel` case: one assistant message carrying N `ToolCalls` entries + N `RoleTool` messages, each `ToolCallID` matched to its branch's `CallID`.
- **`JoinAll`-on-native semantic, pinned.** The projector never synthesises a non-`JoinAll` join; the join machinery stays in-tree for programmatic planners.
- **Non-atomic executor mode.** A per-call option on `parallel.Executor.Execute` converts branch resolve/validate failures into per-branch `Result.Err` rather than a whole-call abort. Atomic remains the default for existing callers.
- **Single-knob opt-out.** `planner.react.parallel_tool_calls` (bool, default `true`) flips the planner between native `CallParallel` emission (true) and 107c's serialization fallback (false). Both paths stay correct.
- **Close the carried-over 107c silent tail-drop (§13).** A reserved planner-control name (`_finish` / `_spawn_task` / `_await_task`) co-occurring with any other tool-call in one response is rejected with `planner.ErrInvalidDecision` instead of silently dropping the extras. Holds on BOTH the native-parallel path and the serialization opt-out, and regardless of whether the reserved name is the head or in the tail.

## Non-goals

- **Atomic pause mid-parallel.** `parallel.Executor` still fails loud with `ErrParallelPauseUnsupported` on a mid-execution pause request (Phase 47 placeholder; Phase 50's checkpoint-atomicity was never wired because the executor had no consumer). Phase 107d accepts this as a documented limitation — dev-path branches are short tool invokes and the pause window is tiny. True checkpointed atomic-pause-mid-parallel is deferred to a Phase 50-extension follow-up.
- **`JoinFirstSuccess` / `JoinN` / `JoinKeyed` emission from the React planner.** Native tool-calling has no channel for them; they remain programmatic-planner surface. `JoinKeyed` stays unimplemented (the Phase 47 `ErrParallelInvalidJoin` reject is unchanged).
- **Removing `PendingToolCalls` / the serialization path.** It survives as the opt-out (`parallel_tool_calls: false`) and as the same-turn-discovery-race guard (107c risk #4 — a `tool_search` + a call to the not-yet-declared tool in one response must still serialise). The drain machinery (`drainPending`, `OnPendingToolCalls`) is untouched.
- **Live per-branch streaming in the Console.** Parallel tool-call cards render on `task.completed` from the trajectory (107c behaviour); live per-branch arg streaming is post-V1.
- **A second dispatcher.** The `internal/runtime/parallel.Executor` is the one dispatch site; this phase adds no fanout code of its own (§13).
- **Wiring `parallel.Executor` into non-dev binaries.** The dev `ToolExecutor` (`cmd/harbor/cmd_dev_executor.go`) is the only consumer V1.1.x ships; a production server `ToolExecutor` is out of scope here.

## Acceptance criteria

The bullets below are binding. Numbering is sequential.

### Executor dispatch

- [ ] **AC-1** `cmd/harbor/cmd_dev_executor.go`'s `ExecuteDecision` replaces the `case planner.CallParallel` `ErrDecisionShapeUnsupported` reject with a real dispatch through a `*parallel.Executor`. The executor is constructed once (in `newDevToolExecutor` or `bootDevStack`) backed by the existing catalog (which already satisfies `parallel.Resolver` via `Resolve(name)`); it is immutable after construction (D-025). `SpawnTask` / `AwaitTask` keep their `ErrDecisionShapeUnsupported` rejects unchanged.
- [ ] **AC-2** The parallel dispatch runs in **non-atomic** mode (AC-7): a branch whose tool fails to resolve or whose args fail `Validate` surfaces as that branch's error result, NOT a whole-call abort. Every branch in `CallParallel.Branches` produces exactly one outcome (success value or error) — the count of outcomes equals the count of branches, so every provider `tool_call_id` can be answered.
- [ ] **AC-3** Per-branch D-026 projection. The executor applies the existing `projectForLLM` (heavy-output → artifact-stub) to each branch's result independently before assembling the aggregate `llmObservation`. The raw aggregate `observation` carries the untruncated per-branch values; the `llmObservation` carries the per-branch projected forms. A parallel observation with several heavy branches MUST NOT trip the LLM-edge `ErrContextLeak` guard.
- [ ] **AC-4** Aggregate observation shape. The executor returns an aggregate carrying, per branch: `{call_id, tool, value | error}` (the `call_id` sourced from the originating `CallParallel.Branches[Index].CallID`; the `Index`→`CallID` correlation is deterministic per `parallel.Result.Index`). This shape is what the prompt builder (AC-9) decomposes into N `RoleTool` messages. Branch ordering in the aggregate is branch-index order (JoinAll semantics).

### Parallel executor adaptation

- [ ] **AC-5** `internal/runtime/parallel`: the React-emitted `CallParallel` always carries `Join: JoinAll` (or nil → `normaliseJoin` → `JoinAll`). The projector (AC-8) never synthesises `JoinFirstSuccess` / `JoinN` / `JoinKeyed`. No change to `dispatchFirstSuccess` / `dispatchN` / `dispatchAll` — they remain correct for programmatic callers; only the React caller is constrained to `JoinAll`.
- [ ] **AC-6** `dispatchAll` is reused verbatim as the native-path engine — it already produces one `Result` per branch (success or `Err`) in branch-index order, which is exactly the one-result-per-`tool_call_id` shape the wire contract needs. No new fanout code (§13).
- [ ] **AC-7** Non-atomic execution mode. `parallel.Executor.Execute` gains a per-call way to select non-atomic setup (e.g. a variadic option `parallel.WithNonAtomicSetup()` or an explicit method — the implementing agent picks the signature and pins it with a test). In non-atomic mode, a branch's `Resolve` miss or `Validate` failure becomes that branch's `Result.Err` and the branch is skipped at dispatch; valid branches still fan out. The atomic default (`ErrParallelBranchInvalidArgs` whole-call abort) is unchanged for existing callers. The branch-count cap (`AbsoluteMaxParallel`) and missing-identity reject (`ErrIdentityMissing`) stay fail-loud in BOTH modes.

### React planner emission

- [ ] **AC-8** `internal/planner/react/projector.go`: when `len(resp.ToolCalls) > 1` AND native parallel is enabled (AC-11), `projectResponse` emits `planner.CallParallel{Branches: [...]CallTool, Join: nil}` (one `CallTool` per ToolCall, each carrying its `CallID`) instead of the head-CallTool + `PendingToolCalls`-tail serialization. When native parallel is disabled, the 107c serialization fallback fires unchanged.
- [ ] **AC-9** `internal/planner/react/prompt.go`: `renderNativeStepPair` (or the trajectory-replay site) gains a `case planner.CallParallel`. For a step whose `Action` is a `CallParallel` and whose `Observation` is the AC-4 aggregate, it emits: (a) ONE assistant `ChatMessage` whose `ToolCalls` slice carries all N structured calls (each `{ID, Name, Args}` from the branches); (b) N `RoleTool` messages, one per branch, each `ToolCallID` matched to the branch's `CallID` and `Content` = that branch's projected observation. The existing single-`CallTool` path is unchanged. Every `tool_call_id` in the assistant message has exactly one matching `RoleTool` message (the wire-contract invariant).
- [ ] **AC-10** `trajectory.Step.Action` stores the `planner.CallParallel` (the slot is already `any`; no wire-shape change). `Step.Observation` / `Step.LLMObservation` carry the AC-4 aggregate. The runloop's existing single-step append (`internal/runtime/steering/runloop.go:643-651`) handles this unchanged — one trajectory step per `CallParallel` decision, aggregate observation attached.
- [ ] **AC-11** Config + option. `internal/config/config.go` gains `planner.react.parallel_tool_calls bool` (yaml: `parallel_tool_calls`, default `true`). `internal/config/validate.go` accepts it (no enum to reject; bool). A new `react.Option` (`react.WithParallelToolCalls(bool)`) threads it into the planner; `cmd/harbor/cmd_dev.go::bootDevStack` plumbs the config value through. Default `true` = native `CallParallel`; `false` = 107c serialization fallback.

### Carried-over 107c fix — reserved-name tail-drop (§13)

- [ ] **AC-21** `internal/planner/react/projector.go`: `projectResponse` rejects, with a wrapped `planner.ErrInvalidDecision`, any LLM response in which a reserved planner-control name (`FinishToolName` / `SpawnTaskToolName` / `AwaitTaskToolName`) co-occurs with one or more other tool-calls — i.e. `len(resp.ToolCalls) > 1` AND any entry's `Name` is a reserved control name. The error message names the offending control tool and states that planner-control meta-tools are standalone (not batchable / not parallelisable). This guard runs BEFORE the head switch, so it fires whether the reserved name is the head or in the tail, and it fires on BOTH the native-parallel path and the serialization opt-out (it is independent of `parallel_tool_calls`). The single-reserved-call cases (one `_finish`, one `_spawn_task`, one `_await_task`) are unchanged — they still translate to `Finish` / `SpawnTask` / `AwaitTask`. The all-regular N>1 case is unchanged — it flows to `CallParallel` (AC-8) or the serialization tail (opt-out). No reserved control name ever reaches `PendingToolCalls` or a `CallParallel` branch.

### Tests

- [ ] **AC-12** Unit (Go) — `internal/planner/react/projector_test.go` extends: N>1 ToolCalls with native parallel ON → `CallParallel{Branches, Join: nil}` carrying every `CallID`; native parallel OFF → head-`CallTool` + `PendingToolCalls` tail (107c behaviour preserved). Single + zero ToolCall cases unchanged.
- [ ] **AC-13** Unit (Go) — `cmd/harbor/cmd_dev_executor_preview_test.go` (or a sibling) extends: a `CallParallel` with mixed success/failure branches → aggregate carries one outcome per branch keyed by `CallID`; a branch with bad args → that branch's error result, other branches still dispatch (AC-2 non-atomic); a `CallParallel` with ≥2 heavy-output branches → each projected to an artifact-stub, aggregate under the heavy threshold (AC-3).
- [ ] **AC-14** Unit (Go) — `internal/runtime/parallel/parallel_test.go` extends: non-atomic mode (AC-7) surfaces a bad-args branch as `Result.Err` while valid branches succeed; atomic mode still aborts the whole call with `ErrParallelBranchInvalidArgs`. The `AbsoluteMaxParallel` cap and missing-identity reject fire in both modes.
- [ ] **AC-15** Unit (Go) — `internal/planner/react/prompt_test.go` extends: a trajectory step whose `Action` is a `CallParallel` with N branches renders ONE assistant message with N `ToolCalls` + N `RoleTool` messages; every `ToolCallID` matches a branch `CallID`; ordering is branch-index order. Golden-prompt update if the assembled message shape is golden-pinned.
- [ ] **AC-16** Integration (Go) — `internal/planner/react/integration_test.go::TestReactPlanner_NativeToolCall_ParallelCalls` (the placeholder named in the 107c test plan) becomes real: scripted LLM emits N>1 ToolCalls in one response; the planner emits `CallParallel`; a real `parallel.Executor` over an in-mem catalog dispatches all branches; identity propagates to every branch; the next-turn prompt carries N `RoleTool` messages. Runs under `-race`.
- [ ] **AC-17** Concurrent-reuse (Go) — extends `internal/planner/react/concurrent_test.go` (or `cmd_dev_executor`'s reuse test): N≥100 concurrent `ExecuteDecision(CallParallel)` calls against ONE shared `devToolExecutor` + ONE shared `parallel.Executor`, each with its own identity and branch set, under `-race`. Asserts no cross-talk, no data race, baseline goroutine count restored. (The `parallel.Executor`'s own D-025 test already pins the dispatcher; this pins the dev-executor merge layer.)
- [ ] **AC-22** Unit (Go) — `internal/planner/react/projector_test.go` extends for AC-21: (a) `_spawn_task` head + two more tool-calls → `ErrInvalidDecision`, error names the control tool; (b) a regular tool head + a `_spawn_task` in the tail → `ErrInvalidDecision` (the tail case), and `PendingToolCalls` is NOT populated; (c) two `_spawn_task` calls in one response → `ErrInvalidDecision` (no SpawnTask emitted, no silent drop); (d) regression: a single `_finish` / single `_spawn_task` / single `_await_task` still translates correctly; (e) the guard fires identically with `parallel_tool_calls` ON and OFF.

### Drift / hygiene

- [ ] **AC-18** `docs/decisions.md` D-169 entry — pins the native-parallel cutover: the executor `CallParallel` branch, the `JoinKind`-collapses-to-`JoinAll`-on-native semantic, the non-atomic execution mode, the default flip, the `parallel.Executor`-reuse (no second dispatcher), AND the carried-over 107c fix (reserved planner-control names are standalone — co-occurrence with another tool-call is `ErrInvalidDecision`, closing the silent tail-drop; AC-21). References D-167, D-056, brief 15 §6.
- [ ] **AC-19** Glossary: update `ParallelExecutor` (it now HAS a production consumer — the dev `ToolExecutor`; note the non-atomic mode), `JoinAll` (note it is the only native-path join), `RunContext.PendingToolCalls` (note it is now the opt-out / discovery-race path, not the default). New entry `parallel_tool_calls` (the operator-yaml knob).
- [ ] **AC-20** `docs/skills/add-an-in-process-tool/SKILL.md` + `docs/skills/drive-the-playground/SKILL.md` — per CLAUDE.md §18 same-PR drift: note that an agent's tools can now be invoked in parallel within one turn (the LLM may call several at once; the runtime dispatches them concurrently), and the `parallel_tool_calls` knob. Keep operator-facing and minimal.

## Files added or changed

### Runtime — dev executor + bootstrap

- `cmd/harbor/cmd_dev_executor.go` — AC-1 + AC-2 + AC-3 + AC-4: replace the `CallParallel` reject with `parallel.Executor` dispatch + per-branch `projectForLLM` + aggregate assembly. ~120 LOC.
- `cmd/harbor/cmd_dev.go` — construct the `*parallel.Executor` in `bootDevStack`; plumb `planner.react.parallel_tool_calls` into the React `Option`. ~20 LOC.
- `cmd/harbor/cmd_dev_executor_preview_test.go` (or a new `cmd_dev_executor_parallel_test.go`) — AC-13. ~180 LOC.

### Runtime — parallel executor

- `internal/runtime/parallel/parallel.go` — AC-7 non-atomic mode (per-call option; the setup-validation loop branches on it). ~40 LOC.
- `internal/runtime/parallel/parallel_test.go` — AC-14. ~80 LOC.

### Runtime — React planner

- `internal/planner/react/projector.go` — AC-8: native `CallParallel` emission gated on the knob; 107c serialization preserved on the OFF path. AC-21: pre-switch guard rejecting reserved-control-name co-occurrence with `ErrInvalidDecision` (carried-over 107c silent tail-drop fix). ~55 LOC.
- `internal/planner/react/react.go` — AC-11: `WithParallelToolCalls` Option + the field the projector reads (per-run, threaded via the existing builder path — D-025). ~25 LOC.
- `internal/planner/react/prompt.go` — AC-9: `renderNativeStepPair` `CallParallel` case (one assistant message, N `RoleTool` messages). ~70 LOC.
- `internal/planner/react/projector_test.go` — AC-12. ~80 LOC.
- `internal/planner/react/prompt_test.go` — AC-15 (+ golden update if pinned). ~60 LOC.
- `internal/planner/react/integration_test.go` — AC-16. ~120 LOC.
- `internal/planner/react/concurrent_test.go` — AC-17 (or in cmd/harbor). ~60 LOC.
- `internal/planner/react/testdata/golden_default_prompt.txt` — only if the parallel replay shape is golden-pinned (likely unaffected — the golden is the system prompt, not a trajectory replay).

### Runtime — config

- `internal/config/config.go` — AC-11 `parallel_tool_calls` field. ~10 LOC.
- `internal/config/validate.go` — AC-11 (bool; no enum reject needed — confirm the field round-trips). ~5 LOC.

### Docs

- `docs/decisions.md` — AC-18 D-169 entry. ~30 lines.
- `docs/glossary.md` — AC-19 updates + `parallel_tool_calls`. ~6 lines.
- `docs/skills/add-an-in-process-tool/SKILL.md` — AC-20 prose. ~8 lines.
- `docs/skills/drive-the-playground/SKILL.md` — AC-20 prose. ~6 lines.
- `examples/dev.yaml` (or equivalent) — illustrate `planner.react.parallel_tool_calls`. ~3 lines.

### Smoke

- `scripts/smoke/phase-107d.sh` — **NEW**. PREFLIGHT_REQUIRES: live-server. Static: dev executor references `parallel.Executor` (the reject is gone). Live: a task whose query elicits multiple tool-calls in one turn → assert ≥2 tool-call events between consecutive assistant turns + `task.completed`. SKIP on no provider key. ~120 LOC.

## Public API surface

- `parallel.Executor` non-atomic execution mode — new per-call option (signature TBD by implementer; pinned by AC-14's test). The atomic default is unchanged for existing callers.
- `react.WithParallelToolCalls(bool) Option` — new planner option; default `true`.
- `config`: `planner.react.parallel_tool_calls bool` — new optional yaml field, default `true`.
- No new `Decision` shapes, no Protocol-surface change, no new LLM wire types — `CallParallel`, `JoinSpec`, `ToolCallStructured`, `ChatMessage.ToolCallID` all already exist.

## Test plan

### Unit (Go)

- `internal/planner/react/projector_test.go` — AC-12 (native vs serialization emission) + AC-22 (reserved-name co-occurrence rejected; carried-over 107c fix).
- `cmd/harbor/cmd_dev_executor_*_test.go` — AC-13 (mixed success/failure, bad-args non-atomic, heavy-output projection).
- `internal/runtime/parallel/parallel_test.go` — AC-14 (non-atomic vs atomic setup).
- `internal/planner/react/prompt_test.go` — AC-15 (N `RoleTool` round-trip).

### Integration (Go)

- `internal/planner/react/integration_test.go::TestReactPlanner_NativeToolCall_ParallelCalls` — AC-16: real `parallel.Executor` over an in-mem catalog, identity propagation across all branches, next-turn N-`RoleTool` prompt. Real driver on the seam, ≥1 failure mode (one failing branch), `-race`.

### Conformance

- N/A — the `Decision` sum and identity contract are unchanged; the Phase 49 conformance pack stays green (verify `go test -race ./internal/planner/conformance/...` before ship).

### Concurrency / leak

- `internal/planner/react/concurrent_test.go` (or cmd/harbor sibling) — AC-17: N≥100 concurrent `CallParallel` dispatches against one shared dev executor + one shared `parallel.Executor`, `-race`, baseline goroutine restore.

## Smoke script additions

`scripts/smoke/phase-107d.sh` — PREFLIGHT_REQUIRES: live-server. Assertions:

1. SKIP when no LLM provider key.
2. Static: `cmd/harbor/cmd_dev_executor.go` references `parallel.Executor` / no longer returns `ErrDecisionShapeUnsupported` for `CallParallel`.
3. Bootstrap a dev token (Phase 105 endpoint).
4. POST `/v1/control/start` with a query that naturally elicits several independent tool-calls in one turn (e.g. "fetch the metadata for these three URLs").
5. Subscribe to `/v1/events/subscribe`; assert ≥2 `tool.invoked` events fire for one assistant turn (parallel dispatch, not one-per-turn serialization).
6. Wait for `task.completed`; fetch `tasks.get`; assert the trajectory has a step whose action is a parallel call (≥2 branches) with one observation per branch.
7. Assert no `ErrContextLeak` in the server log (per-branch heavy-output projection held).

## Coverage target

- `cmd/harbor/`: maintain existing target (the dev executor is the main new surface).
- `internal/runtime/parallel/`: 85% (existing — D-056 baseline; the non-atomic branch is covered by AC-14).
- `internal/planner/react/`: 85% (existing — projector + prompt round-trip lift coverage).
- `internal/config/`: 80% (existing).

## Dependencies

- 107c — native tool-calling cutover (D-167). This phase consumes `ToolCallStructured`, `ChatMessage.ToolCallID`, `renderNativeStepPair`, and the `req.ParallelToolCalls = true` request the projector already sets. **Hard dependency — must land first** (it has).
- 47 — parallel emission + the `internal/runtime/parallel.Executor` (D-056). This phase is that executor's first production consumer (closes the §13 primitive-without-consumer gap the executor has carried since Phase 47).
- 83i — the dev `ToolExecutor` seam (D-152). This phase extends `ExecuteDecision`'s switch.

## Risks / open questions

- **The trajectory round-trip is the load-bearing new code.** Today the prompt builder assumes one `CallTool` → one assistant-tool-call + one `RoleTool`. The N-call case (AC-9) must emit exactly one assistant message with N `tool_calls` and N matching `RoleTool` messages — a malformed pairing (missing or duplicate `tool_call_id`) is a provider-reject on the next turn. The implementing agent MUST test the pairing invariant explicitly (AC-15) and, ideally, against a real provider (the smoke).
- **Pause mid-parallel stays fail-loud.** `ErrParallelPauseUnsupported` fires if a pause request lands mid-dispatch. Accepted as a documented limitation (Non-goals). If an operator workload hits it in practice, that's the trigger to pull the Phase 50 checkpoint-atomicity extension forward — file a follow-up, don't silently swallow.
- **Same-turn discovery race must still serialise.** 107c risk #4: if the LLM emits a `tool_search` AND a call to the not-yet-declared tool in one response, the provider would reject the second call. With native parallel ON, both calls would be emitted as one `CallParallel` — but the second tool isn't declared, so the provider rejects *before* Harbor sees it. Verify the two-turn discovery cycle (107c AC-26) still holds with parallel ON; if a real provider tolerates the same-turn case oddly, the projector may need to keep `tool_search` calls out of a parallel batch. Pause-and-ask if the discovery test regresses.
- **Non-atomic mode signature.** AC-7 leaves the exact API (variadic option vs explicit method) to the implementer. Whichever is chosen, the atomic default MUST be preserved byte-for-byte for the existing (test-only, today) callers — the D-056 atomicity contract is still the right default for programmatic planners. Pin both modes with tests.
- **Aggregate-observation shape vs the Console.** The Console renders tool-call cards from the trajectory on `task.completed`. A `CallParallel` step's aggregate observation is a new shape; verify the existing Console trajectory renderer degrades gracefully (renders N cards or one grouped card) — it reads canonical Protocol state, so the shape must be representable. If the Protocol's trajectory projection assumed one tool per step, that's a follow-up (flag it; don't expand scope here).
- **Reserved-name reject vs. a future batch-spawn.** AC-21 rejects "N `_spawn_task` calls in one turn." That is deliberate for V1.1.x: `SpawnTask` / `AwaitTask` have NO executor consumer yet (the dev `ToolExecutor` still returns `ErrDecisionShapeUnsupported` for both — `cmd/harbor/cmd_dev_executor.go:103-108`; this phase leaves that reject unchanged per AC-1). Same-turn parallel background-spawn is also not a correctness need — background tasks run concurrently *once spawned*, so a planner can spawn them one-per-step across steps and still get concurrent execution. If a future phase wants one-turn batch spawn, the shape is a dedicated batch meta-tool (e.g. `_spawn_tasks` taking an array → one `SpawnTask` carrying N specs), NOT reserved names as `CallParallel` branches — `CallParallel.Branches` is `[]CallTool` (tool invocations), and `SpawnTask`/`AwaitTask` are distinct `Decision` shapes. AC-21 keeps that door open by failing loudly today rather than silently honouring one of N. **Open question for whoever wires the spawn/await dev-executor consumer (currently an unowned "post-V1.1" code comment, no numbered phase — Phase 47 shipped the runtime machinery + emission, but the dev `ToolExecutor` seam from Phase 83i never connected to it):** decide there whether single-spawn-per-turn is sufficient or a batch meta-tool is warranted.
- **Default flip blast radius.** Flipping `parallel_tool_calls` to `true` by default changes runtime behaviour for every existing agent yaml the moment they emit multiple tool-calls. The serialization path stays available (`false`); the smoke + live test must confirm the default path is correct before merge. If risk-averse, the implementer may stage the flip (ship the branch with default `false`, flip in a fast-follow) — but D-167 §2 promised the flip, so default `true` is the intended end state.

## Glossary additions

- **`parallel_tool_calls`** — Phase 107d operator-yaml knob at `planner.react.parallel_tool_calls` (bool, default `true`). `true`: the React planner emits a native `planner.CallParallel` when the LLM returns N>1 tool-calls in one response, and the dev `ToolExecutor` dispatches the branches concurrently via `internal/runtime/parallel.Executor`. `false`: the 107c serialization fallback (`RunContext.PendingToolCalls`) fires instead — one `CallTool` per step. (Alphabetised under P.)
- Updates (not new): `ParallelExecutor` (now has a production consumer — the dev `ToolExecutor`; gains a non-atomic per-call mode where a branch's resolve/validate failure is a per-branch `Result.Err` instead of a whole-call abort), `JoinAll` (the ONLY join kind the native React path emits; the others are programmatic-planner surface), `RunContext.PendingToolCalls` (now the opt-out + same-turn-discovery-race guard, not the default for N>1).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation: identity propagates to EVERY parallel branch (AC-16 asserts this); a cross-session test confirms one session's branches never read another's identity
- [ ] **Concurrent-reuse — AC-17 (N≥100 `CallParallel` dispatches against one shared dev executor + one shared `parallel.Executor`, `-race`, goroutine baseline restored).** The dispatcher's own D-025 test (Phase 47) stays green; this adds the merge-layer reuse test.
- [ ] **Integration test — AC-16 (real `parallel.Executor` on the seam, identity propagation, ≥1 failing branch, `-race`).**
- [ ] **Carried-over 107c fix — AC-21 + AC-22 (reserved planner-control name co-occurring with another tool-call → `ErrInvalidDecision`, no silent tail-drop; head AND tail positions; both `parallel_tool_calls` ON and OFF).**
- [ ] Glossary updated per AC-19
- [ ] `docs/decisions.md` D-169 entry per AC-18
- [ ] Both skills updated per AC-20 (CLAUDE.md §18 same-PR drift)
- [ ] `examples/dev.yaml` shows `parallel_tool_calls`
- [ ] Phase 49 conformance pack green (`go test -race ./internal/planner/conformance/...`)
- [ ] Live smoke against a tool-calling-capable provider confirms concurrent dispatch + no `ErrContextLeak`
