package deterministic_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/conformance"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
)

// TestDeterministic_Conformance runs the Phase 42 conformance harness
// against the Phase 48 DeterministicPlanner factory. The skeleton
// scenarios (Sanity, WakeMode_Declared, Sealed_DecisionSum) pass; the
// deferred scenarios (top-20 prompts, malformed-LLM salvage,
// parallel-call atomicity, wake-mode round-trip, budget-aware finish,
// pause-payload bounds, steering drain) skip per Phase 49.
//
// This is the load-bearing conformance binding for Phase 48's
// WakePoll declaration (D-032 + Phase 48 spec): the
// WakeMode_Declared subtest asserts ResolveWakeMode == WakePoll.
//
// The RunContextFactory ships a minimal valid identity quadruple so
// the Sanity subtest does NOT fail on the planner's identity-mandatory
// pre-check (§6 rule 9 + D-001).
func TestDeterministic_Conformance(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		return conformance.Harness{
			Factory: func() planner.Planner {
				p, err := deterministic.NewDeterministicPlanner(
					deterministic.WithSteps(
						&deterministic.FinishStep{
							Reason: planner.FinishGoal,
						},
					),
				)
				if err != nil {
					t.Fatalf("constructor: %v", err)
				}
				return p
			},
			WakeMode: planner.WakePoll,
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
