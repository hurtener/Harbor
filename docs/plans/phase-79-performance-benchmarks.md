# Phase 79 — performance-benchmarks

## Summary

Phase 79 ships a Go benchmark suite (`go test -bench`) over Harbor's three
hottest runtime seams — engine envelope throughput, event-bus fan-out latency,
and memory-strategy add-turn latency — plus a perf-regression gate that fails a
PR when a benchmark slows beyond a noise-tolerant threshold. Baseline numbers
are committed under `docs/perf/`; the gate compares a PR-branch run against the
committed baseline via `golang.org/x/perf/cmd/benchstat` invoked from a script,
and is wired into CI as one additive job.

## RFC anchor

- RFC §6.1 — runtime engine (envelope throughput under N concurrent runs;
  per-run streaming/backpressure path).
- RFC §6.13 — typed event bus (publish → server-side filter → per-subscriber
  fan-out; the benchmarked subscriber-count vs latency curve).
- RFC §6.6 — memory subsystem (the `truncation` vs `rolling_summary`
  strategy executors whose `AddTurn` latency is benchmarked).

## Briefs informing this phase

- brief 01
- brief 06

## Brief findings incorporated

- brief 01 §"Backpressure inside streaming": "A run that emits hundreds of
  stream frames could fill its outgoing queue and block the producing
  goroutine ... Without it, parallel runs can deadlock each other through
  shared bounded queues." → the engine throughput benchmark drives **N
  concurrent runs** (not one) so the measured number reflects the
  shared-bounded-queue contention path, and a dedicated streaming-throughput
  benchmark exercises `EmitChunk` under the per-run capacity waiter.
- brief 01 §"Validate is per-node": "the perf escape hatch (`none` on hot
  streaming paths) is necessary, keep it." → the engine throughput benchmark
  runs nodes with the zero-value `NodePolicy` (validate `none`) so the number
  measures the engine's intrinsic dispatch cost, not JSON-schema validation.
- brief 06 §"Fan-out": "Publish is O(1) ... A bus dispatcher fans out to
  per-subscription channels with non-blocking sends (drop-oldest if full)." →
  the bus benchmark sweeps subscriber count (1, 8, 16) and asserts the
  fan-out curve, confirming publish stays near-constant as subscribers grow
  and that drop-oldest does not turn into producer blocking.
- brief 06 §"Filter expressions": "Filters are evaluated server-side before
  fan-out. Cardinality of subscribers is bounded per session (configurable;
  default ~16)." → the bus benchmark caps its sweep at the default
  `MaxSubscribersPerSession` (16) rather than benchmarking an
  operationally-impossible subscriber count.

## Findings I'm departing from (if any)

No *brief* finding is departed from. One **master-plan acceptance-line**
calibration is recorded for transparency: the master-plan detail block gives
"perf regression threshold gates PRs (e.g. > 10% slowdown blocks)". The gate
ships with a **30%** default threshold, not 10%. This is a calibration of the
master plan's explicitly-illustrative "e.g." figure against measured reality
(see Risks — concurrent engine/bus microbenchmarks swing ±20-30% from machine
contention alone; a 10% gate flakes on every PR). The master plan's binding
requirements — "perf regression threshold gates PRs" *and* "design the gate to
tolerate noise" — are both satisfied; a literal 10% gate cannot satisfy the
second. The threshold is overridable via `PERF_THRESHOLD`. Recorded in D-136.

## Goals

- A `Benchmark*` suite that calls the **real** engine, event bus, and memory
  strategy executors — no mocks, no stubs, per CLAUDE.md §13.
- Engine throughput: envelopes/sec measured under N concurrent runs against a
  single shared `Engine` (the D-025 reuse contract is the realistic shape).
- Bus fan-out: publish latency as a function of subscriber count.
- Memory-strategy latency: `AddTurn` latency for `truncation` vs
  `rolling_summary`.
- A committed baseline file and a regression gate that fails a PR on a
  statistically-significant slowdown past a noise-tolerant threshold,
  designed not to flake on shared-CI-runner benchmark noise (see Risks for
  why the threshold is 30%, not the master plan's illustrative "10%").
- The gate wired into `.github/workflows/ci.yml` as one additive job.

## Non-goals

- Profiling / flame-graph tooling, continuous perf dashboards, or a
  perf-history time series — out of scope for V1.
- Benchmarking every subsystem. The three seams named in the master-plan
  detail block (engine / bus / memory) are the V1 surface; other subsystems
  get benchmarks when a perf concern is raised.
- Auto-refreshing the committed baseline. The baseline is updated
  deliberately, by a human, in a reviewed PR (see "Risks" — a silent
  auto-refresh would defeat the gate).
- Adding `benchstat` to the runtime binary's production dependency surface.
  It is a dev/CI-only tool invoked via `go run` with a pinned version; it
  never appears in `go.mod`'s production `require` of the `harbor` binary.

## Acceptance criteria

- [ ] `go test -bench=. -run=^$ ./test/benchmarks/...` runs the engine, bus,
      and memory benchmarks to completion with real components.
- [ ] `BenchmarkEngineThroughput` drives ≥2 concurrent-run counts and reports
      `envelopes/sec` via a custom metric.
- [ ] `BenchmarkBusFanOut` sweeps subscriber counts {1, 8, 16} and reports
      per-publish latency for each.
- [ ] `BenchmarkMemoryStrategy` covers both `truncation` and `rolling_summary`
      `AddTurn` paths.
- [ ] `docs/perf/baseline.txt` is committed with baseline numbers produced by
      the suite.
- [ ] `scripts/perf/check-regression.sh` compares a fresh run against the
      committed baseline via `benchstat` and exits non-zero when any
      benchmark regresses beyond the threshold (a statistically-significant
      slowdown past the noise-tolerant 30% default — see Risks).
- [ ] `make bench` runs the suite; `make bench-check` runs the regression
      gate.
- [ ] `.github/workflows/ci.yml` gains exactly one additive job
      (`perf-regression`) that runs the gate.
- [ ] `make vet test lint drift-audit check-mirror preflight` all pass.

## Files added or changed

```text
docs/plans/phase-79-performance-benchmarks.md   # this plan
docs/plans/README.md                            # Phase 79 row + detail block → Shipped
docs/decisions.md                               # D-136
docs/glossary.md                                # "baseline (perf)", "perf-regression gate"
docs/perf/baseline.txt                          # committed baseline numbers
README.md                                       # Status table → Phase 79 Shipped
Makefile                                        # `bench` + `bench-check` targets
scripts/perf/check-regression.sh                # the regression gate
scripts/smoke/phase-79.sh                        # documented SKIP (no Protocol surface)
test/benchmarks/engine_bench_test.go             # engine throughput benchmark
test/benchmarks/bus_bench_test.go                # bus fan-out benchmark
test/benchmarks/memory_bench_test.go             # memory-strategy latency benchmark
test/benchmarks/doc.go                           # package doc comment
.github/workflows/ci.yml                         # +1 job: perf-regression
```

No new top-level directory: `test/benchmarks/` is a subdir of the existing
`test/` tree; `docs/perf/` is a subdir of the existing `docs/` tree; both are
permitted by AGENTS.md §3 ("`docs/` or `test/` subdirs are fine").

## Public API surface

None. Phase 79 adds no exported runtime types, no Protocol method, no config
key. The benchmark functions live in an `_test.go`-only package and are not
importable. `make bench` / `make bench-check` are operator-facing build
targets, not API.

## Test plan

- **Unit:** N/A — the deliverable *is* benchmarks. The benchmark functions
  themselves run under `go test` (the standard-library `testing` harness
  treats `Benchmark*` as part of the package); a smoke `go test -bench=.
  -benchtime=1x -run=^$` invocation in `scripts/perf/check-regression.sh`'s
  CI step confirms they compile and execute.
- **Integration:** Phase 79's `Dependencies` are shipped phases (10, 12, 05,
  24), and the benchmark suite *is* a cross-subsystem integration exercise —
  it wires the real engine, the real `inmem` event bus (with a real
  `audit` redactor), and the real memory strategy executors (with a real
  `inmem` `StateStore`) end-to-end. The benchmarks pass identity through
  every layer (the engine envelopes carry the triple; the memory `AddTurn`
  calls carry an `identity.Quadruple`). This satisfies the §17 integration
  obligation without a separate `test/integration/` file: the seam is
  exercised under real drivers with identity propagation.
- **Conformance:** N/A — no new driver-shaped interface.
- **Concurrency / leak:** `BenchmarkEngineThroughput` runs N concurrent runs
  against a single shared `Engine` — the D-025 concurrent-reuse shape — and
  the existing engine package's concurrent-reuse + leak tests already gate
  that artifact. Phase 79 builds no new reusable artifact, so no new
  concurrent-reuse test is required (the benchmark is a consumer of an
  already-gated artifact).

## Smoke script additions

`scripts/smoke/phase-79.sh` is a documented SKIP: Phase 79 adds no Protocol
endpoint, no REST surface, and no CLI subcommand, so there is no live-server
surface to assert. The script classifies as `static-only`, prints a one-line
explanation, and calls `skip` — the correct and expected shape for a
benchmark-only phase (AGENTS.md §4.2: "A SKIP that should be an OK is a bug"
— here it genuinely should be a SKIP, because there is no surface).

## Coverage target

N/A — benchmark code is exercised by `go test -bench`, not by a coverage
metric. The benchmarked packages (`internal/runtime/engine`,
`internal/events`, `internal/memory/strategy`) keep their existing
phase-mandated coverage targets; Phase 79 does not modify production code,
so no package's coverage moves.

## Dependencies

10 (Engine + workers), 12 (Streaming + backpressure), 05 (EventBus). Phase 24
(memory strategies) is also exercised — it is a transitive dependency of the
memory benchmark and is already shipped.

## Risks / open questions

- **Benchmark noise on shared CI runners — and the 30% threshold.** The
  master-plan acceptance criterion gives "> 10% slowdown blocks" as an
  *illustrative* example ("e.g."). Empirically, Go microbenchmarks of
  Harbor's concurrent engine and bus paths swing ±20-30% run-to-run on a
  loaded / shared machine — measured during this phase's development, a
  self-comparison (same baseline, fresh run) on a contended developer laptop
  produced apparent deltas of +80-100% on the bus benchmark purely from CPU
  contention. A literal 10% gate would flake on every PR. The gate is
  therefore designed to *tolerate* noise, exactly as the master plan's own
  "design the gate to tolerate noise" directive requires: (a) it uses
  `benchstat`, which computes a confidence interval and emits a `~` verdict
  for any change that is NOT statistically significant (`p ≥ 0.05`) — a `~`
  is always a pass; (b) each benchmark runs with `-count=6` so `benchstat`
  has a real sample to compute variance from; (c) the default regression
  threshold is **30%**, generous enough to stay above genuine shared-runner
  jitter while still catching the regression class that matters — a refactor
  that halves throughput (-50%) or doubles latency (+100%); (d) the gate
  fails ONLY on a delta that is *both* statistically significant *and* past
  the 30% threshold; (e) the full human-readable `benchstat` report is
  always printed, so a reviewer can still eyeball a sub-threshold drift the
  gate intentionally lets pass. The threshold is overridable via
  `PERF_THRESHOLD` for a quiet dedicated machine. See D-136.
- **Baseline staleness / cross-hardware comparison.** The committed
  `docs/perf/baseline.txt` is produced on a developer machine; absolute
  numbers differ across hardware, so the baseline's `ns/op` figures are a
  *documented reference*, not an absolute contract. The gate's pass/fail
  decision rides entirely on `benchstat`'s significance test plus the 30%
  band — never on absolute parity with the baseline's hardware. When a
  legitimate perf change lands, the implementer regenerates the baseline
  (`make bench > docs/perf/baseline.txt`) in the same PR and the reviewer
  signs off on the new numbers. The baseline is never auto-refreshed (that
  would silently erase regressions — see Non-goals). If CI shows persistent
  drift unrelated to a code change (e.g. a Go toolchain bump shifts every
  number), the baseline is regenerated in a dedicated `chore` PR.
- **`benchstat` version drift.** The gate pins the tool version — a module
  pseudo-version (`golang.org/x/perf` ships no tagged semver releases), NOT
  `@latest`. The pin lives in `scripts/perf/check-regression.sh` and is
  bumped deliberately.

## Glossary additions

- **baseline (perf)** — the committed reference benchmark numbers
  (`docs/perf/baseline.txt`) the perf-regression gate compares a PR run
  against.
- **perf-regression gate** — the CI job (`perf-regression`) that fails a PR
  when a benchmark shows a statistically-significant slowdown past the
  noise-tolerant 30% threshold versus the committed baseline, using
  `benchstat` confidence intervals to absorb runner noise.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (N/A — no production code
      touched)
- [x] If multi-isolation paths changed: cross-session isolation test passes
      (N/A — no isolation code touched; benchmarks propagate identity but add
      no new identity-scoped path)
- [x] If this phase builds a reusable artifact: concurrent-reuse test passes —
      N/A. Phase 79 builds benchmarks, which are consumers of already-gated
      reusable artifacts; it ships no new reusable artifact.
- [x] If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam: an integration test exists — satisfied. The
      benchmark suite wires the real engine, bus, and memory strategy
      executors end-to-end with real drivers and identity propagation (see
      Test plan → Integration).
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md
      entry filed (N/A — no departures)
