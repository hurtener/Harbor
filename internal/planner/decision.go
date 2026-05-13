package planner

import (
	"encoding/json"

	"github.com/hurtener/Harbor/internal/tasks"
)

// Decision is the sealed sum-type a planner returns from Next.
// Six shapes ship at Phase 42 (RFC §6.2):
//
//   - CallTool: invoke one tool with structured args.
//   - CallParallel: invoke N tools in parallel with a join spec.
//   - SpawnTask: spawn a background task (retain-turn or non-retain-turn).
//   - AwaitTask: block the planner until a spawned task resolves.
//   - RequestPause: pause the run for approval / input / external event.
//   - Finish: terminal decision with a reason + payload.
//
// The interface is sealed via the unexported `isDecision()` marker —
// adding a seventh shape requires editing this file. The predecessor's
// "magic strings as next_node" anti-pattern is explicitly rejected
// here (RFC §6.2 settled decisions); each shape is its own Go type.
//
// `NoOp` is deliberately absent (resolves brief 02 Q-5). Wait-for-
// steering and trajectory-summarisation are Runtime short-circuits,
// not planner decisions.
type Decision interface {
	isDecision()
}

// CallTool invokes one tool with structured args. The Runtime
// dispatches via the production ToolCatalog + ToolPolicy
// (Phase 26 + 26a); the planner does not block on the call.
//
// `Reasoning` is the planner's free-text justification — surfaced in
// observability + audit; capped by the Runtime's payload bounds before
// emit.
type CallTool struct {
	// Tool is the name registered in the ToolCatalogView.
	Tool string
	// Args is the JSON-encoded argument payload matching the tool's
	// ArgsSchema. Validation happens at the catalog edge; an invalid
	// payload produces `tools.ErrToolInvalidArgs` from dispatch.
	Args json.RawMessage
	// Reasoning is the planner's free-text justification.
	Reasoning string
}

func (CallTool) isDecision() {}

// CallParallel invokes N tools concurrently with a JoinSpec describing
// how the Runtime merges results. Atomic setup validation: any
// branch's invalid args fails the whole call before execution (RFC
// §6.2; Phase 47 ships the executor).
//
// Branches share the same step-level pause/cancel atomicity contract
// — see Phase 47's plan.
type CallParallel struct {
	Branches []CallTool
	Join     *JoinSpec
}

func (CallParallel) isDecision() {}

// JoinSpec describes how the Runtime merges N CallParallel branch
// results into a single observation the planner sees in the next
// trajectory step. Phase 47 ships the executor; Phase 42 ships the
// shape so concretes can compile against it.
type JoinSpec struct {
	// Kind is the join strategy. Phase 42 ships the constants;
	// Phase 47 ships the implementations.
	Kind JoinKind
	// MergeKeys is the deterministic merge ordering (only meaningful
	// for JoinKeyed).
	MergeKeys []string
	// N is the success threshold for JoinN — the executor waits until
	// N branches succeed, then cancels the remaining branches. Ignored
	// for any Kind other than JoinN. Values ≤ 0 fall back to JoinAll
	// semantics (the executor validates this at setup time).
	N int
}

// JoinKind enumerates the parallel-result merge strategies.
type JoinKind string

// Join kinds.
const (
	// JoinAll waits for every branch to terminate before producing
	// the merged observation. The default.
	JoinAll JoinKind = "all"
	// JoinFirstSuccess returns the first successful branch; the rest
	// are cancelled. Failures are NOT cancelled until all branches
	// have terminated.
	JoinFirstSuccess JoinKind = "first_success"
	// JoinKeyed produces a keyed merge over the branches; the
	// MergeKeys slice gives the deterministic ordering.
	JoinKeyed JoinKind = "keyed"
	// JoinN waits for N branches to succeed, then cancels the
	// remaining branches. JoinSpec.N carries the threshold; the
	// executor validates 0 < N ≤ len(Branches) at setup time and
	// fails the call with ErrParallelInvalidJoin when out of range.
	// D-056 — Phase 47 introduces JoinN as the third explicit join
	// shape (JoinAll / JoinFirstSuccess / JoinN); JoinKeyed remains
	// a documented future surface (a future runtime phase merges
	// outputs by key).
	JoinN JoinKind = "n"
)

// SpawnTask spawns a background task. When `Spec.RetainTurn` is true
// the foreground turn blocks on the spawned task's group; when false
// the planner returns control to the runtime and consumes
// `tasks.TaskRegistry.WatchGroup` to learn when the group resolves
// (D-032 wake-on-resolution contract).
//
// `GroupID` is optional — when empty, the runtime creates an
// ad-hoc single-member group; when non-empty, the task joins the
// existing group (cross-task fan-in pattern).
type SpawnTask struct {
	Kind    tasks.TaskKind
	Spec    SpawnSpec
	GroupID tasks.TaskGroupID
}

func (SpawnTask) isDecision() {}

// SpawnSpec is the planner-facing spawn descriptor. The Runtime maps
// it into a `tasks.SpawnRequest` (or `tasks.SpawnToolRequest`) at
// dispatch time; identity is filled from the run's quadruple.
//
// At Phase 42 the shape carries only the fields the planner needs to
// specify; the Runtime fills the rest (Identity, IdempotencyKey,
// PropagateOnCancel, NotifyOnComplete). Future phases MAY extend this
// shape with additional planner-controlled fields.
type SpawnSpec struct {
	// Description is the human-readable task description (audit +
	// observability).
	Description string
	// Query is the goal / prompt the spawned task should pursue.
	Query string
	// Priority is the task scheduling priority (-1000..1000). Zero
	// is the default mid-priority.
	Priority int
	// RetainTurn blocks the foreground turn on the spawned task's
	// group resolution. When true the planner WILL re-enter Next
	// only after the group reaches a terminal state. When false the
	// planner returns control to the runtime; the runtime consumes
	// WatchGroup to re-invoke the planner on resolution (D-032).
	RetainTurn bool
	// FailFast applies when SpawnTask creates a fresh group: cancels
	// remaining members when the first fails. Ignored when joining
	// an existing GroupID.
	FailFast bool
}

// AwaitTask blocks the planner until the named task reaches a
// terminal state. The Runtime's executor watches the task's lifecycle
// and re-invokes Next with the MemberOutcome surfaced in the next
// trajectory step.
type AwaitTask struct {
	TaskID tasks.TaskID
}

func (AwaitTask) isDecision() {}

// RequestPause asks the Runtime to pause the run for an external
// signal. The unified pause/resume primitive (later phase) drives the
// pause coordinator; the planner only signals "I need a pause" via
// this decision (RFC §3.3 + §6.3).
//
// `Reason` MUST be one of the four canonical values (see
// IsValidPauseReason). The Runtime rejects an invalid reason with
// ErrInvalidDecision before the pause is issued.
//
// `Payload` is sanitised and depth/size-bounded by the Runtime's
// pauseresume coordinator (RFC §6.3 — depth ≤ 6, ≤ 64 keys, etc.)
// before serialisation.
type RequestPause struct {
	Reason  PauseReason
	Payload map[string]any
}

func (RequestPause) isDecision() {}

// Finish is the terminal decision. The Runtime maps FinishReason →
// Protocol `task.completed` / `task.failed` payloads; `Payload`
// carries the planner's terminal observation (a summary string, a
// structured answer, an ArtifactRef — heavy payloads MUST be
// ArtifactRef-shaped per D-026).
//
// `Reason` MUST be one of the canonical values (see
// IsValidFinishReason). The Runtime rejects an invalid reason with
// ErrInvalidDecision.
type Finish struct {
	Reason   FinishReason
	Payload  any
	Metadata map[string]any
}

func (Finish) isDecision() {}
