# Phase 21 — TaskGroup + retain-turn + patches

## Summary

Land the second half of the Tasks subsystem on top of Phase 20: `TaskGroup` resolution / sealing / cancel / apply, retain-turn semantics that block the foreground turn until a group of background tasks completes, the `ApplyPatch` lifecycle for human-approved context patches, and `AcknowledgeBackground` for the ambient stream of background-completion notifications. Per D-030, this ships separately from Phase 20's per-task surface so the two halves of `TaskRegistry` can be implemented + verified independently — but they live in the same package and reuse the same driver.

## RFC anchor

- RFC §6.8
- RFC §3.5
- RFC §4
- RFC §6.11

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §1 (group governance is part of the predecessor's TaskService surface).** "TaskService's interface mixes orchestration and group governance. A single `TaskService` protocol carries 14 methods covering spawn, list, get, cancel, prioritize, patches, groups, and tool-jobs. Harbor keeps the surface but groups it into named method sets in the Go interface for navigability, and lifts groups into a sibling interface (`TaskGroupRegistry`) once the surface stabilizes." Phase 21 implements this directly: the V1 surface lives on the same `TaskRegistry` interface (one consumer-facing seam), but the implementation is split into `tasks.go` (Phase 20) and `groups.go` (Phase 21) inside the package for navigability. The "lift to a sibling interface" sub-decision waits for V1.5 evidence.
- **brief 05 §2 (data shapes — TaskGroup / GroupRequest / GroupAction / PatchAction / TaskGroupID).** Phase 21 implements all four verbatim.
- **brief 05 §4 (retain-turn semantics).** "Group sealing freezes membership; `retain_turn` blocks the foreground until the group completes (no `HUMAN_GATED` interaction in retain mode)." Phase 21 enforces sealing AND retain-turn at the registry layer; the runtime engine reads `Group.RetainTurn` and pauses its foreground turn dispatch until the group reaches a terminal state.
- **brief 05 §5 ("backgroundtasks-config knobs (timeouts, continuation hops)").** Resolved by RFC §6.8: per-session config with per-spawn override via `SpawnRequest`. Phase 21 plumbs the per-session and per-spawn defaults; the runtime engine consumes them.
- **brief 05 §6 (group lifecycle property tests).** Mandatory; covered under "Test plan."

## Findings I'm departing from (if any)

- None.

## Goals

- Extend `internal/tasks/` with `TaskGroup`, `TaskGroupID`, `TaskGroupStatus`, `GroupRequest`, `GroupAction`, `PatchAction` data shapes + the seven group/patch methods on the existing `TaskRegistry` interface.
- Group lifecycle FSM: `OPEN → SEALED → COMPLETED | CANCELLED` (FAILED is implicit when ≥1 child task fails AND `FailFast` is set on the group). Sealed-but-not-terminal is the "all members spawned, waiting for results" state; sealed groups CANNOT accept new members.
- Retain-turn semantics: `Group.RetainTurn = true` means the runtime engine MUST NOT dispatch new foreground turns from the owning session until the group reaches a terminal state. The registry surfaces this via `RegisterRetainTurnWaiter` — a hook the runtime engine subscribes to.
- Patch governance: `ApplyPatch(sessionID, patchID, action)` transitions a pending patch through `pending → applied | rejected`. Patches are persisted via StateStore (typed wrapper, D-027).
- `AcknowledgeBackground(sessionID, taskIDs)` marks a list of completed background tasks as user-acknowledged; emits one `task.background_acknowledged` event per task.
- All conformance scenarios for groups + patches added to the existing `internal/tasks/conformancetest/` suite. The suite remains the gate for downstream durable drivers.
- Per-session backgroundtasks-config knobs land on `config.TasksConfig`: `RetainTurnTimeout`, `ContinuationHopLimit`. Defaults from RFC §6.8.

## Non-goals

- No durable group store. Groups persist through StateStore (the same channel as tasks); a queued group backend is post-V1.
- No automatic GC of completed groups. Same as Phase 20's task-GC stance — tasks-and-groups remain until the session GC sweeps them.
- No cross-session groups. A group's members must all share the same `(SessionID)`. Cross-session orchestration is a Phase 22 RemoteTransport concern.
- No priority within a group. Group members are equal peers; the runtime decides their dispatch order.
- No nested groups (groups containing other groups). V1 keeps the structure flat; nesting can land if a planner concrete genuinely needs it.
- No history-of-patches API. `ApplyPatch` records the action; querying historical patches goes through the durable event log (Phase 57).

## Acceptance criteria

- [ ] `internal/tasks/groups.go` (new) defines:
  - `TaskGroupID string` (ULID).
  - `TaskGroupStatus string` constants: `GroupOpen`, `GroupSealed`, `GroupCompleted`, `GroupCancelled`.
  - `TaskGroup` struct: `ID, SessionID, OwnerTaskID, Status, RetainTurn, FailFast, Members []TaskID, CreatedAt, UpdatedAt, ResolvedAt, Description`.
  - `GroupRequest`, `GroupAction` (the action a caller wants to apply: `actionSeal`, `actionCancel`, `actionResolve`).
  - `PatchAction string` constants: `PatchAccept`, `PatchReject`.
  - Sentinel errors: `ErrGroupNotFound`, `ErrGroupSealed` (mutation attempted on a sealed group), `ErrGroupNotSealed` (resolve attempted on an open group), `ErrGroupInvalidTransition`, `ErrPatchNotFound`.
- [ ] `internal/tasks/tasks.go` (modified): the `TaskRegistry` interface gains the seven methods + a retain-turn waiter shown below.

```go
ResolveOrCreateGroup(ctx context.Context, req GroupRequest) (*TaskGroup, error)
SealGroup           (ctx context.Context, id TaskGroupID) error
CancelGroup         (ctx context.Context, id TaskGroupID, reason string, propagate bool) error
ApplyGroup          (ctx context.Context, id TaskGroupID, action GroupAction) error
ListGroups          (ctx context.Context, sessionID identity.SessionID, status *TaskGroupStatus) ([]TaskGroup, error)
ApplyPatch          (ctx context.Context, sessionID identity.SessionID, patchID string, action PatchAction) (bool, error)
AcknowledgeBackground(ctx context.Context, sessionID identity.SessionID, ids []TaskID) (int, error)
RegisterRetainTurnWaiter(sessionID identity.SessionID) (<-chan TaskGroupID, func() /* unsubscribe */)
```

- [ ] `internal/tasks/drivers/inprocess/groups.go` (new) implements the seven methods + the retain-turn waiter mechanism. Backing storage extends the existing in-process driver: `map[TaskGroupID]*TaskGroup` + `map[identity.SessionID]map[TaskGroupID]chan struct{}` (per-session retain-turn waiters).
- [ ] `internal/tasks/drivers/inprocess/inprocess.go` (modified): when a member task transitions to a terminal state, the driver checks if the task belongs to a group; if so, AND the group is sealed AND all members are terminal → mark group `GroupCompleted` (or `GroupCancelled` if any member failed and `FailFast`); close the per-session retain-turn waiter channel; emit `group.resolved`.
- [ ] **Bus events:** seven new event types in `internal/events/types.go`: `task.group_created`, `task.group_sealed`, `task.group_resolved`, `task.group_cancelled`, `task.patch_applied`, `task.patch_rejected`, `task.background_acknowledged`. Each gets a typed `EventPayload`.
- [ ] **Config additions:** `TasksConfig` gains `RetainTurnTimeout time.Duration` (default `5 * time.Minute`) and `ContinuationHopLimit int` (default `8`).
- [ ] `internal/tasks/conformancetest/groups_test.go` (new subtests added to the existing `Run` suite):
  - `Group_ResolveOrCreate_Idempotent` — same `(SessionID, GroupID)` returns same group.
  - `Group_Seal_FreezesMembership` — Seal → SpawnTool with `GroupID` returns `ErrGroupSealed`.
  - `Group_RetainTurn_BlocksUntilTerminal` — register waiter; spawn 3 children; mark each running then complete; observe waiter chan unblocks at the third Complete.
  - `Group_FailFast_OnFirstFailure_CancelsRest` — group with `FailFast: true` + 3 members; mark first Failed → driver cancels remaining 2; group transitions to `GroupCancelled`.
  - `Group_Cancel_Cascade_PropagatesToMembers` — `CancelGroup(propagate=true)` cancels all member tasks.
  - `Group_Cancel_NoPropagate_LeavesMembersAlone` — `CancelGroup(propagate=false)` only marks the group; member tasks keep running.
  - `Patch_Apply_HappyPath` — `ApplyPatch(action=PatchAccept)` returns `(true, nil)`; emits `task.patch_applied`.
  - `Patch_Apply_Reject_HappyPath` — `ApplyPatch(action=PatchReject)`.
  - `Patch_Apply_NotFound` — wrong patchID returns `ErrPatchNotFound`.
  - `Acknowledge_Background_EmitsPerTaskEvents` — N background tasks → N `task.background_acknowledged` events.
  - `Group_CrossSession_Isolation` — a group spawned in session A is NOT visible / actionable from session B.
  - `Group_Concurrent_AddRemoveSeal_NoRace` (D-025) — N≥64 concurrent group operations under `-race`. No data races; baseline restored.
- [ ] Coverage on `internal/tasks` ≥ 85%; `internal/tasks/drivers/inprocess` ≥ 90% (post-Phase-21 numbers).
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-21.sh` updated (this phase ships its smoke as a separate file from Phase 20's; the phase-NN convention is one smoke per phase, even when phases share a package).
- [ ] `docs/glossary.md` adds `TaskGroup`, `TaskGroupID`, `TaskGroupStatus`, `RetainTurn`, `FailFast`, `ApplyPatch`, `AcknowledgeBackground` entries.
- [ ] `docs/plans/README.md` Phase 21 row Status flips to Shipped.

## Files added or changed

- `internal/tasks/groups.go` (new) — group + patch data shapes, sentinel errors.
- `internal/tasks/groups_test.go` (new).
- `internal/tasks/tasks.go` (modified) — `TaskRegistry` interface gains the seven methods + the retain-turn waiter.
- `internal/tasks/drivers/inprocess/groups.go` (new) — in-process driver implementations.
- `internal/tasks/drivers/inprocess/inprocess.go` (modified) — terminal-transition wiring fires group resolution.
- `internal/tasks/conformancetest/conformancetest.go` (modified) — adds subtests listed above.
- `internal/events/types.go` (modified) — register the seven `task.group_*` / `task.patch_*` / `task.background_acknowledged` event types.
- `internal/events/payloads.go` (modified) — typed payloads.
- `internal/config/config.go` (modified) — `TasksConfig` gains `RetainTurnTimeout`, `ContinuationHopLimit`.
- `internal/config/loader.go` / `validate.go` (modified) — defaults + validation.
- `scripts/smoke/phase-21.sh` (new)
- `docs/plans/phase-21-task-groups.md` (this file)
- `docs/plans/README.md` (modified)
- `docs/glossary.md` (modified)
- `examples/harbor.yaml` (modified) — document the new TasksConfig fields

No top-level directory additions.

## Public API surface

```go
// internal/tasks/groups.go

package tasks

type TaskGroupID string

type TaskGroupStatus string
const (
    GroupOpen      TaskGroupStatus = "open"
    GroupSealed    TaskGroupStatus = "sealed"
    GroupCompleted TaskGroupStatus = "completed"
    GroupCancelled TaskGroupStatus = "cancelled"
)

type TaskGroup struct {
    ID           TaskGroupID
    SessionID    identity.SessionID
    OwnerTaskID  TaskID
    Status       TaskGroupStatus
    RetainTurn   bool
    FailFast     bool
    Members      []TaskID
    Description  string
    CreatedAt    time.Time
    UpdatedAt    time.Time
    ResolvedAt   *time.Time
}

type GroupRequest struct {
    ID           TaskGroupID  // optional; empty → assign new ULID
    SessionID    identity.SessionID
    OwnerTaskID  TaskID
    RetainTurn   bool
    FailFast     bool
    Description  string
}

type GroupAction string
const (
    ActionSeal    GroupAction = "seal"
    ActionCancel  GroupAction = "cancel"
    ActionResolve GroupAction = "resolve"  // mark sealed → completed when caller knows all members are done
)

type PatchAction string
const (
    PatchAccept PatchAction = "accept"
    PatchReject PatchAction = "reject"
)

var (
    ErrGroupNotFound          = errors.New("tasks: group not found")
    ErrGroupSealed            = errors.New("tasks: group is sealed; cannot mutate membership")
    ErrGroupNotSealed         = errors.New("tasks: group must be sealed before resolve")
    ErrGroupInvalidTransition = errors.New("tasks: invalid group transition")
    ErrPatchNotFound          = errors.New("tasks: patch not found")
)
```

```go
// TaskRegistry interface (extended)

type TaskRegistry interface {
    // ... Phase 20 methods ...

    ResolveOrCreateGroup(ctx context.Context, req GroupRequest) (*TaskGroup, error)
    SealGroup           (ctx context.Context, id TaskGroupID) error
    CancelGroup         (ctx context.Context, id TaskGroupID, reason string, propagate bool) error
    ApplyGroup          (ctx context.Context, id TaskGroupID, action GroupAction) error
    ListGroups          (ctx context.Context, sessionID identity.SessionID, status *TaskGroupStatus) ([]TaskGroup, error)
    ApplyPatch          (ctx context.Context, sessionID identity.SessionID, patchID string, action PatchAction) (bool, error)
    AcknowledgeBackground(ctx context.Context, sessionID identity.SessionID, ids []TaskID) (int, error)

    // RegisterRetainTurnWaiter returns a channel that closes when the
    // session's earliest-active retain-turn group resolves. The
    // returned cancel func unsubscribes; the channel may also close
    // because the group went terminal — callers MUST be tolerant of
    // both reasons.
    //
    // Implementations are required to close the channel exactly once.
    RegisterRetainTurnWaiter(sessionID identity.SessionID) (<-chan TaskGroupID, func())
}
```

## Test plan

- **Unit:** group sentinel-error wrapping; FSM transition matrix (`Open → Sealed`, `Sealed → Completed`, `Sealed → Cancelled`, `Open → Cancelled`, invalid pairs reject); retain-turn waiter channel close-once invariant; backgroundtasks-config defaults.
- **Integration:** wave-end E2E (`test/integration/wave6_test.go`) exercises group + retain-turn against the runtime engine + sessions + state to prove the cross-subsystem composition.
- **Conformance:** subtests added to `internal/tasks/conformancetest/`; the existing per-task suite from Phase 20 still runs; the new group/patch suite runs alongside it. Both are the gate.
- **Concurrency / leak (D-025):** `Group_Concurrent_AddRemoveSeal_NoRace` covers concurrent group manipulation. The Phase 20 `Concurrent_SpawnGetCancel_NoRace` still runs; the suite remains the gate for both halves.

## Smoke script additions

- `scripts/smoke/phase-21.sh`:
  - `go test -race -count=1 -timeout 90s ./internal/tasks/...` → OK on green (covers both Phase 20 + 21 subtests in the same package).
  - `skip "phase 21: task groups have no HTTP/Protocol surface yet (lands in Phase 60+)"`.

## Coverage target

- `internal/tasks`: 85%.
- `internal/tasks/drivers/inprocess`: 90%.

## Dependencies

- Phase 20 (TaskRegistry per-task surface). The two phases live in the same package; Phase 21 cannot ship before Phase 20 lands.

## Risks / open questions

- **Retain-turn waiter back-pressure.** A session with N concurrent retain-turn groups has N waiters. Channel close-on-resolve is fast; subscribers (the runtime engine) MUST consume promptly to avoid blocking. Documented + the channel is buffered size 1 just in case.
- **Group sealing race.** Two concurrent `SealGroup` calls on the same group: the second returns `ErrGroupInvalidTransition` (already sealed). Tested by the conformance suite.
- **`FailFast` semantics under cascade-cancel.** If one member fails AND `FailFast` is on, the driver cancels remaining members. Each cancel cascades to that member's children per Phase 20's `PropagateOnCancel`. Documented.
- **Patch storage.** Patches are persisted via StateStore under `Kind = "task.patch"`. The patch payload is opaque bytes (the actual context-patch shape lives at the planner, Phase 42+). Phase 21 stores + retrieves; the planner consumes.
- **No open RFC §11 questions block this phase.**

## Glossary additions

- **`TaskGroup`** — a sealed-or-open collection of tasks tracked as a unit for parallel-fan-out / retain-turn / aggregate-cancel semantics. Members spawn into the group; sealing freezes membership; resolving fires when all members reach terminal states.
- **`TaskGroupID`** — ULID-shaped identifier for a `TaskGroup`.
- **`TaskGroupStatus`** — group lifecycle state. Values: `open`, `sealed`, `completed`, `cancelled`.
- **`RetainTurn`** — group-level flag; when true, the owning session blocks foreground-turn dispatch until the group reaches terminal.
- **`FailFast`** — group-level flag; when true, the first member failure cancels remaining members + transitions the group to `cancelled`.
- **`ApplyPatch`** — registry action for accepting or rejecting a pending context patch (proposed by a planner / human reviewer). Patches transition `pending → applied | rejected` through the registry.
- **`AcknowledgeBackground`** — registry action marking a list of completed background tasks as user-acknowledged. Emits per-task `task.background_acknowledged` events.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Coverage targets met
- [ ] Multi-isolation: `Group_CrossSession_Isolation` passes
- [ ] **Concurrent-reuse test passes** — `Group_Concurrent_AddRemoveSeal_NoRace` with N≥64 under `-race` (D-025).
- [ ] If new vocabulary: glossary updated (yes — listed above).
- [ ] If a brief finding was departed from: N/A.
