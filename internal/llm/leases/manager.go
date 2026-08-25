// Package leases provides the durable execution-edge reservation contract.
//
// A grant's lease is a capability, not a counter.  The durable manager turns
// it into a per-attempt reservation before a provider call and settles that
// reservation with the content-free receipt afterwards.  Both records are
// updated through StateStore.SaveBatchIf, so two runtime processes sharing a
// store cannot consume the same lease units concurrently.
package leases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	leasePrefix          = "harbor.internal/inference.lease/"
	attemptPrefix        = "harbor.internal/inference.attempt/"
	pendingReceiptPrefix = "harbor.internal/inference.receipt.pending/"
	maxCASRetries        = 8
)

var (
	ErrInvalidRequest  = errors.New("llm/leases: invalid reservation request")
	ErrInsufficient    = llm.ErrLeaseInsufficient
	ErrAttemptConflict = llm.ErrLeaseConflict
)

// TopUpRequest advances a lease epoch. Existing reservations remain bound to
// their old epoch; new attempts use the new capacity and epoch. The exact
// grant, organization, runtime, admitted identity, and agent binding must be
// preserved.
type TopUpRequest struct {
	GrantID        string
	LeaseID        string
	OrganizationID string
	RuntimeID      string
	AgentID        string
	Identity       identity.Quadruple
	Epoch          uint64
	Capacity       int64
	ExpiresAt      time.Time
}

// Store is the durable reservation seam.  It is intentionally backed by the
// mandatory StateStore contract rather than a process-local map.
type Store struct {
	state state.StateStore
	clock func() time.Time
}

// New constructs a durable reservation manager over an existing StateStore.
func New(st state.StateStore, clock func() time.Time) (*Store, error) {
	if st == nil {
		return nil, fmt.Errorf("llm/leases: state store is required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Store{state: st, clock: clock}, nil
}

// Reserve atomically claims units for AttemptID. Replaying the same attempt
// returns the original reservation; reusing it with different claims fails.
// A replay after the reservation expiry atomically releases the stale claim
// and returns the terminal expired record instead of remaining in flight.
func (s *Store) Reserve(ctx context.Context, req llm.LeaseReservationRequest) (llm.LeaseReservation, error) {
	if err := validateRequest(req); err != nil {
		return llm.LeaseReservation{}, err
	}
	leaseQ := identity.InternalCoordinationQuadruple()
	leaseKind := leasePrefix + req.LeaseID
	attemptKind := attemptPrefix + req.AttemptID
	now := s.clock().UTC()
	for range maxCASRetries {
		leaseRec, lease, leaseErr := s.loadLease(ctx, leaseQ, leaseKind)
		if leaseErr != nil && !errors.Is(leaseErr, state.ErrNotFound) {
			return llm.LeaseReservation{}, leaseErr
		}
		attemptRec, attempt, attemptErr := s.loadAttempt(ctx, leaseQ, attemptKind)
		if attemptErr != nil && !errors.Is(attemptErr, state.ErrNotFound) {
			return llm.LeaseReservation{}, attemptErr
		}
		if attemptErr == nil {
			if !attempt.matches(req) {
				return llm.LeaseReservation{}, ErrAttemptConflict
			}
			// A legitimate top-up may have advanced the durable lease while
			// this attempt remains bound to its original epoch and capacity.
			// The attempt carries those immutable replay claims; the current
			// lease only needs to preserve the exact authority binding.
			if leaseErr != nil || !lease.matches(req) {
				return llm.LeaseReservation{}, ErrAttemptConflict
			}
			if attempt.Status == "reserved" && !now.Before(attempt.ExpiresAt) {
				if lease.Reserved < attempt.Units {
					return llm.LeaseReservation{}, ErrAttemptConflict
				}
				lease.Reserved -= attempt.Units
				attempt.Status = "expired"
				attempt.SettledAt = now
				leaseBody, err := json.Marshal(lease)
				if err != nil {
					return llm.LeaseReservation{}, fmt.Errorf("llm/leases: encode expired lease: %w", err)
				}
				attemptBody, err := json.Marshal(attempt)
				if err != nil {
					return llm.LeaseReservation{}, fmt.Errorf("llm/leases: encode expired attempt: %w", err)
				}
				leaseNext := state.NewInternalRecord(state.NewEventID(), leaseQ, leaseKind, leaseBody)
				attemptNext := state.NewInternalRecord(state.NewEventID(), leaseQ, attemptKind, attemptBody)
				if err := s.state.SaveBatchIf(ctx,
					[]state.SlotExpectation{
						state.InternalSlotExpectation(leaseQ, leaseKind, leaseRec.ID),
						state.InternalSlotExpectation(leaseQ, attemptKind, attemptRec.ID),
					},
					[]state.StateRecord{leaseNext, attemptNext}); err != nil {
					if errors.Is(err, state.ErrConditionFailed) {
						continue
					}
					return llm.LeaseReservation{}, fmt.Errorf("llm/leases: expire stale reservation: %w", err)
				}
				return attempt.reservation(true), nil
			}
			return attempt.reservation(true), nil
		}
		if !now.Before(req.ExpiresAt) {
			return llm.LeaseReservation{}, fmt.Errorf("%w: reservation expired", ErrInsufficient)
		}
		if leaseErr != nil {
			lease = newLeaseState(req)
		} else if !lease.matches(req) || lease.Epoch != req.Epoch || lease.Capacity != req.Capacity || !lease.ExpiresAt.Equal(req.ExpiresAt) {
			return llm.LeaseReservation{}, fmt.Errorf("%w: lease epoch/capacity changed", ErrAttemptConflict)
		}
		if lease.ExpiresAt.IsZero() || !now.Before(lease.ExpiresAt) {
			return llm.LeaseReservation{}, fmt.Errorf("%w: lease expired", ErrInsufficient)
		}
		if lease.Capacity-lease.Consumed-lease.Reserved < req.Units {
			return llm.LeaseReservation{}, fmt.Errorf("%w: available=%d requested=%d", ErrInsufficient, lease.Capacity-lease.Consumed-lease.Reserved, req.Units)
		}
		lease.Reserved += req.Units
		attempt = attemptState{AttemptID: req.AttemptID, LogicalCallID: req.LogicalCallID, AttemptNonce: req.AttemptNonce, GrantID: req.GrantID, LeaseID: req.LeaseID, OrganizationID: req.OrganizationID, RuntimeID: req.RuntimeID, AgentID: req.AgentID, Epoch: req.Epoch, Capacity: req.Capacity, Units: req.Units, ExpiresAt: req.ExpiresAt, Identity: req.Identity, Status: "reserved"}
		leaseBody, err := json.Marshal(lease)
		if err != nil {
			return llm.LeaseReservation{}, fmt.Errorf("llm/leases: encode lease: %w", err)
		}
		attemptBody, err := json.Marshal(attempt)
		if err != nil {
			return llm.LeaseReservation{}, fmt.Errorf("llm/leases: encode attempt: %w", err)
		}
		leaseNext := state.NewInternalRecord(state.NewEventID(), leaseQ, leaseKind, leaseBody)
		attemptNext := state.NewInternalRecord(state.NewEventID(), leaseQ, attemptKind, attemptBody)
		exps := []state.SlotExpectation{state.InternalSlotExpectation(leaseQ, leaseKind, expectedID(leaseRec)), state.InternalSlotExpectation(leaseQ, attemptKind, expectedID(attemptRec))}
		if err := s.state.SaveBatchIf(ctx, exps, []state.StateRecord{leaseNext, attemptNext}); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return llm.LeaseReservation{}, fmt.Errorf("llm/leases: reserve: %w", err)
		}
		return attempt.reservation(false), nil
	}
	return llm.LeaseReservation{}, fmt.Errorf("%w: concurrent reservation did not converge", state.ErrConditionFailed)
}

// Settle atomically charges provider-reported usage for every terminal outcome,
// releases only the unused reservation, and stores the receipt for later
// outbox reconciliation. Actual usage may exceed the pre-call estimate; that
// one-call overshoot is recorded rather than discarded.
func (s *Store) Settle(ctx context.Context, req llm.LeaseSettlement) error {
	if req.AttemptID == "" || req.Receipt.ReceiptID == "" || req.LogicalCallID == "" || req.AttemptNonce == "" || req.Units < 0 || req.Units != int64(req.Receipt.TotalTokens) {
		return ErrInvalidRequest
	}
	now := req.Now
	if now.IsZero() {
		now = s.clock().UTC()
	}
	q := identity.InternalCoordinationQuadruple()
	pendingKind := pendingReceiptKind(req.AttemptID)
	for range maxCASRetries {
		ar, a, err := s.loadAttempt(ctx, q, attemptPrefix+req.AttemptID)
		if err != nil {
			return err
		}
		lr, l, err := s.loadLease(ctx, q, leasePrefix+a.LeaseID)
		if err != nil {
			return err
		}
		if a.LogicalCallID != req.LogicalCallID || a.AttemptNonce != req.AttemptNonce ||
			req.Receipt.LogicalCallID != a.LogicalCallID || req.Receipt.AttemptNonce != a.AttemptNonce ||
			req.Receipt.ReceiptID != a.AttemptID || req.Receipt.IdempotencyKey != a.AttemptID || !a.matchesReceipt(req.Receipt) || !l.matchesAttempt(a) {
			return ErrAttemptConflict
		}
		if a.ReceiptHash != "" {
			if a.ReceiptHash == receiptHash(req.Receipt) {
				return s.ensurePendingReceipt(ctx, req.Receipt)
			}
			return ErrAttemptConflict
		}
		wasReserved := a.Status == "reserved"
		if !wasReserved && a.Status != "expired" {
			return ErrAttemptConflict
		}
		if wasReserved {
			if a.Units > l.Reserved {
				return ErrAttemptConflict
			}
			l.Reserved -= a.Units
		}
		// Provider-reported usage is authoritative for every terminal
		// outcome. The pre-call reservation is an estimated total-call
		// bound; tokenizer differences can produce one-call overshoot, which
		// is charged rather than discarded. Only the unused reservation is
		// released.
		a.ConsumedUnits = req.Units
		if req.Units > 0 {
			a.Status = "consumed"
			l.Consumed += req.Units
		} else {
			a.Status = "released"
		}
		a.Receipt = req.Receipt
		a.ReceiptHash = receiptHash(req.Receipt)
		a.SettledAt = now
		lb, err := json.Marshal(l)
		if err != nil {
			return fmt.Errorf("llm/leases: encode settled lease: %w", err)
		}
		ab, err := json.Marshal(a)
		if err != nil {
			return fmt.Errorf("llm/leases: encode settled attempt: %w", err)
		}
		ln := state.NewInternalRecord(state.NewEventID(), q, leasePrefix+a.LeaseID, lb)
		an := state.NewInternalRecord(state.NewEventID(), q, attemptPrefix+a.AttemptID, ab)
		pendingBody, err := json.Marshal(pendingReceiptState{AttemptID: a.AttemptID, ReceiptHash: a.ReceiptHash, Receipt: req.Receipt})
		if err != nil {
			return fmt.Errorf("llm/leases: encode pending receipt: %w", err)
		}
		pn := state.NewInternalRecord(state.NewEventID(), q, pendingKind, pendingBody)
		if err := s.state.SaveBatchIf(ctx,
			[]state.SlotExpectation{
				state.InternalSlotExpectation(q, leasePrefix+a.LeaseID, lr.ID),
				state.InternalSlotExpectation(q, attemptPrefix+a.AttemptID, ar.ID),
				state.InternalSlotExpectation(q, pendingKind, ""),
			},
			[]state.StateRecord{ln, an, pn}); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return fmt.Errorf("llm/leases: settle: %w", err)
		}
		return nil
	}
	return fmt.Errorf("%w: concurrent settlement did not converge", state.ErrConditionFailed)
}

// TopUp durably advances a lease's epoch and capacity. It never rewinds an
// epoch or mutates already settled attempt records.
func (s *Store) TopUp(ctx context.Context, req TopUpRequest) error {
	if err := validateTopUpRequest(req); err != nil {
		return ErrInvalidRequest
	}
	q := identity.InternalCoordinationQuadruple()
	kind := leasePrefix + req.LeaseID
	for range maxCASRetries {
		rec, lease, err := s.loadLease(ctx, q, kind)
		if err != nil {
			return err
		}
		if !lease.matchesTopUp(req) || req.Epoch <= lease.Epoch || req.Capacity < lease.Capacity {
			return ErrAttemptConflict
		}
		lease.Epoch, lease.Capacity, lease.ExpiresAt = req.Epoch, req.Capacity, req.ExpiresAt
		body, marshalErr := json.Marshal(lease)
		if marshalErr != nil {
			return fmt.Errorf("llm/leases: encode top-up: %w", marshalErr)
		}
		next := state.NewInternalRecord(state.NewEventID(), q, kind, body)
		if err := s.state.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, kind, rec.ID)}, next); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return fmt.Errorf("llm/leases: top-up: %w", err)
		}
		return nil
	}
	return fmt.Errorf("%w: concurrent top-up did not converge", state.ErrConditionFailed)
}

// ApplySuccessor implements llm.LeaseSuccessorApplier. It advances exactly
// one already-authenticated grant generation while preserving all local
// reservations and settled consumption. An absent lease is created at the
// successor generation; replaying the exact successor is a no-op. A stale or
// different grant at either epoch fails closed.
func (s *Store) ApplySuccessor(ctx context.Context, predecessor, successor llm.ExternalGrant) error {
	predecessorHash, err := llm.CanonicalExternalGrantHash(predecessor)
	if err != nil {
		return fmt.Errorf("%w: predecessor fingerprint", ErrInvalidRequest)
	}
	successorHash, err := llm.CanonicalExternalGrantHash(successor)
	if err != nil {
		return fmt.Errorf("%w: successor fingerprint", ErrInvalidRequest)
	}
	successorCanonical, err := llm.MarshalCanonicalExternalGrant(successor)
	if err != nil {
		return fmt.Errorf("%w: successor canonical bytes", ErrInvalidRequest)
	}
	if predecessor.Lease.LeaseID == "" || successor.Lease.LeaseID != predecessor.Lease.LeaseID ||
		successor.Lease.Epoch != predecessor.Lease.Epoch+1 || !grantBindingMatches(predecessor, successor) {
		return ErrInvalidRequest
	}
	predecessorRemaining := predecessor.Lease.RemainingTokens()
	successorRemaining := successor.Lease.RemainingTokens()
	if successorRemaining < predecessorRemaining {
		return ErrInvalidRequest
	}
	delta := successorRemaining - predecessorRemaining
	q := identity.InternalCoordinationQuadruple()
	kind := leasePrefix + predecessor.Lease.LeaseID
	for range maxCASRetries {
		rec, lease, loadErr := s.loadLease(ctx, q, kind)
		if errors.Is(loadErr, state.ErrNotFound) {
			created := leaseStateFromGrant(predecessorHash, successor, successorHash, successorCanonical)
			body, marshalErr := json.Marshal(created)
			if marshalErr != nil {
				return fmt.Errorf("llm/leases: encode successor lease: %w", marshalErr)
			}
			next := state.NewInternalRecord(state.NewEventID(), q, kind, body)
			if saveErr := s.state.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, kind, "")}, next); saveErr != nil {
				if errors.Is(saveErr, state.ErrConditionFailed) {
					continue
				}
				return fmt.Errorf("llm/leases: create successor: %w", saveErr)
			}
			return nil
		}
		if loadErr != nil {
			return loadErr
		}
		if !lease.matchesGrantBinding(predecessor) {
			return ErrAttemptConflict
		}
		if lease.Epoch == successor.Lease.Epoch {
			if lease.GrantFingerprint == successorHash && lease.ExpiresAt.Equal(successor.Lease.ExpiresAt) {
				return nil
			}
			legacyInitial := predecessor.Lease.Epoch == 0 && successor.Lease.Epoch == 1 &&
				lease.Epoch == 1 && lease.GrantFingerprint == predecessorHash
			if !legacyInitial {
				return ErrAttemptConflict
			}
		}
		expectedPredecessorEpoch := predecessor.Lease.Epoch
		if expectedPredecessorEpoch == 0 {
			expectedPredecessorEpoch = 1
		}
		if lease.Epoch != expectedPredecessorEpoch ||
			(lease.GrantFingerprint != "" && lease.GrantFingerprint != predecessorHash) ||
			!lease.ExpiresAt.Equal(predecessor.Lease.ExpiresAt) ||
			lease.Capacity <= 0 ||
			delta > math.MaxInt64-lease.Capacity {
			return ErrAttemptConflict
		}
		lease.Epoch = successor.Lease.Epoch
		lease.Capacity += delta
		lease.ExpiresAt = successor.Lease.ExpiresAt
		if lease.RootGrantFingerprint == "" {
			lease.RootGrantFingerprint = predecessorHash
		}
		lease.GrantFingerprint = successorHash
		lease.GrantCanonical = append(json.RawMessage(nil), successorCanonical...)
		body, marshalErr := json.Marshal(lease)
		if marshalErr != nil {
			return fmt.Errorf("llm/leases: encode applied successor: %w", marshalErr)
		}
		next := state.NewInternalRecord(state.NewEventID(), q, kind, body)
		if saveErr := s.state.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, kind, rec.ID)}, next); saveErr != nil {
			if errors.Is(saveErr, state.ErrConditionFailed) {
				continue
			}
			return fmt.Errorf("llm/leases: apply successor: %w", saveErr)
		}
		return nil
	}
	return fmt.Errorf("%w: concurrent successor application did not converge", state.ErrConditionFailed)
}

// ResolveSuccessor returns the latest canonical signed successor for root.
// The root fingerprint is exact, so a mutated or unrelated caller cannot use
// a LeaseID as authority. The returned grant remains untrusted: the wrapper
// validates lineage and runs ordinary strict verification before use.
func (s *Store) ResolveSuccessor(ctx context.Context, root llm.ExternalGrant) (llm.ExternalGrant, bool, error) {
	rootHash, err := llm.CanonicalExternalGrantHash(root)
	if err != nil || root.Lease.LeaseID == "" {
		return llm.ExternalGrant{}, false, ErrInvalidRequest
	}
	_, lease, err := s.loadLease(ctx, identity.InternalCoordinationQuadruple(), leasePrefix+root.Lease.LeaseID)
	if errors.Is(err, state.ErrNotFound) {
		return llm.ExternalGrant{}, false, nil
	}
	if err != nil {
		return llm.ExternalGrant{}, false, err
	}
	if !lease.matchesGrantBinding(root) {
		return llm.ExternalGrant{}, false, fmt.Errorf("%w: root binding mismatch", ErrAttemptConflict)
	}
	rootEpoch := root.Lease.Epoch
	if rootEpoch == 0 {
		rootEpoch = 1
	}
	if lease.RootGrantFingerprint == "" && lease.Epoch == rootEpoch {
		if lease.GrantFingerprint == "" || lease.GrantFingerprint == rootHash {
			return llm.ExternalGrant{}, false, nil
		}
		return llm.ExternalGrant{}, false, fmt.Errorf("%w: root fingerprint mismatch", ErrAttemptConflict)
	}
	if lease.Epoch < rootEpoch || lease.RootGrantFingerprint != rootHash || len(lease.GrantCanonical) == 0 {
		return llm.ExternalGrant{}, false, fmt.Errorf("%w: successor lineage root mismatch", ErrAttemptConflict)
	}
	resolved, err := llm.UnmarshalCanonicalExternalGrant(lease.GrantCanonical)
	if err != nil {
		return llm.ExternalGrant{}, false, fmt.Errorf("%w: successor canonical bytes invalid", ErrAttemptConflict)
	}
	resolvedHash, err := llm.CanonicalExternalGrantHash(resolved)
	if err != nil || resolvedHash != lease.GrantFingerprint || resolved.Lease.Epoch != lease.Epoch ||
		!resolved.Lease.ExpiresAt.Equal(lease.ExpiresAt) || !lease.matchesGrantBinding(resolved) {
		return llm.ExternalGrant{}, false, fmt.Errorf("%w: successor canonical state mismatch", ErrAttemptConflict)
	}
	return resolved, true, nil
}

// Release returns reserved units without consuming them. It is idempotent for
// an already closed attempt and is used by cancellation/expiry reconciliation.
func (s *Store) Release(ctx context.Context, attemptID string) error {
	if strings.TrimSpace(attemptID) == "" {
		return ErrInvalidRequest
	}
	q := identity.InternalCoordinationQuadruple()
	for range maxCASRetries {
		ar, attempt, err := s.loadAttempt(ctx, q, attemptPrefix+attemptID)
		if err != nil {
			return err
		}
		if attempt.Status != "reserved" {
			return nil
		}
		lr, lease, err := s.loadLease(ctx, q, leasePrefix+attempt.LeaseID)
		if err != nil {
			return err
		}
		if lease.Reserved < attempt.Units {
			return ErrAttemptConflict
		}
		lease.Reserved -= attempt.Units
		attempt.Status = "released"
		lb, err := json.Marshal(lease)
		if err != nil {
			return fmt.Errorf("llm/leases: encode released lease: %w", err)
		}
		ab, err := json.Marshal(attempt)
		if err != nil {
			return fmt.Errorf("llm/leases: encode released attempt: %w", err)
		}
		ln := state.NewInternalRecord(state.NewEventID(), q, leasePrefix+attempt.LeaseID, lb)
		an := state.NewInternalRecord(state.NewEventID(), q, attemptPrefix+attemptID, ab)
		if err := s.state.SaveBatchIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, leasePrefix+attempt.LeaseID, lr.ID), state.InternalSlotExpectation(q, attemptPrefix+attemptID, ar.ID)}, []state.StateRecord{ln, an}); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return fmt.Errorf("llm/leases: release: %w", err)
		}
		return nil
	}
	return fmt.Errorf("%w: concurrent release did not converge", state.ErrConditionFailed)
}

// Expire marks reservations whose bounded lease has elapsed as expired. This
// returns their reserved capacity while preserving the distinct terminal state
// that permits a provider result already in flight to settle its late actual
// usage. The bounded maintenance scan is intended for a low-frequency
// reconciler, never the provider-call hot path.
func (s *Store) Expire(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 || limit > state.MaxStateMaintenanceListLimit {
		return 0, ErrInvalidRequest
	}
	recs, err := s.state.ListKindBounded(ctx, state.ListScope{MaintenanceScoped: true}, attemptPrefix, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, rec := range recs {
		var attempt attemptState
		if err := json.Unmarshal(rec.Bytes, &attempt); err != nil {
			return count, err
		}
		if attempt.Status == "reserved" && !now.Before(attempt.ExpiresAt) {
			changed, err := s.expireReservation(ctx, attempt.AttemptID, now)
			if err != nil {
				return count, err
			}
			if changed {
				count++
			}
		}
	}
	return count, nil
}

func (s *Store) expireReservation(ctx context.Context, attemptID string, now time.Time) (bool, error) {
	q := identity.InternalCoordinationQuadruple()
	for range maxCASRetries {
		ar, attempt, err := s.loadAttempt(ctx, q, attemptPrefix+attemptID)
		if err != nil {
			return false, err
		}
		if attempt.Status != "reserved" || now.Before(attempt.ExpiresAt) {
			return false, nil
		}
		lr, lease, err := s.loadLease(ctx, q, leasePrefix+attempt.LeaseID)
		if err != nil {
			return false, err
		}
		if !lease.matchesAttempt(attempt) || lease.Reserved < attempt.Units {
			return false, ErrAttemptConflict
		}
		lease.Reserved -= attempt.Units
		attempt.Status = "expired"
		attempt.SettledAt = now
		lb, err := json.Marshal(lease)
		if err != nil {
			return false, fmt.Errorf("llm/leases: encode expired lease: %w", err)
		}
		ab, err := json.Marshal(attempt)
		if err != nil {
			return false, fmt.Errorf("llm/leases: encode expired attempt: %w", err)
		}
		ln := state.NewInternalRecord(state.NewEventID(), q, leasePrefix+attempt.LeaseID, lb)
		an := state.NewInternalRecord(state.NewEventID(), q, attemptPrefix+attemptID, ab)
		if err := s.state.SaveBatchIf(ctx,
			[]state.SlotExpectation{
				state.InternalSlotExpectation(q, leasePrefix+attempt.LeaseID, lr.ID),
				state.InternalSlotExpectation(q, attemptPrefix+attemptID, ar.ID),
			},
			[]state.StateRecord{ln, an}); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return false, fmt.Errorf("llm/leases: expire: %w", err)
		}
		return true, nil
	}
	return false, fmt.Errorf("%w: concurrent expiry did not converge", state.ErrConditionFailed)
}

// PendingReceipts returns only the bounded, removable handoff records written
// atomically with settlement. Retained attempt history is deliberately not
// scanned: successful acknowledgement removes this prefix, so later crash-gap
// receipts cannot be hidden behind an immutable first page.
func (s *Store) PendingReceipts(ctx context.Context, limit int) ([]llm.AttemptUsageReceipt, error) {
	if limit <= 0 || limit > state.MaxStateMaintenanceListLimit {
		return nil, ErrInvalidRequest
	}
	recs, err := s.state.ListKindBounded(ctx, state.ListScope{MaintenanceScoped: true}, pendingReceiptPrefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]llm.AttemptUsageReceipt, 0, len(recs))
	for _, rec := range recs {
		pending, err := decodePendingReceipt(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, pending.Receipt)
	}
	return out, nil
}

// AcknowledgePendingReceipt removes only the exact pending handoff after the
// durable outbox has accepted the same canonical receipt. Absence is an
// idempotent success; a changed body fails closed.
func (s *Store) AcknowledgePendingReceipt(ctx context.Context, receipt llm.AttemptUsageReceipt) error {
	if receipt.ReceiptID == "" || receipt.CanonicalBodyHash == "" {
		return ErrInvalidRequest
	}
	q := identity.InternalCoordinationQuadruple()
	kind := pendingReceiptKind(receipt.ReceiptID)
	for range maxCASRetries {
		rec, err := s.state.Load(ctx, q, kind)
		if errors.Is(err, state.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		pending, err := decodePendingReceipt(rec)
		if err != nil {
			return err
		}
		if pending.ReceiptHash != receiptHash(receipt) || pending.Receipt.CanonicalBodyHash != receipt.CanonicalBodyHash {
			return ErrAttemptConflict
		}
		changed, err := s.state.DeleteIf(ctx, state.InternalSlotExpectation(q, kind, rec.ID))
		if err != nil {
			return err
		}
		if changed {
			return nil
		}
	}
	return fmt.Errorf("%w: pending receipt acknowledgement did not converge", state.ErrConditionFailed)
}

func (s *Store) ensurePendingReceipt(ctx context.Context, receipt llm.AttemptUsageReceipt) error {
	q := identity.InternalCoordinationQuadruple()
	kind := pendingReceiptKind(receipt.ReceiptID)
	for range maxCASRetries {
		rec, err := s.state.Load(ctx, q, kind)
		if err == nil {
			pending, decodeErr := decodePendingReceipt(rec)
			if decodeErr != nil {
				return decodeErr
			}
			if pending.ReceiptHash != receiptHash(receipt) {
				return ErrAttemptConflict
			}
			return nil
		}
		if !errors.Is(err, state.ErrNotFound) {
			return err
		}
		body, err := json.Marshal(pendingReceiptState{AttemptID: receipt.ReceiptID, ReceiptHash: receiptHash(receipt), Receipt: receipt})
		if err != nil {
			return err
		}
		next := state.NewInternalRecord(state.NewEventID(), q, kind, body)
		if err := s.state.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, kind, "")}, next); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("%w: pending receipt replay did not converge", state.ErrConditionFailed)
}

func pendingReceiptKind(attemptID string) string {
	return pendingReceiptPrefix + hex.EncodeToString([]byte(attemptID))
}

func decodePendingReceipt(rec state.StateRecord) (pendingReceiptState, error) {
	var pending pendingReceiptState
	if err := json.Unmarshal(rec.Bytes, &pending); err != nil {
		return pendingReceiptState{}, fmt.Errorf("llm/leases: decode pending receipt: %w", err)
	}
	if rec.Identity != identity.InternalCoordinationQuadruple() || rec.Kind != pendingReceiptKind(pending.AttemptID) || pending.AttemptID == "" || pending.Receipt.ReceiptID != pending.AttemptID || pending.ReceiptHash != receiptHash(pending.Receipt) {
		return pendingReceiptState{}, ErrAttemptConflict
	}
	return pending, nil
}

type leaseState struct {
	GrantID              string             `json:"grant_id"`
	LeaseID              string             `json:"lease_id"`
	OrganizationID       string             `json:"organization_id"`
	RuntimeID            string             `json:"runtime_id"`
	AgentID              string             `json:"agent_id,omitempty"`
	Identity             identity.Quadruple `json:"identity"`
	Epoch                uint64             `json:"epoch"`
	Capacity             int64              `json:"capacity"`
	Reserved             int64              `json:"reserved"`
	Consumed             int64              `json:"consumed"`
	ExpiresAt            time.Time          `json:"expires_at"`
	GrantFingerprint     string             `json:"grant_fingerprint,omitempty"`
	RootGrantFingerprint string             `json:"root_grant_fingerprint,omitempty"`
	GrantCanonical       json.RawMessage    `json:"grant_canonical,omitempty"`
}
type attemptState struct {
	AttemptID      string                  `json:"attempt_id"`
	LogicalCallID  string                  `json:"logical_call_id"`
	AttemptNonce   string                  `json:"attempt_nonce"`
	GrantID        string                  `json:"grant_id"`
	LeaseID        string                  `json:"lease_id"`
	OrganizationID string                  `json:"organization_id"`
	RuntimeID      string                  `json:"runtime_id"`
	AgentID        string                  `json:"agent_id,omitempty"`
	Epoch          uint64                  `json:"epoch"`
	Capacity       int64                   `json:"capacity"`
	Units          int64                   `json:"units"`
	ConsumedUnits  int64                   `json:"consumed_units"`
	ExpiresAt      time.Time               `json:"expires_at"`
	Identity       identity.Quadruple      `json:"identity"`
	Status         string                  `json:"status"`
	Receipt        llm.AttemptUsageReceipt `json:"receipt,omitempty"`
	ReceiptHash    string                  `json:"receipt_hash,omitempty"`
	SettledAt      time.Time               `json:"settled_at,omitempty"`
}

type pendingReceiptState struct {
	AttemptID   string                  `json:"attempt_id"`
	ReceiptHash string                  `json:"receipt_hash"`
	Receipt     llm.AttemptUsageReceipt `json:"receipt"`
}

func (a attemptState) matches(r llm.LeaseReservationRequest) bool {
	return a.AttemptID == r.AttemptID && a.LogicalCallID == r.LogicalCallID && a.AttemptNonce == r.AttemptNonce && a.GrantID == r.GrantID && a.LeaseID == r.LeaseID && a.OrganizationID == r.OrganizationID && a.RuntimeID == r.RuntimeID && a.AgentID == r.AgentID && a.Epoch == r.Epoch && a.Capacity == r.Capacity && a.Units == r.Units && a.ExpiresAt.Equal(r.ExpiresAt) && a.Identity == r.Identity
}
func (a attemptState) reservation(existing bool) llm.LeaseReservation {
	return llm.LeaseReservation{AttemptID: a.AttemptID, LogicalCallID: a.LogicalCallID, AttemptNonce: a.AttemptNonce, GrantID: a.GrantID, LeaseID: a.LeaseID, Epoch: a.Epoch, Units: a.Units, ExpiresAt: a.ExpiresAt, Status: a.Status, Existing: existing, Receipt: a.Receipt}
}
func expectedID(r state.StateRecord) state.EventID {
	if r.ID == "" {
		return ""
	}
	return r.ID
}
func (s *Store) loadLease(ctx context.Context, q identity.Quadruple, k string) (state.StateRecord, leaseState, error) {
	r, e := s.state.Load(ctx, q, k)
	if e != nil {
		return state.StateRecord{}, leaseState{}, e
	}
	var v leaseState
	e = json.Unmarshal(r.Bytes, &v)
	return r, v, e
}
func (s *Store) loadAttempt(ctx context.Context, q identity.Quadruple, k string) (state.StateRecord, attemptState, error) {
	r, e := s.state.Load(ctx, q, k)
	if e != nil {
		return state.StateRecord{}, attemptState{}, e
	}
	var v attemptState
	e = json.Unmarshal(r.Bytes, &v)
	return r, v, e
}
func validateRequest(r llm.LeaseReservationRequest) error {
	if strings.TrimSpace(r.AttemptID) == "" || strings.TrimSpace(r.LogicalCallID) == "" || strings.TrimSpace(r.AttemptNonce) == "" || strings.TrimSpace(r.GrantID) == "" || strings.TrimSpace(r.LeaseID) == "" || strings.TrimSpace(r.OrganizationID) == "" || strings.TrimSpace(r.RuntimeID) == "" || r.Epoch == 0 || r.Capacity <= 0 || r.Units <= 0 || r.Units > r.Capacity || r.ExpiresAt.IsZero() {
		return ErrInvalidRequest
	}
	if err := identity.Validate(r.Identity.Identity); err != nil {
		return err
	}
	return nil
}

func validateTopUpRequest(r TopUpRequest) error {
	if strings.TrimSpace(r.GrantID) == "" || strings.TrimSpace(r.LeaseID) == "" || strings.TrimSpace(r.OrganizationID) == "" || strings.TrimSpace(r.RuntimeID) == "" || r.Epoch == 0 || r.Capacity <= 0 || r.ExpiresAt.IsZero() {
		return ErrInvalidRequest
	}
	return identity.Validate(r.Identity.Identity)
}

func newLeaseState(r llm.LeaseReservationRequest) leaseState {
	return leaseState{
		GrantID: r.GrantID, LeaseID: r.LeaseID, OrganizationID: r.OrganizationID,
		RuntimeID: r.RuntimeID, AgentID: r.AgentID, Identity: r.Identity,
		Epoch: r.Epoch, Capacity: r.Capacity, ExpiresAt: r.ExpiresAt, GrantFingerprint: r.GrantFingerprint,
	}
}

func (l leaseState) matches(r llm.LeaseReservationRequest) bool {
	return l.GrantID == r.GrantID && l.LeaseID == r.LeaseID && l.OrganizationID == r.OrganizationID && l.RuntimeID == r.RuntimeID && l.AgentID == r.AgentID && l.Identity == r.Identity && (r.GrantFingerprint == "" || l.GrantFingerprint == "" || l.GrantFingerprint == r.GrantFingerprint)
}

func (l leaseState) matchesTopUp(r TopUpRequest) bool {
	return l.GrantID == r.GrantID && l.LeaseID == r.LeaseID && l.OrganizationID == r.OrganizationID && l.RuntimeID == r.RuntimeID && l.AgentID == r.AgentID && l.Identity == r.Identity
}

func (l leaseState) matchesAttempt(a attemptState) bool {
	return l.GrantID == a.GrantID && l.LeaseID == a.LeaseID && l.OrganizationID == a.OrganizationID && l.RuntimeID == a.RuntimeID && l.AgentID == a.AgentID && l.Identity == a.Identity
}

func (l leaseState) matchesGrantBinding(g llm.ExternalGrant) bool {
	return l.GrantID == g.GrantID && l.LeaseID == g.Lease.LeaseID && l.OrganizationID == g.OrganizationID &&
		l.RuntimeID == g.RuntimeID && l.AgentID == g.AgentID && l.Identity == (identity.Quadruple{
		Identity: identity.Identity{TenantID: g.TenantID, UserID: g.UserID, SessionID: g.SessionID},
		RunID:    g.LogicalRunID,
	})
}

func leaseStateFromGrant(rootFingerprint string, g llm.ExternalGrant, fingerprint string, canonical []byte) leaseState {
	return leaseState{
		GrantID: g.GrantID, LeaseID: g.Lease.LeaseID, OrganizationID: g.OrganizationID,
		RuntimeID: g.RuntimeID, AgentID: g.AgentID,
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: g.TenantID, UserID: g.UserID, SessionID: g.SessionID}, RunID: g.LogicalRunID},
		Epoch:    g.Lease.Epoch, Capacity: g.Lease.RemainingTokens(), ExpiresAt: g.Lease.ExpiresAt,
		GrantFingerprint: fingerprint, RootGrantFingerprint: rootFingerprint,
		GrantCanonical: append(json.RawMessage(nil), canonical...),
	}
}

func grantBindingMatches(a, b llm.ExternalGrant) bool {
	return a.GrantID == b.GrantID && a.Lease.LeaseID == b.Lease.LeaseID && a.OrganizationID == b.OrganizationID &&
		a.RuntimeID == b.RuntimeID && a.AgentID == b.AgentID && a.TenantID == b.TenantID && a.UserID == b.UserID &&
		a.SessionID == b.SessionID && a.LogicalRunID == b.LogicalRunID
}

func (a attemptState) matchesReceipt(r llm.AttemptUsageReceipt) bool {
	return a.GrantID == r.GrantID && a.OrganizationID == r.OrganizationID && a.RuntimeID == r.RuntimeID && a.AgentID == r.AgentID &&
		a.Identity.TenantID == r.TenantID && a.Identity.UserID == r.UserID && a.Identity.SessionID == r.SessionID && a.Identity.RunID == r.LogicalRunID
}
func receiptHash(r llm.AttemptUsageReceipt) string {
	h := sha256.Sum256([]byte(r.CanonicalBodyHash + "\x00" + r.ReceiptID))
	return hex.EncodeToString(h[:])
}
