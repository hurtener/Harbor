# Phase 77 — Goroutine leak conformance harness

## Summary

A master goroutine-leak conformance harness: one table-driven Go test that, for
every long-lived Runtime component, records a baseline `runtime.NumGoroutine()`,
constructs + starts + exercises + `Stop()`s the component, then asserts the
count returns to baseline via a bounded eventually-poll. It generalises the
per-package leak tests that Phases 10/12/13/50/52 each shipped individually
into a single conformance suite, and runs on every PR under `-race`. Adding a
future long-lived component is one table row.

## RFC anchor

- RFC §3.5
- RFC §6.1

## Briefs informing this phase

- brief 01
- brief 05

## Brief findings incorporated

- brief 01 §"Core runtime": the predecessor's flow runtime leaked goroutines
  because per-run workers were not joined on teardown; Harbor's `Engine.Stop`
  must join one-goroutine-per-node + dispatcher + sweepers. This harness pins
  that contract mechanically — `Engine` is the first table row.
- brief 01 §"streaming / backpressure": streaming added per-run goroutines that
  the predecessor leaked under deadlock-on-shutdown; the harness exercises the
  engine through a streaming run before `Stop` so the streaming goroutines are
  in-flight at teardown.
- brief 05 §"Sessions and SessionManager": the SessionManager runs a background
  GC sweeper; brief 05 flags that long-lived sweepers are a classic leak source
  when `CloseRegistry` does not `wg.Wait()` the sweeper. The harness covers the
  `sessions.Registry` row and asserts its sweeper is joined.
- brief 05 §"Tasks (unified foreground/background)": the unified TaskRegistry
  owns background continuation goroutines; brief 05 warns these must be
  cancellable by `ctx` and joined on `Close`. The harness covers the
  `tasks` inprocess driver row.

## Findings I'm departing from (if any)

None.

## Goals

- A single conformance test that exercises every long-lived Runtime component
  (anything built once that starts goroutines and exposes `Stop`/`Close`/
  `CloseRegistry`) and asserts no goroutine leak after teardown.
- The per-component cases are table-driven: a future component is one new row,
  not a new test function.
- Real production drivers/components on the seam — no mocks (CLAUDE.md §17.3).
- Each case exercises the component (not just construct→stop) so per-run /
  per-delivery goroutines are in-flight at teardown.
- The harness runs in CI on every PR under `-race`.
- Bounded eventually-poll (deadline + interval), never an instant
  `NumGoroutine()` snapshot (CLAUDE.md §17.4 — an instant check is flaky).

## Non-goals

- Per-package D-025 concurrent-reuse leak tests are NOT replaced — they stay.
  This harness is the cross-subsystem conformance gate that sits *above* them.
- No new top-level directory (CLAUDE.md §3) — the harness lands under the
  existing `test/integration/` tree.
- Not a cross-tenant isolation harness — that is Phase 76's job. Phase 77 is
  purely the goroutine-lifecycle gate.
- Not a chaos / fault-injection harness — that is Phase 78.
- No new Protocol method, REST endpoint, or wire type.

## Acceptance criteria

- [ ] `test/integration/phase77_goroutine_leak_test.go` ships a table-driven
      `TestE2E_Phase77_GoroutineLeakConformance` covering, at minimum: the
      `Engine`, the inmem `EventBus`, the durable `EventBus`, the
      `sessions.Registry`, and the inprocess `TaskRegistry`.
- [ ] Each row constructs the component with real drivers, starts it, exercises
      it (a run / a delivery / an open-close cycle), tears it down, and asserts
      `runtime.NumGoroutine()` returns to baseline within a bounded poll.
- [ ] The harness runs N≥10 construct→exercise→teardown iterations per row so a
      slow leak (one goroutine per cycle) is amplified above the tolerance.
- [ ] The suite passes under `go test -race`.
- [ ] CI runs the harness on every PR via a dedicated `leak-harness` job (or
      step) in `.github/workflows/ci.yml`.
- [ ] If a covered component genuinely leaks a goroutine on teardown, the leak
      is fixed in this PR (CLAUDE.md §17.6) — not skipped around.

## Files added or changed

```text
docs/plans/phase-77-goroutine-leak-harness.md      # this plan
docs/plans/README.md                               # Phase 77 row Pending → Shipped
docs/decisions.md                                  # D-135
docs/glossary.md                                   # "goroutine-leak conformance harness" term
README.md                                          # Status table — Phase 77 Shipped
scripts/smoke/phase-77.sh                          # static-only smoke (documented SKIP + file-existence)
test/integration/phase77_goroutine_leak_test.go    # the harness — the deliverable
.github/workflows/ci.yml                           # new leak-harness job
```

## Public API surface

None. The harness is a `package integration_test` file — it imports the public
constructors of the components it covers (`engine.New`, `events.OpenDriver`,
`sessions.New`, `tasks.OpenDriver`) and exposes no symbols other phases depend
on. The table of component cases is an unexported slice; a future component is
added by appending a row.

## Test plan

- **Unit:** N/A — the harness IS the test; there is no production code to
  unit-test.
- **Integration:** `TestE2E_Phase77_GoroutineLeakConformance` — the table-driven
  conformance suite. Real production drivers on every seam (inmem/durable event
  bus, inmem state store, patterns audit redactor, inprocess task driver). Each
  row exercises the component end-to-end (an engine run with streaming, a bus
  publish→deliver round-trip, a sessions open→close, a task spawn). The failure
  mode covered is a *teardown without ctx cancellation* — each row asserts that
  `Stop`/`Close` joins (does not abandon) the component's goroutines; a row
  whose component leaks fails the suite loudly.
- **Conformance:** the whole file is the conformance suite — one table, every
  long-lived component, one invariant (`NumGoroutine` returns to baseline).
- **Concurrency / leak:** the harness runs each row's construct→exercise→
  teardown cycle N≥10 times so a per-cycle leak accumulates above the
  parked-goroutine tolerance; the suite runs under `-race` in CI.

## Smoke script additions

`scripts/smoke/phase-77.sh` (`static-only`): Phase 77 adds no Protocol surface,
so the smoke is a documented SKIP plus static file-existence assertions —
asserts `test/integration/phase77_goroutine_leak_test.go` exists, asserts the
`leak-harness` CI job is declared in `.github/workflows/ci.yml`, then `skip`s
the live-server portion (no endpoint to hit).

## Coverage target

N/A — the harness is a `test/integration/` file; it ships no production package
of its own. The components it covers retain their own per-package coverage
targets (Phase 10 `internal/runtime/engine` 80%, etc.). The relevant gate is
"the harness passes under `-race` in CI", not a coverage percentage.

## Dependencies

10, 13, 50

## Risks / open questions

- **Parked-goroutine retirement latency.** Go does not retire parked goroutines
  instantly; an instant `NumGoroutine()` snapshot is flaky. Mitigation: the
  established bounded-poll pattern (deadline + 10ms interval + `runtime.Gosched`)
  reused from the per-package leak tests, with a small absolute tolerance
  (`+N`) absorbing test-harness background goroutines. Documented in the harness
  file header.
- **`t.Parallel` pollution.** `NumGoroutine` is process-global; a parallel test
  goroutine inflates the count. Mitigation: the harness does NOT call
  `t.Parallel` (matching `TestRegistry_Sweeper_StartsAndStops_NoLeak`).
- **A genuine leak surfaces.** If a covered component leaks on teardown, that is
  a real bug to fix in this PR (CLAUDE.md §17.6 fix-where-you-find-it), not a
  `t.Skip`. Findings are recorded in the PR body and D-135.
- **CI job collision (Wave 14).** Phases 76 and 79 also add a CI job in the same
  wave. Mitigation: the `ci.yml` edit is one self-contained job appended at the
  end of the `jobs:` map — minimal, localised, clean to rebase.

## Glossary additions

- **goroutine-leak conformance harness** — added to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target — N/A, harness ships no
      production package (see Coverage target).
- [x] If multi-isolation paths changed: cross-session isolation test passes —
      N/A, no identity-scoped code path changed; isolation is Phase 76's gate.
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes** —
      N/A, this phase builds a test harness, not a reusable runtime artifact.
- [x] **If this phase consumes a shipped subsystem's surface: an integration
      test exists** — the harness IS the integration test; it wires the real
      Engine / EventBus / sessions.Registry / TaskRegistry drivers end-to-end,
      exercises a teardown failure mode, and runs under `-race`.
- [x] If new vocabulary: glossary updated — "goroutine-leak conformance harness".
- [x] If a brief finding was departed from: justified above + decisions.md entry
      filed — none departed from.
