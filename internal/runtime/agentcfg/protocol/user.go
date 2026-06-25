package protocol

import (
	"context"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// user.go — the user-tier `agent_config.user.*` verb family: the middle tier
// of the agent-config authorization matrix and the agent-config band's ONE
// durable user-scope WRITE surface. A non-admin caller carrying the verified
// auth.ScopeAgentConfigUser claim owns a durable, versioned safe-subset
// config variant keyed under their REAL (tenant, user) (the registry's
// ConfigScopeUser arm), with full diff/rollback parity to the admin registry.
//
// # The structural widening boundary
//
// The verb maps the bounded AgentConfigUserPayload onto a RESTRICTED
// ConfigPayload: UserPrompt → PromptLayers.User (Base stays nil),
// DisabledServers/DisabledTools → ToolExposure (PausedServers/DisabledTools),
// PersonalSkills → Skills.Names; Base, Connections, and LLMParams stay nil.
// Because the wire payload carries no base / connections / enable / model
// field AND the mapping never populates those sections, a user caller
// physically cannot widen a capability or edit the operator base — the
// restricted mapping is the structural boundary, the scope gate is
// defence-in-depth.
//
// Authority derives from the verified ctx scope (enforced at the wire
// handler), never the request body.

// userPayloadToDomain maps the bounded safe-subset wire payload onto the
// restricted domain ConfigPayload. It populates ONLY the prompt user layer,
// the tool-exposure disables, and the skills membership — never Base,
// Connections, or LLMParams (the structural widening boundary).
func userPayloadToDomain(p prototypes.AgentConfigUserPayload) agentcfg.ConfigPayload {
	var out agentcfg.ConfigPayload
	if p.UserPrompt != "" {
		user := p.UserPrompt
		out.PromptLayers = &agentcfg.PromptLayers{User: &user}
	}
	if len(p.DisabledServers) > 0 || len(p.DisabledTools) > 0 {
		out.ToolExposure = &agentcfg.ToolExposure{
			PausedServers: append([]string(nil), p.DisabledServers...),
			DisabledTools: append([]string(nil), p.DisabledTools...),
		}
	}
	if len(p.PersonalSkills) > 0 {
		out.Skills = &agentcfg.SkillsSelection{Names: append([]string(nil), p.PersonalSkills...)}
	}
	return out
}

// UserGet reads the caller's own durable config variant active revision.
func (s *Service) UserGet(ctx context.Context, req prototypes.AgentConfigUserGetRequest) (prototypes.AgentConfigUserGetResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserGetResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserGetResponse{}, err
	}
	rev, set, err := s.registry.Active(ctx, identity.Quadruple{Identity: id}, req.AgentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return prototypes.AgentConfigUserGetResponse{}, err
	}
	resp := prototypes.AgentConfigUserGetResponse{Set: set, ProtocolVersion: prototypes.ProtocolVersion}
	if set {
		v := revisionToWire(rev)
		resp.Revision = &v
	}
	return resp, nil
}

// UserSetRevision writes a new immutable revision of the caller's durable
// variant from the bounded safe-subset payload and advances the active
// pointer. Serialised per-owner (scope, tenant, real-user, agent) so distinct
// users never contend.
func (s *Service) UserSetRevision(ctx context.Context, req prototypes.AgentConfigUserSetRevisionRequest) (prototypes.AgentConfigUserSetRevisionResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserSetRevisionResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserSetRevisionResponse{}, err
	}
	defer s.lockOwner(agentcfg.ConfigScopeUser, id.TenantID, id.UserID, req.AgentID)()
	rev, err := s.registry.SetRevision(ctx, identity.Quadruple{Identity: id}, req.AgentID, agentcfg.ConfigScopeUser, userPayloadToDomain(req.Payload))
	if err != nil {
		return prototypes.AgentConfigUserSetRevisionResponse{}, err
	}
	return prototypes.AgentConfigUserSetRevisionResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// UserListRevisions returns the caller's own variant revision chain,
// newest-first.
func (s *Service) UserListRevisions(ctx context.Context, req prototypes.AgentConfigUserListRevisionsRequest) (prototypes.AgentConfigUserListRevisionsResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserListRevisionsResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserListRevisionsResponse{}, err
	}
	revs, err := s.registry.ListRevisions(ctx, identity.Quadruple{Identity: id}, req.AgentID, agentcfg.ConfigScopeUser, req.Limit)
	if err != nil {
		return prototypes.AgentConfigUserListRevisionsResponse{}, err
	}
	out := make([]prototypes.AgentConfigRevisionView, 0, len(revs))
	for _, r := range revs {
		out = append(out, revisionToWire(r))
	}
	return prototypes.AgentConfigUserListRevisionsResponse{
		Revisions:       out,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// UserDiff returns the server-side compare of two existing revisions of the
// caller's own variant.
func (s *Service) UserDiff(ctx context.Context, req prototypes.AgentConfigUserDiffRequest) (prototypes.AgentConfigUserDiffResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserDiffResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserDiffResponse{}, err
	}
	d, err := s.registry.Diff(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.FromRevision, req.ToRevision, agentcfg.ConfigScopeUser)
	if err != nil {
		return prototypes.AgentConfigUserDiffResponse{}, err
	}
	return prototypes.AgentConfigUserDiffResponse{
		Diff:            diffToWire(d),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// UserRollback repoints the caller's own variant active pointer to an
// existing revision WITHOUT mutating any revision. Serialised per-owner.
func (s *Service) UserRollback(ctx context.Context, req prototypes.AgentConfigUserRollbackRequest) (prototypes.AgentConfigUserRollbackResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserRollbackResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserRollbackResponse{}, err
	}
	defer s.lockOwner(agentcfg.ConfigScopeUser, id.TenantID, id.UserID, req.AgentID)()
	rev, err := s.registry.Rollback(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.RevisionID, agentcfg.ConfigScopeUser)
	if err != nil {
		return prototypes.AgentConfigUserRollbackResponse{}, err
	}
	return prototypes.AgentConfigUserRollbackResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}
