package steering

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

type interruptPlanner func(context.Context, planner.RunContext) (planner.Decision, error)

func (p interruptPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	return p(ctx, rc)
}

func TestRun_HardCancelInterruptsToolWithoutAnotherDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rl, registry, _ := newTestRunLoop(t)
	started := make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int32
	spec := runSpecFor(runA, &scriptedPlanner{defaultDec: planner.CallTool{Tool: "blocked"}})
	spec.Base.Trajectory = &planner.Trajectory{}
	spec.ToolExecutor = checkpointExecutor(func(ctx context.Context, _ planner.RunContext, _ planner.Decision) (any, any, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		return "returned outcome", "returned outcome", ctx.Err()
	})
	go func() { _, err := rl.Run(ctx, spec); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	inbox, err := registry.Lookup(runA)
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.Enqueue(ControlEvent{Type: ControlCancel, Identity: runA, CallerScope: ScopeOwnerUser, CallerTenant: runA.TenantID, Payload: map[string]any{"hard": true}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("calls=%d result=%v", calls.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("hard Stop did not interrupt blocked tool")
	}
	if len(spec.Base.Trajectory.Steps) != 1 {
		t.Fatal("cancellation lost the returned tool evidence")
	}
	obs, ok := spec.Base.Trajectory.Steps[0].Observation.(map[string]any)
	if !ok || obs["result"] != "returned outcome" || obs["error"] != context.Canceled.Error() {
		t.Fatalf("cancellation lost the result/error pair: %#v", obs)
	}
}

func TestRun_HardCancelConcurrentIdentityIsolation(t *testing.T) {
	const n = 128
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rl, registry, _ := newTestRunLoop(t)
	started := make(chan identity.Quadruple, n)
	done := make(chan error, n)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	for i := range n {
		q := runA
		q.UserID = fmt.Sprintf("user-%d", i)
		q.SessionID = fmt.Sprintf("session-%d", i)
		go func() {
			p := interruptPlanner(func(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
				started <- q
				select {
				case <-ctx.Done():
				case <-release:
				}
				return planner.Finish{Reason: planner.FinishGoal}, nil
			})
			fin, err := rl.Run(ctx, runSpecFor(q, p))
			if i%2 == 0 {
				if !errors.Is(err, context.Canceled) || fin.Reason == planner.FinishGoal {
					done <- fmt.Errorf("cancelled run %d: %+v, %w", i, fin, err)
					return
				}
			} else if err != nil || fin.Reason != planner.FinishGoal {
				done <- fmt.Errorf("sibling run %d cross-cancelled: %+v, %w", i, fin, err)
				return
			}
			done <- nil
		}()
	}
	for range n {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent runs did not start")
		}
	}
	for i := 0; i < n; i += 2 {
		q := runA
		q.UserID, q.SessionID = fmt.Sprintf("user-%d", i), fmt.Sprintf("session-%d", i)
		inbox, err := registry.Lookup(q)
		if err != nil {
			t.Fatal(err)
		}
		if err := inbox.Enqueue(ControlEvent{Type: ControlCancel, Identity: q, CallerScope: ScopeOwnerUser, CallerTenant: q.TenantID, Payload: map[string]any{"hard": true}}); err != nil {
			t.Fatal(err)
		}
	}
	releaseOnce.Do(func() { close(release) })
	for range n {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent execution did not join")
		}
	}
	if registry.Len() != 0 {
		t.Fatal("run inboxes leaked")
	}
}

func TestRun_HardCancelInterruptsPlannerAndRejectsLateFinish(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rl, registry, _ := newTestRunLoop(t)
	started := make(chan struct{})
	result := make(chan error, 1)
	p := interruptPlanner(func(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
		close(started)
		<-ctx.Done()
		// A provider/adapter that returns success while cancellation unwinds
		// cannot resurrect the cancelled run or dispatch a late decision.
		return planner.Finish{Reason: planner.FinishGoal}, nil
	})
	spec := runSpecFor(runA, p)
	sealed := make(chan error, 1)
	spec.Base.SealCompletionChunks = func(ctx context.Context) error {
		sealed <- ctx.Err()
		return ctx.Err()
	}
	go func() {
		fin, err := rl.Run(ctx, spec)
		if fin.Reason == planner.FinishGoal {
			result <- errors.New("late finish resurrected cancelled run")
			return
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("planner did not start")
	}
	inbox, err := registry.Lookup(runA)
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.Enqueue(ControlEvent{Type: ControlCancel, Identity: runA, CallerScope: ScopeOwnerUser, CallerTenant: runA.TenantID, Payload: map[string]any{"hard": true}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("hard cancellation result = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("hard Stop waited for a planner step boundary")
	}
	if err := <-sealed; err != nil {
		t.Fatalf("cancellation cancelled its own terminal bookkeeping: %v", err)
	}
}

type cancelBeforeDispatchCheckpoint struct {
	checkpointRecorder
	registry *Registry
}

func (c *cancelBeforeDispatchCheckpoint) BeforeDispatch(ctx context.Context, rc planner.RunContext, step planner.Step) error {
	if err := c.checkpointRecorder.BeforeDispatch(ctx, rc, step); err != nil {
		return err
	}
	inbox, err := c.registry.Lookup(rc.Quadruple)
	if err != nil {
		return err
	}
	return inbox.Enqueue(ControlEvent{Type: ControlCancel, Identity: rc.Quadruple, CallerScope: ScopeOwnerUser, CallerTenant: rc.Quadruple.TenantID, Payload: map[string]any{"hard": true}})
}

func TestRun_HardCancelDuringIntentPreventsDispatch(t *testing.T) {
	rl, registry, _ := newTestRunLoop(t)
	spec := runSpecFor(runA, &scriptedPlanner{defaultDec: planner.CallTool{Tool: "must-not-run", CallID: "planned"}})
	spec.Base.Trajectory = &planner.Trajectory{}
	checkpoint := &cancelBeforeDispatchCheckpoint{checkpointRecorder: checkpointRecorder{t: t}, registry: registry}
	spec.DispatchCheckpoint = checkpoint
	var calls int
	spec.ToolExecutor = checkpointExecutor(func(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
		calls++
		return nil, nil, nil
	})
	fin, err := rl.Run(t.Context(), spec)
	if !errors.Is(err, context.Canceled) || fin.Reason != planner.FinishCancelled || calls != 0 {
		t.Fatalf("cancelled intent dispatched: finish=%v err=%v calls=%d", fin, err, calls)
	}
	if checkpoint.before != 1 || checkpoint.after != 1 || len(spec.Base.Trajectory.Steps) != 1 {
		t.Fatal("cancelled intent was not settled with its failed tool pair")
	}
}

func TestRun_HardCancelCleanupFailureAndNoTerminalDispatch(t *testing.T) {
	rl, registry, _ := newTestRunLoop(t)
	sealErr := errors.New("fixture seal unavailable")
	spec := runSpecFor(runA, interruptPlanner(func(context.Context, planner.RunContext) (planner.Decision, error) {
		inbox, err := registry.Lookup(runA)
		if err != nil {
			return nil, err
		}
		err = inbox.Enqueue(ControlEvent{Type: ControlCancel, Identity: runA, CallerScope: ScopeOwnerUser, CallerTenant: runA.TenantID, Payload: map[string]any{"hard": true}})
		return planner.Finish{Reason: planner.FinishGoal}, err
	}))
	var sealed bool
	spec.Base.SealCompletionChunks = func(ctx context.Context) error {
		sealed = true
		q, ok := identity.QuadrupleFrom(ctx)
		deadline, bounded := ctx.Deadline()
		if ctx.Err() != nil || !ok || q != runA || !bounded || time.Until(deadline) > 5*time.Second {
			t.Error("cleanup lost live bounded identity context")
		}
		return sealErr
	}
	exec := &recordingHookExecutor{}
	spec.ToolExecutor = exec
	spec.CompletionHook = &CompletionHookSpec{Tool: hookTool}
	titler := newFakeTitler()
	spec.Naming = &NamingSpec{Policy: activePolicy(1, 0, 0), Titler: titler}
	fin, err := rl.Run(t.Context(), spec)
	if fin.Reason != planner.FinishCancelled || !errors.Is(err, context.Canceled) || !errors.Is(err, sealErr) || !sealed {
		t.Fatalf("cancellation/cleanup failure lost: finish=%v err=%v sealed=%v", fin, err, sealed)
	}
	if len(exec.hookCalls()) != 0 || titler.get(runA.SessionID).TurnCount != 0 {
		t.Fatal("hard Stop launched a completion hook or naming")
	}
}

func TestRun_CompletedDecisionRejectsLateHardCancel(t *testing.T) {
	rl, registry, _ := newTestRunLoop(t)
	spec := runSpecFor(runA, &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal}})
	spec.Base.SealCompletionChunks = func(ctx context.Context) error {
		inbox, err := registry.Lookup(runA)
		if err != nil {
			return err
		}
		err = inbox.Enqueue(ControlEvent{Type: ControlCancel, Identity: runA, CallerScope: ScopeOwnerUser, CallerTenant: runA.TenantID, Payload: map[string]any{"hard": true}})
		if !errors.Is(err, ErrInboxNotFound) || ctx.Err() != nil {
			t.Fatalf("terminal execution admitted late Stop: err=%v context=%v", err, ctx.Err())
		}
		return nil
	}
	fin, err := rl.Run(t.Context(), spec)
	if err != nil || fin.Reason != planner.FinishGoal {
		t.Fatalf("late Stop rewrote completed result: finish=%v err=%v", fin, err)
	}
}
