// Package trajectory ships Harbor's append-only planner-execution log
// and the fail-loudly serialise contract that closes the predecessor's
// silent-context-loss bug.
//
// The contract (RFC §6.2 + §3.4):
//
//	Trajectory.Serialize() ([]byte, error)
//
// returns canonical JSON bytes on success; on ANY non-JSON-encodable
// leaf, returns (nil, ErrUnserializable{Field: "..."}). No silent-drop
// path. The reflective walker in serialize.go tracks the field path so
// the error message is actionable.
//
// ToolContext splits into a JSON-encodable Serializable map + an opaque
// HandleID slice. The actual non-serialisable values (callbacks,
// loggers, sockets) live in the runtime's HandleRegistry, which V1
// implements as a process-local sync.Map. Resume with a missing handle
// surfaces ErrToolContextLost — never (nil, nil).
//
// Round-trip is byte-stable: Serialize → Deserialize → Serialize
// returns identical bytes. The invariant is asserted in
// trajectory_test.go via golden bytes.
//
// Phase 43 is the load-bearing predecessor-bug closure. The fail-loudly
// tests in serialize_negative_test.go + toolcontext_test.go are the
// gate.
package trajectory

import (
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// Trajectory is the append-only execution log a planner sees as the
// run progresses. The Planner reads the trajectory (prior steps,
// summary, sources); the Runtime appends each step. Concurrent access
// between planner-reads and runtime-appends is the Runtime's
// responsibility — implementations of a planner-step orchestrator
// MUST serialise the two.
//
// All fields carry explicit JSON tags so Serialize / Deserialize
// produce byte-stable output. The struct-field order is canonical: the
// tag set defines the on-wire shape.
type Trajectory struct {
	// Query is the user-facing query that started the run.
	Query string `json:"query,omitempty"`

	// LLMContext is the visible-to-LLM context snapshot at run start
	// (memories, system notes, prior turn summaries). Values must be
	// JSON-encodable for Serialize to succeed; a non-encodable leaf
	// surfaces ErrUnserializable.
	LLMContext map[string]any `json:"llm_context,omitempty"`

	// ToolContext is the tool-only handle bundle. The Serializable
	// half is JSON-encoded as part of the trajectory; the Handles
	// slice carries opaque IDs whose actual values live in the
	// HandleRegistry.
	ToolContext ToolContext `json:"tool_context"`

	// Steps is the append-only list of trajectory steps.
	Steps []Step `json:"steps,omitempty"`

	// Summary is the compaction artefact produced by the trajectory
	// summariser (Phase 46). Non-nil when the runtime compressed the
	// trajectory; the planner sees only the compacted view.
	Summary *Summary `json:"summary,omitempty"`

	// Sources captures the citations / provenance for the planner's
	// terminal observation.
	Sources []Source `json:"sources,omitempty"`

	// Artifacts is the run's named artifact references. Values are
	// JSON-encoded ArtifactRefs (content-addressed; the bytes live
	// elsewhere via the ArtifactStore).
	Artifacts map[string]artifacts.ArtifactRef `json:"artifacts,omitempty"`

	// HintState carries planner-internal hint state across steps
	// (e.g. "I last summarised at step 4"). Opaque to the Runtime.
	// Values must be JSON-encodable.
	HintState map[string]any `json:"hint_state,omitempty"`

	// SteeringInputs is the history of steering injections observed
	// during the run.
	SteeringInputs []SteeringInjection `json:"steering_inputs,omitempty"`

	// Background captures the resolved outcomes of non-retain-turn
	// background tasks the planner spawned. Keyed by TaskGroupID
	// string for fast lookup.
	Background map[string]BackgroundResult `json:"background,omitempty"`

	// ResumeHint, when non-nil, signals the planner that this is a
	// resume continuation; the planner SHOULD use the hint to
	// reconstruct prior state.
	ResumeHint *ResumeHint `json:"resume_hint,omitempty"`
}

// Step captures one planner-step's action + observation. The Action
// field carries the planner's Decision shape; it is typed as `any`
// because the planner subpackage owns the Decision sum-type
// (importing it here would create a cycle). Callers serialising
// trajectories pass either typed Decision shapes or canonical map
// representations; round-trip byte stability relies on the latter
// (see Trajectory godoc).
type Step struct {
	// Action is the Decision the planner returned for this step.
	// Typed as `any` to avoid a cycle with the planner package;
	// must be JSON-encodable (struct with JSON tags or
	// map[string]any).
	Action any `json:"action,omitempty"`

	// ReasoningTrace is the provider-side thinking trace captured for
	// this step (Phase 83e — D-147). The planner stamps it from
	// `llm.CompleteResponse.Reasoning` after the step's LLM call.
	// It is captured content, kept for observability and `inspect-runs`
	// — it is NEVER re-injected into a subsequent prompt unless the
	// agent's `ReasoningReplay` mode is `text` (D-148). Empty when the
	// provider surfaced no reasoning. Reasoning content can be
	// sensitive; any sink that persists or logs it routes through the
	// audit redactor (CLAUDE.md §7).
	ReasoningTrace string `json:"reasoning_trace,omitempty"`

	// Observation is the runtime's executed-decision result. Shape
	// depends on the Decision: a CallTool yields a ToolResult; a
	// SpawnTask yields a TaskHandle; etc.
	Observation any `json:"observation,omitempty"`

	// LLMObservation is the projection of Observation that becomes
	// the next prompt's input. Distinct from Observation so heavy
	// blobs can be summarised before reaching the LLM (D-026).
	LLMObservation any `json:"llm_observation,omitempty"`

	// Error captures a step-level failure (tool error, repair
	// failure). Empty when the step succeeded.
	Error string `json:"error,omitempty"`

	// Failure is the structured failure record (Phase 44 repair
	// pipeline populates).
	Failure *FailureRecord `json:"failure,omitempty"`

	// Streams captures per-chunk stream outputs the runtime
	// collected during the step.
	Streams map[string][]StreamChunk `json:"streams,omitempty"`

	// StartedAt is the wall-clock step-start timestamp.
	StartedAt time.Time `json:"started_at,omitempty"`

	// LatencyMS is the step end-to-end latency in milliseconds.
	LatencyMS int64 `json:"latency_ms,omitempty"`

	// TokenEstimate is the LLM token consumption estimate for this
	// step (input + output combined).
	TokenEstimate int `json:"token_estimate,omitempty"`
}

// Summary is the compaction artefact produced by Phase 46's
// summariser. Replaces the raw step history in subsequent prompt
// builds when the trajectory exceeds the configured budget.
type Summary struct {
	// Goals captures the planner's running goal-tracking.
	Goals []string `json:"goals,omitempty"`

	// Facts captures the running fact-list extracted from prior
	// observations.
	Facts []string `json:"facts,omitempty"`

	// Pending captures the open subgoals.
	Pending []string `json:"pending,omitempty"`

	// LastOutputDigest is a short hash + summary of the most recent
	// observation, kept so the planner has context for the next step.
	LastOutputDigest string `json:"last_output_digest,omitempty"`

	// Note is the summariser's free-text note (rationale for the
	// compaction, surfaced in observability).
	Note string `json:"note,omitempty"`
}

// Source records a citation / provenance entry for the planner's
// terminal observation.
type Source struct {
	// Kind is the source kind: "tool" / "memory" / "skill" /
	// "user_message" / "artifact".
	Kind string `json:"kind"`

	// Ref is the source-specific reference (tool name + step index;
	// memory key; skill id; etc.).
	Ref string `json:"ref"`
}

// SteeringInjection records a steering event the planner observed.
type SteeringInjection struct {
	// Kind is the control event type (CtlInjectContext, CtlRedirect, etc).
	Kind string `json:"kind"`

	// Payload is the sanitised payload the planner sees.
	Payload map[string]any `json:"payload,omitempty"`

	// AtStep is the trajectory step index at which the injection
	// was observed.
	AtStep int `json:"at_step"`
}

// BackgroundResult is the planner's projection of a resolved
// non-retain-turn task group. The Runtime populates from
// `tasks.GroupCompletion` when the group resolves; the planner reads
// to integrate the outcome into the next prompt.
type BackgroundResult struct {
	// GroupID is the resolved group's identifier.
	GroupID string `json:"group_id"`

	// Status is the group's terminal status ("completed" /
	// "cancelled").
	Status string `json:"status"`

	// ResolvedAt is the resolution wall-clock timestamp.
	ResolvedAt time.Time `json:"resolved_at,omitempty"`

	// Members is the per-member outcome summary (Result / Error /
	// Cancelled, ref-shaped per D-026).
	Members []BackgroundMemberOutcome `json:"members,omitempty"`

	// Reason is the cancel reason when Status == "cancelled";
	// empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// BackgroundMemberOutcome is the planner's projection of one task's
// terminal record inside a BackgroundResult.
type BackgroundMemberOutcome struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`

	// ResultRef is the ArtifactRef key for the task result, or empty
	// when the task did not produce a heavy result.
	ResultRef string `json:"result_ref,omitempty"`

	// ErrorCode is the failure code when Status == "failed".
	ErrorCode string `json:"error_code,omitempty"`

	// ErrorMessage is the human-readable failure message when
	// Status == "failed".
	ErrorMessage string `json:"error_message,omitempty"`
}

// ResumeHint signals the planner that this is a resume continuation.
// The unified pause/resume primitive (Phase 50) populates the hint
// when re-invoking the planner after a pause.
type ResumeHint struct {
	// PauseToken is the opaque token the runtime issued at pause time.
	PauseToken string `json:"pause_token"`

	// ResumedAt is the wall-clock resume timestamp.
	ResumedAt time.Time `json:"resumed_at,omitempty"`

	// ResumePayload is the sanitised payload the resumer supplied
	// (APPROVE/REJECT decision, USER_MESSAGE content, etc.).
	ResumePayload map[string]any `json:"resume_payload,omitempty"`
}

// FailureRecord is Phase 44's structured-failure projection. The
// repair pipeline populates the fields.
type FailureRecord struct {
	// Code is the failure classification ("schema_repair_exhausted",
	// "arg_fill_failed", "graceful_failure", etc.).
	Code string `json:"code"`

	// Message is the human-readable failure message.
	Message string `json:"message"`

	// Attempts is the count of repair attempts before giving up.
	Attempts int `json:"attempts"`
}

// StreamChunk captures one chunk of a streaming tool / LLM output.
// The runtime streaming subsystem populates the slices during step
// execution.
type StreamChunk struct {
	// At is the wall-clock chunk-arrival timestamp.
	At time.Time `json:"at,omitempty"`

	// Data is the raw chunk payload (typically text bytes).
	Data []byte `json:"data,omitempty"`

	// Final is true on the terminating chunk.
	Final bool `json:"final,omitempty"`
}
