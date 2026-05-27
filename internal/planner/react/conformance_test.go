package react_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
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
			//
			// Phase 107c (D-167) — under native tool-calling, the
			// content-string-only `mock.Options.SyntheticContent` does
			// NOT exercise the projector's ToolCalls path; scenarios
			// that need a reserved-name emission (`_spawn_task`,
			// `_await_task`) must drive a custom LLM that returns
			// native `resp.ToolCalls`. The wake-round-trip case wires
			// a scripted ToolCalls client directly.
			ScenarioFactory: func(s conformance.ScenarioName) planner.Planner {
				switch s {
				case conformance.ScenarioWakeRoundTrip:
					// Push-mode round-trip: first response is the
					// SpawnTask emission; second is the Finish that
					// observes the resolved background result.
					return react.New(newScriptedNativeToolCallsLLM([]llm.CompleteResponse{
						// Turn 1: native _spawn_task ToolCall.
						{ToolCalls: []llm.ToolCallStructured{{
							ID:   "call_spawn",
							Name: "_spawn_task",
							Args: json.RawMessage(`{"kind":"background","spec":{"description":"conformance bg task","query":"do the thing","priority":0,"retain_turn":false}}`),
						}}},
						// Turn 2: native _finish ToolCall (post-resolve).
						{ToolCalls: []llm.ToolCallStructured{{
							ID:   "call_finish",
							Name: "_finish",
							Args: json.RawMessage(`{"answer":"wake-round-trip resolved"}`),
						}}},
					}))
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

// scriptedNativeToolCallsLLM serves a fixed list of native-shaped
// `llm.CompleteResponse` values. Phase 107c (D-167) — the conformance
// pack's WakeMode_RoundTrip scenario needs the React planner to emit
// SpawnTask from the FIRST response and Finish from the second; the
// mock driver's `SyntheticContent` field carries only a Content
// string, so it cannot drive the projector's reserved-name ToolCalls
// path. This client wraps a slice of full responses and advances per
// Complete call. Once exhausted the last response repeats (so a
// runaway-loop bug surfaces as Finish{NoPath} via Phase 45's MaxSteps
// breaker rather than a panic).
type scriptedNativeToolCallsLLM struct {
	mu        sync.Mutex
	responses []llm.CompleteResponse
	cursor    int
}

func newScriptedNativeToolCallsLLM(responses []llm.CompleteResponse) *scriptedNativeToolCallsLLM {
	return &scriptedNativeToolCallsLLM{responses: responses}
}

func (s *scriptedNativeToolCallsLLM) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursor >= len(s.responses) {
		if len(s.responses) == 0 {
			return llm.CompleteResponse{}, nil
		}
		return s.responses[len(s.responses)-1], nil
	}
	out := s.responses[s.cursor]
	s.cursor++
	return out, nil
}

func (s *scriptedNativeToolCallsLLM) Close(_ context.Context) error {
	return nil
}
