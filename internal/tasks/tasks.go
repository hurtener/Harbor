// Package tasks owns Harbor's unified task surface: the
// `TaskRegistry` interface every planner / runtime / steering caller
// uses to spawn, list, cancel, and prioritise both foreground runs
// and background tasks under one `TaskID` namespace.
//
// Phase 20 ships the per-task surface (Spawn / Get / List / Cancel /
// Prioritize / Mark*); Phase 21 lays groups + retain-turn + patches
// on top. Bundling the whole TaskService into one phase would slow
// the wave-end E2E + delay the per-task surface that downstream
// phases (steering Phase 53, planner Phase 42) want as a stable
// foundation. The split is recorded in `docs/decisions.md` as D-030.
//
// Lifecycle FSM (enforced at the driver):
//
//	PENDING ──Spawn──▶ RUNNING ──MarkComplete──▶ COMPLETE
//	   │                  │
//	   │                  ├──MarkPaused──▶ PAUSED ──MarkResumed──▶ RUNNING
//	   │                  │
//	   │                  └──MarkFailed──▶ FAILED (terminal)
//	   │
//	   └──Cancel──▶ CANCELLED (terminal; valid from any non-terminal state)
//
// Invalid transitions return `ErrInvalidTransition` (wrapped with
// the from/to states named in the message). Terminal-to-anything is
// invalid; same-state is invalid (no idempotent self-transitions on
// the driver — the runtime engine knows whether a transition is
// real before calling).
//
// Idempotency. `Spawn` keys on `(SessionID, IdempotencyKey)`:
// same key → returns the existing `TaskHandle` with `Reused: true`;
// divergent `SpawnRequest` under the same key returns
// `ErrIdempotencyConflict`. Empty `IdempotencyKey` disables dedup
// entirely (each Spawn yields a fresh handle, no collisions).
//
// Cancellation propagation. `Cancel` walks the children index per
// `Task.PropagateOnCancel`:
//
//   - `"cascade"` (default): BFS through descendants, transitioning
//     each to `StatusCancelled`, emitting `task.cancelled` per cancel.
//   - `"isolate"`: only the target task transitions; children stay
//     in their current state.
//
// Identity. The triple `(tenant, user, session)` is mandatory at
// every API boundary (D-001). Empty tenant/user/session in
// `SpawnRequest.Identity` (or the ctx Identity for Get/Cancel) is
// rejected with `ErrIdentityRequired`. RunID is task-scoped: the
// foreground run IS a task; the background task has its own ID.
//
// Persistence. Each lifecycle transition writes through the
// configured `state.StateStore` as `StateRecord{Kind:
// "task.lifecycle", Bytes: marshal(task)}`. The wrapper layer is
// the typed adapter per D-027 — opaque bytes go to the leaf store.
// Caller-side audit redaction runs against `Description`, `Query`,
// `Result`, and `Error` BEFORE Save (per D-020).
//
// Bus events. Each lifecycle transition emits one of the registered
// `task.*` event types on the configured `events.EventBus`; payloads
// are typed (one struct per event type) and carry the `TaskID`,
// prior status (where applicable), and new status. Identity is on
// `Event.Identity` (the existing `Quadruple` field).
//
// SpawnTool. The `SpawnTool` surface lifts from RFC §6.8 verbatim
// so the FSM models `task.tool` lifecycle today. Actual tool
// dispatch wiring lands at Phase 26+; in Phase 20 `SpawnTool`
// returns a `TaskHandle` whose execution body is a no-op stub —
// the task persists at `StatusPending` and never auto-advances.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// LifecycleKind is the StateStore Kind constant for task-lifecycle
// records. Centralised so callers / tests / Phase 60 Protocol mappers
// reference one symbol.
const LifecycleKind = "task.lifecycle"

// TaskID is the unified identifier covering both foreground runs
// and background tasks (brief 05 §1). ULID-shaped at construction
// time; the registry assigns the value, callers do not.
type TaskID string

// TaskKind distinguishes a foreground task (a run inside a session's
// primary turn) from a background task (a spawned-without-blocking
// task). Both share the same TaskID namespace; this field is the
// discriminator.
type TaskKind string

// Task kinds.
const (
	// KindForeground is the kind for a run inside a session's primary
	// turn — what the predecessor called a "trace_id" lives here under
	// the unified TaskID.
	KindForeground TaskKind = "foreground"
	// KindBackground is the kind for a task spawned without blocking
	// the parent run.
	KindBackground TaskKind = "background"
)

// TaskStatus is the lifecycle state. Transitions are enforced by the
// driver; invalid transitions return `ErrInvalidTransition`.
type TaskStatus string

// Task statuses.
const (
	// StatusPending is the initial state assigned by Spawn.
	StatusPending TaskStatus = "pending"
	// StatusRunning is the active-execution state.
	StatusRunning TaskStatus = "running"
	// StatusPaused is the pause-state for retain-turn / HITL flows.
	// Phase 21 layers retain-turn semantics on top; Phase 20 only
	// enforces the FSM transition Running → Paused → Running.
	StatusPaused TaskStatus = "paused"
	// StatusComplete is a terminal state — execution finished
	// successfully and `Task.Result` is populated.
	StatusComplete TaskStatus = "complete"
	// StatusFailed is a terminal state — execution finished
	// unsuccessfully and `Task.Error` is populated.
	StatusFailed TaskStatus = "failed"
	// StatusCancelled is a terminal state — `Cancel` was invoked
	// (possibly via cascade from a parent).
	StatusCancelled TaskStatus = "cancelled"
)

// PropagateOnCancel controls how `Cancel` walks descendants.
const (
	// PropagateCascade walks the children index in BFS order,
	// transitioning each descendant to StatusCancelled. The default.
	PropagateCascade = "cascade"
	// PropagateIsolate only transitions the target task; children
	// keep running.
	PropagateIsolate = "isolate"
)

// Task is the persisted lifecycle record for one task. The Identity
// quadruple is captured immutably on Spawn; the runtime engine drives
// state transitions via the registry's Mark* methods.
//
// Group/patch fields are reserved for Phase 21 — the surface is
// intentionally narrow at Phase 20 so the Phase 21 PR adds those
// fields against a stable shape.
type Task struct {
	ID                TaskID
	Identity          identity.Quadruple
	Kind              TaskKind
	Status            TaskStatus
	Priority          int
	ParentTaskID      *TaskID
	Description       string
	Query             string
	Result            *TaskResult
	Error             *TaskError
	PropagateOnCancel string
	NotifyOnComplete  bool
	IdempotencyKey    string
	CreatedAt         int64 // unix nanoseconds; matches sessions / events convention
	UpdatedAt         int64 // unix nanoseconds
	// ToolCount is the running count of tool dispatches the runtime
	// has performed against this task. Advanced exclusively through
	// `TaskRegistry.IncrementToolCount` — never set directly by callers
	// (Phase 83m item 7). Projected to `prototypes.TaskRow.ToolCount`
	// for the Console Tasks page.
	ToolCount int
	// InputArtifactIDs carry operator-uploaded multimodal inputs the
	// run consumes on its first planner turn (Round-7 F11 / D-166).
	// The run loop materializes these into `RunContext.InputArtifacts`
	// via the per-MIME dispatcher: image bytes inline as
	// `ImagePart.DataURL`; everything else stays as an `ArtifactStub`
	// the LLM routes to a matching tool through the tool catalog. Empty
	// is the common case — text-only turns.
	InputArtifactIDs []string
}

// SpawnRequest is the input shape for `Spawn`. Identity is mandatory.
// `IdempotencyKey` is namespaced by `Identity.SessionID`: same key
// across different sessions creates two distinct tasks.
//
// `PropagateOnCancel` defaults to "cascade" when empty; "isolate"
// is opt-in for tasks that must survive a parent's cancellation.
//
// `GroupID` (Phase 21, optional) wires the new task into an existing
// `TaskGroup`. The driver verifies the group is `Open` (sealed or
// terminal groups reject with `ErrGroupSealed`) and registers the
// new task as a member. Empty `GroupID` is the default — most
// foreground turns aren't group members.
type SpawnRequest struct {
	Identity          identity.Quadruple
	Kind              TaskKind
	ParentTaskID      *TaskID
	Description       string
	Query             string
	Priority          int
	IdempotencyKey    string
	PropagateOnCancel string
	NotifyOnComplete  bool
	GroupID           TaskGroupID
	// InputArtifactIDs are operator-uploaded multimodal inputs the
	// task carries onto its first planner turn (Round-7 F11 / D-166).
	// Persisted onto `Task.InputArtifactIDs`; consumed by the run
	// loop's first-turn materializer. Empty is the text-only default.
	InputArtifactIDs []string
}

// SpawnToolRequest is the input shape for `SpawnTool`. The shape
// lifts from RFC §6.8 verbatim so the FSM models tool-task lifecycle
// today; actual tool dispatch wiring lands at Phase 26.
//
// Phase 20's `SpawnTool` execution body is a no-op stub: the task is
// persisted at `StatusPending` and never auto-advances. The runtime
// engine (Phase 26) drives the lifecycle once dispatch is wired.
//
// `GroupID` (Phase 21, optional) wires the new tool task into an
// existing `TaskGroup`. See `SpawnRequest.GroupID` for the contract.
type SpawnToolRequest struct {
	Identity          identity.Quadruple
	ParentTaskID      *TaskID
	ToolName          string
	ToolArgs          json.RawMessage
	Description       string
	Priority          int
	IdempotencyKey    string
	PropagateOnCancel string
	NotifyOnComplete  bool
	GroupID           TaskGroupID
}

// TaskHandle is the return shape of `Spawn` / `SpawnTool`. `Reused`
// is true when an idempotency-key match returned an existing handle.
type TaskHandle struct {
	ID     TaskID
	Reused bool
}

// TaskFilter is the read-side filter for `List`.
//
// Empty pointer fields are wildcards: a zero-valued `TaskFilter`
// returns every task in the session.
type TaskFilter struct {
	Status   *TaskStatus
	Kind     *TaskKind
	ParentID *TaskID
}

// TaskSummary is the projection returned by `List`. Compact by
// design — full Task records are loaded via `Get` when needed.
type TaskSummary struct {
	ID        TaskID
	Status    TaskStatus
	Kind      TaskKind
	Priority  int
	UpdatedAt int64 // unix nanoseconds
}

// TaskResult carries the successful-completion payload. `Value` is
// pre-redacted by the caller (D-020); the registry stores it
// verbatim.
//
// Phase 106 (V1.2) pins the answer-envelope contract: when the
// run-loop driver (cmd/harbor/cmd_dev_runloop.go::handleSpawn)
// produces TaskResult from a planner.Finish, `Value` is the JSON
// encoding of:
//
//	{
//	  "answer":          string,  // the LLM's natural-language answer
//	  "finish_reason":   string,  // planner.FinishReason as string
//	  "tool_calls_seen": int      // len(traj.Steps) at finish
//	}
//
// Consumers (Console Playground, CLI, third-party UIs) MAY rely on
// this shape. Future planners that return richer answers (markdown
// structure, multimodal) will EXTEND the shape with new keys, never
// break existing ones (forward-compatible additive evolution).
type TaskResult struct {
	Value json.RawMessage
}

// TaskError carries the failure payload. `Code` is a caller-defined
// short string; `Message` is the human-readable explanation, also
// pre-redacted by the caller.
type TaskError struct {
	Code    string
	Message string
}

// TaskRegistry is the orchestration surface for the task subsystem.
//
// Implementations MUST be safe for concurrent use by N goroutines
// against a single shared instance (D-025). Mutable state must be
// guarded; per-call state lives in `ctx`, never on the driver.
//
// The Mark* methods are the lifecycle drive-points called by the
// runtime engine; Cancel / Prioritize are caller-initiated (planner,
// steering, Console).
type TaskRegistry interface {
	// Spawn creates a new task or returns the existing handle when an
	// idempotency-key match is found. Returns `ErrIdentityRequired`
	// when the request's identity triple is incomplete.
	Spawn(ctx context.Context, req SpawnRequest) (TaskHandle, error)

	// SpawnTool creates a task representing a tool invocation. Phase
	// 20 ships the surface; tool dispatch wiring lands at Phase 26+
	// — the persisted task stays at `StatusPending` until then.
	SpawnTool(ctx context.Context, req SpawnToolRequest) (TaskHandle, error)

	// Get loads the task with `id`. Returns `ErrNotFound` (wrapped)
	// when no record exists or the task is not visible to the ctx
	// identity (cross-tenant / cross-session reads are rejected).
	Get(ctx context.Context, id TaskID) (*Task, error)

	// List returns task summaries for the given session matching `f`.
	// Empty pointer fields in `f` are wildcards.
	List(ctx context.Context, sessionID identity.Identity, f TaskFilter) ([]TaskSummary, error)

	// Cancel transitions the task to `StatusCancelled`. The descendant
	// walk depends on the task's `PropagateOnCancel`:
	//
	//   - "cascade" (default): BFS through children, each emitting
	//     `task.cancelled`.
	//   - "isolate": only the target transitions.
	//
	// Returns (true, nil) when the task transitioned; (false, nil)
	// when the task was already terminal (Cancel is idempotent on
	// already-terminal states). Returns `ErrNotFound` when the task
	// does not exist.
	Cancel(ctx context.Context, id TaskID, reason string) (bool, error)

	// Prioritize updates the task's `Priority`. Phase 20 stores the
	// value but does not preempt or reorder execution — scheduling is
	// the runtime engine's concern. Emits `task.prioritised`.
	//
	// Returns (true, nil) when the priority changed; (false, nil)
	// when the value matched (no-op write).
	Prioritize(ctx context.Context, id TaskID, priority int) (bool, error)

	// MarkRunning transitions Pending or Paused → Running. Invalid
	// transitions return `ErrInvalidTransition`.
	MarkRunning(ctx context.Context, id TaskID) error

	// MarkPaused transitions Running → Paused.
	MarkPaused(ctx context.Context, id TaskID) error

	// MarkResumed transitions Paused → Running. Distinct method (vs
	// MarkRunning) so the bus event can be `task.resumed` rather than
	// `task.started`.
	MarkResumed(ctx context.Context, id TaskID) error

	// MarkComplete transitions Running → Complete. Persists `result`
	// on the Task record; emits `task.completed`.
	MarkComplete(ctx context.Context, id TaskID, result TaskResult) error

	// MarkFailed transitions Running → Failed. Persists `err` on the
	// Task record; emits `task.failed`.
	MarkFailed(ctx context.Context, id TaskID, err TaskError) error

	// IncrementToolCount atomically increments `Task.ToolCount` by 1
	// and persists the updated record (Phase 83m item 7). NOT
	// idempotent — every call increments. The new value is reflected
	// on the next `Get` / `List` projection (`prototypes.TaskRow.ToolCount`).
	//
	// Returns `ErrNotFound` when the task does not exist or is not
	// visible to the ctx identity; `ErrRegistryClosed` after Close.
	// Does NOT change the task's FSM status — runs against tasks in
	// any non-terminal state. Terminal tasks (Complete / Failed /
	// Cancelled) still accept increments (the runloop's late-arriving
	// tool dispatches against a cancelled run can still be counted);
	// the storage write is unconditional.
	//
	// The runloop calls this from its CallTool dispatch path once the
	// ToolExecutor returns without error — that is the only documented
	// producer in V1.
	IncrementToolCount(ctx context.Context, id TaskID) error

	// ResolveOrCreateGroup is the idempotent group constructor. Empty
	// `GroupRequest.ID` → registry assigns a fresh ULID. Non-empty +
	// already-existing → the existing group is returned unchanged.
	// Identity is mandatory; cross-session reuse of an ID is rejected
	// with `ErrGroupNotFound` (existence-without-access).
	//
	// Emits `task.group_created` on the first creation; the no-op
	// idempotent return does NOT re-emit.
	ResolveOrCreateGroup(ctx context.Context, req GroupRequest) (*TaskGroup, error)

	// SealGroup transitions an open group to `GroupSealed`, freezing
	// membership. Sealed groups still have non-terminal members; the
	// driver resolves the group automatically when the last member
	// transitions to terminal.
	//
	// Invalid transitions (already sealed; terminal) return
	// `ErrGroupInvalidTransition`. Emits `task.group_sealed`.
	SealGroup(ctx context.Context, id TaskGroupID) error

	// CancelGroup transitions a non-terminal group to `GroupCancelled`
	// and (when `propagate=true`) cancels every non-terminal member
	// task. The `reason` is a short caller-controlled string (same
	// `SafePayload` contract as `Cancel`'s reason).
	//
	// Emits `task.group_cancelled` carrying the canonical
	// `GroupCompletion` payload (so `WatchGroup` subscribers receive
	// the cancel-with-reason as a single typed delivery).
	CancelGroup(ctx context.Context, id TaskGroupID, reason string, propagate bool) error

	// ApplyGroup is the action-verb wrapper over `SealGroup` /
	// `CancelGroup` / explicit resolve. Convenience for callers that
	// dispatch by enum.
	//
	//   - ActionSeal    → SealGroup
	//   - ActionCancel  → CancelGroup(reason="action:cancel", propagate=true)
	//   - ActionResolve → mark sealed → completed (errors with
	//                     `ErrGroupNotSealed` on an open group)
	ApplyGroup(ctx context.Context, id TaskGroupID, action GroupAction) error

	// ListGroups returns the groups owned by `sessionID` matching the
	// optional `status` filter (nil = wildcard). Empty list + nil
	// error is the no-groups case; missing-identity returns
	// `ErrIdentityRequired`.
	ListGroups(ctx context.Context, sessionID identity.Identity, status *TaskGroupStatus) ([]TaskGroup, error)

	// ApplyPatch transitions a pending patch through
	// `pending → applied | rejected`. Returns `(true, nil)` when the
	// transition occurred; `(false, nil)` when the patch was already
	// in the target terminal state (idempotent). Returns
	// `ErrPatchNotFound` on a missing patch ID.
	//
	// Patches are persisted through StateStore under `Kind =
	// "task.patch"`. The patch payload is opaque bytes (D-027); the
	// actual context-patch shape lives at the planner (Phase 42+).
	//
	// Emits `task.patch_applied` or `task.patch_rejected` on a real
	// transition; no re-emit on the no-op path.
	ApplyPatch(ctx context.Context, sessionID identity.Identity, patchID string, action PatchAction) (bool, error)

	// AcknowledgeBackground marks completed background tasks as
	// user-acknowledged. Returns the count of tasks that transitioned
	// from un-acknowledged → acknowledged (idempotent on a re-ack).
	//
	// Emits one `task.background_acknowledged` event per task on the
	// real-transition path. Unknown task IDs are silently skipped (no
	// error; the count reflects only the real ack transitions).
	AcknowledgeBackground(ctx context.Context, sessionID identity.Identity, ids []TaskID) (int, error)

	// RegisterRetainTurnWaiter returns a channel that closes when the
	// session's earliest-active retain-turn group resolves, and a
	// cancel func that unsubscribes. The runtime engine consumes the
	// closed channel as the "all retain-turn groups have resolved"
	// signal so foreground-turn dispatch can resume.
	//
	// Buffered size 1 — the resolve path delivers the resolved
	// group's ID without blocking even if the consumer is slow.
	// Channel close is the termination signal; the optional payload
	// is the ID of the group whose terminal transition triggered the
	// wake.
	//
	// Implementations are required to close the channel exactly once.
	RegisterRetainTurnWaiter(sessionID identity.Identity) (<-chan TaskGroupID, func())

	// WatchGroup is the non-retain-turn dual of
	// `RegisterRetainTurnWaiter`: it does NOT block any foreground
	// turn — the planner is free to proceed while the group runs in
	// the background. When the group reaches a terminal state, the
	// runtime delivers a typed `GroupCompletion` payload on the
	// returned channel and closes it.
	//
	// Callers typically use this as a "wake the planner" signal so
	// background results integrate back into the conversation; see
	// the "Wake policy modes" godoc in `groups.go` for the three
	// patterns (push, poll, hybrid) the planner runtime can implement
	// against this single mechanism.
	//
	// Returns `ErrGroupNotFound` when the group is unknown at
	// registration time (e.g. resolved + GC'd). For a
	// resolved-but-still-tracked group, the implementation returns a
	// channel that is *already* primed with the cached
	// `GroupCompletion` (so late subscribers don't deadlock).
	//
	// The cancel func unsubscribes; calling it after a delivery is a
	// no-op. The channel is closed exactly once — either by the
	// resolve path (with a delivery) or by the cancel path (without
	// a delivery).
	//
	// Concurrent reuse: multiple subscribers on the same group all
	// receive the same payload (D-025).
	WatchGroup(sessionID identity.Identity, groupID TaskGroupID) (<-chan GroupCompletion, func(), error)

	// Close releases registry resources. Subsequent operations return
	// `ErrRegistryClosed`. Idempotent.
	Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrNotFound — Get / Cancel / Prioritize / Mark* targeting a
	// TaskID that has no record (or the record is not visible to the
	// ctx identity).
	ErrNotFound = errors.New("tasks: task not found")
	// ErrInvalidTransition — Mark* called for a transition that is
	// not in the FSM table (e.g. Pending → Complete skipping Running).
	ErrInvalidTransition = errors.New("tasks: invalid lifecycle transition")
	// ErrIdempotencyConflict — Spawn called with a previously-seen
	// IdempotencyKey but a divergent SpawnRequest. Tells the caller
	// a retry policy bug exists upstream.
	ErrIdempotencyConflict = errors.New("tasks: idempotency key reused with divergent SpawnRequest")
	// ErrIdentityRequired — Spawn / Get / List / Cancel / Prioritize
	// / Mark* called with an Identity missing one of (tenant, user,
	// session). Identity is mandatory (D-001).
	ErrIdentityRequired = errors.New("tasks: identity required (tenant/user/session)")
	// ErrUnknownDriver — Open was asked for a driver name no
	// registered factory handles.
	ErrUnknownDriver = errors.New("tasks: unknown driver")
	// ErrRegistryClosed — any operation called after Close.
	ErrRegistryClosed = errors.New("tasks: registry is closed")
	// ErrInvalidRequest — SpawnRequest / SpawnToolRequest fails
	// structural validation (empty Kind, negative priority, unknown
	// PropagateOnCancel value, etc.).
	ErrInvalidRequest = errors.New("tasks: invalid request")
)

// ValidateRequest checks structural invariants Spawn needs before
// touching driver storage: identity triple present, Kind known,
// PropagateOnCancel known (or empty for the default).
func ValidateRequest(req SpawnRequest) error {
	if err := validateIdentity(req.Identity); err != nil {
		return err
	}
	switch req.Kind {
	case KindForeground, KindBackground:
		// ok
	case "":
		return fmt.Errorf("%w: kind required", ErrInvalidRequest)
	default:
		return fmt.Errorf("%w: kind %q not in {foreground,background}", ErrInvalidRequest, req.Kind)
	}
	if err := validatePropagate(req.PropagateOnCancel); err != nil {
		return err
	}
	return nil
}

// ValidateToolRequest mirrors ValidateRequest for SpawnToolRequest.
func ValidateToolRequest(req SpawnToolRequest) error {
	if err := validateIdentity(req.Identity); err != nil {
		return err
	}
	if req.ToolName == "" {
		return fmt.Errorf("%w: tool_name required", ErrInvalidRequest)
	}
	if err := validatePropagate(req.PropagateOnCancel); err != nil {
		return err
	}
	return nil
}

// validateIdentity returns wrapped ErrIdentityRequired when any of
// tenant/user/session is empty. RunID is permitted to be empty
// (session-scoped tasks are valid, e.g. background work tied to a
// session but not a specific run).
func validateIdentity(q identity.Quadruple) error {
	if q.TenantID == "" || q.UserID == "" || q.SessionID == "" {
		return ErrIdentityRequired
	}
	return nil
}

// validatePropagate accepts the empty string (driver substitutes the
// default), "cascade", or "isolate". Any other value is invalid.
func validatePropagate(p string) error {
	switch p {
	case "", PropagateCascade, PropagateIsolate:
		return nil
	default:
		return fmt.Errorf("%w: propagate_on_cancel %q not in {cascade,isolate}", ErrInvalidRequest, p)
	}
}

// Factory builds a TaskRegistry from a Dependencies struct. Drivers
// expose one Factory each via init() → Register.
type Factory func(deps Dependencies) (TaskRegistry, error)

// Dependencies bundles the wiring inputs every TaskRegistry driver
// needs. Sessions / state / events are required; the redactor and
// config are passed through verbatim. Wiring lives in `cmd/harbor`
// (or test helpers); the registry never reaches into ctx for these.
type Dependencies struct {
	// Store is the StateStore used to persist task lifecycle records
	// (D-027 typed-wrapper-over-generic). Required.
	Store state.StateStore
	// Bus is the EventBus where lifecycle events land. Required.
	Bus events.EventBus
	// Redactor is the audit redactor applied to Description / Query
	// / Result / Error BEFORE Save (D-020). Required; wiring code
	// passes the global redactor here.
	Redactor audit.Redactor
	// Cfg carries Phase 20's TasksConfig (driver name today; Phase
	// 21 adds RetainTurnTimeout + ContinuationHopLimit).
	Cfg config.TasksConfig
}

// ctxKey is the unexported key under which a TaskRegistry is
// propagated on a context. Independent from identity / audit /
// events / state ctx keys.
type ctxKey int

const registryCtxKey ctxKey = iota

// WithRegistry attaches r to ctx so downstream handlers can recover
// it via MustFrom or From.
func WithRegistry(ctx context.Context, r TaskRegistry) context.Context {
	return context.WithValue(ctx, registryCtxKey, r)
}

// MustFrom returns the TaskRegistry in ctx; panics with
// ErrRegistryClosed (used as the sentinel for "no registry
// configured") when none is present. Use in handler/runtime paths
// where a registry is mandatory.
func MustFrom(ctx context.Context) TaskRegistry {
	r, ok := From(ctx)
	if !ok {
		panic(ErrRegistryClosed)
	}
	return r
}

// From returns the TaskRegistry in ctx and a presence bool. Use
// when absence is recoverable.
func From(ctx context.Context) (TaskRegistry, bool) {
	r, ok := ctx.Value(registryCtxKey).(TaskRegistry)
	return r, ok
}
