# Research brief: State, Tasks, Artifacts, Sessions, Distributed contracts

> Scope: the durability and identity backbone of the Harbor runtime. Sibling briefs cover the DAG runtime, planner protocol, tools, memory/skills, events/telemetry. This brief is phase-planning depth, not encyclopedia coverage.

---

## 1. Subsystem overview

This subsystem is the **durability layer** of Harbor. Five contracts live here, each shipped as a Go interface plus drivers:

- **StateStore** — append-only audit log + checkpointing surface for runs, tasks, planner state, memory snapshots, steering events, trajectories, and remote-agent bindings. The persistence floor for everything else.
- **ArtifactStore** — content-addressed blob store for heavy outputs (PDFs, images, large text). Returns compact `ArtifactRef`s that are safe to embed in LLM context.
- **TaskRegistry** — orchestration surface for foreground and background work. Spawns, lists, gets, cancels, prioritizes, groups. **Harbor unifies foreground and background under a single `TaskID` namespace** — a key upgrade.
- **SessionRegistry** — lifecycle of long-lived multi-turn conversations. A session contains many runs (foreground tasks). Identity is the triple `(tenant_id, user_id, session_id)`.
- **MessageBus / RemoteTransport** — pluggable transports for distributed execution. V1 ships in-process defaults; the contracts exist so a distributed backend can land post-V1 without runtime changes.

### Why three persistence backends at V1
The reference implementation we are inheriting from defines these contracts but ships **only an in-memory driver and an audit-log adapter** — no production persistence. Operators have to assemble queueing, worker management, and discovery themselves. Harbor breaks that pattern by shipping **three** drivers from day one:

1. **InMemory** — zero dependencies; the default for embedded use, dev, and tests.
2. **SQLite** — single-binary deployments via `modernc.org/sqlite` (CGo-free, matching the gateway's storage stance).
3. **Postgres** — multi-node production via `pgx`.

All three pass the same conformance suite. Designing the interface against three backends from t=0 forces clean abstractions; designing against one tends to leak that backend's assumptions into the contract.

### Mandatory artifacts policy
The reference implementation ships a `NoOpArtifactStore` fallback that warns and truncates. **Harbor removes this fallback.** An ArtifactStore is always configured; the in-memory driver is the floor. A heavy output above the threshold (default: 32KB, configurable) routes through the ArtifactStore — never inline. This is a runtime-level invariant, not a per-tool opt-in flag.

---

## 2. Key data shapes (Go-flavored)

```go
// ---- Identity triple ----------------------------------------------------

type TenantID string
type UserID   string
type SessionID string
type TaskID    string  // unifies foreground runs and background tasks
type EventID   string

// Identity flows through ctx; never stored package-globally.
type Identity struct {
    TenantID  TenantID
    UserID    UserID
    SessionID SessionID
}

// ---- StateStore --------------------------------------------------------

type StateEvent struct {
    EventID  EventID         // ULID, monotonically increasing
    TaskID   TaskID          // run or background task
    Kind     string          // "task.started", "tool.called", ...
    Ts       time.Time
    NodeName string          // optional
    Payload  json.RawMessage // canonical JSON; redacted upstream
}

type RemoteAgentBinding struct {
    TaskID         TaskID
    ContextID      string
    RemoteTaskID   string
    AgentURL       string
    RemoteSkill    string
    TenantID       TenantID
    UserID         UserID
    LastRemoteID   string
    Terminal       bool
    Metadata       map[string]any
}

type StateStore interface {
    // Core (mandatory; same in every driver)
    SaveEvent(ctx context.Context, ev StateEvent) error          // idempotent on EventID
    LoadHistory(ctx context.Context, id TaskID) ([]StateEvent, error)
    SaveBinding(ctx context.Context, b RemoteAgentBinding) error
    FindBinding(ctx context.Context, q BindingQuery) (*RemoteAgentBinding, error)
    ListBindings(ctx context.Context, sessionID SessionID) ([]RemoteAgentBinding, error)
    MarkBindingTerminal(ctx context.Context, taskID TaskID, contextID, remoteTaskID string) error

    // Planner checkpoints
    SavePlannerCheckpoint(ctx context.Context, token string, payload []byte) error
    LoadPlannerCheckpoint(ctx context.Context, token string) ([]byte, bool, error)

    // Memory state
    SaveMemoryState(ctx context.Context, key MemoryKey, state []byte) error
    LoadMemoryState(ctx context.Context, key MemoryKey) ([]byte, bool, error)

    // Tasks + lifecycle updates
    SaveTask(ctx context.Context, t Task) error
    ListTasks(ctx context.Context, sessionID SessionID, f TaskFilter) ([]Task, error)
    SaveTaskUpdate(ctx context.Context, u TaskUpdate) error
    ListTaskUpdates(ctx context.Context, sessionID SessionID, f UpdateFilter) ([]TaskUpdate, error)

    // Steering
    SaveSteering(ctx context.Context, ev SteeringEvent) error
    ListSteering(ctx context.Context, sessionID SessionID, f SteeringFilter) ([]SteeringEvent, error)

    // Trajectories (planner reasoning log)
    SaveTrajectory(ctx context.Context, taskID TaskID, t Trajectory) error
    GetTrajectory(ctx context.Context, taskID TaskID) (*Trajectory, bool, error)
    ListTaskIDs(ctx context.Context, sessionID SessionID, limit int) ([]TaskID, error)
}
```

Note: the reference implementation breaks these into **eight optional `Supports*` protocols** and uses `hasattr` duck-typing. Harbor merges them into **one mandatory interface** because all three V1 drivers implement everything. Optionality breeds capability-detection ceremony in callers; we pay that cost up front.

```go
// ---- ArtifactStore -----------------------------------------------------

type ArtifactScope struct {
    TenantID  TenantID
    UserID    UserID
    SessionID SessionID
    TaskID    TaskID
}

type ArtifactRef struct {
    ID        string         // "{namespace}_{sha256[:12]}"
    MimeType  string
    SizeBytes int64
    Filename  string
    SHA256    string
    Scope     ArtifactScope
    Namespace string
    Source    map[string]any // tool-name, preview, warnings
}

type ArtifactStore interface {
    PutBytes(ctx context.Context, data []byte, opts PutOpts) (ArtifactRef, error)
    PutText (ctx context.Context, text string, opts PutOpts) (ArtifactRef, error)
    Get     (ctx context.Context, id string) ([]byte, bool, error)
    GetRef  (ctx context.Context, id string) (*ArtifactRef, bool, error)
    Exists  (ctx context.Context, id string) (bool, error)
    Delete  (ctx context.Context, id string) (bool, error)
    List    (ctx context.Context, filter ArtifactScope) ([]ArtifactRef, error)
}
```

```go
// ---- Task / Task lifecycle --------------------------------------------

type TaskKind   string  // "foreground" | "background"
type TaskStatus string  // PENDING | RUNNING | PAUSED | COMPLETE | FAILED | CANCELLED

type Task struct {
    ID            TaskID
    SessionID     SessionID
    TenantID      TenantID
    UserID        UserID
    Kind          TaskKind
    Status        TaskStatus
    Priority      int
    ParentTaskID  *TaskID         // nil for top-level foreground turns
    GroupID       *TaskGroupID
    Description   string
    Query         string
    Context       *TaskContextSnapshot
    Result        *TaskResult
    Error         *TaskError
    CreatedAt     time.Time
    UpdatedAt     time.Time
    PropagateOnCancel string      // "cascade" | "isolate"
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

    // Groups (parallel fan-out / retain-turn semantics)
    ResolveOrCreateGroup(ctx context.Context, req GroupRequest) (*TaskGroup, error)
    SealGroup           (ctx context.Context, id TaskGroupID) error
    CancelGroup         (ctx context.Context, id TaskGroupID, reason string, propagate bool) error
    ApplyGroup          (ctx context.Context, id TaskGroupID, action GroupAction) error
    ListGroups          (ctx context.Context, sessionID SessionID, status *TaskGroupStatus) ([]TaskGroup, error)

    // Patches (pending context patches awaiting human approval)
    ApplyPatch          (ctx context.Context, sessionID SessionID, patchID string, action PatchAction) (bool, error)
    AcknowledgeBackground(ctx context.Context, sessionID SessionID, ids []TaskID) (int, error)
}
```

```go
// ---- Session ----------------------------------------------------------

type Session struct {
    ID        SessionID
    TenantID  TenantID
    UserID    UserID
    OpenedAt  time.Time
    LastSeen  time.Time
    Closed    bool
    Limits    SessionLimits
    Context   SessionContext   // version, hash, llm/tool ctx, memory, artifacts
}

type SessionRegistry interface {
    Open    (ctx context.Context, id SessionID, ident Identity) (*Session, error)
    Get     (ctx context.Context, id SessionID) (*Session, error)
    Touch   (ctx context.Context, id SessionID) error
    Close   (ctx context.Context, id SessionID, reason string) error
    Inspect (ctx context.Context, id SessionID) (*SessionSnapshot, error)  // protocol-facing
    GC      (ctx context.Context, policy GCPolicy) (int, error)
}
```

```go
// ---- MessageBus / RemoteTransport -------------------------------------

type BusEnvelope struct {
    Edge     string
    Source   string
    Target   string
    TaskID   TaskID
    Payload  json.RawMessage
    Headers  map[string]any
    Meta     map[string]any
}

type MessageBus interface {
    Publish(ctx context.Context, env BusEnvelope) error  // at-least-once
}

type RemoteTransport interface {
    Send  (ctx context.Context, req RemoteCallRequest) (RemoteCallResult, error)
    Stream(ctx context.Context, req RemoteCallRequest) (RemoteEventStream, error)
    GetTask     (ctx context.Context, taskID, contextID string) (*RemoteTaskSnapshot, error)
    Subscribe   (ctx context.Context, taskID, contextID string) (RemoteTaskEventStream, error)
    Cancel      (ctx context.Context, taskID, contextID string) error
}
```

---

## 3. Public API surface

**Runtime → StateStore.** Every state transition emits a `StateEvent` (unified with the typed event bus — same wire shape, the bus is a fan-out projection of the audit log). Planner checkpoints land here on every protocol-level pause. Memory subsystem persists rolling state here. Skills subsystem persists generated skills here.

**Runtime → ArtifactStore.** Tool results above the size threshold are auto-routed; the runtime injects a `ScopedArtifacts` facade per task that auto-stamps the identity triple on writes and scope-checks on reads. Tools call `Upload` / `Download` against the facade — they never see raw scopes.

**Planner → TaskRegistry.** The planner emits decisions; the runtime translates `task.subagent` / `task.tool` / `task.cancel` / `task.prioritize` opcodes into TaskRegistry calls. The registry is reachable from the protocol's task-control surface, so the Console and CLI can drive it the same way the planner does.

**Protocol → SessionRegistry, TaskRegistry, StateStore.** The Harbor Protocol exposes:
- `sessions.open / list / inspect / close`
- `tasks.list / get / cancel / prioritize / spawn / inspect`
- `state.history / load_planner_checkpoint / list_trajectories`
- `artifacts.list / get / get_ref / delete` (scope-checked)

Console renders projections of these; it never reads internal Go structs.

**Distributed seam.** When a `MessageBus` driver is configured, the runtime publishes envelopes on cross-worker edges. When a `RemoteTransport` is configured for an A2A-targeted edge, calls go through it instead of in-process. V1 ships in-process drivers; the contracts are versioned and frozen so a future distributed driver lands without runtime churn.

---

## 4. Internal mechanics

**Schema and migrations.** Forward-only, numbered monotonically (`0001_init.sql`, `0002_*.sql`, ...) per driver. Each migration ends with a `schema_migrations` insert. SQLite uses WAL. Postgres uses `pgx` migrations. Drivers self-register from `init()`; `cmd/harbor` blank-imports them. Editing a merged migration is forbidden; corrections land as new migrations.

**Conformance test approach.** A single `statestoretest.RunSuite(t, factory)` helper drives **every** scenario against any factory function `func() (StateStore, func(), error)`. The InMemory, SQLite, and Postgres test packages each call the suite with their factory. CI runs all three. A new optional capability is a new method on the interface plus a new conformance scenario — no per-driver hand-waving.

**Idempotency.** `SaveEvent` keys on `EventID` (ULID provided by caller) and is a no-op on duplicate. `Spawn` honors an `IdempotencyKey` per `(SessionID, IdempotencyKey)` so a retried spawn returns the original `TaskHandle`.

**Artifact dedup and content addressing.** IDs are `{namespace}_{sha256[:12]}`. Re-uploading identical bytes returns the existing ref. The `ScopedArtifacts` facade is immutable post-construction; access control reads scope fields and rejects on mismatch. Listing by scope treats `nil` fields as wildcards.

**Task lifecycle state machine.**
```
PENDING → RUNNING → COMPLETE
              ↓
            PAUSED (planner-initiated; durable via planner checkpoint)
              ↓
            RUNNING (after resume)
              ↓
            FAILED | CANCELLED | COMPLETE
```
Cancellation propagation honors `propagate_on_cancel` ("cascade" | "isolate"). Group sealing freezes membership; `retain_turn` blocks the foreground until the group completes (no `HUMAN_GATED` interaction in retain mode).

**Session-lifetime invariants.** A session is open until explicitly closed or GC'd. Reopen-after-close is forbidden — clients open a new session. The identity triple is captured on open and **immutable** for the session's lifetime; reusing a session ID across tenants/users is rejected. `Touch` updates `LastSeen`; GC sweeps sessions whose `LastSeen` exceeded the policy TTL and have no RUNNING tasks.

**Distributed delivery semantics (V1 contracts).** `MessageBus.Publish` is **at-least-once** — handlers must be idempotent on `(TaskID, Edge, EventID)`. `RemoteTransport.Send` is request/reply; `Stream` yields ordered events with a final `done=true`. No exactly-once primitive; the at-least-once + idempotent-handler pattern matches the in-process driver's semantics so behavior is consistent.

**Audit redaction.** Every `StateEvent.Payload` passes through a redactor before persistence. Tool arguments and results are summarized or hashed; full payloads are never stored. This is a runtime-level guarantee.

---

## 5. Sharp edges from the source (and how Harbor handles them)

- **No production StateStore backend ships.** `~/Repos/Penguiflow/penguiflow/penguiflow/state/in_memory.py` is the only driver in the canonical package; durable persistence is left to operators. **Harbor ships three drivers.**
- **Optional duck-typed capabilities.** `~/Repos/Penguiflow/penguiflow/penguiflow/state/protocol.py` defines eight `Supports*` protocols with `hasattr` detection in callers. **Harbor mandates the full surface in one interface.**
- **Artifacts are opt-in.** `~/Repos/Penguiflow/penguiflow/penguiflow/artifacts.py` ships `NoOpArtifactStore` as the silent fallback (`logger.warning(...)` once). **Harbor removes the no-op; in-memory is the floor and routing is mandatory above the threshold.**
- **Foreground/background identity is split.** Foreground runs are identified by `trace_id` while background tasks have a separate `task_id` namespace tracked by `~/Repos/Penguiflow/penguiflow/penguiflow/sessions/task_service.py`. The runtime needs to translate between them. **Harbor unifies under `TaskID` — runs are tasks of kind `foreground`.**
- **Distributed contracts ship without backends.** `~/Repos/Penguiflow/penguiflow/penguiflow/bus.py` and `remote.py` define `MessageBus` and `RemoteTransport` as protocols only. **Harbor mirrors that for V1 (deliberate — the user accepted in-process at V1) but versions and freezes the contracts so a distributed driver can ship later without runtime churn.**
- **TaskService's interface mixes orchestration and group governance.** A single `TaskService` protocol carries 14 methods covering spawn, list, get, cancel, prioritize, patches, groups, and tool-jobs. **Harbor keeps the surface but groups it into named method sets in the Go interface for navigability, and lifts groups into a sibling interface (`TaskGroupRegistry`) once the surface stabilizes.**
- **`StateStoreSessionAdapter` writes session updates as audit events keyed by `f"session:{session_id}"`.** That's a string-trick for compatibility with a trace-keyed audit log. **Harbor's StateStore is task-keyed at the schema level; sessions are first-class with their own table; the adapter trick is unnecessary.**

---

## 6. Tests required

- **Unit tests per driver.** Each driver covers its own edge cases (SQLite WAL behavior, Postgres advisory locks, in-memory clone semantics).
- **Cross-driver conformance.** One suite in `internal/storage/conformance/` runs the full scenario set against any factory. Required for the InMemory, SQLite, and Postgres drivers; the same suite validates a future distributed driver.
- **Migration tests.** Clean DB starts cleanly. Existing DB at version N migrates to N+1. Round-trip of a real workload across the migration leaves data intact.
- **Concurrency tests.** N concurrent sessions × M concurrent tasks each, asserting no cross-talk in events, memory, artifacts, or task results. This is the Harbor analogue of the gateway's "cross-tenant isolation" gate.
- **Goroutine leak tests.** Long-lived components (TaskRegistry workers, GC sweeper, distributed bus subscribers) return `runtime.NumGoroutine` to baseline after shutdown.
- **Artifact cleanup tests.** TTL expiry, LRU eviction at session/trace byte limits, dedup-on-rewrite, scope-mismatch rejection.
- **Cross-tenant isolation.** Storing an artifact under tenant A and attempting to read under tenant B fails. Same for tasks, sessions, memory, trajectories.
- **Session lifetime.** Open → many runs → close. Reopen-after-close rejected. GC removes idle sessions with no RUNNING tasks but leaves sessions with active tasks alone.
- **Idempotency.** Replaying a `SaveEvent` with the same `EventID` is a no-op. Spawning with the same `IdempotencyKey` returns the same handle.

---

## 7. Phase decomposition suggestion

This subsystem maps to roughly **9 phases**. Each is one shippable surface with its own conformance / smoke checks.

1. **State-1: StateStore interface + InMemory driver + conformance harness.** Audit log, planner checkpoints, memory state, task records, trajectories. Conformance suite skeleton; InMemory passes. Used by every later phase.
2. **State-2: SQLite driver.** `modernc.org/sqlite`, WAL journal, forward-only migrations. Same conformance suite passes.
3. **State-3: Postgres driver.** `pgx`, advisory locks for binding semantics, JSONB payloads. Same conformance suite passes. Three-driver matrix in CI.
4. **Artifacts-1: ArtifactStore interface + InMemory + Filesystem drivers.** `ScopedArtifacts` facade; mandatory routing above threshold; deletion-by-scope; size-limit enforcement.
5. **Artifacts-2: SQLite-blob and Postgres-blob drivers, plus S3-style driver.** Persistent artifact lifetimes that survive restart; matches the StateStore driver triad.
6. **Tasks-1: TaskRegistry interface + InProcess driver.** Foreground/background unification under `TaskID`. Lifecycle state machine. Cancellation propagation. Idempotency. Persists through StateStore.
7. **Tasks-2: TaskGroup support.** Group resolution, sealing, retain-turn semantics, group-level cancel/apply, patch governance. Background-tasks-config knobs (timeouts, continuation hops).
8. **Sessions-1: SessionRegistry + lifecycle.** Open/get/touch/close/GC. Identity-triple immutability. Limits enforcement. Protocol-facing `Inspect`.
9. **Distributed-1: MessageBus + RemoteTransport contracts.** In-process MessageBus driver (loopback). RemoteTransport stub for A2A; V1 ships the contract and a single in-process implementation; durable distributed backends are post-V1 phases (`Distributed-2`, `Distributed-3`, …).

A future post-V1 phase (`Tasks-Durable`) implements a queued backend (Postgres-as-queue or NATS JetStream) for background tasks, slotting in behind the `TaskRegistry` interface without runtime changes.

---

## 8. Cross-subsystem dependencies

| Consumer | Depends on |
|---|---|
| **Memory subsystem** | `StateStore.SaveMemoryState/LoadMemoryState`, scoped by `MemoryKey(tenant, user, session)`. |
| **Skills subsystem** | `StateStore` for generated-skill persistence + audit; `ArtifactStore` for skill payload bytes when skills include heavy attachments. |
| **Planner** | `StateStore.SavePlannerCheckpoint/LoadPlannerCheckpoint` for protocol-level pause/resume; `StateStore.SaveTrajectory` for reasoning logs. |
| **Tools** | `ArtifactStore` (mandatory routing for heavy outputs); `StateStore` for invocation audit; `RemoteTransport` for A2A-routed tools. |
| **Events / typed bus** | `StateEvent` is the shared wire shape; the bus is a fan-out projection of the StateStore audit log. |
| **Steering / HITL** | `TaskRegistry.Cancel/Prioritize`, `StateStore.SaveSteering`. |
| **Audit / governance** | Every write goes through StateStore; every event is redacted on emit; per-tenant queries are the default. |
| **Harbor Protocol** | Exposes Sessions, Tasks, State queries, and Artifacts to Console / CLI / third-party clients. |
| **CLI (`harbor dev`)** | Boots in-memory drivers by default; `--persist sqlite://...` switches to durable; same Protocol surface either way. |

---

## 9. Open questions for the user

1. **Heavy-output threshold.** What size triggers mandatory ArtifactStore routing? Default proposed: 32 KB. Reasonable range 16 KB – 128 KB. Lower = more router overhead and smaller LLM context; higher = more context bloat risk. Should the threshold be a runtime config or per-tool overridable?
2. **Session GC policy.** Default proposed: idle TTL 24 h, hard cap 30 days, sweep every 15 min, refuse-to-GC any session with a RUNNING task. Confirm or revise.
3. **Build-tag strategy for SQLite + Postgres.** Three options: (a) ship both in the default binary so operators choose at config time; (b) build tags so distros can drop one for size; (c) one binary per backend. The gateway ships SQLite-only by default — should Harbor match, or is the multi-backend story enough of a feature to warrant the size cost?
4. **Distributed-1 scope at V1.** Confirm: V1 ships the `MessageBus` and `RemoteTransport` interfaces, an in-process `MessageBus` (loopback), and a `RemoteTransport` capable of speaking A2A to a remote agent; **no** durable bus driver (NATS, Redis Streams, Postgres-as-queue) at V1. Post-V1 phases add those.
5. **TaskGroup retain-turn timeouts and continuation hops** — should these be defaults on the runtime, per-session config, or per-spawn override? The reference implementation makes them per-session-config; Harbor can simplify.
