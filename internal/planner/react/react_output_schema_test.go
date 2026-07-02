package react_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

const outputSchemaJSON = `{
	"type": "object",
	"required": ["sentiment"],
	"properties": {"sentiment": {"type": "string"}},
	"additionalProperties": false
}`

// captureReqClient records every request it sees and returns a fixed
// response, so a test can inspect how the planner shaped ResponseFormat
// + Validator.
type captureReqClient struct {
	lastReq atomic.Pointer[llm.CompleteRequest]
	resp    llm.CompleteResponse
}

func (c *captureReqClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	r := req
	c.lastReq.Store(&r)
	return c.resp, nil
}

func (c *captureReqClient) Close(_ context.Context) error { return nil }

func compileTestSchema(t *testing.T) *planner.OutputSchemaValidator {
	t.Helper()
	v, err := planner.CompileOutputSchema(json.RawMessage(outputSchemaJSON))
	if err != nil {
		t.Fatalf("CompileOutputSchema: %v", err)
	}
	return v
}

// TestReact_OutputSchema_SetsResponseFormatAndValidator — when the run
// carries an output schema, the terminal completion request carries
// ResponseFormat{FormatJSONSchema, schema} and a non-nil Validator.
func TestReact_OutputSchema_SetsResponseFormatAndValidator(t *testing.T) {
	t.Parallel()
	client := &captureReqClient{resp: llm.CompleteResponse{Content: `{"sentiment":"positive"}`}}
	p := react.New(client)

	q := fixedQuadruple(t, "run-schema-1")
	rc := rcWith(q, "classify the sentiment", nil)
	rc.OutputSchema = compileTestSchema(t)

	dec, err := p.Next(ctxWith(t, q), rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, ok := dec.(planner.Finish); !ok {
		t.Fatalf("expected terminal Finish, got %T", dec)
	}

	req := client.lastReq.Load()
	if req == nil {
		t.Fatal("no request captured")
	}
	if req.ResponseFormat == nil {
		t.Fatal("ResponseFormat is nil, want FormatJSONSchema on a schema-constrained run")
	}
	if req.ResponseFormat.Kind != llm.FormatJSONSchema {
		t.Errorf("ResponseFormat.Kind = %q, want %q", req.ResponseFormat.Kind, llm.FormatJSONSchema)
	}
	if len(req.ResponseFormat.JSONSchema) == 0 {
		t.Error("ResponseFormat.JSONSchema is empty, want the run's schema bytes")
	}
	if req.Validator == nil {
		t.Fatal("Validator is nil, want the retry-with-feedback validator engaged")
	}
}

// TestReact_OutputSchema_ValidatorIsToolCallAware — the installed
// Validator passes a tool-calling (non-terminal) turn and validates a
// content (terminal) turn against the schema.
func TestReact_OutputSchema_ValidatorIsToolCallAware(t *testing.T) {
	t.Parallel()
	client := &captureReqClient{resp: llm.CompleteResponse{Content: `{"sentiment":"positive"}`}}
	p := react.New(client)

	q := fixedQuadruple(t, "run-schema-2")
	rc := rcWith(q, "classify", nil)
	rc.OutputSchema = compileTestSchema(t)

	if _, err := p.Next(ctxWith(t, q), rc); err != nil {
		t.Fatalf("Next: %v", err)
	}
	v := client.lastReq.Load().Validator

	// A tool-calling turn: content is a preamble, not the answer. Pass.
	toolTurn := llm.CompleteResponse{
		Content:   "let me look that up",
		ToolCalls: []llm.ToolCallStructured{{ID: "c1", Name: "search", Args: json.RawMessage(`{}`)}},
	}
	if err := v(toolTurn); err != nil {
		t.Errorf("Validator on a tool-calling turn = %v, want nil (non-terminal)", err)
	}

	// A conforming terminal turn passes.
	if err := v(llm.CompleteResponse{Content: `{"sentiment":"negative"}`}); err != nil {
		t.Errorf("Validator on a conforming terminal turn = %v, want nil", err)
	}

	// A non-conforming terminal turn fails loud.
	if err := v(llm.CompleteResponse{Content: `{"mood":"blue"}`}); err == nil {
		t.Error("Validator on a non-conforming terminal turn = nil, want error")
	}
	// Plain non-JSON text fails loud.
	if err := v(llm.CompleteResponse{Content: `just some prose`}); err == nil {
		t.Error("Validator on non-JSON terminal content = nil, want error")
	}
}

// TestReact_OutputSchema_UnwrapsRespondWithEnvelope — S1 seam closure.
// On a schema-constrained run whose LLM response is shaped as the
// OutputModeTools `{"name":"respond_with","arguments":{...}}` envelope
// (what the downgrade layer's write half asks the model to emit — see
// internal/llm/output.ParseRespondWith's read-half godoc), the terminal
// Finish.Payload carries the UNWRAPPED `arguments` payload, not the
// envelope — this is exactly what RunOnce.capturePayloadJSON preserves
// verbatim (via its json.RawMessage case) into the envelope's
// AnswerPayload, so a Tools-mode profile's answer_payload holds the
// caller's schema-shaped value instead of the envelope wrapper.
func TestReact_OutputSchema_UnwrapsRespondWithEnvelope(t *testing.T) {
	t.Parallel()
	envelope := `{"name":"respond_with","arguments":{"sentiment":"positive"}}`
	client := &captureReqClient{resp: llm.CompleteResponse{Content: envelope}}
	p := react.New(client)

	q := fixedQuadruple(t, "run-schema-tools-unwrap")
	rc := rcWith(q, "classify the sentiment", nil)
	rc.OutputSchema = compileTestSchema(t)

	dec, err := p.Next(ctxWith(t, q), rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("expected terminal Finish, got %T", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("FinishReason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	raw, ok := fin.Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("Finish.Payload = %T (%v), want json.RawMessage (the unwrapped arguments)", fin.Payload, fin.Payload)
	}
	var got struct {
		Sentiment string `json:"sentiment"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unwrapped Payload not valid JSON: %v (%s)", err, raw)
	}
	if got.Sentiment != "positive" {
		t.Errorf("unwrapped Payload sentiment = %q, want positive", got.Sentiment)
	}
	// The envelope wrapper itself must NOT be present anywhere in the
	// captured payload — proof this is the unwrapped arguments, not the
	// `{"name":"respond_with",...}` shell.
	if strings.Contains(string(raw), "respond_with") {
		t.Errorf("Payload still carries the respond_with envelope: %s", raw)
	}

	// The installed Validator ALSO unwraps before validating: a
	// conforming envelope on the terminal turn passes.
	v := client.lastReq.Load().Validator
	if v == nil {
		t.Fatal("Validator is nil, want the schema Validator engaged")
	}
	if err := v(llm.CompleteResponse{Content: envelope}); err != nil {
		t.Errorf("Validator on a conforming respond_with envelope = %v, want nil (unwrap-before-validate)", err)
	}
}

// TestReact_NoOutputSchema_LeavesRequestUnconstrained — without a
// schema the request carries no ResponseFormat and no Validator (zero
// default-path change).
func TestReact_NoOutputSchema_LeavesRequestUnconstrained(t *testing.T) {
	t.Parallel()
	client := &captureReqClient{resp: llm.CompleteResponse{Content: "plain answer"}}
	p := react.New(client)

	q := fixedQuadruple(t, "run-plain")
	if _, err := p.Next(ctxWith(t, q), rcWith(q, "just answer", nil)); err != nil {
		t.Fatalf("Next: %v", err)
	}
	req := client.lastReq.Load()
	if req.ResponseFormat != nil {
		t.Errorf("ResponseFormat = %+v, want nil on a plain run", req.ResponseFormat)
	}
	if req.Validator != nil {
		t.Error("Validator is non-nil on a plain run, want nil")
	}
}
