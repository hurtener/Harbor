# Research Brief 06 — Events, Observability, and Developer Experience

> **Status:** research input for the RFC and phase plans. Harbor-native vocabulary throughout. Source-code citations point to the predecessor codebase (internal context only) so we can validate decisions later; the predecessor itself is intentionally unnamed in this document.

---

## 1. Subsystem overview

Harbor treats **events as the canonical projection of runtime state**. Anything anyone outside the runtime — Console, CLI, third-party tools, audit log, observability vendor, IDE extension, TUI — needs to know about a running agent, they learn by subscribing to the event bus. The Runtime owns the bus; everything else is a client.

This is the deliberate inversion of the predecessor's split. There, the runtime emits a typed observability record (`FlowEvent`) on one channel, and streams partial output (`StreamChunk` carried inside `Message` envelopes) on a second channel (`~/Repos/Penguiflow/penguiflow/penguiflow/metrics.py`, `~/Repos/Penguiflow/penguiflow/penguiflow/streaming.py`). That works, but it forces every consumer to fuse two streams to reconstruct what happened in a run, and it conflates "wire-level event the protocol exposes" with "telemetry record the metrics middleware consumes." Harbor unifies them: **one typed event bus, protocol-grade**, used both for live UI streaming and for telemetry — with logging and OpenTelemetry deriving from the same events rather than being parallel paths.

Why protocol-grade matters: it guarantees Console, third-party consoles, and `harbor dev` see exactly the same data shape that production observability sees. There is no privileged "internal" view. Combined with the Harbor Protocol decoupling rule (Console NEVER reads runtime internals), this makes the event bus the single contract that has to stay stable across versions — and unlocks remote attach, multi-runtime fleet view, IDE/TUI integrations, and observability-vendor adapters as natural extensions rather than custom features.

---

## 2. Key data shapes (Go-flavored sketches)

```go
package events

// Event is the canonical record. All consumers see the same shape.
type Event struct {
    // Routing
    Type     EventType  // typed, exhaustive (see §"Event taxonomy" below)
    Sequence uint64     // monotonic per-bus, gap-free; used for ordering & replay
    EmittedAt time.Time

    // Identity (the isolation triple + run/step granularity)
    TenantID  string
    UserID    string
    SessionID string
    RunID     string  // a planner run inside a session
    TraceID   string  // OTel trace id (aligns with TraceContext)
    SpanID    string

    // Source
    Producer  string // "runtime", "planner", "tool:<name>", "task", "steering"
    NodeName  string // when applicable
    NodeID    string

    // Payload (typed via Type; never untyped any)
    Payload  EventPayload
    Extra    map[string]string // bounded, low-cardinality; safe for metric labels

    // Cost / resource accounting (when applicable)
    LatencyMs   *float64
    TokensIn    *uint32
    TokensOut   *uint32
    CostUSD     *float64
    QueueDepth  *QueueDepthSnapshot
}

type EventType string // exhaustively enumerated

type EventPayload interface{ eventPayload() } // sealed by package

type EventBus interface {
    Publish(ctx context.Context, ev Event) error
    Subscribe(ctx context.Context, filter Filter) (Subscription, error)
    // Replay reads from a sequence cursor; backed by the durable event log when available.
    Replay(ctx context.Context, from Cursor, filter Filter) (Subscription, error)
}

type Subscription interface {
    Recv(ctx context.Context) (Event, error)
    Ack(ev Event) error // for at-least-once durable subscribers
    Close() error
}

type Filter struct {
    TenantID, UserID, SessionID, RunID string // server-enforced isolation
    Types     []EventType
    Producers []string
    SinceSeq  *uint64
    Backpressure BackpressurePolicy
}
```

Companion shapes the same package owns:

- `Logger` — a thin wrapper around `log/slog` that pre-pins the standard attribute set (see §"Logging") and emits an event on `WARN`/`ERROR` so logs always have an event peer.
- `MetricsRegistry` — registers the canonical metrics derived from events. Backed by OTel Metrics SDK; Prometheus exporter as a driver.
- `Tracer` — `go.opentelemetry.io/otel/trace.Tracer` wrapper that injects `traceparent` into outgoing HTTP southbound calls and `_meta.traceparent`/`HARBOR_TRACEPARENT` into MCP southbound (env on stdio spawn, `_meta` per request — same convention as the Portico gateway).
- `CLICommand` — `cobra.Command` wrapper enforcing Harbor's CLI conventions (structured errors, `--quiet`, `--dry-run`, `--json` output mode, hint strings).
- `TestKit` — see §"Test kit"; a public `harbortest` package consumers import.

---

## 3. Public API surface

**What the Runtime emits.** Every subsystem produces typed events through a single `Bus.Publish` call. Subsystems do not own their own observability channel. Concretely:

- Session/Run lifecycle: `session.opened`, `session.closed`, `run.started`, `run.completed`, `run.failed`, `run.paused`, `run.resumed`, `run.cancelled`.
- Planner step: `planner.step.started`, `planner.step.completed`, `planner.decision`, `planner.tool_chosen`, `planner.parallel_dispatch`, `planner.join`, `planner.reflect`, `planner.finish`. (Generic across planner implementations — the planner interface emits these regardless of strategy.)
- Tool execution: `tool.invoked`, `tool.completed`, `tool.failed`, `tool.timeout`, `tool.retry`, `tool.cancelled`.
- LLM: `llm.request`, `llm.stream.chunk`, `llm.stream.done`, `llm.completed`, `llm.failed` (carries `TokensIn`/`TokensOut`/`CostUSD`).
- Task (foreground + background unified): `task.spawned`, `task.progress`, `task.completed`, `task.failed`, `task.cancelled`, `task.merged` (background-task results merging into parent).
- Steering: `steering.received`, `steering.applied`, `steering.rejected` for `CANCEL`, `REDIRECT`, `INJECT_CONTEXT`, `USER_MESSAGE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `PRIORITIZE`.
- Memory / Skills / Artifacts: `memory.injected`, `memory.evicted`, `skill.retrieved`, `skill.generated`, `artifact.created`, `artifact.referenced`, `artifact.deleted`.
- Bus / queue health: `queue.saturated`, `bus.dropped` (with reason and dropped-event sequence range).
- Errors: `runtime.error`, `runtime.panic_recovered`. Errors are first-class events; they are *not* "logs that look like events."

(The predecessor's enumeration covers ~12 runtime events in `core.py` and ~20 planner events in `planner/react.py` — Harbor's larger taxonomy is the cost of unifying everything onto one bus, and the cost is paid once.)

**What subscribers consume.** Clients call `Subscribe(ctx, Filter)` and receive a `Subscription`. Filters are server-enforced for isolation: a Console subscription always passes `(tenant, user, session)` from its authenticated identity, and the runtime rejects any subscribe call that omits them unless the caller has explicit `admin` scope. A late subscriber requesting replay from a cursor receives historical events from the durable log (when configured) interleaved with live events; the bus guarantees no duplicates and no gaps within a `RunID`.

**What `harbor dev` exposes.** `harbor dev` boots the runtime headless, opens the protocol on `127.0.0.1:<port>`, starts the embedded Console, watches the project directory for changes, hot-reloads on Go-source changes (graceful-stop in-flight runs first; configurable), and exposes a draft-save scratchpad endpoint for the dynamic-agent-scaffolding flow described in `harbor_inherited_lessons.md`. All of this is implemented as protocol clients of the same runtime — no private hooks.

**What the test kit gives authors.** A `harbortest` package: `RunOnce(ctx, agent, input) (Output, EventLog, error)`; `AssertSequence(log, []EventType{...})`; `AssertNoLeaks(log)` (cross-tenant/session leakage detector); `SimulateFailure(toolName, code, n)`; `RecordedEvents(runID) []Event`. The point is the same as the predecessor's `testkit.py` (`~/Repos/Penguiflow/penguiflow/penguiflow/testkit.py`): make a flow-level test ten lines or fewer.

---

## 4. Internal mechanics

**Bounded channels with explicit drop policy.** Each subscription is backed by a bounded buffered channel. Drop policy is **drop-oldest** by default, and the moment the bus drops an event for a subscriber it emits a `bus.dropped` event on that subscriber's stream describing the dropped sequence range and reason. This converts silent loss into a visible, replayable signal — the predecessor logs queue depth in the `FlowEvent` (`queue_depth_in`, `queue_depth_out`, `trace_pending`, `trace_inflight`, see `metrics.py:11-72`) but does not have the "I dropped these specific sequences" guarantee, and that becomes hard to debug under saturation.

**Filter expressions.** Filters are evaluated server-side before fan-out. Cardinality of subscribers is bounded per session (configurable; default ~16 simultaneous subscribers per session). A subscription with a filter that matches nothing for 60s is reaped (the protocol surfaces this with a `subscription.idle_closed` control message) so misbehaving clients do not pin runtime state forever.

**Fan-out.** Publish is O(1) — it stamps `Sequence`, attaches identity from `ctx`, and pushes onto an MPSC ingress channel. A bus dispatcher fans out to per-subscription channels with non-blocking sends (drop-oldest if full). The ingress channel is sized so a healthy runtime does not block the producer; saturation there triggers the `bus.saturated` event and slows producers via `ctx.Done()`-driven backpressure rather than blocking forever.

**Replay semantics.** Late subscribers can request replay-from-cursor. Without a durable log, replay is best-effort within an in-memory ring buffer (default 10k events, configurable). With a durable log driver attached (which uses the StateStore from research brief 05), replay is exact. The `events.Cursor` is just `(SessionID, Sequence)`; clients that resume after a disconnect pass the last acknowledged cursor and pick up cleanly.

**Isolation-triple filtering by default.** Subscribe ignores any filter that elides `TenantID`/`UserID`/`SessionID` unless the caller has `admin` scope. Cross-tenant subscriptions are an explicit, audited operation. This is the runtime analogue of Portico's tenant-scoped query rules and is one of the three load-bearing isolation guarantees in `harbor_isolation.md`.

---

## 5. Sharp edges from the source (don't repeat)

- **Two-channel split.** `FlowEvent` (`metrics.py`) is a separate record from `StreamChunk` (`streaming.py`), and both flow through different paths: events go through the middleware chain; chunks go through the message bus inside `Message.payload`. Every dashboard, replay tool, and Console feature in the source has to fuse them. **Lesson:** unify on one bus from t=0.
- **Middleware-as-event-sink.** The predecessor wires telemetry as middleware that fires per-node (`middlewares.py:20-84` — `log_flow_events`). It works, but it couples observability lifetime to node execution. Harbor's bus-first model lets observability subscribe at any granularity (planner step, tool, run, session) without inserting middleware everywhere.
- **No OpenTelemetry in the runtime.** The runtime has no OTel instrumentation; OTel only appears in the playground UI's JS dependencies. **Lesson:** OTel traces and metrics should be a first-class derivation of the event bus, shipped from t=0, not retrofitted.
- **Logging in two formats with a flag.** `configure_logging(structured: bool)` (`logging.py:102-152`) toggles between human and JSON. Harbor: pick one shape per environment via slog handler — JSON in production, text in dev — no toggle inside the library. The predecessor also notes "Avoid logging large payloads; prefer artifacts/resources and log references" (`docs/observability/logging.md`) — bake this into the Logger so it can't be forgotten.
- **Tightly coupled Playground.** `cli/playground.py` is 2,478 lines and embeds session management, generation, steering, tasks, AGUI, and the SSE bus in one FastAPI process (`cli/playground.py:1346-2426` enumerates 30+ HTTP routes, several of them duplicating runtime APIs). The Playground both *consumes* and *re-implements* runtime concepts. Harbor avoids this by making `harbor dev` boot the runtime in-process and have the Console talk to it via the protocol — no Playground-private endpoints. `playground_state.py` shows the strain: a `PlaygroundStateStore` Protocol (`cli/playground_state.py:20-37`) that re-declares a subset of `StateStore` because the Playground was implemented before unification.
- **Skill / spec generator can draft but not save.** `cli/generate.py` runs once and emits files; there is no "draft, iterate, save when ready" loop. Harbor's `harbor dev` ships this from the start (per `harbor_inherited_lessons.md`).
- **Visualization couples to private state.** `viz.py:137` accesses `flow._floes` directly. Harbor's visualization derives from the canonical event/topology surface published over the protocol — no private fields.
- **Metrics cardinality footgun.** The predecessor docs warn "Never tag metrics by `trace_id`" (`docs/observability/metrics-and-alerts.md`). In Harbor, `Event.TraceID` is for logs and OTel traces; `MetricsRegistry` derives metrics from `Event.Type`/`NodeName`/`Producer` only, and the metrics derivation layer enforces this — a developer cannot accidentally tag a metric by `RunID` or `TraceID`.

---

## 6. Tests required

- **Unit tests** for `Event`, `EventType` exhaustiveness, `Filter` parsing, `Cursor` ordering.
- **Fan-out tests**: 100 subscribers, single producer, all see all events with correct ordering.
- **Drop-policy tests**: slow subscriber, fast producer, asserts `bus.dropped` events with correct sequence ranges.
- **Late-subscriber replay tests**: subscribe-from-cursor with both ring-buffer and durable backends.
- **Cross-tenant isolation tests**: subscriber for tenant A receives zero events emitted by tenant B; `admin` scope can bypass; assertion on the audit event for the bypass.
- **OTel integration tests**: events translate to spans correctly; `traceparent` propagated through HTTP southbound; `_meta.traceparent` propagated through stdio MCP.
- **CLI golden tests**: `harbor dev --help`, `harbor scaffold --help`, `harbor validate`, `harbor inspect-events --run <id>` produce stable output matching golden files.
- **Hot-reload tests**: file change triggers graceful run drain and restart; in-flight runs cancel cleanly; new runs pick up new code.
- **Logging tests**: `Logger.Error` emits both an slog record and a paired `runtime.error` bus event; standard attribute set always present.
- **Metrics-cardinality lint test**: a static check that fails CI if any metric registers a label deriving from `TraceID`/`RunID`/free-form input.

---

## 7. Phase decomposition suggestion

These compose into the larger 50-phase plan; rough sizing of each:

1. **Event taxonomy + in-memory `EventBus`.** `Event`, `EventType`, sealed `EventPayload`, MPSC ingress, fan-out, drop-oldest with `bus.dropped`. ~1 phase.
2. **Filter and isolation enforcement.** Server-side filtering, identity-triple gates, admin-scope bypass with audit event. ~1 phase.
3. **Replay semantics.** In-memory ring buffer + cursor; replay-from-cursor with gap-free guarantee. ~1 phase.
4. **Durable event log driver.** Backed by the StateStore subsystem (cross-fork dependency on brief 05's seam); persists `Event` records keyed by `(SessionID, Sequence)`. ~1–2 phases.
5. **slog Logger + standard attribute set.** Pinned attributes; JSON in prod, text in dev; auto-emit `runtime.error` events on `ERROR`. ~1 phase.
6. **OpenTelemetry traces + metrics.** Tracer wrapper, span lifecycle from events, propagation conventions (`traceparent` HTTP / `_meta.traceparent` MCP / `HARBOR_TRACEPARENT` env on stdio spawn). ~1–2 phases.
7. **Metrics derivation + Prometheus driver.** Cardinality-safe registry; OTLP exporter default; Prometheus exporter as alt driver. ~1 phase.
8. **Harbor CLI skeleton.** `harbor` binary with subcommands `dev`, `scaffold`, `validate`, `version`, `inspect-events`, `inspect-runs`. Cobra-based. ~1 phase.
9. **`harbor dev` v1.** Boot embedded runtime + embedded Console over local protocol; no hot-reload yet. ~1 phase.
10. **`harbor dev` hot-reload.** fsnotify watcher, graceful-drain restart policy, in-flight-run handling. ~1 phase.
11. **Draft-save dynamic agent scaffolding.** `harbor scaffold` with draft persistence in a project-local `.harbor/drafts/` (or whatever the project layout settles on); `harbor dev` reads/writes drafts and provides the protocol surface for Console-side iteration. ~1–2 phases.
12. **Configuration loader.** YAML/env/flag layering; reload semantics (declared per-key as `restart_required` or `live`); validation errors point to source. ~1 phase.
13. **Test kit (`harbortest`).** `RunOnce`, `AssertSequence`, `AssertNoLeaks`, `SimulateFailure`, `RecordedEvents`. ~1 phase.
14. **Visualization protocol surface.** Topology snapshots published over the bus as `topology.snapshot` events; Console consumes; CLI `harbor inspect-topology --run` renders ASCII. ~1 phase.
15. **Console feature parity gates.** Each operator-creatable resource visible from list view; Playwright e2e against `harbor dev` per the Portico §4.5.1 model. (Spans multiple phases as Console grows.)

That is 13–15 phases for events/observability/devx alone, fitting comfortably inside a 50-phase plan once the other subsystems take their phases. Each phase ships with smoke checks (per `feedback_harbor_doc_hygiene.md`) and updates the contributor docs.

---

## 8. Cross-subsystem dependencies

- **Producers:** every other subsystem (planner, tools, LLM client, tasks, steering, memory, skills, artifacts, sessions, MCP/A2A southbound, the runtime kernel itself) emits to the bus. The contract is "import the events package, get the `Producer` constant for your subsystem, call `Publish`."
- **Persistence:** the durable event log driver depends on the StateStore (research brief 05). Without it, replay degrades to ring-buffer-only. The runtime ships a usable in-memory experience without StateStore.
- **Console:** Console subscribes to the bus through the Harbor Protocol surface. Console renders projections; it does not import the events package.
- **CLI:** `harbor dev` and `harbor inspect-*` are bus consumers via the protocol — they use the same client SDK a third-party tool would.
- **Audit:** auditing is a bus subscriber that persists a redacted projection. It is not a parallel system.
- **A2A / MCP southbound:** outbound calls use `Tracer` to inject propagation headers; inbound calls extract them and stamp them on emitted events. This makes traces continuous across processes.

---

## 9. Open questions for the user

1. **Wire format.** The protocol exposes the bus to Console, CLI, and third-party clients. Candidates: gRPC server-streaming, NDJSON over chunked HTTP, SSE + REST hybrid, JSON-RPC + WebSocket. Each has tradeoffs (TUI ergonomics, browser support, multiplexing, language client maturity). Pick one before we lock the protocol.
2. **Metrics exporter set at V1.** OTLP is the safe default. Should Harbor also ship a built-in Prometheus `/metrics` endpoint at V1 (popular for self-hosted setups), or treat that as a post-V1 driver?
3. **Default subscription filters.** Should `harbor dev`'s Console default to `(tenant, user, session)` of the active run, or fan-in across all runs of the local user? The first is safer; the second is friendlier for multi-run debugging.
4. **Event schema versioning.** Best-effort additive (new `EventType`s and new optional fields are non-breaking)? Strict semver with deprecation windows? The latter is heavier but matters once third-party Consoles exist.
5. **CLI subcommand breadth at V1.** Confirmed: `harbor dev`. Proposed for V1: `scaffold`, `validate`, `inspect-events`, `inspect-runs`, `inspect-topology`, `version`. Should `deploy` and `package` also land at V1, or wait for Harbor Cloud's shape?
