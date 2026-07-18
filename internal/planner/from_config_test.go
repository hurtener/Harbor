package planner

import (
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestConfigFromOperator_Golden — the value-level B3 regression gate.
// Supersedes the Wave A devstack-side parity test
// (TestPlannerConfigFromConfig_FieldParityWithProduction): with ONE
// exported projection there is no second copy to compare against, so
// the gate moves to the owning package and pins every field's carry.
func TestConfigFromOperator_Golden(t *testing.T) {
	t.Parallel()
	parallel := false
	in := config.PlannerConfig{
		Driver:                 "react",
		MaxSteps:               7,
		ExtraGuidance:          "always cite sources",
		ReasoningReplay:        "text",
		MaxToolExamplesPerTool: 3,
		ParallelToolCalls:      &parallel,
		Extra:                  map[string]string{"k": "v"},
	}
	out := ConfigFromOperator(in)
	if out.Driver != "react" {
		t.Errorf("Driver = %q, want react", out.Driver)
	}
	if out.MaxSteps != 7 {
		t.Errorf("MaxSteps = %d, want 7", out.MaxSteps)
	}
	if out.ExtraGuidance != "always cite sources" {
		t.Errorf("ExtraGuidance = %q, want carried verbatim (B3 regression)", out.ExtraGuidance)
	}
	if out.ReasoningReplay != ReasoningReplayMode("text") {
		t.Errorf("ReasoningReplay = %q, want text (B3 regression)", out.ReasoningReplay)
	}
	if out.MaxToolExamplesPerTool != 3 {
		t.Errorf("MaxToolExamplesPerTool = %d, want 3 (B3 regression)", out.MaxToolExamplesPerTool)
	}
	if out.ParallelToolCalls == nil || *out.ParallelToolCalls != false {
		t.Errorf("ParallelToolCalls = %v, want explicit false carried through (B3 regression)", out.ParallelToolCalls)
	}
	if out.Extra["k"] != "v" {
		t.Errorf("Extra = %v, want map carried", out.Extra)
	}
}

// TestConfigFromOperator_Defaults pins the empty-driver default and
// the nil-pointer / nil-map conventions.
func TestConfigFromOperator_Defaults(t *testing.T) {
	t.Parallel()
	out := ConfigFromOperator(config.PlannerConfig{})
	if out.Driver != "react" {
		t.Errorf("empty Driver projected to %q, want react (the V1 reference default)", out.Driver)
	}
	// Nil ParallelToolCalls stays nil (the react factory distinguishes
	// "unset" from explicit false — D-169).
	if out.ParallelToolCalls != nil {
		t.Errorf("nil ParallelToolCalls projected to %v, want nil", out.ParallelToolCalls)
	}
	if out.Extra != nil {
		t.Errorf("empty Extra projected to %v, want nil", out.Extra)
	}
}

// TestConfigFromOperator_ExtraIsDefensiveCopy — mutating the projected
// Extra map never reaches the caller's config.
func TestConfigFromOperator_ExtraIsDefensiveCopy(t *testing.T) {
	t.Parallel()
	in := config.PlannerConfig{Extra: map[string]string{"k": "v"}}
	out := ConfigFromOperator(in)
	out.Extra["k"] = "mutated"
	if in.Extra["k"] != "v" {
		t.Error("mutating the projected Extra map reached the caller's config")
	}
}

// TestConfigFromOperator_FieldParity — the reflection gate the phase
// plan pins (Phase 110c acceptance): EVERY `config.PlannerConfig`
// field is either projected by ConfigFromOperator or named in the
// exclusion map with a reason locating its actual consumer. A new
// config field without either fails the build — the mechanical closure
// of the B3 drift class (devstack silently dropping four planner
// fields while its comment claimed field-for-field parity).
func TestConfigFromOperator_FieldParity(t *testing.T) {
	t.Parallel()
	projected := map[string]bool{
		"Driver":                 true,
		"MaxSteps":               true,
		"ExtraGuidance":          true,
		"ReasoningReplay":        true,
		"MaxToolExamplesPerTool": true,
		"ParallelToolCalls":      true,
		"Extra":                  true,
	}
	excluded := map[string]string{
		"SkillsContextMax":      "run-loop skill-retrieval cap — resolved via config.PlannerConfig.SkillsContextMaxResolved(), consumed by the per-task run loop, never by a planner driver",
		"AbsoluteMaxSpawnDepth": "tool-executor SpawnTask depth clamp — resolved via config.PlannerConfig.SpawnDepthCap(), consumed by the executor, never by a planner driver",
		"MaxBatchSpawns":        "tool-executor Batch breadth cap — resolved via config.PlannerConfig.BatchSpawnCap(), consumed by the executor's batch dispatch, never by a planner driver",
		"PlanningHints":         "per-run prompt steering — projected via planner.HintsFromConfig onto RunContext.PlanningHints, not onto the driver config",
		"TokenBudget":           "trajectory-compression threshold (Phase 111e, D-202) — a runtime-level run option projected onto RunSpec.Base.Budget.TokenBudget by the per-task run-loop drivers (brief 02 §planner-knobs: never planner state); the assembly builds the CompressionRunner from it, never a planner driver",
	}
	typ := reflect.TypeOf(config.PlannerConfig{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		_, isProjected := projected[f.Name]
		_, isExcluded := excluded[f.Name]
		switch {
		case isProjected && isExcluded:
			t.Errorf("PlannerConfig.%s listed as both projected and excluded — pick one", f.Name)
		case !isProjected && !isExcluded:
			t.Errorf("PlannerConfig.%s is neither projected by ConfigFromOperator nor excluded with a reason — the B3 silent-field-drop class; map it or exclude it explicitly", f.Name)
		}
	}
	for name := range projected {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("projected field PlannerConfig.%s no longer exists — update the parity sets", name)
		}
	}
	for name := range excluded {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("excluded field PlannerConfig.%s no longer exists — update the parity sets", name)
		}
	}
}

// TestHintsFromConfig_ZeroProjectsNil — an empty YAML block projects
// to nil so the `<planning_constraints>` prompt wrapper is omitted.
func TestHintsFromConfig_ZeroProjectsNil(t *testing.T) {
	t.Parallel()
	if got := HintsFromConfig(config.PlannerPlanningHintsCfg{}); got != nil {
		t.Errorf("HintsFromConfig(zero) = %+v, want nil", got)
	}
}

// TestHintsFromConfig_Populated pins the two YAML-exposed fields and
// the defensive slice copy.
func TestHintsFromConfig_Populated(t *testing.T) {
	t.Parallel()
	in := config.PlannerPlanningHintsCfg{
		Constraints:    "never call write tools without approval",
		PreferredTools: []string{"clock.now", "text.echo"},
	}
	got := HintsFromConfig(in)
	if got == nil {
		t.Fatal("HintsFromConfig returned nil for a populated block")
	}
	if got.Constraints != in.Constraints {
		t.Errorf("Constraints = %q, want carried verbatim", got.Constraints)
	}
	if len(got.PreferredTools) != 2 || got.PreferredTools[0] != "clock.now" {
		t.Errorf("PreferredTools = %v", got.PreferredTools)
	}
	got.PreferredTools[0] = "mutated"
	if in.PreferredTools[0] != "clock.now" {
		t.Error("mutating the projected PreferredTools slice reached the caller's config")
	}
	// Constraints-only is also non-zero.
	if HintsFromConfig(config.PlannerPlanningHintsCfg{Constraints: "x"}) == nil {
		t.Error("Constraints-only block projected to nil, want populated hints")
	}
}
