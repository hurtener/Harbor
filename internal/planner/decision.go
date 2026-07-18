package planner

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/tasks"
)

// Decision is the sealed sum-type a planner returns from Next.
// Seven shapes ship (RFC §6.2):
//
//   - CallTool: invoke one tool with structured args.
//   - CallParallel: invoke N tools in parallel with a join spec.
//   - Batch: one native multi-call response mixing catalog-tool
//     branches with non-retain-turn task spawns.
//   - SpawnTask: spawn a background task (retain-turn or non-retain-turn).
//   - AwaitTask: block the planner until a spawned task resolves.
//   - RequestPause: pause the run for approval / input / external event.
//   - Finish: terminal decision with a reason + payload.
//
// The interface is sealed via the unexported `isDecision()` marker —
// adding a further shape requires editing this file. The predecessor's
// "magic strings as next_node" anti-pattern is explicitly rejected
// here (RFC §6.2 settled decisions); each shape is its own Go type.
//
// `NoOp` is deliberately absent. Wait-for-
// steering and trajectory-summarisation are Runtime short-circuits,
// not planner decisions.
type Decision interface {
	isDecision()
}

// CallTool invokes one tool with structured args. The Runtime
// dispatches via the production ToolCatalog + ToolPolicy;
// the planner does not block on the call.
//
// The action shape is intentionally narrow — `{tool, args}` only.
// An earlier phase dropped the former `Reasoning` field: the model
// emits the action JSON, and the provider-side thinking trace is
// captured separately on `trajectory.Step.ReasoningTrace` via
// `llm.CompleteResponse.Reasoning`. Reasoning is captured content, not
// part of the structured decision; replaying it into prompts is an
// operator-controlled per-agent knob, never a schema field.
type CallTool struct {
	// Tool is the name registered in the ToolCatalogView.
	Tool string
	// Args is the JSON-encoded argument payload matching the tool's
	// ArgsSchema. Validation happens at the catalog edge; an invalid
	// payload produces `tools.ErrToolInvalidArgs` from dispatch.
	Args json.RawMessage
	// CallID is the provider-assigned tool-call identifier (Phase
	// native tool-calling). Empty for prompt-
	// engineered CallTool emissions; non-empty when the call
	// originated from a native ToolCall. Round-trips on the
	// RoleTool message's ToolCallID field.
	CallID string
}

func (CallTool) isDecision() {}

// CallParallel invokes N tools concurrently with a JoinSpec describing
// how the Runtime merges results. Atomic setup validation: any
// branch's invalid args fails the whole call before execution (RFC
// §6.2; Harbor ships the executor).
//
// Branches share the same step-level pause/cancel atomicity contract
// see the plan.
type CallParallel struct {
	Branches []CallTool
	Join     *JoinSpec
}

func (CallParallel) isDecision() {}

// JoinSpec describes how the Runtime merges N CallParallel branch
// results into a single observation the planner sees in the next
// trajectory step. Harbor ships the executor; Harbor ships the
// shape so concretes can compile against it.
type JoinSpec struct {
	// Kind is the join strategy. Harbor ships the constants;
	// Harbor ships the implementations.
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
	// Harbor introduces JoinN as the third explicit join
	// shape (JoinAll / JoinFirstSuccess / JoinN); JoinKeyed remains
	// a documented future surface (a future runtime phase merges
	// outputs by key).
	JoinN JoinKind = "n"
)

// Batch groups zero-or-more catalog-tool branches with zero-or-more
// task spawns projected from ONE native multi-call LLM response — the
// shape a projector constructs when a model batches a `_spawn_task`
// call alongside an ordinary tool call (or alongside other spawns) in
// a single response.
//
// Batch is a distinct fourth dispatch shape, NOT a widening of
// CallParallel: tool-invocation accounting counts Tools and Spawns
// separately (a spawn is never a tool invocation — see
// DecisionInvocationCount, which returns len(Tools) for a Batch and
// counts Spawns as zero). Reserved-control terminal/blocking names
// (`_finish` / `_await_task`) are never Batch members; only catalog
// tools and non-retain-turn spawns are.
//
// Invariants (enforced by NewBatch, failing loud on violation):
//
//   - len(Tools)+len(Spawns) >= 2. A single-branch would-be Batch is
//     degenerate; producers construct the plain CallTool / SpawnTask /
//     CallParallel shape instead (one representation per semantic).
//   - Every Spawns[i].Spec.RetainTurn is false. A turn-retaining spawn
//     inside a non-blocking multi-dispatch is a contradiction.
type Batch struct {
	// Tools are the catalog-tool branches, dispatched concurrently
	// and joined per Join (nil collapses to JoinAll, matching the
	// native-parallel path).
	Tools []CallTool
	// Spawns are the task spawns; every entry's Spec.RetainTurn is
	// false.
	Spawns []SpawnTask
	// Join governs ONLY Tools. It is nil (JoinAll) when Tools is
	// empty — a spawns-only Batch carries no join.
	Join *JoinSpec
}

func (Batch) isDecision() {}

// NewBatch validates and constructs a Batch, failing loud (wrapping
// ErrInvalidDecision) on a degenerate batch (fewer than two combined
// Tools+Spawns branches) or any retain-turn spawn. Every producer of a
// Batch — the React projector today, future concrete planners — routes
// through this constructor so the structural invariants hold at every
// call site.
//
// NewBatch validates STRUCTURAL invariants only. Semantic checks that
// need projection context (e.g. FailFast disagreement across
// auto-grouped spawns) live at the producing projector, and the
// operator-configured breadth cap lives at the dispatch edge.
func NewBatch(tools []CallTool, spawns []SpawnTask, join *JoinSpec) (Batch, error) {
	if len(tools)+len(spawns) < 2 {
		return Batch{}, fmt.Errorf(
			"%w: Batch requires at least 2 combined branches, got %d tools + %d spawns (construct the plain CallTool / SpawnTask / CallParallel shape for a single branch)",
			ErrInvalidDecision, len(tools), len(spawns),
		)
	}
	for i, sp := range spawns {
		if sp.Spec.RetainTurn {
			return Batch{}, fmt.Errorf(
				"%w: Batch spawn %d has RetainTurn=true (a turn-retaining spawn cannot ride a non-blocking batch dispatch)",
				ErrInvalidDecision, i,
			)
		}
	}
	return Batch{Tools: tools, Spawns: spawns, Join: join}, nil
}

// SpawnTask spawns a background task. When `Spec.RetainTurn` is true
// the foreground turn blocks on the spawned task's group; when false
// the planner returns control to the runtime and consumes
// `tasks.TaskRegistry.WatchGroup` to learn when the group resolves
// (wake-on-resolution contract).
//
// `GroupID` is optional — when empty, the runtime creates an
// ad-hoc single-member group; when non-empty, the task joins the
// existing group (cross-task fan-in pattern).
type SpawnTask struct {
	Kind    tasks.TaskKind
	Spec    SpawnSpec
	GroupID tasks.TaskGroupID
	// CallID is the provider-assigned tool-call identifier of the
	// native `_spawn_task` call this spawn was projected from,
	// mirroring CallTool.CallID. Batch dispatch keys each spawn's
	// observation by it so every native tool_call_id — spawn calls
	// included — is answered. Empty for programmatic (non-native)
	// spawn emissions, exactly like CallTool.CallID; the projector
	// stamps it when partitioning a native multi-call response.
	CallID string
}

func (SpawnTask) isDecision() {}

// SpawnSpec is the planner-facing spawn descriptor. The Runtime maps
// it into a `tasks.SpawnRequest` (or `tasks.SpawnToolRequest`) at
// dispatch time; identity is filled from the run's quadruple.
//
// At the shape carries only the fields the planner needs to
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
	// WatchGroup to re-invoke the planner on resolution.
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
// ArtifactRef-shaped).
//
// `Reason` MUST be one of the canonical values (see
// IsValidFinishReason). The Runtime rejects an invalid reason with
// ErrInvalidDecision.
//
// Payload's contract on an output-schema run (WithOutputSchema / the
// per-task output_schema field, validated at the shared
// internal/runtime/runctx envelope builder): a `string` payload is
// treated as raw JSON TEXT, never as a plain Go string to be quoted for
// the caller — this is the react terminal-answer reality, where
// `resp.Content` IS the model's JSON encoding of the answer. A plain Go
// string like `"done"` is NOT valid JSON and fails loud with a hint
// naming the fix (quote it, or return a structured payload instead).
// Structured payloads (a map, a struct, any non-string/[]byte/
// json.RawMessage Go value) are marshaled via encoding/json. A
// json.RawMessage or []byte payload is captured verbatim, bytes
// unchanged.
type Finish struct {
	Reason   FinishReason
	Payload  any
	Metadata map[string]any
}

func (Finish) isDecision() {}
