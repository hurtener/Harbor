package protocol

import (
	"context"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// UserReconcileLiveProfile runs the existing signed OAuth MCP recovery
// lifecycle for the caller's durable ConfigScopeUser profile, then returns a
// fresh profile projection for immediate tool re-listing. It does not accept
// or mint any provider, descriptor, authority, JTI, tenant, or user selector.
func (s *Service) UserReconcileLiveProfile(ctx context.Context, req prototypes.AgentConfigUserReconcileLiveProfileRequest) (prototypes.AgentConfigUserReconcileLiveProfileResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserReconcileLiveProfileResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserReconcileLiveProfileResponse{}, err
	}
	if err := requireUserSignedOAuthMCPAuthorization(ctx, id, req.AgentID); err != nil {
		return prototypes.AgentConfigUserReconcileLiveProfileResponse{}, err
	}
	if s.signedOAuthMCPUserReconciler == nil {
		return prototypes.AgentConfigUserReconcileLiveProfileResponse{}, ErrSignedCapabilityUnavailable
	}
	q := identity.Quadruple{Identity: id}
	if err := s.signedOAuthMCPUserReconciler.ReconcileSignedOAuthMCPCapabilityForScope(ctx, q, req.AgentID, agentcfg.ConfigScopeUser); err != nil {
		return prototypes.AgentConfigUserReconcileLiveProfileResponse{}, err
	}
	rev, set, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return prototypes.AgentConfigUserReconcileLiveProfileResponse{}, err
	}
	resp := prototypes.AgentConfigUserReconcileLiveProfileResponse{Set: set, ProtocolVersion: prototypes.ProtocolVersion}
	if set {
		v := revisionToWire(rev)
		resp.Revision = &v
	}
	return resp, nil
}
