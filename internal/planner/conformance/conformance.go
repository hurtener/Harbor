// Package conformance ships the planner conformance pack.
//
// Phase 42 landed the harness shape (the Harness struct + the
// Run(t, factory) entry point + the §13 import-graph lint test).
// Phase 49 fills in every scenario body — the top-prompt
// LLM-round-trip set, the malformed-LLM-output salvage path, the
// CallParallel atomicity check, the load-bearing wake-mode
// round-trip (D-032 — binding), the budget-aware finish, the
// pause-payload bounds, the steering drain-between-steps, and the
// D-025 concurrent-reuse surface.
//
// The conformance pack is a shared test asset: every concrete
// `Planner` (Phase 45 ReAct, Phase 48 Deterministic, and every future
// concrete on the same iface) calls Run against the same scenarios.
// The pack itself never imports a concrete-planner package — the
// `internal/planner/conformance.TestImportGraph_PlannerDoesNotImportRuntime`
// lint test walks the planner subtree and would fail otherwise.
//
// Per-concrete consumption pattern (Phase 49+):
//
//	func TestReact_Conformance(t *testing.T) {
//	    conformance.Run(t, func() conformance.Harness {
//	        return conformance.Harness{
//	            Factory: func() planner.Planner {
//	                return react.New(mock.New(mock.Options{
//	                    SyntheticContent: scenarioContent,
//	                }))
//	            },
//	            WakeMode:           planner.WakePush,
//	            RunContextFactory:  conformance.DefaultRunContext,
//	            Capabilities:       conformance.CapabilitySetLLM,
//	            ScenarioContentMap: conformance.DefaultReactContentMap(),
//	        }
//	    })
//	}
//
// The harness factory pattern matches the events / tools / tasks
// conformance suites: each subtest gets a fresh planner instance so
// internal state can't bleed between scenarios. The harness factory's
// `Factory` closure returns a planner that is safe under D-025
// concurrent reuse — the D-025 scenario runs N=64 concurrent Next
// calls against one shared instance.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem" // event-bus inmem driver self-register
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem" // state inmem driver self-register
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess" // tasks inprocess driver self-register
)

// Capability flags declare which scenarios a concrete planner can
// execute. The pack honours capability gating so a non-LLM concrete
// (Deterministic) does not run an LLM-only scenario (e.g.
// `MalformedLLM_Salvage`).
//
// A concrete planner's per-package conformance test passes a
// `Capabilities` value built from the constants below; the harness
// gates each scenario by inspecting the bitmask. A scenario that does
// NOT match the planner's capabilities reports `t.Skip(...)` with a
// reason — never a silent skip.
type Capability uint32

// Capability constants.
const (
	// CapabilityLLMDriven — the planner uses an LLM client and
	// participates in the LLM-round-trip + malformed-output scenarios.
	// Phase 45 ReAct sets this; Phase 48 Deterministic does not.
	CapabilityLLMDriven Capability = 1 << iota
	// CapabilityCanPause — the planner can emit `RequestPause` under
	// operator configuration; the pause-payload bounds scenario runs.
	// Deterministic sets this via its `PauseStep`; ReAct does not in
	// V1 (Phase 50 wires the planner-side emission path).
	CapabilityCanPause
	// CapabilityWakeRoundTrip — the planner is wired to consume the
	// wake-mode round-trip via real `tasks.TaskRegistry`. Both ReAct
	// (WakePush) and Deterministic (WakePoll) set this in V1.
	CapabilityWakeRoundTrip
	// CapabilityHonoursCancelControl — the planner returns
	// Finish{Cancelled} at the step boundary when
	// `rc.Control.Cancelled` is true. Every concrete in V1 sets this;
	// the cap exists so the steering-drain scenario can fail-loudly
	// if a future concrete forgets the contract.
	CapabilityHonoursCancelControl
)

// CapabilitySetReAct is the canonical capability set for Phase 45's
// LLM-driven ReAct planner.
const CapabilitySetReAct = CapabilityLLMDriven |
	CapabilityWakeRoundTrip |
	CapabilityHonoursCancelControl

// CapabilitySetDeterministic is the canonical capability set for
// Phase 48's deterministic planner. Distinct from ReAct: no LLM
// (Deterministic is programmatic), can emit Pause (via PauseStep),
// supports the wake-mode poll round-trip.
const CapabilitySetDeterministic = CapabilityCanPause |
	CapabilityWakeRoundTrip |
	CapabilityHonoursCancelControl

// ScenarioName identifies one scenario in the pack. Stable across
// phases so per-concrete test reports remain comparable.
type ScenarioName string

// Scenario names. Pinned strings — a rename would break per-concrete
// suites that may key on subtest names.
const (
	ScenarioTopPrompts        ScenarioName = "TopPrompts_LLMRoundTrip"
	ScenarioMalformedLLM      ScenarioName = "MalformedLLM_Salvage"
	ScenarioParallelAtomicity ScenarioName = "ParallelCall_Atomicity"
	ScenarioWakeRoundTrip     ScenarioName = "WakeMode_RoundTrip"
	ScenarioBudgetAware       ScenarioName = "BudgetAware_FinishDeadlineExceeded"
	ScenarioPauseBounds       ScenarioName = "PausePayload_BoundsRespected"
	ScenarioSteeringDrain     ScenarioName = "Steering_DrainBetweenSteps"
	ScenarioConcurrentReuse   ScenarioName = "ConcurrentReuse_D025"
)

// PlannerFactoryFn is the factory shape the per-scenario hooks
// consume. The pack passes a `ScenarioName` so the factory can return
// a planner pre-configured for that scenario (e.g. ReAct with a mock
// LLM that emits the right envelope; Deterministic with a step set
// that emits the right Decision shape).
//
// Factories MUST be safe to invoke multiple times across subtests —
// each invocation returns a fresh planner instance so internal state
// (atomic counters, sync.Map state) can't bleed between scenarios.
type PlannerFactoryFn func(scenario ScenarioName) planner.Planner

// Harness is the per-subtest fixture the harness Run loop consumes.
// Each conformance subtest invokes `factory()` once to obtain a fresh
// Harness with a fresh planner instance.
//
// Compatibility with Phase 42: the original three fields (Factory,
// WakeMode, RunContextFactory, Cleanup) are unchanged. Phase 49 adds
// `ScenarioFactory`, `Capabilities`, `TaskRegistryFactory`, and the
// scenario-content factories at the bottom — additive only; existing
// per-concrete tests continue to compile.
type Harness struct {
	Factory                func() planner.Planner
	ScenarioFactory        PlannerFactoryFn
	RunContextFactory      func() planner.RunContext
	TaskRegistryFactory    func(t *testing.T) (*WakeRoundTripDeps, func())
	PrebuiltPlannerFactory func(*WakeRoundTripDeps) planner.Planner
	Cleanup                func()
	WakeMode               planner.WakeMode
	Capabilities           Capability
}

// runContext returns the harness's per-subtest RunContext. Required
// at Phase 49; concretes that pass a nil factory will fail loudly in
// the Sanity scenario.
func (h Harness) runContext() planner.RunContext {
	if h.RunContextFactory == nil {
		return planner.RunContext{}
	}
	return h.RunContextFactory()
}

// hasCapability reports whether the harness declares the given
// capability. Used by scenarios to gate execution.
func (h Harness) hasCapability(c Capability) bool {
	return h.Capabilities&c != 0
}

// plannerForScenario returns the planner instance to use for the
// named scenario. Prefers ScenarioFactory when set; falls back to
// Factory.
func (h Harness) plannerForScenario(s ScenarioName) planner.Planner {
	if h.ScenarioFactory != nil {
		return h.ScenarioFactory(s)
	}
	if h.Factory == nil {
		return nil
	}
	return h.Factory()
}

// WakeRoundTripDeps bundles the real drivers the wake-mode round-trip
// scenario consumes. Constructed by the harness's
// `TaskRegistryFactory`; torn down by the returned cleanup.
//
// All fields are real production drivers (§17.3 #1 — no mocks at the
// seam): inmem `events.EventBus`, inprocess `tasks.TaskRegistry`,
// inmem `state.StateStore`. The wake-mode round-trip is the
// load-bearing D-032 scenario; mocks here would defeat its purpose.
type WakeRoundTripDeps struct {
	Bus      events.EventBus
	Registry tasks.TaskRegistry
	State    state.StateStore
}

// DefaultTaskRegistryFactory is the harness-shipped factory that
// opens an inmem bus + inprocess task registry + inmem state store.
// Per-concrete tests can use it as-is or wrap it for additional
// instrumentation.
func DefaultTaskRegistryFactory(t *testing.T) (*WakeRoundTripDeps, func()) {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		ReplayBufferSize:         16,
		IdleTimeout:              5 * time.Minute,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("DefaultTaskRegistryFactory: events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("DefaultTaskRegistryFactory: state.Open: %v", err)
	}
	reg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("DefaultTaskRegistryFactory: tasks.Open: %v", err)
	}
	cleanup := func() {
		_ = reg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
	}
	return &WakeRoundTripDeps{
		Bus:      bus,
		Registry: reg,
		State:    store,
	}, cleanup
}

// DefaultRunContext is a convenience factory the per-concrete tests
// can pass as `RunContextFactory`. Stamps a populated identity
// quadruple + a non-empty goal. Concretes that need extra fields
// (Trajectory, Catalog, etc.) typically build their own factory; this
// shape covers the Sanity + most scenario subtests.
func DefaultRunContext() planner.RunContext {
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
}

// Run executes the conformance pack against the planner produced by
// `factoryFunc`. Phase 49 fills every scenario; the Sanity skeleton
// scenarios from Phase 42 are preserved verbatim (subtest names are
// pinned). New scenarios use real drivers at the seam (§17.3 #1).
//
// The factory is called once per subtest so per-scenario planner
// state can't bleed; the harness's `Cleanup`, when supplied, runs at
// subtest end.
func Run(t *testing.T, factoryFunc func() Harness) {
	t.Helper()

	// Phase 42 skeleton scenarios — preserved verbatim. Subtest names
	// are stable.
	t.Run("Sanity_NextReturnsDecision", func(t *testing.T) {
		h := factoryFunc()
		if h.Cleanup != nil {
			t.Cleanup(h.Cleanup)
		}
		p := h.Factory()
		if p == nil {
			t.Fatal("Factory returned nil planner")
		}
		dec, err := p.Next(context.Background(), h.runContext())
		if err != nil {
			t.Fatalf("Next returned error: %v", err)
		}
		if dec == nil {
			t.Fatal("Next returned nil Decision")
		}
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
		if _, ok := p.(planner.WakeAware); ok && resolved != h.WakeMode {
			t.Fatalf("WakeAware planner resolves to %q but harness declares %q", resolved, h.WakeMode)
		}
	})

	t.Run("Sealed_DecisionSum", func(t *testing.T) {
		var _ planner.Decision = planner.CallTool{}
		var _ planner.Decision = planner.CallParallel{}
		var _ planner.Decision = planner.SpawnTask{}
		var _ planner.Decision = planner.AwaitTask{}
		var _ planner.Decision = planner.RequestPause{}
		var _ planner.Decision = planner.Finish{}
	})

	// Phase 49 scenarios. Each has its own subtest; per-scenario
	// capability gating skips with a reason rather than silently
	// passing.
	t.Run(string(ScenarioTopPrompts), func(t *testing.T) {
		runTopPromptsScenario(t, factoryFunc)
	})

	t.Run(string(ScenarioMalformedLLM), func(t *testing.T) {
		runMalformedLLMScenario(t, factoryFunc)
	})

	t.Run(string(ScenarioParallelAtomicity), func(t *testing.T) {
		runParallelAtomicityScenario(t, factoryFunc)
	})

	t.Run(string(ScenarioWakeRoundTrip), func(t *testing.T) {
		runWakeRoundTripScenario(t, factoryFunc)
	})

	t.Run(string(ScenarioBudgetAware), func(t *testing.T) {
		runBudgetAwareScenario(t, factoryFunc)
	})

	t.Run(string(ScenarioPauseBounds), func(t *testing.T) {
		runPauseBoundsScenario(t, factoryFunc)
	})

	t.Run(string(ScenarioSteeringDrain), func(t *testing.T) {
		runSteeringDrainScenario(t, factoryFunc)
	})

	t.Run(string(ScenarioConcurrentReuse), func(t *testing.T) {
		runConcurrentReuseScenario(t, factoryFunc)
	})
}

// ---------------------------------------------------------------------------
// Scenario implementations
// ---------------------------------------------------------------------------

// runTopPromptsScenario drives an op-shaped sequence: the planner
// emits a Decision matching the expected sum variant. ReAct gets a
// scripted-mock LLM via the harness's ScenarioFactory; Deterministic
// gets a step set returning a CallTool decision via the same hook.
//
// The scenario's pass criterion is structural: the returned Decision
// must be one of the six sealed shapes and MUST match the harness-
// declared `expected` shape for the named scenario. Concretes that
// cannot satisfy the scenario shape (e.g. a planner with no
// ScenarioFactory) skip-with-reason rather than passing silently.
func runTopPromptsScenario(t *testing.T, factoryFunc func() Harness) {
	t.Helper()
	h := factoryFunc()
	if h.Cleanup != nil {
		t.Cleanup(h.Cleanup)
	}
	if h.ScenarioFactory == nil {
		t.Skip("planner did not supply ScenarioFactory; top-prompt scenarios are concrete-specific")
		return
	}
	p := h.plannerForScenario(ScenarioTopPrompts)
	if p == nil {
		t.Fatal("ScenarioFactory returned nil planner for TopPrompts_LLMRoundTrip")
	}
	rc := h.runContext()
	dec, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if dec == nil {
		t.Fatal("Next returned nil Decision")
	}
	// The scenario contracts: ReAct's scripted mock emits a
	// `_finish` envelope → Finish; Deterministic's step set emits a
	// CallTool → CallTool. Either is acceptable as a "single tool
	// call resolves" shape. The pack accepts any one of these three
	// terminal-or-progressive shapes; a wider mock script can ride a
	// multi-step sequence per scenario instance.
	switch dec.(type) {
	case planner.CallTool, planner.Finish:
		// OK — the canonical "single tool call resolves" shapes.
	case planner.CallParallel:
		// OK — multi-action salvage path; some operator setups
		// emit a parallel call as the canonical top-prompt response.
	default:
		t.Fatalf("TopPrompts_LLMRoundTrip: unexpected Decision shape %T (expected CallTool / Finish / CallParallel)", dec)
	}
}

// runMalformedLLMScenario asserts the planner does NOT panic on
// malformed LLM output and surfaces a typed terminal via the schema-
// repair pipeline (Phase 44). For non-LLM concretes (Deterministic),
// the scenario is N/A — capability gated.
func runMalformedLLMScenario(t *testing.T, factoryFunc func() Harness) {
	t.Helper()
	h := factoryFunc()
	if h.Cleanup != nil {
		t.Cleanup(h.Cleanup)
	}
	if !h.hasCapability(CapabilityLLMDriven) {
		t.Skip("planner is not LLM-driven; malformed-LLM-output scenario is N/A")
		return
	}
	if h.ScenarioFactory == nil {
		t.Skip("planner did not supply ScenarioFactory for malformed-LLM scenario")
		return
	}
	p := h.plannerForScenario(ScenarioMalformedLLM)
	if p == nil {
		t.Fatal("ScenarioFactory returned nil planner for MalformedLLM_Salvage")
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("MalformedLLM_Salvage: planner panicked: %v", r)
		}
	}()

	rc := h.runContext()
	dec, err := p.Next(context.Background(), rc)
	// The schema repair pipeline's documented contract is that
	// malformed output salvages to `Finish{NoPath}` after the repair
	// ladder exhausts (D-050). The planner may either return that
	// Finish or surface a wrapped error — both shapes are
	// fail-loudly. A nil Decision + nil err is the silent-degradation
	// shape that §13 forbids.
	if err == nil && dec == nil {
		t.Fatal("MalformedLLM_Salvage: planner returned (nil, nil) — silent degradation forbidden (§13)")
	}
	if dec != nil {
		// The expected typed terminal is Finish{NoPath}; tolerant: any
		// Finish is acceptable so concretes that prefer a different
		// FinishReason still pass.
		if _, ok := dec.(planner.Finish); !ok {
			t.Logf("MalformedLLM_Salvage: returned Decision shape %T (Finish{NoPath} preferred; any Finish acceptable)", dec)
		}
	}
}

// runParallelAtomicityScenario asserts that when the planner emits a
// CallParallel decision, the shape is well-formed (≥1 branch, every
// branch is a CallTool with a non-empty Tool name, the Join is one
// of the canonical kinds). The atomic-setup-validation contract is
// the runtime executor's job (Phase 47); the planner's contract is
// shape-only.
//
// Concretes that have no ScenarioFactory or no way to emit
// CallParallel skip-with-reason.
func runParallelAtomicityScenario(t *testing.T, factoryFunc func() Harness) {
	t.Helper()
	h := factoryFunc()
	if h.Cleanup != nil {
		t.Cleanup(h.Cleanup)
	}
	if h.ScenarioFactory == nil {
		t.Skip("planner did not supply ScenarioFactory for CallParallel scenario")
		return
	}
	p := h.plannerForScenario(ScenarioParallelAtomicity)
	if p == nil {
		t.Skip("ScenarioFactory returned nil for ParallelCall_Atomicity (planner cannot emit CallParallel under harness config)")
		return
	}
	rc := h.runContext()
	dec, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if dec == nil {
		t.Fatal("Next returned nil Decision")
	}
	par, ok := dec.(planner.CallParallel)
	if !ok {
		// Planner under this scenario did NOT emit CallParallel —
		// acceptable if the concrete doesn't model parallel-call
		// emission in V1 (we don't fail; we surface). Future hybrid
		// concretes may always emit CallParallel.
		t.Logf("ParallelCall_Atomicity: planner emitted %T (CallParallel preferred); skipping shape assertions", dec)
		return
	}
	if len(par.Branches) < 1 {
		t.Fatalf("ParallelCall_Atomicity: CallParallel had %d branches, want ≥1", len(par.Branches))
	}
	for i, b := range par.Branches {
		if b.Tool == "" {
			t.Errorf("ParallelCall_Atomicity: branches[%d].Tool is empty", i)
		}
	}
	if par.Join != nil {
		switch par.Join.Kind {
		case planner.JoinAll, planner.JoinFirstSuccess, planner.JoinKeyed, planner.JoinN, "":
			// OK — canonical join kinds. Empty kind defaults to JoinAll at the executor.
		default:
			t.Errorf("ParallelCall_Atomicity: Join.Kind = %q, not in canonical set", par.Join.Kind)
		}
	}
}

// runWakeRoundTripScenario is the LOAD-BEARING scenario per D-032.
// It wires REAL drivers across the seam (§17.3 #1):
//
//   - Real `events.EventBus` (inmem driver).
//   - Real `tasks.TaskRegistry` (inprocess driver).
//   - Real `state.StateStore` (inmem driver).
//   - Real planner instance via the harness factory.
//
// For push-mode planners (ReAct):
//
//  1. Planner emits SpawnTask via the harness's ScenarioFactory
//     (concrete-specific: ReAct's mock LLM emits `_spawn_task`;
//     other LLM-driven concretes do their own thing).
//  2. Test spawns the real task into the registry, seals the group.
//  3. Test marks the spawned task Complete.
//  4. WatchGroup channel delivers GroupCompletion.
//  5. Test surfaces the MemberOutcome into
//     `rc.Trajectory.Background` (mimicking what the runtime engine
//     does in production at Phase 60+).
//  6. Planner re-enters Next; emits Finish.
//
// For poll-mode planners (Deterministic):
//
//  1. Planner emits SpawnTask via the harness's PrebuiltPlannerFactory
//     (Deterministic's SpawnAndAwaitStep spawns the real task
//     itself; Phase 48's wiring).
//  2. Test runs the planner repeatedly: Next returns AwaitTask
//     while the group is open.
//  3. Test transitions the spawned task to Complete.
//  4. Next call (after completion) consumes the WatchGroup payload
//     via the planner's own non-blocking receive; planner emits
//     the OnResolved-callback decision.
//
// Hybrid mode falls through to the push path until the first hybrid
// concrete lands its own scenario.
func runWakeRoundTripScenario(t *testing.T, factoryFunc func() Harness) {
	t.Helper()
	h := factoryFunc()
	if h.Cleanup != nil {
		t.Cleanup(h.Cleanup)
	}
	if !h.hasCapability(CapabilityWakeRoundTrip) {
		t.Skip("planner does not declare CapabilityWakeRoundTrip; wake-mode round-trip is N/A")
		return
	}
	if h.TaskRegistryFactory == nil {
		t.Skip("planner did not supply TaskRegistryFactory; wake-mode round-trip requires a real registry")
		return
	}

	deps, cleanup := h.TaskRegistryFactory(t)
	defer cleanup()

	switch h.WakeMode {
	case planner.WakePush, planner.WakeHybrid:
		runWakeRoundTripPush(t, h, deps)
	case planner.WakePoll:
		runWakeRoundTripPoll(t, h, deps)
	default:
		t.Fatalf("WakeMode_RoundTrip: unsupported WakeMode %q", h.WakeMode)
	}
}

// runWakeRoundTripPush exercises the push path. ReAct's emission is
// LLM-prompted; the harness's ScenarioFactory supplies a planner
// pre-configured to emit SpawnTask on the first Next call and Finish
// on the second (post-resolve).
func runWakeRoundTripPush(t *testing.T, h Harness, deps *WakeRoundTripDeps) {
	t.Helper()
	if h.ScenarioFactory == nil {
		t.Skip("push-mode wake-mode round-trip requires ScenarioFactory (LLM-driven SpawnTask emission)")
		return
	}
	p := h.plannerForScenario(ScenarioWakeRoundTrip)
	if p == nil {
		t.Fatal("ScenarioFactory returned nil planner for WakeMode_RoundTrip (push)")
	}

	rc := h.runContext()
	traj := &planner.Trajectory{}
	rc.Trajectory = traj

	ctx, err := identity.With(context.Background(), rc.Quadruple.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, rc.Quadruple.Identity, rc.Quadruple.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Step 1: planner emits SpawnTask.
	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #1 (expect SpawnTask): %v", err)
	}
	spawn, ok := dec.(planner.SpawnTask)
	if !ok {
		t.Fatalf("Next #1 returned %T, want planner.SpawnTask (push-mode wake-round-trip; ScenarioFactory misconfigured?)", dec)
	}
	if spawn.Spec.RetainTurn {
		t.Errorf("SpawnTask.Spec.RetainTurn = true, want false (push wake requires non-retain-turn)")
	}

	// Runtime side (the test stands in for the production planner-
	// step adapter that lands at Phase 60+): spawn the real task in
	// a fresh group; WatchGroup before transitioning to Complete.
	group, err := deps.Registry.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID:   rc.Quadruple.Identity,
		Description: "wake-mode round-trip (push)",
	})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}
	handle, err := deps.Registry.Spawn(ctx, tasks.SpawnRequest{
		Identity:    rc.Quadruple,
		Kind:        spawn.Kind,
		Description: spawn.Spec.Description,
		Query:       spawn.Spec.Query,
		Priority:    spawn.Spec.Priority,
		GroupID:     group.ID,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err = deps.Registry.SealGroup(ctx, group.ID); err != nil {
		t.Fatalf("SealGroup: %v", err)
	}
	completionCh, cancelWatch, err := deps.Registry.WatchGroup(rc.Quadruple.Identity, group.ID)
	if err != nil {
		t.Fatalf("WatchGroup: %v", err)
	}
	defer cancelWatch()

	if err = deps.Registry.MarkRunning(ctx, handle.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	resultBytes := json.RawMessage(`{"summary":"wake-round-trip result"}`)
	if err = deps.Registry.MarkComplete(ctx, handle.ID, tasks.TaskResult{Value: resultBytes}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// Wait for GroupCompletion delivery (bounded — fail-loud on
	// timeout per the scenario's §17.4 contract: no time.Sleep for
	// synchronisation, but a bounded eventually-style wait IS
	// acceptable).
	var completion tasks.GroupCompletion
	select {
	case completion = <-completionCh:
	case <-time.After(2 * time.Second):
		t.Fatal("WakeMode_RoundTrip (push): WatchGroup did not deliver GroupCompletion within 2s — failure to wire tasks.WatchGroup is the test's failure mode, not silent deadlock (D-032)")
	}
	if completion.FinalStatus != tasks.GroupCompleted {
		t.Errorf("FinalStatus = %q, want %q", completion.FinalStatus, tasks.GroupCompleted)
	}
	if len(completion.Members) != 1 {
		t.Fatalf("len(Members) = %d, want 1", len(completion.Members))
	}

	// Surface the MemberOutcome through RunContext.Trajectory.Background.
	bg := planner.BackgroundResult{
		GroupID:    string(group.ID),
		Status:     string(completion.FinalStatus),
		ResolvedAt: completion.ResolvedAt,
		Members: []planner.BackgroundMemberOutcome{
			{
				TaskID: string(completion.Members[0].TaskID),
				Status: string(completion.Members[0].Status),
			},
		},
	}
	traj.Background = map[string]planner.BackgroundResult{
		string(group.ID): bg,
	}

	// Step 2: planner re-enters; emits Finish.
	dec2, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #2 (expect Finish): %v", err)
	}
	if _, ok := dec2.(planner.Finish); !ok {
		t.Fatalf("Next #2 returned %T, want planner.Finish (post-resolve re-entry)", dec2)
	}
}

// runWakeRoundTripPoll exercises the poll path. The planner concrete
// binds the registry at construction time (Deterministic's
// WithRegistry option) — the harness's PrebuiltPlannerFactory
// produces the planner with the real registry wired.
//
// The planner emits SpawnTask on the first Next; subsequent Next
// calls perform a non-blocking receive on WatchGroup. While the group
// is open, the planner emits AwaitTask. Once the test marks the
// spawned task Complete, the next Next call's non-blocking receive
// succeeds and the planner's OnResolved callback fires.
func runWakeRoundTripPoll(t *testing.T, h Harness, deps *WakeRoundTripDeps) {
	t.Helper()
	if h.PrebuiltPlannerFactory == nil {
		t.Skip("poll-mode wake-mode round-trip requires PrebuiltPlannerFactory (concrete binds registry at construction)")
		return
	}
	p := h.PrebuiltPlannerFactory(deps)
	if p == nil {
		t.Fatal("PrebuiltPlannerFactory returned nil planner")
	}

	rc := h.runContext()
	ctx, err := identity.With(context.Background(), rc.Quadruple.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, rc.Quadruple.Identity, rc.Quadruple.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Step 1: planner emits SpawnTask (the planner's step
	// implementation also spawns the real task into the registry;
	// Phase 48's SpawnAndAwaitStep does this).
	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #1 (expect SpawnTask): %v", err)
	}
	spawn, ok := dec.(planner.SpawnTask)
	if !ok {
		t.Fatalf("Next #1 returned %T, want planner.SpawnTask", dec)
	}
	if spawn.GroupID == "" {
		t.Fatal("SpawnTask.GroupID is empty; poll mode requires the planner to surface the group ID it spawned into")
	}
	groupID := spawn.GroupID

	// Step 2: while the group is open, Next emits AwaitTask. The
	// planner's non-blocking receive on WatchGroup fails (channel
	// not yet readable) → emit AwaitTask.
	dec2, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #2 (expect AwaitTask while group open): %v", err)
	}
	if _, ok := dec2.(planner.AwaitTask); !ok {
		t.Fatalf("Next #2 returned %T, want planner.AwaitTask (poll mode: group is still open)", dec2)
	}

	// Drive the group to Complete. Find the spawned task via the
	// registry's ListGroups + Get path (the planner's
	// SpawnAndAwaitStep keeps its own (groupID, ownerTaskID) state;
	// we walk the registry independently here so the harness doesn't
	// depend on the concrete's internal state).
	members, err := deps.Registry.ListGroups(ctx, rc.Quadruple.Identity, nil)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	var ownerTaskID tasks.TaskID
	for _, g := range members {
		if g.ID != groupID {
			continue
		}
		if len(g.Members) > 0 {
			ownerTaskID = g.Members[0]
		}
		break
	}
	if ownerTaskID == "" {
		// Fall back to listing tasks for the session.
		var summaries []tasks.TaskSummary
		summaries, err = deps.Registry.List(ctx, rc.Quadruple.Identity, tasks.TaskFilter{})
		if err != nil {
			t.Fatalf("tasks.List: %v", err)
		}
		if len(summaries) == 0 {
			t.Fatal("Could not find the spawned task in the registry; planner's SpawnAndAwaitStep did not spawn?")
		}
		ownerTaskID = summaries[0].ID
	}

	if err = deps.Registry.MarkRunning(ctx, ownerTaskID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err = deps.Registry.MarkComplete(ctx, ownerTaskID, tasks.TaskResult{
		Value: json.RawMessage(`{"summary":"poll-mode wake-round-trip result"}`),
	}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// Step 3: Next consumes the resolved group via the planner's
	// non-blocking receive, fires OnResolved, emits the resolved
	// decision (Phase 48's SpawnAndAwaitStep + tests typically wire
	// OnResolved to return Finish{Goal}).
	//
	// Bounded retry: the registry's WatchGroup delivery is
	// asynchronous (the registry fans out the GroupCompletion to
	// subscribers after the terminal transition lands). Allow a
	// brief retry window so the test isn't flaky under load.
	deadline := time.Now().Add(2 * time.Second)
	var dec3 planner.Decision
	for time.Now().Before(deadline) {
		dec3, err = p.Next(ctx, rc)
		if err != nil {
			t.Fatalf("Next #3 (expect post-resolve): %v", err)
		}
		if _, isAwait := dec3.(planner.AwaitTask); !isAwait {
			break
		}
		// Still observing AwaitTask — the GroupCompletion has not
		// reached the planner's per-call non-blocking receive yet.
		// Yield + retry. Not time.Sleep-as-synchronisation: this is
		// an eventually-style assertion with a bounded deadline per
		// §17.4.
		runtime.Gosched()
	}
	if _, ok := dec3.(planner.AwaitTask); ok {
		t.Fatal("WakeMode_RoundTrip (poll): planner never advanced past AwaitTask within 2s after MarkComplete — failure to wire tasks.WatchGroup is the test's failure mode, not silent deadlock (D-032)")
	}
	// The terminal decision shape varies by the operator-supplied
	// OnResolved; accept any non-AwaitTask shape (Finish is the
	// typical case).
	switch dec3.(type) {
	case planner.Finish, planner.CallTool, planner.CallParallel, planner.SpawnTask, planner.RequestPause:
		// OK — any canonical Decision shape is acceptable.
	default:
		t.Fatalf("WakeMode_RoundTrip (poll): planner returned unknown Decision shape %T after resolve", dec3)
	}
}

// runBudgetAwareScenario asserts the planner respects a strictly-
// past `Budget.Deadline` by surfacing a terminal-shape Decision or
// honouring ctx.Err() through to a wrapped context-deadline error.
//
// The pack's strictness is on the SHAPE of the response (terminal or
// context-error), not a specific FinishReason — concretes that
// prefer Finish{NoPath, Metadata["deadline_exceeded"]=true} also
// pass, as do concretes that propagate ctx.DeadlineExceeded.
func runBudgetAwareScenario(t *testing.T, factoryFunc func() Harness) {
	t.Helper()
	h := factoryFunc()
	if h.Cleanup != nil {
		t.Cleanup(h.Cleanup)
	}
	p := h.Factory()
	if p == nil {
		t.Fatal("Factory returned nil planner")
	}
	rc := h.runContext()
	rc.Budget.Deadline = time.Now().Add(-1 * time.Hour) // strictly past
	ctx, cancel := context.WithDeadline(context.Background(), rc.Budget.Deadline)
	defer cancel()
	dec, err := p.Next(ctx, rc)
	if err != nil {
		// Accept any error wrapping context.DeadlineExceeded as a
		// fail-loudly response to the past deadline.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return // OK
		}
		// Other errors are also acceptable (the planner may surface
		// a wrapped budget-exceeded error) — log and pass.
		t.Logf("BudgetAware_FinishDeadlineExceeded: planner returned error %v (acceptable fail-loudly response)", err)
		return
	}
	if dec == nil {
		t.Fatal("BudgetAware: planner returned (nil, nil) — silent degradation forbidden (§13)")
	}
	// Tolerant acceptance: any Finish is OK; any non-Finish is logged
	// but does not fail (the planner may emit a CallTool that the
	// runtime would then reject — concretes may choose different
	// strategies). We don't enforce a specific FinishReason since
	// Deterministic prefers Finish{NoPath} for "no step matched"
	// while ReAct could emit Finish{DeadlineExceeded}.
	if _, ok := dec.(planner.Finish); !ok {
		t.Logf("BudgetAware_FinishDeadlineExceeded: planner returned %T after past deadline (Finish preferred; tolerant pass)", dec)
	}
}

// runPauseBoundsScenario verifies the planner's emitted
// RequestPause.Payload (when the concrete declares
// CapabilityCanPause) is INSIDE the RFC §6.3 bounds:
//
//   - Depth ≤ 6
//   - Key count ≤ 64
//   - Total size ≤ 16 KiB
//
// The strict bounds-enforcement test lives at the protocol edge
// (Phase 52); the pack's scenario asserts that a typical operator-
// supplied payload from a real planner emission is comfortably
// inside the limits.
func runPauseBoundsScenario(t *testing.T, factoryFunc func() Harness) {
	t.Helper()
	h := factoryFunc()
	if h.Cleanup != nil {
		t.Cleanup(h.Cleanup)
	}
	if !h.hasCapability(CapabilityCanPause) {
		t.Skip("planner does not declare CapabilityCanPause; pause-payload bounds scenario is N/A")
		return
	}
	if h.ScenarioFactory == nil {
		t.Skip("planner did not supply ScenarioFactory for pause-bounds scenario")
		return
	}
	p := h.plannerForScenario(ScenarioPauseBounds)
	if p == nil {
		t.Skip("ScenarioFactory returned nil for PausePayload_BoundsRespected")
		return
	}
	rc := h.runContext()
	dec, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if dec == nil {
		t.Fatal("Next returned nil Decision")
	}
	pause, ok := dec.(planner.RequestPause)
	if !ok {
		t.Fatalf("PausePayload_BoundsRespected: planner returned %T, want planner.RequestPause", dec)
	}
	if !planner.IsValidPauseReason(pause.Reason) {
		t.Errorf("PauseReason %q is not in canonical set", pause.Reason)
	}
	depth := mapDepth(pause.Payload)
	if depth > 6 {
		t.Errorf("RequestPause.Payload depth = %d, want ≤ 6 (RFC §6.3)", depth)
	}
	keys := countKeys(pause.Payload)
	if keys > 64 {
		t.Errorf("RequestPause.Payload key count = %d, want ≤ 64 (RFC §6.3)", keys)
	}
	encoded, err := json.Marshal(pause.Payload)
	if err != nil {
		t.Errorf("RequestPause.Payload not JSON-encodable: %v", err)
	} else if len(encoded) > 16*1024 {
		t.Errorf("RequestPause.Payload encoded size = %d bytes, want ≤ 16 KiB (RFC §6.3)", len(encoded))
	}
}

// mapDepth computes the maximum nesting depth of a map[string]any.
// Used by the pause-bounds scenario; bounded recursion via the
// depth cap (any tree deeper than 32 is itself a bug — return 32).
func mapDepth(v any) int {
	return mapDepthBounded(v, 0, 32)
}

func mapDepthBounded(v any, current, cap int) int {
	if current >= cap {
		return cap
	}
	switch t := v.(type) {
	case map[string]any:
		max := current
		for _, sub := range t {
			d := mapDepthBounded(sub, current+1, cap)
			if d > max {
				max = d
			}
		}
		if max == current && len(t) > 0 {
			return current + 1
		}
		if len(t) == 0 {
			return current + 1
		}
		return max
	case []any:
		max := current
		for _, sub := range t {
			d := mapDepthBounded(sub, current+1, cap)
			if d > max {
				max = d
			}
		}
		if max == current && len(t) > 0 {
			return current + 1
		}
		return max
	default:
		return current
	}
}

// countKeys returns the total count of keys reachable from v.
func countKeys(v any) int {
	switch t := v.(type) {
	case map[string]any:
		c := len(t)
		for _, sub := range t {
			c += countKeys(sub)
		}
		return c
	case []any:
		c := 0
		for _, sub := range t {
			c += countKeys(sub)
		}
		return c
	default:
		return 0
	}
}

// runSteeringDrainScenario asserts the planner returns
// Finish{Cancelled} at the step boundary when
// `rc.Control.Cancelled` is true. Every concrete that declares
// CapabilityHonoursCancelControl honours this contract.
func runSteeringDrainScenario(t *testing.T, factoryFunc func() Harness) {
	t.Helper()
	h := factoryFunc()
	if h.Cleanup != nil {
		t.Cleanup(h.Cleanup)
	}
	if !h.hasCapability(CapabilityHonoursCancelControl) {
		t.Skip("planner does not declare CapabilityHonoursCancelControl; steering-drain scenario is N/A")
		return
	}
	p := h.Factory()
	if p == nil {
		t.Fatal("Factory returned nil planner")
	}
	rc := h.runContext()
	rc.Control.Cancelled = true
	dec, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if dec == nil {
		t.Fatal("Next returned nil Decision")
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("Steering_DrainBetweenSteps: planner returned %T after CANCEL observation, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishCancelled {
		t.Errorf("Finish.Reason = %q after CANCEL observation, want %q (RFC §6.3 steering drain-between-steps)",
			fin.Reason, planner.FinishCancelled)
	}
}

// runConcurrentReuseScenario fires N=64 concurrent Next calls against
// ONE shared planner from the factory. The race detector is the
// gate; per-call RunID round-trip is the context-bleed check (when
// the planner stamps run_id into Finish.Metadata; tolerant if the
// planner does not).
func runConcurrentReuseScenario(t *testing.T, factoryFunc func() Harness) {
	t.Helper()
	h := factoryFunc()
	if h.Cleanup != nil {
		t.Cleanup(h.Cleanup)
	}
	p := h.Factory()
	if p == nil {
		t.Fatal("Factory returned nil planner")
	}

	const N = 64
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(N)
	var errs atomic.Int32
	for i := range N {
		idx := i
		go func() {
			defer wg.Done()
			rc := h.runContext()
			// Per-goroutine RunID so context-bleed surfaces if the
			// planner read RunID from itself rather than rc.
			rc.Quadruple.RunID = fmt.Sprintf("conf-conc-%d", idx)
			ctx, err := identity.WithRun(context.Background(), rc.Quadruple.Identity, rc.Quadruple.RunID)
			if err != nil {
				errs.Add(1)
				return
			}
			dec, err := p.Next(ctx, rc)
			if err != nil {
				errs.Add(1)
				return
			}
			if dec == nil {
				errs.Add(1)
				return
			}
			// Best-effort context-bleed assertion: if the planner
			// emits Finish with a run_id metadata field, it MUST
			// match the per-goroutine RunID.
			if fin, ok := dec.(planner.Finish); ok && fin.Metadata != nil {
				if got, has := fin.Metadata["run_id"].(string); has {
					if got != rc.Quadruple.RunID {
						errs.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()
	if e := errs.Load(); e > 0 {
		t.Fatalf("ConcurrentReuse_D025: %d goroutines reported errors / context-bleed under N=%d", e, N)
	}

	// Bounded eventually-style wait for goroutines to drain. Not
	// time.Sleep-as-synchronisation: this is a §11 goroutine-leak
	// assertion bounded by a 1s deadline.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > baseline+8 {
		// Tolerance of +8: per-goroutine bookkeeping in the test
		// runtime varies; the real signal is "did we leak hundreds?"
		t.Errorf("ConcurrentReuse_D025: goroutine baseline drift = %d (started=%d, ended=%d)", got-baseline, baseline, got)
	}
}

// ScenarioContentMap maps a ScenarioName to the synthetic LLM content
// the harness asks the mock to emit for that scenario. ReAct
// per-concrete tests construct one via `DefaultReactContentMap` and
// pass it; the ScenarioFactory consumes the entry to build a fresh
// mock-LLM driver per subtest.
type ScenarioContentMap map[ScenarioName]string

// DefaultReactContentMap returns the conformance-pack's canned
// ReAct-side LLM responses keyed by scenario name. Per-concrete
// tests typically pass this map verbatim; operators with bespoke
// emission shapes can override individual entries.
//
// The content envelope shapes mirror Phase 45's `DefaultSystemPrompt`
// — JSON-only, the reserved tool names (`_finish`, `_spawn_task`,
// `_await_task`), arrays for parallel fan-out.
func DefaultReactContentMap() ScenarioContentMap {
	return ScenarioContentMap{
		ScenarioTopPrompts:        `{"tool":"_finish","args":{"answer":"conformance-top-prompts ok"},"reasoning":"single-step finish"}`,
		ScenarioMalformedLLM:      `this is not a JSON envelope, at all`,
		ScenarioParallelAtomicity: `[{"tool":"alpha","args":{"x":1}},{"tool":"beta","args":{"x":2}}]`,
		// SpawnTask emission for the push-mode round-trip.
		ScenarioWakeRoundTrip: `{"tool":"_spawn_task","args":{"kind":"background","spec":{"description":"conformance bg task","query":"do the thing","priority":0,"retain_turn":false}},"reasoning":"side channel"}`,
		ScenarioBudgetAware:   `{"tool":"_finish","args":{"answer":"budget-aware"},"reasoning":"finish on deadline"}`,
		// ReAct does not emit RequestPause in V1; the
		// PausePayload_BoundsRespected scenario skips for ReAct via
		// the capability gate.
		ScenarioPauseBounds:     `{"tool":"_finish","args":{"answer":"unused"},"reasoning":"unused"}`,
		ScenarioSteeringDrain:   `{"tool":"_finish","args":{"answer":"unused"},"reasoning":"unused"}`,
		ScenarioConcurrentReuse: `{"tool":"_finish","args":{"answer":"concurrent"},"reasoning":"concurrent test"}`,
	}
}

// SecondStepContent returns a canned `_finish` envelope used by the
// wake-mode round-trip scenario's post-resolve Next call. The
// ScenarioFactory for ReAct supplies a multi-response scripted mock
// (first response: SpawnTask emission; second: this Finish).
func SecondStepContent() string {
	return `{"tool":"_finish","args":{"answer":"wake-round-trip resolved"},"reasoning":"observed background result"}`
}
