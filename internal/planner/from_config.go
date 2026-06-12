// from_config.go — the exported config→planner projections.
//
// Previously the `config.PlannerConfig` → `planner.PlannerConfig`
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
// Import direction: the subsystem imports `internal/config`
// additively; config stays a leaf. The projections are optional sugar
// — programmatic `PlannerConfig` / `PlanningHints` construction
// remains the headless golden path.

package planner

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/config"
)

// ConfigFromOperator maps the operator-facing `config.PlannerConfig`
// onto the registry-facing `planner.PlannerConfig` boundary.
// Empty Driver defaults to "react" — the V1 reference planner — so a
// config that omits the planner block boots unchanged from the
// earlier hardcoded path. The boundary copy matches the OAuth-provider
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
		// pass the *bool through verbatim so the
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
// custom planner Option but not via harbor.yaml; see the plan
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

// DispositionPolicyFromConfig decodes the YAML `multimodal.disposition`
// block into the planner-homed [DispositionPolicy].
// The config block is a thin carrier: the literal `*` key
// decodes onto Default; every other key (exact media type or `type/*`
// family wildcard) decodes onto ByMIME. Programmatic
// [DispositionPolicy] construction produces the same value with no
// config file — that is the seam this projection adapts onto.
//
// Values are re-validated via [ParseDisposition] defense-in-depth
// behind `config.Validate` (the loader's validateMultimodal mirrors
// the grammar locally); a non-grammar value fails loud with
// [ErrInvalidDisposition] rather than decoding a policy that silently
// drops entries (CLAUDE.md §13).
func DispositionPolicyFromConfig(cfg config.MultimodalConfig) (DispositionPolicy, error) {
	if len(cfg.Disposition) == 0 {
		return DispositionPolicy{}, nil
	}
	policy := DispositionPolicy{}
	for key, value := range cfg.Disposition {
		d, err := ParseDisposition(value)
		if err != nil {
			return DispositionPolicy{}, fmt.Errorf("multimodal.disposition[%q]: %w", key, err)
		}
		if key == "*" {
			policy.Default = d
			continue
		}
		if policy.ByMIME == nil {
			policy.ByMIME = make(map[string]AttachmentDisposition, len(cfg.Disposition))
		}
		policy.ByMIME[key] = d
	}
	return policy, nil
}
