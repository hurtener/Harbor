// Wave 9 cross-subsystem integration test per CLAUDE.md §17.5 + §17.7
// step 5 — the wave-end E2E, bundled with the final phase (Phase 54).
//
// Wave 9 closes the steering / pause-resume / Protocol-edge cluster:
//
//   - Phase 50 Pause/Resume Coordinator + handle registry.
//   - Phase 51 Pause-state serialise contract (fail-loud).
//   - Phase 52 Steering inbox + the nine-type control taxonomy.
//   - Phase 53 Steering wiring — the RunLoop per-run planner-step loop.
//   - Phase 53a Agent Registry — registration identity + the three-ID
//     model.
//   - Phase 54 Protocol task control surface — the ten canonical
//     task-control methods + the transport-agnostic ControlSurface.
//
// The wave-end E2E proves these COMPOSE: a Protocol client drives a run
// entirely through the Phase 54 ControlSurface — `start` spawns the
// task, the run's planner loop (Phase 53 RunLoop) reaches a HITL pause
// (routed through the Phase 50 Coordinator over a real Phase 07
// StateStore checkpoint store), an `inject_context` control submitted
// via the surface lands on the Phase 52 inbox, and an `approve` control
// submitted via the surface advances the pause through the Coordinator
// so the planner re-enters and finishes. The Phase 53a Agent Registry is
// wired on the same runtime so the registration-identity seam is part of
// the composed surface.
//
// Per CLAUDE.md §17.3:
//
//  1. Real drivers everywhere on the seam. No mocks at the boundary —
//     real tasks.TaskRegistry (inprocess over a real in-mem
//     state.StateStore), real events.EventBus (inmem), real patterns
//     audit redactor, real pauseresume.Coordinator (over the real
//     StateStore checkpoint store), real registry.AgentRegistry, real
//     steering.Registry + steering.RunLoop, real protocol.ControlSurface.
//  2. Identity propagation: the identity quadruple flows from the
//     Protocol IdentityScope through every layer — the spawned task, the
//     steering inbox, the pause record, the agent registration.
//  3. ≥1 failure mode: a missing-identity Protocol request, a
//     scope-mismatch steering submission, an oversize control payload —
//     each fails closed at the surface edge with the right error code.
//  4. -race is the CI gate.
//  5. N≥10 concurrency stress: concurrent runs each driven through the
//     ControlSurface, no cross-talk, goroutine baseline restored on
//     teardown.
//  6. No time.Sleep-as-synchronisation for the load-bearing waits —
//     bounded eventually-style polling with channel observations
//     (§17.4 + §11). The short settle sleeps before goroutine-baseline
//     snapshots are scheduler-noise tolerances, not synchronisation.
package integration_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// wave9Deps bundles the full Wave 9 runtime surface, all real drivers.
type wave9Deps struct {
	surface  *protocol.ControlSurface
	steering *steering.Registry
	runLoop  *steering.RunLoop
	coord    pauseresume.Coordinator
	agents   *registry.Registry
	tasks    tasks.TaskRegistry
	bus      events.EventBus
	state    state.StateStore
	cleanup  func()
}

func newWave9Deps(t *testing.T) *wave9Deps {
	t.Helper()

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1000,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	// The Coordinator over the REAL in-mem state.StateStore checkpoint
	// store — the §13 pause round-trip rides a durable checkpoint.
	coord := pauseresume.New(
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithBus(bus),
	)
	steerReg := steering.NewRegistry()
	runLoop, err := steering.NewRunLoop(steerReg, coord,
		steering.WithRunLoopBus(bus),
		steering.WithTaskRegistry(taskReg),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	agents, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: red})
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("registry.New: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	return &wave9Deps{
		surface:  surface,
		steering: steerReg,
		runLoop:  runLoop,
		coord:    coord,
		agents:   agents,
		tasks:    taskReg,
		bus:      bus,
		state:    store,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// wave9Run builds a documented dummy run quadruple — no secrets.
func wave9Run(tenant, suffix string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  tenant,
			UserID:    "user-w9",
			SessionID: "session-w9-" + suffix,
		},
		RunID: "run-w9-" + suffix,
	}
}

// wave9Ctx builds a ctx carrying the run's identity under BOTH the
// triple key and the quadruple key — the RunLoop / Coordinator pathway.
func wave9Ctx(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// wave9PauseGate fires its PauseStep exactly once per run.
type wave9PauseGate struct {
	fired sync.Map // runID -> bool
}

func (g *wave9PauseGate) claimOnce(rc planner.RunContext) bool {
	key := rc.Quadruple.RunID
	if _, done := g.fired.Load(key); done {
		return false
	}
	g.fired.Store(key, true)
	return true
}

// wave9HITLPlanner builds a real deterministic planner whose step set is
// [PauseStep (fires once, HITL-approval), FinishStep]. The PauseStep
// emits planner.RequestPause — the §13 emitting consumer the RunLoop
// routes through the unified Coordinator.
func wave9HITLPlanner(t *testing.T, gate *wave9PauseGate) planner.Planner {
	t.Helper()
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(
			&deterministic.PauseStep{
				Reason: planner.PauseApprovalRequired,
				When:   gate.claimOnce,
				PayloadBuilder: func(planner.RunContext) (map[string]any, error) {
					return map[string]any{"gate": "hitl-approval"}, nil
				},
			},
			&deterministic.FinishStep{
				Reason: planner.FinishGoal,
				PayloadBuilder: func(planner.RunContext) (any, error) {
					return "resumed via Protocol approve and finished", nil
				},
			},
		),
	)
	if err != nil {
		t.Fatalf("NewDeterministicPlanner: %v", err)
	}
	return p
}

// eventually polls fn until it returns true or the deadline elapses.
// Bounded real-time wait — not a time.Sleep-as-synchronisation (§17.4).
func eventually(t *testing.T, within time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fn()
}

// codeOfWave9 extracts the stable Protocol error Code, failing the test
// if err is not a *protoerrors.Error.
func codeOfWave9(t *testing.T, err error) protoerrors.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected a *protocol/errors.Error, got nil")
	}
	var pe *protoerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected a *protocol/errors.Error, got %T: %v", err, err)
	}
	return pe.Code
}

// ---------------------------------------------------------------------------
// TestE2E_Wave9_ProtocolDrivenRun_AssembledSurface — the load-bearing
// wave-end shape: a Protocol client drives a HITL-gated run end to end
// through the Phase 54 ControlSurface, across the full Wave 9 runtime.
// ---------------------------------------------------------------------------

func TestE2E_Wave9_ProtocolDrivenRun_AssembledSurface(t *testing.T) {
	deps := newWave9Deps(t)
	defer deps.cleanup()

	q := wave9Run("tenant-w9", "protocol-driven")

	// (1) Register an agent through the Phase 53a Agent Registry — the
	// registration-identity seam is part of the composed Wave 9 surface.
	agentCtx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	rec, err := deps.agents.Register(agentCtx, "wave9-agent", registry.AgentConfig{
		Prompts:       []string{"you are a wave-9 test agent"},
		PlannerConfig: map[string]string{"kind": "deterministic"},
	}, registry.RegisterOptions{DisplayName: "Wave 9 Agent"})
	if err != nil {
		t.Fatalf("agents.Register: %v", err)
	}
	if rec.AgentID == "" {
		t.Fatal("agents.Register returned an empty AgentID")
	}

	// (2) `start` through the Protocol ControlSurface — spawns the task.
	startResp, err := deps.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
		Identity: types.IdentityScope{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID},
		Query:    "do a HITL-gated thing",
	})
	if err != nil {
		t.Fatalf("Dispatch(start): %v", err)
	}
	sr := startResp.(*types.StartResponse)
	if sr.TaskID == "" {
		t.Fatal("Dispatch(start) returned an empty TaskID")
	}
	// Identity propagation: the spawned task carries the run's triple.
	gotTask, err := deps.tasks.Get(agentCtx, tasks.TaskID(sr.TaskID))
	if err != nil {
		t.Fatalf("tasks.Get(%q): %v", sr.TaskID, err)
	}
	if gotTask.Identity.TenantID != q.TenantID || gotTask.Identity.SessionID != q.SessionID {
		t.Fatalf("spawned task identity = %+v, want triple from %+v", gotTask.Identity, q.Identity)
	}

	// (3) Drive the run's planner loop on a goroutine. The RunLoop Opens
	// the run's steering inbox, drives the planner to the PauseStep
	// (RequestPause → Coordinator.Request → durable checkpoint → block
	// in WaitForEvent).
	gate := &wave9PauseGate{}
	p := wave9HITLPlanner(t, gate)
	type runResult struct {
		fin planner.Finish
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		fin, rerr := deps.runLoop.Run(wave9Ctx(t, q), steering.RunSpec{
			Planner:  p,
			Base:     planner.RunContext{Quadruple: q, Goal: "do a HITL-gated thing"},
			TaskID:   tasks.TaskID(sr.TaskID),
			MaxSteps: 16,
		})
		done <- runResult{fin, rerr}
	}()

	// (4) Wait until the RunLoop has Opened the inbox AND reached the
	// pause boundary (the gate fired). Bounded eventually-style wait.
	reachedPause := eventually(t, 3*time.Second, func() bool {
		if _, lerr := deps.steering.Lookup(q); lerr != nil {
			return false
		}
		_, fired := gate.fired.Load(q.RunID)
		return fired
	})
	if !reachedPause {
		t.Fatal("the run never reached the HITL pause boundary — RunLoop did not compose with the planner")
	}

	// (5) `inject_context` through the ControlSurface — lands on the
	// Phase 52 inbox while the run is paused. This proves a Protocol
	// control reaches the live run's inbox mid-pause.
	injResp, err := deps.surface.Dispatch(context.Background(), methods.MethodInjectContext, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID,
			Scope: string(steering.ScopeSessionUser),
		},
		Payload: map[string]any{"note": "operator context injected via Protocol"},
	})
	if err != nil {
		t.Fatalf("Dispatch(inject_context): %v", err)
	}
	if cr := injResp.(*types.ControlResponse); !cr.Accepted {
		t.Fatal("Dispatch(inject_context): not accepted")
	}

	// (6) `approve` through the ControlSurface — advances the pause
	// through the unified Coordinator; the planner re-enters and
	// finishes. A clean Finish IS the proof the whole chain composed:
	// Protocol approve → steering inbox → RunLoop drain → applyEvent →
	// Coordinator.Resume → planner re-enters.
	apprResp, err := deps.surface.Dispatch(context.Background(), methods.MethodApprove, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID,
			Scope: string(steering.ScopeOwnerUser),
		},
		Payload: map[string]any{"approved_by": "operator"},
	})
	if err != nil {
		t.Fatalf("Dispatch(approve): %v", err)
	}
	if cr := apprResp.(*types.ControlResponse); !cr.Accepted {
		t.Fatal("Dispatch(approve): not accepted")
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("RunLoop.Run after Protocol approve: %v", res.err)
		}
		if res.fin.Reason != planner.FinishGoal {
			t.Fatalf("Finish.Reason = %q, want %q — the planner did not re-enter after the Protocol-driven approve", res.fin.Reason, planner.FinishGoal)
		}
		// Identity propagation: the Finish metadata carries the run id.
		if rid, _ := res.fin.Metadata["run_id"].(string); rid != q.RunID {
			t.Fatalf("Finish.Metadata[run_id] = %q, want %q", rid, q.RunID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunLoop.Run did not finish within 3s after the Protocol approve — the Wave 9 surface did not compose end-to-end")
	}
}

// ---------------------------------------------------------------------------
// TestE2E_Wave9_FailureModes_FailClosedAtTheEdge — the §17.3 #3
// failure-mode coverage: every malformed Protocol request fails closed
// at the ControlSurface edge with the right stable error code, and the
// runtime is never reached.
// ---------------------------------------------------------------------------

func TestE2E_Wave9_FailureModes_FailClosedAtTheEdge(t *testing.T) {
	deps := newWave9Deps(t)
	defer deps.cleanup()

	// (a) Missing identity — RFC §5.5: the Protocol rejects any request
	// without an identity scope. A `start` with no tenant fails closed.
	_, err := deps.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
		Identity: types.IdentityScope{User: "u", Session: "s"},
	})
	if got := codeOfWave9(t, err); got != protoerrors.CodeIdentityRequired {
		t.Fatalf("missing-identity start: code = %q, want %q", got, protoerrors.CodeIdentityRequired)
	}

	// (b) Scope mismatch — PRIORITIZE requires admin (RFC §6.3); a
	// session_user caller is below the minimum. The run's inbox must
	// exist for the scope check to be reached.
	q := wave9Run("tenant-w9", "scope-fail")
	if _, oerr := deps.steering.Open(q); oerr != nil {
		t.Fatalf("steering.Open: %v", oerr)
	}
	_, err = deps.surface.Dispatch(context.Background(), methods.MethodPrioritize, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID,
			Scope: string(steering.ScopeSessionUser),
		},
		Payload: map[string]any{"priority": 9},
	})
	if got := codeOfWave9(t, err); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("below-min-scope prioritize: code = %q, want %q", got, protoerrors.CodeScopeMismatch)
	}

	// (c) Oversize control payload — the RFC §6.3 4096-rune string cap
	// is enforced at the edge by Phase 52's ValidatePayload; the surface
	// maps the rejection to CodePayloadInvalid.
	huge := make([]byte, 5000)
	for i := range huge {
		huge[i] = 'x'
	}
	_, err = deps.surface.Dispatch(context.Background(), methods.MethodInjectContext, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID,
			Scope: string(steering.ScopeSessionUser),
		},
		Payload: map[string]any{"note": string(huge)},
	})
	if got := codeOfWave9(t, err); got != protoerrors.CodePayloadInvalid {
		t.Fatalf("oversize-payload inject_context: code = %q, want %q", got, protoerrors.CodePayloadInvalid)
	}

	// (d) A steering control for a run with no live inbox fails closed
	// with CodeNotFound — the run never started.
	_, err = deps.surface.Dispatch(context.Background(), methods.MethodCancel, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: "tenant-w9", User: "user-w9", Session: "session-ghost", Run: "run-ghost",
			Scope: string(steering.ScopeOwnerUser),
		},
	})
	if got := codeOfWave9(t, err); got != protoerrors.CodeNotFound {
		t.Fatalf("no-inbox cancel: code = %q, want %q", got, protoerrors.CodeNotFound)
	}

	// (e) An unknown method fails closed with CodeUnknownMethod.
	_, err = deps.surface.Dispatch(context.Background(), methods.Method("teleport"), &types.StartRequest{})
	if got := codeOfWave9(t, err); got != protoerrors.CodeUnknownMethod {
		t.Fatalf("unknown method: code = %q, want %q", got, protoerrors.CodeUnknownMethod)
	}
}

// ---------------------------------------------------------------------------
// TestE2E_Wave9_Concurrency_NoCrossTalk — the §17.3 concurrency stress:
// N≥10 concurrent runs, each driven through the shared ControlSurface +
// shared RunLoop + shared Coordinator + shared steering Registry. Each
// run is HITL-gated and resumed via a Protocol approve; identity
// isolation holds (no cross-talk), goroutine baseline restored on
// teardown (no leak).
// ---------------------------------------------------------------------------

func TestE2E_Wave9_Concurrency_NoCrossTalk(t *testing.T) {
	const n = 16 // ≥10 per §17.3

	deps := newWave9Deps(t)
	defer deps.cleanup()

	// Settle before snapshotting the goroutine baseline.
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	gate := &wave9PauseGate{} // one shared gate, keyed per run id
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Distinct per-goroutine identity — a context bleed would
			// surface as a foreign triple on the spawned task, the
			// inbox event, or the Finish metadata.
			q := wave9Run(fmt.Sprintf("tenant-%d", i), fmt.Sprintf("conc-%d", i))

			// `start` via the shared surface.
			startResp, err := deps.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
				Identity: types.IdentityScope{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID},
				Query:    fmt.Sprintf("concurrent run %d", i),
			})
			if err != nil {
				errs <- fmt.Errorf("run %d start: %w", i, err)
				return
			}
			taskID := tasks.TaskID(startResp.(*types.StartResponse).TaskID)

			// Drive the planner loop via the shared RunLoop.
			p := wave9HITLPlanner(t, gate)
			runDone := make(chan error, 1)
			go func() {
				_, rerr := deps.runLoop.Run(wave9Ctx(t, q), steering.RunSpec{
					Planner:  p,
					Base:     planner.RunContext{Quadruple: q, Goal: "concurrent HITL run"},
					TaskID:   taskID,
					MaxSteps: 16,
				})
				runDone <- rerr
			}()

			// Wait for the pause boundary.
			reached := eventually(t, 5*time.Second, func() bool {
				if _, lerr := deps.steering.Lookup(q); lerr != nil {
					return false
				}
				_, fired := gate.fired.Load(q.RunID)
				return fired
			})
			if !reached {
				errs <- fmt.Errorf("run %d never reached the pause boundary", i)
				return
			}

			// `approve` via the shared surface — resumes this run only.
			if _, err := deps.surface.Dispatch(context.Background(), methods.MethodApprove, &types.ControlRequest{
				Identity: types.IdentityScope{
					Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID,
					Scope: string(steering.ScopeOwnerUser),
				},
			}); err != nil {
				errs <- fmt.Errorf("run %d approve: %w", i, err)
				return
			}

			select {
			case rerr := <-runDone:
				if rerr != nil {
					errs <- fmt.Errorf("run %d RunLoop: %w", i, rerr)
					return
				}
			case <-time.After(5 * time.Second):
				errs <- fmt.Errorf("run %d did not finish after approve", i)
				return
			}

			// Identity isolation: the spawned task carries this run's
			// own tenant, not a neighbour's.
			tctx, err := identity.With(context.Background(), q.Identity)
			if err != nil {
				errs <- fmt.Errorf("run %d identity.With: %w", i, err)
				return
			}
			got, err := deps.tasks.Get(tctx, taskID)
			if err != nil {
				errs <- fmt.Errorf("run %d tasks.Get: %w", i, err)
				return
			}
			if got.Identity.TenantID != q.TenantID {
				errs <- fmt.Errorf("run %d task tenant = %q, want %q — cross-talk", i, got.Identity.TenantID, q.TenantID)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// No goroutine leak: every run's loop goroutine + the Coordinator's
	// per-pause state are released once Run returns. Small slack for
	// scheduler noise.
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+10 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}
