package conformance_test

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/conformance"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestConformance_SelfTest exercises the conformance pack against an
// in-package stub planner so the pack's coverage gates fire. The
// stub uses a sentinel-driven `Next` that returns one of the six
// canonical Decision shapes per scenario (selected by the
// ScenarioFactory hook).
//
// Why a separate self-test rather than coverage-via-consumer:
// per-concrete tests (`internal/planner/react/conformance_test.go`,
// `internal/planner/deterministic/conformance_test.go`) DO exercise
// every scenario body, but `go test -coverprofile` measures coverage
// per the test's own package. The conformance pack's coverage target
// (Phase 49: 80%) is met when this self-test runs.
func TestConformance_SelfTest(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		return conformance.Harness{
			Factory: func() planner.Planner {
				return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
			},
			ScenarioFactory: func(s conformance.ScenarioName) planner.Planner {
				switch s {
				case conformance.ScenarioTopPrompts:
					return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
				case conformance.ScenarioParallelAtomicity:
					return &selfStubPlanner{
						decision: planner.CallParallel{
							Branches: []planner.CallTool{
								{Tool: "alpha"},
								{Tool: "beta"},
							},
							Join: &planner.JoinSpec{Kind: planner.JoinAll},
						},
					}
				case conformance.ScenarioPauseBounds:
					return &selfStubPlanner{
						decision: planner.RequestPause{
							Reason: planner.PauseAwaitInput,
							Payload: map[string]any{
								"question": "self-test",
							},
						},
					}
				default:
					return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
				}
			},
			WakeMode: planner.WakePoll, // self-stub picks poll so the scenario fires WITHOUT
			// requiring a real registry — the wake-round-trip skips
			// at the PrebuiltPlannerFactory guard for self-test
			// (we do NOT supply one, so the scenario skips with a
			// reason, fully covering the gating path).
			Capabilities: conformance.CapabilityCanPause |
				conformance.CapabilityHonoursCancelControl,
			TaskRegistryFactory: conformance.DefaultTaskRegistryFactory,
			RunContextFactory: func() planner.RunContext {
				return planner.RunContext{
					Quadruple: identity.Quadruple{
						Identity: identity.Identity{
							TenantID:  "selftest-tenant",
							UserID:    "selftest-user",
							SessionID: "selftest-session",
						},
						RunID: "selftest-run",
					},
					Goal: "self-test",
				}
			},
		}
	})
}

// TestConformance_SelfTest_LLMScenariosFire exercises the LLM-driven
// scenarios (TopPrompts, MalformedLLM) by declaring
// CapabilityLLMDriven on a separate stub. The malformed-LLM scenario
// asserts the planner does NOT panic + surfaces a typed terminal;
// the self-stub returns a Finish to satisfy.
func TestConformance_SelfTest_LLMScenariosFire(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		return conformance.Harness{
			Factory: func() planner.Planner {
				return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
			},
			ScenarioFactory: func(_ conformance.ScenarioName) planner.Planner {
				return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
			},
			WakeMode: planner.WakePush,
			Capabilities: conformance.CapabilityLLMDriven |
				conformance.CapabilityHonoursCancelControl,
			// No TaskRegistryFactory — wake-round-trip skips with a
			// reason (covers the "harness did not supply" branch).
			// No PrebuiltPlannerFactory — push-mode round-trip also
			// skips ScenarioFactory-supplied LLM emission since the
			// self-stub doesn't emit SpawnTask. The scenario falls
			// through to a Finish (acceptable per the assertion).
			RunContextFactory: func() planner.RunContext {
				return planner.RunContext{
					Quadruple: identity.Quadruple{
						Identity: identity.Identity{
							TenantID:  "selftest-tenant",
							UserID:    "selftest-user",
							SessionID: "selftest-session",
						},
						RunID: "selftest-run",
					},
					Goal: "self-test LLM scenarios",
				}
			},
		}
	})
}

// TestConformance_DefaultRunContext exercises the harness's default
// RunContext factory shape — covers the helper used by per-concrete
// tests that don't need a bespoke factory.
func TestConformance_DefaultRunContext(t *testing.T) {
	rc := conformance.DefaultRunContext()
	if rc.Quadruple.TenantID == "" || rc.Quadruple.UserID == "" || rc.Quadruple.SessionID == "" || rc.Quadruple.RunID == "" {
		t.Errorf("DefaultRunContext returned incomplete quadruple: %+v", rc.Quadruple)
	}
	if rc.Goal == "" {
		t.Errorf("DefaultRunContext returned empty Goal")
	}
}

// TestConformance_DefaultTaskRegistryFactory exercises the shipped
// factory's open + close path so coverage gates hit the helper.
func TestConformance_DefaultTaskRegistryFactory(t *testing.T) {
	deps, cleanup := conformance.DefaultTaskRegistryFactory(t)
	defer cleanup()
	if deps == nil {
		t.Fatal("DefaultTaskRegistryFactory returned nil deps")
	}
	if deps.Bus == nil {
		t.Error("WakeRoundTripDeps.Bus is nil")
	}
	if deps.Registry == nil {
		t.Error("WakeRoundTripDeps.Registry is nil")
	}
	if deps.State == nil {
		t.Error("WakeRoundTripDeps.State is nil")
	}
}

// TestConformance_WakeRoundTrip_Push_Stub exercises the push-mode
// wake-round-trip path against a SpawnTask-emitting self-stub. The
// scenario depends on the harness's TaskRegistryFactory firing
// against real drivers; the self-stub's SpawnTask emission triggers
// the runtime-side spawn + WatchGroup + resolve path, then the
// stub's second `Next` call returns Finish.
func TestConformance_WakeRoundTrip_Push_Stub(t *testing.T) {
	// Build a single stub that walks through (SpawnTask, Finish) on
	// successive Next calls. Wrapped in a tiny ScenarioFactory that
	// returns a fresh instance per scenario.
	conformance.Run(t, func() conformance.Harness {
		return conformance.Harness{
			Factory: func() planner.Planner {
				return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
			},
			ScenarioFactory: func(s conformance.ScenarioName) planner.Planner {
				if s == conformance.ScenarioWakeRoundTrip {
					return &spawnFinishStub{}
				}
				return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
			},
			WakeMode: planner.WakePush,
			Capabilities: conformance.CapabilityWakeRoundTrip |
				conformance.CapabilityHonoursCancelControl,
			TaskRegistryFactory: conformance.DefaultTaskRegistryFactory,
			RunContextFactory: func() planner.RunContext {
				return planner.RunContext{
					Quadruple: identity.Quadruple{
						Identity: identity.Identity{
							TenantID:  "selftest-tenant",
							UserID:    "selftest-user",
							SessionID: "selftest-session-push",
						},
						RunID: "selftest-run-push",
					},
					Goal: "self-test push round-trip",
				}
			},
		}
	})
}

// TestConformance_WakeRoundTrip_Poll_Stub exercises the poll-mode
// wake-round-trip path against a stub whose first Next returns
// SpawnTask with a registry-resolvable GroupID; subsequent calls
// emit AwaitTask until the group resolves, then Finish.
//
// The stub mirrors what Phase 48's SpawnAndAwaitStep does: a real
// Spawn against the bound registry, then per-call non-blocking
// receive on WatchGroup.
func TestConformance_WakeRoundTrip_Poll_Stub(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		return conformance.Harness{
			Factory: func() planner.Planner {
				return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
			},
			ScenarioFactory: func(_ conformance.ScenarioName) planner.Planner {
				return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
			},
			PrebuiltPlannerFactory: func(deps *conformance.WakeRoundTripDeps) planner.Planner {
				return &pollWakeStub{deps: deps}
			},
			WakeMode: planner.WakePoll,
			Capabilities: conformance.CapabilityWakeRoundTrip |
				conformance.CapabilityHonoursCancelControl,
			TaskRegistryFactory: conformance.DefaultTaskRegistryFactory,
			RunContextFactory: func() planner.RunContext {
				return planner.RunContext{
					Quadruple: identity.Quadruple{
						Identity: identity.Identity{
							TenantID:  "selftest-tenant",
							UserID:    "selftest-user",
							SessionID: "selftest-session-poll",
						},
						RunID: "selftest-run-poll",
					},
					Goal: "self-test poll round-trip",
				}
			},
		}
	})
}

// TestConformance_FactoryFallback_NoScenarioFactory exercises the
// plannerForScenario fallback path when ScenarioFactory is nil.
// Covers the gating branches in scenarios that have no scenario-
// specific configuration.
func TestConformance_FactoryFallback_NoScenarioFactory(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		return conformance.Harness{
			Factory: func() planner.Planner {
				return &selfStubPlanner{decision: planner.Finish{Reason: planner.FinishGoal}}
			},
			// ScenarioFactory deliberately nil — exercises the
			// Skip-with-reason path in scenarios that require it.
			WakeMode:     planner.WakePush,
			Capabilities: conformance.CapabilityHonoursCancelControl,
			RunContextFactory: func() planner.RunContext {
				return planner.RunContext{
					Quadruple: identity.Quadruple{
						Identity: identity.Identity{
							TenantID:  "selftest-tenant",
							UserID:    "selftest-user",
							SessionID: "selftest-session-fallback",
						},
						RunID: "selftest-run-fallback",
					},
					Goal: "self-test fallback",
				}
			},
		}
	})
}

// TestConformance_DefaultReactContentMap_PopulatesEveryScenario
// asserts the canned ReAct content map covers every scenario the
// pack ships. A missing entry would surface as the per-concrete
// ScenarioFactory falling back to a generic _finish envelope; the
// test pins the expected coverage shape.
func TestConformance_DefaultReactContentMap_PopulatesEveryScenario(t *testing.T) {
	m := conformance.DefaultReactContentMap()
	wantScenarios := []conformance.ScenarioName{
		conformance.ScenarioTopPrompts,
		conformance.ScenarioMalformedLLM,
		conformance.ScenarioParallelAtomicity,
		conformance.ScenarioWakeRoundTrip,
		conformance.ScenarioBudgetAware,
	}
	for _, s := range wantScenarios {
		if _, ok := m[s]; !ok {
			t.Errorf("DefaultReactContentMap missing entry for %q", s)
		}
	}
	if conformance.SecondStepContent() == "" {
		t.Error("SecondStepContent returned empty")
	}
}

// TestConformance_SelfTest_MinimalCapabilities runs the pack against
// a stub that declares NO optional capabilities and returns a
// CallTool decision. This exercises two branch classes the
// fully-capable self-tests above never reach:
//
//   - the capability-gated `t.Skip(...)` branches in the LLM-driven,
//     pause-bounds, wake-round-trip, and steering-drain scenarios
//     (a planner that declares the capability never hits the skip);
//   - the tolerant non-Finish `t.Logf(...)` branches in scenarios
//     like BudgetAware that accept — but log — a non-Finish shape.
func TestConformance_SelfTest_MinimalCapabilities(t *testing.T) {
	conformance.Run(t, func() conformance.Harness {
		return conformance.Harness{
			Factory: func() planner.Planner {
				return &selfStubPlanner{decision: planner.CallTool{Tool: "self-stub-tool"}}
			},
			ScenarioFactory: func(_ conformance.ScenarioName) planner.Planner {
				return &selfStubPlanner{decision: planner.CallTool{Tool: "self-stub-tool"}}
			},
			WakeMode: planner.WakePush,
			// Capabilities deliberately empty — every capability-gated
			// scenario takes its Skip-with-reason branch.
			RunContextFactory: func() planner.RunContext {
				return planner.RunContext{
					Quadruple: identity.Quadruple{
						Identity: identity.Identity{
							TenantID:  "selftest-tenant",
							UserID:    "selftest-user",
							SessionID: "selftest-session-minimal",
						},
						RunID: "selftest-run-minimal",
					},
					Goal: "self-test minimal capabilities",
				}
			},
		}
	})
}

// spawnFinishStub returns SpawnTask on the first Next call and
// Finish on subsequent calls. Used by the push-mode wake-round-trip
// self-test to exercise the runtime-side spawn → resolve flow.
type spawnFinishStub struct {
	mu     sync.Mutex
	called int
}

func (s *spawnFinishStub) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called++
	if s.called == 1 {
		return planner.SpawnTask{
			Kind: "background",
			Spec: planner.SpawnSpec{
				Description: "self-test push round-trip",
				Query:       "self-test query",
				Priority:    0,
				RetainTurn:  false,
			},
		}, nil
	}
	return planner.Finish{
		Reason: planner.FinishGoal,
		Metadata: map[string]any{
			"selftest_wake_resolved": true,
			"run_id":                 rc.Quadruple.RunID,
		},
	}, nil
}

// pollWakeStub mirrors what Phase 48's SpawnAndAwaitStep does: the
// first Next call spawns a real task in the bound registry and
// returns SpawnTask{GroupID}; subsequent calls perform a
// non-blocking receive on WatchGroup. While the group is open the
// stub returns AwaitTask; once it resolves the stub returns Finish.
type pollWakeStub struct {
	mu          sync.Mutex
	deps        *conformance.WakeRoundTripDeps
	spawned     bool
	resolved    bool
	groupID     tasks.TaskGroupID
	ownerTaskID tasks.TaskID
}

func (s *pollWakeStub) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved {
		return planner.Finish{
			Reason: planner.FinishGoal,
			Metadata: map[string]any{
				"selftest_wake_resolved": true,
				"run_id":                 rc.Quadruple.RunID,
			},
		}, nil
	}
	if !s.spawned {
		// Attach identity to ctx for the registry call.
		idCtx, err := identity.With(ctx, rc.Quadruple.Identity)
		if err != nil {
			return nil, err
		}
		group, err := s.deps.Registry.ResolveOrCreateGroup(idCtx, tasks.GroupRequest{
			SessionID:   rc.Quadruple.Identity,
			Description: "selftest poll group",
		})
		if err != nil {
			return nil, err
		}
		handle, err := s.deps.Registry.Spawn(idCtx, tasks.SpawnRequest{
			Identity:    rc.Quadruple,
			Kind:        tasks.KindBackground,
			Description: "selftest poll spawn",
			Query:       "self-test query",
			GroupID:     group.ID,
		})
		if err != nil {
			return nil, err
		}
		if err := s.deps.Registry.SealGroup(idCtx, group.ID); err != nil {
			return nil, err
		}
		s.spawned = true
		s.groupID = group.ID
		s.ownerTaskID = handle.ID
		return planner.SpawnTask{
			Kind:    tasks.KindBackground,
			GroupID: group.ID,
			Spec: planner.SpawnSpec{
				Description: "selftest poll spawn",
				Query:       "self-test query",
				RetainTurn:  false,
			},
		}, nil
	}
	// Poll: non-blocking receive on the group's WatchGroup channel.
	ch, cancel, err := s.deps.Registry.WatchGroup(rc.Quadruple.Identity, s.groupID)
	if err != nil {
		return nil, err
	}
	defer cancel()
	select {
	case _, ok := <-ch:
		if !ok {
			return planner.AwaitTask{TaskID: s.ownerTaskID}, nil
		}
		s.resolved = true
		return planner.Finish{
			Reason: planner.FinishGoal,
			Metadata: map[string]any{
				"selftest_poll_resolved": true,
				"run_id":                 rc.Quadruple.RunID,
			},
		}, nil
	default:
		// Not ready yet — emit AwaitTask. The conformance pack's
		// runWakeRoundTripPoll drives the registry-side completion
		// (it fetches the spawned task via ListGroups + marks it
		// complete). The stub's role is solely the planner-side
		// emission contract: SpawnTask once, then AwaitTask while
		// the group is open, then Finish via the consume branch
		// once the WatchGroup channel fires.
		return planner.AwaitTask{TaskID: s.ownerTaskID}, nil
	}
}

// pollWakeStub implements WakeAware so ResolveWakeMode surfaces the
// declared mode.
func (s *pollWakeStub) WakeMode() planner.WakeMode {
	return planner.WakePoll
}

// selfStubPlanner is a minimal Planner that returns a configured
// Decision on every Next call. Used by the self-test to drive the
// conformance pack's scenarios without depending on a concrete
// planner package (which would create an import cycle).
type selfStubPlanner struct {
	decision planner.Decision
}

func (s *selfStubPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Identity-mandatory mirror — the conformance pack's
	// Sanity_NextReturnsDecision scenario depends on the planner
	// honouring identity in ctx + rc.
	if rc.Quadruple.TenantID == "" || rc.Quadruple.UserID == "" ||
		rc.Quadruple.SessionID == "" {
		return planner.Finish{
			Reason: planner.FinishNoPath,
			Metadata: map[string]any{
				"selftest_identity_missing": true,
			},
		}, nil
	}
	// Steering: honour Cancel observation. The steering-drain
	// scenario fires CapabilityHonoursCancelControl gated on this
	// behaviour.
	if rc.Control.Cancelled {
		return planner.Finish{Reason: planner.FinishCancelled}, nil
	}
	return s.decision, nil
}
