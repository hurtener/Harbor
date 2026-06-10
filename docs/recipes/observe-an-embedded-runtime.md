# Observe an embedded runtime (logs, metrics, traces)

Wire Harbor's full observability surface — the redactor-mandatory
structured Logger, the bus→metrics bridge, and the bus→tracer bridge —
around a headless runtime, then read the same signals the `harbor`
binary produces. Headless first, binary second: everything here is
plain exported Go; the binary path is "it already happened in
`assemble.Assemble`".

This recipe is acceptance-gated: its end-to-end path is executed by
`test/integration/phase111f_telemetry_test.go`, so every snippet
references real exported symbols (Phase 111f, D-203).

> **Import paths.** The snippets use the public `sdk/` facade
> (RFC §3.6; `sdk/telemetry` + `sdk/telemetry/eventbus` + `sdk/audit`
> landed with Phase 112b, D-206) — every import below compiles from an
> external Go module, same as in-module.

## The model in one paragraph

Harbor's observability is **bus-first** (RFC §6.14): subsystems emit
canonical `events.Event` records; logs, metrics, and traces are
*derivations* of that stream, never parallel instrumentation channels.
The Logger pairs every `Error` with a `runtime.error` bus event; the
metrics bridge folds events into low-cardinality counters
(Type/Producer/Node — never identity or run IDs); the trace bridge
turns lifecycle event pairs into OTel spans (where identity and run
IDs *do* ride, as span attributes). One stream in, three signals out.

## The pieces

| Piece | Symbol | Phase |
|-------|--------|-------|
| Redaction (mandatory) | `audit.Open(ctx, cfg.Audit)` | 03 |
| The event bus | `events.OpenWith(ctx, cfg.Events, red, deps)` | 05 / 110d |
| The Logger | `telemetry.New(cfg.Telemetry, red, telemetry.WithBusEmitter(eventbus.New(bus)))` | 04 / 111f |
| Metrics registry + bridge | `telemetry.NewMetricsRegistry` + `telemetry.BridgeBusToMetrics` | 56 |
| Tracer + bridge | `telemetry.NewTracer` + `telemetry.BridgeBusToTracer` | 55 / 111f |
| Engine error hook | `flow.WithRunErrorHandler(stack.RunErrorHandler)` | 111f |

## 0. The short answer: `assemble.Assemble` does all of this

If you assemble through the promoted entry point (the
[headless-embedding recipe](embed-harbor-headless.md)), the whole
chain below is already wired:

```go
stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
// stack.Telemetry      — the redactor-mandatory, bus-paired Logger
// stack.Metrics        — the MetricsRegistry (bus→metrics bridge running)
// stack.Tracer         — the OTel tracer (bus→tracer bridge running)
// stack.RunErrorHandler — the engine run-error → Logger.Error hook
defer stack.Close(ctx) // joins both bridges, flushes the tracer
```

The rest of this recipe is the manual composition — for embedders who
build their own stack, and as documentation of what the assembly does.

## 1. Imports

```go
import (
    "context"

    // Registers the audit / events / state drivers AND the span +
    // metric exporter drivers (noop, otlp, prometheus, otlpmetric).
    _ "github.com/hurtener/Harbor/sdk/drivers/prod"

    "github.com/hurtener/Harbor/sdk/audit"
    "github.com/hurtener/Harbor/sdk/config"
    "github.com/hurtener/Harbor/sdk/events"
    "github.com/hurtener/Harbor/sdk/telemetry"
    "github.com/hurtener/Harbor/sdk/telemetry/eventbus"
)
```

## 2. Assembly order: audit → bus → logger → metrics → traces

The order is load-bearing: the redactor is everyone's mandatory
dependency (no signal leaves the process unredacted), and the bus must
exist before anything can derive from it.

```go
cfg := config.Defaults()
// cfg.Telemetry.ServiceName = "my-embedder"        // span/metric identity
// cfg.Telemetry.OTelEndpoint = "localhost:4317"    // opt-in OTLP; empty = noop spans

red, err := audit.Open(ctx, cfg.Audit)              // 1. redaction
if err != nil { return err }

bus, err := events.OpenWith(ctx, cfg.Events, red, events.Deps{})
if err != nil { return err }                        // 2. the canonical stream
defer bus.Close(ctx)

logger, err := telemetry.New(cfg.Telemetry, red,    // 3. the Logger
    telemetry.WithBusEmitter(eventbus.New(bus)))
if err != nil { return err }
// logger.Error(ctx, ...) now writes the slog record AND publishes a
// paired runtime.error event with the ctx identity (RFC §6.14).

metrics, metricsShutdown, err := telemetry.NewMetricsRegistry(cfg.Telemetry)
if err != nil { return err }                        // 4. metrics
defer metricsShutdown(ctx)
stopMetrics, err := telemetry.BridgeBusToMetrics(ctx, bus, metrics, events.Filter{Admin: true})
if err != nil { return err }
defer stopMetrics()

tracer, tracerShutdown, err := telemetry.NewTracer(cfg.Telemetry)
if err != nil { return err }                        // 5. traces
defer tracerShutdown(ctx)
stopTraces, err := telemetry.BridgeBusToTracer(ctx, bus, tracer, telemetry.DefaultTraceBridgeFilter())
if err != nil { return err }
defer stopTraces()
```

Notes:

- **The redactor is not optional.** `telemetry.New(cfg, nil)` fails
  loud with `ErrRedactorMissing`; there is no unredacted-logger mode.
- **Exporter selection.** An empty `cfg.Telemetry.OTelEndpoint`
  selects the `noop` span exporter (spans are still created, so
  in-process propagation works; nothing is shipped). A non-empty
  endpoint selects `otlp` (OTLP/gRPC). Both register via the
  `sdk/drivers/prod` blank import — without it, construction
  fails loud naming the registered drivers.
- **The default trace filter.** `telemetry.DefaultTraceBridgeFilter()`
  scopes the bridge to the canonical lifecycle pairs
  (`task.started/completed/failed/cancelled`,
  `tool.invoked/completed/failed/...`) so a chatty bus — streaming
  chunks especially — never becomes span flood. Pass your own
  `events.Filter` to widen or narrow it; non-lifecycle events that DO
  pass the filter attach as span events on the enclosing span.
- **Cardinality split (brief 06).** Metrics labels stay
  Type/Producer/Node — never run or identity values. Identity and
  run/task IDs ride on *spans* as attributes. The two bridges enforce
  the split by existing separately; don't blur it.

## 3. The engine run-error hook

A composed flow engine reports terminal node failures through
`engine.WithRunErrorHandler`. The production handler routes the
structured `engine.RunError` through `Logger.Error`, which makes the
failure land on the bus as `runtime.error`:

```go
eng, err := flow.Compose(def, flow.WithRunErrorHandler(stack.RunErrorHandler))
```

(Manual composition: write the same three-line closure the assembly
builds — call `logger.Error(ctx, "engine: run failed", ...)` with the
RunError's node/code/message attrs. The flow engine itself is
module-internal at V1.2 — the `sdk/` facade deliberately omits it, so
this hook applies to in-module flow composition; the assembled stack
already carries the wired handler either way.)

## 4. Reading the signals

- **Logs**: structured slog on stdout (`log_format: json` in
  production, `text` in dev). Every record carries the ctx identity
  (`tenant_id` / `user_id` / `session_id` / `run_id`) and every value
  passed through the redactor first.
- **`runtime.error` events**: subscribe to the bus with your identity
  filter — the same stream the Console's live views read.
- **Metrics**: `metrics.Snapshot(ctx)` for the in-process projection,
  or `telemetry.PrometheusHandler(metrics)` to mount `/metrics` when
  the prometheus exporter driver is configured.
- **Traces**: point `cfg.Telemetry.OTelEndpoint` at an OTLP collector
  (Jaeger, Tempo, Grafana). Task spans parent tool spans within the
  same identity quadruple; failed lifecycles carry span status
  `Error`.

## Troubleshooting

| Symptom | Cause |
|---------|-------|
| `telemetry: logger not configured` | invalid `log_format` / `log_level` — use `json`/`text`, `debug..error` |
| `telemetry: redactor missing` | you passed a nil redactor; open `audit` first |
| `telemetry: unknown span exporter driver` | missing `_ "github.com/hurtener/Harbor/sdk/drivers/prod"` |
| `Logger.Error` writes the log line but no bus event | the ctx carries no identity triple — stamp it via `identity.With` |
| No spans in the collector | empty `OTelEndpoint` (noop exporter), or your filter excludes the event types |
