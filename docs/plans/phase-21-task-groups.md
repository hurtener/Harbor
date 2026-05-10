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
- **Background-task continuation primitive (`WatchGroup`).** When `RetainTurn = false`, the foreground turn proceeds without blocking; the planner needs a way to come back when the group resolves so the work doesn't strand. This is the dual of `RegisterRetainTurnWaiter`: instead of *blocking the foreground turn*, it *registers a wake-up subscription* that fires when the group reaches a terminal state. Phase 21 ships the **mechanism**; the planner subsystem (Phase 42+) chooses the **policy** (push / poll / hybrid — see "Wake policy modes" below). The signature mirrors the retain-turn waiter for symmetry: returns a channel that closes (with the typed completion payload) when the group resolves; cancel func unsubscribes; close-once invariant.

### Wake policy modes — guidance for planner concretes (Phase 42+)

Phase 21 deliberately does NOT bake a `WakeMode` enum into `TaskRegistry` because the choice is a planner-concrete decision, not a registry concern. The three documented modes are:

1. **Push (LLM wake-on-resolution).** The planner subscribes via `WatchGroup`; the runtime engine consumes the closed channel as a wake event and re-invokes the planner with the typed `GroupCompletion` payload as input. Lowest latency, lowest cost — the planner sleeps until something actually happened. Suits long-running background work where intermediate progress isn't actionable.

2. **Poll (deterministic pull).** The planner periodically calls `Get(groupID)` (cheap; in-memory map lookup at the in-process driver) and returns to its main loop when status is terminal. No subscription required. Suits planners that need to interleave background-work checks with other deterministic work, or environments where push delivery isn't reliable.

3. **Hybrid (push for the planner; poll for a status sidecar).** The main planner subscribes via `WatchGroup` (push). A sidecar — typically a small / cheap LLM, or a deterministic templater — polls the group's intermediate state and emits user-visible progress updates between push events. The main planner only wakes when the group resolves; the user sees liveness in the meantime. Suits user-facing agents where silence between turn close and group resolution looks broken.

The contract is one mechanism (`WatchGroup` + `GroupCompletion` payload + the existing `Get(groupID)`); the planner picks the mode. Documented inline so future planner concretes (Phase 45 ReAct, Phase 48 Deterministic, future) wire the mode that fits.

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
  - `GroupCompletion` struct (the typed wake-up payload — see Public API surface).
  - `MemberOutcome` struct (per-member terminal state inside `GroupCompletion`).
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
WatchGroup(sessionID identity.SessionID, groupID TaskGroupID) (<-chan GroupCompletion, func() /* unsubscribe */, error)
```

- [ ] `internal/tasks/drivers/inprocess/groups.go` (new) implements the eight registry methods + the retain-turn waiter + the `WatchGroup` waker. Backing storage extends the existing in-process driver: `map[TaskGroupID]*TaskGroup` + `map[identity.SessionID]map[TaskGroupID]chan struct{}` (per-session retain-turn waiters) + `map[TaskGroupID][]chan GroupCompletion` (per-group completion subscribers; one entry per active `WatchGroup` call). Subscriptions cleared on resolve.
- [ ] `internal/tasks/drivers/inprocess/inprocess.go` (modified): when a member task transitions to a terminal state, the driver checks if the task belongs to a group; if so, AND the group is sealed AND all members are terminal → mark group `GroupCompleted` (or `GroupCancelled` if any member failed and `FailFast`); construct the typed `GroupCompletion` payload (member outcomes derived from each task's terminal `Result` / `Error`); close the per-session retain-turn waiter channel; deliver the payload to every `WatchGroup` subscriber (sending then closing each channel exactly once); emit `task.group_resolved` on the EventBus carrying the same payload.
- [ ] **Bus events:** seven new event types in `internal/events/types.go`: `task.group_created`, `task.group_sealed`, `task.group_resolved`, `task.group_cancelled`, `task.patch_applied`, `task.patch_rejected`, `task.background_acknowledged`. Each gets a typed `EventPayload`. **`task.group_resolved`'s payload IS `GroupCompletion`** so subscribers (planner runtime, sidecar status emitters, durable event log, Console) consume the same typed shape regardless of how they're wired.
- [ ] **Config additions:** `TasksConfig` gains `RetainTurnTimeout time.Duration` (default `5 * time.Minute`) and `ContinuationHopLimit int` (default `8`).
- [ ] `internal/tasks/conformancetest/groups_test.go` (new subtests added to the existing `Run` suite):
  - `Group_ResolveOrCreate_Idempotent` — same `(SessionID, GroupID)` returns same group.
  - `Group_Seal_FreezesMembership` — Seal → SpawnTool with `GroupID` returns `ErrGroupSealed`.
  - `Group_RetainTurn_BlocksUntilTerminal` — register waiter; spawn 3 children; mark each running then complete; observe waiter chan unblocks at the third Complete.
  - `Group_FailFast_OnFirstFailure_CancelsRest` — group with `FailFast: true` + 3 members; mark first Failed → driver cancels remaining 2; group transitions to `GroupCancelled`.
  - `Group_Cancel_Cascade_PropagatesToMembers` — `CancelGroup(propagate=true)` cancels all member tasks.
  - `Group_Cancel_NoPropagate_LeavesMembersAlone` — `CancelGroup(propagate=false)` only marks the group; member tasks keep running.
  - `WatchGroup_Push_DeliversCompletionPayload` — register `WatchGroup`; spawn 3 children; mark each running then complete; observe ONE `GroupCompletion` delivery on the channel with all three `MemberOutcome` entries populated and `FinalStatus=GroupCompleted`. Channel closes after delivery (close-once invariant).
  - `WatchGroup_Push_OnGroupCancelled_DeliversWithReason` — register watcher; `CancelGroup(propagate=true, reason="user-cancelled")`; observe `GroupCompletion{FinalStatus: GroupCancelled, Reason: "user-cancelled"}` delivered before channel close.
  - `WatchGroup_Poll_GetReturnsTerminalAfterResolve` — DO NOT register a watcher; spawn group; mark all members terminal; assert `Get(groupID)` returns terminal status (proves the deterministic poll mode works against the same registry surface, no extra primitives needed).
  - `WatchGroup_Hybrid_PushAndPollCoexist` — register `WatchGroup` AND poll `Get(groupID)` from a separate goroutine (~10ms cadence); mark members terminal; assert BOTH the push channel delivery AND the polling goroutine see terminal status. No subscriptions interfere with each other.
  - `WatchGroup_Unsubscribe_BeforeResolve_NoLeak` — register watcher; call cancel func; mark group terminal; assert NO send on the unsubscribed channel (verified via panic-on-send-to-closed-channel guard) and the channel is closed by the cancel func itself, not by the resolve path.
  - `WatchGroup_AlreadyResolvedGroup_ReturnsErrGroupNotFound` — call `WatchGroup` on a group that resolved AND was GC'd; returns `ErrGroupNotFound`. Calling on a *resolved-but-still-tracked* group MUST return a channel that is already-closed with the cached `GroupCompletion` (so late subscribers don't deadlock).
  - `WatchGroup_MultipleSubscribers_AllReceive` — N=4 concurrent `WatchGroup` calls on the same group; resolve; all 4 channels receive the same `GroupCompletion` payload.
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

// GroupCompletion is the typed payload delivered when a group reaches
// a terminal state. It is the wake-up signal the planner runtime
// (Phase 42+) consumes when a non-retain-turn group resolves; the
// same payload is used as the `task.group_resolved` bus-event payload
// so subscribers (Console, durable-event-log, sidecar status emitters)
// see one canonical shape regardless of how they're wired.
//
// `Members` carries one entry per group member with the member's
// terminal status + result/error. Heavy results MUST be substituted
// with `ArtifactRef`s upstream (D-022, D-026); GroupCompletion is
// payload-shaped, not byte-bound.
type GroupCompletion struct {
    GroupID     TaskGroupID
    SessionID   identity.SessionID
    OwnerTaskID TaskID
    FinalStatus TaskGroupStatus  // GroupCompleted or GroupCancelled
    ResolvedAt  time.Time
    Members     []MemberOutcome
    Reason      string  // populated for GroupCancelled (cancel reason); empty otherwise
}

// MemberOutcome is the per-task terminal record carried inside
// GroupCompletion. Either Result or Error is populated (never both);
// neither is populated when Status == StatusCancelled.
type MemberOutcome struct {
    TaskID TaskID
    Status TaskStatus       // StatusComplete | StatusFailed | StatusCancelled
    Result *TaskResult      // populated when Status == StatusComplete
    Error  *TaskError       // populated when Status == StatusFailed
}

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

    // WatchGroup is the non-retain-turn dual: it does NOT block any
    // foreground turn — the planner is free to proceed while the
    // group runs in the background. When the group reaches a terminal
    // state, the runtime delivers a typed `GroupCompletion` payload
    // on the returned channel and closes the channel. Callers
    // typically use this as a "wake the planner" signal so background
    // results integrate back into the conversation; see
    // "Wake policy modes" in the plan goals for the three patterns
    // (push, poll, hybrid) the planner runtime can implement against
    // this single mechanism.
    //
    // Returns ErrGroupNotFound when the group is unknown at
    // registration time (e.g. resolved + GC'd). For a
    // resolved-but-still-tracked group, the implementation returns
    // a channel that is *already* primed with the cached
    // `GroupCompletion` (so late subscribers don't deadlock).
    //
    // The cancel func unsubscribes; calling it after a delivery
    // is a no-op. The channel is closed exactly once — either by
    // the resolve path (with a delivery) or by the cancel path
    // (without a delivery).
    //
    // Concurrent reuse: multiple subscribers on the same group all
    // receive the same payload (D-025).
    WatchGroup(sessionID identity.SessionID, groupID TaskGroupID) (<-chan GroupCompletion, func(), error)
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
- **`WatchGroup` channel buffering.** Each subscriber's channel is buffered size 1 so the resolve path delivers without blocking even if a slow consumer hasn't received yet. The buffered slot holds the cached payload until the subscriber reads OR the cancel func fires (which drains + closes). Tested by `WatchGroup_AlreadyResolvedGroup_ReturnsErrGroupNotFound` (the late-subscriber case) and `WatchGroup_Unsubscribe_BeforeResolve_NoLeak`.
- **`WatchGroup` payload size.** `GroupCompletion.Members` carries every member's `TaskResult`. For groups with N≥10 members and bytes-shaped results, the payload can grow large. The discipline: producers (tools, sub-tasks) must already be substituting heavy outputs with `ArtifactRef`s upstream (D-026); `MemberOutcome.Result` carries refs, not bytes. The conformance suite's `WatchGroup_Push_DeliversCompletionPayload` verifies that a member result above the heavy-output threshold appears as an ArtifactRef in the payload, not as inline bytes.
- **Wake-policy choice belongs at the planner.** Phase 21 deliberately does NOT bake `WakeMode` into the registry. A future planner concrete (Phase 45 ReAct, Phase 48 Deterministic, future) selects push / poll / hybrid based on its own constraints. If a future implementation needs registry-side mode hints, that's an extension PR.
- **No open RFC §11 questions block this phase.**

## Glossary additions

- **`TaskGroup`** — a sealed-or-open collection of tasks tracked as a unit for parallel-fan-out / retain-turn / aggregate-cancel semantics. Members spawn into the group; sealing freezes membership; resolving fires when all members reach terminal states.
- **`TaskGroupID`** — ULID-shaped identifier for a `TaskGroup`.
- **`TaskGroupStatus`** — group lifecycle state. Values: `open`, `sealed`, `completed`, `cancelled`.
- **`RetainTurn`** — group-level flag; when true, the owning session blocks foreground-turn dispatch until the group reaches terminal.
- **`FailFast`** — group-level flag; when true, the first member failure cancels remaining members + transitions the group to `cancelled`.
- **`ApplyPatch`** — registry action for accepting or rejecting a pending context patch (proposed by a planner / human reviewer). Patches transition `pending → applied | rejected` through the registry.
- **`AcknowledgeBackground`** — registry action marking a list of completed background tasks as user-acknowledged. Emits per-task `task.background_acknowledged` events.
- **`WatchGroup`** — non-blocking dual of `RegisterRetainTurnWaiter`. Returns a channel that delivers a typed `GroupCompletion` payload when the group resolves; the planner runtime consumes the delivery as a wake-up signal so background-task results integrate back into the conversation without manual polling. The mechanism for the three documented wake modes (push / poll / hybrid) — the planner picks the policy.
- **`GroupCompletion`** — typed wake-up payload delivered by `WatchGroup` (and as the `task.group_resolved` bus-event payload). Carries the group's terminal status, resolve timestamp, cancel reason (if cancelled), and a `MemberOutcome` per group member.
- **`MemberOutcome`** — per-task entry inside `GroupCompletion`. Carries `TaskID`, terminal status, and either `Result` (when complete) or `Error` (when failed). Heavy results are substituted with `ArtifactRef`s upstream (D-022, D-026); the payload is ref-shaped, not byte-bound.
- **Push wake (background continuation)** — wake mode where the planner subscribes via `WatchGroup`; the runtime delivers a `GroupCompletion` payload at resolve time; the planner re-enters with the payload as input. Lowest latency; suits long-running background work.
- **Poll wake (background continuation)** — wake mode where the planner periodically calls `Get(groupID)` until status is terminal. No subscription required; suits planners interleaving multiple deterministic checks.
- **Hybrid wake (background continuation)** — wake mode where the main planner subscribes (push) AND a sidecar (typically a small / cheap LLM, or a deterministic templater) emits user-visible status updates between push events. Suits user-facing agents where silence between turn-close and group-resolve looks broken.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Coverage targets met
- [ ] Multi-isolation: `Group_CrossSession_Isolation` passes
- [ ] **Concurrent-reuse test passes** — `Group_Concurrent_AddRemoveSeal_NoRace` with N≥64 under `-race` (D-025).
- [ ] If new vocabulary: glossary updated (yes — listed above).
- [ ] If a brief finding was departed from: N/A.
