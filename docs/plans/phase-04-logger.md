# Phase 04 — slog Logger + standard attribute set

## Summary

Land `internal/telemetry`: Harbor's canonical structured logger. A thin wrapper around `log/slog` that pre-pins the eight-attribute identity set, wires every record through `audit.Redactor` before emission, selects a JSON or text handler from validated `TelemetryConfig`, and exposes a `BusEmitter` seam so Phase 05+ can fire the paired `runtime.error` bus event without re-opening this package. This is the single emit path every downstream subsystem (engine, planner, tools, governance, sessions) imports — no parallel logging facilities.

## RFC anchor

- RFC §6.14
- RFC §3.5
- RFC §6.4
- RFC §10

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- **brief 06 §1 — events are the canonical projection of runtime state.** Logging is a *derivation* of the event bus, not a parallel channel. Phase 04 ships the slog substrate; the `BusEmitter` interface is the seam Phase 05+ uses to make `Logger.Error` emit a paired `runtime.error` bus event so every error log has an event peer (RFC §6.14 Settled item #3).
- **brief 06 §2 — `Logger` pre-pins the standard attribute set.** The phase plan locks the eight identity-shaped attribute names (`tenant_id`, `user_id`, `session_id`, `run_id`, `task_id`, `trace_id`, `span_id`, `tool`) into the Go API so a contributor cannot mistype them. Identity attributes (`tenant_id`, `user_id`, `session_id`, `run_id`, `task_id`) flow from `ctx` via the Phase 01 helpers — never from a global. `trace_id` / `span_id` / `tool` are populated by `With(...)`; OTel context propagation is Phase 55 work.
- **brief 06 §5 — "logging in two formats with a flag" is an anti-pattern.** The predecessor exposed a runtime toggle (`configure_logging(structured: bool)`) and paid for it forever. Harbor selects the slog handler **once at process start** from `TelemetryConfig.LogFormat` (already validated to `json|text` by Phase 02). No in-library toggle. CLI flag layering arrives in Phase 64; the env-var seam (`HARBOR_TELEMETRY_LOG_FORMAT`) already works through the Phase 02 loader.
- **brief 06 §5 — "avoid logging large payloads; prefer artifacts and references."** Baked into the Logger: every structured attribute is fed through `audit.Redactor.Redact` before emission. The redactor's V1 ruleset (Phase 03) covers the canonical secret shapes plus the multimodal safety net (D-021 / D-022) so heavy `DataURL` content becomes `[redacted: <MIME> of <N> bytes]` rather than a megabyte log line.
- **decisions D-020 — Audit owns redaction; Logger consumes the redactor; never re-implements.** Phase 04 takes a constructor-injected `audit.Redactor`. There is no logger-local rule list, no name-fallback list, no "lite" redaction path. Forking redaction would split responsibility and risk the inconsistent output D-020 explicitly closed.
- **decisions D-025 — concurrent reuse contract.** A `*Logger` is a canonical reusable artifact: built once at boot, shared across every emit path, and called from arbitrary goroutines. Phase 04 ships the mandatory N≥100 concurrent-reuse test under `-race` per AGENTS.md §11.

## Findings I'm departing from (if any)

- None.

## Goals

- One process-wide `*Logger` constructor (`telemetry.New(cfg, redactor)`) that returns an immutable, concurrency-safe instance ready to be propagated through every subsystem.
- Pinned eight-attribute identity surface — every record carries the standard attribute set when the values are present in `ctx`; absent values are silently elided rather than emitted as empty strings.
- Redaction-first emission: every attribute value (and the `msg` string) passes through `audit.Redactor.Redact` BEFORE the slog handler sees it. A redactor error is fail-loudly: the record is replaced with a sentinel `"[redacted: log emission blocked by redactor error]"` line rather than emitting the raw payload — defending the §13 forbidden practice "logging unredacted tool args/results."
- `BusEmitter` interface seam so Phase 05 (event bus) can wire `Logger.Error` to emit a paired `runtime.error` event without changing this package. Phase 04 ships a `noopEmitter` default; the production wiring composes `New(cfg, redactor, telemetry.WithBusEmitter(bus))`.
- Coverage ≥85% on `internal/telemetry` per master plan.

## Non-goals

- No OpenTelemetry tracing / span lifecycle / `traceparent` propagation — that lands in Phase 55 (OTel) per RFC §6.14 Settled item #4. The `trace_id` / `span_id` attributes are passthroughs in Phase 04: callers may set them via `With(...)`; this phase does NOT extract them from an OTel context.
- No metrics registry / OTLP / Prometheus exporter — Phases 55-56 territory.
- No CLI flag layering — Phase 64 wires `--log-format=text|json` through Cobra. The Phase 02 loader's env-var override on `HARBOR_TELEMETRY_LOG_FORMAT` is the only override path until then.
- No durable log driver — Phase 57 territory.
- No event-bus implementation — Phase 05. Phase 04 ships the `BusEmitter` interface only; the default emitter is a no-op.
- No logger-local redaction or PII detection. The `audit.Redactor` is the only redaction point (D-020).

## Acceptance criteria

- [ ] `internal/telemetry/logger.go` defines `Logger`, the `BusEmitter` interface, the `Option` type, and the public functions enumerated under "Public API surface" below.
- [ ] `New(cfg config.TelemetryConfig, r audit.Redactor, opts ...Option) (*Logger, error)` returns `ErrLoggerNotConfigured` when `cfg.LogFormat` is not `json` or `text` (defensive — Phase 02 already validates, but the constructor MUST NOT trust upstream); returns a wrapped `audit.ErrRedactorMissing` when `r` is `nil`.
- [ ] Handler selection: `cfg.LogFormat == "json"` returns `slog.NewJSONHandler`; `"text"` returns `slog.NewTextHandler`. The handler is constructed once at `New` time; later mutation is impossible (no setter, no exported pointer to the handler).
- [ ] Standard attribute set pinned: `WithIdentity(id identity.Identity) *Logger` adds `tenant_id`, `user_id`, `session_id`. `WithRun(q identity.Quadruple) *Logger` adds the triple plus `run_id`. `With(attrs ...slog.Attr) *Logger` adds caller-supplied attributes (the only path for `task_id`, `trace_id`, `span_id`, `tool` in this phase).
- [ ] Identity-from-ctx auto-stamping: `Debug/Info/Warn/Error(ctx, msg, attrs ...slog.Attr)` calls inspect `ctx` via `identity.From` / `identity.QuadrupleFrom` and append the standard identity attributes when present. Already-bound attributes from `WithIdentity` / `WithRun` win on conflict (explicit > ctx).
- [ ] Redaction wiring: every emitted record's attribute values AND the `msg` string flow through `audit.Redactor.Redact` BEFORE the slog handler is invoked. A redactor error MUST replace the record with a sentinel line `"[redacted: log emission blocked by redactor error]"` and the original record MUST NOT reach the handler. Test asserts no original payload bytes are written when the redactor errors.
- [ ] `Logger.Error(ctx, msg, attrs...)` invokes the configured `BusEmitter.EmitRuntimeError(ctx, ev)` AFTER the redacted slog record is written. Phase 04 ships a `noopEmitter` default; `WithBusEmitter(b BusEmitter)` swaps in a real implementation. The bus event payload is the same redacted attribute map the slog record received.
- [ ] No package-level mutable logger state. There is NO `telemetry.Default` package var, NO `init()` that constructs a logger, and NO setter exposed on `*Logger`. Constructed once via `New`; passed by pointer.
- [ ] Sentinel errors: `ErrLoggerNotConfigured` (invalid config / construction inputs) and `ErrRedactorMissing` (wraps `audit.ErrRedactorMissing` when the redactor is nil at construction time). Callers compare via `errors.Is`.
- [ ] Concurrent-reuse test (D-025) — N≥100 goroutines logging through independent `WithIdentity` / `WithRun` derivations of a single shared `*Logger` under `-race`. Asserts: no data races, no attribute cross-talk (each goroutine's record carries its own identity), no goroutine leak (baseline-restored). Uses a buffered `bytes.Buffer` handler so emission is observable from the test.
- [ ] Coverage on `internal/telemetry` ≥ 85%.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-04.sh` smoke script present and executable; reports `SKIP` under preflight (Phase 04 has no HTTP surface).

## Files added or changed

- `internal/telemetry/logger.go` (new) — `Logger` + `BusEmitter` interface + `Option` + sentinel errors + constructor.
- `internal/telemetry/logger_test.go` (new) — unit tests, redaction-failure path, concurrent-reuse test, attribute-conflict resolution.
- `internal/telemetry/options.go` (new) — `Option` type + `WithBusEmitter`. **Shipped also:** `WithWriter(io.Writer)` (test-only seam — `New` defaults the slog handler to `os.Stdout`; tests inject a buffered writer via `WithWriter` to inspect emitted records). The deviation is documented inline; no production caller uses `WithWriter`.
- `internal/telemetry/bus.go` (new) — `BusEmitter` interface declaration + `noopEmitter` default. Tiny file kept separate so Phase 05 can re-document the contract without churning `logger.go`.
- `scripts/smoke/phase-04.sh` (new) — smoke skeleton (`SKIP` under preflight; flagged for upgrade if a future surface lands).
- `docs/plans/phase-04-logger.md` (this file).

No top-level directory additions; `internal/telemetry/` is already enumerated in AGENTS.md §3.

## Public API surface

```go
package telemetry

import (
    "context"
    "errors"
    "log/slog"

    "github.com/hurtener/Harbor/internal/audit"
    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/identity"
)

// Logger is the canonical structured logger. Built once at boot via
// New; safe for concurrent use; immutable after construction.
type Logger struct { /* unexported fields */ }

// BusEmitter is the seam Phase 05+ uses to make Logger.Error fire a
// paired runtime.error event. Phase 04 ships a noopEmitter default.
type BusEmitter interface {
    EmitRuntimeError(ctx context.Context, msg string, attrs []slog.Attr)
}

// Option configures the Logger at construction. Phase 04 ships
// WithBusEmitter; later phases (e.g. test kit) may add more.
type Option func(*Logger)

var (
    // ErrLoggerNotConfigured — invalid TelemetryConfig or construction inputs.
    ErrLoggerNotConfigured = errors.New("telemetry: logger not configured")
    // ErrRedactorMissing — wraps audit.ErrRedactorMissing when the
    // redactor is nil at construction time.
    ErrRedactorMissing = errors.New("telemetry: redactor missing")
)

// New constructs a Logger from validated config and a Redactor.
// Returns a wrapped sentinel on invalid input. The handler is chosen
// once and never swapped (RFC §6.14: no in-library toggle).
func New(cfg config.TelemetryConfig, r audit.Redactor, opts ...Option) (*Logger, error)

// WithBusEmitter installs the production runtime.error emitter. Phase
// 05+ wires this when constructing the Logger.
func WithBusEmitter(b BusEmitter) Option

// WithIdentity returns a derived Logger that pre-stamps tenant_id,
// user_id, session_id from id. The base Logger is unchanged.
func (l *Logger) WithIdentity(id identity.Identity) *Logger

// WithRun returns a derived Logger that pre-stamps the identity triple
// plus run_id from q. The base Logger is unchanged.
func (l *Logger) WithRun(q identity.Quadruple) *Logger

// With returns a derived Logger carrying additional attributes (the
// only path for task_id, trace_id, span_id, tool in Phase 04). The
// base Logger is unchanged.
func (l *Logger) With(attrs ...slog.Attr) *Logger

// Debug / Info / Warn / Error emit a structured record. Identity
// attributes are auto-stamped from ctx via identity.From /
// identity.QuadrupleFrom when not already bound. Every value plus
// the msg string flows through the configured Redactor before the
// slog handler is invoked. Error additionally fires the configured
// BusEmitter.EmitRuntimeError.
func (l *Logger) Debug(ctx context.Context, msg string, attrs ...slog.Attr)
func (l *Logger) Info(ctx context.Context, msg string, attrs ...slog.Attr)
func (l *Logger) Warn(ctx context.Context, msg string, attrs ...slog.Attr)
func (l *Logger) Error(ctx context.Context, msg string, attrs ...slog.Attr)
```

The exported surface above plus the two sentinel errors are the entire Phase 04 API. Internal types (`noopEmitter`, the unexported handler holder, the redaction helper) stay unexported.

## Test plan

- **Unit:**
  - `New` happy path produces a non-nil Logger; sentinel errors fire on invalid `LogFormat` and on nil redactor.
  - Handler selection: `cfg.LogFormat == "json"` writes JSON to a buffer; `"text"` writes text. Inspect the buffer contents.
  - `WithIdentity` / `WithRun` round-trip: emitted records carry the expected identity keys and only those.
  - `With(...)` round-trip: arbitrary attributes appear; built-ins are not duplicated.
  - Identity-from-ctx auto-stamp: a record emitted with an `identity.With(ctx, id)` ctx and no `WithIdentity` binding carries `tenant_id`/`user_id`/`session_id`. With both, `WithIdentity` wins.
  - `Quadruple` auto-stamp adds `run_id` when present.
  - Each level (`Debug`/`Info`/`Warn`/`Error`) routes to slog at the right severity.
  - `Error` invokes the configured `BusEmitter.EmitRuntimeError` with the redacted attribute map. `noopEmitter` does nothing; a fake emitter records the call.
  - Redaction happy path: a record with an `api_key` attribute emits the redacted form; the original value never appears in the buffer.
  - Redaction failure path: a stub redactor returning `audit.ErrRedactionFailed` causes the emitted line to be the sentinel `"[redacted: log emission blocked by redactor error]"`. The buffer MUST NOT contain any of the original attribute values; assert byte-level absence of a planted secret.
  - Absent identity components in ctx are silently elided rather than emitted as empty strings.
- **Integration:**
  - End-to-end pipeline against a real `audit.patterns` driver from Phase 03 plus a fake `BusEmitter` recorder. Asserts `Logger.Error` writes a redacted slog record AND fires exactly one bus event with the same redacted payload.
- **Conformance:** N/A (single implementation; no driver registry in this phase).
- **Concurrency / leak (D-025):** `TestLogger_ConcurrentReuse_ReuseContract` — 100+ goroutines, each derives a `*Logger` via `WithIdentity` (or `WithRun`) carrying a goroutine-unique identity, emits 10 records, then exits. Single shared base `*Logger`. Buffered handler with a `sync.Mutex`-guarded write side. Assertions: every emitted record carries the originating goroutine's identity (no cross-talk); `runtime.NumGoroutine()` returns to baseline within 2s of `wg.Wait()`; `go test -race` is the gate.
- **Failure-mode:** redactor that returns an error (forced via fake), bus emitter that panics (logger MUST NOT propagate the panic; recover + emit one warning-class slog record about the emitter failure — without invoking the same emitter again, to avoid recursion).

## Smoke script additions

`scripts/smoke/phase-04.sh` records the surface state explicitly:

- `skip "phase 04: telemetry/logger — Go package only; validated by go test ./internal/telemetry/..."`

The script sources `scripts/smoke/common.sh`, calls `skip` with the message above, then calls `smoke_summary`. Identical shape to `phase-01.sh` / `phase-02.sh` / `phase-03.sh`.

## Coverage target

- `internal/telemetry`: 85%.

## Dependencies

- Phase 03 (audit redactor — for the `audit.Redactor` interface, `audit.ErrRedactorMissing` sentinel, and the canonical V1 ruleset).
- Phase 02 (config — for `config.TelemetryConfig` and the validated `LogFormat` field).
- Phase 01 (identity — for `identity.Identity`, `identity.Quadruple`, `identity.From`, `identity.QuadrupleFrom`).

Wave 2: parallelizable with phases 05 (event bus) and 07 (StateStore iface). Logger does NOT depend on the event bus — Phase 05 wires the `BusEmitter` later.

## Risks / open questions

- **Redaction cost on the hot logging path.** Every record passes through `audit.Redactor.Redact`, which is reflective. Risk: pathological log volume amplifies the cost. Mitigation: benchmark in this phase against representative payloads; if the cost is unacceptable, RFC §6.14 already names a typed-payload protocol (Phase 05's `Event` shape) as the route — but the Logger surface here MUST NOT bypass redaction as a perf win, because that re-creates the §13 forbidden-practice failure mode.
- **`slog.Attr` value-extraction.** slog's `Attr` carries a `Value` union; redaction needs to walk both string-shaped values and `LogValuer` callbacks. Risk: a `LogValuer` that materializes a secret on demand bypasses pre-emission redaction. Mitigation: the Logger resolves every `Attr.Value` via `Value.Resolve()` BEFORE handing it to the redactor; documented on the public API. A `Group` attribute is recursively resolved.
- **Bus-emitter recursion.** A `BusEmitter` whose own implementation emits a log via the same `*Logger` would recurse infinitely on `Error`. Mitigation: `Logger.Error` invokes the emitter exactly once per call and the emitter contract documents "MUST NOT call back into the originating Logger." A test pins this with a fake emitter that calls `Logger.Error` and asserts the second call is suppressed.
- **OTel attribute collisions.** Phase 55 will populate `trace_id` / `span_id` from the OTel context; Phase 04 reserves the names. Risk: a contributor adds a `trace_id` via `With(...)` before Phase 55 lands and creates a stale convention. Mitigation: this phase plan documents that `trace_id` / `span_id` / `tool` / `task_id` are passthrough-only in Phase 04; conflicts with later auto-stamping resolve in favor of explicit `With(...)` to keep behavior monotonic.
- No RFC §11 open questions block this phase. Q-2 (Prometheus exporter) is settled in §6.14; the resolution does not affect Logger semantics.

## Glossary additions

- **Logger** — Harbor's canonical structured logger. Wraps `log/slog` with the eight-attribute standard set (`tenant_id`, `user_id`, `session_id`, `run_id`, `task_id`, `trace_id`, `span_id`, `tool`), routes every record through `audit.Redactor` before emission, and emits a paired `runtime.error` bus event on `Error` via the `BusEmitter` seam. Built once at boot; immutable after construction (D-025). RFC §6.14, brief 06 §1-§2.
- **BusEmitter** — interface that lets Phase 05+ wire a real event-bus emit into `Logger.Error` without re-opening the telemetry package. Phase 04 ships a no-op default. Implementations MUST NOT call back into the originating Logger.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/telemetry` ≥ 85%
- [ ] If multi-isolation paths changed: cross-session isolation test passes (N/A — Logger is identity-aware via ctx, but not identity-scoped storage; the concurrent-reuse test asserts no cross-talk in identity attributes)
- [ ] **Concurrent-reuse test passes — 100+ concurrent invocations against a single shared `*Logger` under `-race`, asserting no data races, no attribute cross-talk, no goroutine leaks.** Per AGENTS.md §5 + §11 + D-025.
- [ ] If new vocabulary: glossary updated (yes — `Logger`, `BusEmitter` terms added)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none)
