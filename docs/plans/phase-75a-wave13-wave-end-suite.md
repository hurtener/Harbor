# Phase 75a — Wave 13 wave-end Playwright suite + Go integration test

## Summary

Phase 75a is the Wave 13 closeout phase: the wave-end Playwright aggregator
suite at `web/console/tests/wave13.spec.ts` (plus per-page spec enumeration)
that covers every V1 Console page end-to-end, and the Go-side
`test/integration/wave13_test.go` that exercises the consolidated Wave 13
Protocol surface with real drivers (wire-type round-trip + cross-page
identity isolation + N≥10 concurrent SSE subscriber stress). A binding page
coverage check (`make wave13-coverage-check` + `scripts/console/check-page-coverage.sh`)
asserts every per-page spec file in `docs/design/console/page-<slug>.md` has
a matching `*.spec.ts` in `web/console/tests/`. Per §17.5, the
implementation bundles into the final Stage-2.3 phase's PR; this plan file
lands in the same `docs(plans)` PR as the other Wave 13 per-phase plans.

## RFC anchor

- RFC §5
- RFC §7

## Briefs informing this phase

- brief 11
- brief 12
- brief 06

## Brief findings incorporated

- **brief 11 §"Layout decomposition (from the mockup)"** — the 14-page IA
  (Overview, Live Runtime, Sessions, Tasks, Agents, Tools, Events,
  Background Jobs, Flows, Memory, MCP Connections, Artifacts, Settings,
  Playground; Evaluations excluded per D-064) is the binding coverage
  surface. The wave-end suite asserts a navigation round-trip + smoke
  assertion for every one of those 14 pages — none skipped.
- **brief 11 §"CC-2 Identity-aware UI."** — every view respects the JWT's
  identity scope. The Go-side wave-end test enforces this at the
  Protocol seam: cross-tenant calls without an elevated scope claim are
  rejected; cross-session reads from a sibling-session identity are
  rejected; the Console's UI gates are convenience and the runtime is
  the security boundary.
- **brief 11 §"CC-3 Notifications" + §"CC-4 Global search (⌘K)."** —
  cross-page concerns (notifications, search) ride the Protocol's
  `notification.*` event topic + `search.*` methods. The Playwright
  aggregator exercises ⌘K from at least one page and asserts the
  notification ribbon renders on Overview.
- **brief 12 §"Why `harbor console`, not `harbor dev`, serves the
  Console."** — the suite targets `harbor console` (D-091), NOT
  `harbor dev`. `harbor dev` is headless at V1; the wave-end suite that
  boots a Console binary must boot the right subcommand.
- **brief 06 §"Two-channel split" / "Unified bus."** — the Go-side wave
  test consumes the canonical event bus only. No parallel observability
  channel; the concurrent-SSE-subscriber stress uses `events.subscribe`
  (the shipped Protocol surface) — not a private hook.

## Findings I'm departing from (if any)

None. The phase implements brief 11's IA verbatim. The `Evaluations` page
is **explicitly excluded** per D-064 (post-V1 subsystem); that exclusion
is recorded in the coverage-check script's allowlist so the binding
"every page-spec has a matching `*.spec.ts`" rule does not flag a false
positive for the absent Evaluations spec.

## Goals

- A wave-end Playwright aggregator at `web/console/tests/wave13.spec.ts`
  that drives `harbor console` against a fixture runtime (the
  `harbor dev`-booted runtime per §17.5) and walks the 14 V1 pages
  end-to-end: navigate to each page, assert the page loads with no
  console errors, assert at least one page-shaped Protocol call
  succeeded, assert cross-page identity isolation (clicking a
  cross-tenant deep-link from a non-admin scope shows a 403-style
  UI gate).
- A Go-side wave-end integration test at
  `test/integration/wave13_test.go` that uses `harbortest/devstack.Assemble`
  (D-094) to build a real stack, drives the consolidated Wave 13
  Protocol surface (sessions / tasks / tools / agents / flows / memory /
  mcp / artifacts / events / runtime / governance / llm), and covers
  the §17.3 mandatory matrix end-to-end.
- A page-enumeration coverage check (`make wave13-coverage-check`,
  backed by `scripts/console/check-page-coverage.sh`) that asserts:
  for every `docs/design/console/page-<slug>.md` (Evaluations excluded
  per D-064), a matching `web/console/tests/<slug>.spec.ts` exists.
  The script is invoked at the top of the final Stage-2.3 PR's CI
  pipeline AND on every Wave-13 implementation PR — it is the
  operator's §12 lock-in #7 binding amendment expressed as a mechanical
  gate.
- The Go-side test runs under `-race` and asserts: identity propagation
  through every Protocol call; at least one failure mode (cross-tenant
  rejection, missing-identity rejection per D-033, or
  `ErrContextLeak` per D-026); N≥10 concurrent SSE subscribers against
  the consolidated wave's bus return clean (no goroutine leak at
  teardown, no cross-talk, no data race).
- The phase plan file lands in the **same `docs(plans)` PR** as the
  other Wave 13 per-phase plans. The **implementation artifacts**
  (`wave13.spec.ts`, per-page `*.spec.ts` checks, `wave13_test.go`,
  the Makefile target, the coverage-check script) bundle into the
  **final Stage-2.3 phase's PR** per §17.5.

## Non-goals

- A separate "Wave 13 checkpoint audit PR." Per §17.5 the audit lands
  as its own `chore(checkpoint): wave-13 audit fixes` PR after the
  wave-end suite merges; this phase plan does not enumerate audit
  findings (the audit is read-only-forking work).
- Cross-runtime fleet-mode end-to-end coverage. Brief 11 §CC-1 plus
  D-091 carve "cross-runtime fleet view" as Console-side aggregation
  for V1; the wave-end suite exercises single-runtime attach only.
  The fleet seam is covered by a later wave.
- An Evaluations page spec. Evaluations is post-V1 per D-064; its
  exclusion is encoded in the coverage-check script's allowlist.
- New Protocol methods, error codes, or wire types. Every Protocol
  surface the wave-end suite hits is **already shipped** by a prior
  Stage-1 or Stage-2 phase (Phase 72/72a-h/74 for the foundation;
  Phase 73a-n for the per-page methods).
- Visual-regression / screenshot diffing. The aggregator does
  structural assertions only (page loads, an expected DOM element is
  present, no console errors). Pixel-diffing is post-V1.
- Performance benchmarks. The N≥10 concurrent SSE stress is a
  cross-talk / leak gate, not a throughput benchmark. Throughput
  benchmarks live in Phase 79.

## Acceptance criteria

- [ ] `web/console/tests/wave13.spec.ts` exists and asserts a
      navigation + smoke round-trip across all 14 V1 pages (Overview,
      Live Runtime, Sessions, Tasks, Agents, Tools, Events, Background
      Jobs, Flows, Memory, MCP Connections, Artifacts, Settings,
      Playground). Evaluations is explicitly excluded per D-064; a
      top-of-file comment names the carve-out and links to D-064.
- [ ] One per-page spec file exists at `web/console/tests/<slug>.spec.ts`
      for each of the 14 V1 pages. Each per-page spec lands as part of
      its owning Stage-2 phase; `wave13.spec.ts` aggregates over them.
- [ ] `test/integration/wave13_test.go` exists and uses
      `harbortest/devstack.Assemble` (D-094) to build a real Wave 13
      stack — no mocks at the seam. Identity propagation
      (`(tenant, user, session)` triple) is asserted on every Protocol
      call the test makes. Runs under `-race`.
- [ ] The Go-side test covers at least one explicit failure mode:
      either a forced cross-tenant rejection (D-079 scope-claim
      degradation), a forced missing-identity rejection (D-033), OR a
      forced heavy-content `ErrContextLeak` per D-026. The chosen
      mode is named in the test's top-of-file comment.
- [ ] The Go-side test spawns N≥10 concurrent SSE subscribers against
      the consolidated wave's bus (each with a distinct identity
      triple), asserts no goroutine leak after teardown
      (`runtime.NumGoroutine` baseline restored), no cross-talk
      (every subscriber sees only events tagged with its own identity),
      no data races.
- [ ] `scripts/console/check-page-coverage.sh` exists, is executable,
      enumerates `docs/design/console/page-*.md` (Evaluations excluded
      via an allowlist), and asserts a matching
      `web/console/tests/<slug>.spec.ts` exists for each. The script
      exits non-zero if any page-spec has no matching `*.spec.ts`.
- [ ] `make wave13-coverage-check` invokes
      `scripts/console/check-page-coverage.sh` and is wired into the
      `frontend` CI job for the final Stage-2.3 PR.
- [ ] `scripts/smoke/phase-75a.sh` exists, is executable, carries the
      `# PREFLIGHT_REQUIRES: static-only` header, asserts the existence
      of the coverage-check script and the Go integration test file,
      and runs the coverage check when `web/console/tests/` is present.
- [ ] `docs/plans/README.md` Phase 75a row added; status `Pending`
      flips to `Shipped` in the bundling Stage-2.3 PR.
- [ ] Glossary entries added: "wave-end Playwright suite", "page
      coverage check", "Wave 13 wave-end integration test".
- [ ] `docs/decisions.md` D-098 (pre-assigned) appended capturing the
      binding §12-lock-in-#7 page-enumeration rule + the
      `make wave13-coverage-check` mechanical enforcement.

## Files added or changed

```text
docs/plans/phase-75a-wave13-wave-end-suite.md   # this plan
docs/plans/README.md                            # row + master plan index update (in bundling PR)
docs/decisions.md                               # D-098 (in bundling PR)
docs/glossary.md                                # three new terms (in bundling PR)
scripts/smoke/phase-75a.sh                      # static-only smoke
scripts/console/check-page-coverage.sh          # bundling PR adds; enumerates pages
Makefile                                        # bundling PR wires `wave13-coverage-check`
web/console/tests/wave13.spec.ts                # bundling PR; aggregator spec
web/console/tests/<slug>.spec.ts                # per-page (lands in each Stage-2 phase)
test/integration/wave13_test.go                 # bundling PR; Go-side wave-end E2E
.github/workflows/ci.yml                        # bundling PR; wires the coverage check
README.md                                       # Status row flip in bundling PR
```

Implementation files (`wave13.spec.ts`, `check-page-coverage.sh`, the
Makefile target, `wave13_test.go`, `.github/workflows/ci.yml` wiring,
`README.md` row flip, `docs/plans/README.md` row flip, `D-098` entry,
glossary append) ride in the **final Stage-2.3 phase's PR** per §17.5.
This planning file (`phase-75a-wave13-wave-end-suite.md`) and
`scripts/smoke/phase-75a.sh` ride in the present `docs(plans)` PR so
the drift-audit's plan↔smoke pairing invariant is satisfied at every
intermediate Wave 13 PR.

## Public API surface

Phase 75a introduces no new public Go API and no new Protocol surface.
The wave-end suite consumes ONLY surfaces shipped by prior Stage-1 /
Stage-2 phases. Net-new artefacts are:

- `scripts/console/check-page-coverage.sh` — Bash script. Exit 0 on
  full coverage (every non-Evaluations page-spec has a matching
  `*.spec.ts`); non-zero with a precise "missing: `<slug>`" message
  otherwise. The Evaluations exclusion lives in a single named
  `EVALUATIONS_EXCLUDED_PER_D_064=1` shell variable so the carve-out
  is grep-visible.
- `make wave13-coverage-check` — Makefile target wrapping the script.
- `web/console/tests/wave13.spec.ts` — TypeScript Playwright spec.
  Public surface = the suite's named tests; no exported types.
- `test/integration/wave13_test.go` — Go test file. Exports nothing;
  consumed only by `go test`.

## Test plan

- **Unit:** N/A — Phase 75a is a wave-end aggregator. The unit
  coverage of each surface lives in the prior Stage-1 / Stage-2
  phases that ship those surfaces.
- **Integration:**
  - `test/integration/wave13_test.go::TestE2E_Wave13_PerPageProtocolRoundTrip`
    — builds the stack via `harbortest/devstack.Assemble`, drives one
    Protocol round-trip per page-cluster (sessions / tasks / tools /
    agents / flows / memory / mcp / artifacts / events / runtime /
    governance / llm), asserts identity propagation on each call.
    Wire-type round-trip = build a request, send it, decode the
    response, assert the decoded `IdentityScope.{tenant, user, session}`
    matches the caller's triple verbatim.
  - `test/integration/wave13_test.go::TestE2E_Wave13_CrossPageIdentityIsolation`
    — boot two sessions under one tenant, two sessions under another;
    drive page-shaped Protocol calls under one identity and assert
    the other identity's resources are NEVER returned (cross-tenant
    reads return empty; cross-session reads within the same tenant
    return empty for non-admin scope claims; admin scope succeeds with
    an explicit elevated claim per D-079).
  - `test/integration/wave13_test.go::TestE2E_Wave13_FailureMode_<ChosenMode>`
    — exercises the one named failure mode (cross-tenant rejection /
    missing-identity rejection / `ErrContextLeak`). The mode chosen
    is recorded in the top-of-file comment; the test asserts the
    failure shape verbatim (sentinel error, exit code, emitted event).
- **Conformance:** N/A — the wave-end suite IS the conformance gate.
  No new driver seam ships in this phase.
- **Concurrency / leak:**
  - `test/integration/wave13_test.go::TestE2E_Wave13_ConcurrentSSESubscribers`
    — spawns N≥10 concurrent SSE subscribers against the live wave's
    `events.subscribe`, each scoped to a distinct identity triple.
    Each subscriber records the events it receives; the test asserts
    (a) every subscriber's log contains ONLY its own identity's
    events (no cross-talk), (b) goroutine count returns to baseline
    after every subscriber unsubscribes and the bus drains
    (`runtime.NumGoroutine` parity within an explicit grace window),
    (c) no race detector hits.
- **Playwright (web/console/tests):**
  - `wave13.spec.ts::wave-13 navigates the 14 V1 IA pages` — walks
    each page; asserts the page renders, the URL matches the expected
    route, no `console.error` was emitted during the navigation.
  - `wave13.spec.ts::wave-13 identity gate blocks cross-tenant deep-link`
    — opens a deep-link encoding a foreign tenant; asserts the UI
    gate renders the 403 state without 5xx.
  - `wave13.spec.ts::wave-13 ⌘K palette opens and dispatches a search`
    — exercises `search.*` (D-061's cross-cutting search surface) from
    at least one page.
  - `wave13.spec.ts::wave-13 notification ribbon renders on Overview`
    — exercises `notification.*` end-to-end (Overview's intervention
    queue is the canonical first consumer; the spec asserts the
    ribbon DOM is present when the runtime emits a fixture
    notification).

## Smoke script additions

`scripts/smoke/phase-75a.sh` is **static-only**. It does not boot a
server; it does not invoke `playwright test`; it does not run `go test`
(both are exercised by their owning CI jobs in the bundling PR). The
smoke's assertions:

1. `scripts/console/check-page-coverage.sh` exists and is executable
   (`assert_file` + an explicit `[ -x ]` check).
2. `Makefile` declares the `wave13-coverage-check` target
   (`assert_grep_absent` reversed via `grep -q "^wave13-coverage-check:" Makefile`).
3. `test/integration/wave13_test.go` exists (`assert_file`).
4. When `web/console/tests/` is present, the per-page spec coverage
   gate runs: invoke `scripts/console/check-page-coverage.sh` and
   assert exit 0 (`ok`). When `web/console/tests/` is absent (the
   pre-final-Stage-2.3 builds), the gate `skip`s with a clear message.

Per §4.2 convention 4 ("404/405/501 → SKIP so phase-N+1 scripts coexist
with phase-N builds"), the analogue here is "missing files / missing
directory → SKIP" — the smoke MUST stay green at every intermediate
Wave 13 PR, going OK once the bundling Stage-2.3 PR lands.

## Coverage target

N/A — Phase 75a ships no new Go production code. The Go-side test
`test/integration/wave13_test.go` consumes shipped surfaces; the
coverage for each is owned by the originating Stage-1 / Stage-2 phase.
The Playwright suite is a UI / Protocol-surface gate; the §17.3
mandatory matrix is the binding success condition (real drivers /
identity propagation / failure mode / `-race`).

The page-coverage script and the Makefile target carry an
`integration-level` correctness assertion via the bundling PR's
`frontend` CI job (`make wave13-coverage-check` must exit 0). No
statement-level coverage gate applies — neither artefact computes
business logic.

## Dependencies

- 72 — Console subscription protocol surface (`events.subscribe`
  scope + cross-tenant claim per D-079). Foundation for the
  concurrent-SSE-subscriber stress.
- 72a — `events.subscribe` filter extensions + `events.aggregate`.
  Foundation for the Events-page Playwright spec.
- 72b — `IdentityScope` extension (`actor` / `requester` /
  `impersonating`). Foundation for the cross-page identity isolation
  test.
- 72c — `search.*` cluster. Foundation for the ⌘K Playwright assertion.
- 72d — `notification.*` event topic. Foundation for the Overview
  ribbon Playwright assertion.
- 72e — `pause.list` snapshot. Foundation for the Overview
  intervention-queue Playwright assertion.
- 72f — Runtime posture (`runtime.info` / `runtime.health` /
  `runtime.counters` / `runtime.drivers` / `metrics.snapshot`).
- 72g — `governance.posture` + `llm.posture`. Foundation for the
  Settings Playwright assertion.
- 72h — Console DB local schema. Foundation for the saved-view
  Playwright assertion.
- 73a-73n — per-page Protocol + UI bundles (one per page). Each
  Stage-2 phase ships its per-page `*.spec.ts`; this phase aggregates.
- 74 — `topology.snapshot`. Foundation for the Live Runtime
  Playwright assertion.
- 75 — Playwright harness baseline. Provides `npx playwright test`
  wiring + CI hook; this phase ships the wave-end aggregator on top.
- 71 — `harbortest/devstack.Assemble` (D-094, **Shipped**). The Go
  integration test consumes it.

## Risks / open questions

- **Risk: Stage-2 phases ship `*.spec.ts` files with inconsistent
  naming.** Mitigation: the coverage-check script derives the
  expected `*.spec.ts` filename mechanically from
  `docs/design/console/page-<slug>.md`. Each Stage-2 phase's
  acceptance criteria pin the exact name (e.g. `tools.spec.ts` for
  `page-tools.md`). The wave-end aggregator references the per-page
  specs by these names; a mismatch fails the coverage check loud.
- **Risk: the N≥10 concurrent SSE stress flakes under CI load.**
  Mitigation: the test uses bounded `eventually`-style channel reads
  with explicit timeouts (no `time.Sleep` per §17.4); the goroutine
  baseline check happens after an explicit `WaitGroup.Wait()` over
  the subscriber unsubscribe loop, not from a fixed wall-clock delay.
  If flakes surface, the §17.6 rule applies: fix in the same PR,
  no `t.Skip` masking.
- **Risk: the Playwright suite couples to design tokens / Skeleton
  markup that a future restyle changes.** Mitigation: assertions
  target route URLs + data-testid attributes (set in each Stage-2
  phase's page component), NOT raw class names or styled selectors.
  A Skeleton theme refresh that preserves the testids passes the
  suite unchanged.
- **Open question: should the wave-end suite ALSO assert
  `make protocol-ts-gen-check` runs clean (D-093)?** Resolved: no —
  that's enforced by the `frontend` CI job at every Wave 13
  implementation PR. The wave-end suite would be re-asserting a gate
  the bundling PR already passes.
- **Open question: should the Go-side test boot the binary or
  in-process compose the stack?** Resolved: in-process via
  `devstack.Assemble` (D-094). Booting the binary is the Playwright
  suite's job (`harbor console` against a `harbor dev`-booted
  runtime); the Go test exercises the Protocol surface
  programmatically without binary-launch overhead, matching the
  §17.2 in-package shape precedent and the existing
  `test/integration/waveN_test.go` series.
- **Open question: where does the coverage-check script live —
  `scripts/console/` or `scripts/`?** Resolved:
  `scripts/console/` per CLAUDE.md §3's pattern (subsystem-scoped
  scripts colocate with their subsystem). The `console/`
  subdirectory under `scripts/` is new; the bundling PR adds it
  alongside the script and the Makefile target. (CLAUDE.md §3
  lists `scripts/smoke/` + `scripts/hooks/` but does not enumerate
  `scripts/console/`; the bundling PR amends §3's tree if drift
  audit flags the new subdir. The drift-audit's "anything that
  doesn't have a home is wrong" rule is satisfied by the §3
  amendment.)

## Glossary additions

- **wave-end Playwright suite** — the aggregator `*.spec.ts` at
  `web/console/tests/waveN.spec.ts` that drives the Console end-to-end
  across every page shipped in Wave N. Phase 75a ships the Wave 13
  variant.
- **page coverage check** — the mechanical gate at
  `scripts/console/check-page-coverage.sh` (invoked via
  `make wave13-coverage-check`) that asserts every
  `docs/design/console/page-<slug>.md` has a matching
  `web/console/tests/<slug>.spec.ts`. Operator §12 lock-in #7 binding
  amendment expressed as a script; Evaluations excluded per D-064.
- **Wave 13 wave-end integration test** — the Go-side
  `test/integration/wave13_test.go` shipped by Phase 75a. Uses
  `harbortest/devstack.Assemble` (D-094); covers per-page Protocol
  round-trip + cross-page identity isolation + N≥10 concurrent SSE
  subscriber stress + at least one named failure mode.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A: Phase 75a
      ships no new production code (wave-end aggregator + integration
      test only). Marked N/A per §14 carve-out.
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes — `TestE2E_Wave13_CrossPageIdentityIsolation` is the
      regression.
- [ ] **If this phase builds a reusable artifact: concurrent-reuse
      test passes** — N/A: this phase builds no reusable artifact;
      it consumes shipped artifacts via the integration test. Marked
      N/A per §14 carve-out.
- [ ] **If this phase consumes a shipped subsystem's surface OR
      closes a cross-subsystem seam: an integration test exists** —
      `test/integration/wave13_test.go` is the integration test;
      it composes real drivers via `devstack.Assemble` (D-094), the
      §17.3 mandatory matrix is satisfied (identity propagation +
      ≥1 failure mode + N≥10 concurrent SSE stress + `-race`).
- [ ] If new vocabulary: glossary updated — "wave-end Playwright
      suite", "page coverage check", "Wave 13 wave-end integration
      test" all appended to `docs/glossary.md` in the bundling PR.
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed — no departure; D-098 (pre-assigned)
      records the operator §12 lock-in #7 binding mechanism in the
      bundling PR.
