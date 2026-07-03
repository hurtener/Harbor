// Package deterministic is the public SDK facade over Harbor's
// internal/planner/deterministic package — the decision-tree planner
// concrete that drives the same Runtime as the LLM-driven ReAct
// planner through the identical Planner seam (RFC §3.6, §6.2).
// The concrete is constructor-configured (an ordered
// DecisionTreeStep slice), not registry-resolved. Alias-based
// re-exports only: no behavior lives here.
package deterministic

import (
	internal "github.com/hurtener/Harbor/internal/planner/deterministic"
)

// Planner + step vocabulary — aliases of the internal types.
type (
	// DeterministicPlanner is the decision-tree planner concrete.
	DeterministicPlanner = internal.DeterministicPlanner
	// Option customises NewDeterministicPlanner.
	Option = internal.Option
	// DecisionTreeStep is the operator-configurable step abstraction.
	DecisionTreeStep = internal.DecisionTreeStep
	// CallToolStep dispatches one tool when its guard matches.
	CallToolStep = internal.CallToolStep
	// FinishStep returns the terminal decision when its guard matches.
	FinishStep = internal.FinishStep
	// PauseStep parks the run on the unified pause primitive.
	PauseStep = internal.PauseStep
	// SpawnAndAwaitStep spawns a background group then polls its join.
	SpawnAndAwaitStep = internal.SpawnAndAwaitStep
	// WatchGroupStep polls an existing task group's resolution.
	WatchGroupStep = internal.WatchGroupStep
)

// DefaultName is the planner Name when WithName is not supplied.
const DefaultName = internal.DefaultName

// NewDeterministicPlanner constructs the planner from its step set
// (fail-loud at construction on an empty/nil step set).
var NewDeterministicPlanner = internal.NewDeterministicPlanner

// Constructor options (see internal/planner/deterministic Option docs).
var (
	// WithName overrides the planner's reported name.
	WithName = internal.WithName
	// WithRegistry supplies the TaskRegistry group-aware steps need.
	WithRegistry = internal.WithRegistry
	// WithSteps supplies the ordered decision-tree step set.
	WithSteps = internal.WithSteps
)
