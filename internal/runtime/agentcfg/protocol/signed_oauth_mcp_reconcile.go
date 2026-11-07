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
	exactFencer   ExactConnectionTeardownFencer
	providers     SignedCapabilityProviderPreparer
	matcher       connectionMatcher
	// gates serializes passes per exact owner slot (see
	// signedReconcileGateKey), so runs for different tenants, agents or
	// user-tier owners never queue behind one another.
	gates *signedReconcileGates
	// continuations lets each slot cursor advance one bounded maintenance page
	// independently. Pair lifetime is tenant+agent scoped even though acting
	// authority remains subject-scoped.
	continuations *signedReconcileContinuations
	// attachBackoff is the bounded per-pair retry window for failed private
	// preparation. Nil (the registration-path literal) disables it.
	attachBackoff *signedAttachBackoff
}

type physicalActiveRegistry interface {
	PhysicalActive(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
}

func (r *SignedOAuthMCPReconciler) detachExact(ctx context.Context, q identity.Quadruple, agentID, name, fingerprint string) error {
	if signedOAuthMCPConfigScope(ctx) == agentcfg.ConfigScopeUser {
		subject, ok := r.detacher.(SubjectExactConnectionDetacher)
		if !ok || subject == nil {
			return ErrSignedCapabilityUnavailable
		}
		return subject.DetachExactConnectionForOwner(ctx, toolauth.Owner{Tenant: q.TenantID, Agent: agentID, User: q.UserID}, name, fingerprint)
	}
	return r.exactDetacher.DetachExactConnection(ctx, q.TenantID, agentID, name, fingerprint)
}

func (r *SignedOAuthMCPReconciler) beginExactTeardown(ctx context.Context, q identity.Quadruple, agentID, name, fingerprint string) (ExactConnectionTeardownFence, error) {
	if signedOAuthMCPConfigScope(ctx) == agentcfg.ConfigScopeUser {
		subject, ok := r.detacher.(SubjectExactConnectionTeardownFencer)
		if !ok || subject == nil {
			return nil, ErrSignedCapabilityUnavailable
		}
		return subject.BeginExactConnectionTeardownForOwner(toolauth.Owner{Tenant: q.TenantID, Agent: agentID, User: q.UserID}, name, fingerprint)
	}
	return r.exactFencer.BeginExactConnectionTeardown(q.TenantID, agentID, name, fingerprint)
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
	exactFencer, ok := detacher.(ExactConnectionTeardownFencer)
	if !ok || exactFencer == nil {
		return nil, fmt.Errorf("%w: signed capability reconciler needs exact teardown admission", ErrSignedCapabilityUnavailable)
	}
	operations, err := agentcfg.NewSignedOAuthMCPOperationStore(store)
	if err != nil {
		return nil, err
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(store)
	if err != nil {
		return nil, err
	}
	fences = fences.WithChangeObserver(agentcfg.CatalogChangeObserverFor(registry))
	return &SignedOAuthMCPReconciler{registry: registry, physical: physical, operations: operations, fences: fences, preparer: preparer, detacher: detacher, exactDetacher: exactDetacher, exactFencer: exactFencer, providers: providers, matcher: matcher,
		gates: newSignedReconcileGates(), continuations: newSignedReconcileContinuations(), attachBackoff: newSignedAttachBackoff()}, nil
}

// ReconcileSignedOAuthMCPCapability converges a single exact agent slot. A
// foreign/corrupt/expired receipt is never dispatched: it is returned to the
// caller, leaving the fence's prior active revision authoritative.
//
// Each durable receipt is converged independently: a failure of one pair is
// collected and joined into the returned error, never a reason to skip the
// remaining pairs of the same page.
func (r *SignedOAuthMCPReconciler) ReconcileSignedOAuthMCPCapability(ctx context.Context, q identity.Quadruple, agentID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(q.TenantID) == "" || strings.TrimSpace(q.UserID) == "" || strings.TrimSpace(q.SessionID) == "" || strings.TrimSpace(agentID) == "" {
		return ErrIdentityRequired
	}
	scope := signedOAuthMCPConfigScope(ctx)
	release, err := r.gates.acquire(ctx, signedReconcileGateKey(scope, q, agentID))
	if err != nil {
		return err
	}
	defer release()
	// The durable user attachment is keyed by (tenant, user, agent). Session
	// is only the audit/run subject that originally authorized the pair, so a
	// new session must continue the same bounded scan rather than start a new
	// ownership slot (and potentially miss the old receipt).
	cursorParts := []string{string(scope), q.TenantID, q.UserID, agentID}
	if scope == agentcfg.ConfigScopeAgent {
		cursorParts = append(cursorParts, q.SessionID)
	}
	cursorKey := strings.Join(cursorParts, "\x00")
	ops, continuation, err := r.operations.ScanTenantPage(ctx, q.TenantID, state.MaxStateScanLimit, r.continuations.get(cursorKey))
	if err != nil {
		return err
	}
	r.continuations.set(cursorKey, continuation)
	var errs []error
	for _, op := range ops {
		if err := r.reconcileOperation(ctx, q, agentID, scope, op); err != nil {
			errs = append(errs, fmt.Errorf("signed capability provider %q: %w", op.Binding.ProviderName, err))
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
	}
	return errors.Join(errs...)
}

// reconcileOperation converges one durable receipt of the scanned page.
func (r *SignedOAuthMCPReconciler) reconcileOperation(ctx context.Context, q identity.Quadruple, agentID string, scope agentcfg.ConfigScope, op agentcfg.SignedOAuthMCPOperation) error {
	if op.Binding.AgentID != agentID {
		return nil
	}
	// Agent-scope reconciliation owns only the operator/agent-tier
	// receipts. User-tier receipts are independently reconciled for the
	// acting subject below; allowing this pass to inspect them would let an
	// operator view mutate a user's desired pair or physical owner.
	if scope == agentcfg.ConfigScopeAgent && (op.ReplayKey.UserID != "" || op.ReplayKey.SessionID != "") {
		return nil
	}
	if scope == agentcfg.ConfigScopeUser && op.Binding.UserID != q.UserID {
		return nil
	}
	ownerQ, err := signedCapabilityOwnerQuadruple(q.TenantID, op.Binding)
	if err != nil {
		return err
	}
	// A user-scoped pair is durable under (tenant, user, agent), but the
	// session is still part of the live run subject. Keep the frozen
	// operation owner for durable-slot reads and use the current verified
	// subject for any path that can materialize or tear down a private
	// provider. Otherwise a later S2 run reconstructs the S1 session from
	// the receipt and sends S1 in the downstream actor binding.
	//
	// An agent-tier pair is owned by its frozen registrar, never by whichever
	// end user's run happens to trigger reconciliation: subjectQ stays the
	// owner, and ensureAttached seats that owner as the working identity for
	// private preparation exactly like the initial registration push.
	subjectQ := ownerQ
	if scope == agentcfg.ConfigScopeUser {
		subjectQ = q
	}
	active, hasActive, err := r.physical.PhysicalActive(ctx, ownerQ, agentID, signedOAuthMCPConfigScope(ctx))
	if err != nil {
		return fmt.Errorf("load physical active signed capability revision: %w", err)
	}
	kind, kindErr := r.operations.Kind(op.ReplayKey)
	if kindErr != nil {
		return kindErr
	}
	activePair, activePairSet, pairErr := active.Payload.SignedOAuthMCPPairByProvider(op.Binding.ProviderName)
	if pairErr != nil {
		return pairErr
	}
	if hasActive && signedCapabilityPairMatchesOperation(activePair, q.TenantID, op.Binding, kind) {
		return r.reconcilePair(ctx, subjectQ, agentID, active, activePair)
	}
	switch op.Phase {
	case agentcfg.SignedOAuthMCPPhaseClaimed:
		if signedCapabilityOperationExpired(op) {
			if err := r.expireIncomplete(ctx, subjectQ, agentID, op, agentcfg.Revision{}, false); err != nil {
				return err
			}
		}
	case agentcfg.SignedOAuthMCPPhaseRevisionCommitted:
		if signedCapabilityOperationExpired(op) {
			revision, getErr := r.registry.Get(ctx, ownerQ, agentID, op.RevisionID, signedOAuthMCPConfigScope(ctx))
			if getErr != nil {
				return getErr
			}
			if err := r.expireIncomplete(ctx, subjectQ, agentID, op, revision, true); err != nil {
				return err
			}
		}
	case agentcfg.SignedOAuthMCPPhaseExpiryAdmitted:
		if err := r.resumeExpiryCompensation(ctx, subjectQ, agentID, op); err != nil {
			return err
		}
	case agentcfg.SignedOAuthMCPPhasePreparationRejectionAdmitted:
		if err := r.resumePreparationRejection(ctx, subjectQ, agentID, op); err != nil {
			return err
		}
	case agentcfg.SignedOAuthMCPPhasePreparationRejected:
		// Terminal cleanup receipt. A later signed operation may already own
		// the agent fence, so historical rejection is deliberately inert.
	case agentcfg.SignedOAuthMCPPhasePublished:
		// A published historical receipt is never authority to reattach. If
		// the current physical desired state has no pair, the paired removal
		// revision landed before its receipt checkpoint; continue teardown.
		if hasActive && !activePairSet {
			op, err = r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalAdmitted, active.RevisionID)
			if err != nil {
				return err
			}
			op, err = r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, active.RevisionID)
			if err != nil {
				return err
			}
			if err := r.resumeRemoval(ctx, subjectQ, agentID, op); err != nil {
				return err
			}
		}
	case agentcfg.SignedOAuthMCPPhaseRemovalAdmitted, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, agentcfg.SignedOAuthMCPPhaseCatalogUnpublished, agentcfg.SignedOAuthMCPPhaseTeardownReceipted:
		if hasActive && activePairSet {
			return fmt.Errorf("%w: stale removal cannot detach the active pair", agentcfg.ErrSignedCapabilityReplay)
		}
		if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalAdmitted {
			op, err = r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, active.RevisionID)
			if err != nil {
				return err
			}
		}
		if err := r.resumeRemoval(ctx, subjectQ, agentID, op); err != nil {
			return err
		}
	case agentcfg.SignedOAuthMCPPhaseRemoved:
		// A different runtime may have published before removal won the
		// shared operation-slot fence. Its process-local handle converges from
		// the terminal durable receipt on the next reconciliation pass.
		operationKind, kindErr := r.operations.Kind(op.ReplayKey)
		if kindErr != nil {
			return kindErr
		}
		fingerprint := signedCapabilityPublisherAttachmentFingerprint(op.Binding.Connection, operationKind, op.PublisherEpoch)
		if err := r.detachExact(ctx, subjectQ, agentID, op.Binding.Connection.Name, fingerprint); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileSignedOAuthMCPCapabilityForScope runs the same durable recovery
// lifecycle against the requested config tier. User callers must pass a
// complete acting identity; the user filter in the scan then admits only that
// user's operation receipts and ConfigScopeUser desired pair.
func (r *SignedOAuthMCPReconciler) ReconcileSignedOAuthMCPCapabilityForScope(ctx context.Context, q identity.Quadruple, agentID string, scope agentcfg.ConfigScope) error {
	if scope != agentcfg.ConfigScopeAgent && scope != agentcfg.ConfigScopeUser {
		return fmt.Errorf("%w: unknown signed capability config scope %q", ErrSignedCapabilityUnavailable, scope)
	}
	return r.ReconcileSignedOAuthMCPCapability(withSignedOAuthMCPConfigScope(ctx, scope), q, agentID)
}

// signedCapabilityOwnerQuadruple rebuilds the immutable registrar slot from a
// tenant-scoped durable receipt. Reconciliation may be invoked by a later
// tenant user/session, but persistence and recovery must use the frozen owner
// slot; the live run subject remains only the exchange/cache subject.
func signedCapabilityOwnerQuadruple(tenant string, binding agentcfg.SignedOAuthMCPBinding) (identity.Quadruple, error) {
	if strings.TrimSpace(tenant) == "" || binding.TenantID != tenant || strings.TrimSpace(binding.UserID) == "" || strings.TrimSpace(binding.SessionID) == "" {
		return identity.Quadruple{}, fmt.Errorf("%w: corrupt signed capability owner binding", agentcfg.ErrSignedCapabilityReplay)
	}
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: binding.UserID, SessionID: binding.SessionID}}, nil
}

func (r *SignedOAuthMCPReconciler) reconcilePair(ctx context.Context, q identity.Quadruple, agentID string, revision agentcfg.Revision, pair *agentcfg.SignedOAuthMCPPair) error {
	if pair.OwnerAgentID != agentID || pair.OwnerUserID != q.UserID ||
		(signedOAuthMCPConfigScope(ctx) == agentcfg.ConfigScopeAgent && pair.OwnerSessionID != q.SessionID) ||
		strings.TrimSpace(pair.Connection.Name) == "" {
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
	if (op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed || op.Phase == agentcfg.SignedOAuthMCPPhaseRevisionCommitted) && op.RevisionID != "" && op.RevisionID != revision.RevisionID {
		return fmt.Errorf("%w: receipt names another revision", agentcfg.ErrSignedCapabilityReplay)
	}
	switch op.Phase {
	case agentcfg.SignedOAuthMCPPhaseClaimed, agentcfg.SignedOAuthMCPPhaseRevisionCommitted:
		if signedCapabilityOperationExpired(op) {
			return r.expireIncomplete(ctx, q, agentID, op, revision, true)
		}
		// Expiry above is never suppressed. Only the attach attempt, whose
		// private preparation previously failed for this exact receipt and
		// revision, waits out its bounded retry window.
		slot := signedAttachSlot(ctx, q, agentID, pair)
		if err := r.attachBackoff.admit(slot, signedAttachFingerprint(pair, revision, op)); err != nil {
			return err
		}
		fence, err := r.validPendingFence(ctx, q.TenantID, agentID, op, revision)
		if err != nil {
			return err
		}
		revision, err = requirePhysicalActiveRevision(ctx, r.physical, q, agentID, revision.RevisionID, revision.ContentHash)
		if err != nil || !signedCapabilityPayloadMatches(revision.Payload, q.TenantID, op.Binding) {
			return fmt.Errorf("%w: recovery candidate is not the physical active pair", agentcfg.ErrSignedCapabilityPending)
		}
		if op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed {
			op, err = r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, revision.RevisionID)
			if err != nil {
				return err
			}
		}
		if err := r.publish(ctx, q, agentID, pair, op, revision); err != nil {
			if !errors.Is(err, tools.ErrArtifactEgressSchema) {
				r.recordAttachFailure(ctx, slot, signedAttachFingerprint(pair, revision, op), err)
				return err
			}
			if rejectErr := r.rejectPreparation(ctx, q, agentID, op, revision); rejectErr != nil {
				return errors.Join(err, rejectErr)
			}
			return nil
		}
		latest, err := r.operations.LoadForPair(ctx, q.TenantID, pair)
		if err != nil {
			return err
		}
		if latest.Phase != agentcfg.SignedOAuthMCPPhasePublished {
			return fmt.Errorf("%w: publication did not converge", agentcfg.ErrSignedCapabilityPending)
		}
		r.attachBackoff.succeed(slot)
		_, err = r.fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceCommitted, revision.RevisionID)
		if errors.Is(err, agentcfg.ErrSignedCapabilityTransition) {
			return r.commitFence(ctx, q.TenantID, agentID, latest, revision)
		}
		return err
	case agentcfg.SignedOAuthMCPPhaseExpiryAdmitted:
		return r.resumeExpiryCompensation(ctx, q, agentID, op)
	case agentcfg.SignedOAuthMCPPhasePreparationRejectionAdmitted:
		return r.resumePreparationRejection(ctx, q, agentID, op)
	case agentcfg.SignedOAuthMCPPhasePreparationRejected:
		return nil
	case agentcfg.SignedOAuthMCPPhasePublished:
		// The fence commit is durable convergence and is never suppressed.
		if err := r.commitFence(ctx, q.TenantID, agentID, op, revision); err != nil {
			return err
		}
		slot := signedAttachSlot(ctx, q, agentID, pair)
		fingerprint := signedAttachFingerprint(pair, revision, op)
		if err := r.attachBackoff.admit(slot, fingerprint); err != nil {
			return err
		}
		if _, err := r.ensureAttached(ctx, q, agentID, pair, revision, op); err != nil {
			r.recordAttachFailure(ctx, slot, fingerprint, err)
			return err
		}
		r.attachBackoff.succeed(slot)
		return nil
	case agentcfg.SignedOAuthMCPPhaseRemovalAdmitted:
		// Admission is durable removal intent. Another runtime may still be
		// performing its desired-state CAS, so a reconciler must never roll it
		// back merely because the old pair remains active. Resume that exact CAS
		// under this runtime's local teardown fence instead.
		return r.resumeAdmittedActiveRemoval(ctx, q, agentID, revision, pair, op)
	case agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted:
		if err := r.detach(ctx, q, agentID, pair, op); err != nil {
			return err
		}
		_, err := r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseCatalogUnpublished, revision.RevisionID)
		return err
	case agentcfg.SignedOAuthMCPPhaseCatalogUnpublished:
		if err := r.detach(ctx, q, agentID, pair, op); err != nil {
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
	kind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return agentcfg.SignedOAuthMCPActivationFence{}, err
	}
	fence, err := r.fences.LoadForOperation(ctx, tenant, agentID, kind)
	if err != nil {
		return agentcfg.SignedOAuthMCPActivationFence{}, err
	}
	if fence.Phase != agentcfg.SignedOAuthMCPFencePending || fence.OperationKind != kind || fence.Fingerprint != op.Fingerprint || fence.CandidateContentHash != revision.ContentHash || (fence.CandidateRevisionID != "" && fence.CandidateRevisionID != revision.RevisionID) {
		return agentcfg.SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: pending fence does not bind candidate", agentcfg.ErrSignedCapabilityPending)
	}
	return fence, nil
}

// adoptClaimedCandidate repairs a lost operation checkpoint only when the
// physical pointer, immutable pair, owner, and pending fence prove the same
// committed candidate. Historical orphans are never enough to advance a claim.
func (r *SignedOAuthMCPReconciler) adoptClaimedCandidate(ctx context.Context, q identity.Quadruple, agentID string, op agentcfg.SignedOAuthMCPOperation, candidate agentcfg.Revision) (agentcfg.SignedOAuthMCPOperation, error) {
	if op.Phase != agentcfg.SignedOAuthMCPPhaseClaimed || q.TenantID != op.Binding.TenantID || q.UserID != op.Binding.UserID || agentID != op.Binding.AgentID ||
		(signedOAuthMCPConfigScope(ctx) == agentcfg.ConfigScopeAgent && q.SessionID != op.Binding.SessionID) ||
		(op.RevisionID != "" && op.RevisionID != candidate.RevisionID) {
		return agentcfg.SignedOAuthMCPOperation{}, fmt.Errorf("%w: claimed candidate crossed its exact owner or revision", agentcfg.ErrSignedCapabilityReplay)
	}
	kind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, err
	}
	if !signedCapabilityPayloadMatchesOperation(candidate.Payload, q.TenantID, op.Binding, kind) {
		return agentcfg.SignedOAuthMCPOperation{}, fmt.Errorf("%w: claimed candidate does not bind operation", agentcfg.ErrSignedCapabilityReplay)
	}
	if _, err := requirePhysicalActiveRevision(ctx, r.physical, q, agentID, candidate.RevisionID, candidate.ContentHash); err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, err
	}
	if _, err := r.validPendingFence(ctx, q.TenantID, agentID, op, candidate); err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, err
	}
	return r.advanceOperation(ctx, op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, candidate.RevisionID)
}

func (r *SignedOAuthMCPReconciler) commitFence(ctx context.Context, tenant, agentID string, op agentcfg.SignedOAuthMCPOperation, revision agentcfg.Revision) error {
	kind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return err
	}
	fence, err := r.fences.LoadForOperation(ctx, tenant, agentID, kind)
	if err != nil {
		return err
	}
	if fence.OperationKind != kind || fence.Fingerprint != op.Fingerprint || fence.CandidateContentHash != revision.ContentHash {
		if fence.Phase != agentcfg.SignedOAuthMCPFenceCommitted {
			return fmt.Errorf("%w: fence does not bind published pair", agentcfg.ErrSignedCapabilityPending)
		}
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: op.Binding.UserID, SessionID: op.Binding.SessionID}}
	if fence.Phase == agentcfg.SignedOAuthMCPFenceCommitted {
		candidate, getErr := r.registry.Get(ctx, q, agentID, op.RevisionID, signedOAuthMCPConfigScope(ctx))
		physicalRevision, physicalErr := requirePhysicalActiveRevision(ctx, r.physical, q, agentID, revision.RevisionID, revision.ContentHash)
		if getErr == nil && physicalErr == nil && fence.CandidateRevisionID == op.RevisionID && candidate.ContentHash == fence.CandidateContentHash &&
			signedCapabilityPayloadMatchesOperation(candidate.Payload, tenant, op.Binding, kind) &&
			signedCapabilityPayloadMatchesOperation(physicalRevision.Payload, tenant, op.Binding, kind) {
			return nil
		}
		return errors.Join(fmt.Errorf("%w: committed fence or active revision does not bind published pair", agentcfg.ErrSignedCapabilityPending), getErr, physicalErr)
	}
	if fence.Phase != agentcfg.SignedOAuthMCPFencePending {
		return fmt.Errorf("%w: aborted fence", agentcfg.ErrSignedCapabilityPending)
	}
	physicalRevision, err := requirePhysicalActiveRevision(ctx, r.physical, q, agentID, revision.RevisionID, revision.ContentHash)
	if err != nil || !signedCapabilityPayloadMatchesOperation(physicalRevision.Payload, tenant, op.Binding, kind) {
		return fmt.Errorf("%w: published recovery candidate is not physically active", agentcfg.ErrSignedCapabilityPending)
	}
	_, err = r.fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceCommitted, revision.RevisionID)
	return err
}

func (r *SignedOAuthMCPReconciler) publish(ctx context.Context, q identity.Quadruple, agentID string, pair *agentcfg.SignedOAuthMCPPair, op agentcfg.SignedOAuthMCPOperation, revision agentcfg.Revision) error {
	publishedBy, err := r.ensureAttached(ctx, q, agentID, pair, revision, op)
	if err != nil {
		return err
	}
	_, err = r.operations.Advance(ctx, publishedBy, agentcfg.SignedOAuthMCPPhasePublished, publishedBy.RevisionID)
	if errors.Is(err, agentcfg.ErrSignedCapabilityTransition) {
		return nil
	}
	return err
}

// recordAttachFailure opens or extends the bounded retry window only for a
// failed external preparation. Caller cancellation and local revalidation or
// CAS races are not evidence that the pair cannot attach.
func (r *SignedOAuthMCPReconciler) recordAttachFailure(ctx context.Context, slot signedAttachBackoffKey, fingerprint string, err error) {
	if ctx.Err() != nil {
		return
	}
	var preparation *signedPreparationError
	if errors.As(err, &preparation) {
		r.attachBackoff.fail(slot, fingerprint)
	}
}

func (r *SignedOAuthMCPReconciler) ensureAttached(ctx context.Context, q identity.Quadruple, agentID string, pair *agentcfg.SignedOAuthMCPPair, revision agentcfg.Revision, op agentcfg.SignedOAuthMCPOperation) (agentcfg.SignedOAuthMCPOperation, error) {
	owner := toolauth.Owner{Tenant: q.TenantID, Agent: agentID}
	if signedOAuthMCPConfigScope(ctx) == agentcfg.ConfigScopeUser {
		owner.User = q.UserID
	}
	// Steady state: a published receipt whose exact publisher generation is
	// already live in this process needs no durable revalidation. The matcher
	// fingerprint binds the durable publisher epoch of the receipt the caller
	// just loaded, so another runtime's takeover or any pair change misses it
	// and falls through to the full revalidated path below. Nothing is
	// published on this path.
	if op.Phase == agentcfg.SignedOAuthMCPPhasePublished && op.PublisherEpoch != "" &&
		r.matcher.ConnectionMatches(owner, pair.Connection.Name, signedCapabilityPairAttachmentFingerprint(pair, op.PublisherEpoch)) {
		return op, nil
	}
	if err := r.revalidateAttachmentAuthority(ctx, q, agentID, revision, pair, op); err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, err
	}
	if op.PublisherEpoch != "" && r.matcher.ConnectionMatches(owner, pair.Connection.Name, signedCapabilityPairAttachmentFingerprint(pair, op.PublisherEpoch)) {
		return op, nil
	}
	op, err := r.operations.AcquirePublisher(ctx, op)
	if err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, err
	}
	if err := r.revalidateAttachmentAuthority(ctx, q, agentID, revision, pair, op); err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, err
	}
	fingerprint := signedCapabilityPairAttachmentFingerprint(pair, op.PublisherEpoch)
	binding := agentcfg.SignedOAuthMCPBinding{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID, AgentID: agentID, Broker: pair.Broker, ProviderName: pair.ProviderName,
		CapabilityRevision: pair.CapabilityRevision, URLDigest: pair.URLDigest, SinkDigest: pair.SinkDigest,
		Audience: pair.Audience, Scopes: pair.Scopes, Connection: pair.Connection}
	provider, err := r.providers.PrepareSignedCapabilityProvider(ctx, pair.Broker, signedCapabilityExchangeBinding(binding, pair.Sink, pair.AuthorityOperationKind, op, r.operations), pair.Scopes)
	if err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, &signedPreparationError{err: err}
	}
	// Private preparation is the owner's connection mint. For an agent-tier
	// pair, q is the frozen registrar slot (never the end user whose run
	// triggered this pass), so seat it as the working identity: the exchange
	// subject then matches the signed actor binding exactly as it did on the
	// initial registration push. A user-tier pair keeps the acting user, who
	// is its owner. Live tool calls never take this path and keep their own
	// run identity.
	attachCtx := ctx
	if signedOAuthMCPConfigScope(ctx) == agentcfg.ConfigScopeAgent {
		ownerCtx, idErr := identity.With(ctx, q.Identity)
		if idErr != nil {
			return agentcfg.SignedOAuthMCPOperation{}, errors.Join(idErr, closePreparedSignedCapability(ctx, nil, provider))
		}
		attachCtx = ownerCtx
	}
	attachCtx = tools.WithEffectiveAgentConfig(tools.WithInvokingAgent(attachCtx, agentID), agentID)
	prepared, err := r.preparer.PrepareConnection(attachCtx, AttachRequest{Identity: q.Identity, AgentID: agentID, Owner: owner, Name: pair.Connection.Name,
		Transport: agentcfg.MCPTransportHTTP, URL: pair.Connection.URL,
		OAuthProvider:         signedCapabilityOAuthProvider(pair.ProviderName, pair.Connection.Injection),
		OAuthProviderOverride: provider.Binding(), OwnOAuthProvider: true, Injection: pair.Connection.Injection.Clone(),
		ToolAllowlist: pair.Connection.ToolAllowlist, ToolDenylist: pair.Connection.ToolDenylist,
		ConnectTimeoutMS: pair.Connection.ConnectTimeoutMS, RequestTimeoutMS: pair.Connection.RequestTimeoutMS,
		ArtifactByteEligible: pair.Connection.ArtifactByteEligible, ArtifactParams: cloneArtifactParams(pair.Connection.ArtifactParams),
		DescriptorFingerprint: fingerprint})
	if err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, &signedPreparationError{err: errors.Join(err, closePreparedSignedCapability(ctx, nil, provider))}
	}
	authorityPrepared, ok := prepared.(AuthorityBoundPreparedConnection)
	if !ok {
		return agentcfg.SignedOAuthMCPOperation{}, errors.Join(
			fmt.Errorf("%w: signed capability preparation lacks an exact staged-publication receipt", ErrSignedCapabilityUnavailable),
			closePreparedSignedCapability(ctx, prepared, provider),
		)
	}
	// ActivateIf reserves the exact provider handle in the production registry
	// before this durable proof. Removal can close that non-dispatchable handle
	// while the proof runs; publication then fails by the same reservation. If
	// publication wins, registry and catalog become visible under one registry
	// lock, so every later removal necessarily observes the exact handle.
	if err := authorityPrepared.ActivateUnder(ctx, func(proofCtx context.Context, publish func() error) error {
		if err := r.revalidateAttachmentAuthority(proofCtx, q, agentID, revision, pair, op); err != nil {
			return err
		}
		return r.operations.FencePublication(proofCtx, op, publish)
	}); err != nil {
		return agentcfg.SignedOAuthMCPOperation{}, errors.Join(err, closePreparedSignedCapability(ctx, prepared, provider))
	}
	provider.Commit(ctx)
	return op, nil
}

func (r *SignedOAuthMCPReconciler) revalidateAttachmentAuthority(ctx context.Context, q identity.Quadruple, agentID string, revision agentcfg.Revision, pair *agentcfg.SignedOAuthMCPPair, expected agentcfg.SignedOAuthMCPOperation) error {
	return revalidateSignedAttachmentAuthority(ctx, r.registry, r.physical, r.operations, r.fences, q, agentID, revision, pair, expected)
}

func revalidateSignedAttachmentAuthority(ctx context.Context, registry agentcfg.Registry, physicalRegistry physicalActiveRegistry, operations *agentcfg.SignedOAuthMCPOperationStore, fences *agentcfg.SignedOAuthMCPActivationFenceStore, q identity.Quadruple, agentID string, revision agentcfg.Revision, pair *agentcfg.SignedOAuthMCPPair, expected agentcfg.SignedOAuthMCPOperation) error {
	physical, err := requirePhysicalActiveRevision(ctx, physicalRegistry, q, agentID, revision.RevisionID, revision.ContentHash)
	if err != nil {
		return errors.Join(
			fmt.Errorf("%w: attachment candidate is no longer physically active", agentcfg.ErrSignedCapabilityPending),
			err,
		)
	}
	kind, err := operations.Kind(expected.ReplayKey)
	if err != nil {
		return err
	}
	if !signedCapabilityPayloadMatchesOperation(physical.Payload, q.TenantID, expected.Binding, kind) ||
		!signedCapabilityPairMatchesOperation(pair, q.TenantID, expected.Binding, kind) {
		return fmt.Errorf("%w: attachment candidate no longer binds the exact pair lifetime", agentcfg.ErrSignedCapabilityPending)
	}
	latest, err := operations.LoadForPair(ctx, q.TenantID, pair)
	if err != nil {
		return err
	}
	if latest.EventID != expected.EventID || latest.Fingerprint != expected.Fingerprint || latest.RevisionID != expected.RevisionID || latest.Phase != expected.Phase || latest.PublisherEpoch != expected.PublisherEpoch {
		return fmt.Errorf("%w: attachment pair lifetime advanced from %q", agentcfg.ErrSignedCapabilityPending, expected.Phase)
	}
	wantFencePhase := agentcfg.SignedOAuthMCPFencePending
	if expected.Phase == agentcfg.SignedOAuthMCPPhasePublished {
		wantFencePhase = agentcfg.SignedOAuthMCPFenceCommitted
	} else if expected.Phase != agentcfg.SignedOAuthMCPPhaseRevisionCommitted {
		return fmt.Errorf("%w: operation phase %q cannot publish an attachment", agentcfg.ErrSignedCapabilityPending, expected.Phase)
	}
	fence, err := fences.LoadForOperation(ctx, q.TenantID, agentID, kind)
	if err != nil {
		return err
	}
	candidateRevisionMismatch := fence.CandidateRevisionID != "" && fence.CandidateRevisionID != revision.RevisionID
	candidateContentMismatch := fence.CandidateContentHash != revision.ContentHash
	if wantFencePhase == agentcfg.SignedOAuthMCPFenceCommitted {
		// A committed fence binds the immutable pair's ORIGINAL candidate. Later
		// section-scoped writes legitimately carry that exact server-owned pair
		// into a sibling revision. After a process restart the current physical
		// revision is that sibling, while the durable fence must remain anchored
		// to the operation's original candidate rather than being rewritten by an
		// unrelated config edit. Re-read and validate both ends: the immutable
		// candidate still binds this operation, and the current physical revision
		// above still carries the same exact pair. A changed/foreign pair, missing
		// candidate, stale active pointer, or mismatched operation therefore stays
		// fail-closed.
		candidateRevisionMismatch = fence.CandidateRevisionID != expected.RevisionID
		candidate, candidateErr := registry.Get(ctx, q, agentID, expected.RevisionID, signedOAuthMCPConfigScope(ctx))
		if candidateErr != nil {
			return errors.Join(fmt.Errorf("%w: committed attachment candidate is unavailable", agentcfg.ErrSignedCapabilityPending), candidateErr)
		}
		candidateContentMismatch = candidate.ContentHash != fence.CandidateContentHash ||
			!signedCapabilityPayloadMatchesOperation(candidate.Payload, q.TenantID, expected.Binding, kind)
	}
	if fence.Phase != wantFencePhase || fence.OperationKind != kind || fence.Fingerprint != expected.Fingerprint || candidateContentMismatch || candidateRevisionMismatch {
		return fmt.Errorf("%w: attachment fence no longer binds the exact pair lifetime", agentcfg.ErrSignedCapabilityPending)
	}
	return nil
}

func (r *SignedOAuthMCPReconciler) detach(ctx context.Context, q identity.Quadruple, agentID string, pair *agentcfg.SignedOAuthMCPPair, op agentcfg.SignedOAuthMCPOperation) error {
	return r.detachExact(ctx, q, agentID, pair.Connection.Name, signedCapabilityPairAttachmentFingerprint(pair, op.PublisherEpoch))
}

func (r *SignedOAuthMCPReconciler) resumeAdmittedActiveRemoval(ctx context.Context, q identity.Quadruple, agentID string, revision agentcfg.Revision, pair *agentcfg.SignedOAuthMCPPair, op agentcfg.SignedOAuthMCPOperation) (retErr error) {
	fingerprint := signedCapabilityPairAttachmentFingerprint(pair, op.PublisherEpoch)
	teardownFence, err := r.beginExactTeardown(ctx, q, agentID, pair.Connection.Name, fingerprint)
	if err != nil {
		return err
	}
	sealed := false
	defer func() {
		if !sealed {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signedCapabilityCleanupTimeout)
			defer cancel()
			retErr = errors.Join(retErr, teardownFence.Cancel(cleanupCtx))
		}
	}()
	payload := carrySiblingsForward(revision, true)
	payload, removed, err := payload.RemoveSignedOAuthMCPPair(pair.ProviderName)
	if err != nil || !removed {
		return errors.Join(fmt.Errorf("%w: active removal pair is absent", agentcfg.ErrSignedCapabilityReplay), err)
	}
	candidateHash, err := agentcfg.ContentHash(agentcfg.NormalizePayload(payload))
	if err != nil {
		return err
	}
	operationKind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return err
	}
	removalCtx := agentcfg.WithSignedOAuthMCPFenceOperation(ctx, operationKind)
	removalRevision, err := r.registry.SetRevision(removalCtx, q, agentID, signedOAuthMCPConfigScope(removalCtx), payload, agentcfg.SetOptions{ExpectedContentHash: revision.ContentHash})
	if err != nil {
		current, set, readErr := r.registry.Active(context.WithoutCancel(ctx), q, agentID, signedOAuthMCPConfigScope(ctx))
		_, currentPairSet, currentPairErr := current.Payload.SignedOAuthMCPPairByProvider(pair.ProviderName)
		if readErr != nil || !set || currentPairErr != nil || currentPairSet || current.ContentHash != candidateHash {
			return errors.Join(err, readErr, currentPairErr)
		}
		removalRevision = current
	}
	teardownFence.Seal()
	sealed = true
	op, err = r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, removalRevision.RevisionID)
	if err != nil {
		return err
	}
	return r.resumeRemoval(ctx, q, agentID, op)
}

func (r *SignedOAuthMCPReconciler) resumeRemoval(ctx context.Context, q identity.Quadruple, agentID string, op agentcfg.SignedOAuthMCPOperation) error {
	operationKind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return err
	}
	fingerprint := signedCapabilityPublisherAttachmentFingerprint(op.Binding.Connection, operationKind, op.PublisherEpoch)
	if op.Phase == agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted {
		if err := r.detachExact(ctx, q, agentID, op.Binding.Connection.Name, fingerprint); err != nil {
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
		if err := r.detachExact(ctx, q, agentID, op.Binding.Connection.Name, fingerprint); err != nil {
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
	candidateRevisionID := ""
	if hasRevision {
		pair, set, pairErr := revision.Payload.SignedOAuthMCPPairByProvider(op.Binding.ProviderName)
		if pairErr != nil || !set || pair.AuthorityOperationKind == "" {
			return fmt.Errorf("%w: expiring revision has no signed pair", agentcfg.ErrSignedCapabilityReplay)
		}
		loaded, err := r.operations.LoadForPair(ctx, q.TenantID, pair)
		if err != nil || loaded.EventID != op.EventID {
			return fmt.Errorf("%w: expiring revision does not bind operation", agentcfg.ErrSignedCapabilityReplay)
		}
		candidateRevisionID = revision.RevisionID
	}
	admitted, err := r.operations.AdmitExpiry(ctx, op, candidateRevisionID, nowUTC())
	if err != nil {
		return err
	}
	return r.resumeExpiryCompensation(ctx, q, agentID, admitted)
}

func (r *SignedOAuthMCPReconciler) resumeExpiryCompensation(ctx context.Context, q identity.Quadruple, agentID string, op agentcfg.SignedOAuthMCPOperation) error {
	if op.Phase != agentcfg.SignedOAuthMCPPhaseExpiryAdmitted {
		return fmt.Errorf("%w: phase %q has no admitted expiry", agentcfg.ErrSignedCapabilityTransition, op.Phase)
	}
	kind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return err
	}
	if q.TenantID != op.Binding.TenantID || q.UserID != op.Binding.UserID || agentID != op.Binding.AgentID ||
		(signedOAuthMCPConfigScope(ctx) == agentcfg.ConfigScopeAgent && q.SessionID != op.Binding.SessionID) {
		return fmt.Errorf("%w: expiry owner does not match frozen registrar", agentcfg.ErrSignedCapabilityReplay)
	}
	// The exact generation fingerprint makes detach idempotent across every
	// crash boundary, including a prior attempt that already closed the cache.
	fingerprint := signedCapabilityPublisherAttachmentFingerprint(op.Binding.Connection, kind, op.PublisherEpoch)
	if err := r.detachExact(ctx, q, agentID, op.Binding.Connection.Name, fingerprint); err != nil {
		return err
	}

	fence, fenceErr := r.fences.LoadForOperation(ctx, q.TenantID, agentID, kind)
	if fenceErr != nil && !errors.Is(fenceErr, state.ErrNotFound) {
		return fenceErr
	}
	hasOwnFence := fenceErr == nil && fence.OperationKind == kind && fence.Fingerprint == op.Fingerprint
	if fenceErr == nil && !hasOwnFence && fence.Phase == agentcfg.SignedOAuthMCPFencePending {
		return fmt.Errorf("%w: expiry found a foreign pending activation fence", agentcfg.ErrSignedCapabilityPending)
	}

	if op.ExpiryCandidateRevisionID != "" {
		candidate, getErr := r.registry.Get(ctx, q, agentID, op.ExpiryCandidateRevisionID, signedOAuthMCPConfigScope(ctx))
		if getErr != nil {
			return getErr
		}
		if !signedCapabilityPayloadMatchesOperation(candidate.Payload, q.TenantID, op.Binding, kind) {
			return fmt.Errorf("%w: expiry candidate does not bind the exact operation", agentcfg.ErrSignedCapabilityReplay)
		}
		if !hasOwnFence || fence.CandidateContentHash != candidate.ContentHash ||
			(fence.CandidateRevisionID != "" && fence.CandidateRevisionID != candidate.RevisionID) {
			return fmt.Errorf("%w: expiry fence does not bind frozen candidate", agentcfg.ErrSignedCapabilityPending)
		}
		physical, set, readErr := r.physical.PhysicalActive(ctx, q, agentID, signedOAuthMCPConfigScope(ctx))
		if readErr != nil {
			return readErr
		}
		if set && physical.RevisionID == candidate.RevisionID {
			if err := restorePreOperationAuthority(ctx, r.registry, q, agentID, candidate, fence.PriorRevisionID, kind); err != nil {
				return err
			}
		}
	}

	physical, set, err := r.physical.PhysicalActive(ctx, q, agentID, signedOAuthMCPConfigScope(ctx))
	if err != nil {
		return err
	}
	if !hasOwnFence && op.ExpiryCandidateRevisionID == "" {
		if set && signedCapabilityPayloadMatchesOperation(physical.Payload, q.TenantID, op.Binding, kind) {
			return fmt.Errorf("%w: matching expiry candidate has no activation fence", agentcfg.ErrSignedCapabilityPending)
		}
		_, err = r.operations.CompleteExpiry(ctx, op)
		if err == nil {
			return nil
		}
		latest, loadErr := r.operations.Load(context.WithoutCancel(ctx), op.ReplayKey)
		if loadErr == nil && latest.Phase == agentcfg.SignedOAuthMCPPhaseExpiredIncomplete && latest.AuthorityGeneration == op.AuthorityGeneration {
			return nil
		}
		return errors.Join(err, loadErr)
	}
	priorRevisionID := ""
	if hasOwnFence {
		priorRevisionID = fence.PriorRevisionID
	}
	if (priorRevisionID == "" && set) || (priorRevisionID != "" && (!set || physical.RevisionID != priorRevisionID)) ||
		(set && physical.RevisionID == op.ExpiryCandidateRevisionID) {
		return fmt.Errorf("%w: expiry did not restore frozen prior authority", agentcfg.ErrSignedCapabilityPending)
	}
	if hasOwnFence {
		switch fence.Phase {
		case agentcfg.SignedOAuthMCPFencePending:
			if _, err := r.fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceAborted, op.ExpiryCandidateRevisionID); err != nil {
				latest, loadErr := r.fences.LoadForOperation(context.WithoutCancel(ctx), q.TenantID, agentID, kind)
				if loadErr != nil || latest.Phase != agentcfg.SignedOAuthMCPFenceAborted || latest.OperationKind != kind || latest.Fingerprint != op.Fingerprint || latest.CandidateRevisionID != op.ExpiryCandidateRevisionID {
					return errors.Join(err, loadErr)
				}
			}
		case agentcfg.SignedOAuthMCPFenceAborted:
			if fence.CandidateRevisionID != op.ExpiryCandidateRevisionID {
				return fmt.Errorf("%w: aborted expiry fence names another candidate", agentcfg.ErrSignedCapabilityPending)
			}
		default:
			return fmt.Errorf("%w: expiry cannot abort activation fence in phase %q", agentcfg.ErrSignedCapabilityPending, fence.Phase)
		}
	}
	_, err = r.operations.CompleteExpiry(ctx, op)
	if err == nil {
		return nil
	}
	latest, loadErr := r.operations.Load(context.WithoutCancel(ctx), op.ReplayKey)
	if loadErr == nil && latest.Phase == agentcfg.SignedOAuthMCPPhaseExpiredIncomplete && latest.AuthorityGeneration == op.AuthorityGeneration && latest.LastExpiredRevisionID == op.ExpiryCandidateRevisionID {
		return nil
	}
	return errors.Join(err, loadErr)
}

// resumePreparationRejection finishes the durable compensation begun after a
// rejected preparation or an owner explicitly retired an incomplete pair. The operation is
// durably admitted before authority is restored, so every crash boundary
// remains recoverable; it becomes terminal only after restoration and exact
// fence abort, and the rejected JTI can never publish on restart.
func (r *SignedOAuthMCPReconciler) resumePreparationRejection(ctx context.Context, q identity.Quadruple, agentID string, op agentcfg.SignedOAuthMCPOperation) error {
	if op.Phase != agentcfg.SignedOAuthMCPPhasePreparationRejectionAdmitted || op.RevisionID == "" {
		return fmt.Errorf("%w: phase %q has no rejected preparation", agentcfg.ErrSignedCapabilityTransition, op.Phase)
	}
	kind, err := r.operations.Kind(op.ReplayKey)
	if err != nil {
		return err
	}
	if q.TenantID != op.Binding.TenantID || q.UserID != op.Binding.UserID || agentID != op.Binding.AgentID ||
		(signedOAuthMCPConfigScope(ctx) == agentcfg.ConfigScopeAgent && q.SessionID != op.Binding.SessionID) {
		return fmt.Errorf("%w: rejected preparation owner does not match frozen registrar", agentcfg.ErrSignedCapabilityReplay)
	}
	candidate, err := r.registry.Get(ctx, q, agentID, op.RevisionID, signedOAuthMCPConfigScope(ctx))
	if err != nil {
		return err
	}
	if !signedCapabilityPayloadMatchesOperation(candidate.Payload, q.TenantID, op.Binding, kind) {
		return fmt.Errorf("%w: rejected preparation candidate does not bind the exact operation", agentcfg.ErrSignedCapabilityReplay)
	}
	fence, err := r.fences.LoadForOperation(ctx, q.TenantID, agentID, kind)
	if err != nil {
		return err
	}
	if fence.OperationKind != kind || fence.Fingerprint != op.Fingerprint || fence.CandidateContentHash != candidate.ContentHash ||
		(fence.CandidateRevisionID != "" && fence.CandidateRevisionID != candidate.RevisionID) {
		return fmt.Errorf("%w: rejected preparation fence does not bind frozen candidate", agentcfg.ErrSignedCapabilityPending)
	}
	if fence.Phase == agentcfg.SignedOAuthMCPFenceAborted {
		_, err = r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhasePreparationRejected, candidate.RevisionID)
		return err
	}
	if fence.Phase != agentcfg.SignedOAuthMCPFencePending {
		return fmt.Errorf("%w: rejected preparation fence is %q", agentcfg.ErrSignedCapabilityPending, fence.Phase)
	}
	// Admission changed the operation EventID, fencing every older publisher
	// before exact teardown. An acknowledgement loss after local publication
	// must not leave that generation blocking a corrected registration.
	if op.PublisherEpoch != "" {
		fingerprint := signedCapabilityPublisherAttachmentFingerprint(op.Binding.Connection, kind, op.PublisherEpoch)
		if err := r.detachExact(ctx, q, agentID, op.Binding.Connection.Name, fingerprint); err != nil {
			return err
		}
	}
	physical, set, err := r.physical.PhysicalActive(ctx, q, agentID, signedOAuthMCPConfigScope(ctx))
	if err != nil {
		return err
	}
	if set && physical.RevisionID == candidate.RevisionID {
		if err := restorePreOperationAuthority(ctx, r.registry, q, agentID, candidate, fence.PriorRevisionID, kind); err != nil {
			return err
		}
	} else if set && signedCapabilityPayloadMatchesOperation(physical.Payload, q.TenantID, op.Binding, kind) {
		return fmt.Errorf("%w: incomplete pair moved to a different active revision", agentcfg.ErrSignedCapabilityPending)
	}
	if _, err = r.fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceAborted, candidate.RevisionID); err != nil {
		return err
	}
	_, err = r.operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhasePreparationRejected, candidate.RevisionID)
	return err
}

func (r *SignedOAuthMCPReconciler) rejectPreparation(ctx context.Context, q identity.Quadruple, agentID string, expected agentcfg.SignedOAuthMCPOperation, candidate agentcfg.Revision) error {
	for range 3 {
		current, err := r.operations.Load(ctx, expected.ReplayKey)
		if err != nil {
			return err
		}
		if current.Fingerprint != expected.Fingerprint || current.RevisionID != candidate.RevisionID {
			return fmt.Errorf("%w: rejected preparation no longer binds committed candidate", agentcfg.ErrSignedCapabilityReplay)
		}
		switch current.Phase {
		case agentcfg.SignedOAuthMCPPhaseRevisionCommitted:
			if _, err := r.operations.Advance(ctx, current, agentcfg.SignedOAuthMCPPhasePreparationRejectionAdmitted, candidate.RevisionID); err != nil {
				if errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
					continue
				}
				return err
			}
		case agentcfg.SignedOAuthMCPPhasePreparationRejectionAdmitted:
			return r.resumePreparationRejection(ctx, q, agentID, current)
		case agentcfg.SignedOAuthMCPPhasePreparationRejected:
			return nil
		default:
			return fmt.Errorf("%w: phase %q cannot reject preparation", agentcfg.ErrSignedCapabilityTransition, current.Phase)
		}
	}
	return fmt.Errorf("%w: preparation rejection admission did not converge", agentcfg.ErrSignedCapabilityPending)
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
