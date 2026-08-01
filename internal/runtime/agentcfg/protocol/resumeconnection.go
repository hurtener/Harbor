package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// resumeMCPConnection converges one accepted auth pause. It re-reads the active
// revision under the owner lock and refuses to prepare or publish unless the
// durable non-secret fingerprint still names the exact desired descriptor.
func (s *Service) resumeMCPConnection(ctx context.Context, invocation pauseresume.ContinuationInvocation) error {
	agentID := invocation.Continuation.Data["agent_id"]
	server := invocation.Continuation.Data["server"]
	fingerprint := invocation.Continuation.Data["descriptor_fingerprint"]
	if agentID == "" || server == "" || fingerprint == "" {
		return fmt.Errorf("%w: incomplete mcp continuation", pauseresume.ErrInvalidContinuation)
	}
	release := s.lockAgent(invocation.Identity.TenantID, agentID)
	defer release()
	q := identity.Quadruple{Identity: invocation.Identity}
	rev, set, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return fmt.Errorf("read active revision: %w", err)
	}
	var desc *agentcfg.MCPConnectionDescriptor
	if set {
		connections := rev.Payload.ConnectionDescriptors()
		for i := range connections {
			candidate := &connections[i]
			if candidate.Name == server && agentcfg.MCPConnectionFingerprint(*candidate) == fingerprint {
				desc = candidate
				break
			}
		}
	}
	if desc == nil {
		// Desired state moved while authorization was outstanding. Terminalize
		// the accepted pause without touching the network or live registry.
		return nil
	}
	if s.preparer == nil {
		return ErrConnectionAttachUnavailable
	}
	owner := toolauth.Owner{Tenant: invocation.Identity.TenantID, Agent: agentID}
	if matcher, ok := s.preparer.(connectionMatcher); ok && matcher.ConnectionMatches(owner, server, fingerprint) {
		return nil
	}
	prepared, err := s.preparer.PrepareConnection(ctx, attachRequestFromDescriptor(invocation.Identity, agentID, *desc))
	if err != nil {
		return fmt.Errorf("prepare resumed mcp connection: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, closePreparedConnection(ctx, prepared))
	}
	if err := prepared.Activate(ctx); err != nil {
		return errors.Join(fmt.Errorf("activate resumed mcp connection: %w", err), closePreparedConnection(ctx, prepared))
	}
	s.emitConnectionLifecycle(ctx, q, agentID, *desc, ConnectionStateOnline, rev.RevisionID, string(invocation.Token), "")
	return nil
}

func attachRequestFromDescriptor(id identity.Identity, agentID string, desc agentcfg.MCPConnectionDescriptor) AttachRequest {
	return AttachRequest{
		Identity: id, AgentID: agentID, Name: desc.Name, Transport: desc.Transport,
		Command: append([]string(nil), desc.Command...), URL: desc.URL, OAuthProvider: desc.OAuthProvider,
		MetaAnnotations:              cloneAnnotations(desc.MetaAnnotations),
		OAuthDiscoveryAllowedOrigins: append([]string(nil), desc.OAuthDiscoveryAllowedOrigins...),
		Injection:                    desc.Injection.Clone(), ArtifactByteEligible: desc.ArtifactByteEligible,
		ArtifactParams: desc.CloneArtifactParams(),
	}
}
