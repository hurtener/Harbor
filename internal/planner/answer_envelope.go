package planner

// AnswerEnvelope is the canonical JSON shape `tasks.TaskResult.Value`
// carries when a run-loop driver completes a task from a
// [Finish] with reason [FinishGoal]:
//
//	{"answer": "<llm text>", "finish_reason": "<FinishReason>", "tool_calls_seen": <int>}
//
// Producers: the per-task run-loop drivers (cmd/harbor +
// harbortest/devstack) marshal it on MarkComplete. Consumers: the
// `tasks.get` Protocol projector reads it into `result_inline`, and
// `internal/runtime/dispatch`'s AwaitTask / retain-turn SpawnTask
// observation parses it back for the awaiting planner.
//
// The encoding is byte-compatible with the map-literal shape
// (keys in the same order; pinned by the golden test in
// answer_envelope_test.go). Forward-compatible — future phases extend
// with new keys; never break existing ones.
//
// named and exported what was an implicit cmd↔cmd
// wire contract (the SDK friction audit's Pattern 1 / P3 finding): one
// `package main` file marshalled what another parsed, with no named
// type anywhere. Home rationale (import direction, recorded in the
// phase plan): the envelope is the projection of [Finish] — it carries
// [FinishReason] verbatim — and both producers (run-loop drivers) and
// consumers (`internal/runtime/dispatch`, Protocol projectors) already
// import `internal/planner`; homing it in `internal/tasks` would force
// a new tasks→planner import edge (tasks is planner-free today).
type AnswerEnvelope struct {
	// Answer is the assistant's final answer text extracted from the
	// Finish payload.
	Answer string `json:"answer"`
	// FinishReason is the planner's Finish.Reason, verbatim
	// (string-typed on the wire).
	FinishReason string `json:"finish_reason"`
	// ToolCallsSeen is the number of actual tool invocations the run
	// dispatched — computed via CountToolInvocations, NOT
	// len(Trajectory.Steps). A CallTool step counts once; a
	// CallParallel step counts once PER branch (the runtime dispatches
	// N tools concurrently, not one); SpawnTask and AwaitTask steps
	// count zero — they are runtime decisions, never tool dispatches.
	//
	// This field's semantics were redefined from an earlier len(Steps)
	// reading, which silently miscounted: undercounting a CallParallel
	// step (1 instead of N branches) and overcounting a SpawnTask /
	// AwaitTask step (a dispatch that never happened). This is a
	// value-semantics change on an SDK-visible field — embedders
	// comparing ToolCallsSeen against a fixed expectation should
	// re-check it after upgrading.
	ToolCallsSeen int `json:"tool_calls_seen"`
}

// Terminal task-error codes a run-loop driver stamps on
// `tasks.TaskError.Code` when a run ends without FinishGoal
// (exported for run-loop drivers).
const (
	// TaskErrorCodeRunLoopError marks a run whose RunLoop.Run returned
	// a non-cancellation error.
	TaskErrorCodeRunLoopError = "runloop_error"
	// TaskErrorCodeCancelled marks a run whose RunLoop.Run surfaced
	// context.Canceled. The FSM has no auto-cancelled status (Cancel
	// is the external-caller surface and requires a reason); Failed
	// with this code is the closest terminal match —
	TaskErrorCodeCancelled = "cancelled"
)

// TaskErrorCodeForFinish maps a non-goal terminal [FinishReason]
// (NoPath, Cancelled, DeadlineExceeded, ConstraintsConflict, ...) to
// the `tasks.TaskError.Code` a run-loop driver stamps when the run
// reached Finish without satisfying the goal. The mapping is the
// reason's string verbatim — exactly the pre-110a behaviour — so the
// Console / operator sees WHY the run ended without a goal.
func TaskErrorCodeForFinish(reason FinishReason) string {
	return string(reason)
}
