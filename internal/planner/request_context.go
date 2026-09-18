package planner

// RequestContextPlanner supplies a message rebuild callback to the composed LLM
// client, so the runtime can compact using the assembled request. Planners that
// do not build LLM requests retain standalone trajectory-based compression.
// This does not change Planner.Next or move compaction policy into a planner.
type RequestContextPlanner interface {
	Planner
	PreparesRequestContext()
}
