package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// SignedOAuthMCPReconciler resumes only durable signed-capability pair operations for one
// exact tenant and agent. It deliberately enumerates revision history rather
// than operation records: an opaque operation receipt alone is not authority
// to attach or detach anything.
type SignedOAuthMCPReconciler struct {
	registry      agentcfg.Registry
	physical      physicalActiveRegistry
	operations    *agentcfg.SignedOAuthMCPOperationStore
	fences        *agentcfg.SignedOAuthMCPActivationFenceStore
	preparer      ConnectionPreparer
	detacher      ConnectionDetacher
	exactDetacher ExactConnectionDetacher
	providers     SignedCapabilityProviderPreparer
	matcher       connectionMatcher
	gate          chan struct{}
	// continuations is internally synchronized by gate; each exact subject and
	// agent advances a bounded tenant maintenance page independently.
	continuations map[string]string
}

type physicalActiveRegistry interface {
	PhysicalActive(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
}

// NewSignedOAuthMCPReconciler constructs the single recovery seam shared by
// boot and run-start. Every side-effecting dependency is mandatory so an
// incomplete runtime fails closed instead of guessing at live state.
func NewSignedOAuthMCPReconciler(registry agentcfg.Registry, store state.StateStore, preparer ConnectionPreparer, detacher ConnectionDetacher, providers SignedCapabilityProviderPreparer) (*SignedOAuthMCPReconciler, error) {
	if registry == nil || store == nil || preparer == nil || detacher == nil || providers == nil {
		return nil, fmt.Errorf("%w: signed capability reconciler missing dependency", ErrSignedCapabilityUnavailable)
	}
	matcher, ok := preparer.(connectionMatcher)
	if !ok || matcher == nil {
		return nil, fmt.Errorf("%w: signed capability reconciler needs exact connection matcher", ErrSignedCapabilityUnavailable)
	}
	physical, ok := registry.(physicalActiveRegistry)
	if !ok || physical == nil {
		return nil, fmt.Errorf("%w: signed capability reconciler needs physical active-pointer recovery", ErrSignedCapabilityUnavailable)
	}
	exactDetacher, ok := detacher.(ExactConnectionDetacher)
	if !ok || exactDetacher == nil {
		return nil, fmt.Errorf("%w: signed capability reconciler needs exact detacher", ErrSignedCapabilityUnavailable)
	}
	operations, err := agentcfg.NewSignedOAuthMCPOperationStore(store)
	if err != nil {
		return nil, err
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(store)
	if err != nil {
		return nil, err
	}
	return &SignedOAuthMCPReconciler{registry: registry, physical: physical, operations: operations, fences: fences, preparer: preparer, detacher: detacher, exactDetacher: exactDetacher, providers: providers, matcher: matcher, gate: make(chan struct{}, 1), continuations: make(map[string]string)}, nil
}

// ReconcileSignedOAuthMCPCapability converges a single exact agent slot. A
// foreign/corrupt/expired receipt is never dispatched: it is returned to the
// caller, leaving the fence's prior active revision authoritative.
func (r *SignedOAuthMCPReconciler) ReconcileSignedOAuthMCPCapability(ctx context.Context, q identity.Quadruple, agentID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if strings.TrimSpace(q.TenantID) == "" || strings.TrimSpace(q.UserID) == "" || strings.TrimSpace(q.SessionID) == "" || strings.TrimSpace(agentID) == "" {
		return ErrIdentityRequired
	}
	active, hasActive, err := r.physical.PhysicalActive(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return fmt.Errorf("load physical active signed capability revision: %w", err)
	}
	activeOperationKind := ""
	if hasActive && active.Payload.SignedOAuthMCPPair != nil {
		activeOperationKind = active.Payload.SignedOAuthMCPPair.AuthorityOperationKind
		if err := r.reconcilePair(ctx, q, agentID, active, active.Payload.SignedOAuthMCPPair); err != nil {
			return err
		}
	}
	cursorKey := strings.Join([]string{q.TenantID, q.UserID, q.SessionID, agentID}, "\x00")
	ops, continuation, err := r.operations.ScanTenantPage(ctx, q.TenantID, state.MaxStateScanLimit, r.continuations[cursorKey])
	if err != nil {
		return err
	}
	r.continuations[cursorKey] = continuation
	for _, op := range ops {
		if op.Binding.UserID != q.UserID || op.Binding.SessionID != q.SessionID || op.Binding.AgentID != agentID {
			continue
		}
		kind, kindErr := r.operations.Kind(op.ReplayKey)
		if kindErr != nil {
			return kindErr
		}
		if kind == activeOperationKind {
			continue
		}
		switch op.Phase {
		case agentcfg.SignedOAuthMCPPhaseClaimed:
			if signedCapabilityOperationExpired(op) {
				if err := r.expireIncomplete(ctx, q, agentID, op, agentcfg.Revision{}, false); err != nil {
					return err
				}
			}
		case agentcfg.SignedOAuthMCPPhaseRevisionCommitted:
			if signedCapabilityOperationExpired(op) {
				revision, getErr := r.registry.Get(ctx, q, agentID, op.RevisionID, agentcfg.ConfigScopeAgent)
				if getErr != nil {
					return getErr
				}
				if err := r.expireIncomplete(ctx, q, agentID, op, revision, true); err != nil {
					return err
				}
			}
		case agentcfg.SignedOAuthMCPPhasePublished:
			// A published historical receipt is never authority to reattach. If
			// the current physical desired state has no pair, the paired removal
			// revision landed before its receipt checkpoint; continue teardown.
			if hasActive && active.Payload.SignedOAuthMCPPair == nil {
				op, err = r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, active.RevisionID)
				if err != nil {
					return err
				}
				if err := r.resumeRemoval(ctx, q, agentID, op); err != nil {
					return err
				}
			}
		case agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, agentcfg.SignedOAuthMCPPhaseCatalogUnpublished, agentcfg.SignedOAuthMCPPhaseTeardownReceipted:
			if hasActive && active.Payload.SignedOAuthMCPPair != nil {
				return fmt.Errorf("%w: stale removal cannot detach the active pair", agentcfg.ErrSignedCapabilityReplay)
			}
			if err := r.resumeRemoval(ctx, q, agentID, op); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *SignedOAuthMCPReconciler) reconcilePair(ctx context.Context, q identity.Quadruple, agentID string, revision agentcfg.Revision, pair *agentcfg.SignedOAuthMCPPair) error {
	if pair.OwnerAgentID != agentID || pair.OwnerUserID != q.UserID || pair.OwnerSessionID != q.SessionID || strings.TrimSpace(pair.Connection.Name) == "" {
		return fmt.Errorf("%w: foreign or incomplete signed pair", agentcfg.ErrSignedCapabilityReplay)
	}
	canonicalURL, sink, err := agentcfg.CanonicalOAuthMCPURL(pair.Connection.URL)
	if err != nil || canonicalURL != pair.Connection.URL || sink != pair.Sink || pair.URLDigest != agentcfg.OAuthMCPURLDigest(canonicalURL) || pair.SinkDigest != agentcfg.OAuthMCPURLDigest(sink) {
		return fmt.Errorf("%w: corrupt signed pair descriptor", agentcfg.ErrSignedCapabilityReplay)
	}
	canonicalScopes, err := agentcfg.CanonicalScopes(pair.Scopes)
	if err != nil || !equalStrings(canonicalScopes, pair.Scopes) || strings.TrimSpace(pair.ProviderName) == "" || strings.TrimSpace(pair.Broker) == "" || strings.TrimSpace(pair.Audience) == "" || strings.TrimSpace(pair.CapabilityRevision) == "" {
		return fmt.Errorf("%w: corrupt signed pair binding", agentcfg.ErrSignedCapabilityReplay)
	}
	op, err := r.operations.LoadForPair(ctx, q.TenantID, pair)
	if err != nil {
		return err
	}
	kind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return err
	}
	if pair.AuthorityOperationKind != kind || pair.AuthorityJTIHash != signedCapabilityJTIHash(op.ReplayKey.JTI) {
		return fmt.Errorf("%w: pair receipt does not bind its exact authority", agentcfg.ErrSignedCapabilityReplay)
	}
	if (op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed || op.Phase == agentcfg.SignedOAuthMCPPhaseRevisionCommitted || op.Phase == agentcfg.SignedOAuthMCPPhasePublished) && op.RevisionID != "" && op.RevisionID != revision.RevisionID {
		return fmt.Errorf("%w: receipt names another revision", agentcfg.ErrSignedCapabilityReplay)
	}
	switch op.Phase {
	case agentcfg.SignedOAuthMCPPhaseClaimed, agentcfg.SignedOAuthMCPPhaseRevisionCommitted:
		if signedCapabilityOperationExpired(op) {
			return r.expireIncomplete(ctx, q, agentID, op, revision, true)
		}
		fence, err := r.validPendingFence(ctx, q.TenantID, agentID, op, revision)
		if err != nil {
			return err
		}
		if op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed {
			op, err = r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, revision.RevisionID)
			if err != nil {
				return err
			}
		}
		if err := r.publish(ctx, q, agentID, pair, op); err != nil {
			return err
		}
		latest, err := r.operations.LoadForPair(ctx, q.TenantID, pair)
		if err != nil {
			return err
		}
		if latest.Phase != agentcfg.SignedOAuthMCPPhasePublished {
			return fmt.Errorf("%w: publication did not converge", agentcfg.ErrSignedCapabilityPending)
		}
		_, err = r.fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceCommitted, revision.RevisionID)
		if errors.Is(err, agentcfg.ErrSignedCapabilityTransition) {
			return r.commitFence(ctx, q.TenantID, agentID, latest, revision)
		}
		return err
	case agentcfg.SignedOAuthMCPPhasePublished:
		if err := r.commitFence(ctx, q.TenantID, agentID, op, revision); err != nil {
			return err
		}
		return r.ensureAttached(ctx, q, agentID, pair)
	case agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted:
		if err := r.detach(ctx, q, agentID, pair); err != nil {
			return err
		}
		_, err := r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseCatalogUnpublished, revision.RevisionID)
		return err
	case agentcfg.SignedOAuthMCPPhaseCatalogUnpublished:
		if err := r.detach(ctx, q, agentID, pair); err != nil {
			return err
		}
		_, err := r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseTeardownReceipted, revision.RevisionID)
		return err
	case agentcfg.SignedOAuthMCPPhaseTeardownReceipted:
		_, err := r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRemoved, revision.RevisionID)
		return err
	case agentcfg.SignedOAuthMCPPhaseRemoved, agentcfg.SignedOAuthMCPPhaseExpiredIncomplete:
		return nil
	default:
		return fmt.Errorf("%w: unknown signed operation phase", agentcfg.ErrSignedCapabilityReplay)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func nowUTC() time.Time { return time.Now().UTC() }

func signedCapabilityOperationExpired(op agentcfg.SignedOAuthMCPOperation) bool {
	return !op.ExpiresAt.Add(agentcfg.SignedOAuthMCPAuthorityClockSkew).After(nowUTC())
}

func (r *SignedOAuthMCPReconciler) validPendingFence(ctx context.Context, tenant, agentID string, op agentcfg.SignedOAuthMCPOperation, revision agentcfg.Revision) (agentcfg.SignedOAuthMCPActivationFence, error) {
	fence, err := r.fences.Load(ctx, tenant, agentID)
	if err != nil {
		return agentcfg.SignedOAuthMCPActivationFence{}, err
	}
	kind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return agentcfg.SignedOAuthMCPActivationFence{}, err
	}
	if fence.Phase != agentcfg.SignedOAuthMCPFencePending || fence.OperationKind != kind || fence.Fingerprint != op.Fingerprint || fence.CandidateContentHash != revision.ContentHash || (fence.CandidateRevisionID != "" && fence.CandidateRevisionID != revision.RevisionID) {
		return agentcfg.SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: pending fence does not bind candidate", agentcfg.ErrSignedCapabilityPending)
	}
	return fence, nil
}

func (r *SignedOAuthMCPReconciler) commitFence(ctx context.Context, tenant, agentID string, op agentcfg.SignedOAuthMCPOperation, revision agentcfg.Revision) error {
	fence, err := r.fences.Load(ctx, tenant, agentID)
	if err != nil {
		return err
	}
	kind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return err
	}
	if fence.OperationKind != kind || fence.Fingerprint != op.Fingerprint || fence.CandidateContentHash != revision.ContentHash {
		return fmt.Errorf("%w: fence does not bind published pair", agentcfg.ErrSignedCapabilityPending)
	}
	if fence.Phase == agentcfg.SignedOAuthMCPFenceCommitted {
		if fence.CandidateRevisionID == revision.RevisionID {
			return nil
		}
		return fmt.Errorf("%w: committed fence names another revision", agentcfg.ErrSignedCapabilityPending)
	}
	if fence.Phase != agentcfg.SignedOAuthMCPFencePending {
		return fmt.Errorf("%w: aborted fence", agentcfg.ErrSignedCapabilityPending)
	}
	_, err = r.fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceCommitted, revision.RevisionID)
	return err
}

func (r *SignedOAuthMCPReconciler) publish(ctx context.Context, q identity.Quadruple, agentID string, pair *agentcfg.SignedOAuthMCPPair, op agentcfg.SignedOAuthMCPOperation) error {
	if err := r.ensureAttached(ctx, q, agentID, pair); err != nil {
		return err
	}
	_, err := r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhasePublished, op.RevisionID)
	if errors.Is(err, agentcfg.ErrSignedCapabilityTransition) {
		return nil
	}
	return err
}

func (r *SignedOAuthMCPReconciler) ensureAttached(ctx context.Context, q identity.Quadruple, agentID string, pair *agentcfg.SignedOAuthMCPPair) error {
	owner := toolauth.Owner{Tenant: q.TenantID, Agent: agentID}
	fingerprint := agentcfg.SignedOAuthMCPConnectionFingerprint(pair.Connection)
	if r.matcher.ConnectionMatches(owner, pair.Connection.Name, fingerprint) {
		return nil
	}
	binding := agentcfg.SignedOAuthMCPBinding{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID, AgentID: agentID, Broker: pair.Broker, ProviderName: pair.ProviderName,
		CapabilityRevision: pair.CapabilityRevision, URLDigest: pair.URLDigest, SinkDigest: pair.SinkDigest,
		Audience: pair.Audience, Scopes: pair.Scopes, Connection: pair.Connection}
	provider, err := r.providers.PrepareSignedCapabilityProvider(ctx, pair.Broker, signedCapabilityExchangeBinding(binding, pair.Sink), pair.Scopes)
	if err != nil {
		return err
	}
	attachCtx := tools.WithInvokingAgent(ctx, agentID)
	prepared, err := r.preparer.PrepareConnection(attachCtx, AttachRequest{Identity: q.Identity, AgentID: agentID, Name: pair.Connection.Name,
		Transport: agentcfg.MCPTransportHTTP, URL: pair.Connection.URL, OAuthProvider: pair.ProviderName,
		OAuthProviderOverride: provider.Binding(), OwnOAuthProvider: true,
		ToolAllowlist: pair.Connection.ToolAllowlist, ToolDenylist: pair.Connection.ToolDenylist,
		ConnectTimeoutMS: pair.Connection.ConnectTimeoutMS, RequestTimeoutMS: pair.Connection.RequestTimeoutMS,
		DescriptorFingerprint: fingerprint})
	if err != nil {
		_ = provider.Close(context.WithoutCancel(ctx))
		return err
	}
	if err := prepared.Activate(ctx); err != nil {
		_ = prepared.Close(context.WithoutCancel(ctx))
		_ = provider.Close(context.WithoutCancel(ctx))
		return err
	}
	provider.Commit(ctx)
	return nil
}

func (r *SignedOAuthMCPReconciler) detach(ctx context.Context, q identity.Quadruple, agentID string, pair *agentcfg.SignedOAuthMCPPair) error {
	return r.exactDetacher.DetachExactConnection(ctx, q.TenantID, agentID, pair.Connection.Name, agentcfg.SignedOAuthMCPConnectionFingerprint(pair.Connection))
}

func (r *SignedOAuthMCPReconciler) resumeRemoval(ctx context.Context, q identity.Quadruple, agentID string, op agentcfg.SignedOAuthMCPOperation) error {
	var err error
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted {
		if err := r.exactDetacher.DetachExactConnection(ctx, q.TenantID, agentID, op.Binding.Connection.Name, agentcfg.SignedOAuthMCPConnectionFingerprint(op.Binding.Connection)); err != nil {
			return err
		}
		op, err = r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseCatalogUnpublished, op.RevisionID)
		if err != nil {
			return err
		}
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseCatalogUnpublished {
		// The exact detach is idempotent. Repeating it here proves the private
		// provider and its cache are closed before the teardown receipt lands.
		if err := r.exactDetacher.DetachExactConnection(ctx, q.TenantID, agentID, op.Binding.Connection.Name, agentcfg.SignedOAuthMCPConnectionFingerprint(op.Binding.Connection)); err != nil {
			return err
		}
		op, err = r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseTeardownReceipted, op.RevisionID)
		if err != nil {
			return err
		}
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseTeardownReceipted {
		_, err = r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseRemoved, op.RevisionID)
	}
	return err
}

func (r *SignedOAuthMCPReconciler) expireIncomplete(ctx context.Context, q identity.Quadruple, agentID string, op agentcfg.SignedOAuthMCPOperation, revision agentcfg.Revision, hasRevision bool) error {
	if op.Phase != agentcfg.SignedOAuthMCPPhaseClaimed && op.Phase != agentcfg.SignedOAuthMCPPhaseRevisionCommitted {
		return fmt.Errorf("%w: phase %q is not expirable", agentcfg.ErrSignedCapabilityTransition, op.Phase)
	}
	if !signedCapabilityOperationExpired(op) {
		return fmt.Errorf("%w: operation has not expired", agentcfg.ErrSignedCapabilityAuthority)
	}
	if hasRevision {
		pair := revision.Payload.SignedOAuthMCPPair
		if pair == nil || pair.AuthorityOperationKind == "" {
			return fmt.Errorf("%w: expiring revision has no signed pair", agentcfg.ErrSignedCapabilityReplay)
		}
		loaded, err := r.operations.LoadForPair(ctx, q.TenantID, pair)
		if err != nil || loaded.EventID != op.EventID {
			return fmt.Errorf("%w: expiring revision does not bind operation", agentcfg.ErrSignedCapabilityReplay)
		}
		if err := r.detach(ctx, q, agentID, pair); err != nil {
			return err
		}
	}

	fence, fenceErr := r.fences.Load(ctx, q.TenantID, agentID)
	if hasRevision {
		if fenceErr != nil {
			return fenceErr
		}
		kind, err := r.operations.Kind(op.ReplayKey)
		if err != nil {
			return err
		}
		if fence.Phase != agentcfg.SignedOAuthMCPFencePending || fence.OperationKind != kind || fence.Fingerprint != op.Fingerprint ||
			fence.CandidateContentHash != revision.ContentHash || (fence.CandidateRevisionID != "" && fence.CandidateRevisionID != revision.RevisionID) {
			return fmt.Errorf("%w: expiry fence does not bind candidate", agentcfg.ErrSignedCapabilityPending)
		}
		operationCtx := agentcfg.WithSignedOAuthMCPFenceOperation(ctx, kind)
		if fence.PriorRevisionID != "" {
			if _, err := r.registry.Rollback(operationCtx, q, agentID, fence.PriorRevisionID, agentcfg.ConfigScopeAgent, compensatingWrite()); err != nil {
				return err
			}
		} else {
			deactivated, err := r.registry.DeactivateIfActive(operationCtx, q, agentID, revision.RevisionID, agentcfg.ConfigScopeAgent)
			if err != nil {
				return err
			}
			if !deactivated {
				physical, set, readErr := r.physical.PhysicalActive(ctx, q, agentID, agentcfg.ConfigScopeAgent)
				if readErr != nil || (set && physical.RevisionID == revision.RevisionID) {
					return errors.Join(fmt.Errorf("%w: expiry did not neutralize candidate", agentcfg.ErrSignedCapabilityPending), readErr)
				}
			}
		}
	} else if fenceErr != nil && !errors.Is(fenceErr, state.ErrNotFound) {
		return fenceErr
	}

	advanced, err := r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseExpiredIncomplete, op.RevisionID)
	if err != nil {
		return err
	}
	if fenceErr == nil && fence.Phase == agentcfg.SignedOAuthMCPFencePending {
		kind, kindErr := r.operations.Kind(advanced.ReplayKey)
		if kindErr != nil {
			return kindErr
		}
		if fence.OperationKind != kind || fence.Fingerprint != advanced.Fingerprint {
			return fmt.Errorf("%w: expiry found a foreign pending fence", agentcfg.ErrSignedCapabilityPending)
		}
		if _, err := r.fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceAborted, op.RevisionID); err != nil {
			latest, loadErr := r.fences.Load(context.WithoutCancel(ctx), q.TenantID, agentID)
			if loadErr != nil || latest.Phase != agentcfg.SignedOAuthMCPFenceAborted || latest.OperationKind != kind {
				return errors.Join(err, loadErr)
			}
		}
	}
	return nil
}

func (r *SignedOAuthMCPReconciler) advanceOperation(ctx context.Context, current agentcfg.SignedOAuthMCPOperation, next agentcfg.SignedOAuthMCPOperationPhase, revisionID string) (agentcfg.SignedOAuthMCPOperation, error) {
	advanced, err := r.operations.Advance(ctx, current, next, revisionID)
	if err == nil {
		return advanced, nil
	}
	latest, loadErr := r.operations.Load(context.WithoutCancel(ctx), current.ReplayKey)
	if loadErr == nil && latest.Phase == next && latest.RevisionID == revisionID && latest.Fingerprint == current.Fingerprint {
		return latest, nil
	}
	return agentcfg.SignedOAuthMCPOperation{}, errors.Join(err, loadErr)
}
