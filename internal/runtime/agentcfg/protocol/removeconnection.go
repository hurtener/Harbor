package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// removeconnection.go — the admin-scoped `agent_config.remove_mcp_connection`
// control method: the removal complement of add_mcp_connection. Where pause
// (set_tool_exposure) is a projection-time flag on an already-attached server
// (the transport stays warm and resume re-exposes it), REMOVAL drops the
// connection from the agent's versioned desired state entirely — the
// descriptor no longer persists and the server is never re-attached.
//
// # A revision, never an imperative teardown (one mechanism, CLAUDE.md §13)
//
// The verb records a NEW revision whose connections section drops the named
// descriptor AND prunes that server's tool-exposure residue (its paused-set
// entry, its disabled-tool entries, and its loading-mode overrides), carrying
// EVERY sibling section (skills / prompt-layers / llm-params / hooks) forward
// under the rebuild-completeness guard. The PHYSICAL teardown — deregistering
// the server's tools from the catalog + MCP registry and closing its
// transport — happens NOT here but at the next run's projection boundary
// (run-start reconciliation's detach leg), so a live transport is never yanked
// mid-run. Rollback past an add detaches through the identical reconcile path.
//
// # The verb governs REVISIONED state only (two distinct loud errors)
//
// An unknown name (not in the active revision's connections) fails loud with
// ErrConnectionNotFound. A name that resolves to a BOOT-declared (yaml)
// server fails loud with a DISTINCT error, ErrBootDeclaredConnection: a
// boot-declared server is not revisioned state — the operator edits yaml and
// restarts. Neither error records a revision or emits an event.
//
// # Agent-bound tokens are NOT deleted on remove
//
// The persisted agent-bound sealed OAuth token (keyed by the agent's
// registration identity) is deliberately NOT deleted on removal: a subsequent
// re-add reuses the completed consent (no second authorization dance). Real
// credential revocation is a provider-side action; a dedicated revoke surface
// is a named follow-up if the need emerges.

// Remove-connection sentinel errors. The wire handler maps each onto a
// canonical Protocol Code + HTTP status; in-process callers compare with
// errors.Is.
var (
	// ErrConnectionNotFound — remove_mcp_connection named a connection that is
	// not in the agent's active revision (never runtime-added, or already
	// removed). No revision recorded, no event emitted (→ 404).
	ErrConnectionNotFound = errors.New("agentcfg/protocol: mcp connection not found in the agent's active revision")
	// ErrBootDeclaredConnection — remove_mcp_connection named a server that is
	// declared in the boot yaml (`tools.mcp_servers`). Distinct from
	// ErrConnectionNotFound: the verb governs revisioned state only; a
	// boot-declared server is edited in yaml + restart, not removed through
	// the control plane. No revision recorded, no event emitted (→ 400).
	ErrBootDeclaredConnection = errors.New("agentcfg/protocol: connection is boot-declared (yaml) — edit tools.mcp_servers and restart; the control plane governs runtime-added connections only")
)

// RemoveMCPConnection removes a runtime-added MCP connection by name. See the
// file doc for the full contract (revision + residue prune, the two distinct
// loud errors, the projection-boundary teardown, token retention).
func (s *Service) RemoveMCPConnection(ctx context.Context, req prototypes.AgentConfigRemoveMCPConnectionRequest) (prototypes.AgentConfigRemoveMCPConnectionResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigRemoveMCPConnectionResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigRemoveMCPConnectionResponse{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return prototypes.AgentConfigRemoveMCPConnectionResponse{}, fmt.Errorf("%w: name is empty", ErrInvalidConnection)
	}
	q := identity.Quadruple{Identity: id}

	// Serialise the registry read-modify-write per agent (so a concurrent
	// section edit cannot revert the removal or fork the parent chain).
	defer s.lockAgent(id.TenantID, req.AgentID)()

	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigRemoveMCPConnectionResponse{}, err
	}

	// Is the name in the agent's revisioned connections?
	var prior []agentcfg.MCPConnectionDescriptor
	if hasActive {
		prior = active.Payload.ConnectionDescriptors()
	}
	remaining, found := dropConnection(prior, name)
	if !found {
		// Not in revisioned state: distinguish a boot-declared server (a
		// distinct loud error — edit yaml + restart) from a plain unknown name.
		if _, boot := s.bootDeclaredMCPServers[name]; boot {
			return prototypes.AgentConfigRemoveMCPConnectionResponse{}, fmt.Errorf("%w: %q", ErrBootDeclaredConnection, name)
		}
		return prototypes.AgentConfigRemoveMCPConnectionResponse{}, fmt.Errorf("%w: %q", ErrConnectionNotFound, name)
	}

	// Build the removing revision: connections minus the named descriptor,
	// tool-exposure residue for that server pruned, every sibling section
	// carried forward (the rebuild-completeness guard).
	payload := agentcfg.ConfigPayload{}
	if hasActive {
		payload.Skills = active.Payload.Skills
		payload.PromptLayers = active.Payload.PromptLayers
		payload.LLMParams = active.Payload.LLMParams
		payload.Hooks = active.Payload.Hooks
		payload.ToolExposure = pruneExposureResidue(active.Payload.ToolExposure, name)
	}
	if len(remaining) > 0 {
		payload.Connections = &agentcfg.ConnectionsSection{Servers: remaining}
	}

	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload)
	if err != nil {
		return prototypes.AgentConfigRemoveMCPConnectionResponse{}, err
	}

	// Emit the canonical `mcp.connection.removed` event alongside the generic
	// `agent.config.revised` the registry already fired. A nil bus is a no-op.
	s.emitConnectionRemoved(ctx, q, req.AgentID, name, rev.RevisionID)

	return prototypes.AgentConfigRemoveMCPConnectionResponse{
		Revision:        revisionToWire(rev),
		Name:            name,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// dropConnection returns the descriptor slice with the named connection
// removed and whether it was present. The input slice is never mutated (a
// fresh slice is returned when the name is found).
func dropConnection(in []agentcfg.MCPConnectionDescriptor, name string) (remaining []agentcfg.MCPConnectionDescriptor, found bool) {
	for _, d := range in {
		if d.Name == name {
			found = true
			continue
		}
		remaining = append(remaining, d)
	}
	return remaining, found
}

// pruneExposureResidue returns a copy of the tool-exposure section with every
// entry that belongs to the removed server dropped: the server's paused-set
// entry (exact match) and per-server loading-mode override (keyed by source
// id), plus every disabled-tool and per-tool loading override whose catalog
// name is prefixed by the server's source id (the MCP driver names a source's
// tools "<source>_<tool>" / "<source>__resource.<uri>" — both carry the
// "<source>_" prefix). A nil / empty result section returns nil so an
// all-pruned section drops out of the canonical form. The input is never
// mutated.
//
// A different server whose name shares the removed server's "<source>_"
// prefix could in principle see one disabled-tool key over-pruned; the
// collision requires one MCP source name to be an underscore-extension of
// another and is accepted as a deliberate V1 simplification (the prune is
// hygiene, not a security boundary — a re-enabled sibling tool is re-disabled
// by the next tool-exposure edit).
func pruneExposureResidue(te *agentcfg.ToolExposure, server string) *agentcfg.ToolExposure {
	if te == nil {
		return nil
	}
	prefix := server + "_"
	out := &agentcfg.ToolExposure{
		PausedServers:      dropExact(te.PausedServers, server),
		DisabledTools:      dropPrefixed(te.DisabledTools, prefix),
		ServerLoadingModes: dropMapKey(te.ServerLoadingModes, server),
		ToolLoadingModes:   dropMapKeysPrefixed(te.ToolLoadingModes, prefix),
	}
	if len(out.PausedServers) == 0 && len(out.DisabledTools) == 0 &&
		len(out.ServerLoadingModes) == 0 && len(out.ToolLoadingModes) == 0 {
		return nil
	}
	return out
}

// dropExact returns a fresh copy of in with every element equal to v removed
// (nil when the result is empty).
func dropExact(in []string, v string) []string {
	var out []string
	for _, s := range in {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

// dropPrefixed returns a fresh copy of in with every element carrying prefix
// removed (nil when the result is empty).
func dropPrefixed(in []string, prefix string) []string {
	var out []string
	for _, s := range in {
		if !strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// dropMapKey returns a fresh copy of m without key k (nil when the result is
// empty).
func dropMapKey(m map[string]string, k string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for key, val := range m {
		if key != k {
			out[key] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// dropMapKeysPrefixed returns a fresh copy of m without any key carrying
// prefix (nil when the result is empty).
func dropMapKeysPrefixed(m map[string]string, prefix string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for key, val := range m {
		if !strings.HasPrefix(key, prefix) {
			out[key] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// emitConnectionRemoved publishes the canonical `mcp.connection.removed`
// event. A nil bus is a no-op (the removal revision is still recorded and the
// generic `agent.config.revised` already fired). A publish failure is logged
// loud (CLAUDE.md §13) but does not undo the removal.
func (s *Service) emitConnectionRemoved(ctx context.Context, q identity.Quadruple, agentID, server, revisionID string) {
	if s.bus == nil {
		return
	}
	now := s.now().UTC()
	s.publishConnectionEvent(ctx, agentcfg.EventTypeMCPConnectionRemoved, q, agentID, server, revisionID, now,
		agentcfg.MCPConnectionRemovedPayload{
			Author: q, AgentID: agentID, ServerID: server, RevisionID: revisionID, OccurredAt: now,
		})
}
