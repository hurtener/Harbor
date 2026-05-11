// Package inproc is Harbor's in-process tool driver. Operators
// register Go functions as Tools via RegisterFunc; the driver
// derives ArgsSchema / OutSchema from the function's input / output
// types via reflection (RFC §6.4 "Tool authors write a function and
// register it", brief 03 §3) and wires the call through the
// ToolPolicy reliability shell so the registered function gets
// production-resilient timeout + retry + validation for free
// (D-024).
//
// Concurrent reuse (D-025): the driver itself is stateless — every
// RegisterFunc call builds a fresh ToolDescriptor and registers it
// in the catalog. The descriptor's Invoke closure captures the
// caller's `fn` (which the caller guarantees is safe for concurrent
// invocation); no mutable state lives in the driver.
package inproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/hurtener/Harbor/internal/tools"
)

// ErrUnsupportedType — RegisterFunc rejected the input/output type
// at registration time because the reflection-based schema deriver
// cannot represent it (interfaces, channels, function-typed fields,
// cyclic structures). The error message names the offending Go
// field so the operator can fix it. Wraps via fmt.Errorf("%w: ...")
// pattern.
var ErrUnsupportedType = errors.New("inproc: unsupported type for tool registration")

// ErrSchemaBuild — the schema compiler choked on the derived JSON
// Schema. Indicates a deriver bug; the operator should report it.
var ErrSchemaBuild = errors.New("inproc: failed to build JSON schema")

// RegisterFunc registers a Go function as a Tool. Input + output
// schemas are derived from the type parameters I and O via
// reflection.
//
// The function `fn` must be safe to invoke concurrently (D-025).
// The driver wraps it in a ToolPolicy shell — timeout + retry +
// validation — so a plain registration is production-resilient.
//
// `opts` configure the descriptor (policy, description, scopes,
// tags, examples). See DescriptorOption in the parent package for
// the full surface.
//
// Example:
//
//	type WeatherArgs struct {
//	    City string `json:"city"`
//	}
//	type WeatherOut struct {
//	    TempC float64 `json:"temp_c"`
//	    Summary string `json:"summary"`
//	}
//
//	err := inproc.RegisterFunc[WeatherArgs, WeatherOut](
//	    cat,
//	    "weather.lookup",
//	    func(ctx context.Context, in WeatherArgs) (WeatherOut, error) { ... },
//	    tools.WithDescription("Look up current weather by city name."),
//	    tools.WithAuthScopes("weather:read"),
//	    tools.WithSideEffect(tools.SideEffectExternal),
//	)
func RegisterFunc[I any, O any](
	cat tools.ToolCatalog,
	name string,
	fn func(ctx context.Context, in I) (O, error),
	opts ...tools.DescriptorOption,
) error {
	if cat == nil {
		return fmt.Errorf("inproc.RegisterFunc: catalog is nil")
	}
	if name == "" {
		return fmt.Errorf("inproc.RegisterFunc: name is empty")
	}
	if fn == nil {
		return fmt.Errorf("inproc.RegisterFunc: fn is nil for tool %q", name)
	}

	cfg := tools.ResolveOptions(opts...)

	// Derive schemas via reflection.
	var zeroIn I
	var zeroOut O
	inSchema, err := DeriveSchema(reflect.TypeOf(zeroIn))
	if err != nil {
		return fmt.Errorf("%w: input type for tool %q: %v", ErrUnsupportedType, name, err)
	}
	outSchema, err := DeriveSchema(reflect.TypeOf(zeroOut))
	if err != nil {
		return fmt.Errorf("%w: output type for tool %q: %v", ErrUnsupportedType, name, err)
	}

	inSchemaBytes, err := json.Marshal(inSchema)
	if err != nil {
		return fmt.Errorf("%w: marshal input schema: %v", ErrSchemaBuild, err)
	}
	outSchemaBytes, err := json.Marshal(outSchema)
	if err != nil {
		return fmt.Errorf("%w: marshal output schema: %v", ErrSchemaBuild, err)
	}

	// Compile the input validator once; cache it in the closure.
	compiledIn, err := compileSchema(inSchemaBytes)
	if err != nil {
		return fmt.Errorf("%w: compile input schema: %v", ErrSchemaBuild, err)
	}
	compiledOut, err := compileSchema(outSchemaBytes)
	if err != nil {
		return fmt.Errorf("%w: compile output schema: %v", ErrSchemaBuild, err)
	}

	tool := tools.Tool{
		Name:        name,
		Description: chooseString(cfg.Description, name),
		ArgsSchema:  inSchemaBytes,
		OutSchema:   outSchemaBytes,
		SideEffects: chooseSideEffect(cfg.SideEffect),
		Tags:        cfg.Tags,
		AuthScopes:  cfg.AuthScopes,
		CostHint:    cfg.CostHint,
		LatencyHint: cfg.LatencyHint,
		SafetyNotes: cfg.SafetyNotes,
		Loading:     chooseLoading(cfg.Loading),
		Examples:    cfg.Examples,
		Source:      cfg.Source,
		Transport:   tools.TransportInProcess,
		Policy:      cfg.Policy,
	}

	descriptor := tools.ToolDescriptor{
		Tool: tool,
		Validate: func(args json.RawMessage) error {
			return validateAgainst(compiledIn, args)
		},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return invokeReflective[I, O](ctx, args, fn, tool.Policy, func(args json.RawMessage) error {
				return validateAgainst(compiledIn, args)
			}, func(result tools.ToolResult) error {
				return validateAgainstResult(compiledOut, result)
			})
		},
	}
	return cat.Register(descriptor)
}

// invokeReflective is the inner-most invocation: decode args into I,
// call fn(ctx, in), marshal the result as a tools.ToolResult, wrap
// the whole thing in the policy shell so retries / timeouts /
// validation all fire uniformly.
func invokeReflective[I any, O any](
	ctx context.Context,
	args json.RawMessage,
	fn func(ctx context.Context, in I) (O, error),
	policy tools.ToolPolicy,
	validateIn func(args json.RawMessage) error,
	validateOut func(result tools.ToolResult) error,
) (tools.ToolResult, error) {
	return tools.RunWithPolicyHooked(
		ctx,
		args,
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			var in I
			if len(args) > 0 && !bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
				dec := json.NewDecoder(bytes.NewReader(args))
				dec.DisallowUnknownFields()
				if err := dec.Decode(&in); err != nil {
					return tools.ToolResult{}, fmt.Errorf("%w: decode args: %v", tools.ErrToolInvalidArgs, err)
				}
			}
			out, err := fn(ctx, in)
			if err != nil {
				return tools.ToolResult{}, err
			}
			return tools.ToolResult{Value: out}, nil
		},
		validateIn,
		validateOut,
		policy,
	)
}

// chooseString returns first when non-empty, else second.
func chooseString(first, second string) string {
	if first != "" {
		return first
	}
	return second
}

// chooseSideEffect normalises a zero-value SideEffect to the
// stateful default.
func chooseSideEffect(s tools.SideEffect) tools.SideEffect {
	if s == "" {
		return tools.SideEffectStateful
	}
	return s
}

// chooseLoading normalises a zero-value LoadingMode to Always.
func chooseLoading(m tools.LoadingMode) tools.LoadingMode {
	if m == "" {
		return tools.LoadingAlways
	}
	return m
}

// schemaMap is a typed alias for the map[string]any shape JSON
// Schema documents use. Used by the deriver for readability.
type schemaMap = map[string]any

// DeriveSchema converts a Go type into a JSON Schema object.
// Exported so the flow package can reuse it for its entry/exit
// types. Returns ErrUnsupportedType for shapes the deriver can't
// represent (interfaces, channels, function values, cyclic
// recursion).
//
// Coverage:
//   - bool → {"type": "boolean"}
//   - int / int8…64 / uint / uint8…64 → {"type": "integer"}
//   - float32 / float64 → {"type": "number"}
//   - string → {"type": "string"}
//   - []byte → {"type": "string", "contentEncoding": "base64"}
//   - []T → {"type": "array", "items": Schema(T)}
//   - map[string]T → {"type": "object", "additionalProperties": Schema(T)}
//   - struct → {"type": "object", "properties": {...}, "required": [...]}
//   - *T → Schema(T) with the property dropped from "required" at the
//     parent level
//   - time.Time → {"type": "string", "format": "date-time"}
//   - json.RawMessage → {} (any-shaped; no constraint)
//
// Struct fields use the `json:"name"` tag for property names; an
// `,omitempty` modifier removes the field from `required`. A `-`
// json tag skips the field entirely.
func DeriveSchema(t reflect.Type) (schemaMap, error) {
	return deriveWithDepth(t, 0, make(map[reflect.Type]bool))
}

// maxDeriveDepth bounds reflection recursion.
const maxDeriveDepth = 32

func deriveWithDepth(t reflect.Type, depth int, visiting map[reflect.Type]bool) (schemaMap, error) {
	if depth > maxDeriveDepth {
		return nil, fmt.Errorf("%w: derivation depth exceeded %d", ErrUnsupportedType, maxDeriveDepth)
	}
	if t == nil {
		return schemaMap{}, nil
	}
	if visiting[t] {
		return nil, fmt.Errorf("%w: cyclic type %s", ErrUnsupportedType, t.String())
	}

	// Special types first.
	if t == reflect.TypeOf(time.Time{}) {
		return schemaMap{"type": "string", "format": "date-time"}, nil
	}
	if t == reflect.TypeOf(json.RawMessage(nil)) {
		return schemaMap{}, nil
	}

	switch t.Kind() {
	case reflect.Bool:
		return schemaMap{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return schemaMap{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return schemaMap{"type": "number"}, nil
	case reflect.String:
		return schemaMap{"type": "string"}, nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return schemaMap{"type": "string", "contentEncoding": "base64"}, nil
		}
		visiting[t] = true
		itemSchema, err := deriveWithDepth(t.Elem(), depth+1, visiting)
		delete(visiting, t)
		if err != nil {
			return nil, err
		}
		return schemaMap{"type": "array", "items": itemSchema}, nil
	case reflect.Array:
		visiting[t] = true
		itemSchema, err := deriveWithDepth(t.Elem(), depth+1, visiting)
		delete(visiting, t)
		if err != nil {
			return nil, err
		}
		return schemaMap{"type": "array", "items": itemSchema, "minItems": float64(t.Len()), "maxItems": float64(t.Len())}, nil
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
		return schemaMap{"type": "object", "additionalProperties": valSchema}, nil
	case reflect.Pointer:
		return deriveWithDepth(t.Elem(), depth+1, visiting)
	case reflect.Struct:
		return deriveStruct(t, depth, visiting)
	case reflect.Interface:
		if t.NumMethod() == 0 {
			return schemaMap{}, nil
		}
		return nil, fmt.Errorf("%w: non-empty interface %s", ErrUnsupportedType, t.String())
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return nil, fmt.Errorf("%w: kind %s not representable", ErrUnsupportedType, t.Kind())
	}
	return nil, fmt.Errorf("%w: unhandled kind %s", ErrUnsupportedType, t.Kind())
}

// deriveStruct walks a struct's exported fields, honouring json
// tags, and produces a JSON-Schema object.
func deriveStruct(t reflect.Type, depth int, visiting map[reflect.Type]bool) (schemaMap, error) {
	visiting[t] = true
	defer delete(visiting, t)

	props := make(schemaMap)
	required := make([]string, 0, t.NumField())

	for i := 0; i < t.NumField(); i++ {
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
	out := schemaMap{
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

// compileSchema compiles a JSON-Schema document into a reusable
// validator. Wraps the schema in a synthetic URL so the compiler
// resolves it stand-alone.
func compileSchema(schemaBytes []byte) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	const syntheticURL = "mem://tool/schema.json"
	if err := c.AddResource(syntheticURL, doc); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	return c.Compile(syntheticURL)
}

// validateAgainst decodes args into a JSON value and validates it
// against schema. The error is human-readable (it carries the
// failing instance path + the constraint that failed).
func validateAgainst(schema *jsonschema.Schema, args json.RawMessage) error {
	if schema == nil {
		return nil
	}
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return fmt.Errorf("decode args: %w", err)
	}
	return schema.Validate(v)
}

// validateAgainstResult marshals result.Value into JSON and
// validates it against schema. Used for output validation.
func validateAgainstResult(schema *jsonschema.Schema, result tools.ToolResult) error {
	if schema == nil {
		return nil
	}
	if result.Value == nil {
		return validateAgainst(schema, json.RawMessage("null"))
	}
	buf, err := json.Marshal(result.Value)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	return validateAgainst(schema, buf)
}
