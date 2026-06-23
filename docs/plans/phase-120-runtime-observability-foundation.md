# Phase 120 — runtime-observability-foundation

## Summary

Gives the running `harbor` binary the leak-detection and profiling surface it currently lacks entirely (the 2026-06 audit found zero pprof, no Go/Process runtime collectors on `/metrics`, no `goleak`, six benchmarks). Registers the Go + Process collectors onto the existing per-instance Prometheus registry, adds Harbor-specific runtime gauges (active runs, the now-bounded engine capacity maps, governance cache sizes, dropped-event counts) so Phase 119's retention fixes are observable and regression-guarded, adopts `goleak` in the hot packages, and exposes raw `runtime/pprof` behind a gated loopback listener that is deliberately kept **off** the authenticated Protocol mux.

## RFC anchor

- RFC §6.14
- RFC §10

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §"Observability — runtime health": a long-running agent process needs in-process visibility into goroutine count, heap, and GC behaviour; scrape-based monitoring is the cheapest path and requires the standard Go/Process collectors to be registered, which Harbor's per-registry isolation pattern currently omits.
- brief 06 §"Leak detection in tests": `runtime.NumGoroutine` baselines with coarse tolerances cannot name a leaking stack and cannot run in parallel; `goleak.VerifyTestMain` is the idiomatic backstop that fails the offending package with the leaked goroutine's creation stack.
- brief 06 §"Profiling posture": pprof is an operator/debug artifact, not a product surface; exposing it must never widen the authenticated attack surface. A gated loopback listener separate from the Protocol/Console mux is the correct shape.

## Findings I'm departing from (if any)

None.

## Goals

- `go_goroutines`, `go_memstats_*`, `go_gc_duration_seconds`, and process RSS/FD metrics appear in the `/metrics` body of a running `harbor`, registered onto the same per-instance registry that already isolates tenant metrics.
- A registration **seam** exists onto the per-instance registry. Today that registry is private inside the prometheus driver (`prometheus.go:64`) and surfaced only as a read-only `Gatherer` (`metrics.go:154-159`) — there is no `Registerer` exposed, and the driver cannot import engine/governance/events (layering). This phase adds an explicit seam in `internal/telemetry/metrics.go` (e.g. `WithExtraCollectors(...)` that the driver registers internally, or exposing the `Registerer`) so the assembly can register both the standard Go/Process collectors and the Harbor gauges. **Preferred alternative:** implement the Harbor gauges as OTel *observable instruments* on the existing `MeterProvider` instead of a raw `client_golang` collector — this needs no new seam AND keeps them on the OTLP export path (a raw collector bypasses the OTel MeterProvider, so OTLP-only operators would not see them).
- A small set of Harbor runtime gauges expose the internals Phase 119 just bounded: active run count, engine capacity-map size, governance cache size, and the events bus dropped-message counter — registered so they also flow through the shipped `metrics.snapshot` projection (the `MetricsRegistry`), which is how Phase 121 surfaces them in the Console with **no new Protocol method**. A slow leak shows up on a scrape and a steady-state test can assert the gauge returns to baseline.
- `goleak` is added to the RFC §10 stack table as a test-tooling dependency (RFC §10 closes with "additions require an RFC PR"; CLAUDE.md §13 forbids new deps without it — so the RFC §10 row lands in this PR, not merely a PR-description note), then `goleak.VerifyTestMain` guards the goroutine-heavy packages (engine, steering, pauseresume, events in-mem), with the existing `NumGoroutine` tests retained as a coarse backstop.
- Raw `runtime/pprof` (goroutine / heap / CPU / allocs profiles) is reachable from a running binary via a gated loopback debug listener (`HARBOR_DEBUG_ADDR`), never mounted on the Protocol/Console HTTP mux, and off by default. The structural protection rests on `net/http/pprof` registering its handlers on `http.DefaultServeMux` while the Protocol/serve transport uses its own `http.NewServeMux` (`transports.NewMux`) — so the debug listener must use its own `*http.Server` and the serve path must never pass a nil handler / `DefaultServeMux`.
- A starter benchmark suite covers the hottest paths. Reconcile against the **existing** benchmarks first — `test/benchmarks/engine_bench_test.go` already has `BenchmarkEngineThroughput` / `BenchmarkEngineStreamingThroughput` — and add the missing ones (steering apply, pauseresume checkpoint, react step) without duplicating engine/streaming coverage.

## Non-goals

- Exposing runtime health *through the Protocol to the Console* — that is Phase 121, which routes these gauges through the **already-shipped** `metrics.snapshot` (no new method) and extends the existing health panel. This phase is runtime-local: a scrape endpoint, the `MetricsRegistry` registration, test tooling, and a debug listener.
- Continuous/always-on profiling, flamegraph storage, or a profiling UI. The debug listener serves on-demand profiles for an operator with loopback access.
- Auth on the debug listener beyond loopback-binding + explicit opt-in. It is dev/operator-only by construction; if it is ever to be remote, that is a separate RFC.

## Acceptance criteria

- [ ] A booted `harbor`'s `/metrics` body contains `go_goroutines` and at least one `go_memstats_*` and one process collector metric; asserted by a `metrics_test.go` that scrapes the handler and greps the body.
- [ ] The Go + Process collectors are registered onto the *per-instance* registry (not the global default registry), preserving the existing tenant-isolation property; a test with two registries asserts no cross-registration.
- [ ] Harbor runtime gauges exist for: active run count, engine capacity-map entry count, governance cache entry count, events-bus dropped count. A steady-state test (building on Phase 119) asserts the engine-capacity gauge returns toward baseline after N runs.
- [ ] RFC §10's stack table gains a `go.uber.org/goleak` test-tooling row in this PR; `goleak.VerifyTestMain` then runs in engine, steering, pauseresume, and events-inmem test packages; a deliberately-leaked goroutine in a scratch test is caught (asserted via a negative test).
- [ ] With `HARBOR_DEBUG_ADDR` set to a loopback address, `GET /debug/pprof/goroutine` on that listener returns a profile; with it unset, no pprof handler is reachable on any listener (asserted: the Protocol/serve mux is its own `http.NewServeMux`, not `DefaultServeMux`, and has no `/debug/pprof` route). A non-loopback `HARBOR_DEBUG_ADDR` is rejected at config-validation.
- [ ] At least five `Benchmark*` functions cover the hot paths, reusing the existing `test/benchmarks/engine_bench_test.go` and adding the missing ones (steering/pauseresume/react) without duplicating engine/streaming benchmarks; all run clean under `go test -bench`.
- [ ] The Harbor gauges are registered on the `MetricsRegistry` (so `metrics.snapshot` projects them for Phase 121), via the new telemetry seam — not only on the raw prometheus registry.

## Files added or changed

- `RFC-001-Harbor.md` §10 — add the `go.uber.org/goleak` test-tooling row (the dependency gate).
- `internal/telemetry/metrics.go` — the new registration seam (`WithExtraCollectors`/`Registerer`, OR OTel observable-instrument helpers) so the assembly can register collectors + Harbor gauges onto the per-instance registry / `MeterProvider`.
- `internal/telemetry/drivers/prometheus/prometheus.go` — register `collectors.NewGoCollector()` + `collectors.NewProcessCollector(...)` on the per-instance registry via the seam.
- `internal/telemetry/drivers/prometheus/prometheus_test.go` — `go_goroutines`-present + per-registry isolation assertions.
- `internal/telemetry/runtimegauges/` (new) — the Harbor runtime gauges (OTel observable instruments preferred; else a `prometheus.Collector`), fed by lock-safe accessors on engine / governance / events.
- `internal/runtime/engine/`, `internal/governance/`, `internal/events/` — minimal lock-safe size-accessors (or a registered metric callback) consumed by the gauges; no per-run state added to artifacts (D-025).
- `cmd/harbor/` (debug listener wiring) — `HARBOR_DEBUG_ADDR` gated `net/http/pprof` mux on a separate `*http.Server` bound to loopback; off by default; stderr banner on enable; reuse an existing `isLoopback` helper (`internal/distributed/a2a/security.go:58` / `internal/server/dev_bootstrap.go:246`).
- `internal/config/config.go` + `loader.go` — optional `DebugAddr` field (documented default: empty = disabled); `Validate` rejects a non-loopback address.
- `internal/runtime/engine/` (reconcile with existing `test/benchmarks/engine_bench_test.go`), `internal/runtime/steering/`, `internal/runtime/pauseresume/`, `internal/planner/react/` — the missing benchmarks only (no streaming-bench package; streaming lives in package `engine`).
- `go.mod` / `go.sum` — `go.uber.org/goleak` (test-only).
- `examples/*.yaml` — document `debug_addr` as an optional dev-only field.

## Public API surface

- `prometheus` driver: no signature change (collectors registered internally at construction).
- `runtimegauges`: a constructor that takes the size-accessors and returns a `prometheus.Collector`; consumed only by the telemetry assembly.
- Config: `DebugAddr string` (optional, default empty).

## Test plan

- **Unit:** collector registration + per-registry isolation; gauge values reflect injected sizes; config validates `DebugAddr` (loopback-only when set).
- **Integration:** `/metrics` scrape against a booted handler shows the Go/Process metrics + Harbor gauges; debug listener serves a goroutine profile when enabled and is absent when disabled; identity-scoped metrics still isolate per registry — run under `-race`.
- **Conformance:** N/A.
- **Concurrency / leak:** `goleak.VerifyTestMain` in the five packages; a negative test proving a leaked goroutine is caught.

## Smoke script additions

- `scripts/smoke/phase-120.sh`:
  - (live-server) `assert_json_truthy` / grep that `/metrics` contains `go_goroutines` once the collectors land (SKIP via 404 until then).
  - (live-server) when the preflight env sets a loopback `HARBOR_DEBUG_ADDR`, assert `/debug/pprof/` returns 200 on that listener and assert it is **absent** (404/no route) on the main API mux.
  - (static-only) assert the serve/Protocol mux wiring contains no `pprof` import path (pprof stays off the authenticated mux).

## Coverage target

- `internal/telemetry/drivers/prometheus`: 85%.
- `internal/telemetry/runtimegauges`: 80% (new package).
- `cmd/harbor` debug-listener wiring: 70% (CLI/tooling default).

## Dependencies

- 04 (slog logger + standard attribute set)
- 119 (the engine/governance maps must be *bounded* before a gauge that watches them is meaningful; the steady-state gauge test reuses Phase 119's loop)
- 111f (telemetry assembly — `telemetry.New` in production, where the collectors and gauge collector are wired)
- 72f (the shipped `metrics.snapshot` projection over `MetricsRegistry` that Phase 121 reads — registering the gauges on the `MetricsRegistry` is what makes them visible there)

## Risks / open questions

- **Registration seam (MAJOR — there is none today):** the per-instance registry is private inside the prometheus driver and exposed only as a read-only `Gatherer` (`metrics.go:154-159`); there is no `Registerer`, and the driver can't import engine/governance/events (layering). A seam MUST be added in `metrics.go`. The cleanest resolution is to register the Harbor gauges as **OTel observable instruments** on the existing `MeterProvider` — no new seam needed for them, and it keeps them on the OTLP export path (a raw `client_golang` collector bypasses the OTel `MeterProvider`, so OTLP-only operators would not see Harbor's gauges). The standard Go/Process collectors still need the driver-side seam.
- **Per-registry collector registration:** the Go/Process collectors must register on the per-instance registry, not `prometheus.DefaultRegisterer`, or isolation breaks. The test asserting two-registry independence is the guard.
- **Gauge accessor shape vs D-025:** the gauges must read sizes without adding mutable per-run state to the `Engine`/governance artifacts. Prefer a registered callback that reads under the existing lock (or an `atomic` length counter maintained where the map is already mutated) over a new field that crosses run boundaries.
- **pprof structural protection:** depends on `net/http/pprof` registering on `http.DefaultServeMux` while `transports.NewMux` builds its own `http.NewServeMux`. The static smoke must assert no serve/Protocol listener is constructed with a nil handler / `DefaultServeMux`. (The posture itself is sound — pprof is correctly off the authenticated mux.)
- **Debug listener safety:** loopback-bind enforced at config-validation time (reuse an existing `isLoopback` helper); empty default; stderr banner on enable (mirrors the mock-LLM dev-escape-hatch posture, CLAUDE.md §13).
- **D-251 posture:** **Proposed D-251** records the observability-foundation posture: standard collectors on the per-instance registry via the new `metrics.go` seam; Harbor gauges as OTel observable instruments on the `MeterProvider` (so they reach both `/metrics` and OTLP and the shipped `metrics.snapshot`); gauges read via lock-safe accessors (no per-run artifact state, D-025); and the binding rule that **pprof is never mounted on the Protocol/Console mux** — only the gated loopback debug listener.
- RFC §11: none directly.

## Glossary additions

- **Debug listener** — the optional, loopback-only, off-by-default `net/http/pprof` HTTP server gated by `HARBOR_DEBUG_ADDR`; distinct from the Protocol/serve mux and never authenticated-Protocol-mounted. (Add to `docs/glossary.md`.)
- **Runtime gauge** — a Harbor-specific Prometheus gauge exposing an internal runtime size (active runs, capacity-map size, cache size, dropped count) on the per-instance registry. (Add to `docs/glossary.md`.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: per-registry isolation test passes (no cross-tenant metric bleed)
- [ ] **If this phase builds a reusable artifact:** the `runtimegauges` collector is construct-once/scrape-many — a concurrent-scrape test (N≥100 concurrent `Collect` calls under `-race`) asserts no race and no torn reads.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** integration test wires the real prometheus driver + engine/governance/events accessors, asserts the scrape body, covers the disabled-debug-listener failure mode, under `-race`.
- [ ] If new vocabulary: glossary updated (Debug listener, Runtime gauge)
- [ ] If a brief finding was departed from: justified + decisions.md entry — N/A; proposed D-251 filed at implementation
