// Package schema is Harbor's neutral Go-type -> JSON-Schema derivation
// home. It converts a reflect.Type into a JSON Schema document
// (structs, slices, maps, pointers, primitives, time.Time,
// json.RawMessage) and wraps the shared jsonschema/v6 compiler so
// callers can compile a derived (or hand-authored) schema document
// into a reusable validator.
//
// This package was promoted out of the in-process tool driver
// (internal/tools/drivers/inproc) so the derivation has exactly ONE
// implementation with TWO consumers: RegisterFunc (deriving a
// registered function's ArgsSchema/OutSchema) and the typed embed
// binding (internal/runtime/assemble.RunTyped, deriving a run's
// output schema from its generic type parameter). Promoting it here
// also closes a pre-existing §13 seam violation: the flow engine
// (internal/runtime/flow) needed the SAME derivation but could only
// reach it by importing a concrete tool driver package, which §4.4/§13
// forbid outside the driver's sanctioned importers. This package has
// no tool-catalog concept at all, so any caller can depend on it
// without gaining a forbidden concrete-driver import.
//
// Dependency direction (binding): this package MUST NOT import any
// concrete tool driver (internal/tools/drivers/...). The dependency
// points driver -> schema, never schema -> driver.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrUnsupportedType is returned when the derivation encounters a Go
// shape it cannot represent as JSON Schema (non-empty interfaces,
// channels, function-typed fields, cyclic structures, non-string map
// keys). The error message names the offending Go field so the caller
// can fix it.
var ErrUnsupportedType = errors.New("schema: unsupported type for derivation")

// ErrSchemaBuild is returned when the schema compiler chokes on a
// derived (or caller-supplied) JSON-Schema document. A derivation-time
// failure here indicates a deriver bug; callers should report it.
var ErrSchemaBuild = errors.New("schema: failed to build JSON schema")

// Map is a typed alias for the map[string]any shape JSON-Schema
// documents use. Exported for readability at call sites that build or
// inspect a derived document directly.
type Map = map[string]any

// maxDeriveDepth bounds reflection recursion so a pathological type
// graph fails loud instead of stack-overflowing.
const maxDeriveDepth = 32

// Derive converts a Go type into a JSON Schema object. Returns
// ErrUnsupportedType for shapes the deriver can't represent
// (interfaces, channels, function values, cyclic recursion).
//
// Coverage:
//   - bool -> {"type": "boolean"}
//   - int / int8...64 / uint / uint8...64 -> {"type": "integer"}
//   - float32 / float64 -> {"type": "number"}
//   - string -> {"type": "string"}
//   - []byte -> {"type": "string", "contentEncoding": "base64"}
//   - []T -> {"type": "array", "items": Schema(T)}
//   - map[string]T -> {"type": "object", "additionalProperties": Schema(T)}
//   - struct -> {"type": "object", "properties": {...}, "required": [...]}
//   - *T -> Schema(T) with the property dropped from "required" at the
//     parent level
//   - time.Time -> {"type": "string", "format": "date-time"}
//   - json.RawMessage -> {} (any-shaped; no constraint)
//
// Struct fields use the `json:"name"` tag for property names; an
// `,omitempty` modifier removes the field from `required`. A `-`
// json tag skips the field entirely.
func Derive(t reflect.Type) (Map, error) {
	return deriveWithDepth(t, 0, make(map[reflect.Type]bool))
}

func deriveWithDepth(t reflect.Type, depth int, visiting map[reflect.Type]bool) (Map, error) {
	if depth > maxDeriveDepth {
		return nil, fmt.Errorf("%w: derivation depth exceeded %d", ErrUnsupportedType, maxDeriveDepth)
	}
	if t == nil {
		return Map{}, nil
	}
	if visiting[t] {
		return nil, fmt.Errorf("%w: cyclic type %s", ErrUnsupportedType, t.String())
	}

	// Special types first.
	if t == reflect.TypeOf(time.Time{}) {
		return Map{"type": "string", "format": "date-time"}, nil
	}
	if t == reflect.TypeOf(json.RawMessage(nil)) {
		return Map{}, nil
	}

	switch t.Kind() {
	case reflect.Bool:
		return Map{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return Map{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return Map{"type": "number"}, nil
	case reflect.String:
		return Map{"type": "string"}, nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return Map{"type": "string", "contentEncoding": "base64"}, nil
		}
		visiting[t] = true
		itemSchema, err := deriveWithDepth(t.Elem(), depth+1, visiting)
		delete(visiting, t)
		if err != nil {
			return nil, err
		}
		return Map{"type": "array", "items": itemSchema}, nil
	case reflect.Array:
		visiting[t] = true
		itemSchema, err := deriveWithDepth(t.Elem(), depth+1, visiting)
		delete(visiting, t)
		if err != nil {
			return nil, err
		}
		return Map{"type": "array", "items": itemSchema, "minItems": float64(t.Len()), "maxItems": float64(t.Len())}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("%w: map key must be string (got %s)", ErrUnsupportedType, t.Key().Kind())
		}
		visiting[t] = true
		valSchema, err := deriveWithDepth(t.Elem(), depth+1, visiting)
		delete(visiting, t)
		if err != nil {
			return nil, err
		}
		return Map{"type": "object", "additionalProperties": valSchema}, nil
	case reflect.Pointer:
		return deriveWithDepth(t.Elem(), depth+1, visiting)
	case reflect.Struct:
		return deriveStruct(t, depth, visiting)
	case reflect.Interface:
		if t.NumMethod() == 0 {
			return Map{}, nil
		}
		return nil, fmt.Errorf("%w: non-empty interface %s", ErrUnsupportedType, t.String())
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return nil, fmt.Errorf("%w: kind %s not representable", ErrUnsupportedType, t.Kind())
	}
	return nil, fmt.Errorf("%w: unhandled kind %s", ErrUnsupportedType, t.Kind())
}

// deriveStruct walks a struct's exported fields, honouring json
// tags, and produces a JSON-Schema object.
func deriveStruct(t reflect.Type, depth int, visiting map[reflect.Type]bool) (Map, error) {
	visiting[t] = true
	defer delete(visiting, t)

	props := make(Map)
	required := make([]string, 0, t.NumField())

	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonName, omitempty := jsonFieldName(f)
		if jsonName == "-" {
			continue
		}
		fieldSchema, err := deriveWithDepth(f.Type, depth+1, visiting)
		if err != nil {
			return nil, fmt.Errorf("field %s.%s: %w", t.String(), f.Name, err)
		}
		props[jsonName] = fieldSchema
		if f.Type.Kind() != reflect.Pointer && !omitempty {
			required = append(required, jsonName)
		}
	}

	sort.Strings(required)
	out := Map{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	out["additionalProperties"] = false
	return out, nil
}

// jsonFieldName returns the field's JSON name + omitempty flag.
func jsonFieldName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, false
	}
	if tag == "-" {
		return "-", false
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		name = f.Name
	}
	omitempty := false
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty
}

// Compile compiles a JSON-Schema document into a reusable validator.
// Wraps the schema in a synthetic URL so the compiler resolves it
// stand-alone.
func Compile(schemaBytes []byte) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	const syntheticURL = "mem://harbor/tools-schema.json"
	if err := c.AddResource(syntheticURL, doc); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	return c.Compile(syntheticURL)
}

// Validate decodes instance into a JSON value and validates it
// against schema. The error is human-readable (it carries the failing
// instance path + the constraint that failed). A nil schema passes
// (no constraint configured). An empty instance is treated as JSON
// null so a schema requiring an object fails loud rather than passing
// vacuously.
func Validate(schema *jsonschema.Schema, instance json.RawMessage) error {
	if schema == nil {
		return nil
	}
	if len(instance) == 0 {
		instance = json.RawMessage("null")
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(instance))
	if err != nil {
		return fmt.Errorf("decode instance: %w", err)
	}
	return schema.Validate(v)
}
