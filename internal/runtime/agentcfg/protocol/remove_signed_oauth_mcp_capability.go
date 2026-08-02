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

// RemoveOAuthMCPCapability advances the one signed-capability pair-lifetime receipt from
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
	exactDetacher, exactOK := s.detacher.(ExactConnectionDetacher)
	if s.signedOAuthMCPOperations == nil || s.detacher == nil || !exactOK {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
	}
	q := identity.Quadruple{Identity: id}
	defer s.lockAgent(id.TenantID, req.AgentID)()
	expectedContentHash := strings.TrimSpace(req.ExpectedContentHash)
	if expectedContentHash == "" {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: expected_content_hash is required to identify the immutable signed pair", agentcfg.ErrRevisionConflict)
	}

	targetRevision, pair, err := s.signedCapabilityRemovalTarget(ctx, q, req.AgentID, expectedContentHash)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	if pair.OwnerAgentID != req.AgentID || pair.OwnerUserID != id.UserID || pair.OwnerSessionID != id.SessionID {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: pair owner does not match requested subject and agent", agentcfg.ErrSignedCapabilityReplay)
	}
	op, err := s.signedOAuthMCPOperations.LoadForPair(ctx, id.TenantID, pair)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}

	// A terminal retry is idempotent. The desired revision may already be
	// inactive after an acknowledgement-loss recovery, so return its frozen
	// revision identity only when the receipt proves the terminal operation.
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemoved {
		removalRevision, err := s.registry.Get(ctx, q, req.AgentID, op.RevisionID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
		return signedCapabilityRemovalResponse(removalRevision, pair, op), nil
	}
	if op.Phase != agentcfg.SignedOAuthMCPPhasePublished && op.Phase != agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted &&
		op.Phase != agentcfg.SignedOAuthMCPPhaseCatalogUnpublished && op.Phase != agentcfg.SignedOAuthMCPPhaseTeardownReceipted {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: operation phase %q cannot be removed", agentcfg.ErrSignedCapabilityReplay, op.Phase)
	}

	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	if !hasActive {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: signed capability removal has no active desired revision", agentcfg.ErrSignedCapabilityReplay)
	}
	removalRevision := active
	if op.Phase == agentcfg.SignedOAuthMCPPhasePublished {
		if active.RevisionID != targetRevision.RevisionID || active.ContentHash != expectedContentHash || active.Payload.SignedOAuthMCPPair == nil ||
			active.Payload.SignedOAuthMCPPair.AuthorityOperationKind != pair.AuthorityOperationKind {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: removal target is not the active signed pair", agentcfg.ErrRevisionConflict)
		}
		payload := carrySiblingsForward(active, true)
		payload.SignedOAuthMCPPair = nil
		candidateHash, hashErr := agentcfg.ContentHash(agentcfg.NormalizePayload(payload))
		if hashErr != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, hashErr
		}
		removalCtx := agentcfg.WithSignedOAuthMCPFenceOperation(ctx, pair.AuthorityOperationKind)
		removalRevision, err = s.registry.SetRevision(removalCtx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload,
			agentcfg.SetOptions{ExpectedContentHash: expectedContentHash})
		if err != nil {
			// An unknown SaveIf result can have committed the exact desired removal.
			// Re-read only the pair absence, then let the receipt CAS decide whether
			// this caller is permitted to advance the lifetime graph.
			current, currentSet, readErr := s.registry.Active(context.WithoutCancel(ctx), q, req.AgentID, agentcfg.ConfigScopeAgent)
			if readErr != nil || !currentSet || current.Payload.SignedOAuthMCPPair != nil || current.ContentHash != candidateHash {
				return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, errors.Join(err, readErr)
			}
			removalRevision = current
		}
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, removalRevision.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
	} else {
		removalRevision, err = s.registry.Get(ctx, q, req.AgentID, op.RevisionID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
		if active.Payload.SignedOAuthMCPPair != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: stale removal cannot detach the active pair", agentcfg.ErrSignedCapabilityReplay)
		}
	}

	// Withdraw through exactly the owner-scoped detacher that owns the MCP
	// catalog source. It is idempotent; a crash after detachment is proven by
	// the subsequent receipt CAS rather than inferred from process-local state.
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted {
		if err := exactDetacher.DetachExactConnection(ctx, id.TenantID, req.AgentID, pair.Connection.Name, signedCapabilityPairAttachmentFingerprint(pair)); err != nil {
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

func (s *Service) signedCapabilityRemovalTarget(ctx context.Context, q identity.Quadruple, agentID, expectedContentHash string) (agentcfg.Revision, *agentcfg.SignedOAuthMCPPair, error) {
	history, err := s.registry.ListRevisions(ctx, q, agentID, agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		return agentcfg.Revision{}, nil, err
	}
	for _, revision := range history {
		if revision.ContentHash == expectedContentHash && revision.Payload.SignedOAuthMCPPair != nil {
			return revision, revision.Payload.SignedOAuthMCPPair, nil
		}
	}
	return agentcfg.Revision{}, nil, fmt.Errorf("%w: expected_content_hash does not identify a signed capability pair revision", agentcfg.ErrRevisionConflict)
}

func signedCapabilityRemovalResponse(revision agentcfg.Revision, pair *agentcfg.SignedOAuthMCPPair, op agentcfg.SignedOAuthMCPOperation) prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse {
	return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{
		Revision: revisionToWire(revision), ProviderName: pair.ProviderName, ConnectionName: pair.Connection.Name,
		OperationPhase: string(op.Phase), ProtocolVersion: prototypes.ProtocolVersion,
	}
}
