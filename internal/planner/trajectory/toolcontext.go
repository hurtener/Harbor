package trajectory

// HandleID is the opaque key for a non-serialisable tool-context handle
// (live callbacks, loggers, sockets, file descriptors). The actual
// value lives in the runtime's HandleRegistry; the Trajectory only
// carries the HandleID across pause/resume.
//
// HandleIDs are caller-generated (ULID / UUID v4 are the recommended
// conventions). The registry stores values by HandleID and does not
// enforce uniqueness on Set — re-registering an existing HandleID
// overwrites silently (standard Go map semantics).
type HandleID string

// ToolContext is the planner-facing tool-handle bundle. The split
// (RFC §6.3 + brief 02 §4) closes the predecessor's silent-context-
// loss bug:
//
//   - Serializable carries JSON-encodable values shared across tool
//     invocations within a run (configs, IDs, plain values). Persisted
//     across pause/resume via Trajectory.Serialize.
//   - Handles carries opaque HandleIDs. The actual values (callbacks,
//     loggers, sockets) live in the runtime's process-local
//     HandleRegistry. On resume, the runtime re-attaches each handle
//     from the registry by ID; a missing handle surfaces
//     ErrToolContextLost — never silently nil.
//
// Tests for the fail-loudly contract: see toolcontext_test.go +
// serialize_negative_test.go.
type ToolContext struct {
	// Serializable carries the JSON-encodable values shared across
	// tool invocations within a run. Non-JSON-encodable values here
	// cause Trajectory.Serialize to return ErrUnserializable.
	Serializable map[string]any `json:"serializable,omitempty"`

	// Handles carries the keys for non-serialisable values the
	// HandleRegistry holds. The actual values are NEVER stored in
	// this struct — they live in the runtime's HandleRegistry and
	// are re-attached by key on resume.
	Handles []HandleID `json:"handles,omitempty"`
}
