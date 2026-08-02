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

var (
	// ErrSignedCapabilityReplay is returned when an authority JTI has already
	// claimed a different immutable capability fingerprint.
	ErrSignedCapabilityReplay = errors.New("agentcfg: signed oauth mcp capability replay")
	// ErrSignedCapabilityTransition marks a transition that is not in D-401's
	// one pair-lifetime operation graph.
	ErrSignedCapabilityTransition = errors.New("agentcfg: invalid signed oauth mcp capability operation transition")
)

// SignedOAuthMCPOperationPhase is a durable pair-lifetime operation phase.
type SignedOAuthMCPOperationPhase string

const (
	SignedOAuthMCPPhaseClaimed                  SignedOAuthMCPOperationPhase = "claimed"
	SignedOAuthMCPPhaseRevisionCommitted        SignedOAuthMCPOperationPhase = "revision_committed"
	SignedOAuthMCPPhasePublished                SignedOAuthMCPOperationPhase = "published"
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

// SignedOAuthMCPOperationStore persists D-401's tenant-scoped anti-replay
// operation record through the mandatory StateStore SaveIf primitive.
type SignedOAuthMCPOperationStore struct{ state state.StateStore }

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

// Advance atomically records the next legal D-401 recovery phase. The caller
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
		return to == SignedOAuthMCPPhaseRemovalRevisionCommitted
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
		SignedOAuthMCPPhasePublished, SignedOAuthMCPPhaseRemovalRevisionCommitted,
		SignedOAuthMCPPhaseCatalogUnpublished, SignedOAuthMCPPhaseTeardownReceipted,
		SignedOAuthMCPPhaseRemoved, SignedOAuthMCPPhaseExpiredIncomplete:
		return true
	default:
		return false
	}
}
