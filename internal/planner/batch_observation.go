package planner

// BatchObservation is the aggregate observation a runtime ToolExecutor
// produces when it dispatches a [Batch] decision. A batch has two
// heterogeneous halves — catalog-tool branches and task spawns — and
// this observation carries both, each index-aligned to the originating
// [Batch.Tools] / [Batch.Spawns] declaration order REGARDLESS of the
// order the branches or spawns actually complete in. The caller MUST
// index or look up by CallID / Index to reconstruct per-`call_id`
// replies — never range a Go map — because provider tool-calling wire
// contracts require exactly one reply per `call_id`, some of them
// order-sensitive.
//
// The shape lives in `internal/planner` (not the runtime executor's
// package) for the same reason [ParallelObservation] does: BOTH sides
// of the round-trip consume it — the production ToolExecutor produces
// it as the step's Observation / LLMObservation, and the React prompt
// builder reads it back to emit the per-branch `role:"tool"` messages.
// Both packages already import `internal/planner`, so placing the type
// here avoids a new cross-package dependency.
//
// JSON-encodable: the trajectory persists Step.Observation /
// Step.LLMObservation across checkpoints, so every field carries a JSON
// tag and the embedded tool `Value`s hold only JSON-encodable results.
type BatchObservation struct {
	// Tools is the per-tool-branch outcome slice, index-aligned to
	// [Batch.Tools]. It reuses [ParallelBranchObservation] verbatim —
	// the tool half of a Batch dispatches through the SAME executor path
	// a [CallParallel] does, so its per-branch observation shape is
	// identical.
	Tools []ParallelBranchObservation `json:"tools,omitempty"`

	// Spawns is the per-spawn-branch REGISTRATION outcome slice,
	// index-aligned to [Batch.Spawns].
	Spawns []BatchSpawnObservation `json:"spawns,omitempty"`

	// Progress is keyed by the native progress call's CallID and index.
	Progress []BatchProgressObservation `json:"progress,omitempty"`
}

// BatchProgressObservation is the result of one native TaskProgress call.
type BatchProgressObservation struct {
	CallID   string `json:"call_id,omitempty"`
	Index    int    `json:"index"`
	Recorded bool   `json:"recorded"`
	Emitted  bool   `json:"emitted"`
	Error    string `json:"error,omitempty"`
}

// BatchSpawnObservation is one [Batch.Spawns] entry's REGISTRATION
// outcome. "Complete" here means REGISTERED, not terminal: a spawn's
// eventual result arrives later via the task registry's group-watch
// path, never through this observation — so a spawn is never reported
// as "not done yet". Exactly one of (TaskID+GroupID) or Error is
// populated: a successful registration carries the assigned task id and
// its group id; a registry reject (depth-cap exceeded, sealed group,
// malformed request) carries that spawn's error as a value, so every
// `call_id` is answered even when one spawn is rejected.
type BatchSpawnObservation struct {
	// CallID is the provider-assigned tool-call identifier sourced from
	// the originating [Batch.Spawns][Index].CallID (the native
	// `_spawn_task` call's id). The prompt builder stamps it onto the
	// matching `role:"tool"` message's ToolCallID. Empty when the spawn
	// carried no provider id (a programmatic spawn); the renderer then
	// falls back to a deterministic synthetic id keyed on Index.
	CallID string `json:"call_id,omitempty"`

	// Index is the spawn's position in the originating [Batch.Spawns]
	// slice. The deterministic merge key: the renderer pairs each
	// spawn's observation to its assistant tool-call by Index regardless
	// of CallID collisions.
	Index int `json:"index"`

	// TaskID is the registered task's id on a successful registration.
	// Empty on a registry reject.
	TaskID string `json:"task_id,omitempty"`

	// GroupID is the registered task's group id on a successful
	// registration — either the spawn's explicit group or the one
	// auto-created group the executor resolves for a batch's ungrouped
	// spawns. Empty on a registry reject, and empty for a single
	// ungrouped spawn that kept the ad-hoc single-member-group path.
	GroupID string `json:"group_id,omitempty"`

	// Error is the spawn's registration failure message. Non-empty only
	// on a registry reject; empty on success.
	Error string `json:"error,omitempty"`
}
