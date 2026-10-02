// Package allocation implements task-scoped cumulative inference accounting on
// the mandatory StateStore CAS contract. No process-local counter is authoritative.
package allocation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

const totalKind = "harbor.internal/inference.allocation"
const attemptPrefix = "harbor.internal/inference.allocation.attempt/"

// Store implements durable task allocation accounting with cross-process CAS.
type Store struct{ state state.StateStore }

// New binds allocation accounting to the runtime's existing persistence floor.
func New(st state.StateStore) *Store { return &Store{state: st} }

var _ llm.AllocationStore = (*Store)(nil)

type total struct {
	Receipts          []llm.AllocationReceipt `json:"receipts"`
	ReceiptsTruncated bool                    `json:"receipts_truncated"`
	BoundBreached     bool                    `json:"bound_breached"`
	Allocation        llm.InferenceAllocation `json:"allocation"`
	Settled           int64                   `json:"settled"`
	Reserved          int64                   `json:"reserved"`
	Unknown           int64                   `json:"unknown"`
	Attempts          int64                   `json:"attempts"`
}
type attempt struct {
	Units  int64  `json:"units"`
	Status string `json:"status"`
	Used   int64  `json:"used"`
}

func validate(q identity.Quadruple, a llm.InferenceAllocation) error {
	if err := identity.Validate(q.Identity); err != nil {
		return fmt.Errorf("allocation identity: %w", err)
	}
	if q.RunID == "" {
		return llm.ErrAllocationInvalid
	}
	return llm.ValidateInferenceAllocation(&a)
}
func (s *Store) load(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation) (state.StateRecord, total, error) {
	if s.state == nil {
		return state.StateRecord{}, total{}, llm.ErrAllocationUnavailable
	}
	rec, err := s.state.Load(ctx, q, totalKind)
	if errors.Is(err, state.ErrNotFound) {
		return state.StateRecord{}, total{Allocation: a}, nil
	}
	if err != nil {
		return rec, total{}, fmt.Errorf("load allocation: %w", err)
	}
	var t total
	if err = json.Unmarshal(rec.Bytes, &t); err != nil {
		return rec, t, fmt.Errorf("decode allocation: %w", err)
	}
	if t.Allocation.AllocationID != a.AllocationID || t.Allocation.Revision != a.Revision || t.Allocation.MaxTotalTokens != a.MaxTotalTokens || t.Allocation.MaxCostMicroUSD != nil || t.Settled < 0 || t.Reserved < 0 || t.Unknown < 0 || t.Unknown > t.Reserved {
		return rec, t, llm.ErrAllocationInvalid
	}
	return rec, t, nil
}
func record(q identity.Quadruple, kind string, v any) (state.StateRecord, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return state.StateRecord{}, fmt.Errorf("encode allocation: %w", err)
	}
	return state.NewInternalRecord(state.NewEventID(), q, kind, b), nil
}

// Reserve holds a conservative complete provider-attempt liability before I/O.
// Existing attempt identities are never authorizations to repeat provider I/O.
func (s *Store) Reserve(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation, id string, units int64) error {
	if err := validate(q, a); err != nil {
		return err
	}
	if id == "" || len(id) > 128 || units <= 0 {
		return llm.ErrAllocationInvalid
	}
	for range 128 {
		rec, t, err := s.load(ctx, q, a)
		if err != nil {
			return err
		}
		if t.BoundBreached || t.Settled > a.MaxTotalTokens || t.Reserved > a.MaxTotalTokens-t.Settled || units > a.MaxTotalTokens-t.Settled-t.Reserved {
			return llm.ErrAllocationExhausted
		}
		t.Reserved += units
		t.Attempts++
		next, err := record(q, totalKind, t)
		if err != nil {
			return err
		}
		ar, err := record(q, attemptPrefix+id, attempt{Units: units, Status: "reserved"})
		if err != nil {
			return err
		}
		err = s.state.SaveBatchIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, totalKind, rec.ID), state.InternalSlotExpectation(q, attemptPrefix+id, "")}, []state.StateRecord{next, ar})
		if errors.Is(err, state.ErrConditionFailed) {
			if _, lookup := s.state.Load(ctx, q, attemptPrefix+id); lookup == nil {
				return llm.ErrAllocationInvalid
			} else if !errors.Is(lookup, state.ErrNotFound) {
				return fmt.Errorf("lookup allocation attempt: %w", lookup)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("reserve allocation: %w", err)
		}
		return nil
	}
	return fmt.Errorf("allocation contention: %w", state.ErrConditionFailed)
}

// Settle records provider-reported usage once. Nil usage retains all liability;
// uncertainty is never refunded by cancellation, restart or elapsed time.
func (s *Store) Settle(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation, id string, used *int64, retainRemainder bool) error {
	return s.settle(ctx, q, a, id, used, retainRemainder, false)
}

// ReportBoundViolation records invalid provider accounting as unknown liability
// and permanently refuses new reservations, atomically with the attempt receipt.
func (s *Store) ReportBoundViolation(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation, id string) error {
	return s.settle(ctx, q, a, id, nil, false, true)
}

func (s *Store) settle(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation, id string, used *int64, retainRemainder, breached bool) error {
	if err := validate(q, a); err != nil {
		return err
	}
	if id == "" || used != nil && *used < 0 {
		return llm.ErrAllocationInvalid
	}
	for range 128 {
		rec, t, err := s.load(ctx, q, a)
		if err != nil {
			return err
		}
		ar, err := s.state.Load(ctx, q, attemptPrefix+id)
		if err != nil {
			return fmt.Errorf("load allocation attempt: %w", err)
		}
		var at attempt
		if err = json.Unmarshal(ar.Bytes, &at); err != nil {
			return fmt.Errorf("decode allocation attempt: %w", err)
		}
		status := "unknown"
		if breached {
			status = "breached"
		} else if used != nil {
			status = "settled"
		}
		if at.Status != "reserved" {
			if at.Status == status && (used == nil || at.Used == *used) {
				return nil
			}
			return llm.ErrAllocationInvalid
		}
		if at.Units <= 0 || at.Units > t.Reserved {
			return llm.ErrAllocationInvalid
		}
		at.Status = status
		if breached {
			t.BoundBreached = true
		}
		if used == nil {
			t.Unknown += at.Units
		} else {
			if *used > 1<<62 || t.Settled > (1<<62)-*used {
				return llm.ErrAllocationInvalid
			}
			if *used > at.Units {
				t.BoundBreached = true
			}
			at.Used = *used
			t.Reserved -= at.Units
			t.Settled += *used
			if retainRemainder && *used < at.Units {
				t.Reserved += at.Units - *used
				t.Unknown += at.Units - *used
			}
		}
		receipt := llm.AllocationReceipt{AttemptID: id, ReservedTokens: at.Units, Status: status}
		if used == nil {
			receipt.UnknownTokens = at.Units
		} else {
			receipt.SettledTokens = *used
			if retainRemainder && *used < at.Units {
				receipt.UnknownTokens = at.Units - *used
			}
		}
		t.Receipts = append(t.Receipts, receipt)
		if len(t.Receipts) > 32 {
			t.Receipts = append([]llm.AllocationReceipt(nil), t.Receipts[len(t.Receipts)-32:]...)
			t.ReceiptsTruncated = true
		}
		next, err := record(q, totalKind, t)
		if err != nil {
			return err
		}
		an, err := record(q, attemptPrefix+id, at)
		if err != nil {
			return err
		}
		err = s.state.SaveBatchIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, totalKind, rec.ID), state.InternalSlotExpectation(q, attemptPrefix+id, ar.ID)}, []state.StateRecord{next, an})
		if errors.Is(err, state.ErrConditionFailed) {
			continue
		}
		if err != nil {
			return fmt.Errorf("settle allocation: %w", err)
		}
		return nil
	}
	return fmt.Errorf("allocation contention: %w", state.ErrConditionFailed)
}

// Snapshot returns accounting without creating or mutating a record.
func (s *Store) Snapshot(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation) (llm.AllocationSnapshot, error) {
	if err := validate(q, a); err != nil {
		return llm.AllocationSnapshot{}, err
	}
	_, t, err := s.load(ctx, q, a)
	if err != nil {
		return llm.AllocationSnapshot{}, err
	}
	if t.Receipts == nil {
		t.Receipts = []llm.AllocationReceipt{}
	}
	return llm.AllocationSnapshot{Receipts: t.Receipts, ReceiptsTruncated: t.ReceiptsTruncated, BoundBreached: t.BoundBreached, AllocationID: a.AllocationID, Revision: a.Revision, MaxTotalTokens: a.MaxTotalTokens, SettledTokens: t.Settled, ReservedTokens: t.Reserved, UnknownTokens: t.Unknown, AttemptCount: t.Attempts, Guarantee: "tokens", PricingStatus: "unavailable"}, nil
}
