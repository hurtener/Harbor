package protocol

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// mcppolicy.go — the agent-config MCP-exposure / per-tool-policy control
// method: `agent_config.set_tool_exposure`. It is a consumer of the
// registry primitive (CLAUDE.md §13) — a tool-exposure edit composes a new
// config revision (so MCP pause/resume + per-tool disable inherit the
// unified diff + rollback) and emits the `mcp.connection.paused` /
// `.resumed` overlay events for each server whose pause state flipped.
//
// Desired-state replace semantics: a set replaces ONLY the tool-exposure
// section of the envelope; the skills + prompt-layer + connections +
// llm-params + hooks sections of the active revision are preserved (a
// tool-exposure edit never clears an agent's skills or a pinned
// run-completion hook — CLAUDE.md §13). The edit applies NEXT-turn (the
// run-start projection reads the active revision; the live transport stays
// warm — pause is projection-time, not a teardown).

// SetToolExposure records a new config revision pinning the supplied
// MCP-exposure desired state (paused servers + disabled tools) and emits an
// `mcp.connection.paused` / `.resumed` event for each server whose pause
// state changed relative to the prior active revision. The skills +
// prompt-layer + connections + llm-params + hooks sections of the active
// revision are carried forward unchanged — every sibling section survives a
// tool-exposure edit.
func (s *Service) SetToolExposure(ctx context.Context, req prototypes.AgentConfigSetToolExposureRequest) (prototypes.AgentConfigSetToolExposureResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetToolExposureResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetToolExposureResponse{}, err
	}
	// Validate the loading-mode override maps at set time so an unknown
	// value can NEVER be persisted — checked BEFORE the active-revision
	// read / any registry write: no revision, no event.
	if err := validateToolExposureLoading(&req.ToolExposure); err != nil {
		return prototypes.AgentConfigSetToolExposureResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	defer s.lockAgent(id.TenantID, req.AgentID)()

	// Read the active revision so the new revision PRESERVES the skills +
	// prompt sections (a tool-exposure edit replaces only its own section)
	// and so the paused/resumed delta is computed against current state.
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigSetToolExposureResponse{}, err
	}
	var prior agentcfg.ConfigPayload
	payload := agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{
			PausedServers:      append([]string(nil), req.ToolExposure.PausedServers...),
			DisabledTools:      append([]string(nil), req.ToolExposure.DisabledTools...),
			ServerLoadingModes: cloneAnnotations(req.ToolExposure.ServerLoadingModes),
			ToolLoadingModes:   cloneAnnotations(req.ToolExposure.ToolLoadingModes),
		},
	}
	if hasActive {
		prior = active.Payload
		payload.Skills = active.Payload.Skills
		payload.PromptLayers = active.Payload.PromptLayers
		payload.Connections = active.Payload.Connections
		payload.LLMParams = active.Payload.LLMParams
		payload.Hooks = active.Payload.Hooks
		payload.Naming = active.Payload.Naming
		// The ordered additive prompt blocks are a sibling section like any
		// other: this verb replaces only its own, so the blocks survive.
		payload.ExtraSystemBlocks = active.Payload.ExtraSystemBlocks
		payload.OAuthProviders = active.Payload.OAuthProviders
		payload.SignedOAuthMCPPair = active.Payload.SignedOAuthMCPPair
		payload.SignedOAuthMCPPairs = active.Payload.SignedOAuthMCPPairs
	}

	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload,
		agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
	if err != nil {
		return prototypes.AgentConfigSetToolExposureResponse{}, err
	}

	// Emit the per-server pause/resume overlay events for the servers whose
	// pause state flipped (diff against the prior active revision, using the
	// NORMALISED stored revision so the delta is exact even on an idempotent
	// re-set, which returns no delta). Diff is the one canonical helper.
	s.emitConnectionEvents(ctx, q, req.AgentID, rev, agentcfg.DiffToolExposure(prior, rev.Payload))

	return prototypes.AgentConfigSetToolExposureResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// emitConnectionEvents publishes one `mcp.connection.paused` event per
// newly-paused server and one `mcp.connection.resumed` per newly-resumed
// server. A nil bus is a no-op (the revision is still recorded; the generic
// `agent.config.revised` already fired from the registry). A publish failure
// is logged loud — a privileged config mutation never goes to the bus
// silently (CLAUDE.md §13) — but does not undo the recorded revision.
func (s *Service) emitConnectionEvents(ctx context.Context, q identity.Quadruple, agentID string, rev agentcfg.Revision, diff agentcfg.ToolExposureDiff) {
	if s.bus == nil {
		return
	}
	now := s.now().UTC()
	for _, server := range diff.PausedAdded {
		s.publishConnectionEvent(ctx, agentcfg.EventTypeMCPConnectionPaused, q, agentID, server, rev.RevisionID, now,
			agentcfg.MCPConnectionPausedPayload{
				Author: q, AgentID: agentID, ServerID: server, RevisionID: rev.RevisionID, OccurredAt: now,
			})
	}
	for _, server := range diff.PausedResumed {
		s.publishConnectionEvent(ctx, agentcfg.EventTypeMCPConnectionResumed, q, agentID, server, rev.RevisionID, now,
			agentcfg.MCPConnectionResumedPayload{
				Author: q, AgentID: agentID, ServerID: server, RevisionID: rev.RevisionID, OccurredAt: now,
			})
	}
}

// publishConnectionEvent publishes one mcp.connection.* event and logs a
// publish failure loud (never silently dropped).
func (s *Service) publishConnectionEvent(ctx context.Context, t events.EventType, q identity.Quadruple, agentID, server, revisionID string, now time.Time, payload events.SafePayload) {
	ev := events.Event{Type: t, Identity: q, OccurredAt: now, Payload: payload}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.logger.WarnContext(ctx, "agent-config: publish mcp.connection event failed",
			"event_type", string(t), "agent_id", agentID, "server_id", server,
			"revision_id", revisionID, "error", err.Error())
	}
}
