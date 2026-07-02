package schema_test

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools/schema"
)

// goldenCorpus is the representative struct the promotion's golden
// test pins BYTE-IDENTICAL against the pre-promotion inproc deriver's
// documented behaviour (CLAUDE.md §17.8-adjacent: this corpus
// encodes the ACTUAL pre-move algorithm, field by field, not a
// reviewer's guess at it) — Phase 144 acceptance criterion 1.
type goldenCorpus struct {
	Name       string            `json:"name"`
	Age        int               `json:"age,omitempty"`
	Score      float64           `json:"score"`
	Active     bool              `json:"active"`
	Tags       []string          `json:"tags"`
	Blob       []byte            `json:"blob"`
	Meta       map[string]string `json:"meta"`
	Ptr        *int              `json:"ptr,omitempty"`
	PtrNoOmit  *string           `json:"ptr_no_omit"`
	When       time.Time         `json:"when"`
	Raw        json.RawMessage   `json:"raw"`
	Hidden     string            `json:"-"`
	unexported string            //nolint:unused // deliberately unexported; asserts the deriver skips it
}

// TestDerive_GoldenCorpus_ByteIdenticalPrePromotion pins the exact
// derived-schema shape for every representative Go kind the deriver
// supports. Any future change to the derivation algorithm that alters
// this shape must update this test deliberately — it cannot drift
// silently.
func TestDerive_GoldenCorpus_ByteIdenticalPrePromotion(t *testing.T) {
	got, err := schema.Derive(reflect.TypeOf(goldenCorpus{}))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	want := schema.Map{
		"type": "object",
		"properties": schema.Map{
			"name":        schema.Map{"type": "string"},
			"age":         schema.Map{"type": "integer"},
			"score":       schema.Map{"type": "number"},
			"active":      schema.Map{"type": "boolean"},
			"tags":        schema.Map{"type": "array", "items": schema.Map{"type": "string"}},
			"blob":        schema.Map{"type": "string", "contentEncoding": "base64"},
			"meta":        schema.Map{"type": "object", "additionalProperties": schema.Map{"type": "string"}},
			"ptr":         schema.Map{"type": "integer"},
			"ptr_no_omit": schema.Map{"type": "string"},
			"when":        schema.Map{"type": "string", "format": "date-time"},
			"raw":         schema.Map{},
		},
		"required":             []string{"active", "blob", "meta", "name", "raw", "score", "tags", "when"},
		"additionalProperties": false,
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Derive(goldenCorpus) mismatch:\n got  = %#v\n want = %#v", got, want)
	}

	// The corpus must also survive a real marshal/compile/validate
	// round-trip — a byte-identical derived shape that the compiler
	// rejects would be a silent regression.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal derived schema: %v", err)
	}
	compiled, err := schema.Compile(b)
	if err != nil {
		t.Fatalf("Compile(derived): %v", err)
	}
	ptrNoOmit := "present"
	instance := goldenCorpus{
		Name: "x", Score: 1, Active: true, Tags: []string{"a"},
		Blob: []byte("hi"), Meta: map[string]string{"k": "v"},
		PtrNoOmit: &ptrNoOmit,
		When:      time.Now(), Raw: json.RawMessage(`{"x":1}`),
	}
	instBytes, err := json.Marshal(instance)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	if err := schema.Validate(compiled, instBytes); err != nil {
		t.Errorf("Validate(conforming instance): %v", err)
	}
}

// TestDerive_FixedSizeArray covers the [N]T branch (minItems/maxItems).
func TestDerive_FixedSizeArray(t *testing.T) {
	got, err := schema.Derive(reflect.TypeOf([3]int{}))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	want := schema.Map{
		"type":     "array",
		"items":    schema.Map{"type": "integer"},
		"minItems": float64(3),
		"maxItems": float64(3),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Derive([3]int) = %#v, want %#v", got, want)
	}
}

// TestDerive_NestedStruct covers a struct field nested inside another
// struct (recursive deriveStruct, not just deriveWithDepth).
func TestDerive_NestedStruct(t *testing.T) {
	type inner struct {
		X int `json:"x"`
	}
	type outer struct {
		Inner inner `json:"inner"`
	}
	got, err := schema.Derive(reflect.TypeOf(outer{}))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	want := schema.Map{
		"type": "object",
		"properties": schema.Map{
			"inner": schema.Map{
				"type":                 "object",
				"properties":           schema.Map{"x": schema.Map{"type": "integer"}},
				"required":             []string{"x"},
				"additionalProperties": false,
			},
		},
		"required":             []string{"inner"},
		"additionalProperties": false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Derive(outer) = %#v, want %#v", got, want)
	}
}

// TestDerive_UnsupportedShapes tables every rejected Go shape: a
// non-empty interface, a channel field, a function field, a non-string
// map key, and a self-referential cyclic struct. Each must fail loud
// with errors.Is(err, schema.ErrUnsupportedType) BEFORE any caller
// attempts to use the (non-existent) derived schema.
func TestDerive_UnsupportedShapes(t *testing.T) {
	type withInterface struct {
		V io.Reader `json:"v"`
	}
	type withChan struct {
		Ch chan int `json:"ch"`
	}
	type withFunc struct {
		F func() `json:"f"`
	}
	type withBadMapKey struct {
		M map[int]string `json:"m"`
	}
	type cyclicNode struct {
		Next *cyclicNode `json:"next,omitempty"`
	}

	cases := []struct {
		name string
		typ  reflect.Type
	}{
		{"non-empty interface", reflect.TypeOf(withInterface{})},
		{"channel field", reflect.TypeOf(withChan{})},
		{"function field", reflect.TypeOf(withFunc{})},
		{"non-string map key", reflect.TypeOf(withBadMapKey{})},
		{"cyclic struct", reflect.TypeOf(cyclicNode{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := schema.Derive(tc.typ)
			if !errors.Is(err, schema.ErrUnsupportedType) {
				t.Fatalf("Derive(%s) = %v, want errors.Is schema.ErrUnsupportedType", tc.name, err)
			}
		})
	}
}

// TestDerive_NilType mirrors the "nil argument" edge (Derive(nil) is
// used by RunTyped when T is itself an interface type such as `any`)
// — an empty, unconstrained schema, not an error.
func TestDerive_NilType(t *testing.T) {
	got, err := schema.Derive(nil)
	if err != nil {
		t.Fatalf("Derive(nil): %v", err)
	}
	if !reflect.DeepEqual(got, schema.Map{}) {
		t.Fatalf("Derive(nil) = %#v, want empty Map", got)
	}
}

// TestCompile_And_Validate_RoundTrip exercises the shared
// compile/validate helpers directly (independent of any deriver call).
func TestCompile_And_Validate_RoundTrip(t *testing.T) {
	raw := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {"name": {"type": "string"}},
		"additionalProperties": false
	}`)
	compiled, err := schema.Compile(raw)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if err := schema.Validate(compiled, json.RawMessage(`{"name":"x"}`)); err != nil {
		t.Errorf("Validate(conforming): %v", err)
	}
	if err := schema.Validate(compiled, json.RawMessage(`{}`)); err == nil {
		t.Error("Validate(missing required field) = nil, want an error")
	}
	// A nil compiled schema is a no-op pass (no constraint configured).
	if err := schema.Validate(nil, json.RawMessage(`{"anything":true}`)); err != nil {
		t.Errorf("Validate(nil schema) = %v, want nil", err)
	}
	// An empty instance is treated as JSON null, which fails an
	// object-required schema loud rather than passing vacuously.
	if err := schema.Validate(compiled, nil); err == nil {
		t.Error("Validate(empty instance against object schema) = nil, want an error")
	}
}

// TestCompile_InvalidDocument_FailsLoud — an unparsable schema
// document is a loud error, never a silent nil validator.
func TestCompile_InvalidDocument_FailsLoud(t *testing.T) {
	if _, err := schema.Compile([]byte(`not json`)); err == nil {
		t.Error("Compile(invalid JSON) = nil error, want an error")
	}
}
