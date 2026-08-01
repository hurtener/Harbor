package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// removeconnection_test.go — unit coverage for
// `agent_config.remove_mcp_connection`: the two distinct loud errors (unknown
// name / boot-declared name), the empty-name validation leg, the atomic
// residue prune + sibling carry-forward, and the `mcp.connection.removed`
// event emission.

// seedConnRevision writes an active revision pinning a single runtime-added
// connection (name) plus tool-exposure residue that BELONGS to that server
// (its paused entry + a "<name>_tool" disabled tool + loading overrides) AND
// residue belonging to an UNRELATED server, so a test can assert the prune is
// surgical.
//
//nolint:unparam // test helper — the server name is a fixed fixture across the cases, kept as a param for call-site readability.
func seedConnRevision(t *testing.T, ctx context.Context, reg agentcfg.Registry, name string) {
	t.Helper()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	payload := agentcfg.ConfigPayload{
		Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
			{Name: name, Transport: agentcfg.MCPTransportHTTP, URL: "https://example.invalid/x"},
			{Name: "other", Transport: agentcfg.MCPTransportHTTP, URL: "https://example.invalid/other"},
		}},
		ToolExposure: &agentcfg.ToolExposure{
			PausedServers:      []string{name, "other"},
			DisabledTools:      []string{name + "_toolA", "other_toolB"},
			ServerLoadingModes: map[string]string{name: "deferred", "other": "always"},
			ToolLoadingModes:   map[string]string{name + "_toolA": "always", "other_toolB": "deferred"},
		},
		Hooks: &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: "sink", TimeoutMS: 1000}},
	}
	if _, err := reg.SetRevision(ctx, q, "agent-remove", agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
}

func removeReq(name string) prototypes.AgentConfigRemoveMCPConnectionRequest {
	return prototypes.AgentConfigRemoveMCPConnectionRequest{Identity: scope(), AgentID: "agent-remove", Name: name}
}

func TestRemoveMCPConnection_DropsDescriptor_PrunesResidue_CarriesSiblings(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	bus := newCollectingBus(t)
	s, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedConnRevision(t, ctx, reg, "srv")

	resp, err := s.RemoveMCPConnection(ctx, removeReq("srv"))
	if err != nil {
		t.Fatalf("RemoveMCPConnection: %v", err)
	}
	if resp.Name != "srv" {
		t.Fatalf("resp.Name = %q, want srv", resp.Name)
	}

	p := resp.Revision.Payload
	// The "srv" descriptor is gone; "other" survives.
	if p.Connections == nil || len(p.Connections.Servers) != 1 || p.Connections.Servers[0].Name != "other" {
		t.Fatalf("connections not pruned to [other]: %#v", p.Connections)
	}
	// The "srv" residue is pruned; "other" residue survives.
	te := p.ToolExposure
	if te == nil {
		t.Fatal("tool_exposure section unexpectedly nil")
	}
	if got := te.PausedServers; len(got) != 1 || got[0] != "other" {
		t.Errorf("paused_servers = %v, want [other]", got)
	}
	if got := te.DisabledTools; len(got) != 1 || got[0] != "other_toolB" {
		t.Errorf("disabled_tools = %v, want [other_toolB]", got)
	}
	if _, ok := te.ServerLoadingModes["srv"]; ok {
		t.Error("server_loading_modes still carries the removed server key")
	}
	if _, ok := te.ServerLoadingModes["other"]; !ok {
		t.Error("server_loading_modes dropped the unrelated server key")
	}
	if _, ok := te.ToolLoadingModes["srv_toolA"]; ok {
		t.Error("tool_loading_modes still carries the removed server's tool key")
	}
	if _, ok := te.ToolLoadingModes["other_toolB"]; !ok {
		t.Error("tool_loading_modes dropped the unrelated tool key")
	}
	// The Hooks sibling section is carried forward unchanged.
	if p.Hooks == nil || p.Hooks.RunCompletion == nil || p.Hooks.RunCompletion.Tool != "sink" {
		t.Errorf("hooks section not carried forward: %#v", p.Hooks)
	}

	// The canonical mcp.connection.removed event fired with the server + revision.
	evs := bus.eventsOfType(agentcfg.EventTypeMCPConnectionRemoved)
	if len(evs) != 1 {
		t.Fatalf("want 1 mcp.connection.removed event, got %d", len(evs))
	}
	pl, ok := evs[0].Payload.(agentcfg.MCPConnectionRemovedPayload)
	if !ok {
		t.Fatalf("payload type = %T, want MCPConnectionRemovedPayload", evs[0].Payload)
	}
	if pl.ServerID != "srv" || pl.RevisionID != resp.Revision.RevisionID {
		t.Errorf("event payload mismatch: %#v", pl)
	}
}

// TestRemoveMCPConnection_SiblingPrefixServer_DisableSurvives is the
// adversarial-review regression: connection names legally contain "_", so
// removing server "git" while sibling "git_hub" stays declared must NOT prune
// "git_hub_clone" from DisabledTools — that would silently RE-ENABLE the
// sibling's admin-disabled tool (a policy downgrade, CLAUDE.md §13). The
// removed server's OWN residue is still pruned.
func TestRemoveMCPConnection_SiblingPrefixServer_DisableSurvives(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	payload := agentcfg.ConfigPayload{
		Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
			{Name: "git", Transport: agentcfg.MCPTransportHTTP, URL: "https://example.invalid/git"},
			{Name: "git_hub", Transport: agentcfg.MCPTransportHTTP, URL: "https://example.invalid/git_hub"},
		}},
		ToolExposure: &agentcfg.ToolExposure{
			// "git_clone" belongs to the removed server; "git_hub_clone" belongs
			// to the SIBLING (it is prefixed by both "git_" and "git_hub_").
			DisabledTools:    []string{"git_clone", "git_hub_clone"},
			ToolLoadingModes: map[string]string{"git_clone": "deferred", "git_hub_clone": "deferred"},
		},
	}
	if _, serr := reg.SetRevision(ctx, q, "agent-remove", agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{}); serr != nil {
		t.Fatalf("seed: %v", serr)
	}

	resp, err := s.RemoveMCPConnection(ctx, removeReq("git"))
	if err != nil {
		t.Fatalf("RemoveMCPConnection: %v", err)
	}
	te := resp.Revision.Payload.ToolExposure
	if te == nil {
		t.Fatal("tool_exposure unexpectedly nil — the sibling's disable was wiped")
	}
	// The sibling's admin-disabled tool SURVIVES the prune.
	if got := te.DisabledTools; len(got) != 1 || got[0] != "git_hub_clone" {
		t.Fatalf("disabled_tools = %v, want [git_hub_clone] (sibling disable must survive — policy downgrade otherwise)", got)
	}
	if _, ok := te.ToolLoadingModes["git_hub_clone"]; !ok {
		t.Error("sibling's tool_loading_modes entry was wrongly pruned")
	}
	// The removed server's OWN residue is pruned.
	if _, ok := te.ToolLoadingModes["git_clone"]; ok {
		t.Error("removed server's own tool_loading_modes entry survived the prune")
	}
	// And only the sibling descriptor remains.
	if conns := resp.Revision.Payload.Connections; conns == nil || len(conns.Servers) != 1 || conns.Servers[0].Name != "git_hub" {
		t.Fatalf("connections = %#v, want [git_hub]", resp.Revision.Payload.Connections)
	}
}

func TestRemoveMCPConnection_UnknownName_FailsLoud_NoRevisionNoEvent(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	bus := newCollectingBus(t)
	s, _ := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
	seedConnRevision(t, ctx, reg, "srv")

	_, err := s.RemoveMCPConnection(ctx, removeReq("ghost"))
	if !errors.Is(err, agentcfgprotocol.ErrConnectionNotFound) {
		t.Fatalf("err = %v, want ErrConnectionNotFound", err)
	}
	if n := len(bus.eventsOfType(agentcfg.EventTypeMCPConnectionRemoved)); n != 0 {
		t.Errorf("unknown-name remove emitted %d removed events, want 0", n)
	}
}

func TestRemoveMCPConnection_BootDeclaredName_DistinctError(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, _ := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBootDeclaredMCPServers([]string{"yaml-srv"}))
	seedConnRevision(t, ctx, reg, "srv")

	// "yaml-srv" is boot-declared and NOT in the revision → distinct error.
	_, err := s.RemoveMCPConnection(ctx, removeReq("yaml-srv"))
	if !errors.Is(err, agentcfgprotocol.ErrBootDeclaredConnection) {
		t.Fatalf("err = %v, want ErrBootDeclaredConnection", err)
	}
	// The distinct error is NOT the plain not-found (the two are disjoint).
	if errors.Is(err, agentcfgprotocol.ErrConnectionNotFound) {
		t.Fatal("boot-declared error must be distinct from ErrConnectionNotFound")
	}
}

func TestRemoveMCPConnection_EmptyName_Invalid(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, _ := agentcfgprotocol.NewService(reg)
	_, err := s.RemoveMCPConnection(ctx, removeReq("   "))
	if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("err = %v, want ErrInvalidConnection", err)
	}
}

func TestRemoveMCPConnection_IncompleteIdentity_FailsClosed(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, _ := agentcfgprotocol.NewService(reg)
	_, err := s.RemoveMCPConnection(ctx, prototypes.AgentConfigRemoveMCPConnectionRequest{
		Identity: prototypes.IdentityScope{Tenant: "t"}, AgentID: "a", Name: "x",
	})
	if !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("err = %v, want ErrIdentityRequired", err)
	}
}

// --- a minimal collecting bus for event assertions ---

type collectingBus struct {
	events.EventBus
	got []events.Event
}

func newCollectingBus(t *testing.T) *collectingBus {
	t.Helper()
	return &collectingBus{}
}

func (b *collectingBus) Publish(_ context.Context, ev events.Event) error {
	b.got = append(b.got, ev)
	return nil
}

func (b *collectingBus) eventsOfType(tp events.EventType) []events.Event {
	var out []events.Event
	for _, e := range b.got {
		if e.Type == tp {
			out = append(out, e)
		}
	}
	return out
}
