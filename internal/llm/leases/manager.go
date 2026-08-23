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
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	leasePrefix   = "harbor.internal/inference.lease/"
	attemptPrefix = "harbor.internal/inference.attempt/"
	maxCASRetries = 8
)

var (
	ErrInvalidRequest  = errors.New("llm/leases: invalid reservation request")
	ErrInsufficient    = errors.New("llm/leases: lease capacity is insufficient")
	ErrOverspend       = errors.New("llm/leases: settled usage exceeds reservation")
	ErrAttemptConflict = errors.New("llm/leases: attempt id is already bound to another reservation")
)

// TopUpRequest advances a lease epoch. Existing reservations remain bound to
// their old epoch; new attempts use the new capacity and epoch.
type TopUpRequest struct {
	LeaseID   string
	Epoch     uint64
	Capacity  int64
	ExpiresAt time.Time
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
func (s *Store) Reserve(ctx context.Context, req llm.LeaseReservationRequest) (llm.LeaseReservation, error) {
	if err := validateRequest(req); err != nil {
		return llm.LeaseReservation{}, err
	}
	leaseQ := identity.InternalCoordinationQuadruple()
	leaseKind := leasePrefix + req.LeaseID
	attemptKind := attemptPrefix + req.AttemptID
	now := s.clock().UTC()
	if !req.ExpiresAt.IsZero() && !now.Before(req.ExpiresAt) {
		return llm.LeaseReservation{}, fmt.Errorf("%w: reservation expired", ErrInsufficient)
	}
	for n := 0; n < maxCASRetries; n++ {
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
			return attempt.reservation(true), nil
		}
		if leaseErr != nil {
			lease = leaseState{LeaseID: req.LeaseID, Epoch: req.Epoch, Capacity: req.Capacity, ExpiresAt: req.ExpiresAt}
		} else if lease.Epoch != req.Epoch || lease.Capacity != req.Capacity || lease.ExpiresAt != req.ExpiresAt {
			return llm.LeaseReservation{}, fmt.Errorf("%w: lease epoch/capacity changed", ErrAttemptConflict)
		}
		if lease.ExpiresAt.IsZero() || !now.Before(lease.ExpiresAt) {
			return llm.LeaseReservation{}, fmt.Errorf("%w: lease expired", ErrInsufficient)
		}
		if lease.Capacity-lease.Consumed-lease.Reserved < req.Units {
			return llm.LeaseReservation{}, fmt.Errorf("%w: available=%d requested=%d", ErrInsufficient, lease.Capacity-lease.Consumed-lease.Reserved, req.Units)
		}
		lease.Reserved += req.Units
		attempt = attemptState{AttemptID: req.AttemptID, LogicalCallID: req.LogicalCallID, AttemptNonce: req.AttemptNonce, GrantID: req.GrantID, LeaseID: req.LeaseID, Epoch: req.Epoch, Units: req.Units, ExpiresAt: req.ExpiresAt, Identity: req.Identity, Status: "reserved"}
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

// Settle atomically moves reserved units to consumed (or releases them on a
// failed/cancelled call) and stores the receipt for later outbox reconciliation.
func (s *Store) Settle(ctx context.Context, req llm.LeaseSettlement) error {
	if req.AttemptID == "" || req.Receipt.ReceiptID == "" || req.LogicalCallID == "" || req.AttemptNonce == "" || req.Units < 0 {
		return ErrInvalidRequest
	}
	now := req.Now
	if now.IsZero() {
		now = s.clock().UTC()
	}
	q := identity.InternalCoordinationQuadruple()
	for n := 0; n < maxCASRetries; n++ {
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
			req.Receipt.ReceiptID != a.AttemptID || req.Receipt.IdempotencyKey != a.AttemptID {
			return ErrAttemptConflict
		}
		if a.Status == "consumed" || a.Status == "released" || a.Status == "expired" {
			if a.ReceiptHash == receiptHash(req.Receipt) {
				return nil
			}
			return ErrAttemptConflict
		}
		if a.Status != "reserved" || req.Units > a.Units {
			return ErrOverspend
		}
		if req.Units > l.Reserved {
			return ErrOverspend
		}
		l.Reserved -= a.Units
		a.Status = "consumed"
		a.ConsumedUnits = req.Units
		if req.Receipt.Status == "success" {
			l.Consumed += req.Units
		} else {
			// A failed or cancelled provider call releases the reservation;
			// it must not consume hard-budget capacity. The receipt remains
			// attached to the attempt for audit/replay idempotency.
			a.Status = "released"
			a.ConsumedUnits = 0
		}
		a.Receipt = req.Receipt
		a.ReceiptHash = receiptHash(req.Receipt)
		a.SettledAt = now
		lb, _ := json.Marshal(l)
		ab, _ := json.Marshal(a)
		ln := state.NewInternalRecord(state.NewEventID(), q, leasePrefix+a.LeaseID, lb)
		an := state.NewInternalRecord(state.NewEventID(), q, attemptPrefix+a.AttemptID, ab)
		if err := s.state.SaveBatchIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, leasePrefix+a.LeaseID, lr.ID), state.InternalSlotExpectation(q, attemptPrefix+a.AttemptID, ar.ID)}, []state.StateRecord{ln, an}); err != nil {
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
	if strings.TrimSpace(req.LeaseID) == "" || req.Epoch == 0 || req.Capacity <= 0 || req.ExpiresAt.IsZero() {
		return ErrInvalidRequest
	}
	q := identity.InternalCoordinationQuadruple()
	kind := leasePrefix + req.LeaseID
	for n := 0; n < maxCASRetries; n++ {
		rec, lease, err := s.loadLease(ctx, q, kind)
		if err != nil {
			return err
		}
		if req.Epoch <= lease.Epoch || req.Capacity < lease.Capacity {
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

// Release returns reserved units without consuming them. It is idempotent for
// an already closed attempt and is used by cancellation/expiry reconciliation.
func (s *Store) Release(ctx context.Context, attemptID string) error {
	if strings.TrimSpace(attemptID) == "" {
		return ErrInvalidRequest
	}
	q := identity.InternalCoordinationQuadruple()
	for n := 0; n < maxCASRetries; n++ {
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
		lb, _ := json.Marshal(lease)
		ab, _ := json.Marshal(attempt)
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

// Expire releases reservations whose bounded lease has elapsed. The bounded
// maintenance scan is intended for a low-frequency reconciler, never the
// provider-call hot path.
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
			if err := s.Release(ctx, attempt.AttemptID); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

// PendingReceipts returns bounded receipts whose provider call settled before
// their outbox enqueue. It is intentionally a low-frequency reconciliation
// seam, not the normal delivery path.
func (s *Store) PendingReceipts(ctx context.Context, limit int) ([]llm.AttemptUsageReceipt, error) {
	if limit <= 0 || limit > state.MaxStateMaintenanceListLimit {
		return nil, ErrInvalidRequest
	}
	recs, err := s.state.ListKindBounded(ctx, state.ListScope{MaintenanceScoped: true}, attemptPrefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]llm.AttemptUsageReceipt, 0, len(recs))
	for _, rec := range recs {
		var a attemptState
		if err := json.Unmarshal(rec.Bytes, &a); err != nil {
			return nil, fmt.Errorf("llm/leases: decode attempt: %w", err)
		}
		if a.Status == "consumed" && a.Receipt.ReceiptID != "" {
			out = append(out, a.Receipt)
		}
	}
	return out, nil
}

type leaseState struct {
	LeaseID   string    `json:"lease_id"`
	Epoch     uint64    `json:"epoch"`
	Capacity  int64     `json:"capacity"`
	Reserved  int64     `json:"reserved"`
	Consumed  int64     `json:"consumed"`
	ExpiresAt time.Time `json:"expires_at"`
}
type attemptState struct {
	AttemptID     string                  `json:"attempt_id"`
	LogicalCallID string                  `json:"logical_call_id"`
	AttemptNonce  string                  `json:"attempt_nonce"`
	GrantID       string                  `json:"grant_id"`
	LeaseID       string                  `json:"lease_id"`
	Epoch         uint64                  `json:"epoch"`
	Units         int64                   `json:"units"`
	ConsumedUnits int64                   `json:"consumed_units"`
	ExpiresAt     time.Time               `json:"expires_at"`
	Identity      identity.Quadruple      `json:"identity"`
	Status        string                  `json:"status"`
	Receipt       llm.AttemptUsageReceipt `json:"receipt,omitempty"`
	ReceiptHash   string                  `json:"receipt_hash,omitempty"`
	SettledAt     time.Time               `json:"settled_at,omitempty"`
}

func (a attemptState) matches(r llm.LeaseReservationRequest) bool {
	return a.AttemptID == r.AttemptID && a.LogicalCallID == r.LogicalCallID && a.AttemptNonce == r.AttemptNonce && a.GrantID == r.GrantID && a.LeaseID == r.LeaseID && a.Epoch == r.Epoch && a.Units == r.Units && a.Identity == r.Identity
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
	if strings.TrimSpace(r.AttemptID) == "" || strings.TrimSpace(r.LogicalCallID) == "" || strings.TrimSpace(r.AttemptNonce) == "" || strings.TrimSpace(r.GrantID) == "" || strings.TrimSpace(r.LeaseID) == "" || r.Epoch == 0 || r.Capacity <= 0 || r.Units <= 0 || r.Units > r.Capacity || r.ExpiresAt.IsZero() {
		return ErrInvalidRequest
	}
	if err := identity.Validate(r.Identity.Identity); err != nil {
		return err
	}
	return nil
}
func receiptHash(r llm.AttemptUsageReceipt) string {
	h := sha256.Sum256([]byte(r.CanonicalBodyHash + "\x00" + r.ReceiptID))
	return hex.EncodeToString(h[:])
}
