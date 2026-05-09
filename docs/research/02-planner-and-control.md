# Research Brief 02 — Planner & Control Plane

> **Status:** Pre-RFC research, internal. This brief shapes the phase plan for
> Harbor's planner subsystem, the control plane (steering), and the pause/resume
> protocol. It is informed by an existing reference implementation that lives
> outside this repo; technical findings are re-expressed in Harbor's vocabulary
> below.

---

## 1. Subsystem overview

Harbor's central architectural commitment is **runtime↔planner separation**.
The runtime owns mechanism: sessions, tasks, events, streaming, retries,
pause/resume, artifacts, tool execution, memory injection, scheduling,
provenance, guardrails. The planner owns *policy*: reasoning, decision-making,
next-action selection.

The reference implementation that informs Harbor ships **exactly one planner**
(a JSON-only ReAct loop) and **no `Planner` interface at all**. The planner
class is concrete, stateful, and not thread-safe (it explicitly says "Create
separate planner instances per task"). Every runtime concern that ought to be
universal — memory injection, pause/resume serialisation, parallel-call
fan-out, schema repair, steering integration, trajectory compression,
reflection, error recovery — was bolted onto that single concrete class. The
class accumulates ~70 internal fields and >1700 lines, plus a 2300-line
"runtime" support module. New planning strategies (Plan-Execute, Workflow,
Graph, Supervisor, MultiAgent, Deterministic, HumanApproval) cannot be added
without forking the class.

**Harbor's biggest architectural lift** is to define a small `Planner`
interface from t=0 and push every runtime concern off the planner and into the
runtime itself, exposed to the planner only through a `RunContext` value. A
ReAct planner is the first concrete; further concretes land in later phases
without runtime changes.

A second commitment, equally load-bearing: the **control plane**
(cancel / pause / resume / inject context / redirect / user-message / approve /
reject / prioritize) is a **runtime** capability, surfaced over the Harbor
Protocol, not a planner-level API. The reference implementation puts steering
inside the planner loop, which couples every planner concrete to a specific
control-plane shape; Harbor inverts that — the runtime intercepts control
events between planner steps, and the planner observes them as advisory inputs
on its `RunContext`.

---

## 2. Key data shapes (Go-flavored sketches)

These are research sketches for the RFC, not final API.

```go
// Planner is the entire policy contract. Implementations: React, PlanExecute,
// Workflow, Graph, Deterministic, HumanApproval, MultiAgent, Supervisor.
type Planner interface {
    Next(ctx context.Context, run RunContext) (Decision, error)
}

// RunContext is what the runtime hands a planner. It is a *read+narrow-write*
// view of the run — the planner cannot reach into runtime internals.
type RunContext struct {
    SessionID, RunID, TenantID, UserID string

    Query       string
    Goal        string             // current goal (may be redirected by control)
    LLMContext  map[string]any     // visible-to-LLM context (memories etc.)
    ToolContext ToolContext        // tool-only handles (loggers, callbacks)
    Trajectory  *Trajectory        // append-only execution log
    Hints       PlanningHints      // optional ordering/parallel limits

    Catalog     ToolCatalog        // tools the planner may call
    Memory      MemoryView         // declared-policy memory access
    Skills      SkillLookup        // search/get on the skills subsystem
    Artifacts   ArtifactStore      // mandatory for heavy outputs

    Control     ControlSignals     // accumulated steering observations
    Budget      Budget             // deadline_s, hop_budget, cost cap
    Clock       func() time.Time
    Emit        func(Event)        // typed, canonical events only
}

// Decision is what a planner returns each step. Note that "tool call",
// "background task", and "subagent" are runtime-level concepts here, NOT
// planner-internal opcodes (the reference implementation overloaded a single
// `next_node` field with magic strings — Harbor does not).
type Decision interface { isDecision() }

type CallTool       struct { Tool string; Args json.RawMessage; Reasoning string }
type CallParallel   struct { Branches []CallTool; Join *JoinSpec }
type SpawnTask      struct { Kind TaskKind; Spec TaskSpec; GroupID string }
type AwaitTask      struct { TaskID string }
type RequestPause   struct { Reason PauseReason; Payload map[string]any }
type Finish         struct { Reason FinishReason; Payload any; Metadata map[string]any }
type NoOp           struct { Reason string } // "wait for steering", "summarising trajectory"

// Trajectory: planner execution log as a first-class artifact. Every step
// captures the action chosen, the observation, the LLM observation
// (compressed/redacted variant for the next prompt), errors, and any streamed
// chunks. Serialisation is deterministic and CONSERVATIVE.
type Trajectory struct {
    Query          string
    LLMContext     map[string]any
    ToolContext    ToolContext        // serialisable handle (see §4)
    Steps          []TrajectoryStep
    Summary        *TrajectorySummary // compaction artefact (goals/facts/pending)
    Sources        []Source
    Artifacts      map[string]ArtifactRef
    HintState      map[string]any
    SteeringInputs []SteeringInjection
    Background     map[string]BackgroundResult
    ResumeHint     *ResumeHint
}

type TrajectoryStep struct {
    Action        Decision
    Observation   any
    LLMObservation any // distinct projection for next-prompt building
    Error         string
    Failure       *FailureRecord
    Streams       map[string][]StreamChunk
    StartedAt     time.Time
    LatencyMS     int64
    TokenEstimate int
}

// Pause/Resume — runtime protocol primitives, not a planner return type.
type PauseReason string
const (
    PauseApprovalRequired   PauseReason = "approval_required"
    PauseAwaitInput         PauseReason = "await_input"
    PauseExternalEvent      PauseReason = "external_event"
    PauseConstraintsConflict PauseReason = "constraints_conflict"
)

type Pause struct {
    Token   ResumeToken    // opaque, runtime-issued
    Reason  PauseReason
    Payload map[string]any // sanitized, depth/size-bounded
    PausedAt time.Time
}

type ResumeToken string // opaque to clients; the runtime owns the encoding

// Steering — control plane primitives. The runtime owns the inbox; planners
// never instantiate it. Events pass through validation+sanitisation before
// reaching planner observation.
type ControlEvent struct {
    SessionID, TaskID, EventID string
    Type      ControlEventType
    Payload   map[string]any // sanitized
    TraceID   string
    Source    string         // "user", "supervisor", "external"
    CreatedAt time.Time
}

type ControlEventType string
const (
    CtlInjectContext ControlEventType = "INJECT_CONTEXT"
    CtlRedirect      ControlEventType = "REDIRECT"
    CtlCancel        ControlEventType = "CANCEL"
    CtlPause         ControlEventType = "PAUSE"
    CtlResume        ControlEventType = "RESUME"
    CtlApprove       ControlEventType = "APPROVE"
    CtlReject        ControlEventType = "REJECT"
    CtlPrioritize    ControlEventType = "PRIORITIZE"
    CtlUserMessage   ControlEventType = "USER_MESSAGE"
)
```

### Pause-reason taxonomy

The reference implementation defines four pause reasons. Harbor preserves the
taxonomy (it is a clean abstraction): `approval_required`, `await_input`,
`external_event`, `constraints_conflict`. Adding a new reason is an RFC-level
change to keep client tooling consistent.

### Control-event taxonomy (nine types)

The reference implementation ships nine event types. Harbor keeps the same
nine, all surfaced over the Protocol, all carrying `(session_id, task_id,
trace_id, event_id)`:

`INJECT_CONTEXT`, `REDIRECT`, `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`,
`APPROVE`, `REJECT`, `USER_MESSAGE`.

---

## 3. Public API surface

### Runtime → planner

```go
type Planner interface {
    Next(ctx context.Context, run RunContext) (Decision, error)
}
```

That is the only call the runtime makes into a planner. Per-step. Stateless on
the planner side except for whatever the implementation wants to keep keyed by
`RunID`. Reentrancy is fine; the runtime serializes calls per run.

### Planner → runtime (via `RunContext`)

The planner never imports runtime internals. It calls only:

- `RunContext.Catalog.{Search,Get,Validate,Schema}` — tool discovery + filter
  + arg validation. The catalog is already pre-filtered by tenant /
  visibility policy when the runtime hands it over.
- `RunContext.Memory.{Hydrate,Persist,Turn}` — declared-policy memory with the
  scoping triple already bound; the planner cannot reach to other scopes.
- `RunContext.Skills.{Search,Get,List,Propose}` — token-savvy skills.
- `RunContext.Artifacts.{Put,Get,Ref}` — heavy-output routing.
- `RunContext.Emit(event)` — emits a typed event onto the canonical bus.

A planner returns a `Decision` and the runtime executes it. The planner does
not call `tool.execute`, does not `spawn` tasks itself, does not write events
beyond canonical ones. This is what makes the planner swappable.

### Protocol exposure for steering

The Harbor Protocol exposes a control-plane endpoint per session/task:

- `POST /v1/sessions/{sid}/tasks/{tid}/control` — submit a `ControlEvent`.
- `GET  /v1/sessions/{sid}/tasks/{tid}/state`    — current state snapshot.
- `WS   /v1/sessions/{sid}/events`               — streaming canonical events.

The runtime validates+sanitises the event (depth/keys/list/string caps), then
deposits it on a per-run inbox. Between planner steps the runtime drains the
inbox, applies side effects (cancel ⇒ raise; pause ⇒ block until resume;
redirect ⇒ rewrite goal; inject_context / user_message ⇒ append a
`SteeringInjection` to the trajectory for the planner's next prompt build),
and emits `control.received` / `control.applied` events. **The planner sees
the result via `RunContext.Control` only; it does not receive the inbox.**

---

## 4. Internal mechanics

### The loop

Per run, the runtime owns a tight loop:

```
while not finished and steps < max_iters:
    if cancelled: emit run.cancelled; return
    drain control events; apply (may rewrite goal, append injections, raise pause/cancel)
    check deadline / hop budget / cost cap
    emit step.start
    if pending_action_queue: pop one (multi-action LLM responses)
    else if auto_seq detector unique: skip LLM call
    else: decision := planner.Next(ctx, run)
    execute decision in runtime:
        - CallTool       → tool dispatch + observation
        - CallParallel   → fan-out, gather, merge per JoinSpec
        - SpawnTask      → enqueue, return handle
        - AwaitTask      → block / return result
        - RequestPause   → durable pause record + protocol event
        - Finish         → exit loop
        - NoOp           → next iteration
    append TrajectoryStep
    if token_budget exceeded: invoke summarizer
    emit step.complete
return finish
```

The runtime *implements* this loop. The planner only contributes
`planner.Next` calls.

### Schema repair pipeline

The reference implementation does this work inside the planner (~1300 lines of
`validation_repair.py`). Harbor pulls it into a shared utility usable by any
planner:

1. **Salvage** — extract first valid JSON object from a malformed string;
   retry parse.
2. **Schema repair** — if action validates but tool args fail, emit a focused
   "fix these missing/invalid fields" sub-prompt instead of regenerating the
   whole action. Configurable: `arg_fill_enabled`, `repair_attempts`,
   `max_consecutive_arg_failures`.
3. **Graceful failure** — after N consecutive arg-validation failures, force a
   `Finish{Reason: NoPath, Followup: true}` to avoid infinite repair loops on
   small models.
4. **Multi-action salvage** — if the LLM emitted several JSON objects in one
   response, queue the additional read-only tool calls for sequential
   execution without another LLM hop (configurable).

This logic should sit in `planner/repair/` and be used by the React concrete
and any other concrete that opts in.

### Parallel-call merge semantics

`CallParallel{Branches, Join}` — branches execute concurrently via
`asyncio.gather` equivalent (in Go: a bounded worker pool). The runtime emits
`tool.call.start` / `tool.call.end` events per branch with `parallel_branch`
extras. Each branch's observation lands at a deterministic key in the join
input. The Join spec supplies a `mapping` from join args to parallel result
fields; if any branch issues a pause, the parallel call rolls up to a single
pause at the parent level (rather than partial completion). Validation
failures in *any* branch's args fail the whole parallel call before execution
starts (atomic setup, best-effort execution).

A system-level cap (`absolute_max_parallel`, default 50 in the reference)
backstops planning hints regardless of what the planner requested.

### Pause-state serialisation (the contract that MUST FAIL LOUDLY)

The reference implementation has a documented sharp edge: when a pause record
is serialised, `tool_context` is wrapped in `try: json.loads(json.dumps(...))
except (TypeError, ValueError): return None`. **It silently drops
non-serialisable tool context on resume.** The trajectory file itself does the
same thing on its `serialise()` method. The result: a planner can pause
holding a callback, get serialised to a state store, get loaded back, and
resume with `tool_context = None` — silently. Bugs that follow are extremely
hard to diagnose because no error is logged.

**Harbor's contract:**

1. `Trajectory.Serialize()` must succeed only if every entry is JSON-encodable;
   on failure it returns `(nil, ErrUnserializable)` naming the offending field
   path.
2. `ToolContext` is split into a **serialisable** half (IDs, configs, plain
   values) and a **non-serialisable** half (live callbacks, loggers,
   sockets). The non-serialisable half is registered with the runtime under a
   handle key; on resume the handle is re-attached from the runtime's live
   registry by key. If the handle cannot be re-attached, resume FAILS with
   `ErrToolContextLost{Handle: "..."}` — never silently.
3. The persistence drivers (in-memory / SQLite / Postgres — see brief 03)
   must round-trip the trajectory byte-for-byte; a conformance test asserts
   this.

### Trajectory compression

When `token_budget` is exceeded, the runtime invokes a configurable summariser
(a separate, cheaper LLM in the reference) to produce a `TrajectorySummary
{Goals, Facts, Pending, LastOutputDigest, Note}`. The compressed digest
replaces the raw step history in subsequent prompt builds. Compression is a
runtime concern (it operates on the trajectory), not a planner concern; the
planner sees the compressed view via `RunContext.Trajectory.Summary`.

---

## 5. Sharp edges in the reference implementation that Harbor must avoid

Citing source paths inside `~/Repos/Penguiflow/penguiflow/penguiflow/`:

1. **Silent context loss on resume.** `planner/pause_management.py:128-138`
   (`_serialise_pause_record`) and `planner/trajectory.py:223-228`
   (`Trajectory.serialise`) both wrap JSON serialisation in a `try/except` that
   sets `tool_context = None` on failure, with no log/raise. Harbor closes this
   per §4 above.

2. **Steering at planner level.** `planner/react_runtime.py:716-794`
   (`_apply_steering`) drains a `SteeringInbox` *inside* the planner loop and
   mutates the trajectory directly. Every alternate planner would need to
   replicate this. Harbor moves the inbox into the runtime; planners observe
   only `RunContext.Control`.

3. **No `Planner` interface.** `planner/react.py:220` is a concrete class with
   ~70 fields and constructor parameters. The "swappable planner" property
   does not exist in the reference. Harbor's V1 RFC ships the interface even
   if only one concrete is wired.

4. **Magic strings as opcodes.** `planner/models.py:260-336` overloads the
   `next_node` string with `final_response`, `parallel`, `task.subagent`,
   `task.tool` as sentinels alongside real tool names. Harbor's `Decision` is
   a sum type; tool calls and runtime opcodes are different shapes. Future
   runtime-level actions (e.g. `delegate`, `wait_event`) extend the sum, not
   the catalog of magic strings.

5. **Parallel pause partial-completion ambiguity.** `planner/parallel.py:31`+
   gathers branches and only then collects the first `pause_result`; siblings
   may have already emitted side effects. Harbor specifies: parallel pause is
   atomic (either no branch starts side-effecting tools, or all branches that
   started must reach a checkpointed observation before the pause record
   commits).

6. **Thread-safety disclaimer.** `planner/react.py:228-231` says "NOT
   thread-safe. Create separate planner instances per task." The runtime
   compensates with a session lock around `run`/`resume`. Harbor's interface
   requires planners to be safe to use concurrently across runs (the runtime
   serialises *within* a run); statefulness keyed only by `RunID` is the
   pattern.

7. **70+ planner constructor parameters.** Harbor's `Planner` interface has
   no constructor; concretes use functional options (`react.New(opts ...Opt)
   Planner`) and most knobs (token budget, hop budget, deadline, max_iters,
   cost cap, schema mode) move to runtime-level run options because they are
   not reasoning-policy concerns.

8. **Steering payload size limits as planner constants.**
   `steering/steering.py:11-15` defines `MAX_STEERING_PAYLOAD_BYTES=16384`,
   `MAX_STEERING_DEPTH=6`, `MAX_STEERING_KEYS=64`,
   `MAX_STEERING_LIST_ITEMS=50`, `MAX_STEERING_STRING=4096`. Harbor keeps the
   limits but at runtime/protocol level, configurable, and applied before the
   event ever reaches a planner.

---

## 6. Tests required

- **Unit (planner-internal):** schema repair pipeline, arg-fill, salvage,
  graceful-failure forcing, multi-action queueing.
- **Interface conformance:** a shared test pack that any `Planner`
  implementation must pass (given a canned tool catalog and LLM mock,
  produces a valid `Decision` for the top 20 prompts; respects budget;
  never panics on malformed LLM output).
- **Trajectory serialisation round-trip:** generate, serialise, deserialise,
  re-serialise; must be byte-identical. Negative case: trajectory with a
  callback in `ToolContext` must produce `ErrUnserializable` (not silently
  drop).
- **Pause/resume durability:** pause → save (in-mem / SQLite / Postgres) →
  load → resume; trajectory and steering history exact match. Attempt resume
  with non-recoverable `ToolContext` handle; must return
  `ErrToolContextLost`.
- **Steering mid-step:** events submitted while a tool call is in flight are
  applied at the next step boundary, never mid-tool. CANCEL during a tool
  call cancels at the next safe boundary; `hard=true` propagates a
  cancellation context to the in-flight tool. PAUSE blocks at the next
  boundary; RESUME unblocks. INJECT_CONTEXT and REDIRECT are visible on the
  next planner step.
- **Concurrency:** N concurrent runs across the same session do not interfere
  with each other's trajectories, control inboxes, or memory. Cross-session
  isolation test asserts no cross-talk in events, memory, artifacts, or
  steering.
- **Parallel branch atomicity:** validation failure in one branch fails the
  whole parallel call; pause in one branch produces a single pause record at
  parent level.
- **Reflection / critique** (later phase): when enabled, the runtime invokes
  the configured critique LLM before finishing; the critique result becomes a
  trajectory step.

---

## 7. Phase decomposition

Sized for "depth, not breadth." Order is the suggested implementation order;
some can be parallelised on the calendar but each ships independently with
its own smoke + tests.

1. **`Planner` interface + `Decision` sum + `RunContext` + `Trajectory`
   types.** No concrete planner yet. Pure types + serialisation contracts +
   round-trip tests + planner-conformance test harness. Smoke: a stub planner
   returns a static `Finish` and the runtime executes it end-to-end.
2. **Reference ReAct planner (minimum viable).** LLM call loop, JSON-only
   action format, tool selection, completion detection, single tool call per
   step. No parallel, no schema repair beyond a single retry. Smoke:
   3-step reasoning task succeeds against a mock LLM.
3. **Runtime control plane and pause/resume.** Per-run control inbox,
   protocol endpoint, validation + sanitisation, taxonomy of nine event
   types, four pause reasons. Pause serialisation contract that fails loudly
   on non-serialisable state. ToolContext split (serialisable/handle).
   Smoke: an APPROVE flow round-trips through the protocol.
4. **Steering at runtime level.** Drain-between-steps semantics, control
   history capped, INJECT_CONTEXT / REDIRECT / USER_MESSAGE / CANCEL / PAUSE
   / RESUME / APPROVE / REJECT / PRIORITIZE all wired. Cancellation
   propagation to in-flight tool calls (soft and hard).
5. **Schema repair pipeline.** Salvage, arg-fill, graceful failure,
   multi-action salvage. Lives in `planner/repair/`, opt-in per concrete.
6. **Parallel-call execution + JoinSpec.** Atomic setup validation, bounded
   concurrency, deterministic merge keys, parallel-pause atomicity contract.
7. **Trajectory compression.** Configurable summariser; runtime-driven; the
   planner sees only the compacted view.
8. **Reflection / critique loop** (optional per planner).
9. **Auto-sequence detection** (deterministic single-tool transitions skip
   the LLM call). Optional, off by default.
10. **Second concrete planner — Plan-Execute** — to prove the interface
    holds. Conformance pack must pass.
11. *(Deferred to later phases)* Workflow planner, Graph planner,
    Deterministic planner, MultiAgent planner, Supervisor planner,
    HumanApproval planner.

That is roughly **9–11 phases** for the planner+control subsystem alone,
which fits the "many phases is fine" stance.

---

## 8. Cross-subsystem dependencies

- **Tools subsystem (brief 04 — separate fork):** `RunContext.Catalog` and
  the `CallTool` / `CallParallel` execution path depend on the unified tool
  abstraction (MCP / A2A / HTTP / in-process behind one interface).
- **Memory subsystem (brief 05):** `RunContext.Memory` exposes the
  declared-policy memory; planner only consumes the view.
- **Skills subsystem (brief 06):** `RunContext.Skills` exposes
  search/get/list/propose; the propose path persists generated skills.
- **Tasks subsystem (brief 07):** `SpawnTask` / `AwaitTask` decisions depend
  on the unified foreground/background task identity model.
- **Artifacts subsystem (brief 08):** `RunContext.Artifacts` is mandatory for
  heavy outputs; the planner cannot return a heavy payload inline.
- **Events subsystem (brief 09):** `RunContext.Emit` writes onto the typed
  event bus; the protocol layer projects those events to clients.
- **State stores (brief 10):** pause records, trajectories, control history
  all persist via the storage interfaces (in-memory / SQLite / Postgres) with
  conformance tests across all three.
- **Sessions subsystem (brief 11):** a planner run is scoped to
  `(tenant, user, session)`; the session owns the run lock; runs are scoped
  within sessions.
- **LLM client (brief 12):** the planner calls the LLM via a
  `JSONLLMClient`-style protocol; the client is configured at run start by
  the runtime, not constructed by the planner.

---

## 9. Open questions for the user

1. **V1 planner concretes.** Settled: ReAct ships in V1. The `Planner`
   interface + conformance pack also ship in V1. Do we additionally ship a
   second concrete in V1 (e.g. Plan-Execute or Deterministic) to *prove* the
   interface holds, or accept that the second concrete lands in V1.1? My lean:
   ship one second concrete (Deterministic — easiest, exercises a different
   shape of `Decision` flow).

2. **Pause-state serialisation format.** Plain JSON, MessagePack, CBOR, or
   protobuf? JSON is simplest and aligns with the protocol; protobuf gives
   schema evolution and smaller payloads at the cost of code generation.
   Recommendation: JSON for the trajectory (fits "events first-class"), with
   a documented schema version and a `format_version` field gating
   round-trip compatibility.

3. **Steering authn/authz model.** Who can submit which control events?
   `CANCEL` and `APPROVE/REJECT` clearly need auth bound to the originating
   user/admin; `INJECT_CONTEXT` from a supervisor agent vs. a human user has
   different trust implications. Should the protocol require a scope/role
   per event type, or is "session-scoped JWT can submit any control event"
   acceptable for V1?

4. **Tool-context handle registry persistence.** The handle that re-attaches
   non-serialisable tool context on resume — is the handle registry
   process-local only (resume must run in the same process), or does it have
   a cross-process discovery mechanism in V1? Lean: process-local in V1,
   documented as a known constraint, with a clear seam for a distributed
   handle directory in a later phase.

5. **`NoOp` decisions.** Does the planner need a `NoOp{Reason}` decision
   (e.g. "wait for steering," "summarising trajectory") to keep the loop
   running without a tool call? The reference implementation handles these
   cases by short-circuiting in the runtime. My lean: keep the runtime
   short-circuits; the planner never returns `NoOp`, simplifying the
   conformance pack.

---

## Source map (internal only — never surfaced in Harbor artifacts)

| Harbor concept                | Source path (`~/Repos/Penguiflow/penguiflow/penguiflow/`)        |
| ---                           | ---                                                              |
| `Planner` (interface)         | (does not exist in source — `planner/react.py:220` is concrete)  |
| `Decision` sum                | `planner/models.py:260` (`PlannerAction` overloaded with magic)  |
| `RunContext`                  | (synthesised; closest is `planner/context.py:69` `ToolContext`)  |
| `Trajectory`                  | `planner/trajectory.py:182`                                      |
| Pause record + serialisation  | `planner/pause.py:13`, `planner/pause_management.py:128`         |
| Pause reasons                 | `planner/context.py:18` `PlannerPauseReason` (4 values)          |
| Control event types           | `state/models.py:179` `SteeringEventType` (9 values)             |
| Control inbox                 | `steering/steering.py:151` `SteeringInbox`                       |
| Control validation/sanitise   | `steering/steering.py:25-148`                                    |
| Run loop                      | `planner/react_runtime.py:1575` `run_loop`                       |
| Step                          | `planner/react_step.py:37` `step`                                |
| Schema repair                 | `planner/validation_repair.py` (1317 lines)                      |
| Parallel exec + join          | `planner/parallel.py:31` `execute_parallel_plan`                 |
| Trajectory compression        | `planner/llm.py` `summarise_trajectory`                          |
| Steering integration in loop  | `planner/react_runtime.py:716` `_apply_steering`                 |
| Constructor surface (sharp)   | `planner/react.py:220-336` (~70 fields, ~50 constructor params)  |
