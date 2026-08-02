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
// transport — happens NOT here but at the next run-start reconcile (the
// detach leg). Exposure correctness is next-turn; teardown is process-global:
// an in-flight run whose next step calls the detached server fails loudly
// (typed not-found / closed transport), never a hang or a silent success —
// see the projection.ReconcileConnections godoc for the full contract.
// Rollback past an add detaches through the identical reconcile path.
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
	// carried forward (the rebuild-completeness guard). The remaining declared
	// set guards the prefix prune: a sibling server whose name extends the
	// removed name (e.g. removing "git" with "git_hub" still declared) never
	// loses its own disable/loading entries — an over-prune would silently
	// RE-ENABLE an admin-disabled tool (a policy downgrade, CLAUDE.md §13).
	payload := agentcfg.ConfigPayload{}
	if hasActive {
		payload.Skills = active.Payload.Skills
		payload.PromptLayers = active.Payload.PromptLayers
		payload.LLMParams = active.Payload.LLMParams
		payload.Hooks = active.Payload.Hooks
		payload.Naming = active.Payload.Naming
		// The ordered additive prompt blocks are a sibling section like any
		// other: this verb replaces only its own, so the blocks survive.
		payload.ExtraSystemBlocks = active.Payload.ExtraSystemBlocks
		payload.OAuthProviders = active.Payload.OAuthProviders
		payload.SignedOAuthMCPPair = active.Payload.SignedOAuthMCPPair
		payload.ToolExposure = pruneExposureResidue(active.Payload.ToolExposure, name, remaining)
	}
	if len(remaining) > 0 {
		payload.Connections = &agentcfg.ConnectionsSection{Servers: remaining}
	}

	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload,
		agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
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
// name is prefixed by the removed server's source id (the MCP driver names a
// source's tools "<source>_<tool>" / "<source>__resource.<uri>" — both carry
// the "<source>_" prefix).
//
// Sibling safety (load-bearing): connection names may legally contain "_", so
// a tool id like "git_hub_clone" is prefixed by BOTH "git_" and "git_hub_".
// The prune therefore takes the REMAINING declared descriptors and refuses to
// drop any entry that is also prefixed by a remaining server's "<name>_" —
// pruning it would silently RE-ENABLE that sibling's admin-disabled tool (a
// policy downgrade, CLAUDE.md §13 silent degradation). Ambiguous entries stay;
// a leftover key that in fact belonged to the removed server is inert residue,
// which is strictly safer than a re-enabled capability.
//
// A nil / empty result section returns nil so an all-pruned section drops out
// of the canonical form. The input is never mutated.
func pruneExposureResidue(te *agentcfg.ToolExposure, server string, remaining []agentcfg.MCPConnectionDescriptor) *agentcfg.ToolExposure {
	if te == nil {
		return nil
	}
	keepPrefixes := make([]string, 0, len(remaining))
	for _, d := range remaining {
		keepPrefixes = append(keepPrefixes, d.Name+"_")
	}
	prefix := server + "_"
	drop := func(key string) bool {
		if !strings.HasPrefix(key, prefix) {
			return false
		}
		for _, kp := range keepPrefixes {
			if strings.HasPrefix(key, kp) {
				return false // also claimed by a remaining sibling — keep it.
			}
		}
		return true
	}
	out := &agentcfg.ToolExposure{
		PausedServers:      dropExact(te.PausedServers, server),
		DisabledTools:      dropWhere(te.DisabledTools, drop),
		ServerLoadingModes: dropMapKey(te.ServerLoadingModes, server),
		ToolLoadingModes:   dropMapKeysWhere(te.ToolLoadingModes, drop),
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

// dropWhere returns a fresh copy of in with every element for which drop
// returns true removed (nil when the result is empty).
func dropWhere(in []string, drop func(string) bool) []string {
	var out []string
	for _, s := range in {
		if !drop(s) {
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

// dropMapKeysWhere returns a fresh copy of m without any key for which drop
// returns true (nil when the result is empty).
func dropMapKeysWhere(m map[string]string, drop func(string) bool) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for key, val := range m {
		if !drop(key) {
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
