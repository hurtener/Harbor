# Phase 20 — TaskRegistry interface + InProcess driver + lifecycle

## Summary

Land `internal/tasks/`: the unified `TaskID` namespace covering both foreground runs and background tasks; the `TaskRegistry` interface (Spawn / Get / List / Cancel / Prioritize); the `InProcess` driver that backs V1; the canonical lifecycle state machine (`PENDING → RUNNING → COMPLETE` with `PAUSED → RUNNING` and terminal `FAILED|CANCELLED`); idempotency via `IdempotencyKey`; cancellation propagation per `PropagateOnCancel` ("cascade" | "isolate"). The `TaskGroup` half (groups, sealing, retain-turn, patches) ships in Phase 21 — this phase focuses on the per-task surface so Phase 21 has a stable foundation. Persists through Phase 07's StateStore via a typed wrapper (D-027).

## RFC anchor

- RFC §6.8
- RFC §3.5
- RFC §4
- RFC §6.11

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §1 (foreground/background unification under TaskID).** "Harbor unifies foreground and background under a single `TaskID` namespace — a key upgrade." Phase 20 ships this from t=0; foreground runs are tasks of `Kind = "foreground"`, background tasks are `Kind = "background"`. The split-trace_id-vs-task_id pattern that the predecessor accumulated is explicitly closed.
- **brief 05 §2 (data shapes).** Phase 20 implements `Task`, `TaskKind`, `TaskStatus`, `SpawnRequest`, `TaskHandle`, `TaskFilter`, `TaskSummary` per the brief shape; `TaskGroup`/`PatchAction`/etc. are stubbed with TODO comments tagging Phase 21.
- **brief 05 §4 (lifecycle state machine + idempotency + cancellation propagation).**
  - Lifecycle: `PENDING → RUNNING → COMPLETE`, with `PAUSED → RUNNING` (planner-initiated; durable via planner checkpoint at Phase 50), `FAILED | CANCELLED` terminal. Phase 20 enforces transitions; invalid transitions return `ErrInvalidTransition` (wrapped).
  - Idempotency: `Spawn` honors `IdempotencyKey` per `(SessionID, IdempotencyKey)` so a retried spawn returns the original `TaskHandle`.
  - Cancellation: `PropagateOnCancel ∈ {"cascade", "isolate"}`. Cascade: cancelling a parent cancels descendants; isolate: cancellation stays local.
- **brief 05 §5 (the predecessor's `StateStoreSessionAdapter` string-trick is unnecessary).** Tasks are first-class consumers of the StateStore generic surface (D-027). The `TaskRegistry.Save(t Task)` reduces to `state.Save(StateRecord{Identity: ..., Kind: "task.lifecycle", Bytes: marshal(t)})`.
- **brief 05 §6 (concurrency tests, isolation tests, idempotency tests).** Mandatory; covered under "Test plan."

## Findings I'm departing from (if any)

- **Brief 05 §7 phase decomposition recommends one phase for the full `TaskRegistry` (groups + tasks + patches + ack-background).** Harbor splits this across Phase 20 (per-task surface) and Phase 21 (groups + retain-turn + patches). Reason: per-task lifecycle is independently shippable and has zero dependencies on group governance; bundling the whole TaskService into one phase would slow the wave-end E2E + delay the per-task surface other phases (steering Phase 53, planner Phase 42) want as a stable foundation. Documented in `docs/decisions.md` as D-030 (settled in this same PR).

## Goals

- Single `internal/tasks/` package with `TaskRegistry` interface + sentinel errors + `Task` / `TaskID` / `TaskKind` / `TaskStatus` data shapes + driver-registry seam.
- One V1 driver under `internal/tasks/drivers/inprocess/` registered as `"inprocess"` via `init()`. Stores task state in memory + persists every state transition through `StateStore` (typed wrapper at this layer per D-027).
- Driver-registry seam (`Register` / `Open` / `OpenDriver` / `RegisteredDrivers`) modeled verbatim on `internal/state/registry.go`.
- Cross-package `conformancetest.Run(t, factory)` suite at `internal/tasks/conformancetest/` — same shape as Phase 07's StateStore conformance. The InProcess driver MUST pass; future durable drivers (post-V1 phase 87) inherit verbatim.
- Lifecycle state machine enforced at the driver: invalid transitions return `ErrInvalidTransition`. Documented FSM diagram lives in package godoc.
- Idempotency: `Spawn` keyed on `(SessionID, IdempotencyKey)` — same key returns same `TaskHandle` regardless of how many times Spawn is called.
- Cancellation propagation: `Cancel` walks the parent-task graph per `PropagateOnCancel`. Cascade-cancel emits one cancel event per descendant; isolate-cancel only touches the target.
- Identity-mandatory at the API boundary: empty tenant/user/session in `Identity` rejected with `ErrIdentityRequired`. RunID is task-scoped (the foreground run IS a task; the background task has its own ID).
- Concurrent-reuse contract enforced by the suite (D-025): N≥100 goroutines spawning, getting, cancelling tasks against a single shared `TaskRegistry` instance under `-race`.
- Bus integration: lifecycle transitions emit events on the `EventBus` — `task.spawned`, `task.started`, `task.paused`, `task.resumed`, `task.completed`, `task.failed`, `task.cancelled`. Each carries the full Quadruple.
- Audit-redaction integration: any payload that touches StateStore (`Task.Description`, `Task.Query`, `Task.Result`, `Task.Error`) goes through the `Redactor` BEFORE Save (caller-side redaction per D-020).

## Non-goals

- No `TaskGroup` / `SealGroup` / `CancelGroup` / `ApplyGroup` / `ListGroups` (Phase 21).
- No `ApplyPatch` / `AcknowledgeBackground` (Phase 21).
- No durable backend. The InProcess driver persists through StateStore; that's the durability boundary at V1. A queued backend (NATS, Postgres-as-queue) is post-V1 Phase 87.
- No retry-after-failure logic. The driver records the failure; the planner / supervisor decides whether to respawn (with a different `IdempotencyKey`).
- No automatic GC of completed tasks. Tasks remain in the registry until explicit `Delete` (post-V1) or process restart. (Phase 8 sessions GC will incidentally clear task records when a session is reaped.)
- No priority-based scheduling. `Task.Priority` is stored + retrievable; the registry does NOT preempt or reorder execution. Scheduling is the runtime engine's concern (Phase 10's worker pool).
- No remote spawn. `SpawnTool` for Phase 20 is in-process only. A2A-routed spawn lives in Phase 22 + Phase 29.

## Acceptance criteria

- [ ] `internal/tasks/tasks.go` defines:
  - `TaskID string`, `TaskKind string` (constants `KindForeground`, `KindBackground`), `TaskStatus string` (constants `StatusPending`, `StatusRunning`, `StatusPaused`, `StatusComplete`, `StatusFailed`, `StatusCancelled`).
  - `Task` struct per RFC §6.8 (minus the group/patch fields, which Phase 21 lays in).
  - `SpawnRequest`, `SpawnToolRequest`, `TaskHandle`, `TaskFilter`, `TaskSummary`, `TaskResult`, `TaskError` data shapes.
  - `TaskRegistry` interface: `Spawn`, `SpawnTool`, `Get`, `List`, `Cancel`, `Prioritize`, `MarkRunning`, `MarkPaused`, `MarkResumed`, `MarkComplete`, `MarkFailed`, `Close`. (The `Mark*` methods are how the runtime engine drives lifecycle transitions; Phase 21 will add `Mark*` group equivalents.)
  - Sentinel errors: `ErrNotFound`, `ErrInvalidTransition`, `ErrIdempotencyConflict`, `ErrIdentityRequired`, `ErrUnknownDriver`, `ErrRegistryClosed`.
  - `ValidateRequest(req SpawnRequest) error` exported helper for boundary validation.
- [ ] `internal/tasks/registry.go` provides `Register(name, factory)` / `Open(ctx, cfg)` / `OpenDriver(name, cfg)` / `RegisteredDrivers()` modeled on `internal/state/registry.go`.
- [ ] `internal/tasks/drivers/inprocess/inprocess.go`:
  - Backing storage: `map[TaskID]*Task` + `map[idempotencyKey]TaskID` + `map[TaskID][]TaskID` (children index for cascade-cancel). Single `sync.RWMutex`.
  - Each state transition writes through `StateStore.Save(StateRecord{Kind: "task.lifecycle", Bytes: marshal(task)})` and emits a typed `EventPayload` on the bus.
  - `Spawn` validates identity + checks `(SessionID, IdempotencyKey)` map. Same key → returns existing `TaskHandle`. New key → assigns ULID `TaskID`, persists, emits `task.spawned`, returns handle.
  - `Cancel` walks the children index per `PropagateOnCancel`. Cascade-cancel processes descendants in BFS order; isolate-cancel touches only the target. Each cancel emits `task.cancelled`.
  - `Prioritize` updates `Task.Priority` + persists; emits `task.prioritised` (new event type).
  - `MarkRunning/Paused/Resumed/Complete/Failed` perform the FSM check (`isValidTransition(from, to)`) before the write.
  - `Close` is idempotent; no goroutines to join.
- [ ] `internal/tasks/conformancetest/conformancetest.go` exports `Run(t, factory)`. Subtests cover:
  - `Spawn_AssignsTaskID` — fresh spawn returns a non-empty ULID `TaskID`.
  - `Spawn_Idempotent_SameKeyReturnsSameHandle` — second spawn with same `(SessionID, IdempotencyKey)` returns the original `TaskHandle`.
  - `Spawn_DifferentSessionsCanReuseKey` — same `IdempotencyKey` across two different sessions creates two distinct tasks (the key is namespaced by SessionID).
  - `Lifecycle_HappyPath` — Spawn → MarkRunning → MarkComplete; final `Get` returns `StatusComplete`.
  - `Lifecycle_PauseResume` — Spawn → Run → Pause → Resume → Complete; intermediate state observable via Get.
  - `Lifecycle_InvalidTransition_RejectsLoudly` — Pending → Complete (skipping Running) returns `ErrInvalidTransition`.
  - `Cancel_Cascade_PropagatesToChildren` — spawn parent + 3 children with `PropagateOnCancel: cascade`; cancel parent → all 4 transition to `StatusCancelled`.
  - `Cancel_Isolate_LeavesChildrenAlone` — spawn parent + 3 children with `PropagateOnCancel: isolate`; cancel parent → only parent transitions; children stay `Running`.
  - `Identity_Mandatory` — Spawn / Get / Cancel each reject empty tenant/user/session with `ErrIdentityRequired`.
  - `CrossTenant_Isolation` — task spawned under tenant A is NOT loadable / cancellable under tenant B.
  - `CrossSession_Isolation` — same shape, session axis.
  - `List_FiltersBySession` — `List(sessionID, filter)` returns only that session's tasks.
  - `Concurrent_SpawnGetCancel_NoRace` (D-025) — N≥128 goroutines mixing Spawn / Get / MarkRunning / Cancel under `-race`. No data races; baseline goroutine count restored.
  - `Close_Idempotent`.
  - `GoroutineLeak_AfterClose`.
- [ ] `internal/tasks/conformancetest/conformancetest_test.go` self-applies `Run` against the InProcess driver factory.
- [ ] `internal/tasks/drivers/inprocess/inprocess_test.go` runs `conformancetest.Run` against the InProcess driver.
- [ ] `internal/tasks/tasks_test.go` covers the registry surface + sentinel-error wrapping.
- [ ] **Bus events:** `internal/events/types.go` (Phase 05's payload registry) gains the seven task-lifecycle event types + their typed `EventPayload` shapes. Each `EventPayload` carries the relevant `TaskID`, prior status, new status; identity is on `Event.Identity`.
- [ ] `cmd/harbor/main.go` adds `_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"`.
- [ ] **Config additions:** `internal/config/config.go` gains `TasksConfig{ Driver string }` (defaults to `"inprocess"`); `loader.go::Default` populates; validator rejects empty driver.
- [ ] Coverage on `internal/tasks` ≥ 85%; `internal/tasks/drivers/inprocess` ≥ 90%.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-20.sh` present and executable.
- [ ] `docs/glossary.md` adds `TaskID`, `TaskKind`, `TaskStatus`, `TaskRegistry`, `IdempotencyKey`, `PropagateOnCancel` entries.
- [ ] `docs/decisions.md` adds **D-030** (Phase 20/21 split rationale).
- [ ] `docs/plans/README.md` Phase 20 row Status flips to Shipped.

## Files added or changed

- `internal/tasks/tasks.go` (new)
- `internal/tasks/registry.go` (new)
- `internal/tasks/tasks_test.go` (new)
- `internal/tasks/conformancetest/conformancetest.go` (new)
- `internal/tasks/conformancetest/conformancetest_test.go` (new)
- `internal/tasks/drivers/inprocess/inprocess.go` (new)
- `internal/tasks/drivers/inprocess/inprocess_test.go` (new)
- `internal/events/types.go` (modified) — register the seven `task.*` event types + typed payloads.
- `internal/events/payloads.go` (modified) — typed payload structs for the new events.
- `internal/config/config.go` (modified) — `TasksConfig` field added.
- `internal/config/loader.go` / `validate.go` (modified) — defaults + validation.
- `cmd/harbor/main.go` (modified) — additive blank import.
- `scripts/smoke/phase-20.sh` (new)
- `docs/plans/phase-20-tasks.md` (this file)
- `docs/plans/README.md` (modified)
- `docs/glossary.md` (modified)
- `docs/decisions.md` (modified) — D-030 entry
- `examples/harbor.yaml` (modified) — document `tasks.driver` field

`internal/tasks/` is enumerated in AGENTS.md §3.

## Public API surface

```go
package tasks

import (
    "context"
    "errors"
    "time"

    "github.com/hurtener/Harbor/internal/identity"
)

type TaskID string

type TaskKind string
const (
    KindForeground TaskKind = "foreground"
    KindBackground TaskKind = "background"
)

type TaskStatus string
const (
    StatusPending   TaskStatus = "pending"
    StatusRunning   TaskStatus = "running"
    StatusPaused    TaskStatus = "paused"
    StatusComplete  TaskStatus = "complete"
    StatusFailed    TaskStatus = "failed"
    StatusCancelled TaskStatus = "cancelled"
)

type Task struct {
    ID                TaskID
    Identity          identity.Identity
    Kind              TaskKind
    Status            TaskStatus
    Priority          int
    ParentTaskID      *TaskID
    Description       string
    Query             string
    Result            *TaskResult
    Error             *TaskError
    PropagateOnCancel string  // "cascade" | "isolate"
    NotifyOnComplete  bool
    CreatedAt         time.Time
    UpdatedAt         time.Time
    // Group fields reserved; populated by Phase 21.
}

type SpawnRequest struct {
    Identity          identity.Identity
    Kind              TaskKind
    ParentTaskID      *TaskID
    Description       string
    Query             string
    Priority          int
    IdempotencyKey    string  // (Identity.SessionID, IdempotencyKey) → TaskID
    PropagateOnCancel string  // "cascade" | "isolate"; default "cascade"
    NotifyOnComplete  bool
}

type TaskHandle struct {
    ID       TaskID
    Reused   bool  // true when an idempotency-key match returned an existing handle
}

type TaskFilter struct {
    Status   *TaskStatus
    Kind     *TaskKind
    ParentID *TaskID
}

type TaskSummary struct {
    ID        TaskID
    Status    TaskStatus
    Kind      TaskKind
    Priority  int
    UpdatedAt time.Time
}

type TaskResult struct {
    Value json.RawMessage  // pre-redacted by caller
}

type TaskError struct {
    Code    string
    Message string
}

type TaskRegistry interface {
    Spawn       (ctx context.Context, req SpawnRequest) (TaskHandle, error)
    SpawnTool   (ctx context.Context, req SpawnToolRequest) (TaskHandle, error)
    Get         (ctx context.Context, id TaskID) (*Task, error)
    List        (ctx context.Context, sessionID identity.SessionID, f TaskFilter) ([]TaskSummary, error)
    Cancel      (ctx context.Context, id TaskID, reason string) (bool, error)
    Prioritize  (ctx context.Context, id TaskID, priority int) (bool, error)

    // Lifecycle drive-points called by the runtime engine.
    MarkRunning  (ctx context.Context, id TaskID) error
    MarkPaused   (ctx context.Context, id TaskID) error
    MarkResumed  (ctx context.Context, id TaskID) error
    MarkComplete (ctx context.Context, id TaskID, result TaskResult) error
    MarkFailed   (ctx context.Context, id TaskID, err TaskError) error

    Close(ctx context.Context) error
}

var (
    ErrNotFound            = errors.New("tasks: task not found")
    ErrInvalidTransition   = errors.New("tasks: invalid lifecycle transition")
    ErrIdempotencyConflict = errors.New("tasks: idempotency key reused with divergent SpawnRequest")
    ErrIdentityRequired    = errors.New("tasks: identity required (tenant/user/session)")
    ErrUnknownDriver       = errors.New("tasks: unknown driver")
    ErrRegistryClosed      = errors.New("tasks: registry is closed")
)

type Factory func(deps Dependencies) (TaskRegistry, error)

type Dependencies struct {
    Store    state.StateStore
    Bus      events.EventBus
    Redactor audit.Redactor  // applied to Description/Query/Result before Save
    Cfg      config.TasksConfig
}

func Register(name string, factory Factory)
func Open(ctx context.Context, deps Dependencies) (TaskRegistry, error)
func OpenDriver(name string, deps Dependencies) (TaskRegistry, error)
func RegisteredDrivers() []string
```

`SpawnToolRequest` shape lifts from RFC §6.8 verbatim; specifies tool-name / tool-args / parent task / etc. Tool dispatch wiring happens at Phase 26+.

## Test plan

- **Unit:** `ValidateRequest` boundary checks; `isValidTransition(from, to)` for every pair (matrix test); registry sentinel-error wrapping; idempotency key normalisation.
- **Integration:** N/A in-package; the wave-end E2E (`test/integration/wave6_test.go`) wires tasks + state + bus + sessions + artifacts together to prove cross-subsystem composition.
- **Conformance:** `conformancetest.Run` is the load-bearing test surface. Subtests enumerated above.
- **Concurrency / leak (D-025):** `Concurrent_SpawnGetCancel_NoRace` is the canonical reusable-artifact test for `TaskRegistry`. N≥128 under `-race`; baseline restored.

## Smoke script additions

- `scripts/smoke/phase-20.sh`:
  - `go test -race -count=1 -timeout 90s ./internal/tasks/...` → OK on green.
  - `skip "phase 20: tasks have no HTTP/Protocol surface yet (lands in Phase 60+)"`.

## Coverage target

- `internal/tasks`: 85%.
- `internal/tasks/drivers/inprocess`: 90%.
- `internal/tasks/conformancetest`: not gated (precedent: Phase 07).

## Dependencies

- Phase 01 (identity).
- Phase 07 (StateStore) — durable lifecycle persistence via the typed wrapper.
- Phase 03 (audit redactor) — applied caller-side before Save.
- Phase 05 (events / bus) — lifecycle events emitted on the bus.

## Risks / open questions

- **`SpawnToolRequest` surface ahead of Phase 26.** The shape is defined here so the FSM can model `task.tool` lifecycle, but actual tool dispatch wiring lands at Phase 26. Phase 20's `SpawnTool` returns a `TaskHandle` whose execution is a no-op until the dispatcher is wired. Documented inline + flagged in the smoke script.
- **`ParentTaskID` pointer indirection.** Optional pointer is verbose at every call site; alternative is a sentinel `TaskID("")`. Sticking with pointer for explicitness — code reads "no parent" rather than "the empty parent."
- **Bus event flood under cascade-cancel.** Cancelling a parent with N descendants emits N+1 events. The bus's drop-oldest backpressure (Phase 05) handles bursts; subscribers that care about audit completeness should use the durable event log (Phase 57). Documented as a known characteristic, not a bug.
- **`PropagateOnCancel: "cascade"` default vs `"isolate"`.** Defaulting to cascade matches the predecessor's pattern + RFC §6.8's example; isolate is opt-in. Documented in `SpawnRequest` godoc.
- **`IdempotencyKey` empty-string semantics.** Empty key disables idempotency (every Spawn yields a fresh handle). Same-empty-key spawns DO NOT collide. Tested by the conformance suite.
- **No open RFC §11 questions block this phase.**

## Glossary additions

- **`TaskID`** — ULID-shaped identifier unifying foreground runs and background tasks. Single namespace; `TaskKind` distinguishes the two.
- **`TaskKind`** — `"foreground"` (a run inside a session's primary turn) or `"background"` (a spawned-without-blocking task).
- **`TaskStatus`** — lifecycle state. Values: `pending`, `running`, `paused`, `complete`, `failed`, `cancelled`. FSM enforced at the registry.
- **`TaskRegistry`** — the orchestration surface for spawning, listing, cancelling, prioritising, and driving the lifecycle FSM of tasks. One mandatory interface; one V1 driver (in-process); future durable backends post-V1.
- **`IdempotencyKey`** — caller-supplied string that, when paired with `SessionID`, deduplicates retried spawns. Same-key spawns return the original `TaskHandle`.
- **`PropagateOnCancel`** — `"cascade"` (cancellation walks descendants) or `"isolate"` (cancellation stays local). Default `"cascade"`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage targets met
- [ ] Multi-isolation: cross-tenant + cross-session conformance subtests pass
- [ ] **Concurrent-reuse test passes** — `Concurrent_SpawnGetCancel_NoRace` with N≥128 under `-race` (D-025).
- [ ] If new vocabulary: glossary updated (yes — listed above).
- [ ] D-030 entry filed in decisions.md (Phase 20/21 split rationale).
- [ ] If a brief finding was departed from: yes — split per D-030; documented in "Findings I'm departing from."
