package corrections

import (
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

// ---------------------------------------------------------------------------
// Quirk 2: Schema sanitization (OpenAI strict mode + permissive mode).
// ---------------------------------------------------------------------------

func TestSanitizer_OpenAIStrict_AddsRequiredFields(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	out, err := sanitizeSchema(in, llm.SchemaOpenAIStrict)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if v, ok := decoded["additionalProperties"]; !ok || v != false {
		t.Errorf("additionalProperties: got %v ok=%v, want false", v, ok)
	}
	if v, ok := decoded["strict"]; !ok || v != true {
		t.Errorf("strict: got %v ok=%v, want true", v, ok)
	}
}

func TestSanitizer_OpenAIStrict_NestedObjectsGetFieldsToo(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{
			"user":{"type":"object","properties":{"name":{"type":"string"}}}
		}
	}`)
	out, err := sanitizeSchema(in, llm.SchemaOpenAIStrict)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	// Outer.
	if decoded["additionalProperties"] != false {
		t.Errorf("outer additionalProperties: got %v want false", decoded["additionalProperties"])
	}
	if decoded["strict"] != true {
		t.Errorf("outer strict: got %v want true", decoded["strict"])
	}
	// Nested.
	props := decoded["properties"].(map[string]any)
	user := props["user"].(map[string]any)
	if user["additionalProperties"] != false {
		t.Errorf("nested user.additionalProperties: got %v want false", user["additionalProperties"])
	}
	if user["strict"] != true {
		t.Errorf("nested user.strict: got %v want true", user["strict"])
	}
}

func TestSanitizer_OpenAIStrict_PreservesExplicitAdditionalProperties(t *testing.T) {
	// Operator explicitly set additionalProperties:true — sanitizer
	// must NOT overwrite (otherwise the operator's intent is lost).
	in := json.RawMessage(`{"type":"object","additionalProperties":true,"properties":{}}`)
	out, err := sanitizeSchema(in, llm.SchemaOpenAIStrict)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if decoded["additionalProperties"] != true {
		t.Errorf("additionalProperties overwritten: got %v want true (operator value)", decoded["additionalProperties"])
	}
}

func TestSanitizer_Permissive_StripsStrictFields(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"strict":true,
		"additionalProperties":false,
		"properties":{
			"nested":{"type":"object","strict":true,"additionalProperties":false}
		}
	}`)
	out, err := sanitizeSchema(in, llm.SchemaPermissive)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if _, ok := decoded["strict"]; ok {
		t.Errorf("outer strict NOT stripped: %v", decoded["strict"])
	}
	if _, ok := decoded["additionalProperties"]; ok {
		t.Errorf("outer additionalProperties NOT stripped: %v", decoded["additionalProperties"])
	}
	nested := decoded["properties"].(map[string]any)["nested"].(map[string]any)
	if _, ok := nested["strict"]; ok {
		t.Errorf("nested strict NOT stripped: %v", nested["strict"])
	}
	if _, ok := nested["additionalProperties"]; ok {
		t.Errorf("nested additionalProperties NOT stripped: %v", nested["additionalProperties"])
	}
}

func TestSanitizer_Default_IsPassthrough(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{}}`)
	out, err := sanitizeSchema(in, llm.SchemaDefault)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	// SchemaDefault is the short-circuit path: the byte slice should
	// be returned verbatim (same backing array is acceptable; we
	// assert content equality).
	if string(out) != string(in) {
		t.Errorf("SchemaDefault changed bytes: in=%q out=%q", in, out)
	}
}

func TestSanitizer_EmptyInput_IsPassthrough(t *testing.T) {
	out, err := sanitizeSchema(nil, llm.SchemaOpenAIStrict)
	if err != nil {
		t.Fatalf("sanitizeSchema(nil): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty input should pass through, got %q", out)
	}
}

func TestSanitizer_InvalidJSON_FailsLoudly(t *testing.T) {
	_, err := sanitizeSchema(json.RawMessage(`{not json}`), llm.SchemaOpenAIStrict)
	if err == nil {
		t.Fatalf("expected error for invalid JSON, got nil")
	}
}

func TestSanitizer_OpenAIStrict_SkipsNonObjectSchemas(t *testing.T) {
	// A string-typed schema should NOT receive additionalProperties or
	// strict — those are object-level fields.
	in := json.RawMessage(`{"type":"string"}`)
	out, err := sanitizeSchema(in, llm.SchemaOpenAIStrict)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if _, ok := decoded["additionalProperties"]; ok {
		t.Errorf("additionalProperties added to non-object schema: %v", decoded)
	}
	if _, ok := decoded["strict"]; ok {
		t.Errorf("strict added to non-object schema: %v", decoded)
	}
}

func TestSanitizer_DescendsCompositionKeywords(t *testing.T) {
	// allOf / anyOf / oneOf entries are schemas and must be visited.
	in := json.RawMessage(`{
		"type":"object",
		"properties":{},
		"allOf":[{"type":"object","properties":{}}]
	}`)
	out, err := sanitizeSchema(in, llm.SchemaOpenAIStrict)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	allOf := decoded["allOf"].([]any)
	first := allOf[0].(map[string]any)
	if first["additionalProperties"] != false {
		t.Errorf("allOf[0].additionalProperties: got %v want false", first["additionalProperties"])
	}
	if first["strict"] != true {
		t.Errorf("allOf[0].strict: got %v want true", first["strict"])
	}
}
