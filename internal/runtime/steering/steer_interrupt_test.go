package steering

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

func enqueueCorrection(registry *Registry, text string) error {
	inbox, err := registry.Lookup(runA)
	if err != nil {
		return err
	}
	return inbox.Enqueue(ControlEvent{Type: ControlUserMessage, Identity: runA, CallerTenant: runA.TenantID, CallerScope: ScopeSessionUser, Payload: map[string]any{"message": text}})
}

func TestRun_SteerInterruptsAttemptAndDiscardsLateDecision(t *testing.T) {
	for _, late := range []planner.Decision{planner.Finish{Reason: planner.FinishGoal}, planner.CallTool{Tool: "obsolete"}} {
		t.Run(fmt.Sprintf("%T", late), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			loop, registry, _ := newTestRunLoop(t)
			entered := make(chan struct{})
			attempts, executions, chunks, flushes := 0, 0, 0, 0
			p := interruptPlanner(func(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
				attempts++
				if attempts == 1 {
					close(entered)
					<-ctx.Done()
					rc.OnChunk("late obsolete content", true, planner.ChunkContent)
					rc.OnPendingToolCalls([]planner.ToolCallDeferred{{Name: "obsolete-tail"}})
					return late, nil
				}
				if len(rc.Control.UserMessages) != 1 || rc.Control.UserMessages[0] != "Use amber" || len(rc.PendingToolCalls) != 0 {
					return nil, errors.New("replan lost correction or retained stale calls")
				}
				if len(rc.InputArtifacts) != 1 {
					return nil, errors.New("interrupted first attempt lost attachments")
				}
				return planner.Finish{Reason: planner.FinishGoal, Metadata: map[string]any{"replanned": true}}, nil
			})
			spec := runSpecFor(runA, p)
			spec.Base.InputArtifacts = []planner.InputArtifactView{{}}
			spec.Base.OnChunk = func(string, bool, planner.ChunkKind) { chunks++ }
			spec.Base.AfterPlannerStep = func(ctx context.Context) error { flushes++; return ctx.Err() }
			spec.ToolExecutor = checkpointExecutor(func(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
				executions++
				return nil, nil, nil
			})
			done := make(chan error, 1)
			go func() {
				fin, err := loop.Run(ctx, spec)
				if err == nil && fin.Metadata["replanned"] != true {
					err = errors.New("late decision won over steering")
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("planner did not enter")
			}
			if err := enqueueCorrection(registry, "Use amber"); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if attempts != 2 || executions != 0 || chunks != 0 || flushes != 2 {
				t.Fatalf("attempts=%d dispatch=%d late chunks=%d flushes=%d", attempts, executions, chunks, flushes)
			}
		})
	}
}

type steerBeforeDispatch struct {
	checkpointRecorder
	registry *Registry
}

func (c *steerBeforeDispatch) BeforeDispatch(ctx context.Context, rc planner.RunContext, step planner.Step) error {
	if err := c.checkpointRecorder.BeforeDispatch(ctx, rc, step); err != nil {
		return err
	}
	return enqueueCorrection(c.registry, "Do not execute the old plan")
}

func TestRun_SteerDuringIntentSettlesWithoutDispatch(t *testing.T) {
	loop, registry, _ := newTestRunLoop(t)
	attempts, executions := 0, 0
	p := interruptPlanner(func(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
		attempts++
		if attempts == 1 {
			return planner.CallTool{Tool: "obsolete", CallID: "old-plan"}, nil
		}
		if len(rc.Control.UserMessages) != 1 {
			return nil, errors.New("correction missing")
		}
		return planner.Finish{Reason: planner.FinishGoal}, nil
	})
	spec := runSpecFor(runA, p)
	spec.Base.Trajectory = &planner.Trajectory{}
	checkpoint := &steerBeforeDispatch{checkpointRecorder: checkpointRecorder{t: t}, registry: registry}
	spec.DispatchCheckpoint = checkpoint
	spec.ToolExecutor = checkpointExecutor(func(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
		executions++
		return "executed", "executed", nil
	})
	if _, err := loop.Run(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if executions != 0 || checkpoint.before != 1 || checkpoint.after != 1 || checkpoint.last.Error != "superseded before dispatch; action was not executed" {
		t.Fatalf("executions=%d intent=%d settlement=%d error=%q", executions, checkpoint.before, checkpoint.after, checkpoint.last.Error)
	}
}

func TestRun_SteerConcurrentIdentityIsolation(t *testing.T) {
	const n = 128
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	loop, registry, _ := newTestRunLoop(t)
	started := make(chan identity.Quadruple, n)
	done := make(chan error, n)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p := interruptPlanner(func(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
		if len(rc.Control.UserMessages) == 0 {
			started <- rc.Quadruple
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("model interrupted: %w", ctx.Err())
			case <-release:
				return planner.Finish{Reason: planner.FinishGoal}, nil
			}
		}
		if len(rc.Control.UserMessages) != 1 || rc.Control.UserMessages[0] != rc.Quadruple.UserID {
			return nil, errors.New("correction crossed identity boundary")
		}
		return planner.Finish{Reason: planner.FinishGoal, Metadata: map[string]any{"steered": true}}, nil
	})
	for i := range n {
		q := runA
		q.UserID, q.SessionID = fmt.Sprintf("user-%d", i), fmt.Sprintf("session-%d", i)
		go func() {
			fin, err := loop.Run(ctx, runSpecFor(q, p))
			if err == nil && (fin.Metadata["steered"] == true) != (i%2 == 0) {
				err = fmt.Errorf("run %d steering outcome crossed identity", i)
			}
			done <- err
		}()
	}
	for range n {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("runs did not start")
		}
	}
	for i := 0; i < n; i += 2 {
		q := runA
		q.UserID, q.SessionID = fmt.Sprintf("user-%d", i), fmt.Sprintf("session-%d", i)
		inbox, err := registry.Lookup(q)
		if err != nil {
			t.Fatal(err)
		}
		if err := inbox.Enqueue(ControlEvent{Type: ControlUserMessage, Identity: q, CallerTenant: q.TenantID, CallerScope: ScopeSessionUser, Payload: map[string]any{"message": q.UserID}}); err != nil {
			t.Fatal(err)
		}
	}
	once.Do(func() { close(release) })
	for range n {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if registry.Len() != 0 {
		t.Fatal("run inboxes leaked")
	}
}

func TestRun_SteerCannotMaskRequiredFailure(t *testing.T) {
	for _, at := range []string{"planner", "flush"} {
		t.Run(at, func(t *testing.T) {
			loop, registry, _ := newTestRunLoop(t)
			failure := errors.New("required persistence failed")
			attempts := 0
			p := interruptPlanner(func(context.Context, planner.RunContext) (planner.Decision, error) {
				attempts++
				if err := enqueueCorrection(registry, "Use amber"); err != nil {
					return nil, err
				}
				if at == "planner" {
					return nil, errors.Join(context.Canceled, failure)
				}
				return planner.Finish{Reason: planner.FinishGoal}, nil
			})
			spec := runSpecFor(runA, p)
			if at == "flush" {
				spec.Base.AfterPlannerStep = func(context.Context) error { return failure }
			}
			if _, err := loop.Run(t.Context(), spec); !errors.Is(err, failure) || attempts != 1 {
				t.Fatalf("required failure became a retry: attempts=%d err=%v", attempts, err)
			}
		})
	}
}

func TestRun_SteerDuringChunkFlushWinsOverFinish(t *testing.T) {
	loop, registry, _ := newTestRunLoop(t)
	attempts, flushes := 0, 0
	p := interruptPlanner(func(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
		attempts++
		if attempts == 2 && (len(rc.Control.UserMessages) != 1 || rc.Control.UserMessages[0] != "Use amber") {
			return nil, errors.New("terminal-race correction lost")
		}
		return planner.Finish{Reason: planner.FinishGoal}, nil
	})
	spec := runSpecFor(runA, p)
	spec.Base.AfterPlannerStep = func(context.Context) error {
		flushes++
		if flushes == 1 {
			return enqueueCorrection(registry, "Use amber")
		}
		return nil
	}
	if _, err := loop.Run(t.Context(), spec); err != nil || attempts != 2 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestInbox_SteerAdmissionFencesPlanningAndTerminal(t *testing.T) {
	_, registry, _ := newTestRunLoop(t)
	inbox, err := registry.Open(runA)
	if err != nil {
		t.Fatal(err)
	}
	_, generation, err := inbox.drainWithGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueCorrection(registry, "Use amber"); err != nil {
		t.Fatal(err)
	}
	if inbox.beginAttempt(generation, func() {}) || inbox.admitDecision(generation, true) {
		t.Fatal("steering between drain and inference admitted the old plan")
	}
	_, generation, err = inbox.drainWithGeneration()
	if err != nil || !inbox.admitDecision(generation, true) {
		t.Fatalf("current finish refused: %v", err)
	}
	if err := enqueueCorrection(registry, "Too late"); !errors.Is(err, ErrInboxNotFound) {
		t.Fatalf("accepted steering after terminal admission: %v", err)
	}
}
