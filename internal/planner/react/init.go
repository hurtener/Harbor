package react

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/planner"
)

// DriverName is the canonical name the react planner registers under.
// The `internal/config` validator's `allowedPlannerDrivers` allowlist
// mirrors this constant. `cmd/harbor/main.go` blank-imports
// this package so the registration fires at process boot (§4.4 seam
// pattern; the OAuth-provider registry precedent).
const DriverName = "react"

// init self-registers the `react` driver under its canonical name. The
// factory adapter below maps the planner-package `FactoryDeps` +
// `PlannerConfig` boundary onto `react.New`'s option-applied surface.
//
// closes the "future phases will read cfg.Planner" note
// and CLAUDE.md §1.3's swappable-planner property gap.
func init() {
	planner.MustRegister(DriverName, factory)
}

// factory is the registered Factory adapter. The dev stack calls this
// indirectly via `planner.Resolve(ctx, cfg.Planner, FactoryDeps{LLM: llmClient})`.
//
// Fails closed on missing LLM (the V1 react planner cannot run without
// one; `react.New` panics on nil LLM but the factory returns the typed
// error so the dev stack's caller surfaces a clean wrapped boot error).
func factory(cfg planner.PlannerConfig, deps planner.FactoryDeps) (planner.Planner, error) {
	if deps.LLM == nil {
		return nil, fmt.Errorf("planner/react: LLM client is required (FactoryDeps.LLM was nil)")
	}
	opts := []Option{}
	if cfg.MaxSteps > 0 {
		opts = append(opts, WithMaxSteps(cfg.MaxSteps))
	}
	if cfg.ExtraGuidance != "" {
		opts = append(opts, WithSystemPromptExtra(cfg.ExtraGuidance))
	}
	// propagate the agent-configured reasoning-
	// replay mode. An empty mode resolves to `never` inside the
	// planner; WithReasoningReplay no-ops on an invalid value (config
	// validation already rejected those pre-boot).
	if cfg.ReasoningReplay != "" {
		opts = append(opts, WithReasoningReplay(cfg.ReasoningReplay))
	}
	// propagate the per-tool curated-example cap. A
	// value ≤ 0 resolves to defaultMaxToolExamples (3) inside the
	// prompt renderer; config validation already rejected negatives.
	if cfg.MaxToolExamplesPerTool > 0 {
		opts = append(opts, WithMaxToolExamplesPerTool(cfg.MaxToolExamplesPerTool))
	}
	// native parallel tool-call emission. The
	// planner's own default is `true`; only override when the operator
	// set the knob explicitly (nil = unset = keep the default).
	if cfg.ParallelToolCalls != nil {
		opts = append(opts, WithParallelToolCalls(*cfg.ParallelToolCalls))
	}
	return New(deps.LLM, opts...), nil
}
