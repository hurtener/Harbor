package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
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

// TestSetToolExposure_LoadingModes_RoundTrip proves the loading-mode
// override maps (D-281) round-trip through set_tool_exposure into the
// recorded revision.
func TestSetToolExposure_LoadingModes_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	resp, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{
			ServerLoadingModes: map[string]string{"srvA": "always"},
			ToolLoadingModes:   map[string]string{"srvA_x": "deferred"},
		},
	})
	if err != nil {
		t.Fatalf("set tool exposure: %v", err)
	}
	pl := resp.Revision.Payload
	if pl.ToolExposure == nil {
		t.Fatal("tool exposure section missing")
	}
	if pl.ToolExposure.ServerLoadingModes["srvA"] != "always" {
		t.Errorf("server_loading_modes = %+v", pl.ToolExposure.ServerLoadingModes)
	}
	if pl.ToolExposure.ToolLoadingModes["srvA_x"] != "deferred" {
		t.Errorf("tool_loading_modes = %+v", pl.ToolExposure.ToolLoadingModes)
	}
}

// TestSetToolExposure_InvalidLoadingMode_FailsLoud_NoRevision proves an
// unknown loading value fails loud with a client error BEFORE any registry
// write: the revision chain is unchanged and no event fires (D-281).
func TestSetToolExposure_InvalidLoadingMode_FailsLoud_NoRevision(t *testing.T) {
	cases := []struct {
		name         string
		server, tool map[string]string
	}{
		{name: "bad server value", server: map[string]string{"srvA": "sometimes"}},
		{name: "bad tool value", tool: map[string]string{"srvA_x": "eager"}},
		{name: "empty server key", server: map[string]string{"": "always"}},
		{name: "empty tool key", tool: map[string]string{"": "deferred"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, bus, reg := busSvc(t)
			sub, err := bus.Subscribe(ctx, events.Filter{Tenant: "t", User: "u", Session: "s"})
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			defer sub.Cancel()

			before, beforeOK, err := reg.Active(ctx, quadFor(t), testAgentID, agentcfg.ConfigScopeAgent)
			if err != nil {
				t.Fatalf("active (before): %v", err)
			}

			_, err = s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
				Identity: scope(), AgentID: testAgentID,
				ToolExposure: prototypes.AgentConfigToolExposure{ServerLoadingModes: tc.server, ToolLoadingModes: tc.tool},
			})
			if !errors.Is(err, agentcfgprotocol.ErrInvalidToolExposureLoading) {
				t.Fatalf("err = %v, want ErrInvalidToolExposureLoading", err)
			}

			after, afterOK, err := reg.Active(ctx, quadFor(t), testAgentID, agentcfg.ConfigScopeAgent)
			if err != nil {
				t.Fatalf("active (after): %v", err)
			}
			if beforeOK != afterOK || (beforeOK && before.RevisionID != after.RevisionID) {
				t.Fatalf("a revision was recorded on an invalid loading value: before=%v/%v after=%v/%v",
					beforeOK, before.RevisionID, afterOK, after.RevisionID)
			}

			select {
			case ev := <-sub.Events():
				t.Fatalf("no event should fire on a rejected set: got %+v", ev)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// TestSetRevision_InvalidLoadingMode_FailsLoud proves the full-payload
// set_revision path validates the loading-mode maps too (parity with
// set_tool_exposure / set_llm_params).
func TestSetRevision_InvalidLoadingMode_FailsLoud(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	_, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{ToolExposure: &prototypes.AgentConfigToolExposure{
			ToolLoadingModes: map[string]string{"srvA_x": "sometimes"},
		}},
	})
	if !errors.Is(err, agentcfgprotocol.ErrInvalidToolExposureLoading) {
		t.Fatalf("err = %v, want ErrInvalidToolExposureLoading", err)
	}
}

// TestSetToolExposure_DiffShowsLoadingModeDelta proves agent_config.diff
// carries the structured loading-mode change arms.
func TestSetToolExposure_DiffShowsLoadingModeDelta(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	r1, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{ServerLoadingModes: map[string]string{"srvA": "always"}},
	})
	if err != nil {
		t.Fatalf("r1: %v", err)
	}
	r2, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{
			ServerLoadingModes: map[string]string{"srvA": "deferred"},
			ToolLoadingModes:   map[string]string{"srvB_y": "always"},
		},
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
	if len(te.ServerLoadingChanges) != 1 || te.ServerLoadingChanges[0].Key != "srvA" ||
		te.ServerLoadingChanges[0].From != "always" || te.ServerLoadingChanges[0].To != "deferred" {
		t.Fatalf("server_loading_changes = %+v", te.ServerLoadingChanges)
	}
	if len(te.ToolLoadingChanges) != 1 || te.ToolLoadingChanges[0].Key != "srvB_y" || te.ToolLoadingChanges[0].To != "always" {
		t.Fatalf("tool_loading_changes = %+v", te.ToolLoadingChanges)
	}
}

// quadFor returns the fixed test identity quadruple `scope()` maps to, for
// direct registry reads in the "no revision recorded" assertions.
func quadFor(t *testing.T) identity.Quadruple {
	t.Helper()
	return identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
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
