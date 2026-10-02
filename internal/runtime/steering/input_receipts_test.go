package steering

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksinprocess "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// This focused runtime integration uses the real task engine and StateStore;
// the scripted planner provides deterministic interruption/consumption barriers.
func inputRunFixture(t *testing.T) (*RunLoop, *Registry, tasks.TaskRegistry, identity.Quadruple, context.Context) {
	t.Helper()
	red := auditpatterns.New()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 256, IdleTimeout: time.Minute, DropWindow: time.Second}, red)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := tasksinprocess.New(tasks.Dependencies{Store: st, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tr.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	ctx, err := identity.With(t.Context(), runA.Identity)
	if err != nil {
		t.Fatal(err)
	}
	h, err := tr.Spawn(ctx, tasks.SpawnRequest{Identity: runA, Kind: tasks.KindForeground, Query: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	q := runA
	q.RunID = string(h.ID)
	loop, registry, _ := newTestRunLoop(t, WithTaskRegistry(tr))
	return loop, registry, tr, q, ctx
}
func inputEvent(q identity.Quadruple, id, text string) ControlEvent {
	return ControlEvent{Type: ControlUserMessage, Identity: q, CallerTenant: q.TenantID, CallerScope: ScopeOwnerUser, EventID: id, Payload: map[string]any{"message": text}}
}

func TestRun_InputReceipts_LostACKConcurrentReplayAndSealedRevision(t *testing.T) {
	loop, registry, tr, q, ctx := inputRunFixture(t)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	first, second, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	calls := 0
	p := interruptPlanner(func(attempt context.Context, rc planner.RunContext) (planner.Decision, error) {
		calls++
		if calls == 1 {
			close(first)
			<-attempt.Done()
			return planner.Finish{Reason: planner.FinishGoal, Payload: "obsolete"}, nil
		}
		if len(rc.Control.UserMessages) != 1 || rc.Control.UserMessages[0] != "revised" {
			return nil, errors.New("input did not arrive exactly once")
		}
		close(second)
		select {
		case <-release:
		case <-attempt.Done():
			return nil, errors.New("duplicate retry interrupted the consuming planner")
		}
		return planner.Finish{Reason: planner.FinishGoal, Payload: "new answer", IncorporatedInputRevision: 999}, nil
	})
	spec := runSpecFor(q, p)
	spec.TaskID = tasks.TaskID(q.RunID)
	spec.Base.Trajectory = &planner.Trajectory{}
	var late tasks.InputReceipt
	spec.Base.SealCompletionChunks = func(context.Context) error {
		in, err := registry.Lookup(q)
		if err != nil {
			return err
		}
		late, err = in.EnqueueInput(ctx, tr, inputEvent(q, "late", "too late"))
		return err
	}
	type result struct {
		fin planner.Finish
		err error
	}
	done := make(chan result, 1)
	go func() { fin, err := loop.Run(ctx, spec); done <- result{fin, err} }()
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	in, err := registry.Lookup(q)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := in.EnqueueInput(ctx, tr, inputEvent(q, "event", "revised"))
	if err != nil || accepted.Status != tasks.InputAccepted {
		t.Fatalf("accepted=%+v err=%v", accepted, err)
	}
	// Pretend this ACK was lost. Exact concurrent retransmissions recover it.
	select {
	case <-second:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := in.EnqueueInput(ctx, tr, inputEvent(q, "event", "revised"))
			if err != nil || r.Revision != 1 {
				t.Errorf("retry=%+v err=%v", r, err)
			}
		}()
	}
	wg.Wait()
	before, err := tr.GetInputReceipt(ctx, tasks.TaskID(q.RunID), "event")
	if err != nil || before.Status != tasks.InputAccepted {
		t.Fatalf("unconsumed receipt=%+v err=%v", before, err)
	}
	close(release)
	got := <-done
	if got.err != nil || got.fin.IncorporatedInputRevision != 1 || calls != 2 {
		t.Fatalf("finish=%+v err=%v calls=%d", got.fin, got.err, calls)
	}
	if late.Status != tasks.InputDeclined || late.Revision != 0 {
		t.Fatalf("late input changed sealed revision=%+v", late)
	}
	after, err := tr.GetInputReceipt(ctx, tasks.TaskID(q.RunID), "event")
	if err != nil || after.Status != tasks.InputApplied {
		t.Fatalf("consumed receipt=%+v err=%v", after, err)
	}
	if len(spec.Base.Trajectory.Steps) != 1 {
		t.Fatalf("duplicate context projections=%d", len(spec.Base.Trajectory.Steps))
	}
	if err := tr.MarkComplete(ctx, tasks.TaskID(q.RunID), tasks.TaskResult{Value: []byte(`"sealed"`), IncorporatedInputRevision: got.fin.IncorporatedInputRevision}); err != nil {
		t.Fatal(err)
	}
}

func TestRun_InputReceipts_CancelBeforeConsumptionRemainsTerminal(t *testing.T) {
	loop, registry, tr, q, ctx := inputRunFixture(t)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	spec := runSpecFor(q, interruptPlanner(func(attempt context.Context, _ planner.RunContext) (planner.Decision, error) {
		calls++
		close(entered)
		<-release
		return nil, attempt.Err()
	}))
	spec.TaskID = tasks.TaskID(q.RunID)
	done := make(chan error, 1)
	go func() { _, err := loop.Run(ctx, spec); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	in, err := registry.Lookup(q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.EnqueueInput(ctx, tr, inputEvent(q, "event", "revised")); err != nil {
		t.Fatal(err)
	}
	if err := in.Enqueue(ControlEvent{Type: ControlCancel, Identity: q, CallerTenant: q.TenantID, CallerScope: ScopeOwnerUser, Payload: map[string]any{"hard": true}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if _, err := tr.Cancel(ctx, tasks.TaskID(q.RunID), "stop"); err != nil {
		t.Fatal(err)
	}
	r, err := tr.GetInputReceipt(ctx, tasks.TaskID(q.RunID), "event")
	if err != nil || r.Status != tasks.InputTerminal || calls != 1 {
		t.Fatalf("receipt=%+v err=%v calls=%d", r, err, calls)
	}
}

type failInputCheckpoint struct{ checkpointRecorder }

func (f *failInputCheckpoint) RecordContext(context.Context, planner.RunContext, planner.Step) error {
	return errors.New("input checkpoint unavailable")
}
func TestRun_InputReceipts_CheckpointFailureNeverReportsApplied(t *testing.T) {
	loop, registry, tr, q, ctx := inputRunFixture(t)
	calls := 0
	spec := runSpecFor(q, interruptPlanner(func(context.Context, planner.RunContext) (planner.Decision, error) {
		calls++
		return planner.CallTool{Tool: "fixture"}, nil
	}))
	spec.TaskID = tasks.TaskID(q.RunID)
	spec.Base.Trajectory = &planner.Trajectory{}
	spec.DispatchCheckpoint = &failInputCheckpoint{checkpointRecorder{t: t}}
	spec.ToolExecutor = checkpointExecutor(func(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
		in, _ := registry.Lookup(q)
		_, err := in.EnqueueInput(ctx, tr, inputEvent(q, "event", "revised"))
		return "fixture", "fixture", err
	})
	if _, err := loop.Run(ctx, spec); err == nil {
		t.Fatal("checkpoint failure hidden")
	}
	r, err := tr.GetInputReceipt(ctx, tasks.TaskID(q.RunID), "event")
	if err != nil || r.Status != tasks.InputAccepted || calls != 1 {
		t.Fatalf("receipt=%+v err=%v calls=%d", r, err, calls)
	}
}

func TestRun_InputReceipts_AfterResponseBeforeFinishForcesNewRevision(t *testing.T) {
	loop, registry, tr, q, ctx := inputRunFixture(t)
	calls, responses := 0, 0
	spec := runSpecFor(q, interruptPlanner(func(context.Context, planner.RunContext) (planner.Decision, error) {
		calls++
		return planner.Finish{Reason: planner.FinishGoal, Payload: "answer"}, nil
	}))
	spec.TaskID = tasks.TaskID(q.RunID)
	spec.Base.Trajectory = &planner.Trajectory{}
	spec.Base.AfterPlannerStep = func(context.Context) error {
		responses++
		if responses <= 2 {
			in, err := registry.Lookup(q)
			if err != nil {
				return err
			}
			id, text := "one", "first correction"
			if responses == 2 {
				id, text = "two", "second correction"
			}
			_, err = in.EnqueueInput(ctx, tr, inputEvent(q, id, text))
			return err
		}
		return nil
	}
	fin, err := loop.Run(ctx, spec)
	if err != nil || calls != 3 || fin.IncorporatedInputRevision != 2 {
		t.Fatalf("finish=%+v err=%v calls=%d", fin, err, calls)
	}
	for _, event := range []string{"one", "two"} {
		r, err := tr.GetInputReceipt(ctx, tasks.TaskID(q.RunID), event)
		if err != nil || r.Status != tasks.InputApplied {
			t.Fatalf("receipt=%+v err=%v", r, err)
		}
	}
	if len(spec.Base.Trajectory.Steps) != 2 {
		t.Fatalf("input projections=%d", len(spec.Base.Trajectory.Steps))
	}
}

type failConsumedInputRegistry struct{ tasks.TaskRegistry }

func (f failConsumedInputRegistry) MarkInputApplied(context.Context, tasks.TaskID, string, uint64) (tasks.InputReceipt, error) {
	return tasks.InputReceipt{}, errors.New("consumed receipt write unavailable")
}
func TestRun_InputReceipts_AppliedWriteFailureStopsDecision(t *testing.T) {
	_, _, tr, q, ctx := inputRunFixture(t)
	loop, registry, _ := newTestRunLoop(t, WithTaskRegistry(failConsumedInputRegistry{tr}))
	calls, executions := 0, 0
	spec := runSpecFor(q, interruptPlanner(func(context.Context, planner.RunContext) (planner.Decision, error) {
		calls++
		return planner.CallTool{Tool: "fixture"}, nil
	}))
	spec.TaskID = tasks.TaskID(q.RunID)
	spec.Base.Trajectory = &planner.Trajectory{}
	spec.ToolExecutor = checkpointExecutor(func(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
		executions++
		in, _ := registry.Lookup(q)
		_, err := in.EnqueueInput(ctx, tr, inputEvent(q, "event", "revised"))
		return "fixture", "fixture", err
	})
	if _, err := loop.Run(ctx, spec); err == nil {
		t.Fatal("consumed receipt failure hidden")
	}
	receipt, err := tr.GetInputReceipt(ctx, tasks.TaskID(q.RunID), "event")
	if err != nil || receipt.Status != tasks.InputAccepted || calls != 2 || executions != 1 {
		t.Fatalf("receipt=%+v err=%v calls=%d executions=%d", receipt, err, calls, executions)
	}
}

func TestRun_InputReceipts_RestorationDoesNotReuseAnotherTasksEventID(t *testing.T) {
	loop, _, tr, q, ctx := inputRunFixture(t)
	if _, err := tr.AcceptInput(ctx, tasks.TaskID(q.RunID), "same-event", "this task correction"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.MarkInputApplied(ctx, tasks.TaskID(q.RunID), "same-event", 1); err != nil {
		t.Fatal(err)
	}
	previous := q
	previous.RunID = "previous-task"
	priorStep, err := steeringContextStep(ControlEvent{Type: ControlUserMessage, Identity: previous, EventID: "same-event", InputRevision: 1, Payload: map[string]any{"message": "earlier task correction"}})
	if err != nil {
		t.Fatal(err)
	}
	spec := runSpecFor(q, interruptPlanner(func(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
		if len(rc.Control.UserMessages) != 0 {
			return nil, errors.New("restoration replayed an already consumed control")
		}
		if len(rc.Trajectory.Steps) != 2 || !hasInputProjection(rc.Trajectory, q.RunID, "same-event") || !hasInputProjection(rc.Trajectory, previous.RunID, "same-event") {
			return nil, errors.New("task-local input identity was conflated across retained turns")
		}
		return planner.Finish{Reason: planner.FinishGoal}, nil
	}))
	spec.TaskID = tasks.TaskID(q.RunID)
	spec.Base.Trajectory = &planner.Trajectory{Steps: []planner.Step{priorStep}}
	fin, err := loop.Run(ctx, spec)
	if err != nil || fin.IncorporatedInputRevision != 1 {
		t.Fatalf("finish=%+v err=%v", fin, err)
	}
}
