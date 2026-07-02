package runctx_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
)

const answerSchema = `{
	"type": "object",
	"required": ["sentiment"],
	"properties": {"sentiment": {"type": "string"}},
	"additionalProperties": false
}`

func mustCompile(t *testing.T) *planner.OutputSchemaValidator {
	t.Helper()
	v, err := planner.CompileOutputSchema(json.RawMessage(answerSchema))
	if err != nil {
		t.Fatalf("CompileOutputSchema: %v", err)
	}
	return v
}

// TestFinishAnswerEnvelope_Table drives the shared builder across every
// shape RunOnce + the two per-task drivers depend on (D-276).
func TestFinishAnswerEnvelope_Table(t *testing.T) {
	traj := &planner.Trajectory{Query: "q"} // zero tool invocations

	t.Run("goal_schema_valid_sets_answer_payload", func(t *testing.T) {
		fin := planner.Finish{Reason: planner.FinishGoal, Payload: `{"sentiment":"positive"}`}
		env, err := runctx.FinishAnswerEnvelope(fin, traj, mustCompile(t))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(env.AnswerPayload) != `{"sentiment":"positive"}` {
			t.Fatalf("AnswerPayload = %s, want exact bytes", env.AnswerPayload)
		}
		if env.Answer != `{"sentiment":"positive"}` {
			t.Fatalf("Answer = %q, want payload string rendering", env.Answer)
		}
	})

	t.Run("goal_schema_invalid_fails_loud", func(t *testing.T) {
		fin := planner.Finish{Reason: planner.FinishGoal, Payload: `{"mood":"great"}`}
		_, err := runctx.FinishAnswerEnvelope(fin, traj, mustCompile(t))
		if !errors.Is(err, planner.ErrOutputInvalid) {
			t.Fatalf("err = %v, want ErrOutputInvalid", err)
		}
	})

	t.Run("goal_schema_non_json_fails_loud", func(t *testing.T) {
		fin := planner.Finish{Reason: planner.FinishGoal, Payload: "not json"}
		_, err := runctx.FinishAnswerEnvelope(fin, traj, mustCompile(t))
		if !errors.Is(err, planner.ErrOutputInvalid) {
			t.Fatalf("err = %v, want ErrOutputInvalid", err)
		}
	})

	t.Run("non_goal_with_schema_never_validated", func(t *testing.T) {
		// A cancelled finish carrying a schema returns the plain envelope
		// with the reason verbatim — NEVER ErrOutputInvalid.
		fin := planner.Finish{Reason: planner.FinishCancelled, Payload: "irrelevant"}
		env, err := runctx.FinishAnswerEnvelope(fin, traj, mustCompile(t))
		if err != nil {
			t.Fatalf("non-goal finish must not error: %v", err)
		}
		if env.FinishReason != string(planner.FinishCancelled) {
			t.Fatalf("FinishReason = %q, want cancelled", env.FinishReason)
		}
		if env.AnswerPayload != nil {
			t.Fatalf("non-goal finish must carry no AnswerPayload, got %s", env.AnswerPayload)
		}
	})
}

// TestFinishAnswerEnvelope_AbsentSchema_ByteIdenticalGolden pins the
// zero-default contract: with no schema, the marshaled envelope is the
// byte-identical three-key shape a plain run produces (D-276).
func TestFinishAnswerEnvelope_AbsentSchema_ByteIdenticalGolden(t *testing.T) {
	fin := planner.Finish{Reason: planner.FinishGoal, Payload: "the answer text"}
	env, err := runctx.FinishAnswerEnvelope(fin, &planner.Trajectory{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const golden = `{"answer":"the answer text","finish_reason":"goal","tool_calls_seen":0}`
	if string(raw) != golden {
		t.Fatalf("envelope bytes = %s\nwant           %s", raw, golden)
	}
}

// TestFinishAnswerEnvelope_WithPayload_Golden pins the schema-run
// marshaled shape: answer_payload appears as the fourth key, the answer
// string mirrors the payload (D-276).
func TestFinishAnswerEnvelope_WithPayload_Golden(t *testing.T) {
	fin := planner.Finish{Reason: planner.FinishGoal, Payload: `{"sentiment":"positive"}`}
	env, err := runctx.FinishAnswerEnvelope(fin, &planner.Trajectory{}, mustCompile(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const golden = `{"answer":"{\"sentiment\":\"positive\"}","finish_reason":"goal","tool_calls_seen":0,"answer_payload":{"sentiment":"positive"}}`
	if string(raw) != golden {
		t.Fatalf("envelope bytes = %s\nwant           %s", raw, golden)
	}
}
