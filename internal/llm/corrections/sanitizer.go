package corrections

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
)

// sanitizeSchema applies the `SchemaSanitizationMode` transform to the
// operator-supplied JSON-Schema bytes. The transform walks every
// nested object schema:
//
//   - `SchemaOpenAIStrict` SETS `additionalProperties:false` and
//     `strict:true` on every `{"type":"object"}` schema that lacks
//     them. OpenAI's structured-output mode requires both fields at
//     every nesting level.
//   - `SchemaPermissive` DELETES `additionalProperties` and `strict`
//     wherever they appear. Some providers reject those keys (e.g.
//     older OpenAI-compatible proxies that don't recognise the strict
//     mode).
//
// The transform produces a fresh byte slice; the input slice is
// never mutated.
//
// JSON Schema's `properties`, `items`, `additionalProperties`,
// `definitions`, `$defs`, `allOf`, `anyOf`, `oneOf`, and `not` are
// the schema-bearing keywords this walker descends into.
// Other keywords (`enum`, `description`, etc.) are passed through.
func sanitizeSchema(in json.RawMessage, mode llm.SchemaSanitizationMode) (json.RawMessage, error) {
	if mode == llm.SchemaDefault {
		return in, nil
	}
	if len(in) == 0 {
		return in, nil
	}
	var decoded any
	if err := json.Unmarshal(in, &decoded); err != nil {
		return nil, fmt.Errorf("decode schema: %w", err)
	}
	walkSchema(decoded, mode)
	out, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("encode schema: %w", err)
	}
	return out, nil
}

// walkSchema mutates `node` in-place per the sanitization mode. The
// decoded JSON tree is owned by the caller (we just unmarshaled it
// from a fresh byte slice), so in-place mutation is safe.
func walkSchema(node any, mode llm.SchemaSanitizationMode) {
	switch v := node.(type) {
	case map[string]any:
		walkSchemaObject(v, mode)
	case []any:
		for _, item := range v {
			walkSchema(item, mode)
		}
	}
}

// walkSchemaObject handles a single JSON-Schema object node. It
// applies the mode's mutation to the object's own attributes, then
// recurses into nested schema-bearing keywords.
func walkSchemaObject(obj map[string]any, mode llm.SchemaSanitizationMode) {
	// Apply the mode to THIS node.
	applySchemaMode(obj, mode)

	// Recurse into `properties`: map[string]<schema>.
	if props, ok := obj["properties"].(map[string]any); ok {
		for _, child := range props {
			walkSchema(child, mode)
		}
	}
	// Recurse into `additionalProperties` when it's a schema (not a
	// bool). The mode applies to nested schemas too.
	if ap, ok := obj["additionalProperties"]; ok {
		if _, isBool := ap.(bool); !isBool {
			walkSchema(ap, mode)
		}
	}
	// Recurse into `items` (array-element schema). Either a single
	// schema or a slice of schemas.
	if items, ok := obj["items"]; ok {
		walkSchema(items, mode)
	}
	// Recurse into `definitions` / `$defs`.
	for _, key := range []string{"definitions", "$defs"} {
		if defs, ok := obj[key].(map[string]any); ok {
			for _, child := range defs {
				walkSchema(child, mode)
			}
		}
	}
	// Recurse into composition keywords.
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := obj[key].([]any); ok {
			for _, item := range arr {
				walkSchema(item, mode)
			}
		}
	}
	if not, ok := obj["not"]; ok {
		walkSchema(not, mode)
	}
}

// applySchemaMode mutates `obj` per the requested mode. The mode is
// applied per node — only `{"type":"object"}` nodes receive the
// `strict`/`additionalProperties` mutation under `openai_strict`.
func applySchemaMode(obj map[string]any, mode llm.SchemaSanitizationMode) {
	switch mode {
	case llm.SchemaOpenAIStrict:
		// Only object-typed schemas get the `additionalProperties:false`+`strict:true`
		// treatment. Other types (arrays, primitives) keep their shape.
		if typeOf(obj) != "object" {
			return
		}
		if _, hasAP := obj["additionalProperties"]; !hasAP {
			obj["additionalProperties"] = false
		}
		if _, hasStrict := obj["strict"]; !hasStrict {
			obj["strict"] = true
		}
	case llm.SchemaPermissive:
		delete(obj, "additionalProperties")
		delete(obj, "strict")
	}
}

// typeOf returns the schema's `type` value as a string. Returns "" when
// the field is missing or non-string (JSON Schema also allows
// `type: ["string", "null"]` arrays, which we treat as a non-object
// shape so the strict-mode mutation skips them).
func typeOf(obj map[string]any) string {
	t, ok := obj["type"]
	if !ok {
		return ""
	}
	s, ok := t.(string)
	if !ok {
		return ""
	}
	return s
}
