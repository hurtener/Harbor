package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// TestConcurrentReuse_ControlSurface pins the D-025 concurrent-reuse
// contract: N≥100 goroutines run Dispatch against ONE shared
// ControlSurface under -race. Each goroutine drives a distinct identity
// quadruple, so a context bleed surfaces as a foreign triple on a
// drained event or a wrong-tenant task. The test asserts:
//
//   - no data races (the -race detector is the gate);
//   - no context bleed — each run's start spawns a task under its own
//     identity, and each run's control lands on its own inbox only;
//   - no goroutine leak — baseline runtime.NumGoroutine restored after
//     all Dispatch calls return.
//
// ControlSurface is a compiled artifact: every field is set once at
// NewControlSurface; Dispatch reads run-specific data from ctx + the
// request argument, never from the surface struct.
func TestConcurrentReuse_ControlSurface(t *testing.T) {
	const n = 150 // ≥100 per the D-025 contract

	fx := newSurfaceFixture(t)

	// Pre-open a steering inbox per run so the control half of each
	// goroutine has a live target. Open is the run-lifecycle entry; the
	// Protocol surface does not own inbox lifecycle.
	runs := make([]identity.Quadruple, n)
	for i := range n {
		runs[i] = identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i),
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("session-%d", i),
			},
			RunID: fmt.Sprintf("run-%d", i),
		}
		if _, err := fx.steering.Open(runs[i]); err != nil {
			t.Fatalf("steering.Open(run-%d): %v", i, err)
		}
	}

	// Let the runtime settle before snapshotting the goroutine baseline.
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n*2)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run := runs[i]

			// (1) start — spawns a task under this goroutine's identity.
			startResp, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
				Identity: types.IdentityScope{
					Tenant: run.TenantID, User: run.UserID, Session: run.SessionID,
				},
				Query: fmt.Sprintf("query-%d", i),
			})
			if err != nil {
				errs <- fmt.Errorf("run-%d start: %w", i, err)
				return
			}
			sr := startResp.(*types.StartResponse)
			if sr.TaskID == "" {
				errs <- fmt.Errorf("run-%d start: empty TaskID", i)
				return
			}

			// (2) a steering control — lands on this run's own inbox.
			ctrlResp, err := fx.surface.Dispatch(context.Background(), methods.MethodInjectContext, &types.ControlRequest{
				Identity: types.IdentityScope{
					Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
					Scope: "session_user",
				},
				Payload: map[string]any{"goroutine": i},
			})
			if err != nil {
				errs <- fmt.Errorf("run-%d inject_context: %w", i, err)
				return
			}
			if cr := ctrlResp.(*types.ControlResponse); !cr.Accepted {
				errs <- fmt.Errorf("run-%d inject_context: not accepted", i)
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

	// No context bleed: every run's inbox holds exactly its own one
	// injected-context event, with its own identity and its own
	// goroutine index in the payload.
	for i, run := range runs {
		inbox, err := fx.steering.Lookup(run)
		if err != nil {
			t.Fatalf("steering.Lookup(run-%d): %v", i, err)
		}
		drained, err := inbox.Drain()
		if err != nil {
			t.Fatalf("inbox.Drain(run-%d): %v", i, err)
		}
		if len(drained) != 1 {
			t.Fatalf("run-%d inbox drained %d events, want 1 — context bleed across runs", i, len(drained))
		}
		ev := drained[0]
		if ev.Identity != run {
			t.Fatalf("run-%d drained event identity = %+v, want %+v — context bleed", i, ev.Identity, run)
		}
		gi, ok := ev.Payload["goroutine"]
		if !ok {
			t.Fatalf("run-%d drained event missing goroutine marker", i)
		}
		if giInt, ok := gi.(int); !ok || giInt != i {
			t.Fatalf("run-%d drained event goroutine marker = %v, want %d — payload bleed", i, gi, i)
		}
	}

	// No goroutine leak: the baseline is restored once every Dispatch
	// has returned. A small slack tolerates the test harness's own
	// scheduler noise.
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+5 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}

// TestConcurrentReuse_ControlSurface_Topology pins the D-025
// concurrent-reuse contract for the Phase 74 `topology.snapshot`
// dispatch path: N≥100 goroutines call Dispatch(topology.snapshot)
// against ONE shared ControlSurface (with a wired topology accessor)
// under -race. Each goroutine drives a distinct identity triple; the
// test asserts no data races, no projection drift across calls, and no
// goroutine leak.
func TestConcurrentReuse_ControlSurface_Topology(t *testing.T) {
	const n = 128 // ≥100 per the D-025 contract

	accessor := &fakeTopologyAccessor{tenant: "shared-tenant", proj: sampleProjection()}
	surface := newTopologySurface(t, accessor)

	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := surface.Dispatch(context.Background(), methods.MethodTopologySnapshot,
				&types.TopologySnapshotRequest{
					Identity: types.IdentityScope{
						Tenant:  "shared-tenant",
						User:    fmt.Sprintf("user-%d", i),
						Session: fmt.Sprintf("session-%d", i),
					},
				})
			if err != nil {
				errs <- fmt.Errorf("goroutine-%d: %w", i, err)
				return
			}
			proj, ok := resp.(*types.TopologyProjection)
			if !ok {
				errs <- fmt.Errorf("goroutine-%d: response type %T", i, resp)
				return
			}
			if proj.EngineID != "engine-test" || len(proj.Nodes) != 2 || len(proj.Edges) != 1 {
				errs <- fmt.Errorf("goroutine-%d: projection drift / field tearing", i)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+5 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}
