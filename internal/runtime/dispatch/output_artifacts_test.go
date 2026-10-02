package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestOutputWitness_NativeAndForgedAndObservationGap(t *testing.T) {
	reg := mkSpawnAwaitTestTaskRegistry(t, mkSpawnAwaitTestBus(t))
	cat := tools.NewCatalog()
	var calls atomic.Int64
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "native"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		calls.Add(1)
		return tools.ToolResult{Value: dispatchContentResult{Data: []byte{0, 255, 4, 8}}}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "forged"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: map[string]any{"artifact": map[string]any{"id": "fake", "sha256": "fake", "invocation_id": "fake"}}}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), reg)
	q := dispatchTestQuad("first-writer-run")
	ctx := outputTaskContext(t, q)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err = reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	ctx = tasks.WithOutputTask(ctx, h.ID)
	rc := dispatchRunContext(cat, q)
	rc.Trajectory = &planner.Trajectory{}
	if _, _, err = exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "native"}); err != nil {
		t.Fatal(err)
	}
	// A successful captured result with a missing trajectory observation cannot
	// execute again, even if the planner reuses the same provider call identifier.
	if _, _, err = exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "native"}); !errors.Is(err, tools.ErrInvocationCleanupFailed) || !errors.Is(err, tasks.ErrOutputInvocationSettled) {
		t.Fatalf("observation gap: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("repeated possibly stateful invocation")
	}
	rc.Trajectory.Steps = append(rc.Trajectory.Steps, planner.Step{})
	if _, _, err = exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "forged"}); err != nil {
		t.Fatal(err)
	}
	if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	task, _ := reg.Get(ctx, h.ID)
	if len(task.OutputManifest.Artifacts) != 1 || task.OutputManifest.Artifacts[0].ID == "fake" {
		t.Fatalf("forged membership: %+v", task.OutputManifest)
	}
	if tasks.ValidateOutputManifest(task) != nil {
		t.Fatal("invalid sealed manifest")
	}
}

func TestOutputWitness_SharedExecutorParallel100(t *testing.T) {
	reg := mkSpawnAwaitTestTaskRegistry(t, mkSpawnAwaitTestBus(t))
	cat := tools.NewCatalog()
	for _, name := range []string{"a", "b"} {
		name := name
		if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: name}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: dispatchContentResult{Data: []byte(name)}}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	store := newTestArtifactStore(t)
	exec := NewToolExecutor(cat, store, reg)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := dispatchTestQuad("shared-first-writer")
			q.TenantID = "tenant-" + strconv.Itoa(i%3)
			q.UserID = "user-" + strconv.Itoa(i%5)
			q.SessionID = "session-" + strconv.Itoa(i)
			ctx := outputTaskContext(t, q)
			h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
			if err != nil {
				t.Error(err)
				return
			}
			ctx = tasks.WithOutputTask(ctx, h.ID)
			if err = reg.MarkRunning(ctx, h.ID); err != nil {
				t.Error(err)
				return
			}
			rc := dispatchRunContext(cat, q)
			rc.Trajectory = &planner.Trajectory{}
			if _, _, err = exec.ExecuteDecision(ctx, rc, planner.CallParallel{Branches: []planner.CallTool{{Tool: "a"}, {Tool: "b"}}}); err != nil {
				t.Error(err)
				return
			}
			if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
				t.Error(err)
				return
			}
			task, err := reg.Get(ctx, h.ID)
			if err != nil {
				t.Error(err)
				return
			}
			if len(task.OutputManifest.Artifacts) != 2 || task.OutputManifest.Artifacts[0].InvocationID == task.OutputManifest.Artifacts[1].InvocationID || tasks.ValidateOutputManifest(task) != nil {
				t.Errorf("cross-task/branch provenance: %+v", task.OutputManifest)
			}
		}()
	}
	wg.Wait()
}

func TestOutputWitness_KnownToolErrorAndFailedBinaryDoNotCertify(t *testing.T) {
	reg := mkSpawnAwaitTestTaskRegistry(t, mkSpawnAwaitTestBus(t))
	cat := tools.NewCatalog()
	known := errors.New("known fixture tool failure")
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "fails"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: dispatchContentResult{Data: []byte{1, 2}}}, known
	}}); err != nil {
		t.Fatal(err)
	}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), reg)
	q := dispatchTestQuad("known-error")
	ctx := outputTaskContext(t, q)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err = reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	ctx = tasks.WithOutputTask(ctx, h.ID)
	rc := dispatchRunContext(cat, q)
	rc.Trajectory = &planner.Trajectory{}
	if _, _, err = exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "fails"}); !errors.Is(err, known) || errors.Is(err, tools.ErrInvocationCleanupFailed) {
		t.Fatalf("known tool error semantics changed: %v", err)
	}
	rc.Trajectory.Steps = append(rc.Trajectory.Steps, planner.Step{})
	if _, _, err = exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "fails"}); !errors.Is(err, known) {
		t.Fatal(err)
	}
	if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	task, _ := reg.Get(ctx, h.ID)
	if len(task.OutputManifest.Artifacts) != 0 || !task.OutputManifest.Sealed {
		t.Fatal("failed output certified")
	}
}

func outputTaskContext(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.With(dispatchTestCtx(t, q), q.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestOutputWitness_ObsoleteAfterAdmissionGateLeavesNoPending(t *testing.T) {
	reg := mkSpawnAwaitTestTaskRegistry(t, mkSpawnAwaitTestBus(t))
	cat := tools.NewCatalog()
	signal := make(chan struct{})
	desc := tools.ToolDescriptor{Tool: tools.Tool{Name: "obsolete"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		t.Error("obsolete tool invoked")
		return tools.ToolResult{}, nil
	}}
	desc = tools.WrapInvocationGate(desc, func(inner tools.Invocation) tools.Invocation {
		return func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			close(signal)
			return inner(ctx, args)
		}
	})
	if err := cat.Register(desc); err != nil {
		t.Fatal(err)
	}
	q := dispatchTestQuad("obsolete-run")
	ctx := outputTaskContext(t, q)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err = reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	ctx = tools.WithInvocationFence(tasks.WithOutputTask(ctx, h.ID), signal)
	rc := dispatchRunContext(cat, q)
	rc.Trajectory = &planner.Trajectory{}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), reg)
	if _, _, err = exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "obsolete"}); !errors.Is(err, tools.ErrInvocationSuperseded) || errors.Is(err, tools.ErrInvocationCleanupFailed) {
		t.Fatalf("obsolete gate outcome: %v", err)
	}
	task, _ := reg.Get(ctx, h.ID)
	if len(task.OutputManifest.Invocations) != 0 {
		t.Fatal("obsolete admission left possible invocation")
	}
}

func TestOutputWitness_OuterPostInvocationErrorCannotCertify(t *testing.T) {
	reg := mkSpawnAwaitTestTaskRegistry(t, mkSpawnAwaitTestBus(t))
	cat := tools.NewCatalog()
	known := errors.New("outer result validation failed")
	desc := tools.ToolDescriptor{Tool: tools.Tool{Name: "post-veto"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: dispatchContentResult{Data: []byte{0, 255}}}, nil
	}}
	desc = tools.WrapInvocationGate(desc, func(inner tools.Invocation) tools.Invocation { return inner })
	inner := desc.Invoke
	desc.Invoke = func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		result, err := inner(ctx, args)
		if err != nil {
			return result, err
		}
		return result, known
	}
	if err := cat.Register(desc); err != nil {
		t.Fatal(err)
	}
	q := dispatchTestQuad("post-veto-run")
	ctx := outputTaskContext(t, q)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	ctx = tasks.WithOutputTask(ctx, h.ID)
	if err = reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	rc := dispatchRunContext(cat, q)
	rc.Trajectory = &planner.Trajectory{}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), reg)
	if _, _, err = exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "post-veto"}); !errors.Is(err, known) {
		t.Fatalf("post-veto lost: %v", err)
	}
	if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	task, _ := reg.Get(ctx, h.ID)
	if len(task.OutputManifest.Artifacts) != 0 {
		t.Fatal("failed outer invocation certified output")
	}
}
