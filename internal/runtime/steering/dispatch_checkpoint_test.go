package steering

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

type checkpointExecutor func(context.Context, planner.RunContext, planner.Decision) (any, any, error)

func (f checkpointExecutor) ExecuteDecision(ctx context.Context, rc planner.RunContext, d planner.Decision) (any, any, error) {
	return f(ctx, rc, d)
}

type checkpointRecorder struct {
	t      *testing.T
	mu     *sync.RWMutex
	before int
	after  int
	last   planner.Step
}

func (c *checkpointRecorder) BeforeDispatch(ctx context.Context, rc planner.RunContext, step planner.Step) error {
	c.before++
	if q, ok := identity.QuadrupleFrom(ctx); !ok || q != rc.Quadruple || step.Action == nil {
		c.t.Error("intent lost execution identity or action")
	}
	return ctx.Err()
}

func (c *checkpointRecorder) AfterDispatch(ctx context.Context, rc planner.RunContext, step planner.Step) error {
	c.after++
	c.last = step
	if q, ok := identity.QuadrupleFrom(ctx); !ok || q != rc.Quadruple {
		c.t.Error("settlement lost execution identity")
	}
	if err := ctx.Err(); err != nil {
		c.t.Errorf("settlement inherited cancellation: %v", err)
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 6*time.Second {
		c.t.Error("settlement persistence has no bounded deadline")
	}
	if c.mu != nil {
		if !c.mu.TryRLock() {
			c.t.Error("persistence runs under the inspection mutex")
		} else {
			c.mu.RUnlock()
		}
	}
	return nil
}

func TestRunLoop_DispatchCheckpointCancellationAndCounterFailure(t *testing.T) {
	for _, mode := range []string{"cancellation", "counter failure"} {
		t.Run(mode, func(t *testing.T) {
			loop, _, _ := newTestRunLoop(t)
			p := &scriptedPlanner{script: []scriptStep{{dec: planner.CallTool{Tool: "save", CallID: "save-1"}}}}
			spec := runSpecFor(runA, p)
			spec.Base.Trajectory = &planner.Trajectory{}
			var mu sync.RWMutex
			spec.TrajectoryMu = &mu
			checkpoint := &checkpointRecorder{t: t, mu: &mu}
			spec.DispatchCheckpoint = checkpoint
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			spec.ToolExecutor = checkpointExecutor(func(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
				calls++
				if checkpoint.before != 1 {
					t.Error("execution preceded intent")
				}
				if mode == "cancellation" {
					cancel()
				}
				return "raw diagnostic", json.RawMessage(`{"saved":true,"version":9007199254740993}`), nil
			})
			counterErr := errors.New("counter unavailable")
			spec.OnToolDispatched = func(context.Context, int) error {
				if checkpoint.after != 1 {
					t.Error("counter preceded committed settlement")
				}
				if mode == "counter failure" {
					return counterErr
				}
				return nil
			}
			_, err := loop.Run(ctx, spec)
			wantErr := counterErr
			if mode == "cancellation" {
				wantErr = context.Canceled
			}
			if !errors.Is(err, wantErr) || p.stepCount() != 1 || calls != 1 || checkpoint.after != 1 {
				t.Fatalf("outcome lost or action repeated: err=%v decisions=%d calls=%d settlements=%d", err, p.stepCount(), calls, checkpoint.after)
			}
			if len(spec.Base.Trajectory.Steps) != 1 {
				t.Fatal("returned receipt not appended")
			}
			evidence, err := json.Marshal(checkpoint.last.LLMObservation)
			if err != nil || !strings.Contains(string(evidence), `"version":9007199254740993`) {
				t.Fatal("settlement lost exact result")
			}
		})
	}
}

func TestRunLoop_DispatchCheckpointBridgeFailureRetainsReturnedResult(t *testing.T) {
	fx := mkBridgeFixture(t)
	loop, reg, _ := newTestRunLoop(t, WithApprovalGates(map[string]*approval.ApprovalGate{"gate": fx.gate}))
	q := midStepQ("bridge-receipt")
	p := &scriptedPlanner{script: []scriptStep{{dec: planner.CallTool{Tool: "save", CallID: "save-1"}}}}
	spec := runSpecFor(q, p)
	spec.Base.Trajectory = &planner.Trajectory{}
	checkpoint := &checkpointRecorder{t: t}
	spec.DispatchCheckpoint = checkpoint
	entered := make(chan struct{})
	spec.ToolExecutor = checkpointExecutor(func(ctx context.Context, _ planner.RunContext, _ planner.Decision) (any, any, error) {
		close(entered)
		<-ctx.Done()
		// A completed external write can return an acknowledgement while its
		// caller is being cancelled for an unrelated control-bridge failure.
		return "raw", json.RawMessage(`{"saved":true,"receipt":"returned-on-unwind"}`), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), dispatchTestTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := loop.Run(ctx, spec); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("executor not entered")
	}
	if err := fx.gate.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	enqueueApprovalControl(t, reg, q, ControlApprove, "closed-gate-token")
	select {
	case err := <-done:
		if !errors.Is(err, approval.ErrGateClosed) {
			t.Fatalf("bridge failure lost: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("bridge failure did not join executor")
	}
	if checkpoint.before != 1 || checkpoint.after != 1 || p.stepCount() != 1 {
		t.Fatalf("bridge discarded a returned receipt: intents=%d settlements=%d decisions=%d", checkpoint.before, checkpoint.after, p.stepCount())
	}
	data, err := json.Marshal(checkpoint.last.LLMObservation)
	if err != nil || !strings.Contains(string(data), "returned-on-unwind") {
		t.Fatal("bridge outcome was replaced or omitted")
	}
}
