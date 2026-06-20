package protocol_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// busSvc builds an agent-config Service over a registry + a SHARED bus, so a
// test can subscribe and observe the mcp.connection.* overlay events. It
// returns the service, the bus, and the registry (for active-state asserts).
func busSvc(t *testing.T) (*agentcfgprotocol.Service, events.EventBus, agentcfg.Registry) {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()); _ = bus.Close(context.Background()) })
	s, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s, bus, reg
}

// TestSetToolExposure_RecordsRevision_PreservesSkills proves a tool-exposure
// edit composes a new revision pinning the exposure AND preserves the active
// revision's skills section (the desired-state REPLACE semantics).
func TestSetToolExposure_RecordsRevision_PreservesSkills(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	// Seed a skills membership.
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"skA", "skB"}}},
	}); err != nil {
		t.Fatalf("seed skills: %v", err)
	}
	resp, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{
			PausedServers: []string{"srvA"},
			DisabledTools: []string{"srvB_x"},
		},
	})
	if err != nil {
		t.Fatalf("set tool exposure: %v", err)
	}
	pl := resp.Revision.Payload
	if pl.ToolExposure == nil || len(pl.ToolExposure.PausedServers) != 1 || pl.ToolExposure.PausedServers[0] != "srvA" {
		t.Fatalf("tool exposure not recorded: %+v", pl.ToolExposure)
	}
	if pl.Skills == nil || len(pl.Skills.Names) != 2 {
		t.Fatalf("skills section not preserved across tool-exposure edit: %+v", pl.Skills)
	}
}

// TestSetToolExposure_EmitsPausedAndResumed proves the per-server overlay
// events fire on the pause→resume transitions, against the real bus.
func TestSetToolExposure_EmitsPausedAndResumed(t *testing.T) {
	ctx := context.Background()
	s, bus, _ := busSvc(t)
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant: "t", User: "u", Session: "s",
		Types: []events.EventType{agentcfg.EventTypeMCPConnectionPaused, agentcfg.EventTypeMCPConnectionResumed},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	if _, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA"}},
	}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	paused := waitConnEvent(t, sub)
	pp, ok := paused.Payload.(agentcfg.MCPConnectionPausedPayload)
	if !ok {
		t.Fatalf("paused payload type %T", paused.Payload)
	}
	if pp.ServerID != "srvA" || pp.AgentID != testAgentID || pp.Author.UserID != "u" {
		t.Fatalf("paused payload = %+v", pp)
	}

	// Resume (clear the paused set).
	if _, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{},
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	resumed := waitConnEvent(t, sub)
	rp, ok := resumed.Payload.(agentcfg.MCPConnectionResumedPayload)
	if !ok {
		t.Fatalf("resumed payload type %T", resumed.Payload)
	}
	if rp.ServerID != "srvA" {
		t.Fatalf("resumed payload = %+v", rp)
	}
}

func waitConnEvent(t *testing.T, sub events.Subscription) events.Event {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no mcp.connection.* event observed")
		return events.Event{}
	}
}

// TestSetToolExposure_IdentityRequired proves an incomplete identity triple
// fails closed.
func TestSetToolExposure_IdentityRequired(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	_, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: prototypes.IdentityScope{Tenant: "t"}, AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA"}},
	})
	if err == nil {
		t.Fatal("incomplete identity should fail closed")
	}
}

// TestSetToolExposure_DiffShowsExposureDelta proves agent_config.diff carries
// the structured MCP-exposure set-diff across two revisions.
func TestSetToolExposure_DiffShowsExposureDelta(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	r1, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA"}},
	})
	if err != nil {
		t.Fatalf("r1: %v", err)
	}
	r2, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA", "srvB"}, DisabledTools: []string{"srvC_y"}},
	})
	if err != nil {
		t.Fatalf("r2: %v", err)
	}
	diff, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID,
		FromRevision: r1.Revision.RevisionID, ToRevision: r2.Revision.RevisionID,
	})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	te := diff.Diff.ToolExposure
	if len(te.PausedAdded) != 1 || te.PausedAdded[0] != "srvB" {
		t.Fatalf("paused_added = %v, want [srvB]", te.PausedAdded)
	}
	if len(te.DisabledAdded) != 1 || te.DisabledAdded[0] != "srvC_y" {
		t.Fatalf("disabled_added = %v, want [srvC_y]", te.DisabledAdded)
	}
}

// TestSetToolExposure_IdentityScopedIsolation proves a tool-exposure
// revision under tenant t is invisible to another tenant (agent_id is NOT an
// isolation widener; the tenant boundary holds).
func TestSetToolExposure_IdentityScopedIsolation(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	if _, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA"}},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	other := prototypes.IdentityScope{Tenant: "other-tenant", User: "u", Session: "s"}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: other, AgentID: testAgentID})
	if err != nil {
		t.Fatalf("get other tenant: %v", err)
	}
	if got.Set {
		t.Fatal("another tenant saw the agent's tool-exposure revision — isolation breach")
	}
}
