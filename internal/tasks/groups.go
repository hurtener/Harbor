package tasks

// Group + patch governance shapes. Phase 21 lays groups, retain-turn
// semantics, the `WatchGroup` wake mechanism, and patch governance on
// top of the Phase 20 per-task surface (D-030). The shapes live in
// this file for navigability (brief 05 §1) while sharing the
// `TaskRegistry` interface seam.
//
// # Wake policy modes (planner-concrete concern, NOT a registry knob)
//
// The runtime ships ONE mechanism — `WatchGroup` + `GroupCompletion`
// payload + the per-task `Get(taskID)` / per-group `ListGroups`
// surface — and documents three patterns the planner runtime
// (Phase 42+) can implement on top of it:
//
//  1. Push (LLM wake-on-resolution). The planner subscribes via
//     `WatchGroup`; the runtime engine consumes the closed channel as
//     a wake event and re-invokes the planner with the typed
//     `GroupCompletion` payload as input. Lowest latency, lowest cost
//     — the planner sleeps until something actually happened. Suits
//     long-running background work where intermediate progress isn't
//     actionable.
//
//  2. Poll (deterministic pull). The planner periodically calls
//     `ListGroups` / `Get(taskID)` (cheap; in-memory map lookup at
//     the in-process driver) and returns to its main loop when the
//     group's `Status` is terminal. No subscription required. Suits
//     planners interleaving background-work checks with other
//     deterministic work, or environments where push delivery isn't
//     reliable.
//
//  3. Hybrid (push for the planner; poll for a status sidecar). The
//     main planner subscribes via `WatchGroup` (push). A sidecar —
//     typically a small / cheap LLM, or a deterministic templater —
//     polls the group's intermediate state and emits user-visible
//     progress updates between push events. The main planner only
//     wakes when the group resolves; the user sees liveness in the
//     meantime. Suits user-facing agents where silence between turn
//     close and group resolution looks broken.
//
// Phase 21 deliberately does NOT bake a `WakeMode` enum into
// `TaskRegistry` — the choice is a planner-concrete decision, not a
// registry concern. Future planner concretes (Phase 45 ReAct,
// Phase 48 Deterministic, future) select push / poll / hybrid based
// on their own constraints.

import (
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// PatchKind is the StateStore Kind constant for patch records. Phase
// 21 persists pending / applied / rejected patch state through the
// same typed-wrapper-over-generic adapter the per-task surface uses
// (D-027).
const PatchKind = "task.patch"

// GroupKind is the StateStore Kind constant for group lifecycle
// records.
const GroupKind = "task.group"

// TaskGroupID is the ULID-shaped identifier for a `TaskGroup`. The
// caller MAY pre-assign in `GroupRequest.ID` (idempotency); empty →
// the registry assigns a fresh ULID.
type TaskGroupID string

// TaskGroupStatus is the group lifecycle state. The FSM lives at the
// driver:
//
//	Open ──Seal──▶ Sealed ──(all members terminal)──▶ Completed
//	  │                │
//	  │                └──Cancel──▶ Cancelled
//	  └──Cancel──▶ Cancelled (valid from Open too)
//
// `Completed` and `Cancelled` are terminal. Invalid transitions
// return `ErrGroupInvalidTransition`.
type TaskGroupStatus string

// Group statuses.
const (
	// GroupOpen is the initial state — members may be added.
	GroupOpen TaskGroupStatus = "open"
	// GroupSealed freezes membership; the group still has non-terminal
	// members. Sealed groups CANNOT accept new members
	// (`SpawnTool` / `Spawn` carrying a `GroupID` for a sealed group
	// returns `ErrGroupSealed`).
	GroupSealed TaskGroupStatus = "sealed"
	// GroupCompleted is the terminal-success state — the group was
	// sealed AND every member reached terminal state without
	// triggering a `FailFast` cancellation.
	GroupCompleted TaskGroupStatus = "completed"
	// GroupCancelled is the terminal-failure state — the group was
	// cancelled by `CancelGroup`, or `FailFast` was true and a member
	// transitioned to `StatusFailed`.
	GroupCancelled TaskGroupStatus = "cancelled"
)

// TaskGroup is the persisted group record. The Identity is captured
// from the owning session at create time; cross-session group
// membership is forbidden (V1; nesting / cross-session lands post-V1
// if a planner concrete genuinely needs it — brief 05).
//
// `Members` is the in-order list of member task IDs assigned during
// the `Open` phase via the spawn-with-GroupID seam (Phase 26+ wires
// `SpawnTool`'s group hookup). Phase 21 ships the group surface; the
// member-assignment wiring is exercised by the conformance suite
// directly through the new `AddMember` driver helper.
//
// `RetainTurn` is the foreground-blocking flag — when true, the
// owning session blocks foreground-turn dispatch until the group
// reaches a terminal state.
//
// `FailFast` cancels remaining members when the first member fails.
// `ResolvedAt` is non-nil only on terminal status.
type TaskGroup struct {
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ResolvedAt  *time.Time
	SessionID   identity.Identity
	ID          TaskGroupID
	OwnerTaskID TaskID
	Status      TaskGroupStatus
	Description string
	Members     []TaskID
	RetainTurn  bool
	FailFast    bool
}

// GroupRequest is the input shape for `ResolveOrCreateGroup`.
// `ID` is optional — empty → the registry assigns a fresh ULID and
// returns the new group; non-empty + already-existing → the existing
// group is returned (idempotency).
//
// Identity is mandatory.
type GroupRequest struct {
	SessionID   identity.Identity
	ID          TaskGroupID
	OwnerTaskID TaskID
	Description string
	RetainTurn  bool
	FailFast    bool
}

// GroupAction is the verb a caller passes to `ApplyGroup`.
type GroupAction string

// Group actions.
const (
	// ActionSeal seals an open group, freezing membership. Equivalent
	// to `SealGroup`. Idempotent on an already-sealed group's no-op
	// path; invalid on a terminal group (returns
	// `ErrGroupInvalidTransition`).
	ActionSeal GroupAction = "seal"
	// ActionCancel cancels a non-terminal group (propagate = true).
	// Equivalent to `CancelGroup(reason="action:cancel", propagate=true)`.
	ActionCancel GroupAction = "cancel"
	// ActionResolve marks a sealed group as `Completed` when the
	// caller knows all members are done (used by drivers that defer
	// resolution to an external signal — Phase 21's in-process driver
	// resolves automatically when all members are terminal, so this
	// action is rarely needed but exposed for symmetry with the brief
	// 05 surface).
	ActionResolve GroupAction = "resolve"
)

// PatchAction is the verb a caller passes to `ApplyPatch`. Patches
// transition `pending → applied | rejected` through the registry.
// The patch payload is opaque bytes — the actual context-patch shape
// lives at the planner (Phase 42+); Phase 21 stores + retrieves; the
// planner consumes.
type PatchAction string

// Patch actions.
const (
	// PatchAccept transitions a pending patch to `applied`.
	PatchAccept PatchAction = "accept"
	// PatchReject transitions a pending patch to `rejected`.
	PatchReject PatchAction = "reject"
)

// Patch is the persisted patch record. The `Bytes` slot carries the
// caller-shaped patch payload; the registry does not interpret it.
// Identity is mandatory.
type Patch struct {
	CreatedAt time.Time
	UpdatedAt time.Time
	SessionID identity.Identity
	ID        string
	Status    string
	Bytes     []byte
}

// GroupCompletion is the typed wake-up payload delivered by
// `WatchGroup` (and as the `task.group_resolved` bus-event payload).
// Carries the group's terminal status, the resolve timestamp, the
// cancel reason (populated when `FinalStatus == GroupCancelled`),
// and a `MemberOutcome` per group member.
//
// Discipline: `MemberOutcome.Result` is ref-shaped. Heavy results
// upstream MUST already have been substituted with `ArtifactRef`s
// (D-022, D-026); the payload is NOT byte-bound. A
// `TaskResult.Value` carrying inline bytes above the heavy-output
// threshold is a leak — the LLM-edge enforcement pass will fail
// loudly. The conformance suite's
// `WatchGroup_Push_DeliversCompletionPayload` verifies the
// ref-shaped contract by stuffing an ArtifactRef-shaped JSON into a
// member result and asserting it round-trips through the payload
// unchanged.
//
// Concurrent reuse: multiple `WatchGroup` subscribers on the same
// group all receive the same payload (D-025). The driver fans out
// the payload to each subscriber's buffered-size-1 channel without
// blocking.
type GroupCompletion struct {
	ResolvedAt  time.Time
	SessionID   identity.Identity
	GroupID     TaskGroupID
	OwnerTaskID TaskID
	FinalStatus TaskGroupStatus
	Reason      string
	Members     []MemberOutcome
}

// MemberOutcome is the per-task terminal record carried inside
// `GroupCompletion`. Either `Result` is populated (when
// `Status == StatusComplete`) or `Error` is populated (when
// `Status == StatusFailed`); neither is populated when
// `Status == StatusCancelled`.
type MemberOutcome struct {
	Result *TaskResult
	Error  *TaskError
	TaskID TaskID
	Status TaskStatus
}

// Sentinel errors for the group + patch surface. Callers compare via
// `errors.Is`.
var (
	// ErrGroupNotFound — Get / Seal / Cancel / Apply / WatchGroup
	// targeting a TaskGroupID that has no record (or the group is not
	// visible to the ctx identity).
	ErrGroupNotFound = errors.New("tasks: group not found")
	// ErrGroupSealed — mutation attempted on a sealed group (e.g.
	// spawning a new member into a sealed group).
	ErrGroupSealed = errors.New("tasks: group is sealed; cannot mutate membership")
	// ErrGroupNotSealed — `ApplyGroup(ActionResolve)` called on an
	// open (not-yet-sealed) group.
	ErrGroupNotSealed = errors.New("tasks: group must be sealed before resolve")
	// ErrGroupInvalidTransition — FSM transition not in the table
	// (e.g. Sealed → Open; Completed → Anything).
	ErrGroupInvalidTransition = errors.New("tasks: invalid group transition")
	// ErrPatchNotFound — `ApplyPatch` targeting an unknown patch ID.
	ErrPatchNotFound = errors.New("tasks: patch not found")
)
