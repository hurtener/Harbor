// Package benchmarks holds Harbor's V1 performance-benchmark suite
// (Phase 79). It is an `_test.go`-only package: the `Benchmark*`
// functions exercise three of the runtime's hottest seams against
// the REAL components — no mocks, no stubs (CLAUDE.md §13):
//
//   - engine_bench_test.go — engine envelope throughput under N
//     concurrent runs against a single shared *engine.Engine
//     (RFC §6.1).
//   - bus_bench_test.go — event-bus publish/fan-out latency as a
//     function of subscriber count, against the real `inmem`
//     EventBus driver wired with a real audit redactor (RFC §6.13).
//   - memory_bench_test.go — memory-strategy AddTurn latency for the
//     `truncation` and `rolling_summary` strategy executors, wired
//     with a real `inmem` StateStore + EventBus (RFC §6.6).
//   - steering_bench_test.go — steering-ingress round-trip
//     (Inbox.Enqueue + Drain) against the real per-run Inbox, the
//     scope-checked apply path (RFC §6.3).
//   - pauseresume_bench_test.go — pause-checkpoint save latency for
//     Coordinator.Request against a real `inmem` StateStore (RFC §6.4).
//   - react_bench_test.go — single ReAct planner step
//     (Planner.Next) against the real `*react.ReActPlanner`, with a
//     scripted LLM edge (RFC §6.2).
//
// Run the suite with `make bench`; gate a PR against the committed
// baseline (`docs/perf/baseline.txt`) with `make bench-check`. The
// regression gate (`scripts/perf/check-regression.sh`) compares a
// fresh run against the baseline via `benchstat` and fails on a
// statistically-significant slowdown beyond 10%. See
// docs/plans/phase-79-performance-benchmarks.md and D-136.
package benchmarks
