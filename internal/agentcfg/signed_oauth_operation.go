package agentcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

type signedOAuthMCPFenceContextKey struct{}

// WithSignedOAuthMCPFenceOperation marks the single registration write that
// owns a pending fence. It is an internal cross-package capability, not a wire
// value; callers cannot manufacture it from Protocol input.
func WithSignedOAuthMCPFenceOperation(ctx context.Context, operationKind string) context.Context {
	return context.WithValue(ctx, signedOAuthMCPFenceContextKey{}, operationKind)
}

// SignedOAuthMCPFenceOperation returns the owning operation marker carried by
// an internal registration write.
func SignedOAuthMCPFenceOperation(ctx context.Context) string {
	value := ctx.Value(signedOAuthMCPFenceContextKey{})
	operation, ok := value.(string)
	if !ok {
		return ""
	}
	return operation
}

// SignedOAuthMCPActivationFenceKind returns the fixed agent-scoped record
// kind used by registry readers and writers to enforce the durable fence.
func SignedOAuthMCPActivationFenceKind() string { return signedOAuthMCPActivationFenceKind }

var (
	// ErrSignedCapabilityReplay is returned when an authority JTI has already
	// claimed a different immutable capability fingerprint.
	ErrSignedCapabilityReplay = errors.New("agentcfg: signed oauth mcp capability replay")
	// ErrSignedCapabilityTransition marks a transition that is not in the
	// one pair-lifetime operation graph.
	ErrSignedCapabilityTransition = errors.New("agentcfg: invalid signed oauth mcp capability operation transition")
	// ErrSignedCapabilityPending is returned when a foreign writer observes a
	// durable first-install fence. It must retry after the owning operation
	// commits or aborts; treating a physical candidate pointer as authority is
	// forbidden.
	ErrSignedCapabilityPending = errors.New("agentcfg: signed oauth mcp capability activation is pending")
)

// SignedOAuthMCPOperationPhase is a durable pair-lifetime operation phase.
type SignedOAuthMCPOperationPhase string

const (
	SignedOAuthMCPPhaseClaimed                  SignedOAuthMCPOperationPhase = "claimed"
	SignedOAuthMCPPhaseRevisionCommitted        SignedOAuthMCPOperationPhase = "revision_committed"
	SignedOAuthMCPPhasePublished                SignedOAuthMCPOperationPhase = "published"
	SignedOAuthMCPPhaseRemovalAdmitted          SignedOAuthMCPOperationPhase = "removal_admitted"
	SignedOAuthMCPPhaseRemovalRevisionCommitted SignedOAuthMCPOperationPhase = "removal_revision_committed"
	SignedOAuthMCPPhaseCatalogUnpublished       SignedOAuthMCPOperationPhase = "catalog_unpublished"
	SignedOAuthMCPPhaseTeardownReceipted        SignedOAuthMCPOperationPhase = "teardown_receipted"
	SignedOAuthMCPPhaseRemoved                  SignedOAuthMCPOperationPhase = "removed"
	SignedOAuthMCPPhaseExpiryAdmitted           SignedOAuthMCPOperationPhase = "expiry_admitted"
	SignedOAuthMCPPhaseExpiredIncomplete        SignedOAuthMCPOperationPhase = "expired_incomplete"
	// RetirementCleanupClassSignedOAuthMCPPair is the frozen retirement
	// manifest class for one durable signed OAuth MCP pair lifetime. Its
	// resource is two SHA-256 digests only; owner identity and capability
	// material remain exclusively in the signed-pair operation receipt.
	RetirementCleanupClassSignedOAuthMCPPair = "signed_oauth_mcp_pair"
)

// SignedOAuthMCPReplayKey is tenant-scoped by design. Agent ID is a signed
// binding field, not a fourth persistence/isolation principal.
type SignedOAuthMCPReplayKey struct {
	TenantID        string
	TrustAnchorName string
	Issuer          string
	KeyID           string
	JTI             string
}

// SignedOAuthMCPOperation is the bounded durable recovery record. EventID is
// the StateStore generation used by Advance's exact SaveIf predicate.
type SignedOAuthMCPOperation struct {
	ReplayKey                 SignedOAuthMCPReplayKey      `json:"replay_key"`
	Binding                   SignedOAuthMCPBinding        `json:"binding"`
	Fingerprint               string                       `json:"fingerprint"`
	ExpiresAt                 time.Time                    `json:"expires_at"`
	Phase                     SignedOAuthMCPOperationPhase `json:"phase"`
	RevisionID                string                       `json:"revision_id,omitempty"`
	PublisherEpoch            string                       `json:"publisher_epoch,omitempty"`
	ExpiryFromPhase           SignedOAuthMCPOperationPhase `json:"expiry_from_phase,omitempty"`
	ExpiryCandidateRevisionID string                       `json:"expiry_candidate_revision_id,omitempty"`
	AuthorityGeneration       uint64                       `json:"authority_generation,omitempty"`
	ExpiredAttemptCount       uint64                       `json:"expired_attempt_count,omitempty"`
	LastExpiredAt             time.Time                    `json:"last_expired_at,omitempty"`
	LastExpiredRevisionID     string                       `json:"last_expired_revision_id,omitempty"`
	EventID                   state.EventID                `json:"-"`
}

// SignedOAuthMCPOperationStore persists the signed-capability tenant-scoped anti-replay
// operation record through the mandatory StateStore SaveIf primitive.
type SignedOAuthMCPOperationStore struct{ state state.StateStore }

// SignedOAuthMCPActivationFencePhase is the durable first-install authority
// fence. A pending fence hides its matching physical candidate from Active and
// rejects foreign pointer mutations across runtimes.
type SignedOAuthMCPActivationFencePhase string

const (
	SignedOAuthMCPFencePending   SignedOAuthMCPActivationFencePhase = "pending"
	SignedOAuthMCPFenceCommitted SignedOAuthMCPActivationFencePhase = "committed"
	SignedOAuthMCPFenceAborted   SignedOAuthMCPActivationFencePhase = "aborted"
)

// SignedOAuthMCPActivationFence binds one pending candidate to its exact
// operation receipt and prior active revision. It contains no envelope, JTI,
// URL, credential, or other secret material.
type SignedOAuthMCPActivationFence struct {
	TenantID             string                             `json:"tenant_id"`
	AgentID              string                             `json:"agent_id"`
	OperationKind        string                             `json:"operation_kind"`
	Fingerprint          string                             `json:"fingerprint"`
	CandidateContentHash string                             `json:"candidate_content_hash"`
	CandidateRevisionID  string                             `json:"candidate_revision_id,omitempty"`
	PriorRevisionID      string                             `json:"prior_revision_id,omitempty"`
	Phase                SignedOAuthMCPActivationFencePhase `json:"phase"`
	EventID              state.EventID                      `json:"-"`
}

// SignedOAuthMCPActivationFenceStore is the durable signed-capability security fence.
type SignedOAuthMCPActivationFenceStore struct{ state state.StateStore }

const signedOAuthMCPActivationFenceKind = "agentcfg.signed_oauth_mcp.activation_fence"

// NewSignedOAuthMCPActivationFenceStore constructs the StateStore-backed
// fence facade.
func NewSignedOAuthMCPActivationFenceStore(store state.StateStore) (*SignedOAuthMCPActivationFenceStore, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: signed capability activation fence store is nil", ErrInvalidConfig)
	}
	return &SignedOAuthMCPActivationFenceStore{state: store}, nil
}

// Begin creates or resumes exactly the owning pending fence. A different
// operation or candidate content at the same agent slot fails closed.
func (s *SignedOAuthMCPActivationFenceStore) Begin(ctx context.Context, tenant, agentID, operationKind, fingerprint, candidateHash, priorRevisionID string) (SignedOAuthMCPActivationFence, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPActivationFence{}, err
	}
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(agentID) == "" || strings.TrimSpace(operationKind) == "" || strings.TrimSpace(fingerprint) == "" || strings.TrimSpace(candidateHash) == "" {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: incomplete activation fence", ErrSignedCapabilityAuthority)
	}
	quad := signedOAuthMCPActivationFenceQuad(tenant, agentID)
	fence := SignedOAuthMCPActivationFence{TenantID: tenant, AgentID: agentID, OperationKind: operationKind, Fingerprint: fingerprint, CandidateContentHash: candidateHash, PriorRevisionID: priorRevisionID, Phase: SignedOAuthMCPFencePending, EventID: state.NewEventID()}
	encoded, err := json.Marshal(fence)
	if err != nil {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("marshal signed capability activation fence: %w", err)
	}
	err = s.state.SaveIf(ctx, []state.SlotExpectation{{Identity: quad, Kind: signedOAuthMCPActivationFenceKind}}, state.StateRecord{ID: fence.EventID, Identity: quad, Kind: signedOAuthMCPActivationFenceKind, Bytes: encoded})
	if err == nil {
		return fence, nil
	}
	if !errors.Is(err, state.ErrConditionFailed) {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("claim signed capability activation fence: %w", err)
	}
	existing, loadErr := s.Load(ctx, tenant, agentID)
	if loadErr != nil {
		return SignedOAuthMCPActivationFence{}, loadErr
	}
	if existing.OperationKind != operationKind || existing.Fingerprint != fingerprint || existing.CandidateContentHash != candidateHash || existing.PriorRevisionID != priorRevisionID {
		// A terminal fence is a receipt for its immutable candidate, not a
		// permanent agent-wide lease. Once its operation committed or aborted,
		// the next signed capability may install its own exact pending fence.
		// The replacement still CAS-compares the old EventID, so two new
		// operations cannot both cross this boundary.
		if existing.Phase == SignedOAuthMCPFencePending {
			return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: foreign operation owns activation fence", ErrSignedCapabilityPending)
		}
		err = s.state.SaveIf(ctx, []state.SlotExpectation{{Identity: quad, Kind: signedOAuthMCPActivationFenceKind, ExpectedEventID: existing.EventID}}, state.StateRecord{ID: fence.EventID, Identity: quad, Kind: signedOAuthMCPActivationFenceKind, Bytes: encoded})
		if err == nil {
			return fence, nil
		}
		if !errors.Is(err, state.ErrConditionFailed) {
			return SignedOAuthMCPActivationFence{}, fmt.Errorf("replace signed capability activation fence: %w", err)
		}
		latest, latestErr := s.Load(ctx, tenant, agentID)
		if latestErr != nil {
			return SignedOAuthMCPActivationFence{}, latestErr
		}
		if latest.OperationKind == operationKind && latest.Fingerprint == fingerprint && latest.CandidateContentHash == candidateHash && latest.PriorRevisionID == priorRevisionID {
			return latest, nil
		}
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: foreign operation owns activation fence", ErrSignedCapabilityPending)
	}
	return existing, nil
}

// Load returns the exact durable fence for an agent. An absent fence is a
// wrapped StateStore not-found error so callers cannot accidentally treat a
// missing receipt as a committed activation.
func (s *SignedOAuthMCPActivationFenceStore) Load(ctx context.Context, tenant, agentID string) (SignedOAuthMCPActivationFence, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPActivationFence{}, err
	}
	record, err := s.state.Load(ctx, signedOAuthMCPActivationFenceQuad(tenant, agentID), signedOAuthMCPActivationFenceKind)
	if err != nil {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("load signed capability activation fence: %w", err)
	}
	var fence SignedOAuthMCPActivationFence
	if err := json.Unmarshal(record.Bytes, &fence); err != nil {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("decode signed capability activation fence: %w", err)
	}
	if fence.TenantID != tenant || fence.AgentID != agentID || fence.OperationKind == "" || fence.Fingerprint == "" || fence.CandidateContentHash == "" || (fence.Phase != SignedOAuthMCPFencePending && fence.Phase != SignedOAuthMCPFenceCommitted && fence.Phase != SignedOAuthMCPFenceAborted) {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: corrupt signed capability activation fence", ErrSignedCapabilityPending)
	}
	fence.EventID = record.ID
	return fence, nil
}

// Advance marks the exact fence terminally committed or aborted. Candidate ID
// is immutable once recorded, and every transition CAS-compares EventID.
func (s *SignedOAuthMCPActivationFenceStore) Advance(ctx context.Context, current SignedOAuthMCPActivationFence, phase SignedOAuthMCPActivationFencePhase, candidateRevisionID string) (SignedOAuthMCPActivationFence, error) {
	if phase != SignedOAuthMCPFenceCommitted && phase != SignedOAuthMCPFenceAborted {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: invalid activation fence transition", ErrSignedCapabilityTransition)
	}
	if current.Phase != SignedOAuthMCPFencePending || (phase == SignedOAuthMCPFenceCommitted && strings.TrimSpace(candidateRevisionID) == "") {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: activation fence %s -> %s", ErrSignedCapabilityTransition, current.Phase, phase)
	}
	next := current
	next.Phase = phase
	next.CandidateRevisionID = candidateRevisionID
	next.EventID = state.NewEventID()
	encoded, err := json.Marshal(next)
	if err != nil {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("marshal signed capability activation fence: %w", err)
	}
	quad := signedOAuthMCPActivationFenceQuad(current.TenantID, current.AgentID)
	err = s.state.SaveIf(ctx, []state.SlotExpectation{{Identity: quad, Kind: signedOAuthMCPActivationFenceKind, ExpectedEventID: current.EventID}}, state.StateRecord{ID: next.EventID, Identity: quad, Kind: signedOAuthMCPActivationFenceKind, Bytes: encoded})
	if errors.Is(err, state.ErrConditionFailed) {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: activation fence changed concurrently", ErrSignedCapabilityPending)
	}
	if err != nil {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("advance signed capability activation fence: %w", err)
	}
	return next, nil
}

// ReopenForRenewedAuthority changes only the exact aborted fence left by this
// operation's completed expiry compensation back to pending. Begin remains
// intentionally strict: an ordinary retry can never reopen a terminal fence.
func (s *SignedOAuthMCPActivationFenceStore) ReopenForRenewedAuthority(ctx context.Context, current SignedOAuthMCPActivationFence, renewed SignedOAuthMCPOperation, candidateHash, priorRevisionID string) (SignedOAuthMCPActivationFence, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPActivationFence{}, err
	}
	operationKind := ""
	operationQuad := identity.Quadruple{}
	if quad, kind, err := signedOAuthMCPOperationSlot(renewed.ReplayKey); err == nil {
		operationQuad = quad
		operationKind = kind
	}
	if current.Phase != SignedOAuthMCPFenceAborted || renewed.Phase != SignedOAuthMCPPhaseClaimed || renewed.AuthorityGeneration < 2 ||
		renewed.ExpiredAttemptCount == 0 || renewed.LastExpiredAt.IsZero() || renewed.Binding.TenantID != current.TenantID ||
		renewed.Binding.AgentID != current.AgentID || current.OperationKind != operationKind || current.Fingerprint != renewed.Fingerprint ||
		current.CandidateRevisionID != renewed.LastExpiredRevisionID || renewed.EventID == "" || strings.TrimSpace(candidateHash) == "" {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: activation fence is not the exact completed expiry fence", ErrSignedCapabilityTransition)
	}
	next := current
	next.Phase = SignedOAuthMCPFencePending
	next.CandidateContentHash = candidateHash
	next.PriorRevisionID = priorRevisionID
	next.CandidateRevisionID = ""
	next.EventID = state.NewEventID()
	encoded, err := json.Marshal(next)
	if err != nil {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("marshal renewed signed capability activation fence: %w", err)
	}
	quad := signedOAuthMCPActivationFenceQuad(current.TenantID, current.AgentID)
	err = s.state.SaveIf(ctx, []state.SlotExpectation{
		{Identity: quad, Kind: signedOAuthMCPActivationFenceKind, ExpectedEventID: current.EventID},
		{Identity: operationQuad, Kind: operationKind, ExpectedEventID: renewed.EventID},
	}, state.StateRecord{ID: next.EventID, Identity: quad, Kind: signedOAuthMCPActivationFenceKind, Bytes: encoded})
	if errors.Is(err, state.ErrConditionFailed) {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("%w: activation fence changed concurrently", ErrSignedCapabilityPending)
	}
	if err != nil {
		return SignedOAuthMCPActivationFence{}, fmt.Errorf("reopen renewed signed capability activation fence: %w", err)
	}
	return next, nil
}

func signedOAuthMCPActivationFenceQuad(tenant, agentID string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "__agentcfg__", SessionID: agentID}}
}

// NewSignedOAuthMCPOperationStore constructs the durable operation facade.
func NewSignedOAuthMCPOperationStore(store state.StateStore) (*SignedOAuthMCPOperationStore, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: signed capability operation store is nil", ErrInvalidConfig)
	}
	return &SignedOAuthMCPOperationStore{state: store}, nil
}

// Claim atomically creates the first record for key or resumes the exact same
// pair-lifetime operation. A reused JTI may never manufacture a new binding.
func (s *SignedOAuthMCPOperationStore) Claim(ctx context.Context, key SignedOAuthMCPReplayKey, binding SignedOAuthMCPBinding, expiresAt time.Time) (SignedOAuthMCPOperation, bool, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPOperation{}, false, err
	}
	quad, kind, err := signedOAuthMCPOperationSlot(key)
	if err != nil {
		return SignedOAuthMCPOperation{}, false, err
	}
	fingerprint := SignedOAuthMCPPairFingerprint(binding)
	operation := SignedOAuthMCPOperation{ReplayKey: key, Binding: binding, Fingerprint: fingerprint, ExpiresAt: expiresAt.UTC(), Phase: SignedOAuthMCPPhaseClaimed, AuthorityGeneration: 1, EventID: state.NewEventID()}
	bytes, err := json.Marshal(operation)
	if err != nil {
		return SignedOAuthMCPOperation{}, false, fmt.Errorf("marshal signed capability operation: %w", err)
	}
	err = s.state.SaveIf(ctx, []state.SlotExpectation{{Identity: quad, Kind: kind}}, state.StateRecord{ID: operation.EventID, Identity: quad, Kind: kind, Bytes: bytes})
	if err == nil {
		return operation, true, nil
	}
	if !errors.Is(err, state.ErrConditionFailed) {
		return SignedOAuthMCPOperation{}, false, fmt.Errorf("claim signed capability operation: %w", err)
	}
	existing, loadErr := s.load(ctx, quad, kind)
	if loadErr != nil {
		return SignedOAuthMCPOperation{}, false, loadErr
	}
	if existing.ReplayKey != key || existing.Fingerprint != fingerprint || !reflect.DeepEqual(existing.Binding, binding) {
		return SignedOAuthMCPOperation{}, false, fmt.Errorf("%w: jti already binds another capability", ErrSignedCapabilityReplay)
	}
	return existing, false, nil
}

// RenewAuthority consumes a later, freshly verified envelope for the exact
// same replay key and immutable registration binding. It reopens only a fully
// compensated expired_incomplete receipt; published, removal, and in-progress
// expiry records are never renewable.
func (s *SignedOAuthMCPOperationStore) RenewAuthority(ctx context.Context, current SignedOAuthMCPOperation, key SignedOAuthMCPReplayKey, binding SignedOAuthMCPBinding, expiresAt time.Time) (SignedOAuthMCPOperation, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	fingerprint := SignedOAuthMCPPairFingerprint(binding)
	if current.Phase != SignedOAuthMCPPhaseExpiredIncomplete || current.ReplayKey != key || current.Fingerprint != fingerprint ||
		current.Fingerprint != SignedOAuthMCPPairFingerprint(current.Binding) || !reflect.DeepEqual(current.Binding, binding) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: authority renewal does not exactly bind the expired operation", ErrSignedCapabilityReplay)
	}
	expiresAt = expiresAt.UTC()
	if expiresAt.IsZero() || !expiresAt.After(current.ExpiresAt) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: renewed authority expiry must be later", ErrSignedCapabilityAuthority)
	}
	next := current
	next.ExpiresAt = expiresAt
	next.Phase = SignedOAuthMCPPhaseClaimed
	next.RevisionID = ""
	next.PublisherEpoch = ""
	next.ExpiryFromPhase = ""
	next.ExpiryCandidateRevisionID = ""
	next.AuthorityGeneration = effectiveAuthorityGeneration(current.AuthorityGeneration) + 1
	if next.ExpiredAttemptCount == 0 {
		next.ExpiredAttemptCount = 1
	}
	if next.LastExpiredAt.IsZero() {
		next.LastExpiredAt = current.ExpiresAt.Add(SignedOAuthMCPAuthorityClockSkew)
	}
	if next.LastExpiredRevisionID == "" {
		next.LastExpiredRevisionID = current.RevisionID
	}
	return s.saveExact(ctx, current, next, "renew signed capability authority")
}

// AdmitExpiry wins the operation-slot CAS before compensation may detach or
// move any active pointer. It freezes the source phase and exact candidate so
// every restart resumes the same bounded work.
func (s *SignedOAuthMCPOperationStore) AdmitExpiry(ctx context.Context, current SignedOAuthMCPOperation, candidateRevisionID string, expiredAt time.Time) (SignedOAuthMCPOperation, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	if current.Phase != SignedOAuthMCPPhaseClaimed && current.Phase != SignedOAuthMCPPhaseRevisionCommitted {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: phase %q is not expirable", ErrSignedCapabilityTransition, current.Phase)
	}
	if current.Phase == SignedOAuthMCPPhaseRevisionCommitted && (candidateRevisionID == "" || candidateRevisionID != current.RevisionID) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: expiry candidate does not match committed revision", ErrSignedCapabilityReplay)
	}
	if current.Phase == SignedOAuthMCPPhaseClaimed && current.RevisionID != "" && candidateRevisionID != current.RevisionID {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: expiry candidate does not match claimed revision", ErrSignedCapabilityReplay)
	}
	expiredAt = expiredAt.UTC()
	if expiredAt.IsZero() || current.ExpiresAt.Add(SignedOAuthMCPAuthorityClockSkew).After(expiredAt) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: operation has not expired", ErrSignedCapabilityAuthority)
	}
	next := current
	next.Phase = SignedOAuthMCPPhaseExpiryAdmitted
	next.ExpiryFromPhase = current.Phase
	next.ExpiryCandidateRevisionID = candidateRevisionID
	next.AuthorityGeneration = effectiveAuthorityGeneration(current.AuthorityGeneration)
	next.ExpiredAttemptCount++
	next.LastExpiredAt = expiredAt
	next.LastExpiredRevisionID = candidateRevisionID
	return s.saveExact(ctx, current, next, "admit signed capability expiry")
}

// CompleteExpiry records the terminal compensated state. Callers invoke it
// only after exact detach, prior/absence restoration, candidate inactivity,
// and exact activation-fence abort have all succeeded.
func (s *SignedOAuthMCPOperationStore) CompleteExpiry(ctx context.Context, current SignedOAuthMCPOperation) (SignedOAuthMCPOperation, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	if current.Phase != SignedOAuthMCPPhaseExpiryAdmitted ||
		(current.ExpiryFromPhase != SignedOAuthMCPPhaseClaimed && current.ExpiryFromPhase != SignedOAuthMCPPhaseRevisionCommitted) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: phase %q cannot complete expiry", ErrSignedCapabilityTransition, current.Phase)
	}
	next := current
	next.Phase = SignedOAuthMCPPhaseExpiredIncomplete
	next.AuthorityGeneration = effectiveAuthorityGeneration(current.AuthorityGeneration)
	return s.saveExact(ctx, current, next, "complete signed capability expiry")
}

// Load returns the exact tenant-scoped operation record. It is intentionally a
// narrow recovery read: callers must already hold the signed replay key and
// must still compare its immutable fingerprint before resuming a phase.
func (s *SignedOAuthMCPOperationStore) Load(ctx context.Context, key SignedOAuthMCPReplayKey) (SignedOAuthMCPOperation, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	quad, kind, err := signedOAuthMCPOperationSlot(key)
	if err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	op, err := s.load(ctx, quad, kind)
	if err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	if op.ReplayKey != key || op.Fingerprint == "" || !signedOAuthMCPOperationPhaseKnown(op.Phase) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: corrupt signed capability operation", ErrSignedCapabilityReplay)
	}
	return op, nil
}

// ScanTenantPage returns one bounded page of operation receipts for a tenant.
// This is a maintenance-only recovery read; callers must still match the
// complete signed subject and agent before acting on a record.
func (s *SignedOAuthMCPOperationStore) ScanTenantPage(ctx context.Context, tenant string, limit int, continuation string) ([]SignedOAuthMCPOperation, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(tenant) == "" {
		return nil, "", fmt.Errorf("%w: tenant is empty", ErrSignedCapabilityAuthority)
	}
	const operationPrefix = "agentcfg.signed_oauth_mcp."
	page, err := s.state.ScanKindForTenant(ctx, state.ListScope{MaintenanceScoped: true}, tenant, operationPrefix, limit, continuation)
	if err != nil {
		return nil, "", fmt.Errorf("scan signed capability operations: %w", err)
	}
	out := make([]SignedOAuthMCPOperation, 0, len(page.Records))
	for _, record := range page.Records {
		if record.Kind == signedOAuthMCPActivationFenceKind {
			continue
		}
		var op SignedOAuthMCPOperation
		if err := json.Unmarshal(record.Bytes, &op); err != nil {
			return nil, "", fmt.Errorf("%w: decode tenant operation %q", ErrSignedCapabilityReplay, record.Kind)
		}
		_, expectedKind, err := signedOAuthMCPOperationSlot(op.ReplayKey)
		if err != nil || expectedKind != record.Kind || op.ReplayKey.TenantID != tenant || op.Binding.TenantID != tenant ||
			op.Fingerprint != SignedOAuthMCPPairFingerprint(op.Binding) || !signedOAuthMCPOperationPhaseKnown(op.Phase) {
			return nil, "", fmt.Errorf("%w: corrupt tenant operation %q", ErrSignedCapabilityReplay, record.Kind)
		}
		op.EventID = record.ID
		out = append(out, op)
	}
	return out, page.Continuation, nil
}

// LoadForPair returns the frozen pair-lifetime receipt without recovering the
// raw JTI. The opaque operation kind is persisted in immutable pair history,
// so a removal can resume after the registration envelope has expired or its
// verifier key has rotated. It is not bearer authority.
func (s *SignedOAuthMCPOperationStore) LoadForPair(ctx context.Context, tenant string, pair *SignedOAuthMCPPair) (SignedOAuthMCPOperation, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	if pair == nil || tenant == "" || strings.TrimSpace(pair.AuthorityOperationKind) == "" {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: signed pair has no durable operation receipt", ErrSignedCapabilityReplay)
	}
	quad := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "__agentcfg__", SessionID: "__signed_oauth_mcp__"}}
	op, err := s.load(ctx, quad, pair.AuthorityOperationKind)
	if err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	if op.ReplayKey.TenantID != tenant || op.Fingerprint == "" || !signedOAuthMCPOperationPhaseKnown(op.Phase) ||
		op.Binding.Broker != pair.Broker || op.Binding.ProviderName != pair.ProviderName ||
		op.Binding.CapabilityRevision != pair.CapabilityRevision || op.Binding.URLDigest != pair.URLDigest ||
		op.Binding.SinkDigest != pair.SinkDigest || op.Binding.Audience != pair.Audience || op.Binding.AgentID != pair.OwnerAgentID ||
		op.Binding.UserID != pair.OwnerUserID || op.Binding.SessionID != pair.OwnerSessionID ||
		!sameSignedOAuthMCPConnection(op.Binding.Connection, pair.Connection) ||
		op.Fingerprint != SignedOAuthMCPPairFingerprint(SignedOAuthMCPBinding{TenantID: tenant, UserID: pair.OwnerUserID, SessionID: pair.OwnerSessionID, AgentID: pair.OwnerAgentID, Broker: pair.Broker, ProviderName: pair.ProviderName, CapabilityRevision: pair.CapabilityRevision, URLDigest: pair.URLDigest, SinkDigest: pair.SinkDigest, Audience: pair.Audience, Scopes: pair.Scopes, Connection: pair.Connection}) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: corrupt or foreign signed-pair operation receipt", ErrSignedCapabilityReplay)
	}
	return op, nil
}

// Kind returns the opaque durable operation slot retained in pair history.
func (s *SignedOAuthMCPOperationStore) Kind(key SignedOAuthMCPReplayKey) (string, error) {
	_, kind, err := signedOAuthMCPOperationSlot(key)
	return kind, err
}

// SignedOAuthMCPRetirementResource returns the redacted frozen locator used by
// agent retirement. It deliberately hashes the already-opaque operation kind
// again so the manifest and lifecycle status expose only a pair fingerprint
// and an unlinkable operation hash, never JTI, URL, owner, or credentials.
func SignedOAuthMCPRetirementResource(op SignedOAuthMCPOperation) (string, error) {
	if !SignedOAuthMCPRetirementPending(op.Phase) {
		return "", fmt.Errorf("%w: operation is not a valid pending pair lifetime", ErrSignedCapabilityReplay)
	}
	return signedOAuthMCPRetirementResource(op)
}

func signedOAuthMCPRetirementResource(op SignedOAuthMCPOperation) (string, error) {
	if !validSHA256Hex(op.Fingerprint) || !signedOAuthMCPOperationPhaseKnown(op.Phase) {
		return "", fmt.Errorf("%w: operation has no valid retirement fingerprint", ErrSignedCapabilityReplay)
	}
	_, kind, err := signedOAuthMCPOperationSlot(op.ReplayKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(kind))
	return op.Fingerprint + "." + hex.EncodeToString(sum[:]), nil
}

// SignedOAuthMCPRetirementResourceMatches validates a frozen hash-only locator
// against one durable receipt. Callers still tenant-scope their operation scan
// and compare Binding.AgentID before using the receipt's private exact subject.
func SignedOAuthMCPRetirementResourceMatches(resource string, op SignedOAuthMCPOperation) bool {
	want, err := signedOAuthMCPRetirementResource(op)
	return err == nil && resource == want
}

// SignedOAuthMCPRetirementPending reports phases whose published pair lifetime
// has not reached a terminal teardown receipt. Pre-publication candidates are
// reconciler debt rather than a published pair and follow expired_incomplete.
func SignedOAuthMCPRetirementPending(phase SignedOAuthMCPOperationPhase) bool {
	switch phase {
	case SignedOAuthMCPPhasePublished, SignedOAuthMCPPhaseRemovalAdmitted,
		SignedOAuthMCPPhaseRemovalRevisionCommitted, SignedOAuthMCPPhaseCatalogUnpublished,
		SignedOAuthMCPPhaseTeardownReceipted:
		return true
	default:
		return false
	}
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// Advance atomically records the next legal signed-capability recovery phase. The caller
// must use the returned value for any subsequent transition; stale writers lose
// on the exact operation EventID rather than overwriting one another.
func (s *SignedOAuthMCPOperationStore) Advance(ctx context.Context, current SignedOAuthMCPOperation, next SignedOAuthMCPOperationPhase, revisionID string) (SignedOAuthMCPOperation, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	if !signedOAuthMCPTransitionAllowed(current.Phase, next) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: %s -> %s", ErrSignedCapabilityTransition, current.Phase, next)
	}
	quad, kind, err := signedOAuthMCPOperationSlot(current.ReplayKey)
	if err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	nextRecord := current
	nextRecord.Phase = next
	nextRecord.RevisionID = revisionID
	nextRecord.EventID = state.NewEventID()
	bytes, err := json.Marshal(nextRecord)
	if err != nil {
		return SignedOAuthMCPOperation{}, fmt.Errorf("marshal signed capability operation: %w", err)
	}
	err = s.state.SaveIf(ctx, []state.SlotExpectation{{Identity: quad, Kind: kind, ExpectedEventID: current.EventID}}, state.StateRecord{ID: nextRecord.EventID, Identity: quad, Kind: kind, Bytes: bytes})
	if errors.Is(err, state.ErrConditionFailed) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: operation changed concurrently", ErrSignedCapabilityReplay)
	}
	if err != nil {
		return SignedOAuthMCPOperation{}, fmt.Errorf("advance signed capability operation: %w", err)
	}
	return nextRecord, nil
}

func (s *SignedOAuthMCPOperationStore) saveExact(ctx context.Context, current, next SignedOAuthMCPOperation, action string) (SignedOAuthMCPOperation, error) {
	quad, kind, err := signedOAuthMCPOperationSlot(current.ReplayKey)
	if err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	if current.EventID == "" || next.ReplayKey != current.ReplayKey || next.Fingerprint != current.Fingerprint ||
		!reflect.DeepEqual(next.Binding, current.Binding) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: operation mutation changed immutable authority", ErrSignedCapabilityReplay)
	}
	next.EventID = state.NewEventID()
	encoded, err := json.Marshal(next)
	if err != nil {
		return SignedOAuthMCPOperation{}, fmt.Errorf("marshal signed capability operation: %w", err)
	}
	err = s.state.SaveIf(ctx, []state.SlotExpectation{{Identity: quad, Kind: kind, ExpectedEventID: current.EventID}}, state.StateRecord{ID: next.EventID, Identity: quad, Kind: kind, Bytes: encoded})
	if errors.Is(err, state.ErrConditionFailed) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: operation changed concurrently", ErrSignedCapabilityReplay)
	}
	if err != nil {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%s: %w", action, err)
	}
	return next, nil
}

func effectiveAuthorityGeneration(generation uint64) uint64 {
	if generation == 0 {
		return 1
	}
	return generation
}

// AcquirePublisher CAS-mints one opaque publisher epoch for an operation whose
// desired revision is committed or already published. A successor runtime
// takes over by replacing the epoch under the exact operation EventID; every
// provider and connection from an older epoch becomes inert on its next
// durable authorization check. The epoch is internal recovery state only: it
// never rides a revision, Protocol response, broker actor assertion, or audit.
func (s *SignedOAuthMCPOperationStore) AcquirePublisher(ctx context.Context, current SignedOAuthMCPOperation) (SignedOAuthMCPOperation, error) {
	if err := ctx.Err(); err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	if current.Phase != SignedOAuthMCPPhaseRevisionCommitted && current.Phase != SignedOAuthMCPPhasePublished {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: phase %q cannot acquire a publisher", ErrSignedCapabilityTransition, current.Phase)
	}
	quad, kind, err := signedOAuthMCPOperationSlot(current.ReplayKey)
	if err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	next := current
	next.PublisherEpoch = string(state.NewEventID())
	next.EventID = state.NewEventID()
	encoded, err := json.Marshal(next)
	if err != nil {
		return SignedOAuthMCPOperation{}, fmt.Errorf("marshal signed capability publisher epoch: %w", err)
	}
	err = s.state.SaveIf(ctx, []state.SlotExpectation{{Identity: quad, Kind: kind, ExpectedEventID: current.EventID}}, state.StateRecord{ID: next.EventID, Identity: quad, Kind: kind, Bytes: encoded})
	if errors.Is(err, state.ErrConditionFailed) {
		return SignedOAuthMCPOperation{}, fmt.Errorf("%w: publisher changed concurrently", ErrSignedCapabilityReplay)
	}
	if err != nil {
		return SignedOAuthMCPOperation{}, fmt.Errorf("acquire signed capability publisher: %w", err)
	}
	return next, nil
}

// AuthorizeSignedCapabilityUse exact-loads the durable pair-lifetime record
// before a pair-owned provider returns or uses a bearer. Normal data-plane use
// requires the published phase. The narrowly marked private preparation path
// may additionally run while the desired revision is committed; claimed and
// every removal/terminal phase always deny. A missing, malformed, stale, or
// unreadable epoch fails closed.
func (s *SignedOAuthMCPOperationStore) AuthorizeSignedCapabilityUse(ctx context.Context, tenant, operationKind, publisherEpoch string, preparation bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(operationKind) == "" || strings.TrimSpace(publisherEpoch) == "" {
		return fmt.Errorf("%w: signed capability publisher binding is incomplete", ErrSignedCapabilityPending)
	}
	quad := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "__agentcfg__", SessionID: "__signed_oauth_mcp__"}}
	op, err := s.load(ctx, quad, operationKind)
	if err != nil {
		return err
	}
	expectedKind, err := s.Kind(op.ReplayKey)
	if err != nil || expectedKind != operationKind || op.ReplayKey.TenantID != tenant || op.Binding.TenantID != tenant ||
		op.Fingerprint == "" || op.Fingerprint != SignedOAuthMCPPairFingerprint(op.Binding) || !signedOAuthMCPOperationPhaseKnown(op.Phase) ||
		op.PublisherEpoch == "" || op.PublisherEpoch != publisherEpoch {
		return fmt.Errorf("%w: signed capability publisher is stale or corrupt", ErrSignedCapabilityPending)
	}
	if op.Phase == SignedOAuthMCPPhasePublished {
		return nil
	}
	if preparation && op.Phase == SignedOAuthMCPPhaseRevisionCommitted {
		return nil
	}
	return fmt.Errorf("%w: signed capability phase %q denies bearer use", ErrSignedCapabilityPending, op.Phase)
}

// FencePublication holds the StateStore's exact operation-slot lock while fn
// publishes one already-prepared local catalog/registry generation. It performs
// no network I/O and mutates no durable state. A concurrent removal must first
// SaveIf-advance this same EventID to removal_admitted, so exactly one side
// wins before catalog dispatch becomes visible.
func (s *SignedOAuthMCPOperationStore) FencePublication(ctx context.Context, current SignedOAuthMCPOperation, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if current.Phase != SignedOAuthMCPPhaseRevisionCommitted && current.Phase != SignedOAuthMCPPhasePublished {
		return fmt.Errorf("%w: phase %q cannot admit publication", ErrSignedCapabilityTransition, current.Phase)
	}
	quad, kind, err := signedOAuthMCPOperationSlot(current.ReplayKey)
	if err != nil {
		return err
	}
	// Publication is a short, process-local irreversible commit. Once caller
	// cancellation has passed the check above, do not return a cancellation
	// that could be mistaken for proof the callback did not publish.
	err = s.state.FenceIf(context.WithoutCancel(ctx), state.SlotExpectation{Identity: quad, Kind: kind, ExpectedEventID: current.EventID}, fn)
	if errors.Is(err, state.ErrConditionFailed) {
		return fmt.Errorf("%w: publication operation changed concurrently", ErrSignedCapabilityReplay)
	}
	if err != nil {
		return fmt.Errorf("fence signed capability publication: %w", err)
	}
	return nil
}

func (s *SignedOAuthMCPOperationStore) load(ctx context.Context, quad identity.Quadruple, kind string) (SignedOAuthMCPOperation, error) {
	record, err := s.state.Load(ctx, quad, kind)
	if err != nil {
		return SignedOAuthMCPOperation{}, fmt.Errorf("load signed capability operation: %w", err)
	}
	var operation SignedOAuthMCPOperation
	if err := json.Unmarshal(record.Bytes, &operation); err != nil {
		return SignedOAuthMCPOperation{}, fmt.Errorf("decode signed capability operation: %w", err)
	}
	if err := validateSignedOAuthMCPOperation(operation); err != nil {
		return SignedOAuthMCPOperation{}, err
	}
	operation.EventID = record.ID
	return operation, nil
}

func validateSignedOAuthMCPOperation(op SignedOAuthMCPOperation) error {
	if op.Fingerprint == "" || op.Fingerprint != SignedOAuthMCPPairFingerprint(op.Binding) || !signedOAuthMCPOperationPhaseKnown(op.Phase) {
		return fmt.Errorf("%w: corrupt signed capability operation", ErrSignedCapabilityReplay)
	}
	legacyExpired := op.Phase == SignedOAuthMCPPhaseExpiredIncomplete && op.ExpiryFromPhase == "" &&
		op.ExpiryCandidateRevisionID == "" && op.ExpiredAttemptCount == 0 && op.LastExpiredAt.IsZero() && op.LastExpiredRevisionID == ""
	if legacyExpired {
		return nil
	}
	switch op.Phase {
	case SignedOAuthMCPPhaseExpiryAdmitted, SignedOAuthMCPPhaseExpiredIncomplete:
		if (op.ExpiryFromPhase != SignedOAuthMCPPhaseClaimed && op.ExpiryFromPhase != SignedOAuthMCPPhaseRevisionCommitted) ||
			op.ExpiredAttemptCount == 0 || op.LastExpiredAt.IsZero() || op.LastExpiredRevisionID != op.ExpiryCandidateRevisionID {
			return fmt.Errorf("%w: inconsistent signed capability expiry metadata", ErrSignedCapabilityReplay)
		}
		if op.ExpiryFromPhase == SignedOAuthMCPPhaseRevisionCommitted && (op.ExpiryCandidateRevisionID == "" || op.RevisionID != op.ExpiryCandidateRevisionID) {
			return fmt.Errorf("%w: committed expiry metadata lost its candidate", ErrSignedCapabilityReplay)
		}
	default:
		if op.ExpiryFromPhase != "" || op.ExpiryCandidateRevisionID != "" {
			return fmt.Errorf("%w: non-expiry phase carries pending expiry metadata", ErrSignedCapabilityReplay)
		}
	}
	return nil
}

func signedOAuthMCPOperationSlot(key SignedOAuthMCPReplayKey) (identity.Quadruple, string, error) {
	parts := []string{key.TenantID, key.TrustAnchorName, key.Issuer, key.KeyID, key.JTI}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return identity.Quadruple{}, "", fmt.Errorf("%w: replay key component is empty", ErrSignedCapabilityAuthority)
		}
	}
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	kind := "agentcfg.signed_oauth_mcp." + hex.EncodeToString(h.Sum(nil))
	return identity.Quadruple{Identity: identity.Identity{TenantID: key.TenantID, UserID: "__agentcfg__", SessionID: "__signed_oauth_mcp__"}}, kind, nil
}

func signedOAuthMCPTransitionAllowed(from, to SignedOAuthMCPOperationPhase) bool {
	switch from {
	case SignedOAuthMCPPhaseClaimed:
		return to == SignedOAuthMCPPhaseRevisionCommitted
	case SignedOAuthMCPPhaseRevisionCommitted:
		return to == SignedOAuthMCPPhasePublished
	case SignedOAuthMCPPhasePublished:
		return to == SignedOAuthMCPPhaseRemovalAdmitted
	case SignedOAuthMCPPhaseRemovalAdmitted:
		return to == SignedOAuthMCPPhasePublished || to == SignedOAuthMCPPhaseRemovalRevisionCommitted
	case SignedOAuthMCPPhaseRemovalRevisionCommitted:
		return to == SignedOAuthMCPPhaseCatalogUnpublished
	case SignedOAuthMCPPhaseCatalogUnpublished:
		return to == SignedOAuthMCPPhaseTeardownReceipted
	case SignedOAuthMCPPhaseTeardownReceipted:
		return to == SignedOAuthMCPPhaseRemoved
	default:
		return false
	}
}

func signedOAuthMCPOperationPhaseKnown(phase SignedOAuthMCPOperationPhase) bool {
	switch phase {
	case SignedOAuthMCPPhaseClaimed, SignedOAuthMCPPhaseRevisionCommitted,
		SignedOAuthMCPPhasePublished, SignedOAuthMCPPhaseRemovalAdmitted, SignedOAuthMCPPhaseRemovalRevisionCommitted,
		SignedOAuthMCPPhaseCatalogUnpublished, SignedOAuthMCPPhaseTeardownReceipted,
		SignedOAuthMCPPhaseRemoved, SignedOAuthMCPPhaseExpiryAdmitted, SignedOAuthMCPPhaseExpiredIncomplete:
		return true
	default:
		return false
	}
}
