# RFC-001 — Harbor: Architecture & V1 Scope

> **Status:** Drafting (active)
> **Author:** hurtener
> **Last updated:** 2026-05-08
> **Supersedes:** none

This RFC specifies what Harbor is, what it ships at V1, and the binding architectural decisions that all phase plans must respect. Where a section says **Settled**, the decision is closed unless this RFC is amended. Where a section says **Tentative — see §11 Q-N**, an open question must be resolved before the relevant phase ships.

This document is the highest-priority artifact in the repository (see `AGENTS.md` §2). Phase plans, code comments, and contributor docs all defer to it. If a phase plan and this RFC drift, the RFC wins; the plan must be updated.

---

## 1. Executive summary

Harbor is a Go-native runtime SDK for durable, steerable, event-driven AI agents. It ships as a Go module plus a single static binary (`harbor`), with a four-layer architecture:

1. **Harbor Runtime** — the orchestration kernel: tasks, planner runtime, tools, memory, sessions, events, skills, artifacts, the unified pause/resume primitive.
2. **Harbor Protocol** — the canonical event/state contract that the Runtime exposes to any client. Versioned independently.
3. **Harbor Console** — the observability and control-plane UI. A *Protocol client*; ships with the ecosystem; architecturally decoupled.
4. **Harbor CLI** — the `harbor` binary. `harbor dev` boots a local Runtime + Console with hot reload and dynamic agent scaffolding (with draft saving).

V1 ships:

- The Runtime layer with **all** of the subsystems listed in §6.
- The Protocol layer with one wire transport (Settled in §5; Tentative — see §11 Q-1).
- The CLI with `harbor dev`, `harbor scaffold`, `harbor validate`, `harbor inspect-events`, `harbor inspect-runs`, `harbor version`.
- A persistence triad (in-memory / SQLite / Postgres) behind every persistence-shaped interface.
- The reference `react` planner; the `Planner` interface; one second concrete (`deterministic`) to prove the seam.

V1 does **not** ship the Console (separate repo), Harbor Cloud (post-V1), durable distributed transports beyond in-process contracts, or planner concretes beyond `react` and `deterministic`.

Harbor's three non-negotiable product properties — multi-isolation across `(tenant, user, session)`, the Console-as-Protocol-client decoupling, and the swappable Planner — are baked into the architecture from t=0. They are recorded as binding rules in `AGENTS.md` §1, §6, §8 and reiterated below.

---

## 2. Goals and non-goals

### 2.1 Goals

- **G1.** Provide a Go-native runtime with first-class concurrency, durability, and steerability for AI agents — the gap that the wider Go ecosystem currently leaves open.
- **G2.** Ship the architectural seams that long-lived agent platforms turn out to need (events, identity, pause/resume, mandatory artifacts, swappable planner) **from t=0**, not retrofitted.
- **G3.** Operate correctly under multi-isolation: `(tenant, user, session)`, including concurrent sessions for the same user. Cross-session leakage is a security bug, not a style nit.
- **G4.** Expose a versioned Protocol that a Console (ours or third-party), a CLI, an IDE extension, a TUI, or an observability vendor can implement against without reaching into Runtime internals.
- **G5.** Keep the Runtime planner-independent: every Runtime feature must be reachable from any conformant `Planner`.
- **G6.** Make `harbor dev` feel seamless for a developer (local Runtime + Console + hot reload + draft-save scaffolding) while keeping the Console-as-Protocol-client property intact.
- **G7.** Ship doc-and-CI hygiene from t=0: in-repo design (RFC, phase plans, research briefs, AGENTS.md ↔ CLAUDE.md mirror), a preflight gate, per-phase smoke scripts, conformance suites for every multi-driver subsystem.

### 2.2 Non-goals (V1)

- **NG1.** A distributed execution backend with at-least-once / exactly-once durable bus semantics. V1 ships the *contracts* (`MessageBus`, `RemoteTransport`); production drivers (NATS, Redis Streams, Postgres-as-queue) land in a post-V1 phase set.
- **NG2.** Harbor Cloud (managed execution plane). External, post-V1.
- **NG3.** A library of planner concretes beyond `react` and `deterministic`. The `Planner` interface ships, plus one extra concrete to prove the interface holds. PlanExecute, Workflow, Graph, Supervisor, MultiAgent, HumanApproval are post-V1 phases.
- **NG4.** Embedding the Console binary inside the Runtime binary. Consoles are protocol clients; bundling is a deployment convenience, not an architecture.
- **NG5.** A persistent durable backend for background tasks. V1 keeps background tasks in-process; the durable backend is a post-V1 phase that slots in behind `TaskRegistry`.

---

## 3. Architecture overview

### 3.1 The four layers

```text
                                +-----------------------+
                                |    Harbor Console     |
                                |  (Protocol client;    |
                                |   own repo or         |
                                |   web/console/)       |
                                +-----------+-----------+
                                            |
                                            |  Harbor Protocol
                                            |  (events / state /
                                            |   task control / obs)
                                            v
+------------------+    Protocol    +----------------+
|  Harbor CLI      |<-------------->|  Harbor        |
|  (`harbor dev`,  |                |  Runtime       |
|   scaffold, ...) |                |  (the kernel)  |
+------------------+                +----------------+
                                            |
                                            v
                          Tools, MCP, A2A, HTTP, in-process
```

The CLI and the Console are both Protocol clients. `harbor dev` uses the same protocol code path as a remote browser-attached Console. There is no "internal" view of the Runtime — the canonical model is the protocol.

### 3.2 The runtime/planner separation

The Runtime owns *mechanism*: sessions, runs, tasks, events, streaming, retries, pause/resume, artifacts, tool execution, memory injection, scheduling, provenance, guardrails. The Planner owns *policy*: reasoning, decision-making, next-action selection.

The contract is one interface:

```go
type Planner interface {
    Next(ctx context.Context, run RunContext) (Decision, error)
}
```

A `Decision` is a sum type (see §6.2). The Runtime executes the decision; the Planner never reaches into Runtime internals. Tools, memory, skills, artifacts, pause/resume, steering — all are reachable from any Planner via a `RunContext` view, never via direct package imports.

This decouples reasoning strategy from orchestration. The same Runtime executes a `react` planner today and a `deterministic`/`workflow`/`graph`/`supervisor` planner tomorrow. (Settled.)

### 3.3 The unified pause/resume primitive

A run can pause for many reasons that look distinct on the surface:

- **HITL approval** (a human needs to approve a planner-chosen tool call).
- **Tool-side OAuth** (a tool needs interactive auth).
- **A2A `AUTH_REQUIRED` / `INPUT_REQUIRED` task states** (the A2A spec's pause-equivalents).
- **Steering `PAUSE`** (operator/Console pauses a run).

These are **one primitive** at the Runtime level, exposed on the Protocol — not four parallel implementations. The Runtime owns the pause coordinator; planners and tools both *signal* "I need a pause" by returning `RequestPause` or emitting an authn request event; the Runtime drives the protocol-level event + resume token. Authentication on resume is checked against the original pause's identity scope.

This is the cleanest single-point-of-truth in the design and the strongest test of the swappable-planner property: a deterministic / workflow planner inherits pause/resume because it is a Runtime feature, not a planner feature. (Settled.)

### 3.4 The fail-loudly principle

Across the surface, Harbor refuses to silently degrade.

- Pause/resume serialization that encounters a non-serializable handle MUST return `ErrUnserializable` naming the offending field path. There is no "silently set to nil/None" path.
- Identity is mandatory. No `require_explicit_key=False` knob, no default-tenant fallback. Missing identity = fail closed + audit event.
- Capability detection ceremony is forbidden when all V1 drivers implement everything. One mandatory interface per subsystem; conformance test is the gate.
- Two parallel implementations of the same conceptual feature (`use_native_X=true|false`-style toggles) are a smell. Pick one and deepen it.

These are runtime-wide invariants, recorded in `AGENTS.md` §13 forbidden practices. (Settled.)

### 3.5 The concurrent reuse contract (D-025)

**Compiled artifacts are immutable after construction. Per-run state lives in `ctx` + `RunContext`, never on the artifact.** This is the cross-cutting principle that prevents the predecessor's most expensive retrofit: the first version of its flow runtime had thread-safety issues because mutable state on a single-instance "singleton" Flow bled across concurrent invocations once Python's threading model finally allowed parallel execution.

In Harbor every "compiled artifact" — `flow.Engine`, `Tool` (any transport), `Planner` instance, `MemoryStore` driver, `Redactor`, `LLMClient`, `ToolCatalog` — is built once, shared across N concurrent goroutines, and MUST satisfy four guarantees:

1. **No data races** — `go test -race ./...` is the gate; CI runs it.
2. **No context bleed** — run A's input/state never reaches run B; verified by per-run identity assertions in the test.
3. **No cancellation cross-talk** — cancelling run A's `ctx` MUST NOT affect run B; verified by parallel-cancel tests.
4. **No goroutine leaks** — each invocation's goroutines are joined before the invocation returns; baseline-restored test asserts this.

**Every phase that builds a reusable artifact ships a concurrent-reuse test** (N≥100 invocations against a single shared instance under `-race`). AGENTS.md §11 makes this mandatory; phase plan template's pre-merge checklist enforces it. Wave 1 phases 01 (Identity), 02 (Config), and 03 (Audit redactor) include this test from t=0; subsequent waves inherit the pattern.

**Why it matters at design time, not just at test time:** an artifact that needs mutable per-run state pushes the design to expose that state through `RunContext`, not stash it on the receiver. This shapes interface signatures, registry patterns, and lifecycle conventions across the runtime. Done from t=0, it is free; retrofitted, it requires rewriting every artifact's invocation path. The predecessor learned this. Harbor inherits the lesson.

---

## 4. Identity & isolation contract

### 4.1 The identity triple

Every Runtime context carries the triple `(tenant_id, user_id, session_id)`. This triple is the load-bearing isolation key for memory, events, artifacts, tasks, tools, skills, planner state, and audit. The session is the **innermost scope** and the **most active concurrency boundary**.

A user can be in **multiple concurrent sessions**. Those sessions must remain isolated: different memory scopes, different event subscriptions, different tool caches. This is non-negotiable. (Settled.)

```go
package identity

type Identity struct {
    TenantID  string
    UserID    string
    SessionID string
}

func From(ctx context.Context) (Identity, bool)
func MustFrom(ctx context.Context) Identity // panics if absent — handler-only
func With(ctx context.Context, id Identity) context.Context
```

### 4.2 Mandatory identity

Storage methods on `MemoryStore`, `StateStore`, `ArtifactStore`, `TaskRegistry`, `EventBus.Subscribe`, and the catalog filter require the full triple. Missing components fail closed: the operation returns an audit event (`identity.required`) and does not proceed.

Cross-session reads, cross-tenant queries, and admin observability require an explicit elevated scope claim on the Protocol caller (e.g. an `admin` JWT scope). Such requests are audited unconditionally. (Settled.)

### 4.3 Conformance gates

Every persistence-shaped subsystem ships a `conformance.RunSuite(t, factory)` that all drivers (in-mem, SQLite, Postgres) pass. The suite includes:

- Identity-mandatory tests: missing tenant/user/session components fail closed; the audit event is emitted.
- Cross-session no-leak: two concurrent sessions on the same store with different identity triples never observe each other's data.
- Cross-tenant no-leak: same, at the tenant boundary.
- Concurrency stress: 100 sessions × random ops for 30s under `-race`. Final invariant: every read's identity matches the caller's identity exactly.

Phase plans for any persistence-shaped subsystem must invoke this suite. PRs that add new code paths touching identity must include cross-session isolation tests. (Settled — `AGENTS.md` §11.)

---

## 5. Harbor Protocol

### 5.1 Decoupling rule

The Console NEVER reads internal Runtime objects. The Runtime emits the canonical model; the Console renders projections. (Settled — `AGENTS.md` §1, §8.)

Reject-on-sight violations:

- Console code that imports a Runtime-internal Go struct.
- Runtime that exposes an internal state shape via the Protocol "for now."
- A Protocol method that maps 1:1 to an internal Go function signature.
- Runtime that imports the Console package, in any direction.
- A "shortcut" debug endpoint that exposes raw internal state and is "only for dev."

### 5.2 What the Protocol exposes

| Surface | Description |
|---|---|
| **Streaming events** | The typed event bus from §6.13, server-filtered by identity. |
| **Task control** | `start`, `cancel`, `pause`, `resume`, `redirect`, `inject_context`, `approve`, `reject`, `prioritize`, `user_message` (the nine taxonomy entries from §6.3). |
| **State snapshots** | `sessions.inspect`, `tasks.get`, `state.history`, `state.list_trajectories`, `state.load_planner_checkpoint`. |
| **Topology** | `topology.snapshot` events; static graph + live queue depth. |
| **Artifacts** | `artifacts.list`, `artifacts.get`, `artifacts.get_ref`, `artifacts.delete` — all scope-checked. Heavy bytes always go by `ArtifactRef`, never inline. |
| **Traces / metrics** | `traceparent` propagation; OTel traces and metrics derived from the same event bus. |

### 5.3 Versioning

The Protocol version is pinned in `internal/protocol/types/version.go`. Bumping the version is an RFC change. Breaking changes require a deprecation window so third-party Consoles aren't whipsawed.

### 5.4 Wire transport

**Tentative — see §11 Q-1.** The Protocol surface is consumable from a browser, a TUI, an IDE extension, a third-party Console, and an observability vendor. Candidate transports:

- **gRPC server-streaming**: native streaming, language-mature, but TUI/browser ergonomics weak without grpc-web shim.
- **SSE + REST hybrid**: trivial browser support, simple to operate, no native multiplexing, half-duplex.
- **WebSocket + JSON-RPC**: full duplex, browser-native, schema discipline weaker without an external IDL.
- **NDJSON over chunked HTTP**: simplest to debug; weak multiplexing.

V1 must ship one. The current lean: **SSE for the event stream + REST/JSON for the control surface**, both server-enforced for identity. Rationale: lowest implementation cost, browser-native, matches the gateway sibling project's patterns, and the streaming-only direction (server→client) covers events; the request-response direction (client→server) covers control. WebSocket can be added as an alternate transport in a later phase if multiplexing or full-duplex becomes load-bearing.

The transport choice is settled in this RFC subject to Q-1; the relevant phase (Protocol-1) blocks until it resolves.

### 5.5 Authentication

JWT, asymmetric algorithms only (RS256/RS384/RS512/ES256/ES384/ES512). The triple `(tenant, user, session)` is in the JWT claims; the Protocol rejects any request without an identity scope. (Settled — `AGENTS.md` §7.) Extended scopes (`admin`, `console:fleet`) gate cross-session and cross-tenant subscriptions.

---

## 6. Runtime layer

The Runtime is the meat of V1. Each subsystem below is a settled architectural decision; sharp edges and open questions are explicit. Phase plan(s) for each subsystem are sized in `docs/plans/README.md` and the master plan that follows this RFC.

### 6.1 Core runtime

The Runtime is an async, queue-backed graph of `Node`s exchanging `Envelope`s along `Channel`s. It owns: the executor loop, channel semantics (bounded, drop-policy on backpressure), reliability shell (timeouts, retries, validation), streaming primitive, cancellation, subflows, routers, concurrency utilities (`MapConcurrent`, `JoinK`).

```go
package runtime

type Envelope struct {
    Payload    any
    Headers    Headers
    RunID      string      // active concurrency boundary
    SessionID  string
    Timestamp  time.Time
    DeadlineAt *time.Time  // wall-clock; checked before scheduling each node
    Meta       map[string]any
}

type Headers struct {
    TenantID string
    UserID   string
    Topic    string
    Priority int
}

type Engine interface {
    Emit(ctx context.Context, env Envelope, opts ...EmitOption) error
    EmitTo(ctx context.Context, env Envelope, target NodeRef) error
    Fetch(ctx context.Context, opts ...FetchOption) (Envelope, error)
    FetchByRun(ctx context.Context, runID string) (Envelope, error)
    Cancel(ctx context.Context, runID string) (bool, error)
    Stop(ctx context.Context) error
}
```

**Settled decisions:**

- Identity quadruple `(TenantID, UserID, SessionID, RunID)` flows through the Envelope. `RunID` is Harbor's term for what the predecessor called `trace_id`; Harbor reserves `TraceID` for OpenTelemetry-style traces (which may span multiple runs). (Resolves brief 01 Q-1.)
- `DeadlineAt` is wall-clock, not duration. Set once at the boundary.
- The egress fetch dispatcher is **always-on**. The dual-mode (pre-dispatcher direct fetch vs post-dispatcher per-run demux) the predecessor ships exists for backward compatibility Harbor doesn't owe to anyone.
- Per-run capacity backpressure is a Runtime primitive, not a bolt-on. Without it, parallel runs can deadlock through shared bounded channels under streaming load.
- Planner concerns do not leak into the Runtime: a deadline expiration emits `RunError(DeadlineExceeded)` to the egress; planners convert that to a final answer for the user. Working-memory hop dedup is not a Runtime concern.
- Bus publishing failures surface to the Protocol; never silently swallowed.

**Key data shapes** (settled in `docs/research/01-core-runtime.md`):

- `Node`, `NodePolicy` (timeout/retry/validate/backoff), `RunError` (structured), `StreamFrame` (per-stream `Seq`, terminal `Done`).
- Routers: `PredicateRouter`, `UnionRouter`, `RoutePolicy`.
- Concurrency: `MapConcurrent`, `JoinK`.
- Subflows: `Subflow(factory, parent, opts...)` runs a child engine with the parent's `RunID`, mirrors parent cancellation, returns the first egress payload.

**Validation strategy:** Go generics + JSON Schema at the protocol edge. Internal nodes are typed `Node[I, O]` so the compiler enforces shape; runtime validation handles wire-form ingress where types are dynamic. (Resolves brief 01 Q-3.)

**Default queue maxsize:** 64 per-channel default, per-engine override, per-channel override available. (Resolves brief 01 Q-4.)

**Error routing:** errors go to the Protocol unconditionally; egress emission (`emit_errors_to_rookery`-equivalent) is the optional path. (Resolves brief 01 Q-5.)

**Flow-as-Tool registration (Settled — see D-023).** A **Flow** is a typed DAG of `Node`s assembled into a runnable unit (the same machinery that powers subflows in §6.1) that can be **registered as a Tool** in the Tool catalog (§6.4). The planner sees one Tool with an args/result schema; invoking it runs the underlying DAG with the runtime's full reliability shell — `NodePolicy` per-node (timeout / retry / exponential backoff / validation) plus an aggregate `FlowBudget` enforced at flow boundaries.

```go
package flow

type Definition struct {
    Name        string                 // tool-name when registered
    Description string                 // surfaced to the planner
    Entry       NodeID                 // first node in the DAG
    Exit        NodeID                 // node whose output is the flow's result
    Nodes       map[NodeID]NodeSpec    // node → policy + edges
    Budget      Budget                 // optional intrinsic cap (see below)
    InSchema    json.RawMessage        // derived from Entry's input type
    OutSchema   json.RawMessage        // derived from Exit's output type
}

type Budget struct {
    Deadline   time.Duration   // wall-clock cap; 0 = inherit from parent run
    HopBudget  int             // max node hops; 0 = inherit
    CostCap    float64         // USD ceiling enforced via Governance counters; 0 = inherit
}

// Compose builds a runnable Engine from a Definition. The engine is reusable
// across invocations; each invocation gets its own RunID + RunContext.
func Compose(def Definition) (Engine, error)

// RegisterAsTool wires a composed Engine into the Tool catalog. Args/result
// schemas come from def.InSchema / def.OutSchema; Transport is FlowTransport.
// The planner cannot tell a Flow Tool from any other Tool — same one method,
// same dispatch path (RFC §6.4 "Code-level tool dispatch").
func RegisterAsTool(catalog tools.Catalog, def Definition, eng Engine) (tools.Tool, error)
```

**Resilience composition (Settled).** Per-node retry / backoff / timeout / validation come from `NodePolicy` (§6.1 "Key data shapes"). The `Backoff` math is exponential with jitter (`base * 2^attempt + jitter`, capped at `MaxBackoff`); per-node retries respect `MaxRetries`; per-node timeout produces `RunError(NodeTimeout)` and counts against retries. **Per-flow** caps come from `flow.Budget` and are enforced at the engine boundary: deadline = `min(flow.Budget.Deadline, parent_run.RemainingDeadline)`; hop budget = `min(flow.Budget.HopBudget, parent_run.RemainingHops)`; cost cap = `min(flow.Budget.CostCap, parent_run.RemainingCost)`. Exceeding any cap emits `flow.budget_exceeded` and aborts cleanly; the runtime returns a typed `ErrFlowBudgetExceeded` to the calling planner step. **Identity budgets (Governance §6.15) gate the LLM calls inside flow nodes** — the two budget systems compose: a flow can be aborted by either its intrinsic cap or the identity-tier ceiling, whichever fires first.

**Recipe format (declarative DAG authoring) — V1.1, deliberately deferred.** A *recipe* is a YAML/JSON-shaped file that describes a Flow `Definition` declaratively (nodes, policies, edges, budget) so operators can author flows without writing Go. V1 ships **Go-coded `Definition` registration** (operators write a small Go program that calls `flow.Compose(...)` and `flow.RegisterAsTool(...)`); recipes ship as **post-V1 phase 100** to keep V1 scope tight. The `Definition` shape is the contract; the recipe loader is just a parser into the same struct.

### 6.2 Planner interface, Trajectory, RunContext

```go
type Planner interface {
    Next(ctx context.Context, run RunContext) (Decision, error)
}

type RunContext struct {
    SessionID, RunID, TenantID, UserID string

    Query      string
    Goal       string             // current goal (may be redirected by control)
    LLMContext map[string]any     // visible-to-LLM context (memories etc.)
    ToolContext ToolContext       // tool-only handles; serialisable/handle-split
    Trajectory *Trajectory        // append-only execution log
    Hints      PlanningHints      // optional ordering/parallel limits

    Catalog   ToolCatalogView     // schemas only — never Descriptors
    Memory    MemoryView
    Skills    SkillLookup
    Artifacts ArtifactStore

    Control   ControlSignals      // accumulated steering observations
    Budget    Budget              // deadline, hop budget, cost cap
    Clock     func() time.Time
    Emit      func(events.Event)
}

type Decision interface{ isDecision() }

type CallTool      struct { Tool string; Args json.RawMessage; Reasoning string }
type CallParallel  struct { Branches []CallTool; Join *JoinSpec }
type SpawnTask     struct { Kind tasks.Kind; Spec tasks.Spec; GroupID string }
type AwaitTask     struct { TaskID tasks.TaskID }
type RequestPause  struct { Reason pauseresume.Reason; Payload map[string]any }
type Finish        struct { Reason FinishReason; Payload any; Metadata map[string]any }
```

**Settled decisions:**

- `Decision` is a sum type. Runtime opcodes (parallel, spawn, await, pause, finish) are *different shapes* from tool calls. The predecessor's "magic strings as `next_node`" pattern is rejected.
- `RunContext` is the *only* surface the planner sees. Planners do not import Runtime internals. The Runtime hands the planner a pre-filtered catalog (visibility already applied), a memory view (scoping already bound), a skills lookup, the artifact store, and `Control` signals.
- The reference `react` planner uses functional options for the small set of genuinely policy-shaped knobs. Token budget, hop budget, deadline, max_iters, schema mode, cost cap are **runtime-level run options**, not planner state. The predecessor's ~70-field, ~50-constructor-parameter planner class is the anti-pattern.
- Concurrency: planners are safe to use across runs; the Runtime serializes calls *within* a run. State keyed by `RunID` is the pattern.

**Trajectory:**

```go
type Trajectory struct {
    Query          string
    LLMContext     map[string]any
    ToolContext    ToolContext  // serialisable half only — see §6.3
    Steps          []TrajectoryStep
    Summary        *TrajectorySummary  // compaction artefact
    Sources        []Source
    Artifacts      map[string]ArtifactRef
    HintState      map[string]any
    SteeringInputs []SteeringInjection
    Background     map[string]BackgroundResult
    ResumeHint     *ResumeHint
}
```

`Trajectory.Serialize() ([]byte, error)` returns `(nil, ErrUnserializable{Field: "..."})` if any entry is non-JSON-encodable. There is no silent-drop path. (Settled — closes the predecessor's silent-context-loss bug.)

**Schema repair pipeline** lives in `internal/planner/repair/` and is reusable across concretes: salvage → schema repair → graceful failure → multi-action salvage. Configurable per-concrete (`arg_fill_enabled`, `repair_attempts`, `max_consecutive_arg_failures`). (Settled.)

### 6.3 Steering and the unified pause/resume primitive

Steering is a Runtime capability, surfaced over the Protocol. Planners observe `Control` signals; the Runtime owns the inbox.

**Control event taxonomy (nine types — Settled):**
`INJECT_CONTEXT`, `REDIRECT`, `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `USER_MESSAGE`.

**Pause reason taxonomy (four types — Settled):**
`approval_required`, `await_input`, `external_event`, `constraints_conflict`.

**Pause/resume primitive:**

```go
package pauseresume

type Pause struct {
    Token    Token            // opaque, runtime-issued
    Reason   Reason
    Payload  map[string]any   // sanitized; depth/size-bounded
    PausedAt time.Time
}

type Token string  // opaque to clients; runtime owns the encoding

type Coordinator interface {
    Request(ctx context.Context, req PauseRequest) (Pause, error)
    Resume(ctx context.Context, token Token, payload map[string]any) error
    Status(ctx context.Context, token Token) (Status, error)
}
```

**Tool-context split.** The predecessor's silent-context-loss bug is closed by splitting `ToolContext` into:

1. A **serializable** half: IDs, configs, plain values. Serializes via standard JSON.
2. A **non-serializable** half: live callbacks, loggers, sockets, file handles. Registered with the Runtime under a handle key; on resume the handle is re-attached from the Runtime's live registry by key. If the handle cannot be re-attached, resume FAILS with `ErrToolContextLost{Handle: "..."}` — never silently. (Settled.)

**Handle registry persistence.** V1: process-local. Resume must run in the same Runtime process. The seam for a distributed handle directory exists (the registry is an interface) but no production driver ships at V1. (Resolves brief 02 Q-4.)

**Steering authn/authz.** Per-event scopes. `CANCEL`, `APPROVE`, `REJECT`, `PAUSE`, `RESUME` require the originating user/admin scope. `INJECT_CONTEXT`, `USER_MESSAGE` accept the session-scoped user. `PRIORITIZE` requires admin. `REDIRECT` requires the user (the agent's owner). Cross-tenant steering requires admin. (Resolves brief 02 Q-3.)

**Steering payload bounds:** depth ≤ 6, ≤ 64 keys, ≤ 50 list items, ≤ 4096 chars per string, ≤ 16 KiB total. Enforced at the Protocol edge. (Settled.)

**Pause-state serialization format:** JSON with `format_version: 1`. Settled to align with the event bus (also JSON) and operational simplicity. (Resolves brief 02 Q-2.)

**`NoOp` decisions are not part of the `Planner` interface.** Wait-for-steering and trajectory-summarization are Runtime short-circuits. (Resolves brief 02 Q-5.)

### 6.4 Tool catalog and transports

The planner reasons about exactly one concept: a `Tool`. The catalog hides whether the tool is in-process Go, MCP, A2A, or HTTP.

```go
type Tool struct {
    Name        string
    Description string
    ArgsSchema  json.RawMessage  // JSON Schema (object)
    OutSchema   json.RawMessage
    SideEffects SideEffect
    Tags        []string
    AuthScopes  []string
    CostHint    string
    LatencyHint time.Duration
    SafetyNotes string
    Loading     LoadingMode  // Always | Deferred
    Examples    []ToolExample
    Source      ToolSourceID
    Transport   TransportKind  // InProcess | MCP | A2A | HTTP | Flow
    Policy      ToolPolicy     // resilience shell — see below
}

type ToolPolicy struct {
    TimeoutMS    int           // 0 = inherit from RunContext.Budget.Deadline
    MaxRetries   int           // 0 = no retry
    BackoffBase  time.Duration // exponential base; 0 = sensible default (100ms)
    BackoffMax   time.Duration // cap; 0 = sensible default (30s)
    RetryOn      []ErrorClass  // which RunError classes are retryable; default = transient/timeout/5xx
    Validate     ValidateMode  // both / in / out / none
}

type ToolDescriptor struct {
    Tool     Tool
    Invoke   func(ctx context.Context, args json.RawMessage, rc *RunContext) (ToolResult, error)
    Validate func(args json.RawMessage) error
}

type ToolCatalog interface {
    Register(d ToolDescriptor) error
    Resolve(name string) (ToolDescriptor, bool)
    List(filter CatalogFilter) []Tool
}

type CatalogFilter struct {
    TenantID, UserID, SessionID string
    GrantedScopes               []string
    LoadingModes                []LoadingMode
    NameRegex                   *regexp.Regexp
}

type ToolProvider interface {
    Connect(ctx context.Context, rc *RunContext) error
    Discover(ctx context.Context) ([]ToolDescriptor, error)
    Close(ctx context.Context) error
    SourceID() ToolSourceID
}
```

**Settled decisions:**

- The unification is at the **type level**: every `Tool` is the same struct regardless of source. The dispatch is one switch in one place.
- `CatalogFilter` keys on the full identity triple plus `GrantedScopes`. The predecessor filters by tenant only; Harbor goes further from t=0.
- Argument validation runs at the catalog edge; failures are typed `tool.invalid_args` events (not tool errors) so the planner can reformulate via LLM retry feedback.
- Result normalization is a layered pipeline (explicit field-extraction → typed-content blocks → heuristic binary detection → size-based safety net). The size-based safety net **mandates** routing through the `ArtifactStore`; there is no inline-large-payload escape.

**Reliability shell wraps EVERY tool invocation, regardless of transport (Settled — D-024).** The minimum-expression tool — a plain Go function registered via `tools.RegisterFunc(name, fn, opts...)` — gets the same reliability shell as a Flow tool: per-call timeout, exponential-backoff retry, validation, identity-aware cancellation. The runtime's Dispatcher (§6.4 trio) wraps every tool invocation in the `ToolPolicy` shell once, regardless of `Transport`. **The shell is identical to `NodePolicy` for runtime nodes (§6.1)** — same backoff math, same retry classes, same validation modes — so a developer who learned NodePolicy already knows ToolPolicy. Defaults fire when `ToolPolicy` is zero-valued so the most common case ("`@tool`-decorate this function") needs zero ceremony to be production-resilient.

```go
// Minimum-expression tool: a plain Go function registered with sensible defaults.
// Reliability shell (timeout, retry, backoff, validation) applies automatically.
catalog.RegisterFunc(
    "summarize",
    func(ctx context.Context, args SummarizeArgs) (SummaryResult, error) { ... },
)

// Same function with an opinionated policy:
catalog.RegisterFunc(
    "external-fetch",
    fetcher,
    tools.WithPolicy(tools.ToolPolicy{
        TimeoutMS:   5000,
        MaxRetries:  3,
        BackoffBase: 200 * time.Millisecond,
        RetryOn:     []ErrorClass{ErrTransient, ErrTimeout},
    }),
)
```

`tools.RegisterFunc` derives `ArgsSchema` and `OutSchema` from the Go signature via generics + reflection (no manual JSON-Schema authoring for the common case).

**Transports shipped at V1:**

- **InProcess** — tool authors register a Go function via generics + reflection (schemas derived from input/output types).
- **HTTP** — UTCP-style manifest, static auth (API key, bearer, cookie), retry, rate-limit handling.
- **MCP southbound** — Go MCP client driver (stdio + streamable-HTTP + SSE); auto-detect transport via `MCPTransportMode = Auto | SSE | StreamableHTTP`.
- **A2A southbound** — full A2A spec compliance from t=0. Agent Card discovery (`GET /.well-known/agent-card.json`), JSON-RPC `message/send`, `message/stream` (SSE), `tasks/get`, `tasks/cancel`, `tasks/pushNotificationConfig/*`. Registry with route scoring (trust tier, latency tier, capability match).
- **Flow** — a Harbor Flow (DAG, see §6.1 "Flow-as-Tool registration") registered as a Tool. The dispatcher invokes the underlying engine; the per-node `NodePolicy` (retry / exponential backoff / timeout / validation) and the aggregate `flow.Budget` (deadline / hops / cost cap) compose with identity-tier Governance ceilings. The planner sees a Flow Tool the same as any other Tool — one args/result contract, one dispatch path, one set of failure modes (`tool.invalid_args`, `tool.error`, plus `flow.budget_exceeded` mapped to `ErrFlowBudgetExceeded`).

**A2A northbound (V1 candidate — Tentative — see §11 Q-2).** Exposing Harbor as an A2A *server* (so other agents can call us) is a strong V1 candidate but adds protocol-server scope. Lean: defer to V1.1 unless an early adopter demands it.

**HTTP tool definitions:** both inline (Go code: `RegisterHTTPTool(name, method, urlTemplate, ...)`) and out-of-process via UTCP-style manifest. Inline is the dev-loop ergonomic; manifest is the operator deployment shape. (Resolves brief 03 Q-3.)

**Tool-side OAuth + HITL** uses the unified pause/resume primitive. The runtime emits `tool.auth_required` (auth URL, scopes, state), the Coordinator opens a pause record, the user completes OAuth out-of-band, the callback handler resumes the run with the token. The same primitive serves A2A's `TaskState.AUTH_REQUIRED`. (Settled.)

**Audit redaction** lives in the audit subsystem (a single redactor over the event stream) — the canonical record is the event payload, not the Go struct. Per-descriptor `Redact` hooks are not the model. (Resolves brief 03 Q-5.)

**Code-level tool dispatch (Settled — see brief 07).** Tool calling happens at the **runtime/orchestration level**, not at the **LLM provider level**. The LLM client emits text (and optional structured JSON); the runtime parses tool intents, validates them, dispatches them in parallel, and merges results back into the next LLM prompt. Provider differences disappear: parallel tool calling works uniformly across providers because Harbor — not the provider — owns the protocol. The runtime's dispatch trio:

1. **`ActionParser`** (`internal/runtime/planner/parser/`) — extracts a typed `PlannerAction` from raw LLM text. Owns multi-action discovery and the salvage path. Knows Harbor's `next_node` / `args` schema; deliberately knows nothing about OpenAI `tool_calls`, Anthropic `tool_use`, etc.
2. **`Dispatcher`** (`internal/runtime/dispatch/`) — single + parallel folded into one design unit. Validates `args` against the tool's input schema, runs with deadline + cancellation hooks, stamps synthetic call IDs (runtime-stamped, never model-emitted: `call_{action_seq}_{step_index}` for single, `call_{action_seq}_parallel_{branch_index}` for parallel), returns outcomes. One JSON action carries the entire parallel plan including its join spec — this is what makes parallel calling provider-independent.
3. **`ObservationRenderer`** (`internal/runtime/planner/observation/`) — turns a `(Trajectory, latest step)` into the next chat thread, interleaving assistant + user messages from `(action, observation|error|failure)` pairs and applying LLM-facing redaction (heavy outputs replaced with artifact refs).

Plus two siblings:

- **`RepairLoop`** drives `parser → validator → planner-prompt-on-failure` cycles up to `RepairAttempts`. Loud on exhaust; the regex finish-fallback is the documented last resort.
- **`SchemaSanitizer`** (`internal/llm/correction/`) lives **between** the runtime and the LLM client, NOT inside the client. Per-provider `response_format` adjustments live here; the single LLM client is dumb.

Synthetic call ID scope keys are the full `(session_id, run_id, action_seq, branch_index)`. The flatter scoping the source uses is a sharp edge Harbor closes.

### 6.5 LLM client layer

```go
type LLMClient interface {
    // One method. Streaming is signalled via opts.Stream + callbacks.
    // The runtime owns prompt construction, tool semantics, parsing, and parallel dispatch.
    Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
}

type CompleteRequest struct {
    Model          string
    Messages       []ChatMessage      // role + content only (system/user/assistant)
    ResponseFormat *ResponseFormat    // nil | json_object | json_schema(schema)
    Stream         bool
    OnContent      func(delta string, done bool)   // optional content delta callback
    OnReasoning    func(delta string, done bool)   // optional thinking-channel delta callback
    Temperature    *float32
    MaxTokens      *int
    Stops          []string
    ReasoningEffort string  // "off" | "low" | "medium" | "high" | ""
    Extra          map[string]any  // sanitized provider passthrough
    // No Tools, no ToolChoice, no FunctionCall.
}

type CompleteResponse struct {
    Content string
    Cost    Cost   // tokens in/out + dollars; runtime aggregates
    Usage   Usage  // tokens, latency, provider extras
}
```

**The client is one method.** No `Tools []ToolSpec`, no `ToolChoice`, no provider-specific tool-call shapes. Tool dispatch is the runtime's job (see §6.4 "Code-level tool dispatch"). This is the elegance principle: provider differences disappear because the runtime — not the provider — owns the protocol.

**Single architecture, no toggle.** A `use_native_llm=True/False` mode would ship two parallel implementations of the same conceptual feature. Harbor picks one architecture and bakes the per-provider correction layer in as a `SchemaSanitizer` plus message-shape normalization stack — both runtime utilities called *before* the client request, not flags on the client. (Settled — `AGENTS.md` §13.)

**Default driver: `bifrost` (`github.com/maximhq/bifrost/core`) — Settled — see brief 08.** A pure-Go LLM gateway library with first-class drivers for 23 providers (OpenAI, Anthropic, Google, Vertex, Bedrock, Azure, OpenRouter, XAI, Mistral, Ollama, Groq, Cohere, Cerebras, Fireworks, Perplexity, Replicate, ElevenLabs, HuggingFace, Nebius, Parasail, SGL, vLLM, Runway). Empirically validated on 2026-05-08 against six OpenRouter-routed models: 23 of 24 gating items pass (six models × four checks: basic chat, `json_object` response_format, streaming with content callback, ctx cancellation; plus token usage and cost reporting on every model). The one cancellation FAIL is a measurement artifact for long streams, not a functional defect — Harbor's runtime can abandon the channel reader on `ctx.Done()` without consequence. Adopting bifrost requires Go 1.26+ (matching its `go.mod`); Harbor's `go.mod` is bumped accordingly. The original CGo-required candidate is rejected.

Bifrost's `Tools` / `ToolChoice` parameters are intentionally NOT used — Harbor's runtime owns tool dispatch (see §6.4 "Code-level tool dispatch"). Bifrost is the LLM-call substrate; Harbor is the orchestration layer above it.

**Structured output strategies (Settled):** `OutputMode = Native | Tools | Prompted`. Per-provider `ModelProfile` selects the mode. Downgrade chain: `json_schema → json_object → text` on `invalid_json_schema` errors. Mode is observable via `llm.mode_downgraded` events. The `Tools` mode here is an LLM-level structured-output technique (asking the model to emit a single tool call shape as a workaround for providers without `json_schema`); it does NOT change the design — the runtime still parses and dispatches, the LLM client still emits text/JSON.

**Retry with feedback (Settled):** validation/parse failures feed back into the planner via the `RepairLoop`; observable; bounded by `RepairAttempts` per planner step.

**Multimodal inputs (V1, Settled — see D-021).** `CompleteRequest.Messages` carries multimodal content through `ChatMessage.Content`. The common case is text-only (`Content.Text != nil`); multimodal cases use `Content.Parts`:

```go
type ChatMessage struct {
    Role    Role
    Content Content
    Name    *string  // optional, for tool / participant naming
}

type Content struct {
    // Exactly one of Text or Parts is set. Text is the common case.
    Text  *string
    Parts []ContentPart
}

type PartType string

const (
    PartText  PartType = "text"
    PartImage PartType = "image"
    PartAudio PartType = "audio"
    PartFile  PartType = "file"
)

type ContentPart struct {
    Type  PartType
    Text  string      // when Type == PartText
    Image *ImagePart  // when Type == PartImage
    Audio *AudioPart  // when Type == PartAudio
    File  *FilePart   // when Type == PartFile
}

type ImagePart struct {
    // Exactly one of URL / DataURL / Artifact is set.
    URL      string         // remote URL the provider can fetch
    DataURL  string         // data:image/...;base64,...
    Artifact *artifacts.Ref // canonical Harbor reference (D-022)
    MIME     string         // image/jpeg, image/png, image/webp, ...
    Detail   string         // "low" | "high" | "auto" (provider hint)
}

type AudioPart struct {
    URL      string
    DataURL  string
    Artifact *artifacts.Ref
    MIME     string         // audio/mpeg, audio/wav, audio/ogg, ...
}

type FilePart struct {
    URL      string
    DataURL  string
    Artifact *artifacts.Ref
    MIME     string         // application/pdf, text/csv, ...
    Filename string         // hint shown to the model when the provider supports it
}
```

The `bifrost` driver translates Harbor's `ContentPart` to bifrost's per-provider content shape; bifrost handles the OpenAI / Anthropic / Gemini variations. **The `LLMClient` interface stays one method** — multimodal is just richer message content, not a new method, not a new request type.

**Canonical binary representation: `ArtifactRef` (D-022).** Of the three supply forms (URL, DataURL, Artifact), `ArtifactRef` is the *canonical* form for non-trivial binary content. Inline `DataURL` is convenient for small images but carries the bytes through every layer (events, audit, memory, persistence) — so it's bounded by the `heavy-output threshold` (32 KB default, RFC §6.10). Above the threshold, the runtime *automatically* materializes `DataURL` content into `ArtifactRef`s and rewrites the message before persistence and event emission. URLs pass through unchanged when the provider can fetch them.

**Multimodal outputs — post-V1 via tools (D-021).** Image generation, speech synthesis, transcription, and video editing/generation are delivered as Harbor **tools** that return `ArtifactRef`s. The planner emits a `tool.<name>` action; the runtime invokes the tool via the existing dispatcher (RFC §6.4); the tool wrapper internally calls bifrost's media APIs (which already cover all 23 providers' media surfaces — see brief 08 §"What `bifrost` provides"). The `LLMClient` itself never gains an output method beyond `Complete`. Phase 97 ships the media-input tool wrappers; phase 98 ships media-output wrappers. **The protocol and types settled here in V1 mean the post-V1 work is "implement tool wrappers," not "redesign."**

**Context-window safety net (Settled — D-026).** A runtime-wide invariant: **no message reaching the LLM carries raw heavy content.** The safety net is multi-stage; each producer respects the boundary, and a single enforcement pass at the LLM-client edge catches anything that slipped through.

*Stage 1 — at the producer:*

- **Tool results** above the heavy-output threshold (§6.10) are routed to the `ArtifactStore` by the Dispatcher; the planner sees an `ArtifactRef`, not bytes.
- **Memory turns** containing heavy content carry `ArtifactRef`s, not the original payload (§6.6).
- **Multimodal inputs** above the threshold are auto-materialized to `ArtifactRef` at `CompleteRequest` construction (D-022 above).
- **`ObservationRenderer`** (§6.4) replaces heavy observation outputs with `ArtifactStub`s when interleaving them into the next chat thread.

*Stage 2 — at the LLM-client edge (the catch-all):*
After the planner constructs `CompleteRequest` and before the driver (`bifrost`) ships it, a **single pass** of the runtime walks the messages and:

1. **Asserts no raw heavy content survived** — any string / byte slice / `DataURL` whose size ≥ threshold that *isn't* already an `ArtifactRef`-shaped stub is a bug; fail loudly with `ErrContextLeak` (and emit `llm.context_leak` audit event so operators can find the offending producer).
2. **Estimates total tokens** of the assembled request against the model's configured context limit. If the estimate is within `ContextWindowReserve` of the limit (default 5%), fail loudly with `ErrContextWindowExceeded`. V1 does not auto-truncate; the planner gets a typed error and is expected to recover (drop older turns, summarize, etc.) — auto-cascade is **post-V1** (an extension to memory's `rolling_summary` plus a `PromptAssembler` orchestrator; tracked but not on the V1 floor).

**The standard `ArtifactStub` (Settled).** When the runtime substitutes heavy content, the LLM sees a compact, model-agnostic stub:

```go
// In-prompt rendering (text-mode JSON, model-friendly):
//   {"artifact_ref":"ref-abc-def","mime":"image/png","size_bytes":65536,
//    "hash":"sha256:...","summary":"User-uploaded screenshot at turn 3",
//    "fetch":{"tool":"artifact.fetch","id":"ref-abc-def"}}
//
// Or in multimodal Parts: a text-only ContentPart whose body is the
// stub JSON above (the binary part is replaced wholesale).

type ArtifactStub struct {
    Ref       string  `json:"artifact_ref"`
    MIME      string  `json:"mime"`
    SizeBytes int64   `json:"size_bytes"`
    Hash      string  `json:"hash,omitempty"`     // sha256 prefix
    Summary   string  `json:"summary,omitempty"`  // operator/runtime caption
    Fetch     *Fetch  `json:"fetch,omitempty"`    // hint: "use this tool to read the bytes"
}

type Fetch struct {
    Tool string `json:"tool"` // e.g. "artifact.fetch_image"
    ID   string `json:"id"`   // ArtifactRef ID
}
```

The stub format is uniform across producers (tool result, memory turn, multimodal input). Operators can override `Summary` per-producer; the rest is runtime-stamped. **The stub is the only thing the LLM ever sees in place of heavy content** — operators do NOT swap formats per provider, because the rendered JSON works in every model's prompt.

**Multimodal interaction with adjacent subsystems (Settled — D-021):**

- **Audit redactor (§6.4):** recognizes `DataURL` and inline-base64 patterns; emits `[redacted: image/<MIME> of <N> bytes]` placeholders or rewrites to `ArtifactRef`. `ArtifactRef` itself passes through unredacted (it's already a reference, not data). Phase 03 handles this from t=0.
- **Memory (§6.6):** strategies handle multimodal turns. `truncation` drops them wholesale (the artifacts in the store are GC'd by the artifact subsystem's lifecycle, not memory). `rolling_summary` for V1 substitutes a `[image: <ArtifactRef>, MIME=<type>, size=<N>]` placeholder when summarizing; vision-aware summarization (calling a vision model to describe the image) is post-V1.
- **Tools (§6.4):** any tool can declare `ArtifactRef` in its `args` schema or `result` shape. The runtime resolves refs at invocation; the tool reads bytes via the `ArtifactStore`. No special "media tool" type — multimodal is a convention on top of the existing tool catalog.
- **Skills (§6.7):** Skills.md attachments already settled as `ArtifactRef`s (RFC §6.7); the same convention applies.

### 6.6 Memory subsystem

Memory is declared-policy, identity-scoped, and pluggable across persistence backends.

```go
package memory

type Strategy string
const (
    StrategyNone           Strategy = "none"
    StrategyTruncation     Strategy = "truncation"
    StrategyRollingSummary Strategy = "rolling_summary"
)

type Config struct {
    Strategy           Strategy
    Budget             Budget
    Isolation          IsolationPolicy   // RequireExplicitKey: true (mandatory)
    SummarizerModel    string
    IncludeTrajectory  bool
    RecoveryBacklogMax int
    RetryAttempts      int
    RetryBackoffBase   time.Duration
    DegradedRetryEvery time.Duration
}

type Store interface {
    AddTurn(ctx context.Context, id identity.Identity, turn ConversationTurn) error
    GetLLMContext(ctx context.Context, id identity.Identity) (LLMContextPatch, error)
    EstimateTokens(ctx context.Context, id identity.Identity) (int, error)
    Flush(ctx context.Context, id identity.Identity) error
    Health(ctx context.Context, id identity.Identity) (Health, error)
    Snapshot(ctx context.Context, id identity.Identity) (Snapshot, error)
    Restore(ctx context.Context, id identity.Identity, snap Snapshot) error
}
```

**Settled:**

- Three strategies: `none` (no-op), `truncation` (recent-window + budget enforcement), `rolling_summary` (background summarization, health states `healthy → retry → degraded → recovering → healthy`).
- Identity is **mandatory**. The predecessor's `require_explicit_key=False` knob is removed from Harbor. Missing identity = empty result + audit event. (Settled.)
- Three drivers ship at V1: in-memory, SQLite, Postgres. One conformance suite passes against all three.
- `llm_context` vs `tool_context` separation is preserved: identifiers live in `tool_context` (LLM-invisible); conversation state lives in `llm_context`. The Go analogue is "identity flows via `context.Context`, never through prompt-visible state."
- The summarizer is an injectable callable; the LLM call lives in the LLM-client subsystem; memory consumes a `Summarizer` interface.

**Memory budget at very long sessions — Tentative — see §11 Q-4.** `rolling_summary` covers hours; an *episodic memory* tier (durable summaries promoted from session to user scope) is post-V1 unless V1 user feedback demands it earlier.

### 6.7 Skills subsystem

Skills are a Runtime subsystem distinct from any external skill-distribution role. They are token-savvy, DB-backed, identity-scoped, and bring two Harbor-defining features:

1. **Skills.md importer** — first-class. Drop a Skills.md file/pack, get an indexed Harbor skill out the other side. The predecessor's per-skill-manual-adaptation gap is closed.
2. **In-runtime generator with persistence** — an agent can author a new skill that becomes a first-class Harbor skill discoverable by subsequent runs. The predecessor ships a draft generator with `"Do not claim to save or persist anything"` hardcoded into its prompt because the runtime cannot back the claim; Harbor inverts: runtime ships persistence, prompt is updated, audit is mandatory.

```go
type Skill struct {
    ID, Name, Title, Description string
    Trigger string  // non-empty; planner-visible match cue
    TaskType string // browser | api | code | domain | unknown
    Tags, Steps, Preconditions, FailureModes []string
    RequiredTools, RequiredNS, RequiredTags []string
    Origin Origin  // PackImport | Generated
    OriginRef string
    Scope Scope  // Project | Tenant | Global
    ScopeTenantID, ScopeProjectID string
    ContentHash string
    CreatedAt, UpdatedAt, LastUsed time.Time
    UseCount int
    Extra map[string]any
}

type SkillProvider interface {
    GetRelevant(ctx context.Context, q SkillQuery, cap CapabilityContext) (Retrieval, error)
    Search(ctx context.Context, q SkillSearchQuery, cap CapabilityContext) (SearchResponse, error)
    GetByName(ctx context.Context, names []string, cap CapabilityContext) ([]SkillDetail, error)
    List(ctx context.Context, req ListRequest, cap CapabilityContext) (ListResponse, error)
    Directory(ctx context.Context, cfg DirectoryConfig, cap CapabilityContext) ([]DirectoryEntry, error)
    FormatForInjection(skills []SkillDetail, maxTokens int) (text string, raw, final int, summarized bool, err error)
}
```

**Planner-facing tools (Settled):** `skill_search`, `skill_get`, `skill_list`, `skill_propose(persist=true)` — registered through the regular tool catalog like any other tool.

**Search ranking ladder:** FTS5 → regex → exact, scoring constants matching the predecessor's calibrated values. SQLite-FTS5 is conditionally available (`modernc.org/sqlite` build); the regex/exact fallback is tested with `FTS5=off` builds in CI. (Settled.)

**Capability filtering + redaction:** at injection time. Disallowed tool names are scrubbed from skill text; PII patterns redacted when `redact_pii=true`. Tiered budgeter: full → drop optional → cap steps to 3. (Settled.)

**Virtual-directory pattern (Settled):** `Directory(cfg)` returns identity-scoped, capability-filtered, pinned-then-{recent|top} entries. Up to `max_entries` (default 30, range 1–200).

**Skills.md importer pipeline (Settled):**

1. Parse YAML frontmatter + Markdown body via a deterministic CommonMark-only parser.
2. Normalize body sections (`## Steps`, `## Preconditions`, `## Failure modes`) into structured fields.
3. Resolve sibling resource files; record them as `Extra.attachments`.
4. Validate via the same `Skill` validator the operator loader uses.
5. Round-trip test: any spec-compliant Skills.md imports without source edits and re-exports byte-stable.

**Generator with persistence (Settled):** validates the draft, stamps `Origin=Generated`, stamps `OriginRef = "gen:{session_id}:{run_id}"`, scopes by operator-provided `Scope` (default `project`), inserts via the LocalDB upsert. Conflict policy: refuse to overwrite a `PackImport` skill of the same name; for `Generated → Generated`, last-write-wins gated by `ContentHash` change. Audit: `(actor=identity_triple, action="skill.created", skill_id, content_hash, source_excerpt_hash)`.

**Skill versioning model — Tentative — see §11 Q-5.** Content-hash-as-version + `OriginRef` for lineage at V1; explicit semver versions are a post-V1 follow-up if cross-tenant rolling forward demands it.

**Skills.md attachments — Settled.** Stored as `ArtifactRef`s via the artifact subsystem (option (b) in brief 04 Q-5). Clean separation, survives machine moves, integrates with mandatory-artifact policy.

**Conflict policy — Settled.** Refuse to import (Portico-distributed cannot overwrite Generated). `existing_origin != "pack"` short-circuit pattern. (Resolves brief 04 Q-2.)

**Generator scope default — Settled.** `project` scope by default when `skill_propose(persist=true)` is invoked mid-session. (Resolves brief 04 Q-4.)

### 6.8 Tasks (unified foreground/background)

```go
type TaskKind   string  // "foreground" | "background"
type TaskStatus string  // PENDING | RUNNING | PAUSED | COMPLETE | FAILED | CANCELLED

type Task struct {
    ID                TaskID
    SessionID         SessionID
    TenantID, UserID  string
    Kind              TaskKind
    Status            TaskStatus
    Priority          int
    ParentTaskID      *TaskID
    GroupID           *TaskGroupID
    Description       string
    Query             string
    Context           *TaskContextSnapshot
    Result            *TaskResult
    Error             *TaskError
    CreatedAt         time.Time
    UpdatedAt         time.Time
    PropagateOnCancel string  // "cascade" | "isolate"
    NotifyOnComplete  bool
    MergeStrategy     MergeStrategy
}

type TaskRegistry interface {
    Spawn       (ctx context.Context, req SpawnRequest) (TaskHandle, error)
    SpawnTool   (ctx context.Context, req SpawnToolRequest) (TaskHandle, error)
    Get         (ctx context.Context, id TaskID) (*Task, error)
    List        (ctx context.Context, sessionID SessionID, f TaskFilter) ([]TaskSummary, error)
    Cancel      (ctx context.Context, id TaskID, reason string) (bool, error)
    Prioritize  (ctx context.Context, id TaskID, priority int) (bool, error)
    // Group governance (lifted to a sibling interface in a later phase if needed):
    ResolveOrCreateGroup(ctx context.Context, req GroupRequest) (*TaskGroup, error)
    SealGroup           (ctx context.Context, id TaskGroupID) error
    CancelGroup         (ctx context.Context, id TaskGroupID, reason string, propagate bool) error
    ApplyGroup          (ctx context.Context, id TaskGroupID, action GroupAction) error
    ListGroups          (ctx context.Context, sessionID SessionID, status *TaskGroupStatus) ([]TaskGroup, error)
    ApplyPatch          (ctx context.Context, sessionID SessionID, patchID string, action PatchAction) (bool, error)
    AcknowledgeBackground(ctx context.Context, sessionID SessionID, ids []TaskID) (int, error)
}
```

**Settled:**

- **Foreground and background unify under one `TaskID` namespace.** A foreground run is a task of kind `foreground`. The predecessor splits `trace_id` (foreground) from a separate `task_id` namespace (background) and even fakes a synthetic `trace_id` like `session:<id>` to fit session updates into a trace-keyed audit log; Harbor's `TaskID` with `Kind` collapses that.
- **Lifecycle:** `PENDING → RUNNING → COMPLETE`, with `PAUSED → RUNNING` (planner-initiated, durable via planner checkpoint), `FAILED | CANCELLED` terminal.
- **Cancellation propagation** honors `PropagateOnCancel` (`cascade` | `isolate`).
- **Idempotency:** `Spawn` honors an `IdempotencyKey` per `(SessionID, IdempotencyKey)` so a retried spawn returns the original handle.
- **Background tasks at V1: in-process only.** The seam (`TaskRegistry` interface) is ready for a durable backend (Postgres-as-queue, NATS JetStream) post-V1.

**Retain-turn timeouts and continuation hops — Settled.** Per-session config (matching the predecessor's stance), with per-spawn override via `SpawnRequest`. (Resolves brief 05 Q-5.)

### 6.9 Sessions and SessionManager

A session is a longer-lived, multi-turn conversation that contains many runs. Identity for runtime concerns is the triple `(tenant, user, session)`; runs are scoped within sessions.

```go
type Session struct {
    ID        SessionID
    TenantID, UserID string
    OpenedAt  time.Time
    LastSeen  time.Time
    Closed    bool
    Limits    SessionLimits
    Context   SessionContext  // version, hash, llm/tool ctx, memory, artifacts
}

type SessionRegistry interface {
    Open    (ctx context.Context, id SessionID, ident identity.Identity) (*Session, error)
    Get     (ctx context.Context, id SessionID) (*Session, error)
    Touch   (ctx context.Context, id SessionID) error
    Close   (ctx context.Context, id SessionID, reason string) error
    Inspect (ctx context.Context, id SessionID) (*SessionSnapshot, error)
    GC      (ctx context.Context, policy GCPolicy) (int, error)
}
```

**Settled session-lifetime invariants:**

- A session is open until explicitly closed or GC'd.
- **Reopen-after-close is forbidden.** Clients open a new session.
- The identity triple is captured on `Open` and **immutable** for the session's lifetime; reusing a session ID across tenants/users is rejected.
- `Touch` updates `LastSeen`; GC sweeps idle sessions per policy and **never** reaps a session with a `RUNNING` task.

**Session GC defaults — Settled.** Idle TTL 24 h, hard cap 30 days, sweep every 15 min, refuse-to-GC any session with a `RUNNING` task. Configurable via `GCPolicy`. (Resolves brief 05 Q-2.)

### 6.10 Artifacts

```go
type ArtifactScope struct {
    TenantID, UserID, SessionID, TaskID string
}

type ArtifactRef struct {
    ID, MimeType string
    SizeBytes    int64
    Filename, SHA256 string
    Scope        ArtifactScope
    Namespace    string
    Source       map[string]any
}

type Store interface {
    PutBytes(ctx context.Context, data []byte, opts PutOpts) (ArtifactRef, error)
    PutText (ctx context.Context, text string, opts PutOpts) (ArtifactRef, error)
    Get     (ctx context.Context, id string) ([]byte, bool, error)
    GetRef  (ctx context.Context, id string) (*ArtifactRef, bool, error)
    Exists  (ctx context.Context, id string) (bool, error)
    Delete  (ctx context.Context, id string) (bool, error)
    List    (ctx context.Context, filter ArtifactScope) ([]ArtifactRef, error)
}
```

**Settled:**

- Heavy outputs MUST route through the ArtifactStore. There is no opt-in flag and no `NoOp` fallback. An in-memory driver is the floor; production drivers (filesystem, SQLite-blob, Postgres-blob, S3-style) ship as additional drivers behind the same interface.
- IDs are content-addressed: `{namespace}_{sha256[:12]}`. Re-uploading identical bytes returns the existing ref.
- Access goes through a `ScopedArtifacts` facade per task that auto-stamps the identity triple on writes and scope-checks on reads. Tools never see raw scopes.

**Heavy-output threshold — Settled at 32 KB default, runtime-configurable, per-tool overridable.** (Resolves brief 05 Q-1.)

### 6.11 StateStore

```go
// EventID is a ULID supplied by the caller; the store keys idempotency on it.
type EventID string

// StateRecord is the unit of persistence. Bytes is opaque to the store —
// callers serialize their domain types and run them through audit redaction
// upstream of Save (the store does not redact).
type StateRecord struct {
    ID         EventID
    Identity   identity.Quadruple
    Kind       string    // caller-namespaced, e.g. "session.lifecycle", "task.checkpoint"
    Version    int       // optimistic-concurrency hint for typed wrappers
    Bytes      []byte    // pre-redacted, caller-serialized payload
    UpdatedAt  time.Time
}

type StateStore interface {
    Save(ctx context.Context, r StateRecord) error                                    // idempotent on EventID; ErrIdempotencyConflict on same-ID-different-bytes
    Load(ctx context.Context, id identity.Quadruple, kind string) (StateRecord, error)
    LoadByEventID(ctx context.Context, eventID EventID) (StateRecord, error)
    Delete(ctx context.Context, id identity.Quadruple, kind string) error
    Close(ctx context.Context) error
}
```

**Settled (revised — D-027):**

- **Generic key-value-of-typed-bytes surface.** `StateStore` is a five-method interface keyed on `(identity.Quadruple, Kind string, Bytes []byte)` with idempotency on a caller-provided `EventID` (ULID). Consuming subsystems (sessions, tasks, planner checkpoints, memory snapshots, steering events, distributed bindings, trajectories) land their **typed wrappers at their own layer** atop this surface — not inside `internal/state`. Example: `SessionRegistry.Save(s Session)` reduces to `StateStore.Save(StateRecord{Identity: s.Identity, Kind: "session.lifecycle", Bytes: marshal(s)})`. This keeps `internal/state` a leaf with no upstream Harbor deps beyond `internal/identity` and `internal/config`.
- One mandatory interface, three V1 drivers (in-memory, SQLite, Postgres), one conformance suite. The predecessor's eight optional `Supports*` capability protocols + `hasattr` duck-typing are explicitly rejected — if all V1 drivers implement everything, optional capabilities are ceremony.
- Forward-only migrations, per-driver migration directories. Each migration ends with `INSERT OR IGNORE INTO schema_migrations(version) VALUES (N);` (or driver equivalent).
- WAL journal mode for SQLite.
- Idempotency: `Save` keys on `EventID`; same-ID + same-bytes is a no-op, same-ID + different-bytes returns `ErrIdempotencyConflict` (caller-controlled retry semantics — the store never silently overwrites).
- Identity-mandatory at the API boundary: empty tenant / user / session in the `Quadruple` rejected with `ErrIdentityRequired`. Empty `RunID` is acceptable for session-scoped state.
- Audit redaction is **upstream** of `Save`. The store stores opaque bytes; mixing redaction into the persistence layer would couple a leaf package to the audit subsystem and split responsibility (D-020).

**Earlier typed sketch (superseded by D-027 — kept for history):** an earlier draft listed 21 typed methods (`SaveTask`, `SaveTrajectory`, `SaveBinding`, `SaveSteering`, `SaveMemoryState`, etc.) keyed on domain types from unshipped phases. That shape would have inverted the dependency graph (a leaf persistence interface importing types from its consumers); the generic surface is strictly more general and lets each consumer ship its typed adapter at the right layer.

**Build-tag strategy — Settled.** Both SQLite and Postgres drivers ship in the default binary; operators choose at config time. Distros that need a smaller binary use build tags to drop one. (Resolves brief 05 Q-3.)

### 6.12 Distributed contracts (V1: contracts only)

```go
type BusEnvelope struct {
    Edge, Source, Target string
    TaskID    TaskID
    Payload   json.RawMessage
    Headers   map[string]any
    Meta      map[string]any
}

type MessageBus interface {
    Publish(ctx context.Context, env BusEnvelope) error  // at-least-once
}

type RemoteTransport interface {
    Send  (ctx context.Context, req RemoteCallRequest) (RemoteCallResult, error)
    Stream(ctx context.Context, req RemoteCallRequest) (RemoteEventStream, error)
    GetTask  (ctx context.Context, taskID, contextID string) (*RemoteTaskSnapshot, error)
    Subscribe(ctx context.Context, taskID, contextID string) (RemoteTaskEventStream, error)
    Cancel   (ctx context.Context, taskID, contextID string) error
}
```

**Settled:**

- V1 ships the interfaces, an in-process `MessageBus` (loopback), and a `RemoteTransport` capable of speaking A2A to remote agents.
- No durable distributed bus driver (NATS, Redis Streams, Postgres-as-queue) at V1. Post-V1 phases (`Distributed-2`, `Distributed-3`, …) add those.
- Delivery semantics: `MessageBus.Publish` is at-least-once; handlers must be idempotent on `(TaskID, Edge, EventID)`. `RemoteTransport.Send` is request/reply; `Stream` yields ordered events with a final `done=true`. (Resolves brief 05 Q-4.)

### 6.13 Typed event bus

The event bus is the **canonical projection of runtime state**. One bus, protocol-grade. Used both for live UI streaming and for telemetry — logging and OpenTelemetry derive from the same events rather than being parallel paths.

```go
package events

// EventType is a string-typed exhaustive enum. Each canonical type
// is declared as an exported constant + registered in init() so the
// registry stays the single source of truth.
type EventType string

// EventPayload is sealed via an unexported method on Sealed (an
// embedded struct any caller can compose into its concrete payload
// type). Bus-internal payloads compose SafeSealed instead, marking
// them as SafePayload — the bus skips the audit redactor for these
// (no secrets by construction; preserves typed access on the
// subscriber side). External payloads default to NOT-SafePayload;
// the bus runs their value through audit.Redactor and the
// subscriber-side payload becomes a RedactedMap when the redactor
// reflects a struct into a map.
type EventPayload interface {
    isEventPayload()
}
type Sealed struct{}
type SafePayload interface {
    EventPayload
    isSafePayload()
}
type SafeSealed struct{ Sealed }
type RedactedMap struct {
    Sealed
    Data map[string]any
}

type Event struct {
    Type       EventType
    Identity   identity.Quadruple // tenant + user + session + run, mandatory triple
    OccurredAt time.Time          // assigned by Publish when zero
    Sequence   uint64             // monotonic per-bus, gap-free; assigned by Publish
    Payload    EventPayload
    Extra      map[string]string  // bounded, low-cardinality; reserved for Phase 56 metric labels
}

type Filter struct {
    Tenant, User, Session string
    Types                 []EventType
    Admin                 bool
}

type EventBus interface {
    Publish(ctx context.Context, ev Event) error
    Subscribe(ctx context.Context, f Filter) (Subscription, error)
    Close(ctx context.Context) error
}
```

**Settled:**

- One bus, not two. The predecessor's split of telemetry vs chunked-output channels is unified on this single typed bus from t=0.
- `EventBus` (the Go-level name shipped as `internal/events.EventBus`) ships with `Publish` / `Subscribe` / `Close`. The `Replay(ctx, Cursor, Filter)` method is a separate concern and lives in Phase 06's replay-equipped driver — when that driver lands, callers will type-assert the returned `EventBus` to a `Replayer` capability interface, keeping the core surface lean.
- Drop policy on backpressure: drop-oldest, with a `bus.dropped` event describing the dropped sequence range. Notices are windowed at most once per `DropWindow` per subscriber.
- Server-enforced isolation filter: `Subscribe` rejects empty-triple non-admin filters with `ErrIdentityScopeRequired`. Every `Admin: true` Subscribe additionally emits an `audit.admin_scope_used` event so abuse is retroactively detectable. Cryptographic verification of the admin claim is wired in Phase 61 (Protocol auth); Phase 05 trusts the boolean.
- **Audit-before-emit boundary.** Every `Publish` runs the payload through `audit.Redactor` before enqueueing — except for `SafePayload`-marked types, which bypass the redactor (their declarer guarantees no secret-shaped fields; preserves typed access for bus-internal events and well-known metadata). On redaction failure: the bus emits a sibling `audit.redaction_failed` event (with NO original payload bytes) AND returns the wrapped error to the caller. The original event is NOT enqueued (D-020).
- Identity-mandatory: `Publish` rejects events whose Quadruple lacks tenant/user/session with `ErrIdentityRequired`. Empty `RunID` is acceptable for session-scoped events.
- Sequence numbering: per-bus monotonic via `atomic.Uint64`; gap-free. Caller-prefilled `Sequence != 0` is rejected with `ErrSequenceProvided`.
- Replay-from-cursor: ring buffer (default 10k events) when no durable log; exact replay when the durable log driver (StateStore-backed, Phase 57) is configured. Replay capability lives in Phase 06.
- Cardinality safety: future metric derivation (Phase 56) will draw labels from `Event.Type` and `Event.Extra` only — never `RunID` or `TraceID`. A static lint check enforces this in CI; the script ships as a Phase 05 stub at `scripts/check-event-cardinality.sh` and tightens in Phase 56.

**Event taxonomy** is Settled and lives in `internal/events/events.go`. V1 starter set: `runtime.error`, `runtime.warning`, `bus.dropped`, `bus.subscription_idle_closed`, `audit.redaction_failed`, `audit.admin_scope_used`, `governance.budget_exceeded`, `governance.rate_limited`. Adding new types is at-the-seam: declare an exported constant and register it in `init()`. The `TestEventTypes_Exhaustiveness` smoke gate runs in preflight.

**Default subscription filters in `harbor dev`:** `(tenant, user, session)` of the active run by default. Multi-run debugging requires an explicit operator opt-in. (Resolves brief 06 Q-3.)

**Schema versioning — Settled.** Best-effort additive: new `EventType`s and new optional fields are non-breaking. Strict semver for the bus-wire schema once third-party Consoles exist (V1.5+). (Resolves brief 06 Q-4.)

**Earlier sketch (superseded by D-028 — kept for history):** an earlier draft of §6.13 carried flat identity fields (`TenantID, UserID, SessionID, RunID`) plus `EmittedAt`, plus optional metric-shaped fields (`LatencyMs *float64`, `TokensIn *uint32`, `TokensOut *uint32`, `CostUSD *float64`, `QueueDepth *QueueDepthSnapshot`), and called the bus interface `Bus`. The shipped surface uses `identity.Quadruple` (re-using Phase 01's type), `OccurredAt`, no inline metric fields (Phase 56 derives labels from `Extra`), and renamed `Bus` → `EventBus`. The earlier draft also ranged the bus interface over `Replay` directly; replay is now a Phase 06 capability layer. D-028 captures the reconciliation.

### 6.14 Telemetry

Slog + OpenTelemetry from t=0. The Runtime emits events; the events drive both slog records (via the `Logger` wrapper) and OTel spans/metrics (via `Tracer` and `MetricsRegistry`). No retrofit.

**Settled:**

- One logger: `log/slog`. JSON in production, text in dev. No toggle inside the library; the slog handler is selected at process start.
- Standard attribute set on every logger: `tenant_id`, `user_id`, `session_id`, `run_id`, `task_id`, `trace_id`, `span_id`, `tool` (when present).
- `Logger.Error` emits both an slog record AND a paired `runtime.error` bus event so logs always have an event peer. (Settled.)
- OTel propagation: `traceparent` for HTTP southbound; `_meta.traceparent` for stdio MCP per-request; `HARBOR_TRACEPARENT` env var on stdio spawn for the initial trace.
- Metrics exporter: OTLP default. A built-in Prometheus `/metrics` endpoint ships at V1 for self-hosted setups (popular operator preference). (Resolves brief 06 Q-2.)

---

### 6.15 Governance subsystem

Governance is Harbor's middleware between the Runtime and the `LLMClient` driver. It owns identity-scoped policies — cost accumulators + ceilings, rate limits, per-call token budgets, and (post-V1) key rotation, model swap, failover chains, circuit breakers — that the LLM-call substrate (bifrost) doesn't and shouldn't know about, because it doesn't know Harbor's identity triple.

```go
package governance

type Subsystem interface {
    // PreCall is invoked before each LLMClient.Complete.
    // Returns a typed sentinel error to gate the call:
    //   ErrBudgetExceeded, ErrRateLimited, ErrMaxTokensExceeded, ErrKeyUnavailable.
    // Returning an error fails loudly; the runtime emits the corresponding event and
    // can route to the unified pause/resume primitive when configured.
    PreCall(ctx context.Context, ident Identity, req llm.CompleteRequest) error

    // PostCall is invoked after each LLMClient.Complete (success or failure).
    // Accumulates cost / tokens / latency; emits events; updates rate-bucket state;
    // drives circuit-breaker bookkeeping (post-V1).
    PostCall(ctx context.Context, ident Identity, req llm.CompleteRequest, resp llm.CompleteResponse, err error) error
}

type Identity struct {
    TenantID, UserID, SessionID, RunID string
    Tier                               string  // "free" | "team" | "enterprise" | custom
}

// Policy interfaces (each lives behind the §4.4 seam pattern with multiple drivers):
type CostPolicy   interface { /* check + accumulate budgets */ }
type RatePolicy   interface { /* token-bucket + bookkeeping */ }
type KeyResolver  interface { /* per-call key selection (wraps bifrost.KeySelector) */ }
type ModelOverride interface { /* mid-session model swap (post-V1) */ }
type FailoverPolicy interface { /* orchestrated provider chain (post-V1) */ }
type CircuitBreaker interface { /* per-(provider, key) health (post-V1) */ }
```

**What bifrost gives us free** (just by using it as library):

- Multi-key load balancing per provider (`Key.Weight`).
- Per-key model whitelist / blacklist (`Key.Models`, `Key.BlacklistedModels`).
- Per-request `KeySelector` hook — Harbor's identity triple flows here via `ctx`.
- `Bifrost.ReloadConfig(...)` for non-realtime config swap.
- `Account.GetKeysForProvider(ctx, provider)` invoked per request — keys can change without `ReloadConfig`.
- Cost reporting passthrough (`Usage.Cost.{TotalCost, InputTokensCost, OutputTokensCost, ReasoningTokensCost, ...}`).
- Connection pooling + drop-excess-requests backpressure.
- `LLMPlugin` / `MCPPlugin` pre/post hook architecture (available; intentionally NOT used for identity-scoped policies — see boundary note below).

**V1 scope (Settled).** See master plan phases 36a + 36b.

1. **Cost accumulator**, identity-scoped. Aggregates `Usage.Cost.TotalCost` per `(tenant, user, session)` and per model. StateStore-backed (in-mem / SQLite / Postgres conformance).
2. **Per-identity cost ceilings.** PreCall checks; emits `governance.budget_exceeded` event; fails loudly with `ErrBudgetExceeded`.
3. **Per-identity rate limits.** Token bucket per `(identity, model)`. PreCall checks; emits `governance.rate_limited`; fails with `ErrRateLimited`.
4. **Per-call MaxTokens per identity tier.** PreCall enforces a configured ceiling before the request goes out.
5. **Live events on the bus.** `llm.cost.recorded`, `llm.tokens.recorded`, `governance.budget_*`, `governance.ratelimit_*`. Console subscribes via Protocol once Console lands.

**Post-V1 (deliberately tracked — see master plan phases 91–96).**

| Phase | Capability | Why post-V1 |
|------:|------------|-------------|
| 91 | Console-driven key rotation (Protocol `governance.rotate_key`) | Operator workflow; needs Console to land first |
| 92 | Console-driven mid-session model swap (Protocol `governance.swap_model`) | Operator workflow |
| 93 | Failover chains as Harbor policy | Has policy + audit implications best done with Console visibility |
| 94 | Provider circuit breakers per `(provider, key)` | Cleaner once we have failover |
| 95 | LLM cache (exact-match + semantic) | Big complexity; not a V1 floor item |
| 96 | PII redaction at the LLM boundary | Audit subsystem owns the redactor; post-V1 |

**Boundary with adjacent subsystems.**

- **LLM client (§6.5):** Governance wraps the `LLMClient` interface. The LLMClient stays one method; the bifrost driver underneath is unaware of identity scopes.
- **Audit:** Governance emits events; Audit redacts and persists. **Audit owns PII redaction at the LLM boundary; Governance owns thresholds.** (Settled — D-020.)
- **Pause/resume (§6.3):** A `BudgetExceeded` or `RateLimited` event can trigger a pause via the unified pause/resume primitive, surfacing in Console as a steering event with `INJECT_CONTEXT` ("you're at budget — pause for operator approval").
- **Bifrost layer:** Governance does NOT use bifrost's `LLMPlugin` architecture for identity-scoped logic — that would couple Harbor's governance to bifrost's plugin lifecycle and hide it from Harbor's audit + event bus. Bifrost plugins remain available for low-level transforms (provider-quirk normalization that doesn't depend on identity).
- **Failover (post-V1):** Harbor orchestrates failover at the Governance layer; it does NOT push a per-call `Fallbacks` array into bifrost. Each fallback hop is a Harbor event with cost + identity attached. (Settled — D-018.)

**Key rotation (post-V1, Settled mechanism).** Console pushes a new key value via Protocol → Harbor's `Account` impl swaps keys atomically (`atomic.Pointer` over the live key set) → bifrost picks it up on the next call via `Account.GetKeysForProvider(ctx, ...)`. **No `ReloadConfig` race.** Old keys are invalidated immediately. (Settled — D-019.)

**Persistence.** Governance accumulators (cost, tokens, rate-bucket state) live in StateStore (in-mem / SQLite / Postgres drivers). Forward-only migrations per §9. Conformance test asserts identical behavior across backends. Cross-session isolation test asserts one session's accumulator doesn't bleed into another.

**Hot-reloadable fields (operator-facing).** Ceilings, rate limits, MaxTokens tiers, key set. Other Governance config remains restart-required per §10.

---

## 7. Console layer

The Console is **its own product, in its own repository.** It is a SvelteKit + adapter-static SPA that talks to the Runtime exclusively over the Harbor Protocol. (Settled — `AGENTS.md` §4.5.)

**Scope of Console (V1):**

- Live event stream view per session/run, with filter/search.
- Run timeline with planner steps, tool calls, LLM calls, costs.
- Task list and control (cancel/pause/resume/prioritize/approve/reject).
- Session list with identity scope.
- Artifact browser (read-only listing, download by ref).
- State inspector (planner checkpoints, trajectories — read-only, redacted).
- Topology visualization (node graph, queue depth).

**Out of scope (V1):**

- Authoring agents in the Console (the dev-loop scaffolding lives in `harbor dev` + CLI, with the Console as the inspector — not the editor).
- Hosting the Console in the Harbor Runtime binary. (Even when `harbor dev` boots a local Console, the Console is spawned as a separate static-file server or embedded via a thin static-file handler that talks to the Runtime via the Protocol — not via direct package imports.)

The Console repo and its phase plans land in a separate sequence. Some Console-related phases live in this repo (Protocol surface evolution, e2e Playwright tests against `harbor dev`); the Console itself does not.

---

## 8. CLI layer

The Harbor CLI is a single binary `harbor` with subcommands. (Settled.)

```text
harbor dev               Boot local Runtime + embedded Console + hot reload + draft-save scaffolding
harbor scaffold          Generate a new agent skeleton from a template
harbor validate          Validate config / skills / agent definitions without booting
harbor inspect-events    Tail or filter the event bus of a running Runtime
harbor inspect-runs      List recent runs; show a run's trajectory
harbor inspect-topology  Render a run's node graph as ASCII
harbor version           Print version, build hash, supported Protocol version
```

**Settled:**

- All subcommands are Protocol clients of the Runtime; they use the same client SDK a third-party tool would.
- `harbor dev` boots the Runtime headless on `127.0.0.1:<port>`, opens the Protocol, starts the embedded Console, watches the project directory for changes, hot-reloads on Go-source changes (graceful-stop in-flight runs first; configurable), and exposes a draft-save scratchpad endpoint for dynamic agent scaffolding.
- The dynamic scaffolding flow: a developer iterates on an agent in the dev loop, saves drafts (project-local `.harbor/drafts/`), and only commits to a final scaffold when satisfied.
- `deploy` and `package` subcommands are NOT V1. They land with Harbor Cloud's shape. (Resolves brief 06 Q-5.)

CLI subcommand additions are an RFC update, not a casual change.

---

## 9. Persistence triad

V1 ships **three** drivers behind every persistence-shaped interface (`StateStore`, `ArtifactStore`, `MemoryStore`, `SkillStore`):

1. **In-memory** — zero dependencies; default for embedded use, dev, tests.
2. **SQLite** — `modernc.org/sqlite` (CGo-free); single-binary deployments.
3. **Postgres** — `pgx`; multi-node production.

All three pass the same conformance suite. Designing the interface against three backends from t=0 forces clean abstractions; designing against one tends to leak that backend's assumptions into the contract.

**Settled:**

- One mandatory interface per subsystem. No optional `Supports*` ceremony.
- Forward-only, per-driver migrations.
- WAL journal mode for SQLite.
- Both SQLite and Postgres drivers ship in the default binary; operators choose at config time.
- Conformance test approach: `conformance.RunSuite(t, factory)` driven against any factory; CI runs all three drivers.

**Cross-driver tests** are mandatory. A new optional capability is a new method on the interface plus a new conformance scenario — no per-driver hand-waving.

---

## 10. Stack decisions

| Area | Decision | Status |
|---|---|---|
| Language | Go 1.26+ | Settled |
| Module path | `github.com/hurtener/Harbor` | Settled |
| License | **Apache-2.0** (MIT acceptable; see License subsection) | Settled |
| Build | `CGO_ENABLED=0`, static binary, `-ldflags='-s -w'` | Settled |
| SQLite | `modernc.org/sqlite` (CGo-free) | Settled |
| Postgres | `pgx` | Settled |
| LLM client | `github.com/maximhq/bifrost/core` (pure Go) wrapped behind `LLMClient` interface (one method, code-level tool dispatch in runtime); `SchemaSanitizer` between runtime and client | Settled — see Q-3 + brief 08 |
| Logger | `log/slog` (JSON prod, text dev) | Settled |
| Tracing | OpenTelemetry SDK | Settled |
| Metrics | OTel + built-in Prometheus `/metrics` | Settled |
| HTTP | stdlib `net/http` | Settled |
| JSON | stdlib `encoding/json` (consider `goccy/go-json` if perf-bound; not V1) | Settled |
| JSON Schema | `santhosh-tekuri/jsonschema` | Settled |
| ULID | `oklog/ulid` | Settled |
| YAML | `goccy/go-yaml` | Settled |
| CLI | `cobra` | Settled |
| Console | SvelteKit + adapter-static + Skeleton | Settled |
| Protocol wire | SSE + REST (event stream + control surface) | Tentative — see Q-1 |

Additions to this surface require an RFC PR (see `AGENTS.md` §13).

### License (Settled — Apache-2.0)

Harbor is published under **Apache License 2.0**. The full text lives in `/LICENSE` at the repo root.

**Rationale.** Two permissive open-source licenses were considered: MIT and Apache-2.0. Both are "open" in the OSI sense and broadly compatible with the dependency surface (bifrost, modernc/sqlite, pgx, all stdlib-equivalent transitive deps). The choice is Apache-2.0 because:

1. **Patent grant.** Apache-2.0 §3 includes an explicit, irrevocable patent license from contributors to users. For a runtime that companies will build agents on top of — and contribute back to — a clean patent grant materially reduces adoption friction. MIT is silent on patents; that silence is fine for small libraries but creates ambiguity for infrastructure.
2. **Notice and attribution discipline.** Apache-2.0 §4(d)'s NOTICE-file mechanism makes attribution requirements explicit and machine-readable, which fits the "many third-party drivers, many providers" surface Harbor will accumulate.
3. **Positioning consistency.** Harbor frames itself as infrastructure-grade ("Kubernetes for agents"). Apache-2.0 is the dominant license in that neighborhood: Go itself, Kubernetes, Docker, Terraform, OpenTelemetry, gRPC, Containerd, Bifrost (our LLM client), Cobra (our CLI). MIT is more common for libraries-as-libraries (Gin, Fasthttp, Chi); Apache-2.0 is more common for platforms-and-runtimes.

**MIT remains acceptable.** If the maintainer prefers MIT (lighter, fewer obligations on contributors, matches some sibling projects), the flip is mechanical: replace `/LICENSE`, update this RFC entry, update the stack table row, update `README.md`. No code changes needed because Harbor's dependencies are MIT-or-Apache-compatible either way. This is recorded so a future re-read knows MIT was a real alternate, not an oversight.

**License compatibility with dependencies.** Bifrost (`github.com/maximhq/bifrost/core`) is the only non-stdlib LLM-related dependency at V1; its license must be Apache-2.0 or MIT-compatible — to be verified in the Phase 33 PR by reading its `LICENSE` file at the pinned version. (Sanity check: large-org Go projects with similar ancestry are universally one of these.)

**Contributor License Agreement (CLA): not used.** Apache-2.0's §5 ("Submission of Contributions") establishes the contribution license inbound by default. Harbor does not require a separate CLA for V1. If commercial contribution patterns later require one, that is a separate RFC.

---

## 11. Open questions

These must be resolved before the relevant phase ships. Each Q-N is referenced inline in §5/§6/§10 above.

- **Q-1 (Protocol wire transport).** SSE + REST is the lean, but WebSocket + JSON-RPC and gRPC server-streaming remain viable. Decide before Protocol-1 phase ships. *Owner:* hurtener.
- **Q-2 (A2A northbound at V1).** Is exposing Harbor as an A2A *server* in V1 scope, or V1.1? Lean: V1.1 unless an early adopter demands it. *Owner:* hurtener.
- **Q-3 (LLM client choice) — RESOLVED (2026-05-08).** The original CGo-required candidate is rejected (conflict with `AGENTS.md` §5/§13). Replacement: `github.com/maximhq/bifrost/core` — pure Go, 23 first-class providers, empirically validated against six OpenRouter-routed models (23 of 24 gating items pass; the lone non-pass is a cancellation-timing measurement artifact, not a defect). Validation harness and full results in `docs/research/08-llm-client-validation.md`. The L-2 phase is no longer a decision gate; it is a normal implementation phase.
- **Q-4 (Episodic memory tier).** Is a durable summaries-promoted-to-user-scope tier a V1 feature or post-V1? Lean: post-V1 unless V1 user feedback demands otherwise.
- **Q-5 (Skill versioning model).** Content-hash-as-version + `OriginRef` at V1; explicit semver versions at V1.5 if cross-tenant rolling-forward demands. *Owner:* hurtener.
- **Q-6 (Second V1 planner concrete).** Settled here as `deterministic` (smallest concrete that exercises a non-LLM `Decision` shape). The choice is recorded for grep-ability.

These open questions are tracked as GitHub issues once the RFC is approved; the issue references replace the inline Tentative markers.

---

## 12. Out of scope for V1 / Future work

- **Harbor Cloud.** Managed execution plane. Separate product, post-V1.
- **Durable distributed bus drivers** (NATS, Redis Streams, Postgres-as-queue). Post-V1 phase set (`Distributed-2`, `Distributed-3`, …).
- **Additional planner concretes** beyond `react` and `deterministic`. PlanExecute, Workflow, Graph, Supervisor, MultiAgent, HumanApproval all wait on V1 evidence that the interface holds.
- **Reflection / critique loops** in the reference planner. Optional per concrete; not on V1's critical path.
- **Auto-sequence detection** (deterministic single-tool transitions skip the LLM call). Optional optimization, off by default.
- **Cross-process tool-context handle directory.** V1 keeps the registry process-local; a distributed handle directory is post-V1.
- **A2A northbound server** (Harbor as an A2A endpoint). V1 candidate but de-prioritized; revisit at V1.1.
- **An `episodic memory` tier** above `rolling_summary`.
- **Visualization editor** in the Console. V1 ships read-only topology visualization; an editor is later.

---

## 13. Appendix A — subsystem summary cross-reference

| Subsystem | RFC § | Briefs |
|---|---|---|
| Core runtime (engine, messages, streaming, routers, concurrency, playbooks) | §6.1 | `01-core-runtime.md` |
| Planner interface, Trajectory, RunContext | §6.2 | `02-planner-and-control.md`, `07-code-level-tool-calling.md` |
| Steering and unified pause/resume | §6.3 | `02-planner-and-control.md` + cross-fork synthesis |
| Tool catalog and transports | §6.4 | `03-tools-and-llm.md`, `07-code-level-tool-calling.md` |
| LLM client | §6.5 | `03-tools-and-llm.md`, `07-code-level-tool-calling.md`, `08-llm-client-validation.md` |
| Memory | §6.6 | `04-memory-and-skills.md` |
| Skills | §6.7 | `04-memory-and-skills.md` |
| Tasks | §6.8 | `05-state-tasks-artifacts-sessions.md` |
| Sessions | §6.9 | `05-state-tasks-artifacts-sessions.md` |
| Artifacts | §6.10 | `05-state-tasks-artifacts-sessions.md` |
| StateStore | §6.11 | `05-state-tasks-artifacts-sessions.md` |
| Distributed contracts | §6.12 | `05-state-tasks-artifacts-sessions.md` |
| Typed event bus | §6.13 | `06-events-observability-devx.md` |
| Telemetry (slog + OTel) | §6.14 | `06-events-observability-devx.md` |
| Governance (cost / rate / key rotation / failover) | §6.15 | `03-tools-and-llm.md`, `08-llm-client-validation.md` (cross-cutting) |
| Console (separate repo) | §7 | `06-events-observability-devx.md` |
| CLI | §8 | `06-events-observability-devx.md` |

---

## 14. Appendix B — the seven explicit upgrades baked in from t=0

These are the architectural decisions Harbor takes against the broader design space. Each is specified above; the appendix lists them here so phase plans can reference the doctrine in one place.

1. **Swappable planner.** A `Planner` interface from t=0; runtime owns mechanism, planner owns policy. The runtime never depends on a specific reasoning strategy. (See §3.2, §6.2.)
2. **Pause/resume as a runtime primitive, not a planner return type.** One coordinator serves HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED`/`INPUT_REQUIRED`, and steering `PAUSE`. (See §3.3, §6.3.)
3. **Native background tasks under unified `TaskID`.** Foreground and background are kinds of the same task; identity is unified. The runtime is task-keyed at the schema level. (See §6.8, §6.11.)
4. **One typed event bus.** Telemetry, streaming, and protocol emission share one canonical model. Logging and OTel derive from it; no parallel channels. (See §6.13, §6.14.)
5. **Tool transport unified at the type level.** Every `Tool` is the same struct regardless of source (in-process, HTTP, MCP, A2A). Dispatch is one switch in one place; visibility is filtered by the identity triple. (See §6.4.)
6. **Mandatory artifacts for heavy outputs.** No opt-in flag; no `NoOp` fallback. The router is always-on and the size threshold is configurable. (See §6.10.)
7. **Console as a Protocol client.** The Runtime is headless and emits canonical events; the Console renders projections. The Runtime never imports the Console; the Console never reads Runtime internals. This is what unlocks remote attach, fleet view, IDE/TUI clients, and observability-vendor adapters. (See §3.1, §5, §7.)

These are the doctrine. Phase plans cite them by number when justifying design choices.

---

*This RFC is the source of truth for V1 architecture. Updates land via PRs labeled `rfc`. Phase plans defer to it; if a phase plan and this RFC drift, the RFC wins and the plan is updated in the same PR.*
