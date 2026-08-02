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
// the registry's exact-generation deletion. A concurrent winner is preserved
// and reported as changed=false.
func restorePreOperationAuthority(ctx context.Context, registry agentcfg.Registry, q identity.Quadruple, agentID string, candidate agentcfg.Revision, priorRevisionID, operationKind string) (bool, error) {
	if candidate.RevisionID == "" || candidate.ContentHash == "" {
		return false, fmt.Errorf("%w: compensation candidate is incomplete", agentcfg.ErrRevisionNotFound)
	}
	restoreCtx := ctx
	if operationKind != "" {
		restoreCtx = agentcfg.WithSignedOAuthMCPFenceOperation(ctx, operationKind)
	}
	if priorRevisionID == "" {
		return registry.DeactivateIfActive(restoreCtx, q, agentID, candidate.RevisionID, agentcfg.ConfigScopeAgent)
	}
	if _, err := registry.Rollback(restoreCtx, q, agentID, priorRevisionID, agentcfg.ConfigScopeAgent,
		agentcfg.SetOptions{ExpectedContentHash: candidate.ContentHash}); err != nil {
		if errors.Is(err, agentcfg.ErrRevisionConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func findRevisionByContentHash(ctx context.Context, registry agentcfg.Registry, q identity.Quadruple, agentID, contentHash string) (agentcfg.Revision, bool, error) {
	history, err := registry.ListRevisions(ctx, q, agentID, agentcfg.ConfigScopeAgent, 0)
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
