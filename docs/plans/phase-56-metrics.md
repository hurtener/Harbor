# Phase 56 — Metrics + OTLP + Prometheus drivers

## Summary

Land OpenTelemetry metrics in `internal/telemetry`: a `MetricsRegistry` that derives canonical counters from `events.Event` records — keyed **only** by `Event.Type` plus the bounded, low-cardinality `Event.Extra` keys `producer` and `node` — so a developer physically cannot tag a metric by `RunID` / `TraceID`. The metric exporter sits behind the same `§4.4` driver seam Phase 55 established (`internal/telemetry/drivers/{otlpmetric,prometheus}/`): OTLP/gRPC is the default, and a built-in Prometheus `/metrics` `http.Handler` ships at V1 for self-hosted setups. A static `go/parser` cardinality-lint test gates CI — it fails the build if any metric label derives from `RunID` / `TraceID` / free-form input.

## RFC anchor

- RFC §6.14
- RFC §6.13
- RFC §11

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- **brief 06 §"Lessons from the predecessor" / "Metrics cardinality footgun" — "In Harbor, `Event.TraceID` is for logs and OTel traces; `MetricsRegistry` derives metrics from `Event.Type`/`NodeName`/`Producer` only, and the metrics derivation layer enforces this — a developer cannot accidentally tag a metric by `RunID` or `TraceID`."** Phase 56 makes this **structural**, not just disciplined: `MetricsRegistry.RecordEvent(ev)` reads `ev.Type` and the two reserved `ev.Extra` keys (`producer`, `node`) — it never touches `ev.Identity` (which is where `RunID` lives). The registry has no code path that can read the run quadruple onto a label. The static cardinality-lint test (`internal/telemetry/cardinalitylint`) is the CI gate brief 06 §"Metrics-cardinality lint test" calls for: a `go/parser` AST walk that fails CI if a metric instrument is created with a label key matching `run_id` / `trace_id` / `span_id` / `task_id` or with a label value sourced from `Event.Identity`.
- **brief 06 §"Key data shapes" — `Event.Extra` is "bounded, low-cardinality; safe for metric labels".** The `events.Event` doc (Phase 05) already reserves `Extra` "for Phase 56's bounded low-cardinality metric labels." Phase 56 consumes that reserved slot: `producer` and `node` are the two canonical label keys read from `Extra`. No `events.Event` struct change is needed — the brief's `Producer` / `NodeName` shape is realised as reserved `Extra` keys, which keeps the cardinality boundary inside the bounded map by construction.
- **brief 06 §"Roadmap" item 7 — "Metrics derivation + Prometheus driver. Cardinality-safe registry; OTLP exporter default; Prometheus exporter as alt driver. ~1 phase."** Phase 56 ships exactly this: the cardinality-safe `MetricsRegistry`, the OTLP/gRPC metric exporter as the default driver, and the Prometheus exporter as the alternate driver — plus the built-in `/metrics` `http.Handler` RFC §6.14 promotes to V1.
- **brief 06 §"Subsystem components" — "`MetricsRegistry` — registers the canonical metrics derived from events. Backed by OTel Metrics SDK; Prometheus exporter as a driver."** The registry is backed by the OTel Metrics SDK (`go.opentelemetry.io/otel/sdk/metric`); the Prometheus exporter is a registered `§4.4` driver, exactly as the brief describes. The registry is built once at boot and shared across every emit path — a D-025 reusable artifact.
- **brief 06 §1 — "one typed event bus … with logging and OpenTelemetry deriving from the same events rather than being parallel paths."** Metrics are a *derivation of the event bus*, not a parallel instrumentation path — the same load-bearing decision Phase 55 applied to traces. `MetricsRegistry` has no public `Counter()` / `Inc()` surface that a subsystem could call directly; its only recording entry point is `RecordEvent(ctx, ev)`. Subsystems emit `events.Event` records; the event-to-metric bridge does the rest.

## Findings I'm departing from (if any)

None. The brief's `Event.Producer` / `Event.NodeName` are realised as the reserved `Event.Extra["producer"]` / `Event.Extra["node"]` keys rather than as new `events.Event` struct fields — this is not a departure: the Phase 05 `events.Event` doc explicitly reserves `Extra` for "Phase 56's bounded low-cardinality metric labels," and keeping the two values inside the bounded `Extra` map is the same cardinality boundary the brief intends, with no `events.Event` shape change.

## Goals

- A `MetricsRegistry` over the OTel Metrics SDK (`go.opentelemetry.io/otel/sdk/metric.MeterProvider`), constructed once via `NewMetricsRegistry(cfg, ...)`, immutable and concurrency-safe (D-025). The second return is a shutdown/flush hook callers defer at process teardown.
- Canonical core counters derived from `events.Event` records via `RegisterEvent(ctx, ev)` — keyed **only** by `event_type` (`ev.Type`), `producer` (`ev.Extra["producer"]`), and `node` (`ev.Extra["node"]`). The registry never reads `ev.Identity`, so a label sourced from `RunID` / `TraceID` is impossible by construction.
- A `§4.4` metric-exporter driver seam: a `MetricExporter` factory + registry; an `otlpmetric` driver (OTLP/gRPC, the default) and a `prometheus` driver (the built-in pull endpoint). Callers depend only on the `telemetry` package; drivers self-register via blank import at `cmd/harbor`. The seam mirrors the Phase 55 `SpanExporter` seam exactly — sibling driver dirs under `internal/telemetry/drivers/`.
- A built-in Prometheus `/metrics` `http.Handler` (`telemetry.PrometheusHandler`) that a future Runtime server (Phase 60+) mounts at `/metrics`. RFC §6.14 promotes this to V1.
- A static cardinality-lint: `internal/telemetry/cardinalitylint` ships `ScanMetricsTree`, a `go/parser` AST walk that fails CI if a metric label key/value derives from `RunID` / `TraceID` / `SpanID` / `TaskID` or from `Event.Identity`. Mirrors the Phase 58 `internal/protocol/singlesource` checker shape.
- Coverage ≥ 85% on `internal/telemetry` per the master plan.

## Non-goals

- **No transport wiring of `/metrics` into a live Runtime server.** There is no `internal/server/` package and `cmd/harbor` is a stub until Phase 09+/60. Phase 56 ships `PrometheusHandler` as a standalone `http.Handler` constructor; the Phase 60+ server bootstrap mounts it at `/metrics`. The handler's first consumer is the Phase 56 unit + integration tests, which exercise it via `httptest` — that discharges the §13 primitive-with-consumer obligation.
- **No new metric instruments beyond the core counters.** Phase 56 ships the cardinality-safe registry + the core event counters. Per-subsystem custom instruments (latency histograms, queue-depth gauges) are added by the owning subsystems in later phases, through the same `RegisterEvent` bridge — not by sprinkling `meter.Counter(...)` calls.
- **No change to `events.Event`.** The `Producer` / `NodeName` values are read from the already-reserved `Event.Extra` keys; no struct field is added.
- **No metric sampling / aggregation-temporality tuning.** The OTel SDK defaults (cumulative temporality, the default delta-vs-cumulative per exporter) are V1; tuning is post-V1.
- **No Console-side metrics view.** The Console consumes metrics via the Protocol / the Prometheus endpoint (Phase 60+); Phase 56 is Runtime-side only.
- **No `TelemetryConfig` field for the metrics exporter driver name.** Exporter selection follows the same `OTelEndpoint`-based rule Phase 55 established: empty `OTelEndpoint` → the Prometheus driver (pull-only, no collector needed); non-empty → the OTLP/gRPC driver. `WithMetricExporterDriver` is a test-only override. This keeps Phase 56 a backward-compatible, zero-config-key addition (CLAUDE.md §10).

## Acceptance criteria

- [ ] `internal/telemetry/metrics.go` defines `MetricsRegistry`, `NewMetricsRegistry(cfg config.TelemetryConfig, opts ...MetricsOption) (*MetricsRegistry, func(context.Context) error, error)` (the second return is the shutdown/flush hook), and the sentinel errors `ErrMetricsNotConfigured` / `ErrMetricExporterUnknown`.
- [ ] `NewMetricsRegistry` selects the metric exporter by `cfg.OTelEndpoint`: empty → the `prometheus` driver (pull-only `/metrics`; no collector needed); non-empty → the `otlpmetric` driver (OTLP/gRPC to that endpoint). An unknown explicitly-configured exporter driver returns a wrapped `ErrMetricExporterUnknown` whose message lists the registered driver names.
- [ ] `MetricsRegistry.RegisterEvent(ctx context.Context, ev events.Event)` increments the canonical `harbor_events_total` counter with labels `event_type` = `ev.Type`, `producer` = `ev.Extra["producer"]` (`"unknown"` when absent), `node` = `ev.Extra["node"]` (`""` when absent). It reads NO field of `ev.Identity`. A `ev.Type` not in the `events` canonical registry still records under `event_type` verbatim (the registry does not re-validate — the bus already did).
- [ ] `MetricsRegistry` exposes NO public per-instrument surface (no exported `Counter` / `Gauge` / `Meter` accessor). The only recording entry point is `RegisterEvent`. Metrics are a derivation of the event bus (brief 06 §1).
- [ ] `telemetry.PrometheusHandler(reg *MetricsRegistry) (http.Handler, error)` returns the `http.Handler` that serves the Prometheus text exposition format. It returns a wrapped error when `reg` was not constructed with the `prometheus` exporter driver active (the pull handler needs the Prometheus exporter's registry). The handler is safe for concurrent use.
- [ ] `§4.4` seam: `MetricExporter` interface lives in the `telemetry` package; `otlpmetric` and `prometheus` drivers live in `internal/telemetry/drivers/{otlpmetric,prometheus}/`; each self-registers from `init()`; a factory in `internal/telemetry/metrics.go` dispatches by name; the factory's error lists registered drivers. Nothing imports a concrete driver except `cmd/harbor` (blank import) and that driver's own tests.
- [ ] No package-level mutable metrics state. There is NO `telemetry.DefaultMetricsRegistry` package var and NO setter on `*MetricsRegistry`. The metric-exporter registry is the §4.4-style write-once driver registry (the documented exception). `NewMetricsRegistry` does NOT touch any OTel global — it builds a private `MeterProvider`, never `otel.SetMeterProvider` (which would be a parallel global channel; the registry is passed explicitly).
- [ ] Static cardinality-lint: `internal/telemetry/cardinalitylint/cardinalitylint.go` defines `ScanMetricsTree(telemetryRoot string) ([]Violation, error)` — a `go/parser` AST walk that returns a `Violation` for any metric-label literal or `Event.Identity`-sourced label value matching the forbidden set (`run_id`, `trace_id`, `span_id`, `task_id`, and any selector on an `events.Event`'s `Identity` field). `TestCardinalityLint_TelemetryTreeIsClean` runs it against the real `internal/telemetry` tree and fails CI on any violation. A negative test (`cardinalitylint_violation_test.go` with a `testdata/` fixture) proves the checker actually catches a `RunID`-labelled instrument.
- [ ] Sentinel errors: `ErrMetricsNotConfigured` (invalid config — empty `ServiceName`), `ErrMetricExporterUnknown` (unknown explicitly-configured exporter driver). Callers compare via `errors.Is`.
- [ ] Concurrent-reuse test (D-025) — `TestConcurrentReuse_MetricsRegistry`: N≥100 goroutines each call `RegisterEvent` against a single shared `*MetricsRegistry` with goroutine-unique identity quadruples AND goroutine-unique `Extra` producer/node values, under `-race`. Asserts: no data races, no label cross-talk (the recorded series for goroutine G carries G's `producer`/`node` and never G's `run_id`), no goroutine leak (`runtime.NumGoroutine()` baseline-restored after shutdown).
- [ ] Integration test (`test/integration/phase56_metrics_test.go`, `TestE2E_Phase56_*`) — real `events.EventBus` (inmem driver) + real `telemetry.Logger` + real `*MetricsRegistry` backed by the real `prometheus` driver. Asserts: events published on the bus → `RegisterEvent` → the `/metrics` `httptest` response contains `harbor_events_total{event_type="...",producer="...",node="..."}` with the right count; the `/metrics` body contains NO `run_id` / `trace_id` substring even though the published events carry full identity quadruples; identity propagates through the bus layer but is dropped at the metric boundary; ≥1 failure mode (`ErrMetricExporterUnknown` on a bad driver name, or `PrometheusHandler` on an OTLP-backed registry); an N≥10 concurrency stress; runs under `-race`.
- [ ] Coverage on `internal/telemetry` ≥ 85%.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-56.sh` present, executable, runs the metrics + driver + cardinality-lint tests + the integration test under `-race`, and exercises the `/metrics` endpoint shape via the Go integration test; reports `OK` ≥ the acceptance-criteria count it covers, `FAIL` = 0. There is no live Runtime `/metrics` HTTP surface yet (the server bootstrap is Phase 60+) — the live-endpoint assertion `SKIP`s per the 404/405/501 convention.

## Files added or changed

- `internal/telemetry/metrics.go` (new) — `MetricsRegistry` + `NewMetricsRegistry` + the `MetricExporter` interface + the exporter factory/registry + `RegisterEvent` + `PrometheusHandler` + the core-counter definitions + sentinel errors.
- `internal/telemetry/metrics_test.go` (new) — unit tests: exporter selection, `RegisterEvent` label derivation, the "no `Identity` field is read" assertion, `PrometheusHandler` shape, the D-025 concurrent-reuse test.
- `internal/telemetry/drivers/otlpmetric/otlpmetric.go` (new) — the OTLP/gRPC `MetricExporter` driver (default; opt-in via `OTelEndpoint`).
- `internal/telemetry/drivers/otlpmetric/otlpmetric_test.go` (new).
- `internal/telemetry/drivers/prometheus/prometheus.go` (new) — the Prometheus `MetricExporter` driver (the built-in pull endpoint; default when `OTelEndpoint` is empty).
- `internal/telemetry/drivers/prometheus/prometheus_test.go` (new).
- `internal/telemetry/cardinalitylint/cardinalitylint.go` (new) — `ScanMetricsTree` + `Violation` + the forbidden-label set + the `go/parser` AST walk.
- `internal/telemetry/cardinalitylint/cardinalitylint_test.go` (new) — `TestCardinalityLint_TelemetryTreeIsClean` (the build-gating clean-tree lint) + the per-kind detection tests + the negative tests over the `testdata/badmetric` fixture proving the checker catches a `RunID`-labelled instrument and flags exactly two violations (the span-like attribute slice is left alone).
- `internal/telemetry/cardinalitylint/internal_test.go` (new) — unit tests for the unexported predicates (`identityFieldSelector`, `renderSelectorBase`, `isMetricWithAttributes`, `scanAttributeCall`).
- `internal/telemetry/cardinalitylint/testdata/badmetric/badmetric.go` (new) — the fixture: a `RunID`-labelled instrument, NOT compiled into the build (under `testdata/`).
- `cmd/harbor/main.go` (changed) — blank-import the two metric-exporter drivers for self-registration, alongside the existing `noop` / `otlp` span-exporter imports.
- `test/integration/phase56_metrics_test.go` (new) — the cross-subsystem integration test (events bus + logger + registry + `/metrics` `httptest`).
- `scripts/smoke/phase-56.sh` (new) — smoke script.
- `docs/plans/phase-56-metrics.md` (this file).
- `docs/decisions.md` (changed) — D-076 entry.
- `docs/glossary.md` (changed) — `MetricsRegistry`, `metrics cardinality lint`, `harbor_events_total` entries.
- `README.md` + `docs/plans/README.md` (changed) — Phase 56 status flipped to Shipped.
- `go.mod` / `go.sum` (changed) — `go.opentelemetry.io/otel/sdk/metric`, `.../otel/exporters/otlp/otlpmetric/otlpmetricgrpc`, `.../otel/exporters/prometheus` promoted to direct dependencies (and the transitive `github.com/prometheus/*` deps the Prometheus exporter pulls). RFC-sanctioned: RFC §6.14 names the OTel metrics SDK + the built-in Prometheus `/metrics` endpoint explicitly.

No top-level directory additions; `internal/telemetry/` is already enumerated in CLAUDE.md §3, and `internal/telemetry/drivers/` + `internal/telemetry/cardinalitylint/` follow the §4.4 `drivers/<driver>/` convention and the Phase 58 `internal/protocol/singlesource/` lint-package precedent respectively.

## Public API surface

```go
package telemetry

import (
    "context"
    "errors"
    "net/http"

    sdkmetric "go.opentelemetry.io/otel/sdk/metric"

    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/events"
)

// MetricsRegistry is the canonical OTel metrics wrapper. Built once at
// boot via NewMetricsRegistry; safe for concurrent use; immutable after
// construction (D-025). Metrics are a derivation of the event bus —
// RegisterEvent is the only recording entry point.
type MetricsRegistry struct { /* unexported fields */ }

// MetricExporter is the §4.4 seam: drivers (otlpmetric, prometheus)
// self-register and a factory dispatches by name. A driver returns a
// ready OTel SDK metric.Reader (the prometheus driver's Reader is also
// an http.Handler source; otlpmetric's is a PeriodicReader).
type MetricExporter interface {
    Reader(ctx context.Context, cfg config.TelemetryConfig) (sdkmetric.Reader, error)
}

// RegisterMetricExporter installs a MetricExporter driver. Called from
// a driver's init(). Re-registering the same name is a no-op; an empty
// name panics.
func RegisterMetricExporter(name string, e MetricExporter)

// MetricsOption configures the MetricsRegistry at construction.
type MetricsOption func(*metricsConfig)

var (
    // ErrMetricsNotConfigured — invalid TelemetryConfig at construction.
    ErrMetricsNotConfigured = errors.New("telemetry: metrics registry not configured")
    // ErrMetricExporterUnknown — an explicitly-configured exporter
    // driver is not registered. The message lists registered names.
    ErrMetricExporterUnknown = errors.New("telemetry: unknown metric exporter driver")
)

// NewMetricsRegistry constructs a MetricsRegistry from validated config.
// The returned shutdown func flushes and shuts down the reader +
// provider. Empty cfg.OTelEndpoint selects the prometheus exporter; a
// non-empty endpoint selects otlpmetric.
func NewMetricsRegistry(cfg config.TelemetryConfig, opts ...MetricsOption) (*MetricsRegistry, func(context.Context) error, error)

// RegisterEvent increments the canonical harbor_events_total counter
// from ev: labels are event_type (ev.Type), producer
// (ev.Extra["producer"], "unknown" when absent), node
// (ev.Extra["node"], "" when absent). It reads NO field of ev.Identity
// — the run quadruple cannot reach a metric label.
func (r *MetricsRegistry) RegisterEvent(ctx context.Context, ev events.Event)

// PrometheusHandler returns the http.Handler that serves the Prometheus
// text exposition format for reg. Errors when reg was not built with
// the prometheus exporter driver active.
func PrometheusHandler(reg *MetricsRegistry) (http.Handler, error)
```

The exported surface above plus the two sentinel errors is the entire Phase 56 API. Internal types (`metricsConfig`, the exporter registry map, the core-counter holder) stay unexported.

```go
package cardinalitylint

// Violation is a single metrics-cardinality breach: a metric label
// derived from a forbidden high-cardinality source.
type Violation struct {
    File   string
    Line   int
    Kind   string // KindForbiddenLabelKey | KindIdentitySourcedLabel
    Detail string
}

// ScanMetricsTree walks the Go source tree rooted at telemetryRoot and
// returns every cardinality Violation. Pure function, no package-level
// mutable state, safe for concurrent use (D-025).
func ScanMetricsTree(telemetryRoot string) ([]Violation, error)
```

## Test plan

- **Unit:**
  - `NewMetricsRegistry` happy path: empty `OTelEndpoint` → a non-nil `*MetricsRegistry` backed by the `prometheus` reader; the shutdown func is non-nil and returns nil.
  - `NewMetricsRegistry` with a non-empty `OTelEndpoint` → the `otlpmetric` reader (the OTLP/gRPC exporter connects lazily, so no live collector is needed at construction).
  - `NewMetricsRegistry` with `ServiceName == ""` → `ErrMetricsNotConfigured`.
  - Exporter selection: an explicitly-configured unknown driver name → `ErrMetricExporterUnknown` whose message contains `otlpmetric` and `prometheus`.
  - `RegisterEvent` label derivation: an `events.Event` with `Extra{"producer":"planner","node":"react"}` produces a `harbor_events_total` series with `event_type` / `producer` / `node` matching; an event with no `Extra` produces `producer="unknown"`, `node=""`.
  - **Cardinality boundary**: `RegisterEvent` is called with an `events.Event` carrying a full identity quadruple (non-empty `RunID`); the recorded series carries NO `run_id` label and no label value equal to the `RunID`. Asserted by reading the Prometheus exposition text and substring-checking.
  - `PrometheusHandler` on a `prometheus`-backed registry → a non-nil handler; an `httptest` GET returns 200 with `text/plain` and a body containing `harbor_events_total`.
  - `PrometheusHandler` on an `otlpmetric`-backed registry → a wrapped error (the pull handler needs the Prometheus reader).
  - `otlpmetric` driver: `Reader` with a syntactically-valid endpoint returns a non-nil `sdkmetric.Reader`.
  - `prometheus` driver: `Reader` returns a non-nil reader that also yields a usable `http.Handler`.
  - Cardinality-lint per-kind: `ScanMetricsTree` on a fixture with a `run_id`-keyed instrument → a `KindForbiddenLabelKey` violation; on a fixture labelling with `ev.Identity.RunID` → a `KindIdentitySourcedLabel` violation; on the clean `internal/telemetry` tree → zero violations.
- **Integration** (`test/integration/phase56_metrics_test.go`, real drivers on the seam, under `-race`):
  - Real `events.EventBus` (inmem driver) + real `telemetry.Logger` + real `*MetricsRegistry` backed by the real `prometheus` driver.
  - Publish N events on the bus across two tenants → `RegisterEvent` each → the `/metrics` `httptest` response contains `harbor_events_total` with the expected aggregate count per `event_type` / `producer` / `node`.
  - **Cardinality boundary end-to-end**: the published events carry full identity quadruples (distinct `RunID`s per run); assert the `/metrics` body contains NO `run_id` / `trace_id` / `task_id` substring.
  - Identity propagation: identity flows ctx → event → the bus subscriber sees it; assert it is DROPPED at the metric boundary (the metric series is identity-free) — the metric boundary is the cardinality firewall.
  - Failure mode: `NewMetricsRegistry` with an explicitly-configured bogus exporter driver → `ErrMetricExporterUnknown`; and `PrometheusHandler` on an `otlpmetric`-backed registry → a wrapped error.
  - Concurrency stress: N≥10 concurrent producers publish events + `RegisterEvent` against the one shared `*MetricsRegistry`; assert the final `/metrics` counts are exact (no lost increments), no goroutine leak after shutdown.
- **Conformance:** N/A — the `MetricExporter` seam has two drivers but the OTel SDK's `metric.Reader` is itself the conformance contract (the SDK exercises it). The driver tests assert each driver returns a usable `sdkmetric.Reader`. Same rationale as Phase 55's `SpanExporter` seam.
- **Concurrency / leak (D-025):** `TestConcurrentReuse_MetricsRegistry` — N≥100 goroutines, each with a goroutine-unique identity quadruple AND goroutine-unique `Extra` producer/node, call `RegisterEvent` against one shared `*MetricsRegistry` under `-race`. Assertions: no data races (`-race` gate), no label cross-talk (each goroutine's series carries its own `producer`/`node`, never another's, and never any `run_id`), `runtime.NumGoroutine()` baseline-restored within 2s of `wg.Wait()` + registry shutdown.

## Smoke script additions

`scripts/smoke/phase-56.sh`:

- Runs `go test -race ./internal/telemetry/...` and records `OK` when the metrics + driver + cardinality-lint tests pass, `FAIL` otherwise.
- Runs `go test -race -run TestE2E_Phase56 ./test/integration/` and records `OK` on the integration test (which exercises the `/metrics` `httptest` surface end-to-end).
- The script has no live Runtime `/metrics` endpoint to hit — the Runtime server bootstrap that mounts `PrometheusHandler` at `/metrics` is Phase 60+. The script sources `scripts/smoke/common.sh`, runs the Go tests, and `skip`s the live-endpoint line per the 404/405/501 convention, then `smoke_summary`. Identical shape to `phase-55.sh` (Go-package phase, no live endpoint).

## Coverage target

- `internal/telemetry`: 85% (master plan). The new files (`metrics.go`) plus the two driver packages and the `cardinalitylint` package are covered by `metrics_test.go`, the driver tests, the cardinality-lint tests, and the integration test.

## Dependencies

- Phase 55 (telemetry — the `SpanExporter` §4.4 seam shape Phase 56's `MetricExporter` seam mirrors; the `internal/telemetry/drivers/` layout; the `TelemetryConfig`-based exporter-selection idiom).
- Phase 05 (events `EventBus` + `Event` — `RegisterEvent` derives metrics from `events.Event`; `Event.Extra` is the reserved low-cardinality label slot Phase 56 consumes).

Wave placement: Phase 56 is the second (final) phase of the telemetry-completion wave (55 OTel traces, 56 metrics). It depends only on already-shipped phases.

## Risks / open questions

- **The OTel metrics SDK + Prometheus exporter are new direct dependencies.** `go.opentelemetry.io/otel/metric` was already an indirect dependency; Phase 56 promotes `go.opentelemetry.io/otel/sdk/metric`, `.../exporters/otlp/otlpmetric/otlpmetricgrpc`, and `.../exporters/prometheus` to direct, and the Prometheus exporter transitively pulls `github.com/prometheus/client_golang` + siblings. This is RFC-sanctioned — RFC §6.14 names the OTel metrics SDK AND the built-in Prometheus `/metrics` endpoint explicitly. No RFC PR needed; the PR description notes the promotion with this rationale.
- **The `otlpmetric` driver's live behaviour is hard to unit-test without a collector.** Mitigation: same as Phase 55's `otlp` span driver — the driver test asserts `Reader` returns a non-nil `sdkmetric.Reader` for a syntactically-valid endpoint (the OTLP/gRPC exporter connects lazily). A real collector run is an operator smoke, not a CI gate; the integration test uses the `prometheus` driver (fully in-process, no collector) to assert metric shape end-to-end.
- **The `/metrics` endpoint is not yet wired into a live server.** There is no `internal/server/` package; `cmd/harbor` is a stub. Phase 56 ships `PrometheusHandler` as a standalone `http.Handler` constructor — the Phase 60+ server bootstrap mounts it. This is the same pattern Phase 55's propagation carriers used (standalone helpers, wired by a later/other phase). The §13 primitive-with-consumer obligation is discharged by the Phase 56 unit + integration tests exercising the handler via `httptest`.
- **Cardinality discipline relies on the lint catching label literals AND `Identity`-sourced label values.** Mitigation: the lint is structural in two layers — (1) `MetricsRegistry.RegisterEvent` has no code path that reads `ev.Identity`, so the *production* boundary is closed by construction; (2) `ScanMetricsTree` AST-walks for both forbidden label-key literals and selectors on an `events.Event`'s `Identity` field, so a future hand-rolled instrument that tries to add a `run_id` label fails CI. The negative test (`cardinalitylint_violation_test.go` + `testdata/badmetric`) proves the checker actually catches the violation it claims to.
- RFC §11's settled questions: the master-plan Phase 56 row cites "§11 Q-5 settled". RFC-001 §11's Q-5 is the *skill-versioning* question — unrelated to metrics. The metrics-exporter question brief 06 raised is **brief 06 Q-2**, which RFC §6.14 resolves: "Metrics exporter: OTLP default. A built-in Prometheus `/metrics` endpoint ships at V1 … (Resolves brief 06 Q-2)." The master-plan "§11 Q-5" citation is therefore read as "the §11-tracked metrics-exporter question is settled" — the substantive resolution is RFC §6.14 / brief 06 Q-2. This is a §4.3 citation clarification, documented in D-076; no design departure.

## Glossary additions

- **`MetricsRegistry`** — Harbor's OpenTelemetry metrics wrapper (`internal/telemetry`). Wraps the OTel Metrics SDK `MeterProvider`; derives canonical counters from `events.Event` records via `RegisterEvent` — keyed **only** by `event_type` / `producer` / `node`, never by any `Event.Identity` field, so the run quadruple cannot reach a metric label. Built once at boot; immutable (D-025). The metric exporter sits behind a §4.4 driver seam (`otlpmetric` default, `prometheus` for the built-in `/metrics` pull endpoint). RFC §6.14, brief 06, D-076.
- **metrics cardinality lint** — Harbor's build-gating static check (`internal/telemetry/cardinalitylint`) that fails CI if any metric label derives from a high-cardinality source — `run_id`, `trace_id`, `span_id`, `task_id`, or a value sourced from `events.Event.Identity`. A `go/parser` AST walk, mirroring the Phase 58 `internal/protocol/singlesource` checker. It makes the brief 06 "metrics cardinality footgun" lesson mechanically un-violatable. brief 06 §"Metrics-cardinality lint test", D-076.
- **`harbor_events_total`** — the canonical Phase 56 core counter: total `events.Event` records observed by the `MetricsRegistry`, labelled `event_type` / `producer` / `node`. The first metric derived through the event-to-metric bridge; per-subsystem instruments land later through the same bridge, never through a direct `meter.Counter(...)` call. RFC §6.14, D-076.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/telemetry` ≥ 85%
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the concurrent-reuse + integration tests assert identity is DROPPED at the metric boundary — `MetricsRegistry` is identity-blind by construction, which is the cardinality firewall)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent invocations against a single shared `*MetricsRegistry` under `-race`, asserting no data races, no label cross-talk, no goroutine leaks. Per CLAUDE.md §5 + §11 + D-025.
- [ ] **Integration test exists** (`test/integration/phase56_metrics_test.go`) — wires real `events.EventBus` + `telemetry.Logger` + `*MetricsRegistry` + the real `prometheus` driver, asserts the `/metrics` body is identity-free, covers ≥1 failure mode, runs under `-race`. Per CLAUDE.md §17.
- [ ] If new vocabulary: glossary updated (yes — `MetricsRegistry`, `metrics cardinality lint`, `harbor_events_total`)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed; D-076 entry filed for the settled implementation calls)
