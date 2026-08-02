package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// RemoveOAuthMCPCapability advances the one D-401 pair-lifetime receipt from
// published through desired-state removal, catalog withdrawal, teardown, and
// its anti-replay tombstone. It intentionally accepts no authority envelope:
// removal is authorized by the verified admin caller and the frozen exact pair
// receipt, so expiry or verifier-key rotation can never strand a live bearer.
func (s *Service) RemoveOAuthMCPCapability(ctx context.Context, req prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest) (prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	if s.signedOAuthMCPOperations == nil || s.detacher == nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
	}
	q := identity.Quadruple{Identity: id}
	defer s.lockAgent(id.TenantID, req.AgentID)()

	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	if !hasActive {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: no signed capability pair history is active", agentcfg.ErrSignedCapabilityReplay)
	}
	pair := active.Payload.SignedOAuthMCPPair
	if pair == nil {
		// After desired-state removal, the active revision deliberately contains
		// no pair. Immutable history is the only safe source for a terminal
		// retry; it must still bind the exact same owner and receipt.
		history, listErr := s.registry.ListRevisions(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, 0)
		if listErr != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, listErr
		}
		for _, revision := range history {
			if revision.Payload.SignedOAuthMCPPair != nil {
				pair = revision.Payload.SignedOAuthMCPPair
				break
			}
		}
		if pair == nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: no signed capability pair history", agentcfg.ErrSignedCapabilityReplay)
		}
	}
	if pair.OwnerAgentID != req.AgentID {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: pair owner does not match requested agent", agentcfg.ErrSignedCapabilityReplay)
	}
	op, err := s.signedOAuthMCPOperations.LoadForPair(ctx, id.TenantID, pair)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}

	// A terminal retry is idempotent. The desired revision may already be
	// inactive after an acknowledgement-loss recovery, so return its frozen
	// revision identity only when the receipt proves the terminal operation.
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemoved {
		return signedCapabilityRemovalResponse(active, pair, op), nil
	}
	if op.Phase != agentcfg.SignedOAuthMCPPhasePublished && op.Phase != agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted &&
		op.Phase != agentcfg.SignedOAuthMCPPhaseCatalogUnpublished && op.Phase != agentcfg.SignedOAuthMCPPhaseTeardownReceipted {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: operation phase %q cannot be removed", agentcfg.ErrSignedCapabilityReplay, op.Phase)
	}

	removalRevision := active
	if op.Phase == agentcfg.SignedOAuthMCPPhasePublished {
		if active.Payload.SignedOAuthMCPPair == nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: published receipt has no active pair", agentcfg.ErrSignedCapabilityReplay)
		}
		payload := carrySiblingsForward(active, true)
		payload.SignedOAuthMCPPair = nil
		removalRevision, err = s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload,
			agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
		if err != nil {
			// An unknown SaveIf result can have committed the exact desired removal.
			// Re-read only the pair absence, then let the receipt CAS decide whether
			// this caller is permitted to advance the lifetime graph.
			current, currentSet, readErr := s.registry.Active(context.WithoutCancel(ctx), q, req.AgentID, agentcfg.ConfigScopeAgent)
			if readErr != nil || !currentSet || current.Payload.SignedOAuthMCPPair != nil {
				return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
			}
			removalRevision = current
		}
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, removalRevision.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
	}

	// Withdraw through exactly the owner-scoped detacher that owns the MCP
	// catalog source. It is idempotent; a crash after detachment is proven by
	// the subsequent receipt CAS rather than inferred from process-local state.
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted {
		if err := s.detacher.DetachConnection(ctx, id.TenantID, req.AgentID, pair.Connection.Name); err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseCatalogUnpublished, removalRevision.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseCatalogUnpublished {
		// The signed provider is private to the prepared MCP connection. Its
		// owner-scoped detacher closes that connection and its provider binding as
		// one receipt; no generic ProviderSet operation can observe or replace it.
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseTeardownReceipted, removalRevision.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseTeardownReceipted {
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRemoved, removalRevision.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
	}
	return signedCapabilityRemovalResponse(removalRevision, pair, op), nil
}

func signedCapabilityRemovalResponse(revision agentcfg.Revision, pair *agentcfg.SignedOAuthMCPPair, op agentcfg.SignedOAuthMCPOperation) prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse {
	return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{
		Revision: revisionToWire(revision), ProviderName: pair.ProviderName, ConnectionName: pair.Connection.Name,
		OperationPhase: string(op.Phase), ProtocolVersion: prototypes.ProtocolVersion,
	}
}
