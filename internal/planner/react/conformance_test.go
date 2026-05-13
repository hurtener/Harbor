package react_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/conformance"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// TestReact_Conformance runs the full Phase 49 conformance pack
// against Phase 45's ReActPlanner. The pack's per-scenario gating
// (capabilities + ScenarioFactory) lets the same Run entrypoint
// drive the LLM-side scenarios (top-prompts, malformed-LLM,
// parallel-atomicity, wake-mode-push round-trip), the steering-drain
// scenario, the budget-aware scenario, and the concurrent-reuse
// scenario.
//
// The harness wires REAL drivers across the wake-mode-round-trip
// seam (§17.3 #1 + D-032 binding): inmem `events.EventBus`,
// inprocess `tasks.TaskRegistry`, inmem `state.StateStore`. No
// mocks at the seam.
//
// ReAct does NOT declare `CapabilityCanPause` in V1 — Phase 45's
// emission path does not produce `RequestPause` (the unified pause
// primitive lands at Phase 50; the ReAct upgrade to emit
// `RequestPause` for HITL approval / OAuth lands with that wave).
// The `PausePayload_BoundsRespected` scenario therefore skips for
// ReAct with a reason — never silently.
func TestReact_Conformance(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		contentMap := conformance.DefaultReactContentMap()
		return conformance.Harness{
			// Default factory for scenarios that do not need a
			// scenario-specific LLM envelope (Sanity, WakeMode_Declared,
			// Sealed_DecisionSum, BudgetAware, Steering, Concurrent).
			Factory: func() planner.Planner {
				driver := mock.New(mock.Options{
					SyntheticContent: `{"tool":"_finish","args":{"answer":"conformance"}}`,
				})
				return react.New(driver)
			},
			// ScenarioFactory shapes the LLM envelope per scenario.
			// Each invocation returns a fresh ReAct + fresh mock so
			// per-subtest state can't bleed.
			ScenarioFactory: func(s conformance.ScenarioName) planner.Planner {
				switch s {
				case conformance.ScenarioWakeRoundTrip:
					// Push-mode round-trip: first response is the
					// SpawnTask emission; second is the Finish that
					// observes the resolved background result.
					driver := mock.New(mock.Options{
						SyntheticContent: contentMap[s],
					})
					p := react.New(driver)
					// We need a SECOND response after the wake. The
					// mock driver's SyntheticContent override is
					// constant per-invocation; wrap the driver to
					// surface a different response on the second
					// call.
					return reactWithScriptedResponses(p, []string{
						contentMap[s],
						conformance.SecondStepContent(),
					})
				default:
					content, ok := contentMap[s]
					if !ok || content == "" {
						content = `{"tool":"_finish","args":{"answer":"conformance"}}`
					}
					driver := mock.New(mock.Options{
						SyntheticContent: content,
					})
					return react.New(driver)
				}
			},
			WakeMode:            planner.WakePush,
			Capabilities:        conformance.CapabilitySetReAct,
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

// reactWithScriptedResponses returns a planner that uses a scripted
// LLM client with the supplied content sequence. Built on top of the
// existing `react.New` shape so the planner's full repair + emission
// pipeline runs; the only swap is the LLM driver.
//
// Each Complete call returns the next content; once the script is
// exhausted the last content repeats forever (so a runaway-loop bug
// surfaces as Finish{NoPath} rather than a panic).
func reactWithScriptedResponses(_ planner.Planner, contents []string) planner.Planner {
	client := newScriptedLLMForConformance(contents)
	return react.New(client)
}
