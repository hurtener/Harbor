package deterministic_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestE2E_Deterministic_SpawnAwaitResolveFinish is the load-bearing
// §13 primitive-with-consumer scenario test for Phase 48. The
// deterministic planner emits, in order:
//
//  1. SpawnTask — the SpawnAndAwaitStep's first call.
//  2. AwaitTask — the SpawnAndAwaitStep's WatchGroup non-blocking
//     receive sees an open group.
//  3. CallTool — the SpawnAndAwaitStep's WatchGroup receives the
//     resolved completion; OnResolved returns a CallTool decision.
//  4. Finish — a guarded FinishStep observes the CallTool's
//     trajectory step.
//
// The test wires a real tasks.TaskRegistry (in-process driver) + a
// real events.EventBus (inmem driver), spawns a background task in
// the registry, resolves it between planner calls, and asserts each
// Decision shape. This satisfies the §13 primitive-with-consumer
// policy by demonstrating the Phase 20/21 task registry surface is
// exercised by a real concrete planner — closing the policy for the
// deterministic-planner side of the wave. Phase 49's conformance
// pack uses this concrete as the second leg of cross-planner
// round-trip scenarios.
//
// Cross-subsystem wiring covered (CLAUDE.md §17):
//   - tasks.TaskRegistry (Phase 20) — Spawn, SealGroup,
//     WatchGroup, MarkRunning, MarkComplete, ResolveOrCreateGroup.
//   - tasks.TaskGroup (Phase 21) — GroupCompletion → MemberOutcome.
//   - events.EventBus (Phase 05) — the registry publishes lifecycle
//     events as it walks the FSM; the test does not assert on them
//     but exercises the real bus driver.
//   - planner.Planner (Phase 42) — interface contract surface.
//   - planner.WakePoll (Phase 42 + D-032) — the wake mode the step
//     implements via non-blocking WatchGroup receive.
func TestE2E_Deterministic_SpawnAwaitResolveFinish(t *testing.T) {
	deps := mustStepsDeps(t)

	q := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "t-scenario",
			UserID:    "u-scenario",
			SessionID: "s-scenario",
		},
		RunID: "r-scenario",
	}
	ctxIdent, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// The step graph:
	//
	//  Step 1: FinishStep gated on `CallTool already seen` — fires
	//          on the FINAL planner call (after the trajectory has
	//          a CallTool action recorded).
	//  Step 2: SpawnAndAwaitStep — emits SpawnTask first, then
	//          AwaitTask while the group is open, then CallTool
	//          (via OnResolved) once the group resolves.
	//  Step 3: FinishStep fallback (no guard) — only fires if every
	//          other step skips; surfaces a structural bug in the
	//          tree if it ever does.
	spawnStep := &deterministic.SpawnAndAwaitStep{
		StepID: "scenario-spawn",
		Kind:   tasks.KindBackground,
		SpecBuilder: func(_ planner.RunContext) (planner.SpawnSpec, error) {
			return planner.SpawnSpec{
				Description: "scenario background task",
				Query:       "do work",
				RetainTurn:  false,
			}, nil
		},
		OnResolved: func(_ planner.RunContext, members []tasks.MemberOutcome) (planner.Decision, error) {
			// Surface the resolved member's task ID into the
			// emitted CallTool args so the test can pin the
			// round-trip.
			if len(members) == 0 {
				return planner.Finish{
					Reason: planner.FinishNoPath,
					Metadata: map[string]any{
						"deterministic": "no_members",
					},
				}, nil
			}
			args, _ := json.Marshal(map[string]any{
				"task_id": string(members[0].TaskID),
				"status":  string(members[0].Status),
			})
			return planner.CallTool{
				Tool: "use_result",
				Args: args,
			}, nil
		},
	}

	finishGated := &deterministic.FinishStep{
		Reason: planner.FinishGoal,
		PayloadBuilder: func(rc planner.RunContext) (any, error) {
			// Surface the last CallTool's task_id arg as the
			// terminal payload.
			if rc.Trajectory == nil || len(rc.Trajectory.Steps) == 0 {
				return nil, nil
			}
			last := rc.Trajectory.Steps[len(rc.Trajectory.Steps)-1]
			if call, ok := last.Action.(planner.CallTool); ok {
				return string(call.Args), nil
			}
			return nil, nil
		},
		When: func(rc planner.RunContext) bool {
			if rc.Trajectory == nil {
				return false
			}
			for _, step := range rc.Trajectory.Steps {
				if _, ok := step.Action.(planner.CallTool); ok {
					return true
				}
			}
			return false
		},
	}

	finishFallback := &deterministic.FinishStep{
		Reason: planner.FinishNoPath,
		MetadataBuilder: func(_ planner.RunContext) (map[string]any, error) {
			return map[string]any{"deterministic": "scenario_fallback"}, nil
		},
	}

	p, err := deterministic.NewDeterministicPlanner(
		deterministic.WithSteps(finishGated, spawnStep, finishFallback),
		deterministic.WithRegistry(deps.registry),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// --- Call 1: expect SpawnTask. ----------------------------------
	rc := planner.RunContext{Quadruple: q, Goal: "scenario"}
	rc.Trajectory = &planner.Trajectory{}
	dec1, err := p.Next(ctxIdent, rc)
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	spawn, ok := dec1.(planner.SpawnTask)
	if !ok {
		t.Fatalf("dec1 = %T, want planner.SpawnTask", dec1)
	}
	if spawn.GroupID == "" {
		t.Fatal("dec1: SpawnTask.GroupID empty")
	}

	// Find the spawned member to drive its lifecycle. The registry
	// surfaces it via List.
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
		t.Fatal("spawned member task not found via List")
	}

	// --- Call 2: expect AwaitTask (group still open). --------------
	dec2, err := p.Next(ctxIdent, rc)
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	await, ok := dec2.(planner.AwaitTask)
	if !ok {
		t.Fatalf("dec2 = %T, want planner.AwaitTask", dec2)
	}
	if await.TaskID == "" {
		t.Error("AwaitTask.TaskID empty")
	}

	// Drive the spawned member through its FSM. The group is sealed
	// (the spawn step sealed it after Spawn), so the member
	// reaching `complete` resolves the group automatically.
	if err := deps.registry.MarkRunning(ctxIdent, memberID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := deps.registry.MarkComplete(ctxIdent, memberID, tasks.TaskResult{
		Value: json.RawMessage(`{"answer":42}`),
	}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// --- Call 3: expect CallTool (group resolved → OnResolved). ----
	dec3, err := p.Next(ctxIdent, rc)
	if err != nil {
		t.Fatalf("Next #3: %v", err)
	}
	call, ok := dec3.(planner.CallTool)
	if !ok {
		t.Fatalf("dec3 = %T, want planner.CallTool", dec3)
	}
	if call.Tool != "use_result" {
		t.Errorf("CallTool.Tool = %q, want %q", call.Tool, "use_result")
	}
	// Args contain the resolved task id + status — proof the
	// MemberOutcome made it through OnResolved.
	var parsed map[string]string
	if err := json.Unmarshal(call.Args, &parsed); err != nil {
		t.Fatalf("decode call args: %v", err)
	}
	if parsed["task_id"] != string(memberID) {
		t.Errorf("CallTool args task_id = %q, want %q", parsed["task_id"], string(memberID))
	}
	if parsed["status"] != string(tasks.StatusComplete) {
		t.Errorf("CallTool args status = %q, want %q", parsed["status"], string(tasks.StatusComplete))
	}

	// Append the CallTool action onto the trajectory so the gated
	// FinishStep fires next call.
	rc.Trajectory.Steps = append(rc.Trajectory.Steps, planner.Step{
		Action: call,
	})

	// --- Call 4: expect Finish (gated by CallTool in trajectory). --
	dec4, err := p.Next(ctxIdent, rc)
	if err != nil {
		t.Fatalf("Next #4: %v", err)
	}
	fin, ok := dec4.(planner.Finish)
	if !ok {
		t.Fatalf("dec4 = %T, want planner.Finish", dec4)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Finish.Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	// Payload echoes the CallTool args.
	if got, _ := fin.Payload.(string); got != string(call.Args) {
		t.Errorf("Finish.Payload = %v, want %q", fin.Payload, string(call.Args))
	}
}
