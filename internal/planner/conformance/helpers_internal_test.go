package conformance

import (
	"testing"
)

// TestMapDepth_AssortedShapes exercises the unexported map-depth helper
// across the shapes the pause-payload bounds scenario will encounter
// in V1. The depth cap (32) is the early-return guard for adversarial
// recursive structures; the scenario's primary use case is single-
// digit depth.
func TestMapDepth_AssortedShapes(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  int
	}{
		{"primitive_scalar", "hello", 0},
		{"nil_input", nil, 0},
		{"empty_map", map[string]any{}, 1},
		{"flat_map", map[string]any{"a": 1, "b": 2}, 1},
		{"two_level_map", map[string]any{"a": map[string]any{"b": 1}}, 2},
		{"three_level_map", map[string]any{
			"a": map[string]any{
				"b": map[string]any{"c": 1},
			},
		}, 3},
		{"flat_slice", []any{1, 2, 3}, 1},
		{"nested_slice", []any{
			map[string]any{"x": 1},
		}, 2},
		{"mixed", map[string]any{
			"list": []any{
				map[string]any{
					"inner": 42,
				},
			},
		}, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapDepth(tc.input)
			if got != tc.want {
				t.Errorf("mapDepth(%v) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestMapDepth_BoundedAtCap exercises the depth-cap branch: a tree
// deeper than 32 returns 32 (the documented bound).
func TestMapDepth_BoundedAtCap(t *testing.T) {
	// Build a 40-deep nested map; expect depth = 32 (cap).
	root := map[string]any{}
	curr := root
	for range 40 {
		next := map[string]any{}
		curr["k"] = next
		curr = next
	}
	got := mapDepth(root)
	if got != 32 {
		t.Errorf("mapDepth on 40-deep nested map = %d, want 32 (cap)", got)
	}
}

// TestCountKeys_AssortedShapes pins the key-count helper used by the
// pause-payload bounds scenario.
func TestCountKeys_AssortedShapes(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  int
	}{
		{"primitive_scalar", "hello", 0},
		{"empty_map", map[string]any{}, 0},
		{"flat_map_3_keys", map[string]any{"a": 1, "b": 2, "c": 3}, 3},
		{"nested_map", map[string]any{
			"outer": map[string]any{
				"inner1": 1,
				"inner2": 2,
			},
		}, 3}, // 1 (outer) + 2 (inner)
		{"slice_of_maps", []any{
			map[string]any{"a": 1},
			map[string]any{"b": 2, "c": 3},
		}, 3}, // 1 + 2 (no entry for the slice itself)
		{"slice_of_scalars", []any{1, 2, 3}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := countKeys(tc.input)
			if got != tc.want {
				t.Errorf("countKeys(%v) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestIsValidCapability_DocumentsBitmask asserts the canonical
// CapabilitySet constants combine to a non-zero, distinguishable
// bitmask shape.
func TestCapability_BitmaskShape(t *testing.T) {
	if CapabilitySetReAct == 0 {
		t.Error("CapabilitySetReAct is zero — bitmask construction failed")
	}
	if CapabilitySetDeterministic == 0 {
		t.Error("CapabilitySetDeterministic is zero — bitmask construction failed")
	}
	if CapabilitySetReAct == CapabilitySetDeterministic {
		t.Error("CapabilitySetReAct == CapabilitySetDeterministic — distinguishability broken (the harness gates each scenario per the bitmask)")
	}
	// ReAct declares LLMDriven; Deterministic does not.
	if CapabilitySetReAct&CapabilityLLMDriven == 0 {
		t.Error("CapabilitySetReAct missing CapabilityLLMDriven flag")
	}
	if CapabilitySetDeterministic&CapabilityLLMDriven != 0 {
		t.Error("CapabilitySetDeterministic includes CapabilityLLMDriven; Deterministic is NOT LLM-driven")
	}
	if CapabilitySetDeterministic&CapabilityCanPause == 0 {
		t.Error("CapabilitySetDeterministic missing CapabilityCanPause flag (PauseStep is a Phase 48 step type)")
	}
	if CapabilitySetReAct&CapabilityCanPause != 0 {
		t.Error("CapabilitySetReAct includes CapabilityCanPause; ReAct does NOT emit RequestPause in V1 (Phase 50)")
	}
}

// TestRunContext_Fallback exercises the nil-RunContextFactory
// fallback in the unexported runContext helper.
func TestRunContext_Fallback(t *testing.T) {
	h := Harness{}
	rc := h.runContext()
	// Zero RunContext is acceptable; the planner concrete's identity-
	// mandatory pre-check is the gate (per-concrete responsibility).
	if rc.Quadruple.TenantID != "" {
		t.Errorf("runContext() with nil factory returned populated identity %v", rc.Quadruple)
	}
}

// TestHasCapability_BitmaskMatch exercises the bitmask check helper.
func TestHasCapability_BitmaskMatch(t *testing.T) {
	h := Harness{Capabilities: CapabilityLLMDriven | CapabilityWakeRoundTrip}
	if !h.hasCapability(CapabilityLLMDriven) {
		t.Error("hasCapability(CapabilityLLMDriven) returned false; bitmask check broken")
	}
	if !h.hasCapability(CapabilityWakeRoundTrip) {
		t.Error("hasCapability(CapabilityWakeRoundTrip) returned false")
	}
	if h.hasCapability(CapabilityCanPause) {
		t.Error("hasCapability(CapabilityCanPause) returned true; flag was NOT set")
	}
}
