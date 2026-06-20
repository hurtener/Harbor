// cmd/harbor/cmd_dev_runloop_agentcfg_test.go — proves the per-task
// RunLoop driver's run-start agent-config skills projection consumes the
// desired-state registry end-to-end (the §13 consumer of the agent-config
// registry primitive). It drives the REAL driver method
// projectAgentConfigSkills against a REAL StateStore-backed registry, so the
// consumer-side run-loop path is exercised, not just the registry round-trip
// (closes the wave's adversarial-review WARN).

package main

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func acTestRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return reg
}

func acTestViews(names ...string) []skills.SkillView {
	out := make([]skills.SkillView, 0, len(names))
	for _, n := range names {
		out = append(out, skills.SkillView{Name: n})
	}
	return out
}

func acViewNames(vs []skills.SkillView) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

// TestPerTaskRunLoopDriver_ProjectsAgentConfigSkills_AtRunStart proves the
// REAL run-loop driver method narrows the run's skill views to the agent's
// active config-revision membership — and that a rollback changes what the
// next run would see. This is the run-loop consumer path, over a real
// registry.
func TestPerTaskRunLoopDriver_ProjectsAgentConfigSkills_AtRunStart(t *testing.T) {
	ctx := context.Background()
	reg := acTestRegistry(t)
	const agentID = "harbor-dev-agent"
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}

	// A driver with no registry wired → ungated (every skill passes).
	bare := &perTaskRunLoopDriver{}
	got, err := bare.projectAgentConfigSkills(ctx, q, acTestViews("a", "b"))
	if err != nil {
		t.Fatalf("bare driver: %v", err)
	}
	if names := acViewNames(got); len(names) != 2 {
		t.Fatalf("bare driver filtered to %v, want both", names)
	}

	// A driver with the registry wired but no active revision → ungated.
	d := &perTaskRunLoopDriver{agentConfig: reg, agentConfigID: agentID}
	got, err = d.projectAgentConfigSkills(ctx, q, acTestViews("a", "b"))
	if err != nil {
		t.Fatalf("no-revision driver: %v", err)
	}
	if names := acViewNames(got); len(names) != 2 {
		t.Fatalf("no-revision driver filtered to %v, want both", names)
	}

	// Set an active revision pinning {a,c} → the next run sees only a,c.
	r1, err := reg.SetRevision(ctx, q, agentID, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a", "c"}},
	})
	if err != nil {
		t.Fatalf("set r1: %v", err)
	}
	got, err = d.projectAgentConfigSkills(ctx, q, acTestViews("a", "b", "c"))
	if err != nil {
		t.Fatalf("projected: %v", err)
	}
	if names := acViewNames(got); len(names) != 2 || names[0] != "a" || names[1] != "c" {
		t.Fatalf("active-revision projection = %v, want [a c]", names)
	}

	// Set a wider revision, then roll back to r1 → the next run narrows again.
	if _, err := reg.SetRevision(ctx, q, agentID, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a", "b", "c"}},
	}); err != nil {
		t.Fatalf("set r2: %v", err)
	}
	if _, err := reg.Rollback(ctx, q, agentID, r1.RevisionID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, err = d.projectAgentConfigSkills(ctx, q, acTestViews("a", "b", "c"))
	if err != nil {
		t.Fatalf("post-rollback projected: %v", err)
	}
	if names := acViewNames(got); len(names) != 2 || names[0] != "a" || names[1] != "c" {
		t.Fatalf("post-rollback projection = %v, want [a c]", names)
	}
}
