package planner

// ParallelObservation is the aggregate observation a runtime
// ToolExecutor produces when it dispatches a [CallParallel] decision
// (Phase 107d — D-169). It carries one [ParallelBranchObservation] per
// branch, in branch-index order (JoinAll semantics), so the planner's
// trajectory replay can decompose it back into the N native
// `role:"tool"` messages the provider wire contract requires — exactly
// one answer per `tool_call_id`.
//
// The shape lives in `internal/planner` (not the runtime executor's
// package) because BOTH sides of the round-trip consume it: the dev
// `ToolExecutor` (cmd/harbor) produces it as the step's Observation /
// LLMObservation, and the React prompt builder
// (`internal/planner/react`) reads it back to emit the per-branch
// `RoleTool` messages. Both packages already import `internal/planner`;
// placing the type here avoids a new cross-package dependency.
//
// JSON-encodable: the trajectory persists Step.Observation /
// Step.LLMObservation across checkpoints, so every field carries a JSON
// tag and `Value` holds only JSON-encodable tool results.
type ParallelObservation struct {
	// Branches is the per-branch outcome slice in branch-index order.
	Branches []ParallelBranchObservation `json:"branches,omitempty"`
}

// ParallelBranchObservation is one branch's outcome inside a
// [ParallelObservation]. Exactly one of `Value` (success) or `Error`
// (failure) is populated — never both.
type ParallelBranchObservation struct {
	// CallID is the provider-assigned tool-call identifier sourced from
	// the originating `CallParallel.Branches[Index].CallID`. The prompt
	// builder stamps it onto the matching `RoleTool` message's
	// ToolCallID so the assistant `tool_calls[i]` and its answer pair
	// up. Empty when the branch carried no provider ID (the renderer
	// then falls back to a deterministic synthetic ID keyed on Index).
	CallID string `json:"call_id,omitempty"`

	// Tool is the branch's tool name (same as CallParallel.Branches[Index].Tool).
	Tool string `json:"tool,omitempty"`

	// Index is the branch's position in the originating
	// `CallParallel.Branches` slice. The deterministic merge key: the
	// renderer pairs each branch's observation to its assistant
	// tool-call by Index regardless of CallID collisions.
	Index int `json:"index"`

	// Value is the branch's success result. For the raw aggregate it is
	// the untruncated tool value; for the LLM-facing aggregate it is the
	// D-026 projected form (heavy results replaced by an artifact-stub
	// summary). Nil on failure.
	Value any `json:"value,omitempty"`

	// Error is the branch's failure message. Non-empty only on failure
	// (resolve miss, args-validation failure, or tool Invoke error).
	Error string `json:"error,omitempty"`
}
