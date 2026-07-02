package main

// Agent-provenance producer test: proves the per-task run-loop driver
// actually stamps its agentConfigID onto the run ctx via
// tools.WithInvokingAgent — the ctx a southbound tool transport reads at
// dispatch. This pins the PRODUCER side of the provenance seam (the MCP
// driver's `_meta.agent_id` stamp is tested in its own package); the
// devstack twin (harbortest/devstack) carries the identical assertion so
// the two run loops cannot drift (§17.6 twin discipline).

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
)

// provenanceProbePlanner records the invoking-agent provenance carried on
// the ctx the RunLoop hands the planner, then finishes immediately.
type provenanceProbePlanner struct {
	got chan string // receives InvokingAgentFrom's value ("" when absent)
}

func (p *provenanceProbePlanner) Next(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
	agentID, _ := tools.InvokingAgentFrom(ctx)
	select {
	case p.got <- agentID:
	default:
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// runProvenanceProbe boots a real driver (bus + RunLoop + TaskRegistry +
// probe planner) with the given agentConfigID, spawns one task, and returns
// the provenance value the planner observed on the run ctx.
func runProvenanceProbe(t *testing.T, agentConfigID string) string {
	t.Helper()
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &provenanceProbePlanner{got: make(chan string, 1)}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:           bus,
		runLoop:       rl,
		planner:       p,
		tasks:         reg,
		agentConfigID: agentConfigID,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	_ = spawnDriverTestTask(t, reg)

	select {
	case got := <-p.got:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never fired — driver did not pick up task.spawned")
		return ""
	}
}

// TestPerTaskRunLoopDriver_StampsInvokingAgentProvenance — the run loop
// stamps its non-empty agentConfigID as ctx provenance at run start.
func TestPerTaskRunLoopDriver_StampsInvokingAgentProvenance(t *testing.T) {
	if got := runProvenanceProbe(t, "agent-prov-1"); got != "agent-prov-1" {
		t.Fatalf("run ctx provenance = %q, want %q (run loop did not stamp WithInvokingAgent)", got, "agent-prov-1")
	}
}

// TestPerTaskRunLoopDriver_NoAgentConfigID_NoProvenance — a bare run (empty
// agentConfigID) stamps nothing: absence is the valid embedder shape.
func TestPerTaskRunLoopDriver_NoAgentConfigID_NoProvenance(t *testing.T) {
	if got := runProvenanceProbe(t, ""); got != "" {
		t.Fatalf("run ctx provenance = %q, want absent for an empty agentConfigID", got)
	}
}
