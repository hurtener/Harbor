// from_config.go — the exported config→planner projections (Phase 110c,
// D-196).
//
// Before 110c the `config.PlannerConfig` → `planner.PlannerConfig`
// projection existed twice — unexported `plannerConfigFromConfig` in
// `cmd/harbor/cmd_dev.go` and a hand-maintained `harbortest/devstack`
// mirror. The devstack copy shipped live bug B3 (SDK friction audit
// §1): it mapped only Driver/MaxSteps/Extra, silently dropping
// `ExtraGuidance` / `ReasoningReplay` / `MaxToolExamplesPerTool` /
// `ParallelToolCalls` — despite its own "MUST track production
// field-for-field" comment. ONE exported projection plus the
// reflection field-parity gate in from_config_test.go is the
// mechanical closure: a new `config.PlannerConfig` field without a
// projection (or an explicit exclusion naming why) fails the build.
//
// Import direction (D-193): the subsystem imports `internal/config`
// additively; config stays a leaf. The projections are optional sugar
// — programmatic `PlannerConfig` / `PlanningHints` construction
// remains the headless golden path.

package planner

import "github.com/hurtener/Harbor/internal/config"

// ConfigFromOperator maps the operator-facing `config.PlannerConfig`
// onto the registry-facing `planner.PlannerConfig` boundary (D-103).
// Empty Driver defaults to "react" — the V1 reference planner — so a
// config that omits the planner block boots unchanged from the
// pre-D-103 hardcoded path. The boundary copy matches the D-095
// OAuth-provider precedent (`internal/config` keeps its own struct
// shape; the subsystem owns the projection).
//
// Three `config.PlannerConfig` fields are deliberately NOT part of
// this projection (they are run-loop / executor knobs, not planner-
// driver boundary fields — the parity test names each exclusion):
//   - `SkillsContextMax` — resolved via
//     `config.PlannerConfig.SkillsContextMaxResolved()` and consumed by
//     the run loop's skill retrieval, not the planner driver.
//   - `AbsoluteMaxSpawnDepth` — resolved via
//     `config.PlannerConfig.SpawnDepthCap()` and consumed by the tool
//     executor's SpawnTask depth clamp.
//   - `PlanningHints` — projected via `HintsFromConfig` onto
//     `RunContext.PlanningHints` per run, not onto the driver config.
func ConfigFromOperator(cfg config.PlannerConfig) PlannerConfig {
	driver := cfg.Driver
	if driver == "" {
		driver = "react"
	}
	var extra map[string]string
	if len(cfg.Extra) > 0 {
		extra = make(map[string]string, len(cfg.Extra))
		for k, v := range cfg.Extra {
			extra[k] = v
		}
	}
	return PlannerConfig{
		Driver:                 driver,
		MaxSteps:               cfg.MaxSteps,
		ExtraGuidance:          cfg.ExtraGuidance,
		ReasoningReplay:        ReasoningReplayMode(cfg.ReasoningReplay),
		MaxToolExamplesPerTool: cfg.MaxToolExamplesPerTool,
		// Phase 107d (D-169): pass the *bool through verbatim so the
		// react factory distinguishes "unset" (nil → default true) from
		// an explicit false (107c serialization opt-out).
		ParallelToolCalls: cfg.ParallelToolCalls,
		Extra:             extra,
	}
}

// HintsFromConfig projects the YAML `config.PlannerPlanningHintsCfg`
// onto a `*PlanningHints`. Returns nil when the YAML block is empty —
// the per-task run loop then hands the planner a nil PlanningHints and
// the `<planning_constraints>` prompt wrapper is omitted entirely.
//
// V1.1 projects only the two YAML-exposed fields (Constraints +
// PreferredTools). The richer Go-struct fields on PlanningHints
// (ParallelGroups, DisallowTools, Budget) remain reachable through a
// custom planner Option but not via harbor.yaml; see Phase 83f's plan
// risks/open-questions section.
func HintsFromConfig(cfg config.PlannerPlanningHintsCfg) *PlanningHints {
	if cfg.IsZero() {
		return nil
	}
	return &PlanningHints{
		Constraints:    cfg.Constraints,
		PreferredTools: append([]string(nil), cfg.PreferredTools...),
	}
}
