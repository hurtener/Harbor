// phase64_task_fsm_bridge_test.go — D-098 / issue #123 integration
// test. Exercises the production task FSM bridge end-to-end through
// the real `harbortest/devstack.Assemble` wiring (which mirrors
// `cmd/harbor/cmd_dev.go::bootDevStack` per D-094).
//
// The test asserts: when a foreground task is Spawned, the
// perTaskRunLoopDriver picks up the task.spawned event, drives the
// RunLoop against the configured planner, and translates the
// RunLoop's exit shape into TaskRegistry.Mark{Complete,Failed} so the
// task FSM reaches a terminal state without timing out.
//
// Real drivers everywhere (CLAUDE.md §17.3): real audit Redactor,
// real EventBus (inmem), real TaskRegistry (inprocess driver), real
// pauseresume Coordinator, real steering Registry, real RunLoop,
// real ReAct planner, real perTaskRunLoopDriver. The mock LLM is the
// only stub — it's an explicit dev-only escape hatch (§13 amendment).

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestTaskFSMBridge_ProductionPath_ReachesTerminalState pins D-098's
// closure of D-097's deliberate carve-out. Before this fix a foreground
// task spawned under `harbor dev` reached StatusPending and stayed
// there forever — the RunLoop ran but nothing translated its exit
// into a task FSM transition. The Console / operator would see a
// running planner with a pending task; the divergence between the
// FSM and reality is exactly the kind of correctness regression §17.6
// names.
//
// The test uses the production wiring (devstack.Assemble — D-094
// source-of-truth invariant) so a future regression in cmd_dev.go's
// driver construction OR harbortest/devstack's mirror would fail here.
func TestTaskFSMBridge_ProductionPath_ReachesTerminalState(t *testing.T) {
	t.Parallel()

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Mock LLM with a model profile so the ReAct planner has the
	// context-window knobs it needs. The mock LLM returns a terminal
	// response on every call so the planner finishes quickly without
	// network access (§17.4: integration tests are hermetic).
	llmSnap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: &llmSnap,
	})
	defer stack.Close()

	if stack.Tasks == nil {
		t.Fatal("devstack: Tasks registry is nil (the FSM bridge cannot fire without it)")
	}
	if stack.RunLoopDriver == nil {
		t.Fatal("devstack: RunLoopDriver is nil (the D-097 driver should have wired by default)")
	}

	// Spawn a foreground task under the dev identity. The registry
	// publishes task.spawned on the bus; the driver picks it up and
	// drives the RunLoop. The driver then calls Mark* on the registry
	// based on the RunLoop's exit shape.
	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	ctx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: devID},
		Kind:     tasks.KindForeground,
		Query:    "task FSM bridge end-to-end",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	// Poll the task's status with a bounded real-time timeout. The
	// FSM transitions Pending → Running → {Complete, Failed}. We
	// accept either terminal status — the precise outcome depends on
	// what the mock LLM returns (the test's contract is "reaches
	// terminal", not "reaches StatusComplete specifically"). A
	// long-lived StatusPending or StatusRunning is the regression
	// shape D-098 closes.
	deadline := time.Now().Add(8 * time.Second)
	var observed tasks.TaskStatus
	for time.Now().Before(deadline) {
		task, getErr := stack.Tasks.Get(ctx, h.ID)
		if getErr == nil {
			observed = task.Status
			if isTerminal(task.Status) {
				return // success: FSM reached a terminal state
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task FSM stuck at %q after 8s — D-098 bridge did not transition the task to a terminal state (StatusComplete/StatusFailed/StatusCancelled)",
		observed)
}

// isTerminal — local mirror of internal/tasks/drivers/inprocess.isTerminal
// (which is unexported). The set is {Complete, Failed, Cancelled};
// any of these satisfies the FSM-bridge invariant.
func isTerminal(s tasks.TaskStatus) bool {
	return s == tasks.StatusComplete || s == tasks.StatusFailed || s == tasks.StatusCancelled
}
