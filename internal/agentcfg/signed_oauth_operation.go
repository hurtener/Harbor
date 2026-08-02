package agentcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	SignedOAuthMCPPhaseExpiredIncomplete        SignedOAuthMCPOperationPhase = "expired_incomplete"
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
	ReplayKey   SignedOAuthMCPReplayKey      `json:"replay_key"`
	Binding     SignedOAuthMCPBinding        `json:"binding"`
	Fingerprint string                       `json:"fingerprint"`
	ExpiresAt   time.Time                    `json:"expires_at"`
	Phase       SignedOAuthMCPOperationPhase `json:"phase"`
	RevisionID  string                       `json:"revision_id,omitempty"`
	EventID     state.EventID                `json:"-"`
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
	operation := SignedOAuthMCPOperation{ReplayKey: key, Binding: binding, Fingerprint: fingerprint, ExpiresAt: expiresAt.UTC(), Phase: SignedOAuthMCPPhaseClaimed, EventID: state.NewEventID()}
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
	if existing.ReplayKey != key || existing.Fingerprint != fingerprint {
		return SignedOAuthMCPOperation{}, false, fmt.Errorf("%w: jti already binds another capability", ErrSignedCapabilityReplay)
	}
	return existing, false, nil
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
	operation.EventID = record.ID
	return operation, nil
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
	if to == SignedOAuthMCPPhaseExpiredIncomplete {
		return from == SignedOAuthMCPPhaseClaimed || from == SignedOAuthMCPPhaseRevisionCommitted
	}
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
		SignedOAuthMCPPhaseRemoved, SignedOAuthMCPPhaseExpiredIncomplete:
		return true
	default:
		return false
	}
}
