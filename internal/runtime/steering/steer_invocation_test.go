package steering

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

func TestRun_SteerFencesQueuedInvocationWithoutCancellingActiveTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	loop, registry, _ := newTestRunLoop(t)
	cat := tools.NewCatalog()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var activeCalls, queuedCalls atomic.Int32
	for _, name := range []string{"active", "queued"} {
		d := tools.ToolDescriptor{Tool: tools.Tool{Name: name}, Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			invoke := func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				if name == "active" {
					activeCalls.Add(1)
					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return tools.ToolResult{}, ctx.Err()
					}
					return tools.ToolResult{Value: "completed receipt"}, ctx.Err()
				}
				queuedCalls.Add(1)
				return tools.ToolResult{}, nil
			}
			if name == "queued" {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return tools.ToolResult{}, ctx.Err()
				}
			}
			return tools.RunWithPolicy(ctx, args, invoke, nil, nil, tools.ToolPolicy{RetryOn: []tools.ErrorClass{}})
		}}
		if err := cat.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	attempts := 0
	spec := runSpecFor(runA, interruptPlanner(func(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
		attempts++
		if attempts == 1 {
			return planner.CallParallel{Branches: []planner.CallTool{{Tool: "active"}, {Tool: "queued"}}}, nil
		}
		if len(rc.Control.UserMessages) != 1 {
			return nil, errors.New("correction missing after queued call was refused")
		}
		return planner.Finish{Reason: planner.FinishGoal}, nil
	}))
	spec.ToolExecutor = checkpointExecutor(func(ctx context.Context, _ planner.RunContext, d planner.Decision) (any, any, error) {
		results, err := parallel.New(cat).Execute(ctx, d.(planner.CallParallel))
		if err == nil && (len(results) != 2 || results[0].Err != nil || results[0].Result.Value != "completed receipt" || results[1].Err == nil) {
			err = errors.New("active receipt lost or queued invocation was not refused")
			t.Error(err)
		}
		return results, results, err
	})
	done := make(chan error, 1)
	go func() { _, err := loop.Run(ctx, spec); done <- err }()
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("branches did not enter")
		}
	}
	if err := enqueueCorrection(registry, "Use the revised plan"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if activeCalls.Load() != 1 || queuedCalls.Load() != 0 || attempts != 2 {
		t.Fatalf("active=%d queued=%d attempts=%d", activeCalls.Load(), queuedCalls.Load(), attempts)
	}
}

func TestRun_SteerWithdrawsPendingApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	fx := mkBridgeFixture(t)
	registry := NewRegistry()
	loop, err := NewRunLoop(registry, fx.coord)
	if err != nil {
		t.Fatal(err)
	}
	sub, closeSub := subscribeForApprovalRequested(t, fx.bus, runA.Identity)
	defer closeSub()
	calls, attempts := 0, 0
	desc := catalog.WrapWithApproval(tools.ToolDescriptor{Tool: tools.Tool{Name: "guarded"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		calls++
		return tools.ToolResult{}, nil
	}}, fx.gate, catalog.ApprovalWrapperOptions{})
	spec := runSpecFor(runA, interruptPlanner(func(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
		attempts++
		if attempts == 1 {
			return planner.CallTool{Tool: "guarded"}, nil
		}
		if len(rc.Control.UserMessages) != 1 {
			return nil, errors.New("correction lost after approval withdrawal")
		}
		return planner.Finish{Reason: planner.FinishGoal}, nil
	}))
	spec.ToolExecutor = checkpointExecutor(func(ctx context.Context, _ planner.RunContext, _ planner.Decision) (any, any, error) {
		r, err := desc.Invoke(ctx, json.RawMessage(`{}`))
		return r.Value, r.Value, err
	})
	done := make(chan error, 1)
	go func() { _, err := loop.Run(ctx, spec); done <- err }()
	token := waitForApprovalRequested(t, sub)
	if err := enqueueCorrection(registry, "Do not execute that action"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	statusCtx, err := identity.With(t.Context(), runA.Identity)
	if err != nil {
		t.Fatal(err)
	}
	status, err := fx.coord.Status(statusCtx, token)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject {
		t.Errorf("obsolete approval remains live: %+v", status)
	}
	if calls != 0 || attempts != 2 {
		t.Fatalf("calls=%d attempts=%d", calls, attempts)
	}
}

func TestRun_InvocationCleanupFailureSettlesThenStops(t *testing.T) {
	loop, _, _ := newTestRunLoop(t)
	checkpoint := &checkpointRecorder{t: t}
	requests := 0
	spec := runSpecFor(runA, interruptPlanner(func(context.Context, planner.RunContext) (planner.Decision, error) {
		requests++
		return planner.CallTool{Tool: "guarded"}, nil
	}))
	spec.Base.Trajectory = &planner.Trajectory{}
	spec.DispatchCheckpoint = checkpoint
	failure := errors.Join(tools.ErrInvocationSuperseded, tools.ErrInvocationCleanupFailed)
	spec.ToolExecutor = checkpointExecutor(func(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
		return "retained receipt", "retained receipt", failure
	})
	_, err := loop.Run(t.Context(), spec)
	if !errors.Is(err, tools.ErrInvocationCleanupFailed) || requests != 1 || checkpoint.after != 1 {
		t.Fatalf("requests=%d settlement=%d err=%v", requests, checkpoint.after, err)
	}
}
