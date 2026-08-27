package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools"
)

// digestHex matches a SHA-256 hex digest — the generation fingerprint's
// exact shape — so a regression test can prove a refusal message carries
// no generation digest no matter which value leaked.
var digestHex = regexp.MustCompile(`[0-9a-f]{64}`)

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
		AppVisible:  true,
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
// handling, form, and the App-only / App-visible / resource classification — changes
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
		{"app-visible classification", func(d *tools.ToolDescriptor) { d.Tool.AppVisible = false }},
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

// TestCurrentGenerationFor_RetryOnNilVsExplicitEmptyDistinct pins the
// behaviorally meaningful ToolPolicy.RetryOn distinction the digest must
// preserve: a nil RetryOn (zero value) INHERITS the default retry
// allowlist ([transient, timeout, 5xx]) at dispatch, while a non-nil
// EMPTY RetryOn means "retry on nothing" (one attempt only). Encoding
// only the sorted set members would collapse the two policies into one
// generation; the nil-vs-explicit presence marker keeps them distinct,
// while sorting a non-empty set's members stays order-independent.
func TestCurrentGenerationFor_RetryOnNilVsExplicitEmptyDistinct(t *testing.T) {
	nilRetry := genBaseSet()
	nilRetry[0].Tool.Policy.RetryOn = nil
	explicitEmpty := genBaseSet()
	explicitEmpty[0].Tool.Policy.RetryOn = []tools.ErrorClass{}
	explicitSet := genBaseSet()
	explicitSet[0].Tool.Policy.RetryOn = []tools.ErrorClass{tools.ErrClassTransient, tools.ErrClassTimeout}

	genNil := currentGenerationFor(nilRetry)
	genEmpty := currentGenerationFor(explicitEmpty)
	genSet := currentGenerationFor(explicitSet)
	if genNil == "" || genEmpty == "" || genSet == "" {
		t.Fatal("all three RetryOn shapes must produce a known generation")
	}
	if genNil == genEmpty {
		t.Fatalf("nil RetryOn (inherit defaults) and explicit empty RetryOn (retry nothing) collapsed into one generation %q", genNil)
	}
	if genEmpty == genSet {
		t.Fatalf("explicit empty RetryOn collapsed with an explicit non-empty RetryOn set (both %q)", genEmpty)
	}
	// Reversed member order of the same set must NOT move the generation
	// (the set members stay sorted); the nil/empty distinction above is
	// carried by the presence marker, not by member order.
	reversed := genBaseSet()
	reversed[0].Tool.Policy.RetryOn = []tools.ErrorClass{tools.ErrClassTimeout, tools.ErrClassTransient}
	if got := currentGenerationFor(reversed); got == "" || got != genSet {
		t.Fatalf("reordering RetryOn members moved the generation (got %q, want %q)", got, genSet)
	}
	// The same distinction survives the REAL registry path — the MCP
	// driver projects a non-nil empty RetryOn for max_attempts=1 and a
	// nil RetryOn when the server declares no retry override.
	reg1 := NewRegistry()
	stageAppServer(t, reg1, "srv-a", nilRetry)
	reg2 := NewRegistry()
	stageAppServer(t, reg2, "srv-a", explicitEmpty)
	g1, ok1 := reg1.CurrentGeneration("srv-a")
	g2, ok2 := reg2.CurrentGeneration("srv-a")
	if !ok1 || !ok2 || g1 == "" || g2 == "" {
		t.Fatalf("registry path must establish both generations (g1=%q ok1=%v g2=%q ok2=%v)", g1, ok1, g2, ok2)
	}
	if g1 == g2 {
		t.Fatalf("nil vs explicit-empty RetryOn collapsed through the registry (%q == %q)", g1, g2)
	}
}

// TestCurrentGenerationFor_InvalidUTF8SchemasFailClosed proves non-UTF-8
// schema bytes are rejected BEFORE any JSON token decoding: the JSON
// decoder replacement-normalizes invalid bytes to U+FFFD instead of
// failing, so without the UTF-8 gate two DISTINCT corrupt documents
// (e.g. a pattern carrying \xff vs \xfe) would collapse into ONE
// authoritative generation. Both must instead fail the whole generation
// closed — never converge, never hash as authoritative state.
func TestCurrentGenerationFor_InvalidUTF8SchemasFailClosed(t *testing.T) {
	distinct := []string{
		"\xff", // a bare invalid byte, not a JSON document at all
		`{"type":"object","properties":{"x":{"type":"string","pattern":"a\xffb"}}}`,
		`{"type":"object","properties":{"x":{"type":"string","pattern":"a\xfeb"}}}`, // a DIFFERENT invalid byte
	}
	for _, raw := range distinct {
		if _, err := canonicalJSONSchema(json.RawMessage(raw)); err == nil {
			t.Errorf("canonicalJSONSchema(%q) succeeded, want an error", raw)
		}
	}
	for _, raw := range distinct {
		descs := genBaseSet()
		descs[0].Tool.ArgsSchema = json.RawMessage(raw)
		if got := currentGenerationFor(descs); got != "" {
			t.Errorf("invalid-UTF-8 ArgsSchema %q yielded %q, want empty (unknown, fail closed)", raw, got)
		}
		descs = genBaseSet()
		descs[0].Tool.OutSchema = json.RawMessage(raw)
		if got := currentGenerationFor(descs); got != "" {
			t.Errorf("invalid-UTF-8 OutSchema %q yielded %q, want empty (unknown, fail closed)", raw, got)
		}
	}
	// The U+FFFD replacement character, when it appears as VALID UTF-8
	// inside a string, is still just a character — but the two INVALID
	// byte sequences that the decoder would normalize onto it fail
	// closed, so they can never converge onto one authoritative digest.
	replacement := genBaseSet()
	replacement[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","pattern":"a\ufffdb"}}}`)
	if got := currentGenerationFor(replacement); got == "" {
		t.Fatal("a valid-UTF-8 U+FFFD literal inside a string is a legitimate (if odd) character — the schema must still hash")
	}
}

// TestCurrentGenerationFor_AbsentOptionalSchemaIsStableExplicitAbsence
// pins the prior semantic-JSON fix for the REAL MCP shape: a tool that
// declares no output schema (OutSchema absent — outputSchema is OPTIONAL
// on the MCP wire) must hash as a stable explicit absence —
// deterministically, replica-stably, and distinct from a declared schema —
// exactly like an absent input schema, and never fail closed. Malformed
// non-empty schema bytes still fail the whole generation closed: absent
// is stable, malformed is unknown, never a collapse.
func TestCurrentGenerationFor_AbsentOptionalSchemaIsStableExplicitAbsence(t *testing.T) {
	absentOut := genBaseSet()
	absentOut[0].Tool.OutSchema = nil // absent optional output schema (real MCP: outputSchema is optional)
	absentOutAlt := genBaseSet()
	absentOutAlt[0].Tool.OutSchema = json.RawMessage("") // the MCP driver's encoding of "no schema declared"
	g1, g2 := currentGenerationFor(absentOut), currentGenerationFor(absentOutAlt)
	if g1 == "" || g1 != g2 {
		t.Fatalf("absent OutSchema must be a stable explicit-absence marker (gen1=%q gen2=%q)", g1, g2)
	}
	// Absent OutSchema + present ArgsSchema differs from BOTH schemas
	// absent, and from a declared OutSchema.
	bothAbsent := genBaseSet()
	bothAbsent[0].Tool.ArgsSchema = nil
	bothAbsent[0].Tool.OutSchema = nil
	declared := currentGenerationFor(genBaseSet())
	if declared == "" || declared == g1 {
		t.Fatal("a declared OutSchema must hash differently from the absent-schema marker")
	}
	if got := currentGenerationFor(bothAbsent); got == "" || got == g1 {
		t.Fatalf("both-absent must be its own stable marker, got %q", got)
	}
	// The mirror case: absent INPUT schema with a declared output schema.
	absentIn := genBaseSet()
	absentIn[0].Tool.ArgsSchema = nil
	if got := currentGenerationFor(absentIn); got == "" || got == g1 {
		t.Fatalf("absent ArgsSchema + present OutSchema must be a distinct stable marker, got %q", got)
	}
	// Malformed non-empty OutSchema still fails closed — absence is
	// stable, malformed is unknown.
	for _, raw := range []string{`{"type":"object",`, `{"a":1,"a":2}`} {
		descs := genBaseSet()
		descs[0].Tool.OutSchema = json.RawMessage(raw)
		if got := currentGenerationFor(descs); got != "" {
			t.Errorf("malformed OutSchema %q yielded %q, want empty (unknown, fail closed)", raw, got)
		}
	}
}

// TestRegistry_ResolveAppToolAtGeneration_ExactGenerationAtomic pins the
// atomic compare+resolve seam: the descriptor is returned ONLY when the
// server's current generation exactly equals the expected one, compared
// under the SAME registry read lock as the lookup. A refresh/replacement
// between an earlier generation read and this call — simulated by reading
// gen1, replacing the server (gen1 → gen2), then resolving with gen1 —
// fails typed (ErrGenerationMismatch) and never returns (never executes)
// the new row. Resolving with the NEW generation returns the NEW
// descriptor; a name miss within the exact generation stays a plain
// not-found.
func TestRegistry_ResolveAppToolAtGeneration_ExactGenerationAtomic(t *testing.T) {
	reg := NewRegistry()
	v1Calls := new(atomic.Int64)
	v2Calls := new(atomic.Int64)
	v1 := tools.ToolDescriptor{
		Tool: tools.Tool{Name: "srv-a_cb", Source: "srv-a", Transport: tools.TransportMCP, AppOnly: true},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			v1Calls.Add(1)
			return tools.ToolResult{Value: map[string]any{"ok": "v1"}}, nil
		},
	}
	v2 := tools.ToolDescriptor{
		Tool: tools.Tool{Name: "srv-a_cb-v2", Source: "srv-a", Transport: tools.TransportMCP, AppOnly: true},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			v2Calls.Add(1)
			return tools.ToolResult{Value: map[string]any{"ok": "v2"}}, nil
		},
	}
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{v1})
	gen1, ok := reg.CurrentGeneration("srv-a")
	if !ok || gen1 == "" {
		t.Fatalf("v1 registration did not establish a generation (gen1=%q ok=%v)", gen1, ok)
	}

	// Resolve under the exact generation succeeds.
	d, ok, err := reg.ResolveAppToolAtGeneration("srv-a", "srv-a_cb", gen1)
	if err != nil || !ok || d.Tool.Name != "srv-a_cb" {
		t.Fatalf("exact-generation resolve failed (d=%+v ok=%v err=%v)", d.Tool, ok, err)
	}

	// Replace with v2 (a different callback name → the generation moves).
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{v2})
	gen2, ok := reg.CurrentGeneration("srv-a")
	if !ok || gen2 == gen1 {
		t.Fatalf("replacement did not move the generation (gen1=%q gen2=%q ok=%v)", gen1, gen2, ok)
	}

	// A caller holding gen1 (the accessor after its generation read) that
	// reaches the atomic compare+resolve after the replacement fails typed
	// and returns no descriptor — the NEW row can never be executed.
	_, ok, err = reg.ResolveAppToolAtGeneration("srv-a", "srv-a_cb-v2", gen1)
	if err == nil || !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("stale-generation resolve err = %v, want a wrapped %v", err, ErrGenerationMismatch)
	}
	if ok {
		t.Fatal("stale-generation resolve must not return a descriptor")
	}
	if got := v2Calls.Load(); got != 0 {
		t.Errorf("v2 invocations = %d, want 0 (the new row must never execute under a stale generation)", got)
	}

	// The NEW generation resolves the NEW descriptor.
	d, ok, err = reg.ResolveAppToolAtGeneration("srv-a", "srv-a_cb-v2", gen2)
	if err != nil || !ok || d.Tool.Name != "srv-a_cb-v2" {
		t.Fatalf("current-generation resolve of the new row failed (d=%+v ok=%v err=%v)", d.Tool, ok, err)
	}
	if _, err := d.Invoke(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("invoke new row: %v", err)
	}
	if got := v2Calls.Load(); got != 1 {
		t.Errorf("v2 invocations = %d, want exactly 1 (the new row executes only when resolved under its own generation)", got)
	}

	// A name the exact generation does not hold stays a plain not-found
	// (no error, ok=false) — distinct from the generation-mismatch refusal.
	_, ok, err = reg.ResolveAppToolAtGeneration("srv-a", "srv-a_cb", gen2)
	if err != nil || ok {
		t.Fatalf("name-miss under the exact generation must be a plain not-found (ok=%v err=%v)", ok, err)
	}

	// An absent server refuses typed (no current generation to compare) —
	// a mismatch-family refusal, never a resolution.
	_, ok, err = reg.ResolveAppToolAtGeneration("absent-srv", "x", gen2)
	if err == nil || !errors.Is(err, ErrGenerationMismatch) || ok {
		t.Fatalf("absent-server resolve must refuse with ErrGenerationMismatch (ok=%v err=%v)", ok, err)
	}
}

// TestRegistry_ResolveAppToolAtGeneration_ConcurrentIsolation runs N
// concurrent atomic compare+resolve calls against ONE shared Registry
// under -race: the compare+resolve holds one read lock, so concurrent
// exact-generation readers never observe a torn generation/descriptor
// pairing — every success resolves a descriptor whose generation matched
// at the same instant, and every refusal is a typed mismatch.
func TestRegistry_ResolveAppToolAtGeneration_ConcurrentIsolation(t *testing.T) {
	reg := NewRegistry()
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{
		appDesc("srv-a_cb", "srv-a", true),
	})
	gen, ok := reg.CurrentGeneration("srv-a")
	if !ok || gen == "" {
		t.Fatalf("no generation established: %q", gen)
	}
	const n = 128
	var wg sync.WaitGroup
	start := make(chan struct{})
	refusals := new(atomic.Int64)
	hits := new(atomic.Int64)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := reg.ResolveAppToolAtGeneration("srv-a", "srv-a_cb", gen)
			switch {
			case err != nil:
				if !errors.Is(err, ErrGenerationMismatch) {
					t.Errorf("unexpected resolve error: %v", err)
				}
				refusals.Add(1)
			case ok:
				hits.Add(1)
			default:
				t.Errorf("plain not-found under a current generation")
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := hits.Load(); got != n {
		t.Errorf("successful exact-generation resolves = %d, want %d (an unchanged generation must never refuse)", got, n)
	}
	if got := refusals.Load(); got != 0 {
		t.Errorf("typed refusals = %d, want 0 under an unchanged generation", got)
	}
}

// TestRegistry_ResolveAppToolAtGeneration_MismatchErrorHidesDigests pins
// the P2 hardening: a generation-mismatch refusal is TYPED
// (ErrGenerationMismatch — the admission is stale, never a resolution of
// the new row) but its error text discloses neither the current nor the
// expected catalog-generation digest. The accessor wraps this error
// verbatim into the wire-facing CodeScopeMismatch message, so a digest in
// the text would leak catalog state to whoever probes the refusal.
func TestRegistry_ResolveAppToolAtGeneration_MismatchErrorHidesDigests(t *testing.T) {
	reg := NewRegistry()
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{
		appDesc("srv-a_cb", "srv-a", true),
	})
	gen1, ok := reg.CurrentGeneration("srv-a")
	if !ok || gen1 == "" {
		t.Fatalf("v1 registration did not establish a generation (gen1=%q ok=%v)", gen1, ok)
	}
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{
		appDesc("srv-a_cb-v2", "srv-a", true),
	})
	gen2, ok := reg.CurrentGeneration("srv-a")
	if !ok || gen2 == gen1 {
		t.Fatalf("replacement did not move the generation (gen1=%q gen2=%q ok=%v)", gen1, gen2, ok)
	}

	// A caller holding gen1 that reaches the atomic compare+resolve after
	// the replacement (gen1 → gen2) is refused TYPED — the typed mismatch
	// still propagates through errors.Is — while the text carries no
	// digest: neither generation value, and no 64-hex SHA-256 digest run
	// anywhere in the message.
	_, ok, err := reg.ResolveAppToolAtGeneration("srv-a", "srv-a_cb-v2", gen1)
	if err == nil || !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("stale-generation resolve err = %v, want a wrapped %v", err, ErrGenerationMismatch)
	}
	if ok {
		t.Fatal("stale-generation resolve must not return a descriptor")
	}
	if msg := err.Error(); strings.Contains(msg, gen1) || strings.Contains(msg, gen2) {
		t.Errorf("mismatch error text discloses a generation digest: %q", msg)
	} else if digestHex.MatchString(msg) {
		t.Errorf("mismatch error text carries a hex digest: %q", msg)
	}

	// The absent-server refusal (no current generation to compare) is the
	// same fail-closed family and likewise carries no digest.
	_, ok, err = reg.ResolveAppToolAtGeneration("absent-srv", "x", gen2)
	if err == nil || !errors.Is(err, ErrGenerationMismatch) || ok {
		t.Fatalf("absent-server resolve must refuse with ErrGenerationMismatch (ok=%v err=%v)", ok, err)
	}
	if msg := err.Error(); digestHex.MatchString(msg) {
		t.Errorf("absent-server refusal text carries a hex digest: %q", msg)
	}
}

// TestRegistry_RecordDiscovery_RefreshesAppOnlyPartitionWithGeneration
// pins the P2 latent-path fix: RecordDiscovery is the no-network
// counterpart to RefreshDiscovery, so it must rebuild BOTH projections
// from the SAME fresh descriptor set — the App dispatch catalog
// (entry.appVisible) AND the deterministic current generation — under one
// write lock. A subsequent generation-bound app-only resolution
// (ResolveAppToolAtGeneration) must see the refreshed partition: the new
// app-only callback resolves under the new generation, the stale callback
// is gone, and an ordinary descriptor never leaks into the app-only
// partition.
func TestRegistry_RecordDiscovery_RefreshesAppOnlyPartitionWithGeneration(t *testing.T) {
	reg := NewRegistry()
	stageAppServer(t, reg, "srv-a", []tools.ToolDescriptor{
		appDesc("srv-a_old", "srv-a", true),
	})
	genBefore, ok := reg.CurrentGeneration("srv-a")
	if !ok || genBefore == "" {
		t.Fatalf("staged server must have an established generation (gen=%q ok=%v)", genBefore, ok)
	}
	// The boot-time path re-records a FRESH descriptor slice after a new
	// discovery: the old app-only callback is replaced by a new one, plus
	// an ordinary tool that must stay OUT of the app-only partition.
	fresh := []tools.ToolDescriptor{
		appDesc("srv-a_plain", "srv-a", false),
		appDesc("srv-a_new", "srv-a", true),
	}
	if err := reg.RecordDiscovery("srv-a", fresh); err != nil {
		t.Fatalf("RecordDiscovery: %v", err)
	}
	genAfter, ok := reg.CurrentGeneration("srv-a")
	if !ok || genAfter == "" || genAfter == genBefore {
		t.Fatalf("RecordDiscovery must move the generation with the refreshed set (before=%q after=%q ok=%v)", genBefore, genAfter, ok)
	}
	// A generation-bound app-only resolution against the refreshed
	// generation resolves the NEW callback — the partition the accessor
	// reads was rebuilt from the SAME set that moved the generation.
	d, ok, err := reg.ResolveAppToolAtGeneration("srv-a", "srv-a_new", genAfter)
	if err != nil || !ok || d.Tool.Name != "srv-a_new" {
		t.Fatalf("generation-bound resolve of the refreshed callback failed (d=%+v ok=%v err=%v)", d.Tool, ok, err)
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_old"); ok {
		t.Fatalf("stale app-only callback %q survived RecordDiscovery — the partition must rebuild from the fresh set", "srv-a_old")
	}
	if _, ok := reg.ResolveAppTool("srv-a", "srv-a_plain"); ok {
		t.Fatalf("ordinary descriptor %q leaked into the app-only partition", "srv-a_plain")
	}
}
