package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
)

// restorePreOperationAuthority conditionally restores the exact authority that
// preceded candidate. A prior revision is repointed only while candidate's
// content hash is still active; a truly absent prestate is restored through
// the registry's exact-generation deletion. A concurrent winner is preserved.
func restorePreOperationAuthority(ctx context.Context, registry agentcfg.Registry, q identity.Quadruple, agentID string, candidate agentcfg.Revision, priorRevisionID, operationKind string) error {
	if candidate.RevisionID == "" || candidate.ContentHash == "" {
		return fmt.Errorf("%w: compensation candidate is incomplete", agentcfg.ErrRevisionNotFound)
	}
	restoreCtx := ctx
	if operationKind != "" {
		restoreCtx = agentcfg.WithSignedOAuthMCPFenceOperation(ctx, operationKind)
	}
	if priorRevisionID == "" {
		_, err := registry.DeactivateIfActive(restoreCtx, q, agentID, candidate.RevisionID, signedOAuthMCPConfigScope(ctx))
		return err
	}
	if _, err := registry.Rollback(restoreCtx, q, agentID, priorRevisionID, signedOAuthMCPConfigScope(ctx),
		agentcfg.SetOptions{ExpectedContentHash: candidate.ContentHash}); err != nil {
		if errors.Is(err, agentcfg.ErrRevisionConflict) {
			return nil
		}
		return err
	}
	return nil
}

func findRevisionByContentHash(ctx context.Context, registry agentcfg.Registry, q identity.Quadruple, agentID, contentHash string) (agentcfg.Revision, bool, error) {
	history, err := registry.ListRevisions(ctx, q, agentID, signedOAuthMCPConfigScope(ctx), 0)
	if err != nil {
		return agentcfg.Revision{}, false, err
	}
	for _, revision := range history {
		if revision.ContentHash == contentHash {
			return revision, true, nil
		}
	}
	return agentcfg.Revision{}, false, nil
}

// requirePhysicalActiveRevision proves that the durable active pointer names
// the exact candidate revision. Immutable history is not authority: a failed
// pointer write may leave an unreferenced revision that ListRevisions and Get
// can still read.
func requirePhysicalActiveRevision(ctx context.Context, physical physicalActiveRegistry, q identity.Quadruple, agentID, revisionID, contentHash string) (agentcfg.Revision, error) {
	revision, set, err := physical.PhysicalActive(ctx, q, agentID, signedOAuthMCPConfigScope(ctx))
	if err != nil {
		return agentcfg.Revision{}, fmt.Errorf("%w: load physical active candidate: %w", agentcfg.ErrSignedCapabilityPending, err)
	}
	if !set || revision.RevisionID != revisionID || (contentHash != "" && revision.ContentHash != contentHash) {
		return agentcfg.Revision{}, fmt.Errorf("%w: physical active pointer does not name candidate revision %q", agentcfg.ErrSignedCapabilityPending, revisionID)
	}
	return revision, nil
}
