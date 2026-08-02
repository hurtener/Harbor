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
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// SignedOAuthMCPReconciler resumes only durable D-401 pair operations for one
// exact tenant and agent. It deliberately enumerates revision history rather
// than operation records: an opaque operation receipt alone is not authority
// to attach or detach anything.
type SignedOAuthMCPReconciler struct {
	registry   agentcfg.Registry
	operations *agentcfg.SignedOAuthMCPOperationStore
	fences     *agentcfg.SignedOAuthMCPActivationFenceStore
	preparer   ConnectionPreparer
	detacher   ConnectionDetacher
	providers  SignedCapabilityProviderPreparer
	matcher    connectionMatcher
	gate       chan struct{}
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
	operations, err := agentcfg.NewSignedOAuthMCPOperationStore(store)
	if err != nil {
		return nil, err
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(store)
	if err != nil {
		return nil, err
	}
	return &SignedOAuthMCPReconciler{registry: registry, operations: operations, fences: fences, preparer: preparer, detacher: detacher, providers: providers, matcher: matcher, gate: make(chan struct{}, 1)}, nil
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
	history, err := r.registry.ListRevisions(ctx, q, agentID, agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		return fmt.Errorf("list signed capability revisions: %w", err)
	}
	for _, revision := range history {
		pair := revision.Payload.SignedOAuthMCPPair
		if pair == nil {
			continue
		}
		if err := r.reconcilePair(ctx, q, agentID, revision, pair); err != nil {
			return err
		}
	}
	return nil
}

func (r *SignedOAuthMCPReconciler) reconcilePair(ctx context.Context, q identity.Quadruple, agentID string, revision agentcfg.Revision, pair *agentcfg.SignedOAuthMCPPair) error {
	if pair.OwnerAgentID != agentID || strings.TrimSpace(pair.Connection.Name) == "" {
		return fmt.Errorf("%w: foreign or incomplete signed pair", agentcfg.ErrSignedCapabilityReplay)
	}
	canonicalURL, sink, err := agentcfg.CanonicalOAuthMCPURL(pair.Connection.URL)
	if err != nil || canonicalURL != pair.Connection.URL || sink != pair.Sink || pair.URLDigest != agentcfg.OAuthMCPURLDigest(canonicalURL) {
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
		if !op.ExpiresAt.After(nowUTC()) {
			return fmt.Errorf("%w: incomplete authority operation has expired", agentcfg.ErrSignedCapabilityAuthority)
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
	fingerprint := agentcfg.MCPConnectionFingerprint(agentcfg.MCPConnectionDescriptor{Name: pair.Connection.Name, Transport: agentcfg.MCPTransportHTTP, URL: pair.Connection.URL, OAuthProvider: pair.ProviderName})
	if r.matcher.ConnectionMatches(owner, pair.Connection.Name, fingerprint) {
		return nil
	}
	provider, err := r.providers.PrepareSignedCapabilityProvider(ctx, q.TenantID, agentID, pair.ProviderName, pair.Broker, pair.Audience, pair.Sink, pair.Scopes)
	if err != nil {
		return err
	}
	prepared, err := r.preparer.PrepareConnection(ctx, AttachRequest{Identity: q.Identity, AgentID: agentID, Name: pair.Connection.Name, Transport: agentcfg.MCPTransportHTTP, URL: pair.Connection.URL, OAuthProvider: pair.ProviderName, OAuthProviderOverride: provider.Binding()})
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
	return r.detacher.DetachConnection(ctx, q.TenantID, agentID, pair.Connection.Name)
}
