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
	LLMContext     map[string]any                   `json:"llm_context,omitempty"`
	Summary        *Summary                         `json:"summary,omitempty"`
	Artifacts      map[string]artifacts.ArtifactRef `json:"artifacts,omitempty"`
	HintState      map[string]any                   `json:"hint_state,omitempty"`
	Background     map[string]BackgroundResult      `json:"background,omitempty"`
	ResumeHint     *ResumeHint                      `json:"resume_hint,omitempty"`
	Query          string                           `json:"query,omitempty"`
	ToolContext    ToolContext                      `json:"tool_context"`
	Steps          []Step                           `json:"steps,omitempty"`
	Sources        []Source                         `json:"sources,omitempty"`
	SteeringInputs []SteeringInjection              `json:"steering_inputs,omitempty"`
}

// Step captures one planner-step's action + observation. The Action
// field carries the planner's Decision shape; it is typed as `any`
// because the planner subpackage owns the Decision sum-type
// (importing it here would create a cycle). Callers serialising
// trajectories pass either typed Decision shapes or canonical map
// representations; round-trip byte stability relies on the latter
// (see Trajectory godoc).
type Step struct {
	StartedAt      time.Time                `json:"started_at,omitempty"`
	Action         any                      `json:"action,omitempty"`
	Observation    any                      `json:"observation,omitempty"`
	LLMObservation any                      `json:"llm_observation,omitempty"`
	Failure        *FailureRecord           `json:"failure,omitempty"`
	Streams        map[string][]StreamChunk `json:"streams,omitempty"`
	Error          string                   `json:"error,omitempty"`
	LatencyMS      int64                    `json:"latency_ms,omitempty"`
	TokenEstimate  int                      `json:"token_estimate,omitempty"`
}

// Summary is the compaction artefact produced by Phase 46's
// summariser. Replaces the raw step history in subsequent prompt
// builds when the trajectory exceeds the configured budget.
type Summary struct {
	LastOutputDigest string   `json:"last_output_digest,omitempty"`
	Note             string   `json:"note,omitempty"`
	Goals            []string `json:"goals,omitempty"`
	Facts            []string `json:"facts,omitempty"`
	Pending          []string `json:"pending,omitempty"`
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
	Payload map[string]any `json:"payload,omitempty"`
	Kind    string         `json:"kind"`
	AtStep  int            `json:"at_step"`
}

// BackgroundResult is the planner's projection of a resolved
// non-retain-turn task group. The Runtime populates from
// `tasks.GroupCompletion` when the group resolves; the planner reads
// to integrate the outcome into the next prompt.
type BackgroundResult struct {
	ResolvedAt time.Time                 `json:"resolved_at,omitempty"`
	GroupID    string                    `json:"group_id"`
	Status     string                    `json:"status"`
	Reason     string                    `json:"reason,omitempty"`
	Members    []BackgroundMemberOutcome `json:"members,omitempty"`
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
	ResumedAt     time.Time      `json:"resumed_at,omitempty"`
	ResumePayload map[string]any `json:"resume_payload,omitempty"`
	PauseToken    string         `json:"pause_token"`
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
