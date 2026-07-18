# Research Brief 16 — Parallel intent, task management, and the standalone-spawn fossil

Status: research / pre-RFC. Mines three mature open-source coding agents (nanocoder — TypeScript CLI agent on the Vercel AI SDK; tura — Rust-core agent runtime; opencode — TypeScript coding agent) for how they model and execute multiple actions per model turn, then grounds the transferable patterns against Harbor's sealed `Decision` sum. Informs the v1.16 parallel-intent phases: the `Batch` decision, the AC-21 supersession, the task-management planner meta-tools, and the cancel hierarchy. A separate grounding pass (§6) settles what a spawned child run inherits from its parent.

## 1. Why AC-21's standalone rule is a fossil

Harbor's ReAct projector rejects any planner-control meta-tool (`_finish` / `_spawn_task` / `_await_task`) co-occurring with any other tool call in one native response (`internal/planner/react/projector.go:68-83`, AC-21). The rule was correct for its era: it closed a silent-tail-drop defect in the one-JSON-action-per-step format, where "one response = one intent" was the whole model. Native tool calling changed the primitive — a response is a SET of calls, each with its own `call_id`, each expecting a keyed result. The projector already embraces that for catalog tools (N calls → `CallParallel` with a `JoinSpec`, AC-8); AC-21 is the one place still enforcing the old one-intent world, and models trained on "send a single message with multiple tool calls" conventions trip it in production (observed live: a model batching `_spawn_task` with a catalog tool fails the run at step 0 with `ErrInvalidDecision`).

## 2. Cross-agent patterns (the mined evidence)

Four axes, three codebases:

**a) Typed decision model — universally absent, and Harbor should NOT converge.** nanocoder's turn is an untyped `Message{tool_calls: ToolCall[]}` reclassified post hoc by string name (`tool-executor.tsx:200-207`). opencode's turn is an ordered `ContentPart[]` union where a `task` (spawn) call and a `bash` call are the SAME `ToolCallPart` shape, differentiated only by `name` at settle time (`registry.ts:50-61`). tura goes the other way entirely: it collapses the tool surface into ONE model-visible macro-tool (`command_run`) whose argument is a batch array with a per-item `step` dependency tag — heterogeneity lives inside one call's JSON payload. All three get away with stringly dispatch because they are single-process, non-durable, best-effort loops with no identity isolation, no durable task registry, no cross-process resume. Harbor's sealed sum exists precisely for compile-time exhaustive switching, trajectory audit, and re-hydration — keep it.

**b) Spawn mixed with tools — opencode allows AND prompts for it; nanocoder allows it incidentally; tura never wired it.** opencode dispatches its `task` (subagent-spawn) tool through the exact same `ToolRegistry.settle` / `FiberSet.run` path as `bash`/`read`/`grep` — no special-casing — and its system prompts actively instruct co-mingling: "if you need to launch multiple agents in parallel, send a single message with multiple Task tool calls" (`anthropic.txt:84`, `task.txt:13`). nanocoder never forbids its `agent` spawn tool from co-occurring with file tools; its heterogeneous batch is adjacency-grouped (`[read, read, agent, agent, write] → [[read,read],[agent,agent],[write]]`, `groupForParallelExecution`, `tool-executor.tsx:209-247`) and executed group-by-group. tura's child-agent dispatch is architecturally isolated (CLI subprocess, different transport) and has zero production call sites in the mined snapshot. No AC-21-equivalent rule exists in any of the three.

**c) Blocking vs non-blocking spawn — the axis where Harbor is already ahead.** nanocoder fuses spawn+await into one blocking call (the turn blocks on `Promise.allSettled` over every spawned agent, `tool-executor.tsx:339-341`; no model-visible handle). tura's two spawn primitives are synchronous barriers (`std::thread::spawn` + `.join()`, or a blocking subprocess wait). Only opencode has a true non-blocking mode: `background=true` forks the subagent into a registry-owned scope outside the per-turn fiber set, returns a running-state result with an id almost immediately, and later wakes the parent by INJECTING A MESSAGE into the parent session's stream (`task.ts:202-240`) — a side effect, not a typed completion. None of the three matches Harbor's `SpawnTask(RetainTurn=false)` + `WatchGroup`/`GroupCompletion` typed wake with per-member outcomes. Do not regress toward the weaker fused model; DO adopt opencode's insight that a conversational wake signal composes well on top of the typed one.

**d) Per-branch error isolation — unanimous.** All three convert a failed branch into a value, never letting it poison the join: nanocoder catches per-call and uses `allSettled` for spawn batches; tura returns `CommandRunItemResult{success,error}` per item and even converts a panicked `JoinHandle` into a synthetic failed result (`handler.rs:334-343`); opencode's `settleWith` converts `ToolFailure` into a normal error-typed result before it can fail the fiber (`registry.ts:69-82`). This is Harbor's existing `JoinSpec` philosophy — the `Batch` decision extends it unchanged: per-`call_id` error-as-value, never a batch-killing exception.

**e) Concurrency caps.** Only nanocoder enforces a hard cap on concurrent spawns (`MAX_CONCURRENT_AGENTS = 5`, synthetic error results for the excess, `subagent-executor.ts:42`, `tool-executor.tsx:271-285`). opencode has NO cap anywhere — model self-restraint plus a soft "up to 3" prompt suggestion. tura caps at the process level (`MAX_PLANNING_DEPTH`, `MAX_ACTIVE_RUNTIME_WORKERS`). For Harbor's fail-loud posture, a hard operator-configurable cap with whole-batch loud rejection (mirroring `CallParallel`'s atomic setup validation, `decision.go:60-62`) beats both per-excess truncation and uncapped-and-hope.

**f) How batching is taught to the model.** All three use natural-language tool descriptions / prompt text, never schema-level constraints (tura's `minItems: 5` structural nudge is the one exception). Harbor's current `_spawn_task` description ("Use to launch parallel work…", `discovered_tools.go:305-307`) actively INVITES the co-occurrence AC-21 rejects — the prompt and the validator disagree today, which is itself a defect independent of the redesign.

## 3. What transfers and what deliberately does not

**Transfers:** co-occurrence of spawn + tools in one response (strong precedent, safe once spawns are non-blocking); per-branch error-as-value; a hard spawn cap with loud whole-batch rejection; non-blocking spawn escaping the turn's join barrier (opencode's background mode validates Harbor's existing shape); the conversational wake-message on top of the typed completion.

**Does not transfer:** the untyped reclassify-by-name action model (incompatible with the sealed sum, trajectory typing, durable resume); opencode's "the tool handler privately decides sync/async" (Harbor's group semantics — `GroupID`, `RetainTurn`, `FailFast`, `WatchGroup` — are runtime-owned, never tool-private); blocking-fused spawn+await (weaker than what Harbor ships); tura's macro-tool `step` dependency tags (duplicates `JoinSpec`); uncapped parallelism on prompt-hope alone.

## 4. The `Batch` decision (the shape this brief recommends)

A fourth sealed shape, not a widening of `CallParallel` (whose `len(Branches)` is load-bearing for tool-count accounting — a spawn is explicitly NOT a tool invocation, `runloop.go:880-888`):

```go
type Batch struct {
    Tools  []CallTool   // catalog branches, joined per Join (nil → JoinAll, AC-5 parity)
    Spawns []SpawnTask  // always RetainTurn=false, auto-grouped when ≥2 and no explicit GroupID
    Join   *JoinSpec    // governs ONLY the Tools branches
}
```

Projection: partition native `ToolCalls` by name; `_finish` and `_await_task` KEEP the standalone guard (a terminal decision and a single-target block have no coherent multi-call semantics — and an await-in-batch would create a same-step data dependency on a sibling's not-yet-existing `task_id`); `_spawn_task` becomes batchable with tools and with other spawns; single-call fast paths unchanged; a degenerate one-branch Batch is never constructed (the projector prefers the plain shape — one representation per semantic). Executor: tool branches dispatch exactly as `CallParallel`; spawn branches register via the existing `ResolveOrCreateGroup` + `Spawn` path (auto-group, no new registry method); "spawn completion" = task registered (not finished — that is `WatchGroup`'s job); observation keyed by `call_id` with `RoleTool` replies reconstructed in the ORIGINAL `resp.ToolCalls` order (provider protocols require one result per `call_id`; Go map iteration must never determine reply order). `FailFast` disagreement across auto-grouped spawns fails the batch loud.

## 5. The cancel hierarchy and the task-management meta-tools

The task registry already carries the propagation primitive: `Task.PropagateOnCancel` ∈ {`cascade` (default — BFS through descendants), `isolate`} (`internal/tasks/tasks.go:35-41,134,216`). Nothing exposes it to the planner (`SpawnSpec` deliberately omits it, D-047), and nothing needs to change for the HUMAN side: `TaskRegistry.Cancel(ctx, id, reason)` (`tasks.go:419`) reaches any task directly regardless of propagation mode, each background task is a run with its own per-run steering inbox (`internal/runtime/steering/inbox.go:46`), and the TUI/Console already target individual tasks. The invariant to make explicit: **`isolate` detaches a task from its parent's cascade, never from direct operator control; a session-scoped operator cancel sweeps isolate-marked tasks too. There is no uncancellable task.**

The AGENT side is the gap: the planner's whole management surface is `SpawnTask` (fire), `AwaitTask` (block), `BackgroundResult` (typed outcome at resolution) — no mid-flight observation, no cancel. The model can launch work it cannot see or stop. Therefore, as a power-with-brake pairing (the §13 primitive-with-consumer rule read as governance): `propagate_on_cancel: isolate` becomes model-expressible ONLY in the same wave as `_task_status` (read progress of this run's own descendants) and `_cancel_task` (cancel an own descendant) — descendant-scoped, never arbitrary session tasks. These meta-tools earn their place independently: a model that fanned out four explorations and got its answer from the first should cancel the other three (JoinFirstSuccess economics under the model's own judgment).

Wake-with-a-message (from opencode, adapted to Harbor's substrate): group resolution keeps the typed `MemberOutcome` path for the planner AND emits a notification-class event (the existing `internal/runtime/notifications` subsystem — `notification.task_failed` et al.) that the TUI/Console render conversationally. The model gets structure; the operator gets narrative.

## 6. What a spawned child inherits from its parent (grounding pass)

A spawned child is a FULL run driven by the same machinery as its parent — one `RunLoopDriver` instance (`internal/runtime/serve/serve.go:463-470`, `DriveBackground: true`) drives both; the child's `RunID` is its spawned `TaskID` (`runloop.go:537-540`); `runOne` builds the child's `RunContext` and the SAME react planner + `defaultBuilder.baseRequest` prompt path serves both (`runloop.go:1236`, `react.go:599-608`, `prompt.go:151-152,201`). There is no separate child prompt path.

Inheritance, precisely:

| Field | Child behavior |
|---|---|
| Goal/Query | The planner-chosen `SpawnSpec.Query` (`runloop.go:1239-1240`) — never the parent's goal. |
| DiscoveredTools / RepairCounters / Trajectory | All FRESH (`runloop.go:1237-1260`, `:990`, `:1001`) — the parent's mid-run accumulations never carry over. |
| Identity | Same `(tenant, user, session)` triple; own `RunID`; `ParentTaskID` lineage on the task record (`dispatch.go:364`, `engine.go:229`). |
| LLMOverrides | RE-RESOLVED for the child (`runloop.go:632-668`) — deterministic layers (tenant/agent) match the parent; the one-shot session override is `Consume`d keyed by the identity triple (`overrides.go:221-236`), so a child can miss an override its parent already consumed. |
| Catalog / memory / skills / session artifacts | Freshly projected/fetched per run, session-scoped — normally identical content to the parent's view; session artifacts the parent produced BEFORE spawning ARE visible to the child. |

**Cache posture (consumed by brief 17 and the batch-executor phase):** a same-day child's prompt prefix is WARM through the static system sections (`<identity>` is deliberately date-only for KV-cache stability, `prompt.go:390-393`; the six static instructional sections; the durable base/user prompt layers) and usually the always-loaded tool catalog — and COLD from the first divergence: the discovered-tools tail (child starts empty), the repair-guidance tail (fresh counters), the goal turn, and the trajectory replay. For a no-discovered-tools, no-repair-guidance parent, the shared prefix extends to the start of the user-turn message.

**Depth vs breadth:** spawn depth is capped at dispatch (`toolExecutor.spawnTask`, `dispatch.go:352-357`) by `planner.absolute_max_spawn_depth` (default 4, `config.go:1694,1728`), walking `ParentTaskID` hops. The cap bounds DEPTH only — sibling spawns in one Batch share the parent's depth and are NOT mutually limited (`dispatch.go:95`), which is exactly why the Batch decision needs its own explicit breadth cap (`MaxBatchSpawns`) with whole-batch loud rejection.
