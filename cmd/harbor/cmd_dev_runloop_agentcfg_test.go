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
	"encoding/json"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/skills"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
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

func acToolCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	reg := func(name string, source tools.ToolSourceID) {
		t.Helper()
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
			Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	reg("srvA_one", "srvA")
	reg("srvA_two", "srvA")
	reg("srvB_three", "srvB")
	return cat
}

// TestPerTaskRunLoopDriver_ProjectsAgentConfigToolExposure_AtRunStart proves
// the REAL run-loop driver method excludes a paused server's tools (and a
// disabled tool) from the run's planner catalog view — the run-loop consumer
// path of the agent-config tool-exposure projection, over a real registry +
// real catalog.
func TestPerTaskRunLoopDriver_ProjectsAgentConfigToolExposure_AtRunStart(t *testing.T) {
	ctx := context.Background()
	reg := acTestRegistry(t)
	cat := acToolCatalog(t)
	const agentID = "harbor-dev-agent"
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	filter := tools.CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"}

	listNames := func(v tools.PlannerCatalogView) map[string]bool {
		out := map[string]bool{}
		for _, tl := range v.List() {
			out[tl.Name] = true
		}
		return out
	}

	// No registry wired → ungated (all three tools visible).
	bare := &perTaskRunLoopDriver{catalog: cat}
	v, err := bare.projectAgentConfigCatalog(ctx, q, filter)
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	if len(v.List()) != 3 {
		t.Fatalf("bare driver filtered the catalog: %v", listNames(v))
	}

	// Registry wired; pause srvA + disable srvB_three → next run sees neither.
	d := &perTaskRunLoopDriver{agentConfig: reg, agentConfigID: agentID, catalog: cat}
	if _, err := reg.SetRevision(ctx, q, agentID, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srvA"}, DisabledTools: []string{"srvB_three"}},
	}); err != nil {
		t.Fatalf("set revision: %v", err)
	}
	v, err = d.projectAgentConfigCatalog(ctx, q, filter)
	if err != nil {
		t.Fatalf("projected: %v", err)
	}
	got := listNames(v)
	if got["srvA_one"] || got["srvA_two"] {
		t.Fatalf("paused server srvA tools still visible: %v", got)
	}
	if got["srvB_three"] {
		t.Fatalf("disabled tool srvB_three still visible: %v", got)
	}
}

// TestPerTaskRunLoopDriver_ProjectsAgentConfigPromptLayers_AtRunStart proves
// the REAL run-loop driver method overlays the agent's active-config layered
// system prompt (operator base + user layer) onto the run's resolved override
// bundle at run start — the run-loop consumer path of the layered-prompt
// projection, over a real registry. A rollback changes what the next run sees.
func TestPerTaskRunLoopDriver_ProjectsAgentConfigPromptLayers_AtRunStart(t *testing.T) {
	ctx := context.Background()
	reg := acTestRegistry(t)
	const agentID = "harbor-dev-agent"
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	base := func(s string) *string { return &s }

	// No registry wired → bundle passes through unchanged.
	bare := &perTaskRunLoopDriver{}
	ov, err := bare.projectAgentConfigPromptLayers(ctx, q, nil)
	if err != nil {
		t.Fatalf("bare driver: %v", err)
	}
	if ov != nil {
		t.Fatalf("bare driver should not synthesise an override bundle: %+v", ov)
	}

	// Registry wired but no active revision → unchanged.
	d := &perTaskRunLoopDriver{agentConfig: reg, agentConfigID: agentID}
	ov, err = d.projectAgentConfigPromptLayers(ctx, q, nil)
	if err != nil {
		t.Fatalf("no-revision driver: %v", err)
	}
	if ov != nil {
		t.Fatalf("no-revision driver should not synthesise a bundle: %+v", ov)
	}

	// Set an active revision pinning base+user → the next run carries them.
	r1, err := reg.SetRevision(ctx, q, agentID, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: base("OPERATOR_BASE"), User: base("USER_LAYER")},
	})
	if err != nil {
		t.Fatalf("set r1: %v", err)
	}
	ov, err = d.projectAgentConfigPromptLayers(ctx, q, nil)
	if err != nil {
		t.Fatalf("projected: %v", err)
	}
	if ov == nil || ov.BasePromptLayer == nil || *ov.BasePromptLayer != "OPERATOR_BASE" ||
		ov.UserPromptLayer == nil || *ov.UserPromptLayer != "USER_LAYER" {
		t.Fatalf("active-revision projection = %+v", ov)
	}

	// A later revision clears the user layer (base only); roll back to r1 →
	// the next run sees the user layer again.
	if _, err := reg.SetRevision(ctx, q, agentID, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: base("OPERATOR_BASE")},
	}); err != nil {
		t.Fatalf("set r2: %v", err)
	}
	ovR2, err := d.projectAgentConfigPromptLayers(ctx, q, &planner.LLMOverrides{})
	if err != nil {
		t.Fatalf("r2 projected: %v", err)
	}
	if ovR2.UserPromptLayer != nil {
		t.Fatalf("r2 should have no user layer: %+v", ovR2.UserPromptLayer)
	}
	if _, err := reg.Rollback(ctx, q, agentID, r1.RevisionID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	ov, err = d.projectAgentConfigPromptLayers(ctx, q, nil)
	if err != nil {
		t.Fatalf("post-rollback projected: %v", err)
	}
	if ov == nil || ov.UserPromptLayer == nil || *ov.UserPromptLayer != "USER_LAYER" {
		t.Fatalf("post-rollback projection = %+v", ov)
	}
}
