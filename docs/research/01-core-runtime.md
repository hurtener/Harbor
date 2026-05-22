# Research Brief 01 — Core Runtime + Streaming

Status: research / pre-RFC. Internal vocabulary proposed below; final names settle in the RFC.

## 1. Subsystem overview

The Core Runtime is the foundation of the Harbor Runtime layer. Every other subsystem (Tools, Memory, Tasks, Skills, Sessions, Steering, Artifacts, Planner) sits on top of it. It owns three things and three things only:

1. **A typed, async, queue-backed graph** of `Node`s that exchange `Envelope`s along `Channel`s. The graph is a DAG by default, with explicit opt-in for self-loops on individual nodes.
2. **A reliability shell** around each node invocation — timeouts, retries-with-backoff, validation, deadlines, per-run cancellation, and an error envelope that carries enough context to debug a production failure offline.
3. **A streaming primitive** for incremental outputs that share a parent run identity, with sequence ordering and per-stream backpressure.

It is foundational because it decides the *shape* of every other subsystem's contract. Planners drive nodes; Tools are nodes; Memory and Skills are consulted between node hops; Tasks are tracked by run identity; Steering injects events into the same queues; the Protocol streams the same Envelopes outward to clients. If the core is wrong, everything above it inherits the cost.

It is **not** responsible for: planner reasoning, tool transports, memory policy, skill retrieval, persistence, the protocol surface, observability sinks, or HITL. Those are upstream subsystems that *consume* the core.

## 2. Key data shapes

Sketches — not final. Identifiers, generics, and tag layout are RFC concerns.

```go
// Envelope is the canonical message shape on every channel.
type Envelope struct {
    Payload    any              // typed by NodeRegistry contract
    Headers    Headers          // routing + identity
    RunID      string           // stable per run (replaces "trace_id")
    SessionID  string           // session this run belongs to
    Timestamp  time.Time
    DeadlineAt *time.Time       // wall-clock; nil = no deadline
    Meta       map[string]any   // free-form propagation (cost, debug, A2A binding ids)
}

type Headers struct {
    TenantID string
    UserID   string
    Topic    string
    Priority int
}

// StreamFrame is a chunked payload tied to a parent run.
type StreamFrame struct {
    StreamID string         // defaults to RunID; can be sub-stream within a run
    Seq      int            // monotonic per StreamID
    Text     string         // model-emitted text or adapter-converted
    Done     bool           // terminal frame for this StreamID
    Meta     map[string]any // tokens, finish reason, citations, etc.
}

// Node wraps a typed async function.
type Node struct {
    Name       string
    Func       NodeFunc
    Policy     NodePolicy
    AllowCycle bool
    nodeID     string // stable runtime id
}

type NodeFunc func(ctx context.Context, in Envelope, nctx *NodeContext) (Envelope, error)
// nil result == "no emission, drop"; one Envelope == one downstream emit;
// multiple emissions go via NodeContext.Emit explicitly.

type NodePolicy struct {
    Validate     ValidateMode  // both / in / out / none
    TimeoutMS    int           // 0 = none
    MaxRetries   int
    BackoffBase  time.Duration
    BackoffMult  float64
    MaxBackoff   time.Duration
}

// NodeContext is the per-invocation handle a Node receives.
type NodeContext struct {
    // Hidden state — channels, runtime ref, stream sequence map.
}

// Engine is the runtime container.
type Engine interface {
    Emit(ctx context.Context, env Envelope, opts ...EmitOption) error
    EmitTo(ctx context.Context, env Envelope, target NodeRef) error
    Fetch(ctx context.Context, opts ...FetchOption) (Envelope, error)
    FetchByRun(ctx context.Context, runID string) (Envelope, error)
    Cancel(ctx context.Context, runID string) (bool, error)
    Stop(ctx context.Context) error
}

type EngineOption func(*engineConfig)
// queue size, allow_cycles, middleware list, error-emission policy, message bus, etc.

type RunError struct {
    RunID, NodeName, NodeID string
    Code     RunErrorCode  // NodeTimeout, NodeException, RunCancelled, DeadlineExceeded, ...
    Message  string
    Cause    error
    Metadata map[string]any
}
```

### Key design choices to bake in

- **Identity is a quadruple, not a single id.** `(TenantID, UserID, SessionID, RunID)` is the runtime identity. `RunID` is the active concurrency boundary; `SessionID` groups runs into a multi-turn conversation.
- **`DeadlineAt` is wall-clock, not duration.** Set once at the boundary; checked before scheduling each node.
- **`Meta` is free-form.** It survives fan-out, fan-in, and subflow boundaries. Last-write-wins on key collisions unless an explicit merge function is registered (deferred to an RFC follow-up).
- **`Validate` is per-node.** `both / in / out / none` — the perf escape hatch (`none` on hot streaming paths) is necessary, keep it.

## 3. Public API surface

What the runtime exposes to other subsystems:

- `engine.New(adjacencies ..., opts ...)` — build an Engine from a list of `(Node, []Node)` adjacency pairs. Cycle detection runs at construction; `WithAllowCycles()` opts in.
- `engine.Run(registry)` — start the worker goroutines, one per node.
- `engine.Stop()` — graceful shutdown: cancel workers, drain in-flight invocations, release dispatchers, clear capacity waiters.
- `engine.Emit(env, ..., WithRunID(id))` — ingress. Without `WithRunID`, the run identity is read from the envelope.
- `engine.Fetch(WithRunID(id))` — egress. Without `WithRunID`, returns whatever lands first in the global egress queue. With `WithRunID`, demultiplexes per-run via a dispatcher goroutine.
- `engine.Cancel(runID)` — idempotent per-run cancellation that propagates through queues and active invocations.
- `NodeContext` exposes `Emit / EmitNoWait / EmitChunk / Fetch / FetchAny / FetchNoWait / CallSubflow`. The subset is wider than most graph runtimes need because the planner uses it for fan-out, controller-style multi-hop loops, and SSE-style streaming sinks.
- `Subflow(factory, parent, opts...)` — runs a child Engine with the parent's run id, mirrors parent cancellation into the child, returns the first egress payload.
- `NodeRegistry` — per-node `(InType, OutType)` registration plus a validator function (e.g. JSON-schema or Go-generic validator). Validation is opt-in per `NodePolicy.Validate`.

## 4. Internal mechanics

**Worker loop.** One goroutine per `Node`. Each iteration: `Fetch` from incoming channels → check deadline → check cancel → invoke under reliability shell → emit to outgoing channels (or to the Outlet) → finalize bookkeeping. Cancellation propagation uses a sentinel error (`RunCancelled`) that unwinds the loop without killing the worker.

**Channel semantics.** Each adjacency `(A, B)` gets a bounded queue of size `QueueMaxSize` (default 64). Backpressure is implicit: a slow consumer pauses upstream `Emit`. Two synthetic endpoints — `Inlet` (ingress) and `Outlet` (egress) — receive channels from "nodes with no parents" and "nodes with no children" respectively, so external code talks to the engine via the same channel mechanic.

**Backpressure inside streaming.** A run that emits hundreds of stream frames could fill its outgoing queue and block the producing goroutine. Harbor addresses this with a per-run capacity waiter: per-run pending counters gate chunk emissions when a single run's pending count exceeds the queue maxsize. This is *not* a nice-to-have — without it, parallel runs can deadlock each other through shared bounded queues.

**Cancellation propagation.** `Cancel(runID)` does four things: sets a per-run atomic flag, drops the run's already-enqueued envelopes from every channel, cancels active invocation goroutines, and drains the per-run egress queue. Subflow runs mirror parent cancellation via a watcher goroutine. `Cancel` returns `bool` indicating whether the run was active; idempotent.

**Reliability shell.** The reliability shell wraps the node call with timeout, retry-with-backoff, and run-cancel checks. On terminal failure it constructs a `RunError` and optionally routes it to the egress. Harbor keeps the shell, keeps the optional error-to-egress, and adds an "error-to-protocol" hook so Console can render failures without the egress consumer needing to handle them.

**Trace-scoped fetch dispatcher.** A separate goroutine reads from the egress queue and demultiplexes results into per-run subqueues so that callers can `FetchByRun(runID)` without ordering surprises. This is non-trivial — it is what makes the API "one engine, many concurrent runs, each addressable" instead of "one engine, one outstanding consumer at a time."

**Subflow lifecycle.** A subflow is a freshly-built engine that runs to completion for one parent envelope, then `Stop`s. Cancellation is mirrored from the parent. The cleanup is `defer cancel(); defer engine.Stop()`.

## 5. Sharp edges to design out

Catalogued so they are designed-out, not rediscovered:

- **An egress endpoint with two bolted-on modes is a trap.** A pre-dispatcher mode where `Fetch` reads directly from incoming channels, plus a post-dispatcher mode where a separate goroutine demuxes into per-run subqueues, leaves `Fetch(from=...)` filtering unsupported once the dispatcher engages. Harbor: pick one model and ship it. Recommendation — the dispatcher is on by default, always, and the API is consistent. There is no legacy "before" mode to be compatible with.
- **A non-blocking emit that silently rejects a run id is a trap.** Harbor: the API should be type-shaped so this can't be expressed at compile time, e.g. only `Emit` takes `WithRunID`, not `EmitNoWait`.
- **A type-mismatch between declared and returned message types must be a hard error, not a warning.** A node registered for `Envelope -> Envelope` that returns a raw payload must fail loudly. Static typing in Go catches most of this; the runtime check on top should `RunError` rather than log-and-continue.
- **`Stop` releases capacity waiters by setting them.** Any goroutine awaiting capacity resumes, sees no engine, and must error out cleanly. The waiter's resumption path needs an explicit "engine stopped" sentinel, not "you happen to observe run_count=0."
- **Deadline expiry must not synthesize a planner-layer payload.** Synthesizing a `FinalAnswer` on deadline is a planner-layer concern leaking into the runtime. Harbor: the runtime emits a `RunError(DeadlineExceeded)` to the egress; planners can choose to convert that into a final answer for the user, but the runtime stays out of presentation.
- **Working-memory hop dedup does not belong in the runtime.** Short-circuiting emit calls when a working-memory payload's `hops` matches the last seen value is a controller-loop optimization. Harbor: the planner subsystem owns this, the runtime stays generic.
- **Per-run roundtrip locks are subtle.** Locks that serialize concurrent emit/fetch calls sharing the same run id have a subtle failure mode (lock not released on error path). Harbor: design `Emit/Fetch` so concurrent same-run roundtrips either work natively or are forbidden by the API; no half-measure.
- **Bus publishing failures must be surfaced, not logged.** Harbor: surface a downstream-bus failure to the protocol/observability path during graph emit; never silently swallow it.
- **Subflow registries recreated each call cost hot paths.** A subflow factory returning a fresh engine and registry on every invocation is correct but expensive. Harbor: cache the validator adapters at registration time; construct the engine cheaply.

## 6. Tests required

Coverage gates per Harbor's CI rigor (`feedback_harbor_doc_hygiene.md`):

### Unit

- Envelope construction defaults (`RunID`, `Timestamp`).
- `NodePolicy` validation modes.
- Cycle detector accepts trees / DAGs; rejects cycles unless `AllowCycle` is set.
- Backoff calculation given various `attempt`/`MaxBackoff` combinations.
- `RunError.ToPayload()` round-trip (JSON).

### Integration

- Linear graph: emit → fetch returns expected envelope.
- Fan-out + `JoinK`: K parallel branches reduce to one output.
- `MapConcurrent` honors max-concurrency bound.
- Predicate router selects targets; explicit policy overrides predicate.
- Streaming: per-stream `Seq` ordering, terminal `Done` frame, downstream backpressure under a slow consumer.
- Subflow: parent emits, subflow processes, parent receives.

### Concurrency

- N concurrent runs, each addressed by `RunID`, no cross-run frames in `FetchByRun`.
- Capacity waiter does not deadlock under streaming + parallel runs.
- `Cancel(runID)` drops queued envelopes for that run only; other runs continue.
- Subflow cancellation mirrors when the parent is cancelled mid-subflow.

### Goroutine leak

- `runtime.NumGoroutine()` returns to baseline after `Stop()` for: idle engine, engine with in-flight runs, engine with pending capacity waiters, engine running a subflow.

### Fuzz / property

- Cycle detector against random graphs.
- `Cancel` is idempotent regardless of when it's called relative to the run lifecycle.

### Smoke (per-phase scripts)

- `harbor dev` boots; protocol stream produces the same `node_start / node_success / node_failed` events the runtime logs.

## 7. Phase decomposition

Six phases for this subsystem, each shippable on its own:

### Phase 01a — Envelopes, Headers, Identity

- Scope: `Envelope`, `Headers`, identity quadruple, `Meta` propagation rules.
- Acceptance: type-checks; `Envelope.WithRunID` returns a copy; `(Tenant, User, Session, Run)` round-trips through serialization.
- Tests: unit + serialization round-trip.
- Smoke: N/A (no surface yet).

### Phase 01b — Engine skeleton + Channel + worker loop

- Scope: `Engine`, `Channel`, `Inlet/Outlet`, worker goroutines, `Run / Stop`, cycle detection, basic `Emit / Fetch`.
- Acceptance: linear graph end-to-end works; `Stop` joins all goroutines; goroutine-leak test passes.
- Tests: unit + leak test + stop-while-emitting test.
- Smoke: dev binary boots an empty engine, `/healthz`-equivalent returns OK.

### Phase 01c — Reliability shell (timeout, retry, validation, errors)

- Scope: `NodePolicy`, validate modes, timeout, retry-with-backoff, `RunError`, error-emission policy.
- Acceptance: timeout produces `RunError(NodeTimeout)`; retries respect `MaxRetries`; validate=both rejects malformed envelopes.
- Tests: unit on backoff math; integration on each error code.
- Smoke: a deliberately-failing node produces a structured `RunError` on the egress.

### Phase 01d — Streaming primitive + per-run capacity backpressure

- Scope: `StreamFrame`, `EmitChunk`, per-stream `Seq`, capacity waiters keyed by run.
- Acceptance: N parallel runs each streaming K frames; ordering preserved per `StreamID`; no cross-run deadlock.
- Tests: integration + concurrency + goroutine-leak under streaming.
- Smoke: stream a synthetic 100-frame run end-to-end via egress.

### Phase 01e — Cancellation + per-run fetch dispatcher

- Scope: `Cancel(runID)`, queue draining, invocation cancellation, per-run egress demux, `FetchByRun`.
- Acceptance: `Cancel` is idempotent, drops only the targeted run, returns true/false correctly; `FetchByRun` never returns frames from another run.
- Tests: integration + concurrency.
- Smoke: start two runs, cancel one, verify the other completes.

### Phase 01f — Routers, concurrency utilities, subflows

- Scope: `PredicateRouter`, `UnionRouter`, `RoutePolicy`, `MapConcurrent`, `JoinK`, `Subflow`.
- Acceptance: each pattern matches its specified behavior; subflow cancellation mirrors parent.
- Tests: integration per pattern.
- Smoke: a fan-out / join-K example flow runs to completion via the dev binary.

## 8. Cross-subsystem dependencies

**This subsystem needs (downstream):** nothing from elsewhere in Harbor. Pure foundation.

**This subsystem is needed by (upstream):**

- **Events bus** — emits typed events from worker-loop hooks; this subsystem provides the hooks.
- **Protocol** — `EventStream` projects the event-bus output; `TaskControl` calls `Engine.Cancel`; `Observability` reads queue depths and worker state.
- **Tasks** — task identity layered on top of `RunID`; foreground tasks are runs.
- **Tools** — every tool call is a node invocation; the tool transport adapters are nodes.
- **Planner** — orchestrates emits, owns deadline interpretation, owns final-answer presentation.
- **Memory / Skills** — consulted between hops via `NodeContext` helpers (added in their own phases).
- **Steering** — injects events keyed by `RunID`; uses `Cancel` for hard interrupts.
- **Sessions** — group `RunID`s by `SessionID`; this subsystem provides the identity.

## 9. Open questions for the user

1. **Run vs trace vocabulary.** Harbor user notes already establish `RunID` as the runtime concurrency boundary. Confirm: keep `RunID` as the canonical name; reserve `TraceID` for OpenTelemetry-style traces (often spanning multiple runs). OK?
2. **Subflow as a first-class API or as planner sugar?** Harbor could keep `Subflow` in the runtime, or push it to the planner subsystem since it is mostly used for controller-style reasoning. Strong runtime reason to keep it: cancellation mirroring is engine-level, not planner-level.
3. **Per-node validation: schema-first or Go-generic-first?** Harbor options: (a) per-node `(In, Out any)` plus a function pointer, (b) JSON-schema validation independent of Go types, (c) generics-typed nodes (`Node[I, O]`) so the compiler enforces shape and runtime validation only handles wire-form ingress. Recommendation: (c) for the typed core, (b) reserved for protocol-edge ingress where the type is dynamic.
4. **Default queue maxsize.** Default to 64. Harbor's three persistence backends (in-mem / SQLite / Postgres) imply different practical bounds. Set per-engine in config, or per-channel? Recommendation: per-engine default plus a per-channel override.
5. **Error routing default.** Harbor's protocol-first stance suggests errors should *always* surface to the protocol's event stream regardless of whether they also go to the egress envelope path. Confirm: errors go to protocol unconditionally; egress emission is the optional one.

---

### Subsystem concept map

| Concept in this brief | Notes |
|---|---|
| `Engine`, `Channel`, `Inlet`, `Outlet` | runtime container, queue edges, synthetic endpoints |
| `Envelope`, `Headers`, `StreamFrame` | typed wire shapes |
| `Node`, `NodePolicy`, validate modes | node wrapper + execution policy |
| Reliability shell, retry, timeout | per-invocation |
| `Cancel`, capacity waiter | per-run lifecycle |
| `RoutePolicy`, predicate / union router | routing |
| `MapConcurrent`, `JoinK` | concurrency utilities |
| `Subflow` | subgraph execution |
| `RunError` | structured error envelope |
| Streaming primitive | chunked output + adapters |
