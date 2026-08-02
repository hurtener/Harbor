package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// retireSignedOAuthMCPPair is retirement's private signed-pair adapter. The public
// admin subject authorizes retiring the agent, but never impersonates the
// pair owner: discovery resolves the hash-only manifest locator back to one
// tenant-scoped durable receipt and all exact teardown facts come from that
// receipt's stored subject and binding.
func (s *Service) retireSignedOAuthMCPPair(ctx context.Context, tenant, agentID, resource, retirementRevisionID string) error {
	if s.signedOAuthMCPOperations == nil {
		return fmt.Errorf("%w: retirement cleanup requires signed capability operation store", ErrMisconfigured)
	}
	exactDetacher, exactOK := s.detacher.(ExactConnectionDetacher)
	exactFencer, fenceOK := s.detacher.(ExactConnectionTeardownFencer)
	if !exactOK || !fenceOK {
		return fmt.Errorf("%w: retirement cleanup requires exact signed capability teardown", ErrMisconfigured)
	}
	op, owner, err := s.findRetirementSignedOAuthMCPOperation(ctx, tenant, agentID, resource)
	if err != nil {
		return err
	}
	// The full subject stays private to this adapter. It is deliberately not
	// copied into the retirement manifest, status, event, or public request.
	if owner.TenantID != tenant || owner.UserID == "" || owner.SessionID == "" || op.Binding.AgentID != agentID {
		return fmt.Errorf("%w: signed capability retirement receipt crossed owner scope", agentcfg.ErrRetirementConflict)
	}
	ownerCtx, err := identity.With(ctx, owner.Identity)
	if err != nil {
		return fmt.Errorf("%w: restore signed capability retirement owner: %w", agentcfg.ErrRetirementConflict, err)
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemoved || op.Phase == agentcfg.SignedOAuthMCPPhaseExpiredIncomplete {
		return nil
	}
	if !agentcfg.SignedOAuthMCPRetirementPending(op.Phase) {
		return fmt.Errorf("%w: signed capability phase %q is not retirement cleanup debt", agentcfg.ErrRetirementConflict, op.Phase)
	}
	operationKind, err := s.signedOAuthMCPOperations.Kind(op.ReplayKey)
	if err != nil {
		return err
	}
	fingerprint := signedCapabilityAttachmentFingerprint(op.Binding.Connection, operationKind)
	revisionID := retirementRevisionID
	if revisionID == "" {
		revisionID = op.RevisionID
	}

	if op.Phase == agentcfg.SignedOAuthMCPPhasePublished {
		op, err = s.advanceRetirementSignedOAuthMCPOperation(ownerCtx, op, agentcfg.SignedOAuthMCPPhaseRemovalAdmitted, op.RevisionID, resource)
		if err != nil {
			return err
		}
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalAdmitted {
		// The lifecycle tombstone already made the pair non-authoritative. Seal
		// the local exact-publication fence before recording that durable fact in
		// the shared signed-pair graph; no authority envelope or current verifier is
		// consulted on this path.
		teardownFence, beginErr := exactFencer.BeginExactConnectionTeardown(tenant, agentID, op.Binding.Connection.Name, fingerprint)
		if beginErr != nil {
			return beginErr
		}
		if teardownFence == nil {
			return fmt.Errorf("%w: exact signed capability teardown returned no fence", ErrMisconfigured)
		}
		teardownFence.Seal()
		op, err = s.advanceRetirementSignedOAuthMCPOperation(ownerCtx, op, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, revisionID, resource)
		if err != nil {
			return err
		}
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted {
		if err := exactDetacher.DetachExactConnection(ownerCtx, tenant, agentID, op.Binding.Connection.Name, fingerprint); err != nil {
			return err
		}
		op, err = s.advanceRetirementSignedOAuthMCPOperation(ownerCtx, op, agentcfg.SignedOAuthMCPPhaseCatalogUnpublished, op.RevisionID, resource)
		if err != nil {
			return err
		}
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseCatalogUnpublished {
		// Exact detach owns the private retryable closing receipt. Repeating it
		// proves close+revoke before teardown_receipted; a close failure leaves
		// both the signed-pair phase and retirement manifest item unacknowledged.
		if err := exactDetacher.DetachExactConnection(ownerCtx, tenant, agentID, op.Binding.Connection.Name, fingerprint); err != nil {
			return err
		}
		op, err = s.advanceRetirementSignedOAuthMCPOperation(ownerCtx, op, agentcfg.SignedOAuthMCPPhaseTeardownReceipted, op.RevisionID, resource)
		if err != nil {
			return err
		}
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseTeardownReceipted {
		_, err = s.advanceRetirementSignedOAuthMCPOperation(ownerCtx, op, agentcfg.SignedOAuthMCPPhaseRemoved, op.RevisionID, resource)
	}
	return err
}

func (s *Service) findRetirementSignedOAuthMCPOperation(ctx context.Context, tenant, agentID, resource string) (agentcfg.SignedOAuthMCPOperation, identity.Quadruple, error) {
	continuation := ""
	var match *agentcfg.SignedOAuthMCPOperation
	for {
		page, next, err := s.signedOAuthMCPOperations.ScanTenantPage(ctx, tenant, state.MaxStateScanLimit, continuation)
		if err != nil {
			return agentcfg.SignedOAuthMCPOperation{}, identity.Quadruple{}, err
		}
		for i := range page {
			op := page[i]
			if op.Binding.AgentID != agentID || !agentcfg.SignedOAuthMCPRetirementResourceMatches(resource, op) {
				continue
			}
			if match != nil {
				return agentcfg.SignedOAuthMCPOperation{}, identity.Quadruple{}, fmt.Errorf("%w: signed capability retirement locator is ambiguous", agentcfg.ErrRetirementConflict)
			}
			copy := op
			match = &copy
		}
		if next == "" {
			break
		}
		continuation = next
	}
	if match == nil {
		return agentcfg.SignedOAuthMCPOperation{}, identity.Quadruple{}, fmt.Errorf("%w: signed capability retirement locator is absent or foreign", agentcfg.ErrRetirementConflict)
	}
	owner := identity.Quadruple{Identity: identity.Identity{TenantID: match.Binding.TenantID, UserID: match.Binding.UserID, SessionID: match.Binding.SessionID}}
	return *match, owner, nil
}

func (s *Service) advanceRetirementSignedOAuthMCPOperation(ctx context.Context, current agentcfg.SignedOAuthMCPOperation, next agentcfg.SignedOAuthMCPOperationPhase, revisionID, resource string) (agentcfg.SignedOAuthMCPOperation, error) {
	advanced, err := s.signedOAuthMCPOperations.Advance(ctx, current, next, revisionID)
	if err == nil {
		return advanced, nil
	}
	latest, loadErr := s.signedOAuthMCPOperations.Load(context.WithoutCancel(ctx), current.ReplayKey)
	if loadErr == nil && agentcfg.SignedOAuthMCPRetirementResourceMatches(resource, latest) && signedOAuthMCPRetirementPhaseAtLeast(latest.Phase, next) {
		return latest, nil
	}
	return agentcfg.SignedOAuthMCPOperation{}, errors.Join(err, loadErr)
}

func signedOAuthMCPRetirementPhaseAtLeast(got, want agentcfg.SignedOAuthMCPOperationPhase) bool {
	rank := func(phase agentcfg.SignedOAuthMCPOperationPhase) int {
		switch phase {
		case agentcfg.SignedOAuthMCPPhasePublished:
			return 1
		case agentcfg.SignedOAuthMCPPhaseRemovalAdmitted:
			return 2
		case agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted:
			return 3
		case agentcfg.SignedOAuthMCPPhaseCatalogUnpublished:
			return 4
		case agentcfg.SignedOAuthMCPPhaseTeardownReceipted:
			return 5
		case agentcfg.SignedOAuthMCPPhaseRemoved:
			return 6
		default:
			return 0
		}
	}
	return rank(want) > 0 && rank(got) >= rank(want)
}
