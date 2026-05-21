# Phase 76 — cross-tenant-isolation-harness

## Summary

A master cross-tenant + cross-session isolation conformance harness that
exercises all six identity-scoped subsystems — StateStore, ArtifactStore,
MemoryStore, SkillStore, TaskRegistry, EventBus — simultaneously, under
~100 concurrent sessions doing randomized writes-then-reads, and asserts the
binding invariant: every read returns only data whose `(tenant, user,
session)` tuple exactly matches the caller's. It is the V1 integrity gate;
a regression here is a security bug, not a test flake.

## RFC anchor

- RFC §4.3

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 05 §"Concurrency tests": "N concurrent sessions × M concurrent tasks
  each, asserting no cross-talk in events, memory, artifacts, or task results.
  This is the Harbor analogue of the gateway's cross-tenant isolation gate."
  The harness implements exactly this — 100 concurrent session-workers, each
  hammering all six subsystems, asserting no cross-talk.
- brief 05 §"Cross-tenant isolation": "Storing an artifact under tenant A and
  attempting to read under tenant B fails. Same for tasks, sessions, memory,
  trajectories." `TestE2E_Isolation_CrossScopeReadIsBlind` is the targeted
  positive proof: fixed identities write distinct data; each reads under its
  own scope and sees only its own rows.
- brief 05 §"Conformance test approach": "A single suite drives every scenario
  against any factory." This harness composes the six per-subsystem
  conformance suites into one cross-subsystem soak rather than re-implementing
  per-driver checks — it opens each subsystem through its production registry
  factory (`state.Open`, `artifacts.Open`, …), never a mock.
- brief 06 §124 "Isolation-triple filtering by default": "Subscribe ignores
  any filter that elides Tenant/User/Session unless the caller has admin
  scope." The harness's `EventBus` fail-closed subtest asserts a partial-triple
  `Subscribe` without admin scope is rejected.
- brief 06 §147 "Cross-tenant isolation tests: subscriber for tenant A
  receives zero events emitted by tenant B." Each soak worker subscribes
  scoped to its own triple and asserts every delivered event is
  identity-matched.

## Findings I'm departing from (if any)

None. The harness is a pure composition of patterns the briefs and shipped
per-subsystem conformance suites already established; it introduces no new
design surface, only a cross-subsystem integration gate.

## Goals

- Prove the six identity-scoped subsystems hold the multi-isolation invariant
  *simultaneously* under concurrent load against a single shared driver
  instance of each — not just each subsystem in isolation (the per-subsystem
  suites already cover that).
- Run on every PR as a fast gate, with the master-plan-specified 30 s soak
  available behind an env var for deeper validation.
- Surface any cross-tenant or cross-session bleed loudly, with enough context
  (subsystem, expected vs. observed identity) to triage it as the security
  bug it is.

## Non-goals

- Re-testing each subsystem's full per-driver conformance suite — that lives
  in `internal/<subsystem>/conformancetest` and runs already.
- Postgres / SQLite-file driver coverage of every subsystem — the harness
  runs the in-memory drivers (plus SkillStore's `localdb` SQLite `:memory:`
  driver, its only V1 driver). Per-driver Postgres conformance is gated by
  the existing dedicated CI jobs (`state-postgres`, `memory-postgres`, …).
- Goroutine-leak conformance as a first-class subject — that is Phase 77.
  The harness still asserts goroutine-baseline restoration after its soak as
  a hygiene check, but the dedicated leak harness is a separate phase.
- A new Protocol method or REST endpoint — Phase 76 ships no network surface.

## Acceptance criteria

- [x] A harness exercises StateStore, ArtifactStore, MemoryStore, SkillStore,
  TaskRegistry, and EventBus through their production registry factories
  (real drivers, no mocks at the seam).
- [x] ~100 concurrent session-workers, spread across multiple tenants and
  users, run a randomized write-then-read op-mix against all six subsystems
  for the soak window.
- [x] Every read asserts the recovered identity tuple exactly matches the
  caller's; any cross-scope breach fails the test loudly with a categorized
  report.
- [x] The default soak window is fast (~3 s) so every PR can run it; the
  master-plan 30 s soak is available via `HARBOR_ISOLATION_SOAK=<dur>`;
  `-short` forces the fast window.
- [x] A targeted positive-proof test (`CrossScopeReadIsBlind`) covers the
  cross-session boundary (same tenant+user, different session) and the
  cross-tenant boundary explicitly.
- [x] A fail-closed test asserts every one of the six subsystems rejects an
  incomplete identity triple (the named §17.3 failure mode).
- [x] The harness runs under `-race`; goroutine baseline is restored after
  the soak.
- [x] CI runs the harness on every PR (the `isolation` job in
  `.github/workflows/ci.yml`).

## Files added or changed

```text
test/integration/isolation_conformance_test.go   # the harness (new)
.github/workflows/ci.yml                          # new `isolation` job
scripts/smoke/phase-76.sh                         # smoke skeleton (static SKIP)
docs/plans/phase-76-cross-tenant-isolation-harness.md  # this plan
docs/plans/README.md                              # Phase 76 row → Shipped
docs/decisions.md                                 # D-134
docs/glossary.md                                  # "isolation conformance harness"
README.md                                         # Status table Phase 76 → Shipped
```

No new top-level directory: the harness lives in the existing
`test/integration/` (AGENTS.md §3 / §17.2 — the canonical home for
cross-subsystem tests that span more than two subsystems).

## Public API surface

None. The harness is `_test.go`-only code in package `integration_test`; it
exports no production symbols. Other phases do not depend on it — it is a
terminal gate, not a primitive.

## Test plan

- **Unit:** N/A — the harness IS the test; it has no production code to
  unit-test.
- **Integration:** `TestE2E_Isolation_ConformanceHarness` — 100 concurrent
  sessions across 5 tenants × 4 users, randomized op-mix over all six
  subsystems for the soak window, asserting zero cross-scope bleed.
  `TestE2E_Isolation_CrossScopeReadIsBlind` — targeted positive proof across
  the cross-session and cross-tenant boundaries with fixed identities.
- **Conformance:** the harness is itself the cross-subsystem conformance gate
  composing the six per-subsystem suites; real drivers via `state.Open`,
  `artifacts.Open`, `memory.Open`, `skills.OpenDriver`, `tasks.Open`,
  `events.Open`.
- **Concurrency / leak:** the soak is the concurrency stress (100 shared-
  instance workers under `-race`); `TestE2E_Isolation_ConformanceHarness`
  asserts `runtime.NumGoroutine` returns to baseline after the soak via a
  bounded eventually-poll (no `time.Sleep`).
- **Failure mode (§17.3):** `TestE2E_Isolation_FailClosedOnMissingIdentity` —
  every subsystem rejects an incomplete identity triple.

## Smoke script additions

`scripts/smoke/phase-76.sh` is a `static-only` script that documents the
SKIP: Phase 76 adds no Protocol endpoint or REST surface, so there is nothing
for the live-server smoke to hit. The harness is exercised by `make test`
and the dedicated `isolation` CI job, not by the preflight smoke gate. The
script asserts the harness file exists (a static file-existence check) so a
future deletion is caught, then SKIPs the server surface per the 404/405/501
→ SKIP convention.

## Coverage target

N/A as a per-package line target — the harness is `_test.go`-only and adds
no production code, so it neither moves nor is measured by a package coverage
gate. The binding metric is behavioural: the soak runs ≥ 1000 op-cycles with
zero breaches and the three tests pass under `-race`.

## Dependencies

07 (StateStore), 17 (ArtifactStore), 20 (TaskRegistry), 23 (MemoryStore),
37 (SkillStore), 05 (EventBus) — all shipped.

## Risks / open questions

- **This is the integrity gate.** A regression here is a security bug. The
  harness is deliberately probabilistic in the soak (randomized op-mix) AND
  deterministic in `CrossScopeReadIsBlind` (fixed identities) so a leak is
  caught by at least one path even if scheduling hides it from the other.
- **Soak-window tuning.** Too short and a rare scheduling-dependent leak
  escapes; too long and CI drags. The ~3 s default with 100 workers yields
  ≥ 1000 op-cycles — broad enough that a real cross-scope bug surfaces with
  overwhelming probability. The 30 s soak (`HARBOR_ISOLATION_SOAK=30s`) is
  the deeper net for release validation. See D-134.
- **`ci.yml` collision risk.** Phases 77 (goroutine-leak harness) and 79
  (perf benchmarks) land in parallel and also add a CI job. This phase's
  `ci.yml` edit is a single self-contained `isolation` job appended to the
  jobs map to keep the coordinator's rebase clean.

## Glossary additions

- **isolation conformance harness** — added to `docs/glossary.md` in this PR.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target — N/A; harness is
  `_test.go`-only, adds no production code.
- [x] If multi-isolation paths changed: cross-session isolation test passes —
  the harness IS the cross-session isolation test.
- [x] If this phase builds a reusable artifact: concurrent-reuse test passes —
  N/A; the harness builds no reusable production artifact. It DOES run a
  100-way concurrent-reuse soak against the six shipped artifacts' single
  shared instances, which is the cross-subsystem analogue of each
  subsystem's own D-025 test.
- [x] If this phase consumes a shipped subsystem's surface: an integration
  test exists, wires real drivers end-to-end, asserts identity propagation,
  covers ≥ 1 failure mode, runs under `-race` — yes, this is the whole phase.
- [x] If new vocabulary: glossary updated — "isolation conformance harness".
- [x] If a brief finding was departed from: N/A — no departures.
