package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools"
)

// genBaseTool returns a tool populating EVERY stable semantic field
// family the generation digest covers, so a mutation test can flip
// exactly one family while every other stays fixed.
func genBaseTool() tools.Tool {
	return tools.Tool{
		Name:        "srv-a_tool",
		Description: "base description",
		ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		OutSchema:   json.RawMessage(`{"type":"object","properties":{"y":{"type":"number"}}}`),
		SideEffects: tools.SideEffectWrite,
		Tags:        []string{"tag-b", "tag-a"},
		AuthScopes:  []string{"scope-a", "scope-b"},
		CostHint:    "normal",
		LatencyHint: 250 * time.Millisecond,
		SafetyNotes: "writes production data",
		Loading:     tools.LoadingAlways,
		Examples: []tools.ToolExample{
			{Description: "ex one", Tags: []string{"ex-tag-b", "ex-tag-a"}, Args: map[string]any{"x": "v1", "n": float64(2)}},
		},
		Source:      "srv-a",
		Transport:   tools.TransportMCP,
		Policy:      tools.DefaultPolicy(),
		HandlesMIME: []string{"image/*", "text/plain"},
		Form:        tools.ToolFormTool,
		AppOnly:     true,
	}
}

// genBaseSet wraps the base tool as one descriptor set.
func genBaseSet() []tools.ToolDescriptor {
	return []tools.ToolDescriptor{{Tool: genBaseTool()}}
}

// TestCurrentGenerationFor_EverySemanticFieldFamilyMatters is the
// table-driven sensitivity gate: mutating ANY covered stable semantic
// field family — including schemas, side effects, tags / scopes,
// cost / latency / safety hints, loading, examples (description / tags /
// canonical JSON args), source, transport, every policy field, MIME
// handling, form, and the app-only / resource classification — changes
// the deterministic generation. A covered field that fails to move the
// generation is a lossy digest, exactly the P1 defect this replaces.
func TestCurrentGenerationFor_EverySemanticFieldFamilyMatters(t *testing.T) {
	baseGen := currentGenerationFor(genBaseSet())
	if baseGen == "" {
		t.Fatal("the base descriptor set produced an empty (unknown) generation")
	}

	cases := []struct {
		name   string
		mutate func(*tools.ToolDescriptor)
	}{
		{"name", func(d *tools.ToolDescriptor) { d.Tool.Name = "srv-a_tool-v2" }},
		{"description", func(d *tools.ToolDescriptor) { d.Tool.Description = "mutated description" }},
		{"args schema", func(d *tools.ToolDescriptor) {
			d.Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"z":{"type":"string"}}}`)
		}},
		{"out schema", func(d *tools.ToolDescriptor) {
			d.Tool.OutSchema = json.RawMessage(`{"type":"object","properties":{"w":{"type":"number"}}}`)
		}},
		{"side effects", func(d *tools.ToolDescriptor) { d.Tool.SideEffects = tools.SideEffectExternal }},
		{"tags", func(d *tools.ToolDescriptor) { d.Tool.Tags = []string{"tag-a", "tag-b", "tag-c"} }},
		{"auth scopes", func(d *tools.ToolDescriptor) { d.Tool.AuthScopes = []string{"scope-a", "scope-b", "scope-c"} }},
		{"cost hint", func(d *tools.ToolDescriptor) { d.Tool.CostHint = "expensive" }},
		{"latency hint", func(d *tools.ToolDescriptor) { d.Tool.LatencyHint = 500 * time.Millisecond }},
		{"safety notes", func(d *tools.ToolDescriptor) { d.Tool.SafetyNotes = "mutated safety notes" }},
		{"loading", func(d *tools.ToolDescriptor) { d.Tool.Loading = tools.LoadingDeferred }},
		{"example description", func(d *tools.ToolDescriptor) { d.Tool.Examples[0].Description = "ex two" }},
		{"example args", func(d *tools.ToolDescriptor) { d.Tool.Examples[0].Args = map[string]any{"x": "v2"} }},
		{"example tags", func(d *tools.ToolDescriptor) { d.Tool.Examples[0].Tags = []string{"ex-tag-a", "ex-tag-b", "ex-tag-c"} }},
		{"source", func(d *tools.ToolDescriptor) { d.Tool.Source = "srv-b" }},
		{"transport", func(d *tools.ToolDescriptor) { d.Tool.Transport = tools.TransportHTTP }},
		{"policy timeout", func(d *tools.ToolDescriptor) { d.Tool.Policy.TimeoutMS = 1 }},
		{"policy max retries", func(d *tools.ToolDescriptor) { d.Tool.Policy.MaxRetries = 9 }},
		{"policy backoff base", func(d *tools.ToolDescriptor) { d.Tool.Policy.BackoffBase = 5 * time.Millisecond }},
		{"policy backoff mult", func(d *tools.ToolDescriptor) { d.Tool.Policy.BackoffMult = 3 }},
		{"policy backoff max", func(d *tools.ToolDescriptor) { d.Tool.Policy.BackoffMax = 2 * time.Second }},
		{"policy retry-on", func(d *tools.ToolDescriptor) { d.Tool.Policy.RetryOn = []tools.ErrorClass{tools.ErrClassPermanent} }},
		{"policy validate", func(d *tools.ToolDescriptor) { d.Tool.Policy.Validate = tools.ValidateIn }},
		{"mime handling", func(d *tools.ToolDescriptor) { d.Tool.HandlesMIME = []string{"audio/*"} }},
		{"form (resource classification)", func(d *tools.ToolDescriptor) { d.Tool.Form = tools.ToolFormResource }},
		{"app-only classification", func(d *tools.ToolDescriptor) { d.Tool.AppOnly = false }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			descs := genBaseSet()
			tc.mutate(&descs[0])
			got := currentGenerationFor(descs)
			if got == "" {
				t.Fatal("mutated set produced an empty (unknown) generation")
			}
			if got == baseGen {
				t.Errorf("mutating %q did not change the generation: %q == %q", tc.name, got, baseGen)
			}
		})
	}
}

// TestCurrentGenerationFor_InvokeValidateAndOrderAreExcluded proves the
// digest is a pure function of the STABLE semantic fields: different
// Invoke / Validate function values (process code), different set-like
// element order, and different descriptor discovery order all leave the
// generation unchanged, while order-BEARING data (the Examples list)
// still moves it.
func TestCurrentGenerationFor_InvokeValidateAndOrderAreExcluded(t *testing.T) {
	inv1 := func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }
	inv2 := func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: 1}, nil
	}
	val1 := func(json.RawMessage) error { return nil }
	val2 := func(json.RawMessage) error { return errors.New("different validator") }

	a := tools.ToolDescriptor{Tool: genBaseTool(), Invoke: inv1, Validate: val1}
	b := tools.ToolDescriptor{Tool: genBaseTool(), Invoke: inv2, Validate: val2}
	if ga, gb := currentGenerationFor([]tools.ToolDescriptor{a}), currentGenerationFor([]tools.ToolDescriptor{b}); ga != gb {
		t.Fatalf("different Invoke/Validate function values changed the generation (%q != %q) — functions are excluded", ga, gb)
	}

	// Descriptor discovery order must never matter.
	if ga, gb := currentGenerationFor([]tools.ToolDescriptor{a, b}), currentGenerationFor([]tools.ToolDescriptor{b, a}); ga != gb {
		t.Fatalf("descriptor order changed the generation (%q != %q) — rows are sorted", ga, gb)
	}

	// Set-like element order must never matter (Tags here; the same
	// sorting covers AuthScopes / HandlesMIME / RetryOn / example Tags).
	reordered := func() tools.Tool {
		tt := genBaseTool()
		tt.Tags = []string{"tag-a", "tag-b"}
		return tt
	}
	orig := func() tools.Tool {
		tt := genBaseTool()
		tt.Tags = []string{"tag-b", "tag-a"}
		return tt
	}
	if ga, gb := currentGenerationFor([]tools.ToolDescriptor{{Tool: reordered()}}), currentGenerationFor([]tools.ToolDescriptor{{Tool: orig()}}); ga != gb {
		t.Fatalf("set-like element order changed the generation (%q != %q)", ga, gb)
	}

	// Example ARGS are a map — key order must never matter (encoding/json
	// sorts map keys), and nil vs empty must be equivalent ("no args").
	nilArgs := func() tools.Tool {
		tt := genBaseTool()
		tt.Examples[0].Args = nil
		return tt
	}
	emptyArgs := func() tools.Tool {
		tt := genBaseTool()
		tt.Examples[0].Args = map[string]any{}
		return tt
	}
	if ga, gb := currentGenerationFor([]tools.ToolDescriptor{{Tool: nilArgs()}}), currentGenerationFor([]tools.ToolDescriptor{{Tool: emptyArgs()}}); ga != gb {
		t.Fatalf("nil vs empty example args changed the generation (%q != %q)", ga, gb)
	}
}

// TestCurrentGenerationFor_ExamplesAreOrderBearing proves the Examples
// list is order-BEARING (the planner sees the examples in order), so
// reversing it changes the generation — the honest counterpart to the
// set-like sorting above.
func TestCurrentGenerationFor_ExamplesAreOrderBearing(t *testing.T) {
	fwd := func() tools.Tool {
		tt := genBaseTool()
		tt.Examples = []tools.ToolExample{
			{Description: "first", Args: map[string]any{"x": "1"}},
			{Description: "second", Args: map[string]any{"x": "2"}},
		}
		return tt
	}
	rev := func() tools.Tool {
		tt := genBaseTool()
		tt.Examples = []tools.ToolExample{
			{Description: "second", Args: map[string]any{"x": "2"}},
			{Description: "first", Args: map[string]any{"x": "1"}},
		}
		return tt
	}
	ga, gb := currentGenerationFor([]tools.ToolDescriptor{{Tool: fwd()}}), currentGenerationFor([]tools.ToolDescriptor{{Tool: rev()}})
	if ga == "" || gb == "" || ga == gb {
		t.Fatalf("reversing the order-bearing Examples list must change the generation (fwd=%q rev=%q)", ga, gb)
	}
}

// TestCurrentGenerationFor_JSONSchemaReplicaStability proves the P1
// fix: ArgsSchema / OutSchema are canonicalized by SEMANTIC JSON value
// before hashing, so two replicas whose discovery serialization differs
// only in whitespace or object-key order — a harmless divergence — hash
// identically. The byte-faithful expectation is gone: semantically equal
// schemas are replica-stable by construction.
func TestCurrentGenerationFor_JSONSchemaReplicaStability(t *testing.T) {
	baseGen := currentGenerationFor(genBaseSet())
	if baseGen == "" {
		t.Fatal("the base descriptor set produced an empty (unknown) generation")
	}
	// Every variant re-renders the SAME semantic ArgsSchema (an object
	// with one string property "x") with different whitespace / key order
	// / nesting layout, and must hash identically to the base.
	variants := []struct {
		name   string
		schema string
	}{
		{"whitespace", `{"type": "object", "properties": {"x": {"type": "string"}}}`},
		{"key order", `{"properties":{"x":{"type":"string"}},"type":"object"}`},
		{"nested whitespace and key order", `{"properties": { "x": { "type": "string" } }, "type": "object"}`},
		{"pretty multi-line", "{\n\t\"type\": \"object\",\n\t\"properties\": {\n\t\t\"x\": { \"type\": \"string\" }\n\t}\n}"},
	}
	for _, tc := range variants {
		descs := genBaseSet()
		descs[0].Tool.ArgsSchema = json.RawMessage(tc.schema)
		got := currentGenerationFor(descs)
		if got == "" {
			t.Fatalf("%s variant produced an empty (unknown) generation", tc.name)
		}
		if got != baseGen {
			t.Errorf("%s variant changed the generation (%q != %q) — semantically equal schemas must be replica-stable", tc.name, got, baseGen)
		}
	}
	// The same replica-stability holds through the REAL registry path
	// with a genuinely different discovery serialization.
	reg1 := NewRegistry()
	stageAppServer(t, reg1, "srv-a", genBaseSet())
	reg2 := NewRegistry()
	reordered := genBaseSet()
	reordered[0].Tool.ArgsSchema = json.RawMessage(`{"properties":{"x":{"type":"string"}},"type":"object"}`)
	reordered[0].Tool.OutSchema = json.RawMessage(`{"properties":{"y":{"type":"number"}},"type":"object"}`)
	stageAppServer(t, reg2, "srv-a", reordered)
	gen1, ok1 := reg1.CurrentGeneration("srv-a")
	gen2, ok2 := reg2.CurrentGeneration("srv-a")
	if !ok1 || !ok2 || gen1 == "" || gen1 != gen2 {
		t.Fatalf("whitespace/key-order-only discovery divergence must not break shared-key admission routing (gen1=%q ok1=%v gen2=%q ok2=%v)", gen1, ok1, gen2, ok2)
	}
}

// TestCurrentGenerationFor_JSONSchemaNumbersAreStable proves number
// literals canonicalize by VALUE: semantically equal forms (1, 1.0, 1e0,
// 1E+0, 0.1e1, -0 vs 0) hash identically, a meaningful number change
// still moves the generation, and a number with no exact canonical
// decimal form fails the whole generation closed.
func TestCurrentGenerationFor_JSONSchemaNumbersAreStable(t *testing.T) {
	forms := []string{
		`{"type":"object","properties":{"n":{"type":"number","maximum":1}}}`,
		`{"type":"object","properties":{"n":{"type":"number","maximum":1.0}}}`,
		`{"type":"object","properties":{"n":{"type":"number","maximum":1e0}}}`,
		`{"type":"object","properties":{"n":{"type":"number","maximum":1E+0}}}`,
		`{"type":"object","properties":{"n":{"type":"number","maximum":0.1e1}}}`,
	}
	var baseGen string
	for i, schema := range forms {
		descs := genBaseSet()
		descs[0].Tool.ArgsSchema = json.RawMessage(schema)
		got := currentGenerationFor(descs)
		if got == "" {
			t.Fatalf("form %q produced an empty (unknown) generation", schema)
		}
		if i == 0 {
			baseGen = got
		} else if got != baseGen {
			t.Errorf("semantically equal number form %q moved the generation (%q != %q)", schema, got, baseGen)
		}
	}
	// Negative zero is the value zero.
	negZero := genBaseSet()
	negZero[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","maximum":-0}}}`)
	zero := genBaseSet()
	zero[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","maximum":0}}}`)
	if ga, gb := currentGenerationFor(negZero), currentGenerationFor(zero); ga == "" || ga != gb {
		t.Fatalf("-0 and 0 are the same value and must converge (got %q vs %q)", ga, gb)
	}
	// A meaningful number change must still move the generation.
	changed := genBaseSet()
	changed[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","maximum":2}}}`)
	if got := currentGenerationFor(changed); got == "" || got == baseGen {
		t.Fatalf("changing the schema number must move the generation (got %q, base %q)", got, baseGen)
	}
	// A number with no exact canonical decimal form (an astronomically
	// large exponent) fails the whole generation closed — never hashed
	// as authoritative state.
	huge := genBaseSet()
	huge[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","maximum":1e99999999999999999999}}}`)
	if got := currentGenerationFor(huge); got != "" {
		t.Fatalf("a number with no exact canonical decimal form must fail closed, got %q", got)
	}
}

// TestCurrentGenerationFor_JSONSchemaArraysAreOrderBearing proves array
// order remains order-bearing in the canonical form: reversing an array
// (here a JSON-Schema `enum`) changes the generation, exactly as it did
// under byte-faithful hashing.
func TestCurrentGenerationFor_JSONSchemaArraysAreOrderBearing(t *testing.T) {
	fwd := genBaseSet()
	fwd[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","enum":["a","b"]}}}`)
	rev := genBaseSet()
	rev[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","enum":["b","a"]}}}`)
	ga, gb := currentGenerationFor(fwd), currentGenerationFor(rev)
	if ga == "" || gb == "" || ga == gb {
		t.Fatalf("array order is order-bearing in JSON Schema: reversing enum must move the generation (fwd=%q rev=%q)", ga, gb)
	}
}

// TestCurrentGenerationFor_NonCanonicalSchemasFailClosed proves the
// fail-closed posture for schemas: any document that is not a single
// canonical JSON value — invalid JSON, trailing data, duplicate object
// members at any nesting depth, or a non-finite / non-exact number —
// makes the ENTIRE generation unknown (""), so admission fails closed
// and raw invalid bytes are never hashed as authoritative state.
func TestCurrentGenerationFor_NonCanonicalSchemasFailClosed(t *testing.T) {
	bad := []string{
		`{"type":"object",`,                  // truncated
		`{"type":"object"`,                   // unterminated
		`{not json`,                          // garbage
		`{"type":"object"} {"type":"array"}`, // trailing document
		`{"type":"object"} trailing`,         // trailing garbage
		`{"a":1,"a":2}`,                      // duplicate top-level member
		`{"a":1,"b":2,"a":3}`,                // duplicate non-adjacent member
		`{"a":{"b":1,"b":2}}`,                // duplicate nested member
		`{"a":[{"b":1,"b":2}]}`,              // duplicate member inside an array element
		`NaN`,                                // non-finite literal (invalid JSON)
	}
	for _, raw := range bad {
		descs := genBaseSet()
		descs[0].Tool.ArgsSchema = json.RawMessage(raw)
		if got := currentGenerationFor(descs); got != "" {
			t.Errorf("ArgsSchema %q yielded %q, want empty (unknown, fail closed)", raw, got)
		}
		descs = genBaseSet()
		descs[0].Tool.OutSchema = json.RawMessage(raw)
		if got := currentGenerationFor(descs); got != "" {
			t.Errorf("OutSchema %q yielded %q, want empty (unknown, fail closed)", raw, got)
		}
	}
}

// TestCurrentGenerationFor_EmptySchemaIsNoSchemaMarker proves the empty
// ArgsSchema / OutSchema (the MCP driver's encoding of "no schema
// declared", produced for wire tools that omit inputSchema /
// outputSchema) keeps a deterministic, non-empty replica-stable
// generation instead of failing closed: it is a legitimate semantic
// state, not a JSON document. A declared schema must still hash
// differently from the marker.
func TestCurrentGenerationFor_EmptySchemaIsNoSchemaMarker(t *testing.T) {
	empty := genBaseSet()
	empty[0].Tool.ArgsSchema = nil
	empty[0].Tool.OutSchema = json.RawMessage(nil)
	emptyOther := genBaseSet()
	emptyOther[0].Tool.ArgsSchema = json.RawMessage{}
	emptyOther[0].Tool.OutSchema = json.RawMessage("")
	ga, gb := currentGenerationFor(empty), currentGenerationFor(emptyOther)
	if ga == "" || ga != gb {
		t.Fatalf("empty schemas must be a deterministic replica-stable marker (gen1=%q gen2=%q)", ga, gb)
	}
	if got := currentGenerationFor(genBaseSet()); got == "" || got == ga {
		t.Fatal("a declared schema must hash differently from the no-schema marker")
	}
}

// TestCurrentGenerationFor_ReplicaEquivalenceStable proves identical
// semantic catalogs across replicas hash identically through the REAL
// registry path: two independent registries staged with the same
// canonical set — including set-like fields in different element order —
// report the same CurrentGeneration, and a change to any covered field
// (here: the args schema) moves it.
func TestCurrentGenerationFor_ReplicaEquivalenceStable(t *testing.T) {
	mk := func() []tools.ToolDescriptor {
		tt := genBaseTool()
		tt.Tags = []string{"tag-b", "tag-a", "tag-c"}
		return []tools.ToolDescriptor{{Tool: tt}}
	}
	reg1 := NewRegistry()
	stageAppServer(t, reg1, "srv-a", mk())
	reg2 := NewRegistry()
	stageAppServer(t, reg2, "srv-a", mk())
	gen1, ok1 := reg1.CurrentGeneration("srv-a")
	gen2, ok2 := reg2.CurrentGeneration("srv-a")
	if !ok1 || !ok2 || gen1 == "" || gen1 != gen2 {
		t.Fatalf("identical semantic catalogs must hash identically across replicas (gen1=%q ok1=%v gen2=%q ok2=%v)", gen1, ok1, gen2, ok2)
	}

	changed := mk()
	changed[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"z":{"type":"boolean"}}}`)
	reg3 := NewRegistry()
	stageAppServer(t, reg3, "srv-a", changed)
	gen3, ok3 := reg3.CurrentGeneration("srv-a")
	if !ok3 || gen3 == gen1 {
		t.Fatalf("changing the args schema must move the generation (gen1=%q gen3=%q ok3=%v)", gen1, gen3, ok3)
	}
}

// TestCurrentGenerationFor_FailsClosedOnNonCanonicalAndEmpty proves the
// fail-closed posture: an empty/nil set yields "" (unknown — a render
// admission never binds an empty generation), and a set containing a
// descriptor whose stable fields cannot be canonically encoded (a
// non-serializable example args value) also yields "" — never a guessed
// digest.
func TestCurrentGenerationFor_FailsClosedOnNonCanonicalAndEmpty(t *testing.T) {
	if got := currentGenerationFor(nil); got != "" {
		t.Fatalf("nil set yielded %q, want empty (unknown)", got)
	}
	if got := currentGenerationFor([]tools.ToolDescriptor{}); got != "" {
		t.Fatalf("empty set yielded %q, want empty (unknown)", got)
	}
	bad := genBaseSet()
	bad[0].Tool.Examples[0].Args = map[string]any{"fn": func() {}}
	if got := currentGenerationFor(bad); got != "" {
		t.Fatalf("non-canonically encodable descriptor yielded %q, want empty (unknown, fail closed)", got)
	}
}
