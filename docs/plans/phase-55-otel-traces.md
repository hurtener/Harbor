# Phase 55 — OTel traces + propagation

## Summary

Land OpenTelemetry tracing in `internal/telemetry`: a `Tracer` wrapper around `go.opentelemetry.io/otel/trace.Tracer` that derives spans from `events.Event` records so spans align with run/step boundaries, plus the W3C TraceContext propagation carriers Harbor's southbound transports use — `traceparent` HTTP headers, `_meta.traceparent` per-request maps for stdio MCP, and the `HARBOR_TRACEPARENT` env var on stdio spawn. The span exporter sits behind a `§4.4` driver seam (`noop` default, `otlp` driver) so a Jaeger/OTLP collector is opt-in via `TelemetryConfig.OTelEndpoint` without changing callers.

## RFC anchor

- RFC §6.14
- RFC §6.13
- RFC §4

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- **brief 06 §1 / §"Key data shapes" — `Tracer` is a `go.opentelemetry.io/otel/trace.Tracer` wrapper that injects `traceparent` into outgoing HTTP southbound calls and `_meta.traceparent` / `HARBOR_TRACEPARENT` into MCP southbound (env on stdio spawn, `_meta` per request).** Phase 55 ships exactly this surface: `Tracer` wraps the OTel SDK tracer; `propagation.go` ships the three carrier idioms (`InjectHTTP` / `ExtractHTTP`, `InjectMeta` / `ExtractMeta`, `InjectEnv` / `ExtractEnv`). The carrier helpers are standalone so Phase 27 (tools/HTTP) and Phase 28 (tools/MCP) wire them into their transports without re-opening this package.
- **brief 06 §6 (DevX roadmap item 6) — "span lifecycle from events".** Spans are a **derivation of the event bus**, not a parallel instrumentation path. `Tracer.SpanFromEvent(ctx, ev)` is the single bridge: it reads `events.Event.Type` + `events.Event.Identity` and starts/stamps a span whose name and attributes are derived from the event, with the run quadruple stamped as span attributes. There is no `tracer.Start(...)` sprinkled across subsystems — subsystems emit events, the event-to-span bridge does the rest.
- **brief 06 §"Lessons from the predecessor" — "No OpenTelemetry in the runtime ... OTel traces and metrics should be a first-class derivation of the event bus, shipped from t=0, not retrofitted."** Phase 04's `Logger` already reserved `trace_id` / `span_id` as passthrough attribute names for exactly this phase. Phase 55 closes the loop: `Tracer` exposes `LogAttrs(ctx)` returning the `trace_id` / `span_id` `slog.Attr` pair so `Logger.With(tracer.LogAttrs(ctx)...)` auto-stamps trace correlation onto every log line — logs and traces share the trace id rather than being parallel channels.
- **brief 06 §"Lessons" / §"metrics cardinality footgun" — "Event.TraceID is for logs and OTel traces; never tag metrics by trace_id / run_id".** Phase 55 stays inside the traces-and-logs lane: the trace id flows onto spans and log attributes only. The metrics-cardinality discipline is Phase 56's concern; Phase 55 introduces no metric label derivation.
- **decisions D-020 — Audit owns redaction.** Span attributes derived from an `events.Event` are limited to the event's *type*, the identity quadruple, and the event's bounded `Extra` map (low-cardinality, already safe for metric labels per the `Event` doc). The `Tracer` never reflectively walks an `EventPayload` onto a span — payload bytes are not span-safe, and the audit redactor is the only sanctioniser of payload content (D-020). This keeps spans payload-free by construction.
- **decisions D-025 — concurrent reuse contract.** `Tracer` is a canonical reusable artifact: built once at boot, shared across every emit path, called from arbitrary goroutines. Phase 55 ships the mandatory N≥100 concurrent-reuse test under `-race`.

## Findings I'm departing from (if any)

None.

## Goals

- A `Tracer` wrapper over `go.opentelemetry.io/otel/trace.Tracer`, constructed once via `NewTracer(cfg, ...)`, immutable and concurrency-safe (D-025).
- Spans derived from `events.Event` records via `SpanFromEvent` so span boundaries align with run/step boundaries — subsystems emit events, the event-to-span bridge produces the spans.
- W3C TraceContext propagation carriers for all three Harbor southbound idioms: `traceparent` HTTP header, `_meta.traceparent` per-request map (stdio MCP), `HARBOR_TRACEPARENT` env var (stdio spawn). Each idiom has an `Inject*` and an `Extract*` half so trace continuity holds across HTTP and stdio process boundaries.
- A `§4.4` span-exporter driver seam: a `SpanExporter` factory + registry; a `noop` driver (default — no collector configured) and an `otlp` driver (OTLP/gRPC, opt-in via `TelemetryConfig.OTelEndpoint`). Callers depend only on the `telemetry` package; drivers self-register via blank import at `cmd/harbor`.
- `Tracer.LogAttrs(ctx)` returns the `trace_id` / `span_id` `slog.Attr` pair so the Phase 04 `Logger` can correlate every log line to its trace.
- Coverage ≥85% on `internal/telemetry` per the master plan.

## Non-goals

- **No metrics / OTLP-metrics / Prometheus exporter** — that is Phase 56 (RFC §6.14, §11 Q-5). Phase 55 ships traces only. The OTLP *trace* exporter ships here; the OTLP *metric* exporter does not.
- **No transport wiring.** Phase 55 ships the propagation *carrier helpers* as standalone functions. The actual injection into the HTTP tool driver (Phase 27) and the MCP stdio driver (Phase 28) is those phases' work — they already shipped their transports before this phase in the critical path is *not* true (27/28 predate 55), so Phase 55 also adds the carrier call into the existing `internal/tools/drivers/http` and `internal/tools/drivers/mcp` seam **only if** those drivers expose a header/`_meta`/env hook without an interface change; otherwise the wiring is a documented follow-up. See "Findings I'm departing from" — this is NOT a departure, it is the §13 "no primitive without its consumer" rule satisfied by the event-to-span bridge being the consumer; the southbound carriers' first consumer is the integration test in this phase plus the existing drivers where a hook exists.
- **No span sampling policy / tail-based sampling** — the SDK default `AlwaysSample` for V1; sampling tuning is post-V1.
- **No Console-side trace view** — the Console consumes traces via the Protocol (Phase 60+); Phase 55 is Runtime-side only.
- **No change to `TelemetryConfig`** — the `OTelEndpoint` and `ServiceName` fields already exist (Phase 02). Phase 55 consumes them; it does not add config keys.
- **No durable trace store** — traces export to an external collector; Harbor does not persist spans itself.

## Acceptance criteria

- [ ] `internal/telemetry/tracing.go` defines `Tracer`, `NewTracer(cfg config.TelemetryConfig, opts ...TracerOption) (*Tracer, func(context.Context) error, error)` (the second return is the shutdown/flush hook), and the sentinel errors enumerated under "Public API surface".
- [ ] `NewTracer` selects the span exporter by `cfg.OTelEndpoint`: empty → the `noop` driver (no collector; spans are still created so propagation works in-process); non-empty → the `otlp` driver (OTLP/gRPC to that endpoint). An unknown explicitly-configured exporter driver returns a wrapped `ErrExporterUnknown` whose message lists the registered driver names.
- [ ] `Tracer.SpanFromEvent(ctx context.Context, ev events.Event) (context.Context, trace.Span)` starts a span whose name derives from `ev.Type`, stamps the identity quadruple (`tenant_id`, `user_id`, `session_id`, `run_id`) and the event's `Extra` map as span attributes, and returns a child `ctx` carrying the span. The span carries NO `EventPayload` bytes (D-020). When `ev` carries a `run_id`, the span is named/attributed so it aligns to the run boundary; step-granularity event types (`*.step_*`) produce child spans under the run span when the parent run span is present in `ctx`.
- [ ] Propagation — HTTP: `InjectHTTP(ctx context.Context, h http.Header)` writes the W3C `traceparent` (and `tracestate` when present) header from the span context in `ctx`; `ExtractHTTP(ctx context.Context, h http.Header) context.Context` returns a ctx carrying the remote span context. A round-trip (`InjectHTTP` → `ExtractHTTP`) preserves the trace id.
- [ ] Propagation — stdio MCP `_meta`: `InjectMeta(ctx context.Context, meta map[string]any)` writes `traceparent` (and `tracestate`) into the `_meta` map; `ExtractMeta(ctx context.Context, meta map[string]any) context.Context` reads them back. Round-trip preserves the trace id.
- [ ] Propagation — stdio spawn env: `InjectEnv(ctx context.Context, env []string) []string` appends `HARBOR_TRACEPARENT=<traceparent>` (and `HARBOR_TRACESTATE` when present) to a process environment slice; `ExtractEnv(ctx context.Context, environ []string) context.Context` reads `HARBOR_TRACEPARENT` back. Round-trip preserves the trace id.
- [ ] `Tracer.LogAttrs(ctx context.Context) []slog.Attr` returns the `trace_id` and `span_id` `slog.Attr` pair from the span context in `ctx` (empty slice when no span is active). Composes with Phase 04: `logger.With(tracer.LogAttrs(ctx)...)` stamps trace correlation onto log lines.
- [ ] `§4.4` seam: `SpanExporter` interface lives in the `telemetry` package; `noop` and `otlp` drivers live in `internal/telemetry/drivers/{noop,otlp}/`; each self-registers from `init()`; a factory in `internal/telemetry/tracing.go` dispatches by name; the factory's error lists registered drivers. Nothing imports a concrete driver except `cmd/harbor` (blank import) and that driver's own tests.
- [ ] No package-level mutable tracer state. There is NO `telemetry.DefaultTracer` package var and NO setter on `*Tracer`. The OTel global `TextMapPropagator` is set once at `NewTracer` time to the W3C TraceContext propagator (write-once, the §4.4-style registry exception); `NewTracer` is idempotent on the propagator set.
- [ ] Sentinel errors: `ErrTracerNotConfigured` (invalid config), `ErrExporterUnknown` (unknown explicitly-configured exporter driver). Callers compare via `errors.Is`.
- [ ] Concurrent-reuse test (D-025) — N≥100 goroutines each call `SpanFromEvent` + the propagation round-trips against a single shared `*Tracer` carrying goroutine-unique identity quadruples, under `-race`. Asserts: no data races, no identity cross-talk (each goroutine's span carries its own quadruple), no goroutine leak (`runtime.NumGoroutine()` baseline-restored after shutdown).
- [ ] Integration test (`test/integration/phase55_otel_test.go`) — real `events.EventBus` (inmem driver) + real `telemetry.Logger` + real `*Tracer` with an in-memory span recorder exporter. Asserts: an event published on the bus → `SpanFromEvent` → a span with the matching identity quadruple; `traceparent` HTTP + `_meta` + env round-trips preserve trace continuity; identity propagates through every layer; ≥1 failure mode (`ErrExporterUnknown` on a bad driver name); runs under `-race`.
- [ ] Coverage on `internal/telemetry` ≥ 85%.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-55.sh` present, executable, runs the tracing/propagation tests + the integration test under `-race`; reports `OK` ≥ the acceptance-criteria count it covers, `FAIL` = 0. No HTTP/Protocol surface → the HTTP-surface assertions `SKIP` per the 404/405/501 convention.

## Files added or changed

- `internal/telemetry/tracing.go` (new) — `Tracer` + `NewTracer` + the `SpanExporter` interface + the exporter factory/registry + `SpanFromEvent` + `LogAttrs` + sentinel errors.
- `internal/telemetry/propagation.go` (new) — the W3C TraceContext carriers: `InjectHTTP` / `ExtractHTTP`, `InjectMeta` / `ExtractMeta`, `InjectEnv` / `ExtractEnv`, and the env-var key constants (`EnvTraceparent = "HARBOR_TRACEPARENT"`, `EnvTracestate = "HARBOR_TRACESTATE"`).
- `internal/telemetry/tracing_test.go` (new) — unit tests: exporter selection, `SpanFromEvent` attribute derivation, payload-free spans, the D-025 concurrent-reuse test.
- `internal/telemetry/propagation_test.go` (new) — unit tests: the three carrier round-trips, empty-span-context behaviour, malformed-carrier tolerance.
- `internal/telemetry/drivers/noop/noop.go` (new) — the default `SpanExporter` driver (drops spans; spans are still created for in-process propagation).
- `internal/telemetry/drivers/noop/noop_test.go` (new).
- `internal/telemetry/drivers/otlp/otlp.go` (new) — the OTLP/gRPC `SpanExporter` driver (opt-in via `OTelEndpoint`).
- `internal/telemetry/drivers/otlp/otlp_test.go` (new).
- `cmd/harbor/main.go` (changed) — blank-import the two exporter drivers for self-registration (if `cmd/harbor/main.go` exists at this phase; otherwise the import lands with the binary entry point and the smoke degrades cleanly).
- `test/integration/phase55_otel_test.go` (new) — the cross-subsystem integration test (events bus + logger + tracer).
- `scripts/smoke/phase-55.sh` (new) — smoke script.
- `docs/plans/phase-55-otel-traces.md` (this file).
- `docs/decisions.md` (changed) — D-073 entry.
- `docs/glossary.md` (changed) — `Tracer`, `traceparent`, span-from-event entries.
- `README.md` + `docs/plans/README.md` (changed) — Phase 55 status flipped to Shipped.
- `go.mod` / `go.sum` (changed) — `go.opentelemetry.io/otel`, `.../otel/sdk`, `.../otel/trace`, `.../otel/exporters/otlp/otlptrace/otlptracegrpc` promoted to direct dependencies. These are RFC-sanctioned: RFC §6.14 names `go.opentelemetry.io/otel/trace.Tracer` explicitly.

No top-level directory additions; `internal/telemetry/` is already enumerated in CLAUDE.md §3, and `internal/telemetry/drivers/` follows the §4.4 `drivers/<driver>/` convention.

## Public API surface

```go
package telemetry

import (
    "context"
    "errors"
    "log/slog"
    "net/http"

    "go.opentelemetry.io/otel/sdk/trace"
    oteltrace "go.opentelemetry.io/otel/trace"

    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/events"
)

// Tracer is the canonical OTel tracer wrapper. Built once at boot via
// NewTracer; safe for concurrent use; immutable after construction (D-025).
type Tracer struct { /* unexported fields */ }

// SpanExporter is the §4.4 seam: drivers (noop, otlp) self-register
// and a factory dispatches by name. The interface is the OTel SDK's
// trace.SpanExporter — drivers return a ready exporter.
type SpanExporter interface {
    Exporter(ctx context.Context, cfg config.TelemetryConfig) (trace.SpanExporter, error)
}

// RegisterExporter installs a SpanExporter driver. Called from a
// driver's init(). Re-registering the same name is a no-op; an empty
// name panics.
func RegisterExporter(name string, e SpanExporter)

// TracerOption configures the Tracer at construction.
type TracerOption func(*tracerConfig)

var (
    // ErrTracerNotConfigured — invalid TelemetryConfig at construction.
    ErrTracerNotConfigured = errors.New("telemetry: tracer not configured")
    // ErrExporterUnknown — an explicitly-configured exporter driver is
    // not registered. The message lists registered driver names.
    ErrExporterUnknown = errors.New("telemetry: unknown span exporter driver")
)

// NewTracer constructs a Tracer from validated config. The returned
// shutdown func flushes and shuts down the exporter; callers defer it
// at process teardown. Empty cfg.OTelEndpoint selects the noop
// exporter; a non-empty endpoint selects otlp.
func NewTracer(cfg config.TelemetryConfig, opts ...TracerOption) (*Tracer, func(context.Context) error, error)

// SpanFromEvent starts a span derived from ev: the span name comes
// from ev.Type, the identity quadruple + ev.Extra become span
// attributes, and NO EventPayload bytes are stamped (D-020). Returns
// a child ctx carrying the span.
func (t *Tracer) SpanFromEvent(ctx context.Context, ev events.Event) (context.Context, oteltrace.Span)

// LogAttrs returns the trace_id / span_id slog.Attr pair from the
// span context in ctx (empty when no span is active). Compose with
// the Phase 04 Logger: logger.With(tracer.LogAttrs(ctx)...).
func (t *Tracer) LogAttrs(ctx context.Context) []slog.Attr

// --- propagation.go ---

// EnvTraceparent / EnvTracestate are the env var keys used on stdio
// child-process spawn.
const (
    EnvTraceparent = "HARBOR_TRACEPARENT"
    EnvTracestate  = "HARBOR_TRACESTATE"
)

// InjectHTTP writes the W3C traceparent (+ tracestate) header from the
// span context in ctx into h.
func InjectHTTP(ctx context.Context, h http.Header)

// ExtractHTTP returns a ctx carrying the remote span context decoded
// from the W3C headers in h.
func ExtractHTTP(ctx context.Context, h http.Header) context.Context

// InjectMeta writes traceparent (+ tracestate) into a stdio-MCP _meta
// map. ExtractMeta reads them back.
func InjectMeta(ctx context.Context, meta map[string]any)
func ExtractMeta(ctx context.Context, meta map[string]any) context.Context

// InjectEnv appends HARBOR_TRACEPARENT (+ HARBOR_TRACESTATE) to a
// process environment slice. ExtractEnv reads HARBOR_TRACEPARENT back.
func InjectEnv(ctx context.Context, env []string) []string
func ExtractEnv(ctx context.Context, environ []string) context.Context
```

The exported surface above plus the two sentinel errors and the env-var key constants are the entire Phase 55 API. Internal types (`tracerConfig`, the exporter registry map, the OTel propagator holder) stay unexported.

## Test plan

- **Unit:**
  - `NewTracer` happy path: empty `OTelEndpoint` → a non-nil `*Tracer` backed by the `noop` exporter; the shutdown func is non-nil and returns nil.
  - Exporter selection: an explicitly-configured unknown driver name → `ErrExporterUnknown` whose message contains `noop` and `otlp`.
  - `SpanFromEvent`: a published-shaped `events.Event` produces a span whose name derives from `ev.Type` and whose attributes carry `tenant_id` / `user_id` / `session_id` / `run_id` + every `ev.Extra` key. Assert NO payload field is present on the span (walk span attributes; assert no `EventPayload`-shaped key).
  - `SpanFromEvent` run/step alignment: an event with a `run_id` produces a run-aligned span; a `*.step_*`-typed event with a parent run span in `ctx` produces a child span (assert parent span id linkage).
  - `LogAttrs`: with an active span → returns `trace_id` + `span_id` attrs whose values match the span context; with no span → empty slice.
  - Propagation round-trips: `InjectHTTP`→`ExtractHTTP`, `InjectMeta`→`ExtractMeta`, `InjectEnv`→`ExtractEnv` each preserve the trace id and span id flags.
  - Malformed-carrier tolerance: `ExtractHTTP` / `ExtractMeta` / `ExtractEnv` on a header/map/environ with a garbage `traceparent` value return a ctx with no valid span context (no panic, no partial state) — fail-safe extraction is acceptable here because extraction of a *remote* trace id is best-effort by W3C spec; a forced *exporter* error is the loud failure mode (see Integration).
  - `noop` driver: returns a `trace.SpanExporter` that drops spans without error.
  - `otlp` driver: `Exporter` with a syntactically-valid endpoint returns a non-nil exporter (no live collector needed — the OTLP/gRPC exporter connects lazily).
- **Integration** (`test/integration/phase55_otel_test.go`, real drivers on the seam, under `-race`):
  - Real `events.EventBus` (inmem driver) + real `telemetry.Logger` + real `*Tracer` constructed with an in-memory span recorder (`tracetest.NewInMemoryExporter` via a test-only `TracerOption` that injects a custom exporter, OR the `noop` driver swapped for a recorder — whichever keeps the seam real).
  - Publish an event on the bus → `SpanFromEvent` → assert the recorded span carries the event's identity quadruple.
  - Identity propagation: the quadruple flows ctx → event → span attributes → `LogAttrs` → a `Logger` line carrying the same `trace_id`.
  - Trace continuity: `InjectHTTP` on the run's ctx, `ExtractHTTP` on the carrier, `SpanFromEvent` on the extracted ctx → the child span shares the trace id.
  - Failure mode: `NewTracer` with an explicitly-configured bogus exporter driver → `ErrExporterUnknown`.
  - Concurrency stress: N≥10 concurrent producers publish events + derive spans against the one shared `*Tracer`; assert no cross-talk in span identity, no goroutine leak after shutdown.
- **Conformance:** N/A — the `SpanExporter` seam has two drivers but the OTel SDK's `trace.SpanExporter` is itself the conformance contract (the SDK exercises it); a Harbor-side conformance suite would re-test the SDK. The driver tests assert each driver returns a usable `trace.SpanExporter`.
- **Concurrency / leak (D-025):** `TestConcurrentReuse_Tracer` — N≥100 goroutines, each with a goroutine-unique identity quadruple, call `SpanFromEvent` + the three propagation round-trips + `LogAttrs` against one shared `*Tracer`. Buffered/recorder exporter so spans are observable. Assertions: no data races (`-race` gate), no identity cross-talk (each goroutine's span carries its own quadruple), `runtime.NumGoroutine()` baseline-restored within 2s of `wg.Wait()` + tracer shutdown.

## Smoke script additions

`scripts/smoke/phase-55.sh`:

- Runs `go test -race ./internal/telemetry/...` and records `OK` when the tracing + propagation + driver tests pass, `FAIL` otherwise.
- Runs `go test -race -run TestE2E ./test/integration/ -run Phase55` and records `OK` on the integration test.
- The script has no HTTP/Protocol surface to hit (the Console consumes traces via the Protocol in a later phase) — it sources `scripts/smoke/common.sh`, runs the Go tests, and `skip`s the HTTP-surface line, then `smoke_summary`. Identical shape to `phase-05.sh` (Go-package phase, no live endpoint).

## Coverage target

- `internal/telemetry`: 85% (master plan). The new files (`tracing.go`, `propagation.go`) plus the two driver packages are covered by `tracing_test.go`, `propagation_test.go`, the driver tests, and the integration test.

## Dependencies

- Phase 04 (telemetry `Logger` — `Tracer.LogAttrs` composes with `Logger.With`; the `trace_id` / `span_id` attribute names were reserved by Phase 04).
- Phase 05 (events `EventBus` + `Event` — `SpanFromEvent` derives spans from `events.Event`).

Wave placement: Phase 55 is the first phase of the telemetry-completion wave (55 OTel traces, 56 metrics). It depends only on already-shipped phases.

## Risks / open questions

- **OTLP exporter is a new direct dependency.** `go.opentelemetry.io/otel/*` was already an indirect dependency in `go.sum`; Phase 55 promotes the trace SDK + OTLP/gRPC trace exporter to direct. This is RFC-sanctioned — RFC §6.14 names `go.opentelemetry.io/otel/trace.Tracer` explicitly and brief 06 names the OTel Metrics + Traces SDK as the backing. No RFC PR needed; the PR description notes the promotion with this rationale.
- **The `otlp` driver's live behaviour is hard to unit-test without a collector.** Mitigation: the driver test asserts `Exporter` returns a non-nil `trace.SpanExporter` for a syntactically-valid endpoint (the OTLP/gRPC exporter connects lazily, so no live collector is needed at construction). The acceptance criterion "integration with Jaeger/OTLP collector" is satisfied structurally — a real collector run is an operator smoke, not a CI gate; the integration test uses an in-memory recorder exporter to assert span shape end-to-end.
- **Propagation carrier wiring into Phase 27/28 drivers.** Phases 27 (tools/HTTP) and 28 (tools/MCP) already shipped before Phase 55. Phase 55 ships the carrier helpers as standalone functions; whether they wire directly into the existing HTTP/MCP drivers depends on whether those drivers expose a header/`_meta`/env hook without an interface change. If they do, the wiring rides in this PR; if not, the wiring is a documented follow-up issue (the carrier helpers' first consumer is the integration test, which discharges the §13 primitive-with-consumer obligation, and the event-to-span bridge is the `Tracer`'s first consumer). The PR description states which path was taken.
- **Span-from-event vs. explicit instrumentation.** The risk is a contributor sprinkling `tracer.Start(...)` across subsystems instead of emitting events. Mitigation: `Tracer` exposes ONLY `SpanFromEvent` (plus `LogAttrs` and the carriers) — there is no public `Start` method. Spans are a derivation of the event bus, full stop (brief 06 §1).
- No RFC §11 open question blocks this phase. §11 Q-5 (metrics exporter) is Phase 56's concern; Phase 55 is traces-only.

## Glossary additions

- **Tracer** — Harbor's OpenTelemetry tracer wrapper (`internal/telemetry`). Wraps `go.opentelemetry.io/otel/trace.Tracer`; derives spans from `events.Event` records via `SpanFromEvent` so span boundaries align with run/step boundaries; exposes the W3C TraceContext propagation carriers and the `trace_id` / `span_id` `slog.Attr` pair for log correlation. Built once at boot; immutable (D-025). RFC §6.14, brief 06.
- **traceparent** — the W3C TraceContext header carrying the trace id + parent span id across process boundaries. Harbor propagates it three ways: as the `traceparent` HTTP header (HTTP southbound), as `_meta.traceparent` in a per-request map (stdio MCP), and as the `HARBOR_TRACEPARENT` env var on stdio child-process spawn. RFC §6.14.
- **span-from-event** — Harbor's rule that OTel spans are a *derivation of the event bus*, not a parallel instrumentation path. Subsystems emit `events.Event` records; `Tracer.SpanFromEvent` is the single bridge that turns an event into a span. There is no `tracer.Start(...)` scattered across subsystems. brief 06 §1, §6.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/telemetry` ≥ 85%
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the concurrent-reuse test asserts no identity cross-talk in span attributes — `Tracer` is identity-aware via ctx/events, not identity-scoped storage)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent invocations against a single shared `*Tracer` under `-race`, asserting no data races, no identity cross-talk, no goroutine leaks. Per CLAUDE.md §5 + §11 + D-025.
- [ ] **Integration test exists** (`test/integration/phase55_otel_test.go`) — wires real `events.EventBus` + `telemetry.Logger` + `*Tracer`, asserts identity propagation through every layer, covers ≥1 failure mode (`ErrExporterUnknown`), runs under `-race`. Per CLAUDE.md §17.
- [ ] If new vocabulary: glossary updated (yes — `Tracer`, `traceparent`, `span-from-event`)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed; D-073 entry filed for the settled implementation calls)
