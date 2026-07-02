// answer_envelope.go — the ONE terminal-validation + answer-envelope
// construction implementation the embed runner (assemble.RunOnce) and
// both per-task RunLoop drivers (cmd/harbor + harbortest/devstack) share.
//
// steering.RunLoop.Run deliberately does NOT validate terminal output —
// validation is the caller's edge (the runner validates after Run
// returns). Each run-loop driver would otherwise re-implement the
// capture-validate-marshal sequence; homing it here keeps exactly one
// validator, one payload-capture rule, and one envelope shape across all
// three call sites (byte-identical envelopes, golden-pinned).

package runctx

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/planner"
)

// FinishAnswerEnvelope builds the canonical [planner.AnswerEnvelope] from
// a terminal Finish, applying output-schema validation at the ONE shared
// site. The three call sites (assemble.RunOnce, the cmd/harbor per-task
// driver, the devstack twin) consume it so the answer envelope — and, on
// a schema-constrained goal Finish, the validated AnswerPayload — is
// byte-identical wherever a run is hosted.
//
// Behaviour:
//   - schema == nil, OR fin.Reason != FinishGoal → the plain three-key
//     envelope ({answer, finish_reason, tool_calls_seen}), byte-identical
//     to a no-schema run. A non-goal terminal (a steering CANCEL surfaced
//     as Finish{Cancelled}, a NoPath / deadline / constraints terminal)
//     was never asked to produce a schema-shaped answer, so it is NEVER
//     validated and NEVER returns ErrOutputInvalid.
//   - schema != nil AND fin.Reason == FinishGoal → the terminal payload
//     is captured to its exact bytes at the validation site (never a
//     lossy string round-trip) and validated against the compiled schema.
//     On success AnswerPayload carries the exact validated bytes and
//     Answer carries their string rendering. On a capture or validation
//     failure the returned error wraps [planner.ErrOutputInvalid] — a
//     loud, typed failure, never a silent fallback to unvalidated text
//     (CLAUDE.md §13). A caller mapping this to a task terminal stamps
//     [planner.TaskErrorCodeOutputInvalid].
//
// The trajectory is read only for [planner.CountToolInvocations]; a nil
// trajectory yields ToolCallsSeen == 0.
func FinishAnswerEnvelope(fin planner.Finish, traj *planner.Trajectory, schema *planner.OutputSchemaValidator) (planner.AnswerEnvelope, error) {
	answer := ExtractAssistantAnswer(fin)

	var answerPayload json.RawMessage
	if schema != nil && fin.Reason == planner.FinishGoal {
		raw, capErr := capturePayloadJSON(fin.Payload)
		if capErr != nil {
			return planner.AnswerEnvelope{}, fmt.Errorf("%w: %w", planner.ErrOutputInvalid, capErr)
		}
		if vErr := schema.Validate(raw); vErr != nil {
			return planner.AnswerEnvelope{}, fmt.Errorf("%w: %w", planner.ErrOutputInvalid, vErr)
		}
		answerPayload = raw
		// Answer carries the payload's string rendering for backward
		// compatibility (the string key is untouched on plain runs).
		answer = string(raw)
	}

	return planner.AnswerEnvelope{
		Answer:        answer,
		FinishReason:  string(fin.Reason),
		ToolCallsSeen: planner.CountToolInvocations(traj),
		AnswerPayload: answerPayload,
	}, nil
}

// capturePayloadJSON renders a terminal Finish payload to the raw JSON
// bytes the output-schema validator checks and the envelope's
// AnswerPayload carries. The ReAct native content path carries the
// model's JSON text as a string; other planners (deterministic flow
// output, external concretes) may carry a Go value or pre-marshalled
// json.RawMessage. Each shape is captured to its exact bytes ONCE here —
// never a string→map→string round-trip whose map key order is
// nondeterministic. A nil payload is a loud error (a schema-constrained
// run must produce a payload).
//
// A string payload is treated as raw JSON TEXT, never as a plain Go
// string to be re-quoted — this is the react terminal-answer reality
// (see [planner.Finish]'s Payload field doc): the model's `resp.Content`
// IS the answer's JSON encoding. A string that is not valid JSON (a
// planner concrete returning Payload: "done" instead of a structured
// value or a quoted JSON string `"\"done\""`) fails loud HERE with a
// hint, rather than surfacing a cryptic "decode terminal output as
// JSON" error two calls downstream inside the schema validator.
func capturePayloadJSON(payload any) (json.RawMessage, error) {
	switch p := payload.(type) {
	case nil:
		return nil, fmt.Errorf("terminal finish carried no payload for a schema-constrained run")
	case json.RawMessage:
		return p, nil
	case []byte:
		return json.RawMessage(p), nil
	case string:
		// The ReAct native terminal path: Content is the model's JSON
		// text. Use its bytes verbatim; validation catches non-JSON.
		if !json.Valid([]byte(p)) {
			return nil, fmt.Errorf(
				"terminal finish payload is a Go string that is not valid JSON text (%q): string payloads must contain JSON text; a plain Go string like \"done\" is not valid JSON — quote it (e.g. `\"done\"`) or return a structured payload",
				p,
			)
		}
		return json.RawMessage(p), nil
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("marshal terminal payload: %w", err)
		}
		return b, nil
	}
}
