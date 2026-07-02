package devstack

// Agent-provenance producer twin test: the devstack run-loop driver must
// stamp its agentConfigID onto the run ctx via tools.WithInvokingAgent
// EXACTLY like cmd/harbor's per-task driver (whose twin assertion lives in
// cmd/harbor/cmd_dev_runloop_provenance_test.go) — the §17.6 twin
// discipline for the provenance seam southbound tool transports read.

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
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

// runDevstackProvenanceProbe boots the devstack run-loop driver (real bus +
// RunLoop + TaskRegistry + probe planner) with the given agentConfigID,
// spawns one foreground task, and returns the provenance value the planner
// observed on the run ctx.
func runDevstackProvenanceProbe(t *testing.T, agentConfigID string) string {
	t.Helper()
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	reg, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.OpenDriver: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &provenanceProbePlanner{got: make(chan string, 1)}
	driver, err := newDevStackRunLoopDriver(devStackRunLoopDriverOpts{
		bus:           bus,
		runLoop:       rl,
		planner:       p,
		tasks:         reg,
		agentConfigID: agentConfigID,
	})
	if err != nil {
		t.Fatalf("newDevStackRunLoopDriver: %v", err)
	}
	if err := driver.start(context.Background()); err != nil {
		t.Fatalf("driver.start: %v", err)
	}
	t.Cleanup(func() { _ = driver.close(context.Background()) })

	id := identity.Identity{TenantID: "tenant-prov", UserID: "user-prov", SessionID: "sess-prov"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id},
		Kind:     tasks.KindForeground,
		Query:    "provenance-probe goal",
	}); err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}

	select {
	case got := <-p.got:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never fired — devstack driver did not pick up task.spawned")
		return ""
	}
}

// TestDevStackRunLoopDriver_StampsInvokingAgentProvenance — the devstack run
// loop stamps its non-empty agentConfigID as ctx provenance at run start,
// identically to the production driver twin.
func TestDevStackRunLoopDriver_StampsInvokingAgentProvenance(t *testing.T) {
	if got := runDevstackProvenanceProbe(t, "agent-prov-1"); got != "agent-prov-1" {
		t.Fatalf("run ctx provenance = %q, want %q (devstack run loop did not stamp WithInvokingAgent)", got, "agent-prov-1")
	}
}

// TestDevStackRunLoopDriver_NoAgentConfigID_NoProvenance — a bare run (empty
// agentConfigID) stamps nothing: absence is the valid embedder shape.
func TestDevStackRunLoopDriver_NoAgentConfigID_NoProvenance(t *testing.T) {
	if got := runDevstackProvenanceProbe(t, ""); got != "" {
		t.Fatalf("run ctx provenance = %q, want absent for an empty agentConfigID", got)
	}
}
