package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// RemoveOAuthMCPCapability advances the one signed-capability pair-lifetime receipt from
// published through desired-state removal, catalog withdrawal, teardown, and
// its anti-replay tombstone. It intentionally accepts no authority envelope:
// removal is authorized by the verified admin caller and the frozen exact pair
// receipt, so expiry or verifier-key rotation can never strand a live bearer.
func (s *Service) RemoveOAuthMCPCapability(ctx context.Context, req prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest) (_ prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse, retErr error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	scope := signedOAuthMCPConfigScope(ctx)
	exactDetacher, exactOK := s.detacher.(ExactConnectionDetacher)
	exactFencer, fenceOK := s.detacher.(ExactConnectionTeardownFencer)
	if s.signedOAuthMCPOperations == nil || s.detacher == nil || !exactOK || !fenceOK {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
	}
	var subjectDetacher SubjectExactConnectionDetacher
	var subjectFencer SubjectExactConnectionTeardownFencer
	if scope == agentcfg.ConfigScopeUser {
		var ok bool
		subjectDetacher, ok = s.detacher.(SubjectExactConnectionDetacher)
		if !ok || subjectDetacher == nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
		}
		subjectFencer, ok = s.detacher.(SubjectExactConnectionTeardownFencer)
		if !ok || subjectFencer == nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
		}
	}
	q := identity.Quadruple{Identity: id}
	lockUser := ""
	if scope == agentcfg.ConfigScopeUser {
		lockUser = id.UserID
	}
	defer s.lockOwner(scope, id.TenantID, lockUser, req.AgentID)()
	expectedContentHash := strings.TrimSpace(req.ExpectedContentHash)
	if expectedContentHash == "" {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: expected_content_hash is required to identify the immutable signed pair", agentcfg.ErrRevisionConflict)
	}

	providerName := strings.TrimSpace(req.ProviderName)
	targetRevision, pair, err := s.signedCapabilityRemovalTarget(ctx, q, req.AgentID, expectedContentHash, providerName)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	if pair.OwnerAgentID != req.AgentID || pair.OwnerUserID != id.UserID ||
		(scope == agentcfg.ConfigScopeAgent && pair.OwnerSessionID != id.SessionID) {
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
		removalRevision, err := s.registry.Get(ctx, q, req.AgentID, op.RevisionID, signedOAuthMCPConfigScope(ctx))
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
		return signedCapabilityRemovalResponse(removalRevision, pair, op), nil
	}
	if op.Phase != agentcfg.SignedOAuthMCPPhasePublished && op.Phase != agentcfg.SignedOAuthMCPPhaseRemovalAdmitted && op.Phase != agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted &&
		op.Phase != agentcfg.SignedOAuthMCPPhaseCatalogUnpublished && op.Phase != agentcfg.SignedOAuthMCPPhaseTeardownReceipted {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: operation phase %q cannot be removed", agentcfg.ErrSignedCapabilityReplay, op.Phase)
	}

	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, signedOAuthMCPConfigScope(ctx))
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	if !hasActive {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: signed capability removal has no active desired revision", agentcfg.ErrSignedCapabilityReplay)
	}
	removalRevision := active
	activePair, activePairSet, pairErr := active.Payload.SignedOAuthMCPPairByProvider(pair.ProviderName)
	if pairErr != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, pairErr
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhasePublished {
		if active.RevisionID != targetRevision.RevisionID || active.ContentHash != expectedContentHash || !activePairSet ||
			activePair.AuthorityOperationKind != pair.AuthorityOperationKind {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: removal target is not the active signed pair", agentcfg.ErrRevisionConflict)
		}
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalAdmitted, targetRevision.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
	}

	descriptorFingerprint := signedCapabilityPairAttachmentFingerprint(pair, op.PublisherEpoch)
	var teardownFence ExactConnectionTeardownFence
	if scope == agentcfg.ConfigScopeUser {
		teardownFence, err = subjectFencer.BeginExactConnectionTeardownForOwner(toolauth.Owner{Tenant: id.TenantID, Agent: req.AgentID, User: id.UserID}, pair.Connection.Name, descriptorFingerprint)
	} else {
		teardownFence, err = exactFencer.BeginExactConnectionTeardown(id.TenantID, req.AgentID, pair.Connection.Name, descriptorFingerprint)
	}
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
	}
	fenceSealed := false
	defer func() {
		if !fenceSealed {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signedCapabilityCleanupTimeout)
			defer cancel()
			retErr = errors.Join(retErr, teardownFence.Cancel(cleanupCtx))
		}
	}()

	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalAdmitted {
		removalBase := targetRevision
		if activePairSet {
			// Admission freezes the pair lifetime, not unrelated sibling
			// sections. A generic writer may have carried the exact immutable
			// operation-bound pair into a newer revision while removal was
			// admitted. Preserve those current siblings and CAS that generation;
			// a foreign or replacement pair remains a hard conflict.
			if !signedCapabilityPairMatchesOperation(activePair, id.TenantID, op.Binding, pair.AuthorityOperationKind) {
				return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: removal target changed after admission", agentcfg.ErrRevisionConflict)
			}
			removalBase = active
		}
		payload := carrySiblingsForward(removalBase, true)
		payload, removed, removeErr := payload.RemoveSignedOAuthMCPPair(pair.ProviderName)
		if removeErr != nil || !removed {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, errors.Join(fmt.Errorf("%w: removal target pair is absent", agentcfg.ErrSignedCapabilityReplay), removeErr)
		}
		candidateHash, hashErr := agentcfg.ContentHash(agentcfg.NormalizePayload(payload))
		if hashErr != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, hashErr
		}
		if !activePairSet {
			if active.ContentHash != candidateHash {
				return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: removal admission does not bind active pair absence", agentcfg.ErrSignedCapabilityReplay)
			}
			removalRevision = active
		} else {
			removalCtx := agentcfg.WithSignedOAuthMCPFenceOperation(ctx, pair.AuthorityOperationKind)
			removalRevision, err = s.registry.SetRevision(removalCtx, q, req.AgentID, signedOAuthMCPConfigScope(removalCtx), payload,
				agentcfg.SetOptions{ExpectedContentHash: removalBase.ContentHash})
			if err != nil {
				// An unknown SaveIf result can have committed the exact desired removal.
				// Re-read only the pair absence, then let the receipt CAS decide whether
				// this caller is permitted to advance the lifetime graph.
				current, currentSet, readErr := s.registry.Active(context.WithoutCancel(ctx), q, req.AgentID, signedOAuthMCPConfigScope(ctx))
				if readErr == nil && currentSet && current.ContentHash == removalBase.ContentHash &&
					signedCapabilityPayloadMatchesOperation(current.Payload, id.TenantID, op.Binding, pair.AuthorityOperationKind) {
					_, rollbackErr := s.signedOAuthMCPOperations.Advance(context.WithoutCancel(ctx), op, agentcfg.SignedOAuthMCPPhasePublished, targetRevision.RevisionID)
					return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, errors.Join(err, rollbackErr)
				}
				_, currentPairSet, currentPairErr := current.Payload.SignedOAuthMCPPairByProvider(pair.ProviderName)
				if readErr != nil || !currentSet || currentPairErr != nil || currentPairSet || current.ContentHash != candidateHash {
					return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, errors.Join(err, readErr, currentPairErr)
				}
				removalRevision = current
			}
		}
		teardownFence.Seal()
		fenceSealed = true
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, removalRevision.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
	} else {
		teardownFence.Seal()
		fenceSealed = true
		removalRevision, err = s.registry.Get(ctx, q, req.AgentID, op.RevisionID, signedOAuthMCPConfigScope(ctx))
		if err != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, err
		}
		_, stillSet, stillErr := active.Payload.SignedOAuthMCPPairByProvider(pair.ProviderName)
		if stillErr != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, stillErr
		}
		if stillSet {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: stale removal cannot detach the active pair", agentcfg.ErrSignedCapabilityReplay)
		}
	}

	// Withdraw through exactly the owner-scoped detacher that owns the MCP
	// catalog source. It is idempotent; a crash after detachment is proven by
	// the subsequent receipt CAS rather than inferred from process-local state.
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted {
		var detachErr error
		if scope == agentcfg.ConfigScopeUser {
			detachErr = subjectDetacher.DetachExactConnectionForOwner(ctx, toolauth.Owner{Tenant: id.TenantID, Agent: req.AgentID, User: id.UserID}, pair.Connection.Name, descriptorFingerprint)
		} else {
			detachErr = exactDetacher.DetachExactConnection(ctx, id.TenantID, req.AgentID, pair.Connection.Name, descriptorFingerprint)
		}
		if detachErr != nil {
			return prototypes.AgentConfigRemoveOAuthMCPCapabilityResponse{}, detachErr
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

// RemoveUserOAuthMCPCapability is the user-tier sibling of
// RemoveOAuthMCPCapability. It authenticates the acting identity and signed
// agent reach before loading the immutable pair receipt, then removes only the
// caller's ConfigScopeUser desired pair and physical owner.
func (s *Service) RemoveUserOAuthMCPCapability(ctx context.Context, req prototypes.AgentConfigUserRemoveOAuthMCPCapabilityRequest) (prototypes.AgentConfigUserRemoveOAuthMCPCapabilityResponse, error) {
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserRemoveOAuthMCPCapabilityResponse{}, err
	}
	if err := requireUserSignedOAuthMCPAuthorization(ctx, id, req.AgentID); err != nil {
		return prototypes.AgentConfigUserRemoveOAuthMCPCapabilityResponse{}, err
	}
	scopedCtx := withSignedOAuthMCPConfigScope(ctx, agentcfg.ConfigScopeUser)
	response, err := s.RemoveOAuthMCPCapability(scopedCtx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: req.Identity, AgentID: req.AgentID, ProviderName: req.ProviderName,
		ExpectedContentHash: req.ExpectedContentHash,
	})
	if err != nil {
		return prototypes.AgentConfigUserRemoveOAuthMCPCapabilityResponse{}, err
	}
	return prototypes.AgentConfigUserRemoveOAuthMCPCapabilityResponse{
		Revision: response.Revision, ProviderName: response.ProviderName, ConnectionName: response.ConnectionName,
		OperationPhase: response.OperationPhase, ProtocolVersion: response.ProtocolVersion,
	}, nil
}

func (s *Service) signedCapabilityRemovalTarget(ctx context.Context, q identity.Quadruple, agentID, expectedContentHash, providerName string) (agentcfg.Revision, *agentcfg.SignedOAuthMCPPair, error) {
	history, err := s.registry.ListRevisions(ctx, q, agentID, signedOAuthMCPConfigScope(ctx), 0)
	if err != nil {
		return agentcfg.Revision{}, nil, err
	}
	for _, revision := range history {
		if revision.ContentHash != expectedContentHash {
			continue
		}
		pairs, pairErr := revision.Payload.EffectiveSignedOAuthMCPPairs()
		if pairErr != nil {
			return agentcfg.Revision{}, nil, pairErr
		}
		if providerName != "" {
			pair, ok := pairs[providerName]
			if !ok {
				return agentcfg.Revision{}, nil, fmt.Errorf("%w: expected_content_hash does not contain provider %q", agentcfg.ErrRevisionConflict, providerName)
			}
			return revision, pair, nil
		}
		if len(pairs) != 1 {
			return agentcfg.Revision{}, nil, fmt.Errorf("%w: provider_name is required when expected_content_hash contains %d signed capability pairs", agentcfg.ErrRevisionConflict, len(pairs))
		}
		for _, pair := range pairs {
			return revision, pair, nil
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
