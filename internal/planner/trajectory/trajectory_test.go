package trajectory_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TestSerialize_HappyPath_EmptyTrajectory — a zero-value Trajectory
// serialises cleanly to a minimal JSON object (every field omits via
// `omitempty` except ToolContext which always serialises).
func TestSerialize_HappyPath_EmptyTrajectory(t *testing.T) {
	tr := &trajectory.Trajectory{}
	out, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(empty) err = %v want nil", err)
	}
	if len(out) == 0 {
		t.Fatalf("Serialize(empty) returned empty bytes")
	}
	// Spot-check shape: the bytes are valid JSON and parse to an
	// object (not null, not an array).
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("Serialize(empty) emitted non-JSON-object bytes: %v\nbytes=%s", err, out)
	}
}

// TestSerialize_HappyPath_Populated — a populated Trajectory with
// JSON-tree shapes in every `any`-valued field serialises and
// deserialises cleanly.
func TestSerialize_HappyPath_Populated(t *testing.T) {
	tr := buildSampleTrajectory()
	out, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(populated) err = %v want nil", err)
	}
	if len(out) == 0 {
		t.Fatalf("Serialize(populated) returned empty bytes")
	}

	// Deserialize must accept the bytes and produce a non-nil
	// *Trajectory.
	back, err := trajectory.Deserialize(out)
	if err != nil {
		t.Fatalf("Deserialize err = %v want nil", err)
	}
	if back == nil {
		t.Fatalf("Deserialize returned nil *Trajectory")
	}
	if back.Query != tr.Query {
		t.Errorf("Query: got %q want %q", back.Query, tr.Query)
	}
}

// TestRoundTrip_ByteStable — the canonical invariant: Serialize →
// Deserialize → Serialize is byte-identical. This is the
// load-bearing acceptance criterion from RFC §3.4 + brief 02 §4.
func TestRoundTrip_ByteStable(t *testing.T) {
	tr := buildSampleTrajectory()
	first, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize first pass err = %v", err)
	}
	back, err := trajectory.Deserialize(first)
	if err != nil {
		t.Fatalf("Deserialize err = %v", err)
	}
	second, err := back.Serialize()
	if err != nil {
		t.Fatalf("Serialize second pass err = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("round-trip byte-stable invariant violated:\n  first  = %s\n  second = %s",
			first, second)
	}
}

// TestRoundTrip_ByteStable_NilHandles — handles slice is nil/empty;
// the omitempty tag drops it; round-trip stays byte-stable.
func TestRoundTrip_ByteStable_NilHandles(t *testing.T) {
	tr := &trajectory.Trajectory{
		Query:       "minimal",
		ToolContext: trajectory.ToolContext{Serializable: map[string]any{"k": "v"}},
	}
	first, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize first = %v", err)
	}
	back, err := trajectory.Deserialize(first)
	if err != nil {
		t.Fatalf("Deserialize = %v", err)
	}
	second, err := back.Serialize()
	if err != nil {
		t.Fatalf("Serialize second = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("round-trip not byte-stable:\n  first  = %s\n  second = %s", first, second)
	}
}

// TestRoundTrip_ByteStable_HandlesPersist — Handles slice round-trips
// as a JSON string array. The actual values live in the registry; the
// trajectory only carries the IDs.
func TestRoundTrip_ByteStable_HandlesPersist(t *testing.T) {
	tr := &trajectory.Trajectory{
		Query: "with-handles",
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"endpoint": "https://example.test"},
			Handles:      []trajectory.HandleID{"h-1", "h-2", "h-3"},
		},
	}
	first, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize first = %v", err)
	}
	back, err := trajectory.Deserialize(first)
	if err != nil {
		t.Fatalf("Deserialize = %v", err)
	}
	if len(back.ToolContext.Handles) != 3 {
		t.Fatalf("Handles lost: got %d want 3", len(back.ToolContext.Handles))
	}
	if back.ToolContext.Handles[0] != "h-1" {
		t.Errorf("Handles[0] = %q want %q", back.ToolContext.Handles[0], "h-1")
	}
	second, err := back.Serialize()
	if err != nil {
		t.Fatalf("Serialize second = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("round-trip not byte-stable")
	}
}

// TestSerialize_NilTrajectory_FailsLoudly — a nil *Trajectory is
// itself unserialisable and surfaces ErrUnserializable. No
// silent-nil-bytes path.
func TestSerialize_NilTrajectory_FailsLoudly(t *testing.T) {
	var tr *trajectory.Trajectory
	out, err := tr.Serialize()
	if err == nil {
		t.Fatalf("nil.Serialize returned nil error — fail-loudly contract violated")
	}
	if out != nil {
		t.Fatalf("nil.Serialize returned non-nil bytes")
	}
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("nil.Serialize err = %v want ErrUnserializable", err)
	}
}

// TestDeserialize_EmptyInput — empty bytes return a clear error,
// never a nil pointer with nil error.
func TestDeserialize_EmptyInput(t *testing.T) {
	_, err := trajectory.Deserialize(nil)
	if err == nil {
		t.Fatalf("Deserialize(nil) returned nil error — empty input must fail")
	}
	_, err = trajectory.Deserialize([]byte{})
	if err == nil {
		t.Fatalf("Deserialize(empty) returned nil error")
	}
}

// TestDeserialize_MalformedJSON — malformed JSON surfaces a parse
// error, not a silent zero-value trajectory.
func TestDeserialize_MalformedJSON(t *testing.T) {
	_, err := trajectory.Deserialize([]byte(`{not json`))
	if err == nil {
		t.Fatalf("Deserialize(malformed) returned nil error")
	}
}

// TestSerialize_GoldenBytes — a minimal stable example. The golden
// bytes pin the canonical encoding so accidental field renames /
// reorderings surface as a clear diff.
func TestSerialize_GoldenBytes(t *testing.T) {
	tr := &trajectory.Trajectory{
		Query: "what is harbor?",
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"alpha": "a", "beta": "b"},
			Handles:      []trajectory.HandleID{"h-x"},
		},
	}
	out, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize = %v", err)
	}

	// The canonical encoding: struct fields in declaration order,
	// map keys alphabetised. Top-level: query / tool_context (then
	// every other field is omitempty / empty).
	want := `{"query":"what is harbor?","tool_context":{"serializable":{"alpha":"a","beta":"b"},"handles":["h-x"]}}`
	if string(out) != want {
		t.Fatalf("golden bytes mismatch:\n  got  = %s\n  want = %s", out, want)
	}
}

// TestSerialize_ZeroValueTimeRoundTrip — time.Time zero values are
// json-encodable and round-trip cleanly.
func TestSerialize_ZeroValueTimeRoundTrip(t *testing.T) {
	tr := &trajectory.Trajectory{
		Steps: []trajectory.Step{{
			Error:     "test-error",
			StartedAt: time.Time{}, // zero
		}},
	}
	out, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize = %v", err)
	}
	back, err := trajectory.Deserialize(out)
	if err != nil {
		t.Fatalf("Deserialize = %v", err)
	}
	if len(back.Steps) != 1 {
		t.Fatalf("Steps lost")
	}
	if back.Steps[0].Error != "test-error" {
		t.Errorf("Error: got %q want %q", back.Steps[0].Error, "test-error")
	}
}

// TestSerialize_TimePopulated — a populated time round-trips correctly.
func TestSerialize_TimePopulated(t *testing.T) {
	when := time.Date(2026, time.May, 12, 10, 0, 0, 0, time.UTC)
	tr := &trajectory.Trajectory{
		Steps: []trajectory.Step{{StartedAt: when, LatencyMS: 42}},
	}
	out, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize = %v", err)
	}
	back, err := trajectory.Deserialize(out)
	if err != nil {
		t.Fatalf("Deserialize = %v", err)
	}
	if !back.Steps[0].StartedAt.Equal(when) {
		t.Errorf("StartedAt: got %v want %v", back.Steps[0].StartedAt, when)
	}
	if back.Steps[0].LatencyMS != 42 {
		t.Errorf("LatencyMS: got %d want 42", back.Steps[0].LatencyMS)
	}
}

// buildSampleTrajectory constructs a Trajectory using JSON-tree shapes
// throughout — the discipline that guarantees byte-stable round-trip.
func buildSampleTrajectory() *trajectory.Trajectory {
	return &trajectory.Trajectory{
		Query: "investigate the topology",
		LLMContext: map[string]any{
			"goal":      "summarise",
			"max_steps": float64(10),
			"flags":     []any{"verbose", "json"},
		},
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{
				"tool_endpoint": "https://api.test/v1",
				"timeout_ms":    float64(5000),
			},
			Handles: []trajectory.HandleID{"h-callback-1"},
		},
		Steps: []trajectory.Step{
			{
				Action: map[string]any{
					"kind":   "call_tool",
					"tool":   "search",
					"args":   map[string]any{"q": "topology"},
					"reason": "user asked",
				},
				Observation:    map[string]any{"hits": float64(3)},
				LLMObservation: map[string]any{"summary": "3 hits"},
				Error:          "",
				StartedAt:      time.Time{},
				LatencyMS:      42,
				TokenEstimate:  128,
			},
		},
		Summary: &trajectory.Summary{
			Goals:            []string{"summarise"},
			Facts:            []string{"3 hits"},
			Pending:          nil,
			LastOutputDigest: "abc123",
			Note:             "compacted at step 1",
		},
		Sources: []trajectory.Source{
			{Kind: "tool", Ref: "search/0"},
		},
		HintState: map[string]any{"last_summary_step": float64(1)},
		SteeringInputs: []trajectory.SteeringInjection{
			{
				Kind:    "inject_context",
				Payload: map[string]any{"hint": "be brief"},
				AtStep:  0,
			},
		},
		Background: map[string]trajectory.BackgroundResult{
			"g-1": {
				GroupID: "g-1",
				Status:  "completed",
				Members: []trajectory.BackgroundMemberOutcome{
					{TaskID: "t-1", Status: "completed", ResultRef: "art-1"},
				},
			},
		},
	}
}
