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

// TestCurrentGenerationFor_JSONIsByteFaithful proves invalid or
// non-canonical JSON cannot be silently confused with different bytes or
// cause replica divergence: the digest is a pure function of the raw
// schema bytes — identical bytes hash identically (even when the JSON is
// invalid), and DIFFERENT byte renderings of the same semantic schema
// hash differently (never coerced into a canonical form that would
// collide).
func TestCurrentGenerationFor_JSONIsByteFaithful(t *testing.T) {
	valid := genBaseSet()
	invalid := genBaseSet()
	invalid[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object",`) // truncated: invalid JSON

	// A replica carrying the SAME invalid bytes must hash identically —
	// no divergence, no coercion to something else.
	invalid2 := genBaseSet()
	invalid2[0].Tool.ArgsSchema = json.RawMessage(`{"type":"object",`)
	if ga, gb := currentGenerationFor(invalid), currentGenerationFor(invalid2); ga != gb {
		t.Fatalf("identical invalid JSON bytes diverged across replicas (%q != %q)", ga, gb)
	}
	if invalidGen := currentGenerationFor(invalid); invalidGen == "" {
		t.Fatal("invalid JSON must still produce a deterministic byte-faithful digest (never coerced, never unknown)")
	}

	// Two different byte renderings of the same semantic schema must NOT
	// collide — no canonicalization laundering.
	spaced := genBaseSet()
	spaced[0].Tool.ArgsSchema = json.RawMessage(`{"type": "object","properties": {"x": {"type": "string"}}}`)
	if ga, gb := currentGenerationFor(valid), currentGenerationFor(spaced); ga == gb {
		t.Fatalf("different JSON bytes must not collide (%q == %q)", ga, gb)
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
