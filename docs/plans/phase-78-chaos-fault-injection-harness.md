# Phase 78 — chaos-fault-injection-harness

## Summary

Phase 78 ships the master chaos / fault-injection harness — a table-driven
integration suite that injects each of the five master-plan-named failure modes
against the **real** Runtime components and asserts every fault produces its
documented loud error / event AND the documented recovery path runs. It follows
the Phase 76 / 77 harness shape: it lives in `test/integration/`, opens
production components through their registry factories, and runs under `-race`
in a dedicated CI job. The harness proves recovery; it never masks a failure.

## RFC anchor

<!-- The master plan lists "n/a" for Phase 78's RFC §; the harness injects
     against surfaces specified in these RFC sections. -->
- RFC §6.1 — runtime engine (mid-run kill / cancellation propagation)
- RFC §6.13 — event bus (bounded-channel drop-oldest backpressure)
- RFC §6.5 — LLM client (provider-quirk handling, retry-with-feedback)
- RFC §6.11 — StateStore (driver disconnect / fail-loud error path)
- RFC §3.4 — pause-state serialisation (`ErrUnserializable` fail-loud contract)

## Briefs informing this phase

- brief 01
- brief 02
- brief 05
- brief 06

## Brief findings incorporated

- **brief 01 §"Pain points to never repeat" — the predecessor's
  deadlock-on-shutdown bug.** The chaos harness's kill-mid-run row cancels a run
  while envelopes are in flight and then asserts a *bounded, clean teardown*
  (the run-cancelled event fires AND `Engine.Stop` joins every goroutine within
  a deadline). A teardown that hung would be exactly the inherited bug; the
  harness gates against it.
- **brief 02 §4 — pause-state serialisation must FAIL LOUDLY with
  `ErrUnserializable` instead of returning nil.** The pause-deserialise row
  hands `Coordinator.Request` a `PauseRequest` whose trajectory carries a live
  channel and asserts `trajectory.ErrUnserializable` surfaces with a non-empty
  field path — never a half-persisted checkpoint, never `(nil, nil)`.
- **brief 06 §"bus backpressure" — bounded channels with an explicit
  drop-oldest policy emit a `dropped` event on the first drop in a window.**
  The drop-messages row saturates a small-buffered subscription and asserts the
  bus emits the typed `bus.dropped` event carrying the dropped sequence range —
  the documented signal that a consumer missed events.
- **brief 05 §"state, fail-closed" — StateStore methods surface store failures
  as wrapped errors; callers never silently degrade.** The StateStore-disconnect
  row wraps the real in-mem driver in a fault-injecting decorator that begins
  returning a transport error after N calls; it asserts the error surfaces
  loudly out of `Save`/`Load`, then asserts the documented recovery path —
  the decorator "reconnects" and subsequent calls succeed.

## Findings I'm departing from (if any)

None.

## Goals

- Ship one table-driven chaos harness covering the five master-plan failure
  modes, each asserting the documented loud error/event AND the documented
  recovery path.
- Inject faults by wrapping the **real** production components in thin
  fault-injecting decorators that live in the test tree — never by substituting
  a stub for a real driver, never by registering a fault driver as a default.
- Run the suite under `-race` in `make test` and in a dedicated `chaos` CI job.
- Honour CLAUDE.md §13 "no silent degradation": every injected fault produces a
  loud, asserted error or event; the harness proves recovery, it does not mask
  failure.

## Non-goals

- A general-purpose fault-injection framework or a configurable chaos DSL — the
  harness is five concrete table rows, not a toolkit.
- Production fault-injection hooks. The harness is integration-test-scoped code
  (`test/integration/`); nothing here ships in the `harbor` binary or on a hot
  path (master-plan "used in integration tests; not on hot path").
- A new top-level directory or a new persistence/transport seam. The harness is
  one `*_test.go` file plus a fault-injecting-decorator helper file in the same
  `integration_test` package.
- Re-testing each subsystem's happy path — Phases 76 / 77 and the per-package
  suites own that. Phase 78 owns the *failure* surface.

## Acceptance criteria

- [ ] `test/integration/phase78_chaos_fault_injection_test.go` ships a
      table-driven `TestE2E_Phase78_ChaosFaultInjection` whose `chaosCases`
      slice carries one row per failure mode.
- [ ] **Kill mid-run** — a run is cancelled while envelopes are in flight; the
      row asserts the `runtime.run_cancelled` event fires (via the engine's
      `RunCancelledHandler` seam) AND `Engine.Stop` tears down cleanly within a
      bounded deadline with no goroutine leak.
- [ ] **Drop messages** — a small-buffered subscription is saturated; the row
      asserts the bus emits the typed `bus.dropped` event carrying the dropped
      sequence range.
- [ ] **Provider quirks** — a mock LLM driver returns malformed output; wrapped
      by the real `retry.Wrap` retry-with-feedback layer with a rejecting
      `Validator`, the row asserts the `llm.retry_with_feedback` event fires AND
      the call exhausts loudly with `llm.ErrRetryExhausted` (no silent bad
      response). A companion sub-assertion proves the recovery path: a driver
      that returns malformed output once then valid output succeeds after one
      retry.
- [ ] **StateStore disconnect** — a fault-injecting decorator over the real
      in-mem `StateStore` begins returning a transport error; the row asserts
      the error surfaces loudly out of `Save`/`Load` (no silent degradation),
      then asserts the documented recovery path — the store "reconnects" and
      subsequent calls succeed.
- [ ] **Force pause-deserialize failure** — a `PauseRequest` whose trajectory
      carries a non-serialisable handle (a live channel) fails
      `Coordinator.Request` loud with `trajectory.ErrUnserializable` and a
      non-empty field path; nothing is half-persisted.
- [ ] Every fault-injecting decorator wraps a real production component and
      lives in a test-tree file — never registered as a driver default
      (CLAUDE.md §13 compliance documented in D-137).
- [ ] The suite runs under `-race`; the dedicated `chaos` CI job runs it on
      every PR.
- [ ] `scripts/smoke/phase-78.sh` (`static-only`) asserts the harness file
      exists, declares the test, is table-driven, and the CI job is wired.

## Files added or changed

```text
test/integration/
  phase78_chaos_fault_injection_test.go   # the harness — the deliverable
  phase78_faults_test.go                  # fault-injecting decorators (test tree)
docs/plans/
  phase-78-chaos-fault-injection-harness.md   # this plan
  README.md                                   # Phase 78 row -> Shipped
docs/
  decisions.md                            # D-137
  glossary.md                             # "chaos harness", "fault injection"
scripts/smoke/
  phase-78.sh                             # static-only smoke
.github/workflows/
  ci.yml                                  # new `chaos` job
README.md                                 # Status table -> Phase 78 Shipped
```

## Public API surface

None. Phase 78 ships only `integration_test`-package test code — no exported
runtime symbol, no Protocol method, no new interface other phases depend on.

## Test plan

- **Unit:** N/A — the harness IS the test; there is no production code to
  unit-test.
- **Integration:** `TestE2E_Phase78_ChaosFaultInjection` — five table rows, each
  injecting one failure mode against a real component opened through its
  production registry factory (`events.Open`, `state.Open`, `engine.New`,
  `pauseresume.New`) with the real `retry.Wrap` LLM layer. Identity propagates
  through every component as a real `(tenant, user, session)` triple. Each row
  IS a failure-mode assertion (CLAUDE.md §17.3 #3). Run under `-race`.
- **Conformance:** N/A — Phase 78 is not a driver-conformance phase.
- **Concurrency / leak:** the kill-mid-run row asserts no goroutine leak after
  `Engine.Stop` (a bounded `runtime.NumGoroutine` poll, never an instant
  snapshot — §17.4). The harness is not `t.Parallel` (`NumGoroutine` is
  process-global, matching Phase 77).

## Smoke script additions

`scripts/smoke/phase-78.sh` (`static-only`) asserts:

1. `test/integration/phase78_chaos_fault_injection_test.go` exists.
2. The harness declares `func TestE2E_Phase78_ChaosFaultInjection`.
3. The harness is table-driven (`chaosCases = []chaosCase{` present).
4. `.github/workflows/ci.yml` declares the `chaos` CI job.

Phase 78 adds no Protocol / REST / CLI surface — the live-server portion SKIPs
(the §4.2 "no endpoint -> SKIP" analogue).

## Coverage target

N/A — Phase 78 adds only `integration_test`-package test code. There is no
production package whose statement coverage changes. The harness's value is
measured by the five failure-mode assertions passing under `-race`, not by a
coverage percentage (same posture as the Phase 76 / 77 harnesses).

## Dependencies

76, 77.

## Risks / open questions

- **Risk: a fault-injecting decorator drifts from the real driver's contract.**
  Mitigation: decorators wrap the real driver and delegate every non-faulting
  call verbatim — they decorate, never re-implement. The harness opens the
  underlying driver through its production factory.
- **Risk: the drop-messages row is timing-sensitive.** Mitigation: the row
  saturates the subscription buffer deterministically (publish strictly more
  events than `SubscriberBufferSize` before draining) and asserts the
  `bus.dropped` event via a bounded eventually-poll on the subscription channel
  — never a `time.Sleep` (§17.4).
- **Risk: misclassifying a fault-injecting decorator as a §13 "test stub as
  production default" violation.** Resolved in D-137: the decorators wrap (do
  not replace) real drivers, live in `*_test.go` files, and are never a
  registry default. This is the §17.3 "real drivers at the seam" pattern with a
  fault overlay, not a stub.

## Glossary additions

- **chaos harness** — added to `docs/glossary.md`.
- **fault injection** — added to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A (test-only code; see Coverage target)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (no isolation path changed; Phase 76 owns that gate)
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A. Phase 78 builds no reusable runtime artifact; it ships integration-test code. The kill-mid-run row still asserts no goroutine leak after teardown.
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — yes; the harness IS the integration test, wires real drivers end-to-end, asserts identity propagation, covers five failure modes, runs under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departure)
