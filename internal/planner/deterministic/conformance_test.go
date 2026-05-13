package deterministic_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/conformance"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestDeterministic_Conformance runs the full Phase 49 conformance
// pack against Phase 48's DeterministicPlanner. The pack's
// per-scenario gating (capabilities + ScenarioFactory) lets the same
// Run entrypoint drive the deterministic-side scenarios (top-prompts
// via a CallTool step; parallel-atomicity via a CallParallel step;
// wake-mode-poll round-trip via the SpawnAndAwaitStep; pause via the
// PauseStep; steering-drain + budget-aware via the planner's
// per-call control-signal handling).
//
// The harness wires REAL drivers across the wake-mode-round-trip
// seam (§17.3 #1 + D-032 binding): inmem `events.EventBus`,
// inprocess `tasks.TaskRegistry`, inmem `state.StateStore`. The
// `PrebuiltPlannerFactory` builds the planner WITH the real
// registry bound via `deterministic.WithRegistry` (Deterministic's
// SpawnAndAwaitStep performs the spawn against the bound registry).
//
// Deterministic does NOT declare `CapabilityLLMDriven` (it has no
// LLM dependency by construction — Phase 48's import-graph guard
// rejects `internal/llm` imports). The `MalformedLLM_Salvage`
// scenario therefore skips with a reason — never silently.
func TestDeterministic_Conformance(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		return conformance.Harness{
			// Default factory for scenarios that do not need a
			// scenario-specific step set (Sanity, WakeMode_Declared,
			// Sealed_DecisionSum, BudgetAware, Steering, Concurrent).
			Factory: func() planner.Planner {
				p, err := deterministic.NewDeterministicPlanner(
					deterministic.WithSteps(
						&deterministic.FinishStep{
							Reason: planner.FinishGoal,
						},
					),
				)
				if err != nil {
					t.Fatalf("Factory constructor: %v", err)
				}
				return p
			},
			// ScenarioFactory shapes the step set per scenario.
			ScenarioFactory: func(s conformance.ScenarioName) planner.Planner {
				switch s {
				case conformance.ScenarioTopPrompts:
					// Top-prompt shape: a single CallTool step.
					p, err := deterministic.NewDeterministicPlanner(
						deterministic.WithSteps(
							&deterministic.CallToolStep{
								Tool: "alpha",
								ArgsBuilder: func(_ planner.RunContext) (json.RawMessage, error) {
									return json.RawMessage(`{"x":1}`), nil
								},
								Reasoning: "top-prompt scenario: single tool call",
							},
						),
					)
					if err != nil {
						t.Fatalf("TopPrompts ScenarioFactory: %v", err)
					}
					return p
				case conformance.ScenarioParallelAtomicity:
					// CallParallel emission via a custom step.
					p, err := deterministic.NewDeterministicPlanner(
						deterministic.WithSteps(
							&parallelEmitStep{},
						),
					)
					if err != nil {
						t.Fatalf("ParallelCall ScenarioFactory: %v", err)
					}
					return p
				case conformance.ScenarioPauseBounds:
					p, err := deterministic.NewDeterministicPlanner(
						deterministic.WithSteps(
							&deterministic.PauseStep{
								Reason: planner.PauseAwaitInput,
								PayloadBuilder: func(_ planner.RunContext) (map[string]any, error) {
									return map[string]any{
										"question": "please confirm",
										"context":  "conformance pause-bounds scenario",
									}, nil
								},
							},
						),
					)
					if err != nil {
						t.Fatalf("PauseBounds ScenarioFactory: %v", err)
					}
					return p
				default:
					// Other scenarios use the default factory's
					// FinishStep — adequate for Steering / Budget /
					// Concurrent.
					p, err := deterministic.NewDeterministicPlanner(
						deterministic.WithSteps(
							&deterministic.FinishStep{
								Reason: planner.FinishGoal,
							},
						),
					)
					if err != nil {
						t.Fatalf("ScenarioFactory default: %v", err)
					}
					return p
				}
			},
			// PrebuiltPlannerFactory drives the WakePoll round-trip:
			// the planner must be constructed AGAINST the real
			// registry from the harness (Deterministic's
			// SpawnAndAwaitStep binds the registry at construction).
			PrebuiltPlannerFactory: func(deps *conformance.WakeRoundTripDeps) planner.Planner {
				p, err := deterministic.NewDeterministicPlanner(
					deterministic.WithRegistry(deps.Registry),
					deterministic.WithSteps(
						&deterministic.SpawnAndAwaitStep{
							StepID: "conf-spawn-await",
							Kind:   tasks.KindBackground,
							SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
								return planner.SpawnSpec{
									Description: "conformance wake-poll round-trip",
									Query:       "wake-poll background work",
									Priority:    0,
									RetainTurn:  false,
								}, nil
							},
							OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
								return planner.Finish{
									Reason: planner.FinishGoal,
									Metadata: map[string]any{
										"via":      "conformance.WakePoll",
										"resolved": true,
									},
								}, nil
							},
						},
					),
				)
				if err != nil {
					t.Fatalf("PrebuiltPlannerFactory: %v", err)
				}
				return p
			},
			WakeMode:            planner.WakePoll,
			Capabilities:        conformance.CapabilitySetDeterministic,
			TaskRegistryFactory: conformance.DefaultTaskRegistryFactory,
			RunContextFactory: func() planner.RunContext {
				return planner.RunContext{
					Quadruple: identity.Quadruple{
						Identity: identity.Identity{
							TenantID:  "conf-tenant",
							UserID:    "conf-user",
							SessionID: "conf-session",
						},
						RunID: "conf-run",
					},
					Goal: "conformance test",
				}
			},
		}
	})
}

// parallelEmitStep is a one-off DecisionTreeStep that emits a
// CallParallel decision for the conformance pack's
// ParallelCall_Atomicity scenario. Two echo branches with a JoinAll
// spec; the conformance pack's shape assertion validates the
// branches' Tool names + the canonical Join kind.
type parallelEmitStep struct{}

func (s *parallelEmitStep) Decide(_ context.Context, _ planner.RunContext) (planner.Decision, bool, error) {
	return planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{"x":1}`)},
			{Tool: "beta", Args: json.RawMessage(`{"x":2}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	}, true, nil
}
