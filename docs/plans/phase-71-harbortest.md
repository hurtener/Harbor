# Phase 71 — `harbortest` test kit package

## Summary

Phase 71 ships the public `harbortest` package — Harbor's first-class
authoring surface for flow-level agent tests. The kit gives test
authors `RunOnce`, `AssertSequence`, `AssertNoLeaks`, `SimulateFailure`,
and `RecordedEvents` so a flow-level test fits in ten lines or fewer
while the cross-tenant / cross-session isolation contract is enforced
by an ergonomic public assertion.

## RFC anchor

- RFC §6.13

## Briefs informing this phase

- brief 06
- brief 01
- brief 05

## Brief findings incorporated

- **brief 06 §3 "What the test kit gives authors."** — A `harbortest`
  package: `RunOnce(ctx, agent, input) (Output, EventLog, error)`;
  `AssertSequence(log, []EventType{...})`; `AssertNoLeaks(log)`
  (cross-tenant/session leakage detector);
  `SimulateFailure(toolName, code, n)`; `RecordedEvents(runID) []Event`.
  The point: make a flow-level test ten lines or fewer.
- **brief 06 §4 "Isolation-triple filtering by default."** — Cross-tenant
  subscriptions are an explicit, audited operation. `AssertNoLeaks` is
  the public, ergonomic version of the same isolation contract — a
  test-author lens over the bus events to catch a cross-session bug
  surfacing in the captured log.
- **brief 06 §5 sharp-edge "Two-channel split."** — Harbor unifies
  observability on one bus. `harbortest` consumes ONE source —
  `events.EventBus` — and produces ONE `EventLog`; the kit does not
  introduce a parallel observability path.
- **brief 01 §4 "worker loop."** — The worker loop's identity-bearing
  envelope flow is the unit `harbortest` exercises end-to-end. A test
  author wants the same loop a production runtime would execute, with
  ergonomic capture; the kit does not stub the runtime, it composes
  real drivers (in-mem events + in-mem state + in-process tools).
- **brief 05 §4 "Identity propagation"** — every storage / event /
  task path scopes by the (tenant, user, session) triple plus RunID.
  `RunOnce` constructs the canonical quadruple via `identity.WithRun`
  and propagates it through `ctx` exactly as production code does.

## Findings I'm departing from (if any)

None. The phase implements brief 06 §13 verbatim. The brief's reference
to a previous `testkit.py` is internal context only — the implementation
is original Go and the package name `harbortest` is the only naming
this PR carries. The §13 "predecessor-naming" rule is not violated:
the kit's design lineage is captured here as "the same shape the test
kit pattern serves in flow-level agent testing," with no reference to
the predecessor project.

## Goals

- Public `harbortest/` package (importable as
  `github.com/hurtener/Harbor/harbortest`) that exposes the five
  documented entry points.
- `RunOnce` constructs a deterministic identity quadruple by default,
  wires a real `events.EventBus` + audit redactor, captures every
  event the run emits, and returns a `(Output, EventLog, error)`
  triple. Errors during stack construction are wrapped and surfaced —
  no silent degradation (CLAUDE.md §5 "Fail loudly").
- `AssertSequence(log, want)` matches `want` against the captured
  log as an ordered subsequence (the log may contain more event types
  between matches; the order of `want` is preserved). Strict-prefix /
  strict-exact variants are out of scope; the subsequence semantics
  cover the brief's "flow-level test" use case.
- `AssertNoLeaks(log)` walks the captured events grouped by identity
  triple and asserts NO event tagged with one triple references the
  RunID of any other triple's events. A deliberate cross-session bug
  fixture in the regression test catches the leak.
- `SimulateFailure(injector, toolName, code, n)` wraps a tool catalog
  via a `FaultInjector` so the next `n` calls to `toolName` return the
  given `tools.ErrorClass`; after `n`, the tool resumes normal
  behaviour. The injector is configured before construction; it is a
  test-only surface.
- `RecordedEvents(runID)` is a method on `EventLog` returning the
  events whose `Identity.RunID == runID`, in their capture order.
- Self-tests in `harbortest/*_test.go` cover the round-trip, the
  assertions, the deliberate-bug `AssertNoLeaks` regression test, the
  `SimulateFailure` counter behaviour, and a D-025 concurrent-reuse
  test (N=100 concurrent `RunOnce` invocations against a single
  shared bus + redactor).

## Non-goals

- A test-only LLM driver. `harbortest` is transport- and provider-
  agnostic; the test author supplies an `Agent` whose `Run` is
  whatever code they want exercised (a planner, a flow, a hand-rolled
  function). Wiring a mock LLM is the test author's concern; the kit
  does not ship a stub LLM (the §13 amendment "Test stubs as
  production defaults" applies here — a stub LLM in this kit would
  encourage tests that exercise the stub instead of the runtime).
- A test-only state driver. The kit composes the real in-mem state
  driver if the test wants persistence; an `Agent` with no persistence
  needs runs without StateStore wiring.
- A test-only event-bus driver. Same posture as state — the in-mem
  `EventBus` driver is real production code and is what `RunOnce`
  composes.
- Snapshot / golden-file assertions on the EventLog. Those are
  out-of-band testing tools; the kit's assertions are programmatic.
- Replay-from-cursor consumption. The kit captures events live via
  `events.EventBus.Subscribe`; replay is Phase 06's surface and not a
  harbortest concern.

## Acceptance criteria

- [ ] `harbortest.RunOnce(ctx, agent, input) (Output, EventLog, error)`
      exists with godoc, wires a real `events.EventBus` (in-mem driver)
      and `audit.Redactor` (patterns driver), constructs an identity
      quadruple, runs the agent, and returns the captured log.
- [ ] `harbortest.AssertSequence(t, log, want)` is implemented with
      ordered-subsequence semantics, returns a clear error naming the
      first missing event type and the actual sequence captured.
- [ ] `harbortest.AssertNoLeaks(t, log)` is implemented and catches a
      deliberate cross-session bug in a regression test
      (`TestAssertNoLeaks_CatchesCrossSessionLeak`).
- [ ] `harbortest.SimulateFailure(injector, toolName, code, n)`
      wraps a tool catalog so the next `n` `Resolve(toolName).Invoke`
      calls fail with the given error class, then resume normal
      behaviour.
- [ ] `EventLog.RecordedEvents(runID)` returns the events for one run.
- [ ] D-025 concurrent-reuse test: N≥100 concurrent `RunOnce`
      invocations against shared deps run clean under `-race` — no
      cross-talk, no goroutine leak (baseline restored).
- [ ] Coverage on `harbortest/` ≥ 85% statement coverage.
- [ ] `scripts/smoke/phase-71.sh` runs the kit's self-tests under
      `-race` and reports OK ≥ the count of acceptance criteria it
      exercises (the unit suite is the smoke).
- [ ] README Status row Phase 71 → Shipped + a `harbortest` pointer
      added to the testing section.
- [ ] `docs/plans/README.md` Phase 71 row `Pending` → `Shipped`.
- [ ] Glossary entries added for the five public symbols.
- [ ] D-085 appended to `docs/decisions.md` with the public-package
      call, the deterministic-identity-default decision, the
      assertion-semantics calls, the `FaultInjector` design, and the
      §13 amendment posture (no stub LLM, no silent fallback).

## Files added or changed

```text
harbortest/
  doc.go                    # package godoc
  agent.go                  # Agent / Output / Input types
  testing.go                # TestingT interface
  eventlog.go               # EventLog + RecordedEvents
  runonce.go                # RunOnce
  assertions.go             # AssertSequence + AssertNoLeaks
  simulate.go               # FaultInjector + SimulateFailure
  agent_test.go             # round-trip + assertion tests
  assertions_test.go        # AssertSequence / AssertNoLeaks (+ deliberate-bug regression)
  simulate_test.go          # SimulateFailure
  concurrent_test.go        # D-025 N=100 concurrent reuse
docs/plans/phase-71-harbortest.md
docs/plans/README.md        # row flip
docs/decisions.md           # D-085
docs/glossary.md            # five new terms
scripts/smoke/phase-71.sh
README.md                   # Status row + testing pointer
```

The `harbortest/` top-level directory is **a new non-`internal/`,
non-`cmd/` package**. CLAUDE.md §3 documents the canonical layout; this
phase adds `harbortest/` to it. The justification is binding: this is
a public test kit consumed by end-users writing tests against their
Harbor-built agents. Public-test-kit packages are conventionally
top-level in Go modules (e.g. `golang.org/x/tools/go/analysis/analysistest`
is at a top-level path inside its module). Internal packages are
import-restricted by the Go toolchain to the module's owning subtree;
the kit is meant to be imported by *consumer* test code outside this
module. The phase updates CLAUDE.md §3 in the same PR.

## Public API surface

```go
// Agent is the unit a test author exercises end-to-end. The runner
// invokes Run once, captures every event the run publishes, and
// returns the (Output, EventLog, error) triple.
type Agent interface {
    Run(ctx context.Context, input any) (output any, err error)
}

// AgentFunc adapts a plain function into an Agent.
type AgentFunc func(ctx context.Context, input any) (any, error)
func (f AgentFunc) Run(ctx context.Context, input any) (any, error) { ... }

// Deps optionally lets the caller inject a shared bus / redactor /
// identity / runID for cross-RunOnce coordination (the typical case
// — passing nil — synthesises deterministic defaults).
type Deps struct {
    Bus      events.EventBus
    Redactor audit.Redactor
    Identity *identity.Identity
    RunID    string
}

// RunOnce executes agent.Run under a freshly-constructed identity
// quadruple (or the caller-provided Identity/RunID) and returns the
// (Output, EventLog, error) triple. Stack-construction failures are
// returned as wrapped errors with the missing component named — the
// kit fails loudly per CLAUDE.md §5.
func RunOnce(ctx context.Context, agent Agent, input any, deps ...Deps) (any, *EventLog, error)

// EventLog is the captured stream from one RunOnce invocation.
type EventLog struct { /* internally-synchronised slice */ }
func (l *EventLog) All() []events.Event
func (l *EventLog) RecordedEvents(runID string) []events.Event
func (l *EventLog) Len() int

// TestingT mirrors *testing.T's subset the kit needs.
type TestingT interface {
    Helper()
    Errorf(format string, args ...any)
}

// AssertSequence verifies `want` appears as an ordered subsequence
// of `log.All()`. Returns true on success; on failure it calls
// t.Errorf with a diff naming the first missing want entry and the
// captured sequence.
func AssertSequence(t TestingT, log *EventLog, want []events.EventType) bool

// AssertNoLeaks verifies no event tagged with one identity triple
// references a RunID owned by a different identity triple in the log.
// Returns true on success; on failure it calls t.Errorf naming the
// offending event(s).
func AssertNoLeaks(t TestingT, log *EventLog) bool

// FaultInjector wraps a tools.ToolCatalog and pops a configured
// failure each time a wrapped tool is resolved + invoked.
type FaultInjector struct { /* concurrent-safe */ }
func NewFaultInjector(cat tools.ToolCatalog) *FaultInjector
func (f *FaultInjector) Catalog() tools.ToolCatalog

// SimulateFailure schedules the next n invocations of toolName to
// fail with the given error class. Subsequent invocations resume
// normal behaviour. Cumulative: calling SimulateFailure twice on the
// same tool stacks the counters.
func SimulateFailure(f *FaultInjector, toolName string, class tools.ErrorClass, n int)
```

## Test plan

- **Unit:**
  - `TestRunOnce_RoundTrip_CapturesEvents` — register a tool, run an
    Agent that invokes it, assert the EventLog contains
    `tool.invoked` + `tool.completed` for the run.
  - `TestRunOnce_DefaultIdentity_Deterministic` — two calls with no
    deps see the SAME deterministic default identity (so test authors
    can predict the values) but DIFFERENT RunIDs.
  - `TestRunOnce_FailsLoudly_OnNilAgent` — `RunOnce(ctx, nil, ...)`
    returns a wrapped error naming the missing component.
  - `TestEventLog_RecordedEvents_FiltersByRun` — two runs in one
    EventLog, `RecordedEvents(runA)` returns only runA's events.
  - `TestAssertSequence_Happy` — exact want sequence matches.
  - `TestAssertSequence_OrderedSubsequence_AllowsIntervening` —
    captured log has extra events between matches; assertion passes.
  - `TestAssertSequence_Fails_OnMissingType` — `t.Errorf` called with
    diff naming the missing type.
  - `TestAssertSequence_Fails_OnOutOfOrder` — captured `B,A` against
    want `A,B`; assertion fails.
  - `TestAssertNoLeaks_Happy` — single-identity log; assertion passes.
  - `TestAssertNoLeaks_CatchesCrossSessionLeak` — the regression
    test: a deliberately-broken Agent publishes an event under runA's
    triple that names runB's RunID; `AssertNoLeaks` flags it via
    `t.Errorf`.
  - `TestSimulateFailure_FailsThenResumes` — register a tool, inject
    3 failures, call the tool 5 times, assert the first 3 fail with
    the configured class and the next 2 succeed.
  - `TestSimulateFailure_PerToolIsolated` — injecting a failure on
    `toolA` does not affect `toolB`.
- **Integration:** the kit IS the integration surface — `RunOnce`
  composes real `events.EventBus` (in-mem driver) + real
  `audit.Redactor` (patterns driver) + real `tools.ToolCatalog`
  (canonical) + the real `inproc` tool driver. The self-tests above
  exercise the seam end-to-end (CLAUDE.md §17.2 in-package shape).
- **Conformance:** N/A — `harbortest` is a single-implementation
  surface (it's the kit; there's no driver seam).
- **Concurrency / leak:** `TestRunOnce_ConcurrentReuse_NoCrossTalk` —
  N=100 concurrent `RunOnce` invocations against a single shared
  `Deps{Bus, Redactor}`. Each goroutine uses a distinct identity
  triple; the test asserts every captured log carries only its own
  identity, the baseline goroutine count is restored at teardown, and
  no race detector hits.

## Smoke script additions

`scripts/smoke/phase-71.sh` runs `go test -race -run '^Test'
./harbortest/...` and exits non-zero on any failure. The phase-71
smoke is unique among the late-phase scripts in that the kit has no
HTTP / Protocol surface — its self-tests ARE its smoke. The script
uses `assert_status 0` against the wrapped `go test` exit code.

## Coverage target

`harbortest/`: 85% statement coverage.

## Dependencies

- 05 — `events.EventBus` (in-mem driver, the canonical bus the kit
  subscribes to).
- 07 — `state.StateStore` (the kit does NOT mandate state but the
  factory shape is the precedent for kit-side construction).
- 09 — `internal/runtime/messages.Envelope` (the kit's `Agent`
  signature is intentionally thinner — `(ctx, input) → output` —
  because end users may not own engine graphs, but the identity
  propagation pattern is the §6 isolation contract Phase 09
  enforces in envelopes).

## Risks / open questions

- **Risk: tests written against `harbortest` couple to V1's event
  taxonomy.** Mitigation: the `EventType` registry's exhaustive-enum
  invariant (Phase 05) means new event types land as exported
  constants; an `AssertSequence` test naming `EventTypeToolCompleted`
  is forward-compatible. The kit doesn't elide unknown types — they
  show up in the log and the test author can choose to assert or not.
- **Risk: end users build production code on top of `harbortest`.**
  Mitigation: the package godoc states it is a TEST kit; the
  `TestingT` interface and the lack of a Cleanup mechanism make it
  awkward as a production wrapper. The package is intentionally
  named `harbortest` so any production import is grep-visible.
- **Open question: should `SimulateFailure` ever expose a CALL-FAILS
  variant that returns a typed `tools.ErrToolPolicyExhausted` instead
  of a fresh error?** Resolved: yes, the wrapper returns an error
  whose class matches the requested `ErrorClass` AND wraps a sentinel
  so the policy shell classifies it correctly. The injector's wrapper
  builds the error via `fmt.Errorf("harbortest: simulated %s failure: %w", class, sentinelFor(class))`.
- **Open question: should `RunOnce` accept a `*engine.Engine`?**
  Resolved: no — the test surface is the `Agent` interface, and the
  Agent can internally use whatever runtime constructs it wants
  (engine, flow, planner, plain function). Coupling the kit to the
  engine type would force every test author to construct an engine
  graph even for unit-shaped tests.

## Glossary additions

- `RunOnce` — the kit's one-shot agent runner.
- `EventLog` — the captured event stream from one `RunOnce` call.
- `AssertSequence` — the ordered-subsequence assertion over an
  EventLog.
- `AssertNoLeaks` — the cross-tenant/session isolation assertion over
  an EventLog.
- `SimulateFailure` — the per-tool fault injector.
- `RecordedEvents` — the per-RunID accessor on EventLog.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (`harbortest` ≥ 85%)
- [x] If multi-isolation paths changed: cross-session isolation test
      passes (`TestAssertNoLeaks_CatchesCrossSessionLeak` is the
      regression).
- [x] **If this phase builds a reusable artifact: concurrent-reuse
      test passes** — `TestRunOnce_ConcurrentReuse_NoCrossTalk` runs
      N=100 concurrent invocations against a shared `Deps`.
- [x] **If this phase consumes a shipped subsystem's surface OR
      closes a cross-subsystem seam: an integration test exists** —
      the kit IS the integration surface (CLAUDE.md §17.2 in-package
      shape); its self-tests wire real `events.EventBus` + real
      `audit.Redactor` + real `tools.ToolCatalog` + real `inproc`
      tool driver. Identity propagation is asserted in every test;
      the deliberate-cross-session-bug regression is the failure
      mode.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A — no departure.
