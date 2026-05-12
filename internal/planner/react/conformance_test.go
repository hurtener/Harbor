package react_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/conformance"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// TestReact_Conformance runs the Phase 42 conformance harness against
// the Phase 45 ReActPlanner factory. The skeleton scenarios (sanity,
// wake-mode declared, sealed sum-type) pass; the deferred scenarios
// (top-20 prompts, malformed-LLM salvage, parallel-call atomicity,
// wake-mode round-trip, budget-aware finish, pause-payload bounds,
// steering drain) skip per Phase 49.
//
// This is the load-bearing conformance binding for Phase 45's
// WakePush declaration (D-032 + Phase 45 spec): the WakeMode_Declared
// subtest asserts ResolveWakeMode == WakePush.
//
// The RunContextFactory ships a minimal valid identity quadruple so
// the Sanity subtest does NOT fail on the planner's identity-mandatory
// pre-check (§6 rule 9 + D-001).
func TestReact_Conformance(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		// Each subtest gets a fresh mock LLM returning a clean
		// `_finish` envelope, so the planner's
		// Sanity_NextReturnsDecision terminates with Finish{Goal}
		// regardless of how the planner builds the prompt over a
		// minimal RunContext.
		driver := mock.New(mock.Options{
			SyntheticContent: `{"tool":"_finish","args":{"answer":"conformance"}}`,
		})
		return conformance.Harness{
			Factory: func() planner.Planner {
				return react.New(driver)
			},
			WakeMode: planner.WakePush,
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
