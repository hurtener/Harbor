package planner

import (
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// Trajectory is the append-only execution log a planner sees as the
// run progresses. Phase 42 ships the SKELETON: the struct shape +
// stub Serialize that returns ErrTrajectoryNotImplemented. Phase 43
// closes the fail-loudly Serialize contract (RFC §6.2 + §3.4):
//
//	Trajectory.Serialize() ([]byte, error)
//
// MUST return (nil, ErrUnserializable{Field:...}) on any
// non-JSON-encodable entry. There is no silent-drop path. The
// stub at Phase 42 returns ErrTrajectoryNotImplemented so callers
// fail loudly rather than receive an empty payload.
//
// The Planner READS the trajectory (prior steps, summary, sources);
// the Runtime appends each step. Concurrent access between planner-
// reads and runtime-appends is the Runtime's responsibility —
// implementations of a planner-step orchestrator MUST serialise the
// two.
type Trajectory struct {
	// Query is the user-facing query that started the run.
	Query string
	// LLMContext is the visible-to-LLM context snapshot at run start.
	LLMContext map[string]any
	// ToolContext is the tool-only handle bundle. Phase 43 closes
	// the serialisable/handle split.
	ToolContext ToolContext
	// Steps is the append-only list of trajectory steps.
	Steps []TrajectoryStep
	// Summary is the compaction artefact produced by the trajectory
	// summariser (Phase 46). Non-nil when the runtime compressed the
	// trajectory; the planner sees only the compacted view.
	Summary *TrajectorySummary
	// Sources captures the citations / provenance for the planner's
	// terminal observation.
	Sources []Source
	// Artifacts is the run's named artifact references.
	Artifacts map[string]artifacts.ArtifactRef
	// HintState carries planner-internal hint state across steps
	// (e.g. "I last summarised at step 4"). Opaque to the Runtime.
	HintState map[string]any
	// SteeringInputs is the history of steering injections observed
	// during the run.
	SteeringInputs []SteeringInjection
	// Background captures the resolved outcomes of non-retain-turn
	// background tasks the planner spawned. Keyed by TaskGroupID
	// string for fast lookup.
	Background map[string]BackgroundResult
	// ResumeHint, when non-nil, signals the planner that this is a
	// resume continuation; the planner SHOULD use the hint to
	// reconstruct prior state.
	ResumeHint *ResumeHint
}

// Serialize is the fail-loudly serialisation entry-point. Phase 43
// implements the contract; Phase 42 ships a stub that returns
// (nil, ErrTrajectoryNotImplemented).
//
// Once Phase 43 lands: returns the JSON-encoded byte representation
// or (nil, ErrUnserializable{Field:...}) when any entry fails JSON
// encoding. There is no silent-drop path — the predecessor's
// silent-context-loss bug is closed here.
func (t *Trajectory) Serialize() ([]byte, error) {
	return nil, ErrTrajectoryNotImplemented
}

// TrajectoryStep captures one planner-step's action + observation.
type TrajectoryStep struct {
	// Action is the Decision the planner returned for this step.
	Action Decision
	// Observation is the runtime's executed-decision result. Shape
	// depends on the Decision: a CallTool yields a ToolResult; a
	// SpawnTask yields a TaskHandle; etc.
	Observation any
	// LLMObservation is the projection of Observation that becomes
	// the next prompt's input. Distinct from Observation so heavy
	// blobs can be summarised before reaching the LLM (D-026).
	LLMObservation any
	// Error captures a step-level failure (tool error, repair
	// failure). Empty when the step succeeded.
	Error string
	// Failure is the structured failure record (Phase 44 repair
	// pipeline populates).
	Failure *FailureRecord
	// Streams captures per-chunk stream outputs the runtime
	// collected during the step.
	Streams map[string][]StreamChunk
	// StartedAt is the wall-clock step-start timestamp.
	StartedAt time.Time
	// LatencyMS is the step end-to-end latency in milliseconds.
	LatencyMS int64
	// TokenEstimate is the LLM token consumption estimate for this
	// step (input + output combined).
	TokenEstimate int
}

// TrajectorySummary is the compaction artefact produced by Phase 46's
// summariser. Replaces the raw step history in subsequent prompt
// builds when the trajectory exceeds the configured budget.
type TrajectorySummary struct {
	// Goals captures the planner's running goal-tracking.
	Goals []string
	// Facts captures the running fact-list extracted from prior
	// observations.
	Facts []string
	// Pending captures the open subgoals.
	Pending []string
	// LastOutputDigest is a short hash + summary of the most recent
	// observation, kept so the planner has context for the next step.
	LastOutputDigest string
	// Note is the summariser's free-text note (rationale for the
	// compaction, surfaced in observability).
	Note string
}

// Source records a citation / provenance entry for the planner's
// terminal observation.
type Source struct {
	// Kind is the source kind: "tool" / "memory" / "skill" /
	// "user_message" / "artifact".
	Kind string
	// Ref is the source-specific reference (tool name + step index;
	// memory key; skill id; etc.).
	Ref string
}

// SteeringInjection records a steering event the planner observed.
type SteeringInjection struct {
	// Kind is the control event type (CtlInjectContext, CtlRedirect, etc).
	Kind string
	// Payload is the sanitised payload the planner sees.
	Payload map[string]any
	// AtStep is the trajectory step index at which the injection
	// was observed.
	AtStep int
}

// BackgroundResult is the planner's projection of a resolved
// non-retain-turn task group. The Runtime populates from
// `tasks.GroupCompletion` when the group resolves; the planner reads
// to integrate the outcome into the next prompt.
type BackgroundResult struct {
	// GroupID is the resolved group's identifier.
	GroupID string
	// Status is the group's terminal status ("completed" /
	// "cancelled").
	Status string
	// ResolvedAt is the resolution wall-clock timestamp.
	ResolvedAt time.Time
	// Members is the per-member outcome summary (Result / Error /
	// Cancelled, ref-shaped per D-026).
	Members []BackgroundMemberOutcome
	// Reason is the cancel reason when Status == "cancelled";
	// empty otherwise.
	Reason string
}

// BackgroundMemberOutcome is the planner's projection of one task's
// terminal record inside a BackgroundResult.
type BackgroundMemberOutcome struct {
	TaskID string
	Status string
	// ResultRef is the ArtifactRef key for the task result, or empty
	// when the task did not produce a heavy result.
	ResultRef string
	// ErrorCode is the failure code when Status == "failed".
	ErrorCode string
	// ErrorMessage is the human-readable failure message when
	// Status == "failed".
	ErrorMessage string
}

// ResumeHint signals the planner that this is a resume continuation.
// The unified pause/resume primitive (later phase) populates the
// hint when re-invoking the planner after a pause.
type ResumeHint struct {
	// PauseToken is the opaque token the runtime issued at pause time.
	PauseToken string
	// ResumedAt is the wall-clock resume timestamp.
	ResumedAt time.Time
	// ResumePayload is the sanitised payload the resumer supplied
	// (APPROVE/REJECT decision, USER_MESSAGE content, etc.).
	ResumePayload map[string]any
}

// FailureRecord is Phase 44's structured-failure projection. The
// repair pipeline populates the fields; Phase 42 ships the shape so
// callers can compile against it.
type FailureRecord struct {
	// Code is the failure classification ("schema_repair_exhausted",
	// "arg_fill_failed", "graceful_failure", etc.).
	Code string
	// Message is the human-readable failure message.
	Message string
	// Attempts is the count of repair attempts before giving up.
	Attempts int
}

// StreamChunk captures one chunk of a streaming tool / LLM output.
// Phase 42 ships the shape; the runtime streaming subsystem
// populates the slices during step execution.
type StreamChunk struct {
	// At is the wall-clock chunk-arrival timestamp.
	At time.Time
	// Data is the raw chunk payload (typically text bytes).
	Data []byte
	// Final is true on the terminating chunk.
	Final bool
}

// ToolContext is the planner-facing tool-handle bundle. Phase 43
// closes the serialisable/handle split (RFC §6.3 — closes the
// predecessor's silent-context-loss bug):
//
//   - Serialisable half: IDs, configs, plain values. Serialises via
//     standard JSON.
//   - Non-serialisable half: live callbacks, loggers, sockets, file
//     handles. Registered with the Runtime under a Handle key; on
//     resume the handle is re-attached from the live registry.
//
// Phase 42 ships the skeleton: a flat map for the serialisable half
// and a Handles map keyed by string. The fail-loudly Serialize and
// the handle-registry round-trip arrive at Phase 43.
type ToolContext struct {
	// Serializable carries the JSON-encodable values shared across
	// tool invocations within a run (configs, IDs, plain values).
	Serializable map[string]any
	// Handles carries the keys for non-serialisable values the
	// Runtime registry holds. The actual values are NEVER stored in
	// this struct — they live in the Runtime's handle registry and
	// are re-attached by key on resume.
	Handles map[string]string
}
