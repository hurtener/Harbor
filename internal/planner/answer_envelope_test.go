package planner

import (
	"encoding/json"
	"testing"
)

// TestAnswerEnvelope_GoldenJSON_Phase106ByteCompat pins the envelope's
// JSON encoding byte-for-byte against the Phase 106 shape — the
// `map[string]any{"answer": ..., "finish_reason": ..., "tool_calls_seen": ...}`
// literal the pre-110a run-loop driver marshalled (Go marshals map keys
// in sorted order; the struct's field order reproduces it). A failure
// here means the cmd↔cmd wire contract drifted: tasks.get's
// result_inline and the AwaitTask observation parse would change shape.
func TestAnswerEnvelope_GoldenJSON_Phase106ByteCompat(t *testing.T) {
	t.Parallel()
	env := AnswerEnvelope{
		Answer:        "the assistant answer",
		FinishReason:  string(FinishGoal),
		ToolCallsSeen: 3,
	}
	got, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal AnswerEnvelope: %v", err)
	}

	// The exact Phase 106 producer shape, marshalled the pre-110a way.
	legacy, err := json.Marshal(map[string]any{
		"answer":          "the assistant answer",
		"finish_reason":   string(FinishGoal),
		"tool_calls_seen": 3,
	})
	if err != nil {
		t.Fatalf("marshal legacy map: %v", err)
	}
	if string(got) != string(legacy) {
		t.Errorf("AnswerEnvelope encoding drifted from the Phase 106 shape:\n got: %s\nwant: %s", got, legacy)
	}

	// The golden literal, pinned explicitly so a change to BOTH sides
	// still fails loud. The additive answer_payload key is `omitempty`,
	// so a plain (no-payload) envelope is byte-identical to before —
	// zero default-path change (D-272).
	const golden = `{"answer":"the assistant answer","finish_reason":"goal","tool_calls_seen":3}`
	if string(got) != golden {
		t.Errorf("AnswerEnvelope encoding = %s, want golden %s", got, golden)
	}
}

// TestAnswerEnvelope_GoldenJSON_WithPayload pins the encoding of a
// structured-output envelope: the additive answer_payload key carries
// the validated raw JSON and appears LAST (struct field order), while
// the three pre-existing keys are unchanged. A plain run omits the key
// entirely (the byte-compat test above); this fixture pins the shape
// when a schema constrained the run (D-272).
func TestAnswerEnvelope_GoldenJSON_WithPayload(t *testing.T) {
	t.Parallel()
	env := AnswerEnvelope{
		Answer:        `{"sentiment":"positive","score":0.9}`,
		FinishReason:  string(FinishGoal),
		ToolCallsSeen: 0,
		AnswerPayload: json.RawMessage(`{"sentiment":"positive","score":0.9}`),
	}
	got, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal AnswerEnvelope: %v", err)
	}
	const golden = `{"answer":"{\"sentiment\":\"positive\",\"score\":0.9}","finish_reason":"goal","tool_calls_seen":0,"answer_payload":{"sentiment":"positive","score":0.9}}`
	if string(got) != golden {
		t.Errorf("with-payload AnswerEnvelope encoding = %s, want golden %s", got, golden)
	}

	// Round-trip: the payload decodes back as raw JSON bytes.
	var back AnswerEnvelope
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(back.AnswerPayload) != `{"sentiment":"positive","score":0.9}` {
		t.Errorf("round-trip AnswerPayload = %s, want the raw JSON", back.AnswerPayload)
	}
}

// TestAnswerEnvelope_RoundTrip asserts the snake_case keys decode back
// into the typed struct (the consumer direction: tasks.get projectors
// and AwaitTask observations parse what run-loop drivers marshal).
func TestAnswerEnvelope_RoundTrip(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"answer":"a","finish_reason":"no_path","tool_calls_seen":7}`)
	var env AnswerEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Answer != "a" || env.FinishReason != string(FinishNoPath) || env.ToolCallsSeen != 7 {
		t.Errorf("round-trip = %+v, want the wire values", env)
	}
}

// TestAnswerEnvelope_TaskErrorCodeForFinish — table test: non-goal
// terminal Finish reasons map to their string verbatim (the pre-110a
// behaviour), and the run-error constants carry the documented values.
func TestAnswerEnvelope_TaskErrorCodeForFinish(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason FinishReason
		want   string
	}{
		{FinishNoPath, "no_path"},
		{FinishCancelled, "cancelled"},
		{FinishDeadlineExceeded, "deadline_exceeded"},
		{FinishConstraintsConflict, "constraints_conflict"},
		{FinishGoal, "goal"}, // verbatim even for goal (callers gate on it before mapping)
	}
	for _, tc := range cases {
		if got := TaskErrorCodeForFinish(tc.reason); got != tc.want {
			t.Errorf("TaskErrorCodeForFinish(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
	if TaskErrorCodeRunLoopError != "runloop_error" {
		t.Errorf("TaskErrorCodeRunLoopError = %q, want runloop_error", TaskErrorCodeRunLoopError)
	}
	if TaskErrorCodeCancelled != "cancelled" {
		t.Errorf("TaskErrorCodeCancelled = %q, want cancelled", TaskErrorCodeCancelled)
	}
}
