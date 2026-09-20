package steering

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

type contextCheckpointRecorder struct {
	checkpointRecorder
	fail  error
	count int
}

func (c *contextCheckpointRecorder) RecordContext(ctx context.Context, rc planner.RunContext, step planner.Step) error {
	c.count++
	if c.mu != nil {
		if !c.mu.TryRLock() {
			c.t.Error("context persistence held inspection lock")
		} else {
			c.mu.RUnlock()
		}
	}
	if step.Action != nil || step.LLMObservation == nil || rc.Quadruple != runA {
		c.t.Error("invalid inert context")
	}
	if c.fail != nil {
		return c.fail
	}
	return ctx.Err()
}
func TestRunLoop_ContextCheckpoint_FailureAndFiltering(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var mu sync.RWMutex
		record := &contextCheckpointRecorder{checkpointRecorder: checkpointRecorder{t: t, mu: &mu}}
		if fail {
			record.fail = errors.New("injected context store failure")
		}
		spec := runSpecFor(runA, &scriptedPlanner{})
		spec.Base.Trajectory = &planner.Trajectory{}
		spec.TrajectoryMu = &mu
		spec.DispatchCheckpoint = record
		for _, kind := range []ControlType{ControlApprove, ControlReject, ControlPause, ControlResume, ControlCancel, ControlPrioritize} {
			if err := checkpointSteeringContext(t.Context(), spec, ControlEvent{Type: kind, Payload: map[string]any{"token": "SECRET-TOKEN"}}); err != nil {
				t.Fatal(err)
			}
		}
		if record.count != 0 || len(spec.Base.Trajectory.Steps) != 0 {
			t.Fatal("action control was retained as context")
		}
		payload := map[string]any{"message": "original", "ignored": "SECRET-TOKEN"}
		err := checkpointSteeringContext(t.Context(), spec, ControlEvent{Type: ControlUserMessage, Payload: payload})
		payload["message"] = "mutated"
		if fail {
			if !errors.Is(err, record.fail) || len(spec.Base.Trajectory.Steps) != 0 {
				t.Fatal("failed context write published a live observation")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		bytes, err := json.Marshal(spec.Base.Trajectory.Steps)
		if err != nil {
			t.Fatal(err)
		}
		if record.count != 1 || !strings.Contains(string(bytes), "original") || strings.Contains(string(bytes), "mutated") || strings.Contains(string(bytes), "SECRET-TOKEN") {
			t.Fatal("context aliased payload or captured unrelated authority")
		}
	}
}

func TestRunLoop_ContextCheckpoint_FailedWriteStopsInference(t *testing.T) {
	loop, registry, _ := newTestRunLoop(t)
	count := 0
	p := &scriptedPlanner{script: []scriptStep{{dec: planner.CallTool{Tool: "read", CallID: "read"}}, {dec: planner.Finish{Reason: planner.FinishGoal}}}}
	spec := runSpecFor(runA, p)
	spec.Base.Trajectory = &planner.Trajectory{}
	boom := errors.New("context store failure")
	checkpoint := &contextCheckpointRecorder{checkpointRecorder: checkpointRecorder{t: t}, fail: boom}
	spec.DispatchCheckpoint = checkpoint
	spec.ToolExecutor = checkpointExecutor(func(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
		count++
		in, err := registry.Lookup(runA)
		if err != nil {
			return nil, nil, err
		}
		err = in.Enqueue(ControlEvent{Type: ControlUserMessage, Identity: runA, CallerScope: ScopeOwnerUser, CallerTenant: runA.TenantID, Payload: map[string]any{"message": "correction"}})
		return "ok", "ok", err
	})
	_, err := loop.Run(t.Context(), spec)
	if !errors.Is(err, boom) || p.stepCount() != 1 || count != 1 || checkpoint.count != 1 {
		t.Fatalf("required context write failed open: err=%v planner=%d tools=%d context=%d", err, p.stepCount(), count, checkpoint.count)
	}
}
