# Phase 53 — steering-wiring

## Summary

Phase 53 wires the Phase 52 steering inbox and the Phase 50 pause/resume
Coordinator into a real per-run planner loop. It ships `RunLoop` — the
runtime component that calls `Planner.Next` between step boundaries, drains
the per-run `steering.Inbox` exactly once per step, applies the nine control
events as side effects (CANCEL hard/soft, PAUSE/RESUME/APPROVE/REJECT onto the
unified `pauseresume.Coordinator`, INJECT_CONTEXT/REDIRECT/USER_MESSAGE/PRIORITIZE
projected onto `RunContext.Control` + the task), and caps control-history per
session. It is the §13 first consumer for BOTH Wave-9 primitives: the Phase 50
Coordinator and the Phase 52 inbox/taxonomy.

## RFC anchor

- RFC §6.3

## Briefs informing this phase

- brief 02

## Brief findings incorporated

- **brief 02 §4 "The loop":** the runtime — not the planner — owns the tight
  per-run loop; the loop drains control events, applies side effects, checks
  budget, then calls `planner.Next`. Phase 53's `RunLoop` implements exactly
  this control flow: `drain → apply → project → Next → execute-decision →
  append-step → repeat`.
- **brief 02 §5 sharp-edge #2 "Steering at planner level":** the predecessor's
  `_apply_steering` drained a `SteeringInbox` *inside* the planner loop and
  mutated the trajectory directly — so every alternate planner had to
  replicate it. Phase 53 keeps the inbox in the Runtime: the `RunLoop` drains;
  the planner sees ONLY `RunContext.Control`, never the `steering.Inbox`.
- **brief 02 §6 "Steering mid-step":** events submitted while a tool call is in
  flight are applied at the *next step boundary*, never mid-tool. CANCEL during
  a tool call cancels at the next safe boundary; `hard=true` propagates a
  cancellation context to the in-flight tool. PAUSE blocks at the next
  boundary; RESUME unblocks. INJECT_CONTEXT and REDIRECT are visible on the
  next planner step. Phase 53's drain-point placement (between steps, after a
  decision's execution completes) is the binding implementation of this.
- **brief 02 §5 sharp-edge #6 "Thread-safety":** Harbor's planners are safe to
  use concurrently across runs; the runtime serialises *within* a run. The
  `RunLoop` is a compiled artifact under D-025 — one shared instance drives N
  concurrent runs, each run's loop reading its per-run scope from `ctx` +
  arguments, never from the `RunLoop` struct.
- **brief 02 §3 "Protocol exposure for steering":** the runtime drains the
  inbox between planner steps, applies side effects, and emits
  `control.received` / `control.applied` events. Phase 52 shipped
  `control.rejected`; Phase 53 ships `control.received` and `control.applied`.

## Findings I'm departing from (if any)

None. The departure worth noting is not from a brief but from an *assumption*
the dispatch frame and several phase plans carried: that an engine-level
"planner run loop" already exists for Phase 53 to "wire steering INTO". It does
not — `internal/runtime/engine` is a typed graph executor (Phase 10–14); the
only code that drives `Planner.Next` today is the Phase 49 conformance harness
and per-planner unit tests. Phase 53 therefore *builds* the per-run planner
loop (`RunLoop`) as the wiring vehicle, rather than retrofitting steering into
the graph engine. This is a §4.3 reasonable plan deviation (a speculative
"wire into existing loop" framing turned out wrong once the code was read); it
is documented here and in D-071, and it does not reach into RFC territory —
RFC §6.3 §4 explicitly says "the runtime *implements* this loop", which is
exactly what `RunLoop` is. The graph engine remains the substrate for
graph-family planners; the ReAct/Deterministic step-loop family runs on
`RunLoop`. A future phase MAY converge the two; that is out of scope here.

## Goals

- Ship `internal/runtime/steering.RunLoop` — the per-run planner-step loop that
  is the wiring vehicle for steering + pause/resume.
- Drain the per-run `steering.Inbox` exactly once per step, at the step
  boundary (after the prior decision finished executing, before the next
  `Planner.Next`). The planner sees only `RunContext.Control`.
- Wire all nine control event side effects:
  - `CANCEL` — soft: set `Control.Cancelled`, planner returns `Finish{Cancelled}`
    at the next boundary; hard (`payload.hard == true`): additionally propagate
    a cancellation `context` into the in-flight decision execution via the
    engine's `Cancel(runID)` mechanism.
  - `PAUSE` — set `Control.PauseRequested`; the planner returns
    `RequestPause{AwaitInput}` at the next boundary, which routes through the
    Coordinator (see below).
  - `RESUME` / `APPROVE` — call `Coordinator.Resume` on the run's outstanding
    pause `Token`; the loop unblocks and re-enters `Planner.Next`.
  - `REJECT` — call `Coordinator.Resume` with a `rejected: true` payload; the
    loop terminates the run with `Finish{ConstraintsConflict}` rather than
    re-entering the planner.
  - `INJECT_CONTEXT` — append the payload to `Control.InjectedContext`.
  - `REDIRECT` — set `Control.RedirectGoal` and update `RunContext.Goal`.
  - `USER_MESSAGE` — append the payload's `message` string to
    `Control.UserMessages`.
  - `PRIORITIZE` — call `tasks.TaskRegistry.Prioritize` for the run's task.
- Route a planner's `RequestPause` decision through the Phase 50
  `pauseresume.Coordinator`: `RequestPause` → `Coordinator.Request` → `Token`
  issued + (when a checkpoint store is configured) durable checkpoint → the
  loop blocks at the boundary → an `APPROVE`/`RESUME` control event arrives via
  the Phase 52 inbox → `Coordinator.Resume` → the planner re-enters.
- Cap control-history per session (`MaxControlHistory`, default 256, the
  newest-wins ring) so a long-lived session's applied-control log is bounded.
- Emit `control.received` (drain time) and `control.applied` (side-effect
  applied) canonical events; never apply an event mid-tool-call.

## Non-goals

- **The steering Protocol endpoints** (`task.cancel`, `task.pause`,
  `task.inject_context`, …). That is Phase 54. Phase 53 ships the runtime
  mechanism; the Protocol projection consumes it.
- **A parallel pause coordinator.** PAUSE/RESUME/APPROVE/REJECT side effects
  converge on the Phase 50 `pauseresume.Coordinator` (CLAUDE.md §7 rule 4,
  D-070 §5). Phase 53 mints no second coordinator.
- **Converging the graph engine and the step-loop.** `RunLoop` is the
  step-loop family's driver; the graph engine stays the graph-family substrate.
  Unifying them is a post-V1 RFC concern.
- **Reflection / critique, multi-action LLM-response queueing, the auto-seq
  detector.** brief 02 §4's loop sketch lists these; they are later-phase
  planner-runtime concerns, not steering wiring.
- **A §4.4 driver seam.** The `RunLoop` is an in-process runtime mechanism with
  no plausible alternate backend — the same call D-070 §5 made for the inbox.

## Acceptance criteria

- [ ] `internal/runtime/steering.RunLoop` exists; `RunLoop.Run(ctx, RunSpec)`
      drives a `Planner` to a terminal `Finish` decision, draining the per-run
      `steering.Inbox` once per step boundary.
- [ ] No control event is ever applied mid-tool-call: the drain happens between
      steps only. An integration test enqueues a control event *during* a
      slow decision-execution and asserts it is observed on the *next* step,
      not the current one.
- [ ] `CANCEL` (soft) — the loop sets `Control.Cancelled`; the planner returns
      `Finish{Cancelled}`; the run terminates. Integration-tested.
- [ ] `CANCEL` (hard, `payload.hard == true`) — additionally propagates
      cancellation into the in-flight decision execution; an in-flight tool
      sees a cancelled `ctx`. Integration-tested.
- [ ] `PAUSE` blocks the run at the next boundary (the loop calls
      `Coordinator.Request` and waits); `RESUME` unblocks it (the loop calls
      `Coordinator.Resume` and re-enters `Planner.Next`). Integration-tested.
- [ ] `INJECT_CONTEXT` / `REDIRECT` / `USER_MESSAGE` are visible on the next
      planner step via `RunContext.Control` (`InjectedContext` / `RedirectGoal`
      + `Goal` / `UserMessages` respectively). Integration-tested.
- [ ] `APPROVE` / `REJECT` advance an outstanding pause via
      `Coordinator.Resume` — APPROVE re-enters the planner, REJECT terminates
      with `Finish{ConstraintsConflict}`. Integration-tested.
- [ ] `PRIORITIZE` calls `tasks.TaskRegistry.Prioritize` for the run's task.
      Integration-tested.
- [ ] **§13:** a planner's `RequestPause` decision routes end-to-end through
      the Phase 50 `pauseresume.Coordinator` — `RequestPause` → `Request` →
      `Token` + durable checkpoint → block → `APPROVE`/`RESUME` via the Phase 52
      inbox → `Resume` → planner re-enters. Integration-tested with the Phase 48
      `deterministic.PauseStep` as the emitting consumer.
- [ ] Control-history is capped per session (`MaxControlHistory`); an
      over-cap session retains only the newest entries. Unit-tested.
- [ ] `control.received` and `control.applied` canonical events are registered
      and emitted; `control.rejected` (Phase 52) is unchanged.
- [ ] `RunLoop` is a D-025 compiled artifact: a concurrent-reuse test runs
      N≥100 concurrent `Run` invocations against one shared `RunLoop` under
      `-race` — no data races, no context bleed, no cross-cancellation, no
      goroutine leaks.
- [ ] `scripts/smoke/phase-53.sh` passes; `make drift-audit` + `make preflight`
      green; coverage on `internal/runtime/steering` ≥ 85%.

## Files added or changed

```text
internal/runtime/steering/
  runloop.go              # NEW — RunLoop compiled artifact + RunSpec + Run
  runloop_test.go         # NEW — unit: drain-point, per-event apply, history cap
  apply.go                # NEW — per-control-type side-effect application
  apply_test.go           # NEW — unit: each of the nine apply paths
  history.go              # NEW — per-session capped control-history ring
  history_test.go         # NEW — unit: cap, newest-wins, isolation
  events.go               # CHANGED — register control.received / control.applied
  events_test.go          # CHANGED — assert the two new event types registered
  errors.go               # CHANGED — add ErrNoPlanner / ErrRunLoopMisconfigured
  concurrent_test.go      # CHANGED — add TestConcurrentReuse_RunLoop (N≥100)
  steering.go             # CHANGED — package doc: Phase 53 wiring section
test/integration/
  phase53_steering_wiring_test.go  # NEW — the 9-event matrix + §13 round-trip + concurrency-mid-step
docs/plans/phase-53-steering-wiring.md   # NEW — this plan
docs/plans/README.md                     # CHANGED — Phase 53 row Pending → Shipped
docs/decisions.md                        # CHANGED — append D-071
docs/glossary.md                         # CHANGED — RunLoop, drain-between-steps, control-history cap
scripts/smoke/phase-53.sh                # NEW — smoke assertions
README.md                                # CHANGED — status table Phase 53 row
```

## Public API surface

```go
// RunLoop is the per-run planner-step loop — the runtime component that
// drives a Planner to a terminal Finish, draining the per-run steering
// Inbox between steps and routing pause decisions through the unified
// pauseresume.Coordinator. One RunLoop is built per Runtime process and
// shared across every run (D-025 compiled artifact).
type RunLoop struct { /* unexported; built by NewRunLoop */ }

// NewRunLoop builds a RunLoop. The Registry (Phase 52) and Coordinator
// (Phase 50) are mandatory; the TaskRegistry (for PRIORITIZE), the
// EventBus (for control.received / control.applied emit), the engine
// hard-cancel hook, and the Clock are optional.
func NewRunLoop(reg *Registry, coord pauseresume.Coordinator, opts ...RunLoopOption) (*RunLoop, error)

type RunLoopOption func(*runLoopConfig)
func WithTaskRegistry(tr tasks.TaskRegistry) RunLoopOption
func WithRunLoopBus(b events.EventBus) RunLoopOption
func WithHardCancelHook(fn func(ctx context.Context, runID string) error) RunLoopOption
func WithRunLoopClock(c Clock) RunLoopOption
func WithMaxControlHistory(n int) RunLoopOption  // default MaxControlHistory

// MaxControlHistory is the default per-session applied-control history cap.
const MaxControlHistory = 256

// RunSpec is the per-run input to RunLoop.Run. All run-specific state lives
// here + ctx — never on the RunLoop struct (D-025).
type RunSpec struct {
    Planner  planner.Planner       // the swappable reasoning policy
    Base     planner.RunContext    // the run's RunContext template (per-step refreshed)
    TaskID   tasks.TaskID          // optional; PRIORITIZE targets this
    MaxSteps int                   // hard cap on planner steps; ≤ 0 → a sane default
}

// Run drives the planner to a terminal decision. It Opens the run's Inbox on
// the Registry, drives the loop, and Retires the Inbox on exit (always —
// even on error). Returns the terminal planner.Finish or a wrapped error.
func (rl *RunLoop) Run(ctx context.Context, spec RunSpec) (planner.Finish, error)

// New canonical event types (registered from this package's init):
const (
    EventTypeControlReceived events.EventType = "control.received"
    EventTypeControlApplied  events.EventType = "control.applied"
)
```

## Test plan

- **Unit:**
  - `runloop_test.go` — the drain happens once per step boundary; the planner's
    `RunContext.Control` reflects exactly the events drained since the last
    step; `MaxSteps` caps the loop; a nil `Planner` fails loud with
    `ErrNoPlanner`.
  - `apply_test.go` — each of the nine `apply<Type>` paths in isolation against
    a stub Coordinator / stub TaskRegistry: CANCEL soft sets `Cancelled`; CANCEL
    hard additionally invokes the hard-cancel hook; PAUSE sets `PauseRequested`;
    INJECT_CONTEXT appends; REDIRECT sets `RedirectGoal` + `Goal`; USER_MESSAGE
    appends; PRIORITIZE calls `Prioritize`; APPROVE/RESUME call `Resume`; REJECT
    calls `Resume` with the rejected payload.
  - `history_test.go` — the per-session ring caps at `MaxControlHistory`,
    newest-wins; two sessions' histories never bleed.
  - `events_test.go` — `control.received` / `control.applied` are registered.
- **Integration:** `test/integration/phase53_steering_wiring_test.go` — the
  binding surface. Real drivers on every seam (§17.3 #1): real
  `steering.Registry`, real `pauseresume.Coordinator` (with a real inmem
  `state.StateStore` checkpoint store), real `events.EventBus` (inmem driver),
  real `tasks.TaskRegistry` (inprocess driver), real `deterministic` planner.
  - `TestE2E_Phase53_NineEventMatrix` — a sub-test per control type; each
    enqueues the event and asserts the documented side effect.
  - `TestE2E_Phase53_PauseRoundTrip_ThroughCoordinator` — the §13 test:
    `deterministic.PauseStep` emits `RequestPause` → the `RunLoop` calls
    `Coordinator.Request` → asserts a `Token` + a checkpoint in the StateStore →
    the loop blocks → an `APPROVE` control event is enqueued → the `RunLoop`
    calls `Coordinator.Resume` → the planner re-enters and the run finishes.
  - `TestE2E_Phase53_NoEventAppliedMidToolCall` — enqueue a control event while
    a slow decision-execution is in flight; assert it is observed on the *next*
    step's `RunContext.Control`, never the current one (drain-between-steps).
  - `TestE2E_Phase53_ConcurrencyMidStep` — N≥10 concurrent runs against one
    shared `RunLoop`, each with concurrent `Enqueue` traffic mid-step; assert no
    cross-talk in `Control`, no cross-cancellation, identity propagation holds.
  - Failure mode: a `REJECT` with no outstanding pause fails loud (the loop
    surfaces a wrapped error, never silently swallows it); a missing-identity
    `RunSpec` fails closed.
- **Conformance:** the Phase 49 planner conformance pack already ships
  `Steering_DrainBetweenSteps` (the planner-side contract — planner returns
  `Finish{Cancelled}` on `Control.Cancelled`). Phase 53 does not add a new
  conformance scenario; it is the *runtime-side* consumer of that contract, and
  the integration matrix is the gate.
- **Concurrency / leak:** `TestConcurrentReuse_RunLoop` in `concurrent_test.go`
  — N≥100 concurrent `Run` invocations against one shared `RunLoop` under
  `-race`; distinct per-goroutine run quadruples (context bleed surfaces as a
  foreign `RunID`); a pre-cancelled-ctx subset (no cross-cancellation);
  baseline `runtime.NumGoroutine` restored after join (no leak).

## Smoke script additions

`scripts/smoke/phase-53.sh`:

- Run `go test -race ./internal/runtime/steering/...` — unit + D-025
  concurrent-reuse for `RunLoop`.
- Run `go test -race -run TestE2E_Phase53 ./test/integration/...` — the 9-event
  matrix + the §13 pause round-trip + concurrency-mid-step.
- Static guard: `internal/runtime/steering/runloop.go` exists and imports
  `internal/runtime/pauseresume` (the §13 consumer wires the real Coordinator —
  no parallel pause coordinator).
- Static guard: no `type .*Coordinator .*interface` under
  `internal/runtime/steering` (pause-family controls converge on the unified
  primitive — CLAUDE.md §7 rule 4).
- Static guard: `events.go` registers `control.received` and `control.applied`.
- Import-graph guard: `internal/runtime/steering` does not import the Console.
- The steering Protocol endpoints land in Phase 54 — `skip` the HTTP surface
  per the 404/405/501 → SKIP convention.

## Coverage target

- `internal/runtime/steering`: 85% (master-plan Phase 53 target; the package
  was at 96.6% after Phase 52 — the new `RunLoop` surface must not drag it
  below 85%).

## Dependencies

- Phase 52 (`internal/runtime/steering` — inbox, taxonomy, `ValidatePayload`,
  `CheckScope`, `Registry`).
- Phase 13 (`internal/runtime/engine` cancellation — the hard-CANCEL
  propagation builds on `engine.Cancel(runID)`; Phase 53 consumes it through
  the optional `WithHardCancelHook` seam so `RunLoop` does not hard-import the
  engine package).
- Phase 50 (`internal/runtime/pauseresume` — `Coordinator.Request` / `Resume` /
  `Status`).
- Phase 48 (`internal/planner/deterministic` — `PauseStep` is the `RequestPause`
  emitting consumer for the §13 test).

## Risks / open questions

- **Risk: `RunLoop` placement.** Building the planner loop inside
  `internal/runtime/steering` keeps Phase 53 in its master-plan-declared
  subsystem (`runtime/steering`) and avoids a new top-level directory, but the
  loop is conceptually a planner-runtime concern. Settled in D-071: the loop
  lives in `internal/runtime/steering` for V1 because steering wiring IS its
  reason to exist; no RFC change — RFC §3 lists `internal/runtime/steering` as
  the steering home and RFC §6.3 §4 says "the runtime implements this loop."
  **Wave 9 §17.5 audit:** confirmed as a layering smell accepted for V1 only —
  **issue #81** is the named exit condition (relocate `RunLoop` to a dedicated
  planner-runtime package at the next planner-runtime phase), replacing the
  earlier open-ended "a future phase MAY relocate it."
- **Risk: hard-CANCEL seam.** `RunLoop` must not hard-import
  `internal/runtime/engine` (it would couple the step-loop family to the graph
  engine). Resolved via the `WithHardCancelHook` functional-option seam — the
  caller wires `engine.Cancel` (or any cancellation propagator); `RunLoop`
  holds only a `func(ctx, runID) error`.
- **REJECT semantics — RFC-pinned.** RFC §6.3 originally listed `REJECT` in the
  taxonomy but did not pin whether a rejected pause re-enters the planner or
  terminates the run; D-071 settled it (`REJECT` → `Coordinator.Resume` with a
  `rejected: true` payload → terminate with `Finish{ConstraintsConflict}`). The
  Wave 9 §17.5 audit flagged that settling an open RFC question in a phase-plan
  decision is RFC drift — so RFC §6.3 was amended (Wave 9 audit chore PR) with
  a "Rejected HITL gate is terminal" paragraph that pins the behaviour. This is
  no longer an open question: the behaviour is RFC-canonical, D-071 records its
  implementation, and re-enter-on-reject would be a future planner-policy RFC
  change.

## Glossary additions

- **RunLoop** — the per-run planner-step loop; the runtime component that
  drives `Planner.Next` to a terminal `Finish`, drains the per-run steering
  inbox between steps, and routes pause decisions through the unified
  pause/resume Coordinator. Added to `docs/glossary.md`.
- **Drain-between-steps** — the invariant that the steering inbox is drained
  exactly once per planner-step boundary (after the prior decision finished
  executing, before the next `Planner.Next`), never mid-tool-call. Added to
  `docs/glossary.md`.
- **Control-history cap** — the per-session bound (`MaxControlHistory`,
  default 256) on the applied-control log; newest-wins ring. Added to
  `docs/glossary.md`.

## §13 primitive-with-consumer — discharged here

**Phase 53 closes the §13 primitive-with-consumer obligation for BOTH Wave-9
primitives:**

1. **The Phase 50 `pauseresume.Coordinator`.** CLAUDE.md §13 names it
   explicitly: "The unified pause/resume primitive requires a `RequestPause`-
   emitting consumer in the same wave … Phase 50 (the primitive) cannot ship
   without at least one planner … emitting `RequestPause` for a real reason."
   Phase 53's `RunLoop` is that consumer. The Phase 48
   `deterministic.PauseStep` emits the `planner.RequestPause` `Decision` shape;
   `RunLoop` routes it through `Coordinator.Request` → `Token` + durable
   checkpoint → block → `APPROVE`/`RESUME` via the Phase 52 inbox →
   `Coordinator.Resume` → planner re-enters. The integration test is
   **`TestE2E_Phase53_PauseRoundTrip_ThroughCoordinator`**
   (`test/integration/phase53_steering_wiring_test.go`).
2. **The Phase 52 steering inbox + nine-type taxonomy.** D-070 §5 explicitly
   deferred the run-loop wiring + the PAUSE/RESUME/APPROVE/REJECT side effects
   to Phase 53. `RunLoop` is the first consumer that `Drain`s a real
   `steering.Inbox`, projects the result onto `RunContext.Control`, and applies
   all nine control-event side effects. The integration test is the 9-event
   matrix **`TestE2E_Phase53_NineEventMatrix`** plus the drain-invariant test
   **`TestE2E_Phase53_NoEventAppliedMidToolCall`** and the concurrency test
   **`TestE2E_Phase53_ConcurrencyMidStep`**.

Phase 53 mints no parallel pause coordinator — PAUSE/RESUME/APPROVE/REJECT
converge on the Phase 50 `pauseresume.Coordinator` (CLAUDE.md §7 rule 4). Both
obligations are discharged in-wave (Wave 9, Stage 3) per the binding
coordinator decision recorded in D-067 §4 and D-070 §5.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. — `TestConcurrentReuse_RunLoop` covers this.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. — `test/integration/phase53_steering_wiring_test.go` covers this.
- [ ] If new vocabulary: glossary updated — RunLoop, drain-between-steps, control-history cap.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-071 records the "no existing run loop" deviation.
