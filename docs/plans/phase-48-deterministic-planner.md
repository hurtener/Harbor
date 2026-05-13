# Phase 48 — Deterministic planner

## Summary

Land Harbor's second concrete `Planner` implementation under
`internal/planner/deterministic/`: a fully programmatic decision-tree
planner that emits `Decision` shapes without any LLM call. Phase 48 IS
the load-bearing validation of CLAUDE.md §1 property 3 ("the Planner is
swappable") — the same Runtime that executes the Phase 45 ReAct concrete
must execute the deterministic concrete with NO changes. Phase 48 ships
the `DeterministicPlanner` struct, its functional-options constructor
(`NewDeterministicPlanner(opts ...Option) (*DeterministicPlanner, error)`),
the `DecisionTreeStep` operator-configurable step abstraction, a
`WakePoll` declaration (D-032 — Phase 48 spec), and a `RunContext`-driven
walker that picks the active step per `Next` call and emits whatever
`Decision` the step computes from `rc` state. The planner also exercises
the wake-mode poll primitive: when a step is waiting on a `TaskGroup`,
the planner performs a non-blocking receive on the channel returned by
`tasks.TaskRegistry.WatchGroup`; not-ready → emit `AwaitTask`; ready →
consume the `MemberOutcome` slice and advance. The §13 primitive-with-
consumer policy demands at least one scenario emits `SpawnTask` then
`AwaitTask` for the same group, exercising the Phase 20/21 registry
surface end-to-end from the planner side; Phase 49's conformance pack
will validate ReAct + Deterministic against the same shared scenarios.

## RFC anchor

- RFC §6.2
- RFC §3.2
- RFC §11 Q-6

## Briefs informing this phase

- brief 02
- brief 05

## Brief findings incorporated

- **brief 02 §1 ("runtime↔planner separation").** "Harbor's biggest
  architectural lift is to define a small `Planner` interface from t=0
  and push every runtime concern off the planner and into the runtime
  itself, exposed to the planner only through a `RunContext` value. A
  ReAct planner is the first concrete; further concretes land in later
  phases without runtime changes." Phase 48 is the on-disk proof: the
  deterministic planner sits on the same `Planner` interface as Phase
  45's ReAct, reads the same `RunContext`, returns the same `Decision`
  sum. No `Planner` interface change. No `RunContext` change. No
  `Decision` shape change. The Runtime is unaware which concrete it
  drives — the load-bearing property under test.
- **brief 02 §2 ("Decision is a sum type; magic strings as `next_node`
  rejected").** "Runtime opcodes (parallel, spawn, await, pause, finish)
  are different shapes from tool calls. The predecessor's 'magic
  strings as `next_node`' pattern is rejected." Phase 48's
  `DecisionTreeStep` abstraction returns typed `Decision` values
  directly — there is no string-based control-flow opcode. Each step
  function evaluates `RunContext` state and returns one of the sealed
  six shapes (`CallTool`, `CallParallel`, `SpawnTask`, `AwaitTask`,
  `RequestPause`, `Finish`). The decision-tree walker is structural,
  not stringly typed.
- **brief 02 §5 ("Sharp edges — 70+ planner constructor parameters").**
  "Harbor's `Planner` interface has no constructor; concretes use
  functional options (`react.New(opts ...Opt) Planner`) and most knobs
  (token budget, hop budget, deadline, max_iters, cost cap, schema
  mode) move to runtime-level run options because they are not
  reasoning-policy concerns." Phase 48's
  `NewDeterministicPlanner(opts ...Option)` follows the same shape as
  Phase 45's `react.New`: small policy-shaped option set, runtime caps
  read from `RunContext.Budget`, no constructor explosion.
- **brief 02 §7 wake-mode paragraph (Phase 48 detail block).**
  "Deterministic ships the `poll` wake mode (D-032): each
  `Planner.Next` invocation reads its outstanding group's
  `GroupCompletion` via a non-blocking receive on the channel returned
  from `tasks.WatchGroup`. If the channel hasn't fired, the planner
  emits `AwaitTask` and the runtime sleeps the step until the next
  deterministic boundary; if it has fired, the planner reads the
  resolved `MemberOutcome` slice and proceeds." Phase 48 implements
  this verbatim. `DeterministicPlanner.WakeMode()` returns
  `planner.WakePoll`; the planner's group-aware step type performs the
  non-blocking receive against `tasks.TaskRegistry.WatchGroup` itself
  rather than relying on the runtime engine to push.
- **brief 02 §5 ("Thread-safety — separate planner instances per task
  is the predecessor's pattern; Harbor inverts").** Phase 48's
  `DeterministicPlanner` is a reusable artifact (D-025): the receiver
  is read-only after construction; per-call state lives on the stack
  and in `RunContext` + `ctx`. `internal/planner/deterministic/
  d025_test.go` pins N=128 concurrent `Next` invocations against one
  shared instance under `-race`.
- **brief 05 §1 ("Wake policy modes — planner-concrete concern, NOT a
  registry knob").** The TaskRegistry exposes ONE mechanism
  (`WatchGroup` + `GroupCompletion`); the three wake patterns (push /
  poll / hybrid) sit on top. Phase 48's deterministic planner is the
  on-disk proof that the registry's mode-neutral surface supports the
  `poll` pattern without registry changes — the registry has no
  knowledge that a poller is reading its channel non-blockingly.
- **brief 05 §1 ("retain-turn flag is the foreground-blocking
  primitive").** Phase 48 deliberately uses non-retain-turn SpawnTask
  shapes (`SpawnSpec.RetainTurn = false`) because the WakePoll mode is
  what proves the deterministic-planner side of the wake mechanism.
  Retain-turn shapes (which block foreground turn dispatch from the
  runtime side) don't exercise the planner-side wake — the runtime
  resumes them automatically.

## Findings I'm departing from (if any)

- **None.** Phase 48 is structurally aligned with both informing
  briefs. The departures relative to the master plan's Phase 48 detail
  block are zero: WakePoll, decision-tree step abstraction, conformance
  pack consumption — all match. The Phase 45 plan ships a forward
  reference for SpawnTask emission ("V1 prompt schema is intentionally
  narrow — SpawnTask emission deferred to a later concrete-planner
  upgrade"); Phase 48 is the FIRST concrete in the wave that emits
  SpawnTask + AwaitTask in scenarios. This is consistent with brief 02
  §7's wake-mode paragraph — no departure.

## Goals

- Ship `internal/planner/deterministic/` housing the
  `DeterministicPlanner` struct, its functional-options constructor
  (`NewDeterministicPlanner(opts ...Option) (*DeterministicPlanner,
  error)`), the `Next(ctx, rc) (Decision, error)` implementation, the
  `WakeMode()` declaration returning `planner.WakePoll` (D-032), and
  the `DecisionTreeStep` abstraction operators configure via
  `WithSteps(...)`.
- Ship a programmatic decision-tree model: each step exposes a
  `Decide(ctx, rc) (planner.Decision, bool, error)` method where the
  bool reports whether the step claimed the current call. The planner
  walks the configured steps in order per `Next` call; the first step
  that returns `(decision, true, nil)` wins. A step that returns
  `(nil, false, nil)` is "skip / not my turn" — the walker advances
  to the next step. A step that returns a non-nil error propagates
  loudly (§13 fail-loudly — no silent skip on error).
- Ship the `WakePoll` semantics: a `WatchGroupStep` step type performs
  a non-blocking receive on the `tasks.TaskRegistry.WatchGroup` channel
  for its tracked group. Not-ready → emit `AwaitTask{TaskID: <owner>}`;
  ready → invoke the operator-supplied `OnResolved(MemberOutcome[])`
  callback that decides the next deterministic decision (typically a
  `CallTool` or `Finish`).
- Ship a `SpawnAndAwaitStep` step type that emits `SpawnTask` on its
  first invocation and transitions to a watch-and-await state for
  subsequent invocations (the planner observes the spawn via
  per-`(SessionID, StepID)` state tracked on the receiver). This is
  the load-bearing scenario that closes the §13 primitive-with-
  consumer policy for Phase 48's side of the wave: SpawnTask +
  AwaitTask are emitted by a real concrete planner against a real
  `TaskRegistry`.
- Ship the conformance-pack integration: `internal/planner/
  deterministic/conformance_test.go` calls `conformance.Run(t,
  factory)` with a factory that constructs a deterministic planner
  whose first step is a `FinishStep` returning `Finish{Reason: Goal}`,
  declares `WakeMode: planner.WakePoll`, and provides a
  `RunContextFactory` so the Sanity subtest gets a populated quadruple.
- Ship the `CallToolStep` + `FinishStep` + `PauseStep` step types as
  the canonical in-package step constructors (Phase 49's conformance
  pack needs at least one of each Decision shape coming out of the
  deterministic planner to round-trip cross-planner scenarios).
- Ship the §13 primitive-with-consumer test
  (`spawn_await_scenario_test.go`): a scenario where the planner emits
  `SpawnTask` on the first `Next`, then `AwaitTask` while the group is
  outstanding, then `CallTool` once the group resolves with a
  `MemberOutcome` payload, then `Finish` on the next call. The test
  wires a real `tasks.TaskRegistry` (in-process driver) and a real
  `events.EventBus` (inmem driver), spawns a background task in the
  registry, resolves it, and asserts the planner advances through all
  four decision shapes.
- Ship the D-025 concurrent-reuse test: N=128 concurrent `Next` calls
  against ONE shared `*DeterministicPlanner` under `-race`. Each
  goroutine carries its own identity quadruple + scripted step set.
  The shared planner is read-only; per-call state lives on the stack
  and in `RunContext`. Identity-bleed detector: each call's decision
  carries the goroutine's RunID through to terminal output. Goroutine
  baseline restored.
- Ship the import-graph contract: the existing Phase 42
  `internal/planner/conformance/importgraph_test.go` walker covers the
  new package by construction (it walks the planner subtree). The
  smoke script asserts via grep.
- Coverage on `internal/planner/deterministic`: ≥ 85%.

## Non-goals

- No LLM call. The deterministic planner is purely programmatic. The
  planner's policy-shaped knob is the operator-supplied
  `DecisionTreeStep` set; there is no prompt builder, no LLM client,
  no retry / downgrade / corrections / safety / governance composition
  (that ladder is the LLM-edge concern Phase 45 consumes; the
  deterministic planner has no LLM edge to compose).
- No conformance-pack scenario implementations beyond Phase 42's
  skeleton. The conformance pack's filled scenarios (top-20 prompts,
  malformed-LLM salvage round-trips, parallel-call atomicity, wake-mode
  round-trip with a real `tasks.WatchGroup`, budget-aware finish,
  pause-payload bounds, steering drain-between-steps) all land in
  Phase 49.
- No runtime loop. Phase 48 ships the `Planner.Next` implementation;
  the runtime executor that calls `Next` in a loop and executes
  Decisions lands in the planner-runtime wiring phases (Phase 47+).
- No SpawnTask retain-turn variant scenarios. The `WakePoll` mode is
  only meaningful for non-retain-turn spawns; retain-turn spawns block
  the foreground turn and the runtime resumes them automatically
  without planner-side polling. The deterministic planner emits
  non-retain-turn spawns; retain-turn shapes are out of scope for the
  wake-mode validation.
- No `CallParallel` emission. The deterministic planner's V1 step
  abstraction supports `CallTool` / `SpawnTask` / `AwaitTask` /
  `RequestPause` / `Finish` — `CallParallel` lands when Phase 47's
  parallel executor exists (the deterministic planner's `Decide`
  signature accepts ANY Decision shape, so future Phase 47-aware step
  types are a non-breaking addition).

## Acceptance criteria

- [ ] `internal/planner/deterministic/` package exists with
  `deterministic.go`, `steps.go`, and the test files
  (`deterministic_test.go`, `steps_test.go`,
  `spawn_await_scenario_test.go`, `conformance_test.go`,
  `d025_test.go`).
- [ ] `DeterministicPlanner` is a struct constructed via
  `NewDeterministicPlanner(opts ...Option) (*DeterministicPlanner,
  error)`; it implements `planner.Planner` AND `planner.WakeAware`
  (returning `planner.WakePoll`).
- [ ] Functional options:
  - [ ] `WithSteps(steps ...DecisionTreeStep) Option` — sets the
    ordered step set the walker traverses on each `Next` call. At
    least one step is required at construction time; zero steps
    returns `ErrInvalidConfig` at `NewDeterministicPlanner` time
    (fail-loudly per §13, not at `Next` time).
  - [ ] `WithRegistry(reg tasks.TaskRegistry) Option` — sets the
    optional registry handle that group-aware steps poll. Required
    when any configured step is a `SpawnAndAwaitStep` or
    `WatchGroupStep`; the constructor validates this and rejects with
    `ErrInvalidConfig` if a group-aware step is configured without a
    registry.
  - [ ] `WithName(name string) Option` — optional human-readable
    identifier (audit + observability). Default: `"deterministic"`.
- [ ] `Next(ctx, rc)` flow:
  1. Honour `ctx.Err()` at entry; return verbatim if cancelled.
  2. Validate identity from `rc.Quadruple` — missing tenant / user /
     session / run components return wrapped
     `planner.ErrIdentityRequired` (§6 rule 9 + D-001).
  3. Observe `rc.Control.Cancelled` — return `Finish{Reason:
     Cancelled, Metadata["steering"]="cancelled"}` if set
     (step-boundary contract per RFC §6.3).
  4. Walk the configured step set in order. For each step:
     - call `Decide(ctx, rc)`;
     - on `(decision, true, nil)` → return the decision verbatim;
     - on `(nil, false, nil)` → advance to the next step;
     - on any non-nil error → return wrapped
       `planner.ErrDeterministicStep` (fail-loudly per §13).
  5. If no step claims the call, return `Finish{Reason: NoPath,
     Metadata["deterministic"]="no_step_matched"}` — fail-loudly per
     §13. A walker that exhausts every step without a match means the
     operator's tree is misconfigured for the current `RunContext`
     state; surface it, don't silently loop.
- [ ] `DecisionTreeStep` interface: `Decide(ctx, rc) (planner.Decision,
  bool, error)`. The interface is exported so operators can implement
  custom steps; in-package step types (`CallToolStep`, `FinishStep`,
  `PauseStep`, `SpawnAndAwaitStep`, `WatchGroupStep`) implement it.
- [ ] `CallToolStep` — operator-configured single-tool dispatch step.
  Fields: `Tool string`, `ArgsBuilder func(planner.RunContext)
  (json.RawMessage, error)`, `Reasoning string`,
  `When func(planner.RunContext) bool` (optional guard; nil → always
  match). On a match `Decide` returns the constructed `CallTool`
  decision with `(decision, true, nil)`. Args-builder error propagates
  via `planner.ErrDeterministicStep`.
- [ ] `FinishStep` — operator-configured terminal step. Fields:
  `Reason planner.FinishReason`, `PayloadBuilder
  func(planner.RunContext) (any, error)` (optional; nil → nil
  Payload), `MetadataBuilder func(planner.RunContext)
  (map[string]any, error)` (optional), `When
  func(planner.RunContext) bool` (optional guard).
- [ ] `PauseStep` — operator-configured pause-request step. Fields:
  `Reason planner.PauseReason`, `PayloadBuilder
  func(planner.RunContext) (map[string]any, error)` (optional),
  `When func(planner.RunContext) bool` (optional guard).
- [ ] `SpawnAndAwaitStep` — operator-configured spawn-then-await step.
  Fields: `Kind tasks.TaskKind`, `SpecBuilder func(planner.RunContext)
  (planner.SpawnSpec, error)`, `GroupID tasks.TaskGroupID` (optional;
  empty → ad-hoc per-step group), `OnResolved
  func(planner.RunContext, []tasks.MemberOutcome) (planner.Decision,
  error)`, `When func(planner.RunContext) bool` (optional guard).
  The step internally tracks per-`(SessionID, StepID)` state via a
  `sync.Map` on the step value — the receiver remains safe for
  concurrent reuse across runs (D-025).
- [ ] `WatchGroupStep` — operator-configured "I am waiting on this
  pre-existing group" step. Fields: `GroupID tasks.TaskGroupID`,
  `OwnerTaskID tasks.TaskID`, `OnResolved func(planner.RunContext,
  []tasks.MemberOutcome) (planner.Decision, error)`, `When
  func(planner.RunContext) bool` (optional guard). Performs the
  non-blocking receive on `WatchGroup`; not-ready → emits
  `AwaitTask{TaskID: OwnerTaskID}`; ready → invokes `OnResolved`.
- [ ] **Wake-mode contract.** `DeterministicPlanner.WakeMode()`
  returns `planner.WakePoll`. Compile-time assertion:
  `var _ planner.WakeAware = (*DeterministicPlanner)(nil)`. Runtime
  assertion (in `deterministic_test.go`):
  `planner.ResolveWakeMode(p) == planner.WakePoll`.
- [ ] **§13 primitive-with-consumer scenario test**
  (`TestE2E_Deterministic_SpawnAwaitResolveFinish`). The planner is
  configured with three steps:
  1. A `FinishStep` guarded by `When: <CallTool already emitted via
     trajectory>` returning `Finish{Reason: Goal, Payload: <task id>}`.
  2. A `SpawnAndAwaitStep` configured to spawn a background task into
     a fresh group; its `OnResolved` returns a `CallTool` decision
     whose args carry the resolved member outcome's task ID.
  3. A `FinishStep` (no guard) returning `Finish{Reason: NoPath,
     Metadata["deterministic"]="default"}` — the fallback for
     completeness.
  The test wires a real `tasks.TaskRegistry` + real `events.EventBus`.
  It invokes `Next` four times and asserts the decision sequence is
  `SpawnTask` → `AwaitTask` → `CallTool` → `Finish`, with the
  background task lifecycle driven by the test between calls.
- [ ] **D-025 concurrent-reuse test** (`d025_test.go`). N=128
  concurrent `Next` calls against ONE shared `*DeterministicPlanner`.
  Per-goroutine identity quadruple; the planner is configured with a
  single `FinishStep` whose `MetadataBuilder` stamps the current
  `RunID`. Asserts:
  - no races (race detector is the gate);
  - no identity bleed (each call's `Finish.Metadata["run_id"]` matches
    the goroutine's RunID);
  - no cancellation cross-talk (pre-cancelled ctx on i%5==0 returns
    ctx.Err() without affecting siblings);
  - no goroutine leak (baseline `runtime.NumGoroutine` restored
    within 500ms of WaitGroup join).
- [ ] **Conformance test** (`conformance_test.go`). Calls
  `conformance.Run(t, factory)` with a factory that constructs a
  deterministic planner whose first step is a `FinishStep` returning
  `Finish{Reason: Goal}` (so the Sanity subtest's `Next` always
  terminates cleanly). The factory declares `WakeMode:
  planner.WakePoll`. The `RunContextFactory` ships a populated
  identity quadruple so the planner's identity-mandatory check does
  not fail the Sanity subtest.
- [ ] **Import-graph contract.** No `internal/runtime/...` imports
  in `internal/planner/deterministic/`. The existing Phase 42 lint
  (`internal/planner/conformance/importgraph_test.go`) covers the
  new package automatically. The smoke script asserts via grep.
- [ ] `scripts/smoke/phase-48.sh` exists, is executable, runs
  `go test -race -count=1 -timeout 180s
  ./internal/planner/deterministic/...`, asserts the `WakePoll`
  declaration via grep, asserts the §13 import-graph contract, and
  asserts the deterministic planner emits each of `CallTool`,
  `SpawnTask`, `AwaitTask`, `Finish` via grep against the scenario
  test file (so Phase 49's conformance pack has cross-planner
  coverage).
- [ ] `docs/decisions.md` D-057 records:
  - the `DecisionTreeStep` interface shape (typed step abstraction
    over a sealed-sum `Decision` return; no magic-string opcodes);
  - the `WakePoll` semantics (non-blocking receive on
    `tasks.WatchGroup` from the planner side; emit `AwaitTask` on
    not-ready);
  - the role of the deterministic planner as the iface-validation
    lens (§1 property 3 — proves the seam is genuinely swappable);
  - the §13 primitive-with-consumer compliance (`SpawnTask` +
    `AwaitTask` emission in scenarios closes the policy for the
    deterministic-planner side; Phase 49 cross-validates).
- [ ] `docs/glossary.md` gains entries for `DeterministicPlanner`,
  `WakePoll`, and `DecisionTreeStep`.
- [ ] `docs/plans/README.md` Phase 48 row flips to `Shipped`.
- [ ] `README.md` Status table gains a Phase 48 row.
- [ ] Coverage on `internal/planner/deterministic`: ≥ 85%.

## Files added or changed

- `internal/planner/deterministic/deterministic.go` (new) —
  `DeterministicPlanner`, `Option`, `NewDeterministicPlanner`, `Next`,
  `WakeMode`, internal helpers.
- `internal/planner/deterministic/steps.go` (new) —
  `DecisionTreeStep` interface + the in-package step types
  (`CallToolStep`, `FinishStep`, `PauseStep`, `SpawnAndAwaitStep`,
  `WatchGroupStep`).
- `internal/planner/deterministic/deterministic_test.go` (new) —
  unit tests: constructor validation, identity-required,
  ctx-cancellation, steering-cancelled, no-step-matched fallthrough,
  step-error propagation, walker ordering, WakeMode declaration.
- `internal/planner/deterministic/steps_test.go` (new) — per-step
  unit tests: `CallToolStep`, `FinishStep`, `PauseStep`,
  `SpawnAndAwaitStep` first-call vs poll-call, `WatchGroupStep`
  not-ready / ready paths.
- `internal/planner/deterministic/spawn_await_scenario_test.go` (new)
  — the load-bearing §13 primitive-with-consumer test wiring a real
  `tasks.TaskRegistry` + real `events.EventBus`. Exercises
  SpawnTask → AwaitTask → CallTool → Finish round-trip.
- `internal/planner/deterministic/conformance_test.go` (new) — calls
  `conformance.Run` with the deterministic factory; declares
  `WakeMode: WakePoll`.
- `internal/planner/deterministic/d025_test.go` (new) — N=128
  concurrent reuse stress.
- `internal/planner/errors.go` (modified) — add
  `ErrIdentityRequired`, `ErrInvalidConfig`, and
  `ErrDeterministicStep` sentinels (planner-level so future
  concretes consume them).
- `scripts/smoke/phase-48.sh` (new) — assertions per "Smoke script
  additions" below.
- `docs/plans/phase-48-deterministic-planner.md` (this file).
- `docs/plans/README.md` (modified) — Phase 48 row → `Shipped`.
- `docs/decisions.md` (modified) — D-057 entry.
- `docs/glossary.md` (modified) — `DeterministicPlanner`,
  `WakePoll`, `DecisionTreeStep` entries.
- `README.md` (modified — Status table gains Phase 48 row).

## Public API surface

```go
package deterministic

import (
    "context"
    "encoding/json"

    "github.com/hurtener/Harbor/internal/planner"
    "github.com/hurtener/Harbor/internal/tasks"
)

// DeterministicPlanner is Harbor's second concrete Planner (Phase 48).
// Programmatic decision tree; no LLM. Reusable artifact (D-025).
type DeterministicPlanner struct { /* ... */ }

// Option configures a DeterministicPlanner at construction time.
type Option func(*config)

// NewDeterministicPlanner constructs a DeterministicPlanner. Returns
// planner.ErrInvalidConfig (wrapped) when the configured step set is
// empty or a group-aware step is configured without a registry.
func NewDeterministicPlanner(opts ...Option) (*DeterministicPlanner, error)

// Next implements planner.Planner.
func (p *DeterministicPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error)

// WakeMode declares the planner's wake-on-resolution strategy. The
// deterministic planner uses WakePoll (D-032 — Phase 48 spec).
func (p *DeterministicPlanner) WakeMode() planner.WakeMode

// Functional options.
func WithSteps(steps ...DecisionTreeStep) Option
func WithRegistry(reg tasks.TaskRegistry) Option
func WithName(name string) Option

// DecisionTreeStep is the operator-configurable step abstraction.
// Implementations return (decision, true, nil) on a match,
// (nil, false, nil) on a skip, or (nil, false, err) on a structural
// failure. The planner walks the configured step set in order per
// Next call; the first claiming step wins.
type DecisionTreeStep interface {
    Decide(ctx context.Context, rc planner.RunContext) (planner.Decision, bool, error)
}

// In-package step types.
type CallToolStep struct {
    Tool        string
    ArgsBuilder func(planner.RunContext) (json.RawMessage, error)
    Reasoning   string
    When        func(planner.RunContext) bool
}

type FinishStep struct {
    Reason          planner.FinishReason
    PayloadBuilder  func(planner.RunContext) (any, error)
    MetadataBuilder func(planner.RunContext) (map[string]any, error)
    When            func(planner.RunContext) bool
}

type PauseStep struct {
    Reason         planner.PauseReason
    PayloadBuilder func(planner.RunContext) (map[string]any, error)
    When           func(planner.RunContext) bool
}

type SpawnAndAwaitStep struct {
    StepID       string
    Kind         tasks.TaskKind
    SpecBuilder  func(planner.RunContext) (planner.SpawnSpec, error)
    GroupID      tasks.TaskGroupID
    OnResolved   func(planner.RunContext, []tasks.MemberOutcome) (planner.Decision, error)
    When         func(planner.RunContext) bool
}

type WatchGroupStep struct {
    GroupID     tasks.TaskGroupID
    OwnerTaskID tasks.TaskID
    OnResolved  func(planner.RunContext, []tasks.MemberOutcome) (planner.Decision, error)
    When        func(planner.RunContext) bool
}
```

```go
package planner

// New sentinel errors. Phase 48 adds these to errors.go so future
// concretes share the same surface.
var (
    ErrIdentityRequired    = errors.New("planner: identity required (tenant/user/session/run)")
    ErrInvalidConfig       = errors.New("planner: invalid configuration")
    ErrDeterministicStep   = errors.New("planner/deterministic: step returned error")
)
```

## Test plan

- **Unit:**
  - `deterministic_test.go::TestNew_RejectsEmptySteps` — zero steps
    returns `ErrInvalidConfig`.
  - `deterministic_test.go::TestNew_RejectsGroupAwareStepWithoutRegistry`
    — `SpawnAndAwaitStep` configured without `WithRegistry` returns
    `ErrInvalidConfig`.
  - `deterministic_test.go::TestNew_AcceptsBasicFinishStep` — single
    `FinishStep` constructs cleanly.
  - `deterministic_test.go::TestNext_RejectsMissingIdentity` — partial
    quadruple returns `ErrIdentityRequired`.
  - `deterministic_test.go::TestNext_HonoursCtxCancel` — pre-cancelled
    ctx returns ctx.Err().
  - `deterministic_test.go::TestNext_ObservesSteeringCancellation` —
    `rc.Control.Cancelled=true` returns `Finish{Cancelled}`.
  - `deterministic_test.go::TestNext_WalksStepsInOrder` — step set
    `[A_skip, B_match, C_skip]` returns B's decision.
  - `deterministic_test.go::TestNext_NoMatchReturnsFinishNoPath` —
    every step skips → `Finish{NoPath,
    Metadata["deterministic"]="no_step_matched"}`.
  - `deterministic_test.go::TestNext_StepErrorPropagates` — a step
    returning an error surfaces as `ErrDeterministicStep`.
  - `deterministic_test.go::TestWakeMode_DeclaresPoll` — `WakeMode()`
    returns `WakePoll`; `ResolveWakeMode(p) == WakePoll`.
  - `steps_test.go::TestCallToolStep_*` — emit on match, args-builder
    error path, optional `When` guard.
  - `steps_test.go::TestFinishStep_*` — payload/metadata builders,
    `When` guard, RunID round-trip via metadata.
  - `steps_test.go::TestPauseStep_*` — Reason validation, payload
    builder.
  - `steps_test.go::TestSpawnAndAwaitStep_FirstCallEmitsSpawn` — first
    invocation returns `SpawnTask{...}`; subsequent invocations
    return `AwaitTask{...}` while the group is outstanding.
  - `steps_test.go::TestWatchGroupStep_NotReadyEmitsAwait` —
    non-blocking receive on an open group returns immediately; step
    emits `AwaitTask`.
  - `steps_test.go::TestWatchGroupStep_ReadyInvokesOnResolved` —
    resolved-group `WatchGroup` channel triggers `OnResolved` and the
    returned decision flows through.
- **Integration:** `spawn_await_scenario_test.go` wires a real
  `tasks.TaskRegistry` (in-process driver) + real `events.EventBus`
  (inmem driver). Four-call scenario: SpawnTask → AwaitTask → CallTool
  → Finish. Asserts each Decision shape and the registry's group
  lifecycle observably transitions through `GroupCompleted`.
- **Conformance:** `conformance_test.go` calls `conformance.Run` with
  the deterministic factory + `WakeMode: WakePoll`. The skeleton
  scenarios pass / skip per Phase 42's design.
- **Concurrency / leak:** `d025_test.go` ships the N=128 stress per
  D-025. Per-goroutine identity quadruple; the planner is shared.

## Smoke script additions

`scripts/smoke/phase-48.sh`:

- Run `go test -race -count=1 -timeout 180s
  ./internal/planner/deterministic/...` → OK on pass / FAIL otherwise.
- Static guard: grep for `WakePoll` in
  `internal/planner/deterministic/deterministic.go` → FAIL if missing
  (Phase 48 spec wake-mode declaration).
- §13 import-graph guard for the deterministic package — no
  `internal/runtime` imports.
- §13 import-graph guard — no `internal/llm` imports (the
  deterministic planner has no LLM dependency by construction).
- Cross-planner coverage assertion: grep for `SpawnTask`, `AwaitTask`,
  `CallTool`, `Finish` in
  `internal/planner/deterministic/spawn_await_scenario_test.go` so
  Phase 49's conformance pack has cross-planner coverage of every
  emission shape.
- Skip the HTTP / Protocol surface stub — Phase 48 has no protocol
  surface yet.

## Coverage target

- `internal/planner/deterministic`: 85%.

## Dependencies

- 42 (planner interface + Decision sum + RunContext + WakeMode).
- 20 (TaskRegistry — the deterministic planner's `SpawnAndAwaitStep`
  and `WatchGroupStep` consume it).
- 21 (TaskGroup + WatchGroup — the `WakePoll` semantics non-blocking
  receive against the channel `WatchGroup` returns).
- 05 (EventBus — the scenario test wires a real bus to observe
  task-lifecycle events).

## Risks / open questions

- **`SpawnAndAwaitStep` per-step state is keyed by `(SessionID,
  StepID)`.** The step type tracks "have I emitted the spawn yet?" via
  a `sync.Map` on the step value. Concurrent reuse across sessions is
  safe (distinct keys); concurrent reuse within ONE session sharing
  the same step would race on the spawn emission. Mitigation: the
  step's contract is per-run (one spawn per `(session, run)`); the
  D-025 stress test uses per-goroutine identity quadruples so the
  pattern is covered. Documented in the step type's godoc.
- **The `WakePoll` non-blocking receive can starve when the group
  resolves between the receive and the `Next` return.** Mitigation:
  the deterministic planner is poll-driven by definition — the next
  `Next` call performs another non-blocking receive. The Phase 47
  parallel executor's eventual scheduling cadence determines how
  often `Next` is invoked; the planner does not own that cadence.
  This is the structural advantage of poll over push (no eager wake
  burden on the registry).
- **A step returning `(nil, false, nil)` is the canonical skip path.**
  The fallthrough invariant is "every step gets a chance, every step
  can refuse." Operators that want a hard halt return
  `(nil, false, err)`; operators that want to ALWAYS match install a
  bare `FinishStep` at the end of the step set. The `Finish{NoPath,
  Metadata["deterministic"]="no_step_matched"}` fallback exists so the
  planner never silently loops on a misconfigured tree — fail-loudly
  per §13.
- **`OnResolved` callbacks are operator-supplied closures.** Operator
  bugs could close over per-run state and cause cross-run bleed if
  shared across goroutines. Mitigation: documented in `OnResolved`'s
  godoc that the callback MUST be safe for concurrent calls (the
  planner is a reusable artifact; the same step instance receives N
  concurrent invocations). The D-025 test exercises this with N=128
  goroutines.

## Glossary additions

- `DeterministicPlanner` — Harbor's second concrete `Planner`
  implementation (`internal/planner/deterministic`). Phase 48 — the
  iface-validation lens that proves the `Planner` interface is
  genuinely swappable (CLAUDE.md §1 property 3 / RFC §6.2). D-057.
- `WakePoll` — the planner's wake-on-resolution strategy declared by
  the deterministic planner. Non-blocking receive on
  `tasks.WatchGroup` from the planner side; not-ready → emit
  `AwaitTask` and let the runtime sleep the step. D-032 + D-057.
- `DecisionTreeStep` — the operator-configurable step abstraction in
  the deterministic planner. Returns a typed `planner.Decision` (one
  of the sealed six shapes) per `Decide(ctx, rc)` call; no
  magic-string opcodes. D-057.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test
  passes — the D-025 stress + per-goroutine identity quadruple pinning
  in `d025_test.go` covers this.
- [ ] **If this phase builds a reusable artifact (engine, tool,
  planner, driver, redactor, client, catalog, etc.): concurrent-reuse
  test passes — N≥100 concurrent invocations against a single shared
  instance under `-race`, asserting no data races, no context bleed,
  no cancellation cross-talk, no goroutine leaks.** See CLAUDE.md §5 +
  §11 + D-025. The `DeterministicPlanner` IS a reusable artifact;
  `d025_test.go` ships the N=128 stress.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes
  a cross-subsystem seam: an integration test exists (in-package
  adapter test OR `test/integration/<topic>_test.go`), wires real
  drivers end-to-end, asserts identity propagation, covers ≥1 failure
  mode, and runs under `-race`.** See CLAUDE.md §17. Phase 48 consumes
  Phase 20 (TaskRegistry) + Phase 21 (TaskGroup / WatchGroup) + Phase
  05 (EventBus); `spawn_await_scenario_test.go` wires all three with
  real drivers and asserts the SpawnTask → AwaitTask → CallTool →
  Finish round-trip.
- [ ] If new vocabulary: glossary updated — YES (3 new entries).
- [ ] If a brief finding was departed from: justified above +
  decisions.md entry filed — D-057.
