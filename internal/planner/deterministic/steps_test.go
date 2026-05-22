package deterministic_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	statedriver "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// stepsTestDeps wires the minimum real-driver dependency stack
// (audit + events + state + tasks) the group-aware step tests need.
type stepsTestDeps struct {
	registry tasks.TaskRegistry
	bus      events.EventBus
}

func mustStepsDeps(t *testing.T) stepsTestDeps {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(t.Context(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		ReplayBufferSize:         16,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := statedriver.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("statedriver.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := tasks.Open(t.Context(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: tasks.DefaultDriver},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	return stepsTestDeps{registry: reg, bus: bus}
}

// --- CallToolStep ----------------------------------------------------------

func TestCallToolStep_EmitsCallToolOnMatch(t *testing.T) {
	step := &deterministic.CallToolStep{
		Tool: "alpha",
		ArgsBuilder: func(_ planner.RunContext) (json.RawMessage, error) {
			return json.RawMessage(`{"x":1}`), nil
		},
	}
	dec, claimed, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim")
	}
	call, ok := dec.(planner.CallTool)
	if !ok {
		t.Fatalf("dec = %T, want planner.CallTool", dec)
	}
	if call.Tool != "alpha" || string(call.Args) != `{"x":1}` {
		t.Errorf("unexpected CallTool: %+v", call)
	}
}

func TestCallToolStep_WhenGuardSkips(t *testing.T) {
	step := &deterministic.CallToolStep{
		Tool: "alpha",
		ArgsBuilder: func(_ planner.RunContext) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
		When: func(_ planner.RunContext) bool { return false },
	}
	dec, claimed, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimed {
		t.Errorf("expected skip, claimed instead: %+v", dec)
	}
}

func TestCallToolStep_ArgsBuilderErrorPropagates(t *testing.T) {
	sentinel := errors.New("args boom")
	step := &deterministic.CallToolStep{
		Tool: "alpha",
		ArgsBuilder: func(_ planner.RunContext) (json.RawMessage, error) {
			return nil, sentinel
		},
	}
	_, _, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is sentinel", err)
	}
}

func TestCallToolStep_MissingArgsBuilderErrors(t *testing.T) {
	step := &deterministic.CallToolStep{Tool: "alpha"}
	_, _, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected error on missing ArgsBuilder, got nil")
	}
}

func TestCallToolStep_MissingToolErrors(t *testing.T) {
	step := &deterministic.CallToolStep{
		ArgsBuilder: func(_ planner.RunContext) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	}
	_, _, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected error on missing Tool, got nil")
	}
}

// --- FinishStep ------------------------------------------------------------

func TestFinishStep_EmitsFinishOnMatch(t *testing.T) {
	step := &deterministic.FinishStep{
		Reason: planner.FinishGoal,
		PayloadBuilder: func(_ planner.RunContext) (any, error) {
			return "done", nil
		},
		MetadataBuilder: func(_ planner.RunContext) (map[string]any, error) {
			return map[string]any{"extra": 1}, nil
		},
	}
	rc := planner.RunContext{Quadruple: validQuadruple()}
	dec, claimed, err := step.Decide(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim")
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("dec = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	if got, _ := fin.Payload.(string); got != "done" {
		t.Errorf("Payload = %v, want %q", fin.Payload, "done")
	}
	if got, _ := fin.Metadata["extra"].(int); got != 1 {
		t.Errorf("Metadata[extra] = %v, want 1", fin.Metadata["extra"])
	}
	// RunID is auto-stamped per D-025 identity round-trip contract.
	if got, _ := fin.Metadata["run_id"].(string); got != rc.Quadruple.RunID {
		t.Errorf("Metadata[run_id] = %v, want %q", fin.Metadata["run_id"], rc.Quadruple.RunID)
	}
}

func TestFinishStep_InvalidReasonErrors(t *testing.T) {
	step := &deterministic.FinishStep{
		Reason: planner.FinishReason("nonsense"),
	}
	_, _, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected error on invalid Reason, got nil")
	}
}

func TestFinishStep_WhenGuardSkips(t *testing.T) {
	step := &deterministic.FinishStep{
		Reason: planner.FinishGoal,
		When:   func(_ planner.RunContext) bool { return false },
	}
	_, claimed, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimed {
		t.Error("expected skip when guard returns false")
	}
}

// --- PauseStep -------------------------------------------------------------

func TestPauseStep_EmitsRequestPauseOnMatch(t *testing.T) {
	step := &deterministic.PauseStep{
		Reason: planner.PauseApprovalRequired,
		PayloadBuilder: func(_ planner.RunContext) (map[string]any, error) {
			return map[string]any{"why": "deterministic"}, nil
		},
	}
	dec, claimed, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim")
	}
	pause, ok := dec.(planner.RequestPause)
	if !ok {
		t.Fatalf("dec = %T, want planner.RequestPause", dec)
	}
	if pause.Reason != planner.PauseApprovalRequired {
		t.Errorf("Reason = %q, want %q", pause.Reason, planner.PauseApprovalRequired)
	}
	if pause.Payload["why"] != "deterministic" {
		t.Errorf("Payload[why] = %v, want %q", pause.Payload["why"], "deterministic")
	}
}

func TestPauseStep_InvalidReasonErrors(t *testing.T) {
	step := &deterministic.PauseStep{Reason: planner.PauseReason("nonsense")}
	_, _, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected error on invalid PauseReason, got nil")
	}
}

// --- SpawnAndAwaitStep -----------------------------------------------------

func TestSpawnAndAwaitStep_RequiresOnResolved(t *testing.T) {
	deps := mustStepsDeps(t)
	step := &deterministic.SpawnAndAwaitStep{
		StepID: "spawn-1",
		Kind:   tasks.KindBackground,
		SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
			return planner.SpawnSpec{Description: "x"}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected step error on missing OnResolved")
	}
	if !errors.Is(err, planner.ErrDeterministicStep) {
		t.Errorf("err = %v, want errors.Is planner.ErrDeterministicStep", err)
	}
}

func TestSpawnAndAwaitStep_FirstCallEmitsSpawn(t *testing.T) {
	deps := mustStepsDeps(t)
	step := &deterministic.SpawnAndAwaitStep{
		StepID: "spawn-1",
		Kind:   tasks.KindBackground,
		SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
			return planner.SpawnSpec{
				Description: "background work",
				Query:       "do thing",
				RetainTurn:  false,
			}, nil
		},
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rc := planner.RunContext{Quadruple: validQuadruple()}
	dec, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spawn, ok := dec.(planner.SpawnTask)
	if !ok {
		t.Fatalf("dec = %T, want planner.SpawnTask", dec)
	}
	if spawn.Spec.Description != "background work" {
		t.Errorf("Spec.Description = %q, want %q", spawn.Spec.Description, "background work")
	}
	if spawn.GroupID == "" {
		t.Error("GroupID empty — should have been assigned by ResolveOrCreateGroup")
	}
}

func TestSpawnAndAwaitStep_SecondCallEmitsAwait(t *testing.T) {
	deps := mustStepsDeps(t)
	step := &deterministic.SpawnAndAwaitStep{
		StepID: "spawn-1",
		Kind:   tasks.KindBackground,
		SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
			return planner.SpawnSpec{Description: "x", RetainTurn: false}, nil
		},
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rc := planner.RunContext{Quadruple: validQuadruple()}
	// First call: emit SpawnTask.
	dec1, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := dec1.(planner.SpawnTask); !ok {
		t.Fatalf("dec1 = %T, want SpawnTask", dec1)
	}
	// Second call (group still open): emit AwaitTask.
	dec2, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	await, ok := dec2.(planner.AwaitTask)
	if !ok {
		t.Fatalf("dec2 = %T, want AwaitTask", dec2)
	}
	if await.TaskID == "" {
		t.Error("AwaitTask.TaskID empty — owner task id not threaded through")
	}
}

// --- WatchGroupStep --------------------------------------------------------

func TestWatchGroupStep_NotReadyEmitsAwait(t *testing.T) {
	deps := mustStepsDeps(t)

	q := validQuadruple()
	ctxWith, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Seed a real group through the registry so the step has
	// something to watch.
	group, err := deps.registry.ResolveOrCreateGroup(ctxWith, tasks.GroupRequest{
		SessionID:   q.Identity,
		OwnerTaskID: tasks.TaskID(q.RunID),
		Description: "watch-group-test",
	})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}

	step := &deterministic.WatchGroupStep{
		GroupID:     group.ID,
		OwnerTaskID: tasks.TaskID(q.RunID),
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec, err := p.Next(context.Background(), planner.RunContext{Quadruple: q})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	await, ok := dec.(planner.AwaitTask)
	if !ok {
		t.Fatalf("dec = %T, want planner.AwaitTask", dec)
	}
	if string(await.TaskID) != q.RunID {
		t.Errorf("AwaitTask.TaskID = %q, want %q", await.TaskID, q.RunID)
	}
}

func TestSpawnAndAwaitStep_WhenGuardSkips(t *testing.T) {
	deps := mustStepsDeps(t)
	step := &deterministic.SpawnAndAwaitStep{
		StepID: "guarded",
		Kind:   tasks.KindBackground,
		SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
			return planner.SpawnSpec{Description: "x"}, nil
		},
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
		When: func(_ planner.RunContext) bool { return false },
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(
			step,
			&deterministic.FinishStep{Reason: planner.FinishGoal},
		),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec, err := p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Guarded step skipped; FinishStep claims.
	if _, ok := dec.(planner.Finish); !ok {
		t.Errorf("dec = %T, want Finish (the guarded SpawnAndAwaitStep should have skipped)", dec)
	}
}

func TestSpawnAndAwaitStep_MissingSpecBuilderErrors(t *testing.T) {
	deps := mustStepsDeps(t)
	step := &deterministic.SpawnAndAwaitStep{
		StepID: "no-spec",
		Kind:   tasks.KindBackground,
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected error on missing SpecBuilder, got nil")
	}
}

func TestSpawnAndAwaitStep_SpecBuilderErrorPropagates(t *testing.T) {
	deps := mustStepsDeps(t)
	sentinel := errors.New("spec boom")
	step := &deterministic.SpawnAndAwaitStep{
		StepID: "spec-err",
		Kind:   tasks.KindBackground,
		SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
			return planner.SpawnSpec{}, sentinel
		},
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is sentinel", err)
	}
}

func TestWatchGroupStep_ResolvedGroupInvokesOnResolved(t *testing.T) {
	deps := mustStepsDeps(t)
	q := validQuadruple()
	ctxIdent, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Seed group + member + drive to terminal so the WatchGroup
	// channel is pre-resolved by the time the planner polls.
	group, err := deps.registry.ResolveOrCreateGroup(ctxIdent, tasks.GroupRequest{
		SessionID:   q.Identity,
		OwnerTaskID: tasks.TaskID(q.RunID),
		Description: "pre-resolved",
	})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}
	handle, err := deps.registry.Spawn(ctxIdent, tasks.SpawnRequest{
		Identity:    q,
		Kind:        tasks.KindBackground,
		Description: "member",
		GroupID:     group.ID,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := deps.registry.SealGroup(ctxIdent, group.ID); err != nil {
		t.Fatalf("SealGroup: %v", err)
	}
	if err := deps.registry.MarkRunning(ctxIdent, handle.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := deps.registry.MarkComplete(ctxIdent, handle.ID, tasks.TaskResult{
		Value: json.RawMessage(`{"v":1}`),
	}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	resolved := false
	step := &deterministic.WatchGroupStep{
		GroupID:     group.ID,
		OwnerTaskID: handle.ID,
		OnResolved: func(_ planner.RunContext, members []tasks.MemberOutcome) (planner.Decision, error) {
			resolved = true
			if len(members) == 0 {
				return planner.Finish{Reason: planner.FinishNoPath}, nil
			}
			return planner.Finish{
				Reason: planner.FinishGoal,
				Payload: map[string]any{
					"task_id": string(members[0].TaskID),
				},
			}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dec, err := p.Next(ctxIdent, planner.RunContext{Quadruple: q})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !resolved {
		t.Error("OnResolved was not invoked despite group already terminal")
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("dec = %T, want Finish", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
}

func TestWatchGroupStep_WhenGuardSkips(t *testing.T) {
	deps := mustStepsDeps(t)
	step := &deterministic.WatchGroupStep{
		GroupID:     tasks.TaskGroupID("anything"),
		OwnerTaskID: tasks.TaskID("anything"),
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
		When: func(_ planner.RunContext) bool { return false },
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(
			step,
			&deterministic.FinishStep{Reason: planner.FinishGoal},
		),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dec, err := p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, ok := dec.(planner.Finish); !ok {
		t.Errorf("dec = %T, want Finish (guarded WatchGroupStep should have skipped)", dec)
	}
}

func TestWatchGroupStep_MissingGroupIDErrors(t *testing.T) {
	deps := mustStepsDeps(t)
	step := &deterministic.WatchGroupStep{
		OwnerTaskID: tasks.TaskID("task-x"),
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected error on missing GroupID, got nil")
	}
}

func TestPauseStep_WhenGuardSkips(t *testing.T) {
	step := &deterministic.PauseStep{
		Reason: planner.PauseAwaitInput,
		When:   func(_ planner.RunContext) bool { return false },
	}
	_, claimed, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimed {
		t.Error("expected skip on guard=false")
	}
}

func TestPauseStep_PayloadBuilderErrorPropagates(t *testing.T) {
	sentinel := errors.New("payload boom")
	step := &deterministic.PauseStep{
		Reason: planner.PauseAwaitInput,
		PayloadBuilder: func(_ planner.RunContext) (map[string]any, error) {
			return nil, sentinel
		},
	}
	_, _, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is sentinel", err)
	}
}

func TestFinishStep_PayloadBuilderErrorPropagates(t *testing.T) {
	sentinel := errors.New("payload boom")
	step := &deterministic.FinishStep{
		Reason: planner.FinishGoal,
		PayloadBuilder: func(_ planner.RunContext) (any, error) {
			return nil, sentinel
		},
	}
	_, _, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is sentinel", err)
	}
}

func TestFinishStep_MetadataBuilderErrorPropagates(t *testing.T) {
	sentinel := errors.New("meta boom")
	step := &deterministic.FinishStep{
		Reason: planner.FinishGoal,
		MetadataBuilder: func(_ planner.RunContext) (map[string]any, error) {
			return nil, sentinel
		},
	}
	_, _, err := step.Decide(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is sentinel", err)
	}
}

func TestSpawnAndAwaitStep_ResolvedSkipsAfterFire(t *testing.T) {
	deps := mustStepsDeps(t)
	q := validQuadruple()
	ctxIdent, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	step := &deterministic.SpawnAndAwaitStep{
		StepID: "skip-after-resolve",
		Kind:   tasks.KindBackground,
		SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
			return planner.SpawnSpec{Description: "x"}, nil
		},
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.CallTool{Tool: "x"}, nil
		},
	}
	fallback := &deterministic.FinishStep{Reason: planner.FinishGoal}

	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step, fallback),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Call 1: SpawnTask emitted.
	rc := planner.RunContext{Quadruple: q}
	if _, err := p.Next(ctxIdent, rc); err != nil {
		t.Fatalf("Next #1: %v", err)
	}

	// Drive the spawned member through to completion.
	summaries, err := deps.registry.List(ctxIdent, q.Identity, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var memberID tasks.TaskID
	for _, s := range summaries {
		if s.Kind == tasks.KindBackground {
			memberID = s.ID
			break
		}
	}
	if memberID == "" {
		t.Fatal("member task not found")
	}
	if err := deps.registry.MarkRunning(ctxIdent, memberID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := deps.registry.MarkComplete(ctxIdent, memberID, tasks.TaskResult{}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// Call 2: OnResolved fires, CallTool emitted.
	dec2, err := p.Next(ctxIdent, rc)
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	if _, ok := dec2.(planner.CallTool); !ok {
		t.Fatalf("dec2 = %T, want CallTool", dec2)
	}

	// Call 3: the SpawnAndAwaitStep is now resolved → skip; the
	// fallback FinishStep claims.
	dec3, err := p.Next(ctxIdent, rc)
	if err != nil {
		t.Fatalf("Next #3: %v", err)
	}
	if _, ok := dec3.(planner.Finish); !ok {
		t.Errorf("dec3 = %T, want Finish (resolved step should have skipped, fallback claimed)", dec3)
	}
}

func TestWatchGroupStep_MissingGroupErrors(t *testing.T) {
	deps := mustStepsDeps(t)
	step := &deterministic.WatchGroupStep{
		GroupID:     tasks.TaskGroupID("does-not-exist"),
		OwnerTaskID: tasks.TaskID("task-x"),
		OnResolved: func(_ planner.RunContext, _ []tasks.MemberOutcome) (planner.Decision, error) {
			return planner.Finish{Reason: planner.FinishGoal}, nil
		},
	}
	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(step),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = p.Next(context.Background(), planner.RunContext{Quadruple: validQuadruple()})
	if err == nil {
		t.Fatal("expected step error on missing group, got nil")
	}
	if !errors.Is(err, planner.ErrDeterministicStep) {
		t.Errorf("err = %v, want errors.Is planner.ErrDeterministicStep", err)
	}
}
