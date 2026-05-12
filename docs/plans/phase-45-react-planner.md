# Phase 45 — Reference ReAct planner (minimum viable)

## Summary

Land Harbor's first concrete `Planner` implementation under
`internal/planner/react/`: an LLM-driven ReAct step loop that, on each
`Next(ctx, rc)` call, builds a prompt from the run's `RunContext`
(query, goal, prior trajectory steps, available tools), invokes the
`llm.LLMClient` (already wrapped with retry + downgrade + corrections +
safety + governance per D-043), parses the response through Phase 44's
`repair.RepairLoop` (salvage → schema repair → graceful failure →
multi-action salvage), and maps the parsed action to a single
`Decision`. The planner ships a JSON-only action format
(`{"tool":..., "args":..., "reasoning":...}` or
`{"tool":"_finish","args":{"answer":...}}`), single-tool-call-per-step
semantics (multi-action LLM responses collapse to the first action —
V1 minimum viable), the `WakePush` declaration (D-032 — Phase 45
spec), and a `MaxSteps` circuit breaker that fails loudly via the new
`planner.max_steps_exceeded` event when the LLM never returns Finish
within the configured trajectory step budget. Functional options
(`WithMaxSteps`, `WithRepairAttempts`, `WithMaxConsecutiveArgFailures`,
`WithArgFillEnabled`, `WithPromptBuilder`, `WithSystemPrompt`) cover
the small policy-shaped knobs RFC §6.2 spec'd as planner-state. The
planner is a reusable artifact (D-025): one instance is safe to share
across N concurrent goroutines; per-run state lives entirely in
`ctx` and `RunContext`.

## RFC anchor

- RFC §6.2
- RFC §3.2
- RFC §6.4
- RFC §6.5

## Briefs informing this phase

- brief 02
- brief 07

## Brief findings incorporated

- **brief 02 §7 ("Reference ReAct planner (minimum viable)").** "LLM
  call loop, JSON-only action format, tool selection, completion
  detection, single tool call per step. No parallel, no schema repair
  beyond a single retry. Smoke: 3-step reasoning task succeeds against
  a mock LLM." Phase 45 ships exactly this minimum: the LLM call loop,
  JSON-only action format, tool selection, completion detection, and
  single-tool-call-per-step. Schema repair is consumed via Phase 44's
  reusable `RepairLoop` (not re-implemented — §13 two-parallel-
  implementations ban); parallel execution is deferred to Phase 47
  (the master plan dependency `47 — Parallel-call execution + JoinSpec`
  declares `Deps: 45, 14`).
- **brief 02 §2 (Decision sum-type rejects "magic strings as
  `next_node`").** "`Decision` is a sum type. Runtime opcodes
  (parallel, spawn, await, pause, finish) are different shapes from
  tool calls. The predecessor's 'magic strings as `next_node`' pattern
  is rejected." Phase 45's prompt asks the LLM for `{"tool":
  "<name>", "args": {...}, "reasoning": "..."}`; completion is
  signalled with the reserved tool name `"_finish"` (which the planner
  detects at decision-mapping time and converts to
  `planner.Finish{Reason: planner.FinishGoal, Payload: <answer>}`).
  The reserved name is NOT a magic-string opcode in the Decision sum
  — it's a prompt-time convention the planner translates to the typed
  `Finish` shape BEFORE returning. The Decision sum stays sealed.
- **brief 02 §4 ("Pause-state serialisation that MUST FAIL LOUDLY").**
  "When a pause record is serialised, `tool_context` is wrapped in
  `try: json.loads(json.dumps(...)) except (TypeError, ValueError):
  return None`. It silently drops non-serialisable tool context on
  resume." Phase 45 inherits the fail-loudly principle for its own
  graceful-failure path: when `MaxSteps` is hit without a `Finish`,
  the planner emits `planner.max_steps_exceeded` event AND returns
  `Finish{Reason: NoPath, Metadata["max_steps_exceeded"]=true}`. No
  silent return. The emit + metadata pair makes the failure observable
  the same way Phase 44's `planner.repair_exhausted` makes repair-loop
  exhaustion observable.
- **brief 02 §5 ("Sharp edges in the reference implementation that
  Harbor must avoid — 70+ planner constructor parameters").**
  "Harbor's `Planner` interface has no constructor; concretes use
  functional options (`react.New(opts ...Opt) Planner`) and most
  knobs (token budget, hop budget, deadline, max_iters, cost cap,
  schema mode) move to runtime-level run options because they are not
  reasoning-policy concerns." Phase 45's `New(client, opts...)` ships
  six functional options for the genuinely policy-shaped knobs:
  `WithMaxSteps`, `WithRepairAttempts`,
  `WithMaxConsecutiveArgFailures`, `WithArgFillEnabled`,
  `WithPromptBuilder`, `WithSystemPrompt`. Token / cost / deadline /
  hop budget remain runtime-level (read via `RunContext.Budget`); the
  planner observes them, does not own them.
- **brief 02 §5 ("Thread-safety disclaimer — predecessor needs
  separate planner instances per task").** "`planner/react.py:228-231`
  says 'NOT thread-safe. Create separate planner instances per task.'
  Harbor's interface requires planners to be safe to use concurrently
  across runs (the runtime serialises within a run); statefulness
  keyed only by `RunID` is the pattern." Phase 45's `ReActPlanner` is
  a reusable artifact (D-025): the receiver is read-only after
  construction; per-call state lives entirely on the stack and in the
  `RunContext`. `internal/planner/react/d025_test.go` pins N=128
  concurrent `Next` invocations against one shared instance under
  `-race`.
- **brief 07 §2 (data flow trace) + §3 (parsing surface).** "The
  model returns plain text. The runtime owns extraction." Phase 45's
  loop follows the brief's data flow verbatim: `Trajectory →
  build_messages → LLMClient.Complete → parser → Decision`. The
  parser is Phase 44's `repair.ActionParser` (consumed via
  `repair.RepairLoop.Run`); no parallel parser implementation lives
  in `internal/planner/react/`.
- **brief 07 §5 ("the planner observes prior steps as assistant /
  user-rendered observations").** "Every prior step is rendered as
  `{role: assistant, content: json.dumps({next_node, args})}`
  followed by `{role: user, content: render_observation(...)}`. The
  'tool result' channel is just a user message; there is no
  provider-native tool-result role." Phase 45's default
  `promptBuilder` ports the shape: each completed `Trajectory.Step`
  renders as an assistant turn (the prior `CallTool` encoded as JSON)
  followed by a user turn (the rendered observation, preferring
  `LLMObservation` over raw `Observation` per D-026 heavy-content
  discipline).
- **brief 07 §10 ("the multi-action salvage is configurable but the
  V1 default is single tool call per step").** Phase 44's repair
  loop's multi-action salvage produces `CallParallel`; Phase 45's
  planner override post-processes a `CallParallel` from the loop to a
  single `CallTool` (the first branch) for V1. The brief's "queue the
  additional read-only tool calls for sequential execution without
  another LLM hop" is a Phase 47+ concern (parallel execution lands
  there). Recorded as D-051.
- **brief 02 §7 wake-mode paragraph (Phase 45 detail block).** "ReAct
  ships the `push` wake mode (D-032): a non-retain-turn `SpawnTask`
  returns control to the runtime; the runtime registers the planner
  against `tasks.WatchGroup`; on `GroupCompletion` the runtime
  re-invokes `Planner.Next` with the resolved `MemberOutcome` slice
  surfaced through `RunContext`." Phase 45's `ReActPlanner`
  implements `planner.WakeAware` returning `planner.WakePush`. The
  conformance pack's `WakeMode_Declared` subtest asserts
  `ResolveWakeMode(p) == WakePush` against this declaration.
  SpawnTask emission itself is deferred to a later planner phase (the
  V1 minimum-viable spec says "single tool call per step" — there is
  no SpawnTask emission path in Phase 45's prompt schema); the
  WakePush declaration is what the conformance pack gates on.

## Findings I'm departing from (if any)

- **brief 02 §2 sketches the LLM directly emitting `SpawnTask /
  AwaitTask / RequestPause` decisions.** Departed for Phase 45 V1
  minimum viable: the LLM emits only `CallTool` (with a reserved
  `_finish` tool name signalling completion). SpawnTask / AwaitTask /
  RequestPause decisions are NOT producible by the LLM in Phase 45;
  they are higher-level reasoning patterns deferred to later planner
  phases (Phase 47 ships `CallParallel` execution; Phase 50 ships the
  unified pause/resume primitive; SpawnTask emission would land in a
  later concrete planner that has the prompt-engineering surface to
  describe background tasks to the LLM). **Why:** the V1 spec says
  "single tool call per step" — the prompt schema is intentionally
  narrow. The conformance pack's wake-mode declaration is what binds
  ReAct to WakePush; the *emission* of SpawnTask is a future-phase
  add. Recorded as D-051.
- **brief 02 §6 lists multi-action salvage as the Phase 44 default
  ("If the LLM emitted several JSON objects in one response, queue
  the additional read-only tool calls for sequential execution").**
  Departed at the planner concrete level: when Phase 44's `RepairLoop`
  returns `planner.CallParallel` (multi-action salvage), Phase 45's
  ReAct loop downgrades to the FIRST `CallTool` and discards the rest
  for V1 minimum viable. **Why:** the master plan's `Phase 45` detail
  block reads "single tool call per step. No parallel, no schema
  repair beyond a single retry"; the parallel-execution primitive
  lands in Phase 47 (`Deps: 45, 14`). Until Phase 47's executor
  exists, returning `CallParallel` from Phase 45 would prematurely
  commit to a runtime path that has no executor. Discarded actions
  are NOT surfaced as fallback context to the next prompt at V1 (a
  forwarding-the-rejected-actions path would be a surface that has no
  test coverage until Phase 47 lands). Recorded as D-051. Phase 47
  will revisit this when the executor exists; the override lives in
  ONE method (`reduceToSingleAction`) so the unwind is a single-file
  refactor.
- **brief 02 §2 puts `MaxSteps` / `HopBudget` at runtime level only.**
  Followed in spirit, departed in mechanism: `Budget.HopBudget` IS
  the runtime-level cap, but Phase 45 ALSO ships a planner-side
  `MaxSteps` functional option as a circuit breaker against an LLM
  that never returns `_finish` even when the runtime hasn't enforced
  a hop cap. **Why:** the runtime's hop budget lands later in the
  planner-runtime wiring (Phase 47+). Until then, a defensive
  planner-side circuit breaker is the only thing standing between a
  buggy LLM mock and an infinite loop. The circuit breaker is the
  SOURCE of the `planner.max_steps_exceeded` emit — it cannot be
  silently elided. When the runtime hop-budget enforcement lands, the
  planner-side `MaxSteps` becomes a redundant defence in depth
  (preferred over a load-bearing single gate). The runtime's hop
  budget remains the authoritative gate; the planner's `MaxSteps` is
  the secondary one. Recorded as D-051.
- **brief 02 §2 envisions `RunContext.Trajectory.Background`
  populated by the runtime BEFORE the planner sees the resolved
  task.** Phase 45 ships the read path (the prompt builder consumes
  `RunContext.Trajectory.Background` if non-empty), but does NOT ship
  the WatchGroup wiring on the planner side (the runtime engine owns
  that — Phase 47+). The WakePush declaration is the contract Phase
  45 ships; the actual runtime hook lands when the engine grows the
  WatchGroup consumer. Recorded as D-051.

## Goals

- Ship `internal/planner/react/` housing the `ReActPlanner` struct,
  its functional-options constructor (`New(client llm.LLMClient, opts
  ...Option) *ReActPlanner`), the `Next(ctx, rc) (Decision, error)`
  implementation, and the `WakeMode()` declaration returning
  `planner.WakePush` (D-032).
- Ship the JSON-only action format prompt: the LLM is asked to emit
  `{"tool": "<name>", "args": {...}, "reasoning": "..."}` for tool
  calls and `{"tool": "_finish", "args": {"answer": "..."},
  "reasoning": "..."}` for completion. The prompt is built by a
  pluggable `PromptBuilder` (default implementation ships in-package;
  operators may inject their own via `WithPromptBuilder`).
- Ship the single-tool-call-per-step semantics: when the repair loop
  returns `CallParallel` (multi-action salvage), the planner reduces
  to the first `CallTool` (V1 minimum viable; Phase 47 will revisit
  when the parallel executor exists).
- Ship completion detection: when the parsed `CallTool.Tool ==
  "_finish"`, the planner returns `Finish{Reason: FinishGoal,
  Payload: <args.answer>, Metadata: {...}}` instead of dispatching
  the reserved tool name as a real tool call.
- Ship the `MaxSteps` circuit breaker: when the run's prior trajectory
  step count is ≥ `MaxSteps` at the start of `Next`, the planner
  emits `planner.max_steps_exceeded` AND returns `Finish{Reason:
  NoPath, Metadata["max_steps_exceeded"]=true}`. Fail-loudly per §13.
- Register `planner.max_steps_exceeded` in
  `internal/planner/events.go` with a typed `MaxStepsExceededPayload`
  (SafePayload) carrying `Identity`, `MaxSteps`, `StepsObserved`,
  `LastTool`, `OccurredAt`.
- Ship the conformance-pack integration: `internal/planner/react/
  conformance_test.go` calls `conformance.Run(t, factory)` with a
  factory that constructs a ReActPlanner backed by `llm/mock` and
  declares `WakeMode: WakePush`.
- Ship the 3-step scenario test (Phase 45 acceptance criterion): a
  scripted mock LLM returns three responses (`CallTool` → `CallTool`
  → `_finish`); the test exercises three successive `Next` calls,
  asserting each returns the expected Decision shape and the
  trajectory grows by one step between calls.
- Ship the D-025 concurrent-reuse test: `internal/planner/react/
  d025_test.go` runs N=128 concurrent `Next` invocations against one
  shared `*ReActPlanner` under `-race`, asserting no races, no
  identity bleed (each call's RunID round-trips out via
  `Finish.Metadata["run_id"]`), no cancellation cross-talk (cancelled
  ctx on i%5==0 returns ctx.Err() without affecting siblings), no
  goroutine leak (baseline `runtime.NumGoroutine` restored).
- Ship the import-graph contract: the `internal/planner/conformance/
  importgraph_test.go` walker (Phase 42) already covers
  `internal/planner/react/` by construction; the smoke script asserts
  no `internal/runtime/...` imports leak in.
- Coverage on `internal/planner/react`: ≥ 85%.

## Non-goals

- No SpawnTask / AwaitTask / RequestPause decision emission. The V1
  minimum-viable spec is "single tool call per step." Later planner
  phases (or a follow-up upgrade to ReAct) add the prompt-engineering
  surface for these decision shapes.
- No `CallParallel` emission. Multi-action salvage from Phase 44's
  loop is collapsed to the first action; the parallel execution
  primitive lands in Phase 47. The `reduceToSingleAction` method is
  the unwind point — Phase 47 deletes the override.
- No runtime loop. Phase 45 ships the `Planner.Next` implementation;
  the runtime executor that calls Next in a loop, executes Decisions,
  and threads observations back into the next prompt lands in the
  planner-runtime wiring phases (Phase 47+).
- No trajectory compression / summariser. The planner consumes
  `Trajectory.Summary` if present (the prompt builder reads it); the
  summariser that POPULATES `Trajectory.Summary` lands in Phase 46.
- No new LLM-edge wrappers. The planner consumes `llm.LLMClient`
  as-is; the retry / downgrade / corrections / safety / governance
  composition stays at the registry edge (D-043 — two-parallel-
  implementations ban).
- No conformance-pack scenarios beyond Phase 42's skeleton. The
  conformance pack's filled scenarios (top-20 prompts, malformed-LLM
  salvage round-trips, parallel-call atomicity, wake-mode round-trip
  with a real `tasks.WatchGroup`, budget-aware finish, pause-payload
  bounds, steering drain-between-steps) all land in Phase 49.

## Acceptance criteria

- [ ] `internal/planner/react/` package exists with `react.go`,
  `prompt.go`, and the test files (`react_test.go`,
  `prompt_test.go`, `conformance_test.go`, `d025_test.go`,
  `integration_test.go`).
- [ ] `ReActPlanner` is a struct constructed via `New(client
  llm.LLMClient, opts ...Option)`; it implements `planner.Planner`
  AND `planner.WakeAware` (returning `planner.WakePush`).
- [ ] Functional options:
  - [ ] `WithMaxSteps(n int) Option` — circuit-breaker cap on the
    trajectory step count. Default 12 (small enough to surface bugs
    quickly; large enough to leave 3-step scenarios headroom).
  - [ ] `WithRepairAttempts(n int) Option` — passed to
    `repair.Config.RepairAttempts`. Default 3.
  - [ ] `WithMaxConsecutiveArgFailures(n int) Option` — passed to
    `repair.Config.MaxConsecutiveArgFailures`. Default 2.
  - [ ] `WithArgFillEnabled(b bool) Option` — passed to
    `repair.Config.ArgFillEnabled`. Default true.
  - [ ] `WithPromptBuilder(b PromptBuilder) Option` — operator
    extension point per RFC §6.2. Default: in-package builder.
  - [ ] `WithSystemPrompt(s string) Option` — overrides the default
    system prompt. Empty value uses the default.
- [ ] `Next(ctx, rc)` flow:
  1. Honour `ctx.Err()` at entry; return verbatim if cancelled.
  2. Validate identity from `rc.Quadruple` — missing tenant / user /
     session / run components return wrapped
     `llm.ErrIdentityMissing`.
  3. Check the `MaxSteps` circuit breaker: if `rc.Trajectory != nil
     && len(rc.Trajectory.Steps) >= maxSteps`, emit
     `planner.max_steps_exceeded` and return `Finish{Reason: NoPath,
     Metadata["max_steps_exceeded"]=true}`. Fail-loudly.
  4. Observe `rc.Control.Cancelled` — return `Finish{Reason:
     Cancelled}` if set (steering observation at step boundary).
  5. Build the LLM request via the configured `PromptBuilder`.
  6. Call `repair.RepairLoop.Run(ctx, rc, client, req,
     validateTool)` to drive the salvage → repair → graceful-failure
     ladder.
  7. Map the returned `planner.Decision` to the planner's final
     `Decision`:
     - `Finish{NoPath}` from the loop (graceful-failure exhaustion)
       → propagate verbatim (`planner.repair_exhausted` already
       emitted by the loop).
     - `CallTool` with `Tool == "_finish"` → translate to
       `Finish{Reason: FinishGoal, Payload: <args.answer>,
       Metadata: {...}}` (completion detection).
     - `CallTool` with another tool name → return verbatim.
     - `CallParallel` (multi-action salvage) → reduce to the first
       `CallTool` (V1 single-tool-call-per-step; D-051).
- [ ] Default prompt builder shape:
  - System prompt: a short instruction asking for the JSON envelope,
    listing available tools (name + description) from
    `rc.Catalog.List()`, and naming `"_finish"` as the completion
    marker with the `{"answer": "..."}` shape.
  - User prompt: the run's `rc.Goal` (or `rc.Query` if Goal is
    empty).
  - For each completed `Trajectory.Step`: an assistant turn (the
    prior `CallTool` rendered as JSON) + a user turn (the rendered
    observation, preferring `LLMObservation` over raw `Observation`
    per D-026 heavy-content discipline).
- [ ] `planner.max_steps_exceeded` event registered in
  `internal/planner/events.go`. Typed `MaxStepsExceededPayload`
  (SafePayload — composes `events.SafeSealed`) carries `Identity`,
  `MaxSteps int`, `StepsObserved int`, `LastTool string`,
  `OccurredAt time.Time`. The emit is the load-bearing observability
  surface that makes the circuit-breaker not silent (§13).
- [ ] Identity-mandatory: a `Next` call with a partial quadruple
  returns wrapped `llm.ErrIdentityMissing`. The repair loop also
  enforces this defensively (Phase 44 contract); the planner's
  pre-check matches the LLM client's behaviour.
- [ ] **3-step reasoning scenario test**
  (`TestReact_ThreeStepScenario`). A scripted mock LLM emits:
  - call 1: `{"tool":"search","args":{"q":"foo"},"reasoning":"step1"}`
  - call 2: `{"tool":"summarize","args":{"text":"bar"},"reasoning":"step2"}`
  - call 3: `{"tool":"_finish","args":{"answer":"done"},"reasoning":"step3"}`
  The test issues three successive `Next` calls. After each
  non-terminal Next call the test appends a synthetic
  `Trajectory.Step` to the RunContext so the next prompt sees the
  prior step. Asserts the three Decisions are `CallTool{search}`,
  `CallTool{summarize}`, `Finish{Reason: FinishGoal, Payload:
  "done"}`.
- [ ] **D-025 concurrent-reuse test** (`d025_test.go`). N=128
  concurrent `Next` calls against ONE shared `*ReActPlanner`.
  Per-goroutine identity quadruple; per-goroutine LLM stub returning
  a scripted single-action response keyed by RunID. Asserts no
  races, no identity bleed (each call's
  `Finish.Metadata["run_id"]` matches the goroutine's RunID for
  finish-path goroutines), no cancellation cross-talk (pre-cancelled
  ctx on i%5==0 returns ctx.Err()), no goroutine leak (baseline
  `runtime.NumGoroutine` restored within 500ms of WaitGroup join).
- [ ] **Conformance test** (`conformance_test.go`). Calls
  `conformance.Run(t, factory)` with a factory that:
  - Constructs a `ReActPlanner` backed by `llm/mock` configured to
    return a clean `_finish` JSON envelope.
  - Declares `WakeMode: planner.WakePush`.
- [ ] **Integration test** (`integration_test.go`). Real
  `events.EventBus` (inmem driver) + a stub `llm.LLMClient` whose
  responses force the repair-exhausted path; asserts the loop
  returns `Finish{NoPath}` with `Metadata["repair_error"]` populated
  AND the bus observes `planner.repair_exhausted` carrying the
  planner's identity. Companion test: a non-empty trajectory plus
  `MaxSteps=1` exercises the planner-level
  `planner.max_steps_exceeded` path on the bus without an LLM call.
- [ ] **Import-graph contract.** No `internal/runtime/...` imports
  in `internal/planner/react/`. The existing Phase 42 lint
  (`internal/planner/conformance/importgraph_test.go`) covers the
  new files automatically. The smoke script asserts via grep.
- [ ] `scripts/smoke/phase-45.sh` exists, is executable, runs `go
  test -race -count=1 -timeout 180s ./internal/planner/react/...`,
  asserts the WakePush declaration via grep, asserts the
  `planner.max_steps_exceeded` event type appears in
  `internal/planner/events.go`, and re-asserts the import-graph
  contract.
- [ ] `docs/decisions.md` D-051 records: (a) JSON-only action format
  with `_finish` reserved tool name (NOT a magic-string opcode in
  the Decision sum), (b) single-tool-call-per-step + the
  multi-action salvage reduction, (c) `MaxSteps` circuit-breaker
  policy + `planner.max_steps_exceeded` event, (d) Phase 45 V1
  deferrals (SpawnTask / AwaitTask / RequestPause emission,
  multi-action fallback-context forwarding), (e) the WakePush
  declaration ships at Phase 45 (the emission of non-retain-turn
  SpawnTask lands in a later phase).
- [ ] `docs/glossary.md` gains entries for `ReActPlanner`,
  `MaxSteps` (planner), `planner.max_steps_exceeded`,
  `MaxStepsExceededPayload`, `_finish` reserved tool name, and the
  JSON action envelope.
- [ ] `docs/plans/README.md` Phase 45 row flips to `Shipped`.
- [ ] `README.md` Status table gains a Phase 45 row.
- [ ] Coverage on `internal/planner/react`: ≥ 85%.

## Files added or changed

- `internal/planner/react/react.go` (new) — `ReActPlanner`, `Option`,
  `New`, `Next`, `WakeMode`, internal helpers
  (`reduceToSingleAction`, `maybeFinish`).
- `internal/planner/react/prompt.go` (new) — `PromptBuilder` interface
  plus the `defaultBuilder` implementation.
- `internal/planner/react/react_test.go` (new) — unit tests:
  options/defaults, identity-required, ctx-cancellation,
  completion-detection, max-steps circuit breaker,
  parallel-reduction, 3-step scenario.
- `internal/planner/react/prompt_test.go` (new) — prompt builder
  unit tests (system-prompt shape, trajectory-step rendering,
  observation preference for `LLMObservation`).
- `internal/planner/react/d025_test.go` (new) — N=128 concurrent
  Next-call stress.
- `internal/planner/react/conformance_test.go` (new) — calls
  `conformance.Run` with the ReActPlanner factory.
- `internal/planner/react/integration_test.go` (new) — real-bus
  positive + negative cases.
- `internal/planner/events.go` (modified) — register
  `planner.max_steps_exceeded` + ship `MaxStepsExceededPayload`.
- `internal/planner/events_test.go` (modified) — assert the new type
  registered + the new payload is SafePayload.
- `scripts/smoke/phase-45.sh` (new) — assertions per "Smoke script
  additions" below.
- `docs/plans/phase-45-react-planner.md` (this file).
- `docs/plans/README.md` (modified) — Phase 45 row → `Shipped`.
- `docs/decisions.md` (modified) — D-051 entry.
- `docs/glossary.md` (modified) — new vocabulary entries.
- `README.md` (modified — Status table gains Phase 45 row).

## Public API surface

```go
package react

import (
    "context"

    "github.com/hurtener/Harbor/internal/llm"
    "github.com/hurtener/Harbor/internal/planner"
)

// ReActPlanner is Harbor's reference LLM-driven planner. Reusable
// artifact (D-025); per-call state lives in ctx + RunContext.
type ReActPlanner struct { /* ... */ }

// Option configures a ReActPlanner at construction time.
type Option func(*ReActPlanner)

// New constructs a ReActPlanner with the supplied LLM client and
// options. Nil client panics — composition error caught at boot.
func New(client llm.LLMClient, opts ...Option) *ReActPlanner

// Next implements planner.Planner. The planner reads from rc + ctx,
// never from the receiver, for run-specific data.
func (p *ReActPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error)

// WakeMode declares the planner's wake-on-resolution strategy. ReAct
// uses WakePush (D-032 — Phase 45 spec).
func (p *ReActPlanner) WakeMode() planner.WakeMode

// Functional options.
func WithMaxSteps(n int) Option
func WithRepairAttempts(n int) Option
func WithMaxConsecutiveArgFailures(n int) Option
func WithArgFillEnabled(b bool) Option
func WithPromptBuilder(b PromptBuilder) Option
func WithSystemPrompt(s string) Option

// PromptBuilder constructs the LLM CompleteRequest from a
// RunContext. Default implementation ships in-package; operators may
// inject their own per RFC §6.2 ("policy-shaped knobs").
type PromptBuilder interface {
    Build(rc planner.RunContext, systemPrompt string) llm.CompleteRequest
}

// FinishToolName is the reserved tool name the LLM emits to signal
// completion. The planner intercepts this BEFORE returning the
// Decision; "_finish" never reaches the runtime as a real tool call.
const FinishToolName = "_finish"

// DefaultMaxSteps is the planner-side circuit-breaker default.
const DefaultMaxSteps = 12
```

```go
package planner

// New planner-emitted event type. Phase 45 registers in events.go
// alongside planner.decision / planner.finish / planner.error /
// planner.repair_exhausted.
const EventTypePlannerMaxStepsExceeded events.EventType = "planner.max_steps_exceeded"

// MaxStepsExceededPayload is the typed payload for the
// planner.max_steps_exceeded event. SafePayload — operator-visible by
// construction.
type MaxStepsExceededPayload struct {
    events.SafeSealed
    Identity      identity.Quadruple
    MaxSteps      int
    StepsObserved int
    LastTool      string
    OccurredAt    time.Time
}
```

## Test plan

- **Unit:**
  - `react_test.go::TestNew_AppliesDefaults` — zero options →
    `DefaultMaxSteps`, `DefaultRepairAttempts`,
    `DefaultMaxConsecutiveArgFailures`, `ArgFillEnabled=true`,
    default builder + default system prompt.
  - `react_test.go::TestNew_PanicsOnNilClient` — `New(nil)` panics.
  - `react_test.go::TestNext_RejectsMissingIdentity` — partial
    quadruple returns wrapped `llm.ErrIdentityMissing`.
  - `react_test.go::TestNext_HonoursCtxCancel` — pre-cancelled ctx
    returns ctx.Err() before any LLM call.
  - `react_test.go::TestNext_ObservesSteeringCancellation` —
    `rc.Control.Cancelled=true` returns `Finish{Cancelled}`.
  - `react_test.go::TestNext_FinishToolNameMappedToFinishDecision`
    — `_finish` parsed action becomes `Finish{Reason: FinishGoal,
    Payload: <answer>}`.
  - `react_test.go::TestNext_ParallelReducesToFirstCallTool` —
    multi-action salvage from the repair loop collapses to the first
    `CallTool`; the rest are dropped (V1).
  - `react_test.go::TestNext_MaxStepsCircuitBreakerEmitsAndFinishes`
    — a RunContext with a Trajectory whose step count ≥ `MaxSteps`
    returns `Finish{NoPath, Metadata["max_steps_exceeded"]=true}`
    AND emits `planner.max_steps_exceeded`.
  - `react_test.go::TestNext_RepairExhaustionPropagatesFinish` —
    stub LLM emits malformed JSON; the repair loop returns
    `Finish{NoPath}`; the planner propagates verbatim (the
    `planner.repair_exhausted` event came from the loop, not the
    planner).
  - `react_test.go::TestReact_ThreeStepScenario` — the load-bearing
    acceptance criterion. Scripted mock LLM through three Next
    calls; asserts every Decision shape + the trajectory append
    between calls.
  - `prompt_test.go::TestDefaultBuilder_*` — system-prompt content
    listing tools, observation rendering, goal-vs-query preference,
    handling of empty trajectory.
- **Integration:** `integration_test.go` wires real
  `events.EventBus` (inmem driver). Two test scenarios: (a)
  repair-exhaustion path with bus assertion (the planner consumes
  `repair.RepairLoop` so the event comes from the loop's
  `gracefulFailure`); (b) `MaxSteps=1` + non-empty trajectory →
  `planner.max_steps_exceeded` event observed on the bus with
  correct identity.
- **Conformance:** `conformance_test.go` calls `conformance.Run`
  with the ReAct factory + `WakeMode: WakePush`. The skeleton
  scenarios pass / skip per Phase 42's design.
- **Concurrency / leak:** `d025_test.go` ships the N=128 stress per
  D-025. The mock LLM is per-goroutine; the planner is shared.

## Smoke script additions

`scripts/smoke/phase-45.sh`:

- Run `go test -race -count=1 -timeout 180s
  ./internal/planner/react/...` → OK on pass / FAIL otherwise.
- Static guard: grep for `WakePush` in
  `internal/planner/react/react.go` → FAIL if missing (Phase 45 spec
  wake-mode declaration).
- Event-registry assertion: grep for
  `EventTypePlannerMaxStepsExceeded` in
  `internal/planner/events.go` → FAIL on miss.
- §13 import-graph guard for the react package — no
  `internal/runtime` imports.
- Static guard: no `internal/llm/retry` import in
  `internal/planner/react/` (composition stays at the LLM registry
  edge — D-043 + D-050).
- Skip the HTTP / Protocol surface stub — Phase 45 has no protocol
  surface yet.

## Coverage target

- `internal/planner/react`: 85%.

## Dependencies

- 42 (planner interface + Decision sum + RunContext).
- 43 (Trajectory + fail-loudly Serialize — the planner reads the
  trajectory in its prompt builder).
- 44 (Schema repair pipeline — the planner consumes
  `repair.RepairLoop`).
- 32 (LLM client core — the planner calls `llm.LLMClient.Complete`).

## Risks / open questions

- **The `_finish` reserved tool name** could collide with an operator
  catalog tool of the same name. Mitigation: the leading underscore
  is a documented convention; future runtime catalog registration
  could reject `_`-prefixed tool names. Phase 45 ships the convention
  and a glossary entry; Phase 47+ tightens via catalog validation.
- **Multi-action salvage reduction discards LLM-emitted intent.** The
  rejected actions in a `CallParallel` from the repair loop carry
  useful context (the LLM thought multiple tools were applicable).
  V1 drops them; Phase 47 will revisit when the executor exists. The
  `reduceToSingleAction` method is the one place this lives —
  refactor is a single-file unwind.
- **`MaxSteps` is a planner-side count, not a token / cost budget.**
  A malicious LLM could emit valid `_finish` envelopes immediately
  and the counter never reaches the cap; conversely a planner that
  hits `MaxSteps` may have made progress but couldn't finish.
  Mitigation: the runtime hop / cost budget (Phase 47+) is the
  authoritative gate; `MaxSteps` is defence in depth.
- **The planner does NOT call the LLM when `MaxSteps` is hit.** This
  is intentional (no LLM call should burn after the circuit breaker
  fires); the test pins this with a stub that increments a counter
  on Complete.
- **WakePush declaration without an emission path.** The planner
  cannot emit SpawnTask in V1 (the prompt schema is "single tool
  call per step"). The WakePush declaration is still load-bearing —
  it binds ReAct to the conformance pack's wake-mode-round-trip
  subtest (Phase 49) so that when SpawnTask emission lands in a
  later phase, the binding is already in place.

## Glossary additions

- `ReActPlanner` — Harbor's reference LLM-driven planner concrete
  (`internal/planner/react`). Implements `planner.Planner` AND
  `planner.WakeAware` (returns `planner.WakePush` — D-032).
- `MaxSteps` (planner) — `ReActPlanner` functional-option cap on the
  observed trajectory step count. Default 12. Fail-loudly per §13.
- `planner.max_steps_exceeded` — planner-emitted event (Phase 45).
  Fires from the `ReActPlanner.Next` `MaxSteps` circuit breaker.
  D-051.
- `MaxStepsExceededPayload` — typed payload for the
  `planner.max_steps_exceeded` event. SafePayload. D-051.
- `_finish` reserved tool name — the LLM emits a JSON envelope with
  `"tool": "_finish"` to signal completion. NOT a magic-string
  opcode in the Decision sum — translated to `Finish` BEFORE return.
  D-051.
- JSON action envelope — the wire shape the LLM emits per ReAct
  step. D-051.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test
  passes — the D-025 stress + per-goroutine identity quadruple
  pinning in `d025_test.go` covers this.
- [ ] **If this phase builds a reusable artifact (engine, tool,
  planner, driver, redactor, client, catalog, etc.): concurrent-
  reuse test passes — N≥100 concurrent invocations against a single
  shared instance under `-race`, asserting no data races, no context
  bleed, no cancellation cross-talk, no goroutine leaks.** See
  AGENTS.md §5 + §11 + D-025. The `ReActPlanner` IS a reusable
  artifact; `d025_test.go` ships the N=128 stress.
- [ ] **If this phase consumes a shipped subsystem's surface OR
  closes a cross-subsystem seam: an integration test exists
  (in-package adapter test OR `test/integration/<topic>_test.go`),
  wires real drivers end-to-end, asserts identity propagation,
  covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md
  §17. Phase 45 consumes `internal/llm` (Phase 32),
  `internal/planner/trajectory` (Phase 43), and
  `internal/planner/repair` (Phase 44); `integration_test.go` wires
  a real `events.EventBus` (inmem) + a stub `LLMClient` (the stub is
  the controlled test fixture at the LLM boundary, mirroring the
  Phase 44 integration pattern).
- [ ] If new vocabulary: glossary updated — YES (6 new entries).
- [ ] If a brief finding was departed from: justified above +
  decisions.md entry filed — D-051.
