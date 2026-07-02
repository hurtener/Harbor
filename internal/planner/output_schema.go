package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrOutputInvalid is the typed sentinel a run-level structured-output
// run returns when the terminal answer fails validation against the
// caller-supplied output schema after the correction budget is spent.
// It wraps the underlying schema-validation error (and, when the
// generation-steering retry loop was engaged and exhausted, the
// [github.com/hurtener/Harbor/internal/llm.ErrRetryExhausted] chain).
// A schema-constrained run NEVER falls back to unvalidated text — the
// caller sees this error instead (CLAUDE.md §13). Callers compare via
// errors.Is.
var ErrOutputInvalid = errors.New("planner: terminal output failed output-schema validation")

// OutputSchemaValidator is the per-run compiled output schema a
// structured-output run carries on [RunContext.OutputSchema]. It bundles
// the raw JSON-Schema bytes (rendered into the terminal completion's
// [llm.ResponseFormat] so the provider constrains generation) with the
// compiled validator (used both by the generation-steering planner's
// per-turn [llm.CompleteRequest.Validator] and by the runtime-edge final
// validation, so the schema is compiled EXACTLY ONCE per run).
//
// Concurrent reuse: the value is immutable after
// [CompileOutputSchema] returns — the compiled schema is read-only, and
// [OutputSchemaValidator.Validate] is safe for concurrent invocation.
// It is per-run state carried on the RunContext, never a field on the
// shared planner artifact.
type OutputSchemaValidator struct {
	raw      json.RawMessage
	compiled *jsonschema.Schema
}

// CompileOutputSchema compiles a caller-supplied JSON-Schema document
// into a reusable per-run validator. A nil / empty / whitespace-only
// schema is a loud error (never a silent no-op, CLAUDE.md §13); an
// un-parseable or un-compilable schema is a loud error naming the fault.
// The compilation happens once per run at the runtime edge; the result
// threads onto [RunContext.OutputSchema].
func CompileOutputSchema(raw json.RawMessage) (*OutputSchemaValidator, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("planner: output schema is empty (a nil/empty schema is a config error, not a no-op)")
	}
	c := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("planner: unmarshal output schema: %w", err)
	}
	const syntheticURL = "mem://harbor/run-output-schema.json"
	if err := c.AddResource(syntheticURL, doc); err != nil {
		return nil, fmt.Errorf("planner: add output schema resource: %w", err)
	}
	compiled, err := c.Compile(syntheticURL)
	if err != nil {
		return nil, fmt.Errorf("planner: compile output schema: %w", err)
	}
	// Copy the raw bytes so a later mutation of the caller's slice cannot
	// change what the provider is asked to enforce (immutable after construction).
	rawCopy := make(json.RawMessage, len(raw))
	copy(rawCopy, raw)
	return &OutputSchemaValidator{raw: rawCopy, compiled: compiled}, nil
}

// Raw returns the schema bytes to render into the terminal completion's
// [llm.ResponseFormat.JSONSchema]. The returned slice is the validator's
// own copy; callers must not mutate it.
func (v *OutputSchemaValidator) Raw() json.RawMessage {
	if v == nil {
		return nil
	}
	return v.raw
}

// Validate checks a candidate terminal-answer payload against the
// compiled schema. An empty payload is treated as JSON null so a schema
// requiring an object fails loud rather than passing vacuously. A
// non-JSON payload fails loud (the model emitted text where structured
// output was demanded). A nil validator passes (no schema constrained
// this run).
func (v *OutputSchemaValidator) Validate(payload json.RawMessage) error {
	if v == nil || v.compiled == nil {
		return nil
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = json.RawMessage("null")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("decode terminal output as JSON: %w", err)
	}
	if err := v.compiled.Validate(inst); err != nil {
		return fmt.Errorf("output schema validation: %w", err)
	}
	return nil
}
