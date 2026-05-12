// Package conformance ships the planner conformance harness skeleton.
//
// Phase 42 lands the harness shape — the `Harness` struct, the public
// `Run(t, factory)` entry point, and the §13 import-graph lint test
// (importgraph_test.go) — but the conformance scenarios themselves
// (top-20 prompts, malformed-LLM-output salvage, parallel-pause
// atomicity, wake-mode round-trip, etc.) skip with a `Phase 49:
// conformance scenarios` reason. Phase 49 fills them in.
//
// Why the skeleton ships at Phase 42:
//
//   - The §13 import-graph lint test is the most load-bearing
//     assertion the planner package can carry. Adding it at t=0
//     gates every concrete added thereafter against the planner-
//     runtime decoupling. The conformance package is the natural
//     home (it's the package that asserts every concrete's shape).
//   - Phase 45 (ReAct) and Phase 48 (Deterministic) consume `Run` —
//     each concrete's PR ships `<concrete>_conformance_test.go` that
//     calls `conformance.Run(t, factory)` and gets the lint + every
//     conformance scenario for free.
//   - The Harness shape locks the consumption contract — Phase 49
//     fills scenarios without redefining the harness shape.
//
// Consumption pattern (Phase 45+):
//
//	func TestReact_Conformance(t *testing.T) {
//	    conformance.Run(t, func() conformance.Harness {
//	        return conformance.Harness{
//	            Factory:  func() planner.Planner { return react.New(...) },
//	            WakeMode: planner.WakePush,
//	        }
//	    })
//	}
//
// The harness factory pattern matches the events / tools / tasks
// conformance suites: each subtest gets a fresh planner instance so
// internal state can't bleed between scenarios.
package conformance

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// Harness is the per-subtest fixture the harness Run loop consumes.
// Each conformance subtest invokes `factory()` once to obtain a fresh
// planner instance.
type Harness struct {
	// Factory constructs a fresh planner instance per subtest. The
	// factory MUST return a planner that is safe for the concurrent-
	// reuse contract (D-025) — the WakeMode-round-trip subtest runs
	// N concurrent Next calls against a single shared instance.
	Factory func() planner.Planner

	// WakeMode is the wake mode the concrete declares (D-032). The
	// conformance pack asserts the round-trip (SpawnTask → group
	// resolves → planner re-enters → reads MemberOutcome) against
	// this declaration. Phase 49 fills the round-trip scenario.
	WakeMode planner.WakeMode

	// Cleanup is called at subtest end. Optional — typical for
	// planner concretes that hold lifecycle resources (LLM client
	// sessions, etc.). Phase 42's stub planner needs no cleanup.
	Cleanup func()
}

// Run executes the conformance pack against the planner produced by
// `factoryFunc`. Phase 42 ships the skeleton: each conformance
// scenario is a `t.Run(name, func(t *testing.T) { t.Skip(...) })`
// pending Phase 49 implementation.
//
// The skeleton's structure matches the events / tools / tasks
// conformance suites — Phase 49 fills bodies without changing the
// subtest names.
func Run(t *testing.T, factoryFunc func() Harness) {
	t.Helper()

	t.Run("Sanity_NextReturnsDecision", func(t *testing.T) {
		h := factoryFunc()
		if h.Cleanup != nil {
			t.Cleanup(h.Cleanup)
		}
		p := h.Factory()
		if p == nil {
			t.Fatal("Factory returned nil planner")
		}
		dec, err := p.Next(context.Background(), planner.RunContext{})
		if err != nil {
			t.Fatalf("Next returned error: %v", err)
		}
		if dec == nil {
			t.Fatal("Next returned nil Decision")
		}
		// Sanity: the returned decision is one of the six
		// sealed-sum-type shapes. The unexported isDecision() marker
		// enforces this at compile time at every call site that
		// references planner.Decision — runtime check is defensive.
		switch dec.(type) {
		case planner.CallTool, planner.CallParallel, planner.SpawnTask,
			planner.AwaitTask, planner.RequestPause, planner.Finish:
			// OK — one of the six canonical shapes.
		default:
			t.Fatalf("Next returned unknown Decision shape: %T", dec)
		}
	})

	t.Run("WakeMode_Declared", func(t *testing.T) {
		h := factoryFunc()
		if h.Cleanup != nil {
			t.Cleanup(h.Cleanup)
		}
		if !planner.IsValidWakeMode(h.WakeMode) {
			t.Fatalf("Harness.WakeMode is not a canonical mode: %q", h.WakeMode)
		}
		p := h.Factory()
		resolved := planner.ResolveWakeMode(p)
		if !planner.IsValidWakeMode(resolved) {
			t.Fatalf("ResolveWakeMode returned non-canonical: %q", resolved)
		}
		// If the planner implements WakeAware, the declared and
		// resolved modes MUST agree. (Concretes that skip WakeAware
		// resolve to WakePush per the documented default; we only
		// assert the agreement when both are explicit.)
		if _, ok := p.(planner.WakeAware); ok && resolved != h.WakeMode {
			t.Fatalf("WakeAware planner resolves to %q but harness declares %q", resolved, h.WakeMode)
		}
	})

	t.Run("Sealed_DecisionSum", func(t *testing.T) {
		// Compile-time assertion that every Decision shape is the
		// sealed sum-type. Each line below would fail to compile if
		// the shape was removed from the sum.
		var _ planner.Decision = planner.CallTool{}
		var _ planner.Decision = planner.CallParallel{}
		var _ planner.Decision = planner.SpawnTask{}
		var _ planner.Decision = planner.AwaitTask{}
		var _ planner.Decision = planner.RequestPause{}
		var _ planner.Decision = planner.Finish{}
	})

	t.Run("TopPrompts_LLMRoundTrip", func(t *testing.T) {
		t.Skip("Phase 49: conformance scenarios — top 20 prompts against a canned LLM mock + tool catalog")
	})

	t.Run("MalformedLLM_Salvage", func(t *testing.T) {
		t.Skip("Phase 49: conformance scenarios — schema-repair pipeline exhaustion → Finish{NoPath}")
	})

	t.Run("ParallelCall_Atomicity", func(t *testing.T) {
		t.Skip("Phase 49: conformance scenarios — CallParallel branch atomicity (one bad branch fails the whole call)")
	})

	t.Run("WakeMode_RoundTrip", func(t *testing.T) {
		t.Skip("Phase 49: conformance scenarios — SpawnTask → group resolves → planner re-enters → reads MemberOutcome")
	})

	t.Run("BudgetAware_FinishDeadlineExceeded", func(t *testing.T) {
		t.Skip("Phase 49: conformance scenarios — over-budget run terminates with Finish{DeadlineExceeded}")
	})

	t.Run("PausePayload_BoundsRespected", func(t *testing.T) {
		t.Skip("Phase 49: conformance scenarios — RequestPause.Payload depth/size bounds (RFC §6.3)")
	})

	t.Run("Steering_DrainBetweenSteps", func(t *testing.T) {
		t.Skip("Phase 49: conformance scenarios — CANCEL / PAUSE / INJECT_CONTEXT applied at step boundary, never mid-step")
	})

	t.Run("ConcurrentReuse_D025", func(t *testing.T) {
		t.Skip("Phase 49: conformance scenarios — N≥100 concurrent Next calls against a shared planner instance; per-package D-025 test in finish/concurrent_test.go covers the stub")
	})
}
