package react

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/planner"
)

// DriverName is the canonical name the react planner registers under.
// The `internal/config` validator's `allowedPlannerDrivers` allowlist
// mirrors this constant (D-103). `cmd/harbor/main.go` blank-imports
// this package so the registration fires at process boot (§4.4 seam
// pattern; D-095 OAuth-provider precedent).
const DriverName = "react"

// init self-registers the `react` driver under its canonical name. The
// factory adapter below maps the planner-package `FactoryDeps` +
// `PlannerConfig` boundary onto `react.New`'s option-applied surface.
//
// D-103 — closes D-097's "future phases will read cfg.Planner" note
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
	return New(deps.LLM, opts...), nil
}
