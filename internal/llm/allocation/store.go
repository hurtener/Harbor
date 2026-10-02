// Package allocation implements task-scoped cumulative inference accounting on
// the mandatory StateStore CAS contract. No process-local counter is authoritative.
package allocation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

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
	Closed            bool                    `json:"closed"`
	ChargedCost       int64                   `json:"charged_cost_micro_usd"`
	ReservedCost      int64                   `json:"reserved_cost_micro_usd"`
	UnknownCost       int64                   `json:"unknown_cost_micro_usd"`
	Receipts          []llm.AllocationReceipt `json:"receipts"`
	ReceiptsTruncated bool                    `json:"receipts_truncated"`
	BoundBreached     bool                    `json:"bound_breached"`
	Allocation        llm.InferenceAllocation `json:"allocation"`
	Settled           int64                   `json:"settled"`
	Reserved          int64                   `json:"reserved"`
	Unknown           int64                   `json:"unknown"`
	Attempts          int64                   `json:"attempts"`
}

// The envelope deliberately does not expose a top-level allocation. Older
// writers reject its missing legacy funding identity instead of ignoring the
// irreversible close bit. Their already-loaded CAS generations also fail.
type totalEnvelope struct {
	SchemaVersion    *int            `json:"schema_version"`
	Fingerprint      string          `json:"fingerprint"`
	Total            *total          `json:"total"`
	LegacyAllocation json.RawMessage `json:"allocation,omitempty"`
}
type attempt struct {
	Cost            int64  `json:"cost_micro_usd"`
	MonetaryStatus  string `json:"monetary_status,omitempty"`
	RetainRemainder bool   `json:"retain_remainder"`
	Units           int64  `json:"units"`
	Status          string `json:"status"`
	Used            int64  `json:"used"`
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
	storedQ, storedKind := allocationLocation(q, totalKind)
	rec, err := s.state.Load(ctx, storedQ, storedKind)
	if errors.Is(err, state.ErrNotFound) {
		rec, err = s.state.Load(ctx, q, totalKind)
	}
	if errors.Is(err, state.ErrNotFound) {
		return state.StateRecord{}, total{Allocation: a}, nil
	}
	if err != nil {
		return rec, total{}, fmt.Errorf("load allocation: %w", err)
	}
	loaded, total, decodeErr := decodeTotal(rec, a)
	if decodeErr != nil && !identity.IsInternalCoordination(rec.Identity.Identity) {
		// Another manager can atomically migrate after our first absent read.
		// Its legacy marker is a fence, not evidence that funding became invalid.
		current, currentErr := s.state.Load(ctx, storedQ, storedKind)
		if currentErr == nil {
			return decodeTotal(current, a)
		}
		if !errors.Is(currentErr, state.ErrNotFound) {
			return current, total, currentErr
		}
	}
	return loaded, total, decodeErr
}

func decodeTotal(rec state.StateRecord, a llm.InferenceAllocation) (state.StateRecord, total, error) {
	var t total
	var envelope totalEnvelope
	if err := json.Unmarshal(rec.Bytes, &envelope); err != nil {
		return rec, t, fmt.Errorf("decode allocation: %w", err)
	}
	switch {
	case !identity.IsInternalCoordination(rec.Identity.Identity) && envelope.SchemaVersion == nil && envelope.Total == nil:
		if err := json.Unmarshal(rec.Bytes, &t); err != nil {
			return rec, t, fmt.Errorf("decode legacy allocation: %w", err)
		}
		if t.Closed {
			// A close bit in the old representation cannot fence old readers.
			return rec, t, llm.ErrAllocationInvalid
		}
	case identity.IsInternalCoordination(rec.Identity.Identity) && envelope.SchemaVersion != nil && *envelope.SchemaVersion == 2 && envelope.Total != nil && len(envelope.LegacyAllocation) == 0:
		fingerprint, err := allocationFingerprint(a)
		if err != nil {
			return rec, t, err
		}
		if envelope.Fingerprint != fingerprint || envelope.Total.Allocation.AllocationID != "" {
			return rec, t, llm.ErrAllocationInvalid
		}
		t = *envelope.Total
		t.Allocation = a
	default:
		return rec, t, llm.ErrAllocationInvalid
	}
	if !llm.EqualInferenceAllocation(&t.Allocation, &a) || t.Settled < 0 || t.Reserved < 0 || t.Unknown < 0 || t.Unknown > t.Reserved || t.ChargedCost < 0 || t.ReservedCost < 0 || t.UnknownCost < 0 || t.UnknownCost > t.ReservedCost {
		return rec, t, llm.ErrAllocationInvalid
	}
	if a.MaxCostMicroUSD == nil && (t.ChargedCost != 0 || t.ReservedCost != 0 || t.UnknownCost != 0) || a.MaxCostMicroUSD != nil && (t.ChargedCost > *a.MaxCostMicroUSD || t.ReservedCost > *a.MaxCostMicroUSD-t.ChargedCost) {
		return rec, t, llm.ErrAllocationInvalid
	}
	return rec, t, nil
}
func record(q identity.Quadruple, kind string, v any) (state.StateRecord, error) {
	if kind == totalKind {
		t, ok := v.(total)
		if !ok {
			return state.StateRecord{}, llm.ErrAllocationInvalid
		}
		fingerprint, err := allocationFingerprint(t.Allocation)
		if err != nil {
			return state.StateRecord{}, err
		}
		t.Allocation = llm.InferenceAllocation{}
		v = totalEnvelope{SchemaVersion: new(2), Fingerprint: fingerprint, Total: &t}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return state.StateRecord{}, fmt.Errorf("encode allocation: %w", err)
	}
	storedQ, storedKind := allocationLocation(q, kind)
	return state.NewInternalRecord(state.NewEventID(), storedQ, storedKind, b), nil
}

// Close linearizes against every reservation on the existing accounting slot.
// It never releases liability or blocks authoritative settlement of an attempt
// admitted before the barrier. Repeated closure, including ACK-loss recovery,
// preserves the same immutable closed state.
func (s *Store) Close(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation) error {
	if err := validate(q, a); err != nil {
		return err
	}
	// Finite concurrent work can produce more mutations than a fixed retry
	// count: each accepted envelope can reserve and later settle. Yield after
	// a lost CAS, within a five-second operation limit that preserves any
	// earlier caller deadline. Timing out never clears or refunds liability.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.Ensure(ctx, q, a); err != nil {
		return err
	}
	delay := 100 * time.Microsecond
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("allocation close contention: %w", err)
		}
		rec, t, err := s.load(ctx, q, a)
		if err != nil {
			return err
		}
		if t.Closed {
			return nil
		}
		t.Closed = true
		next, err := record(q, totalKind, t)
		if err != nil {
			return err
		}
		err = s.state.SaveBatchIf(ctx, []state.SlotExpectation{allocationExpectation(q, totalKind, rec.ID)}, []state.StateRecord{next})
		if errors.Is(err, state.ErrConditionFailed) {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("allocation close contention: %w", ctx.Err())
			case <-timer.C:
			}
			delay = min(delay*2, time.Millisecond)
			continue
		}
		if err != nil {
			return fmt.Errorf("close allocation: %w", err)
		}
		return nil
	}
}

// Reserve holds a conservative complete provider-attempt liability before I/O.
// Existing attempt identities are never authorizations to repeat provider I/O.
func (s *Store) Reserve(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation, id string, units int64) error {
	if a.MaxCostMicroUSD != nil {
		return llm.ErrAllocationPricingUnavailable
	}
	return s.reserve(ctx, q, a, id, units, 0)
}

// ReserveMonetary atomically holds both resource ceilings. Only the trusted
// provider edge computes cost; task text and provider cost floats cannot do so.
func (s *Store) ReserveMonetary(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation, id string, units, cost int64) error {
	if a.MaxCostMicroUSD == nil || cost < 0 {
		return llm.ErrAllocationInvalid
	}
	return s.reserve(ctx, q, a, id, units, cost)
}

func (s *Store) reserve(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation, id string, units, cost int64) error {
	if err := validate(q, a); err != nil {
		return err
	}
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || units <= 0 {
		return llm.ErrAllocationInvalid
	}
	if err := s.Ensure(ctx, q, a); err != nil {
		return err
	}
	for range 128 {
		rec, t, err := s.load(ctx, q, a)
		if err != nil {
			return err
		}
		if t.Closed {
			return llm.ErrAllocationClosed
		}
		if t.BoundBreached || t.Settled > a.MaxTotalTokens || t.Reserved > a.MaxTotalTokens-t.Settled || units > a.MaxTotalTokens-t.Settled-t.Reserved {
			return llm.ErrAllocationExhausted
		}
		if t.Attempts == math.MaxInt64 || a.MaxCostMicroUSD != nil && cost > *a.MaxCostMicroUSD-t.ChargedCost-t.ReservedCost {
			return llm.ErrAllocationExhausted
		}
		t.Reserved += units
		t.ReservedCost += cost
		t.Attempts++
		next, err := record(q, totalKind, t)
		if err != nil {
			return err
		}
		ar, err := record(q, attemptPrefix+id, attempt{Units: units, Cost: cost, Status: "reserved"})
		if err != nil {
			return err
		}
		err = s.state.SaveBatchIf(ctx, []state.SlotExpectation{allocationExpectation(q, totalKind, rec.ID), allocationExpectation(q, attemptPrefix+id, "")}, []state.StateRecord{next, ar})
		if errors.Is(err, state.ErrConditionFailed) {
			if _, lookup := s.loadAttempt(ctx, q, id); lookup == nil {
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
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || used != nil && *used < 0 {
		return llm.ErrAllocationInvalid
	}
	if err := s.Ensure(ctx, q, a); err != nil {
		return err
	}
	for range 128 {
		rec, t, err := s.load(ctx, q, a)
		if err != nil {
			return err
		}
		ar, err := s.loadAttempt(ctx, q, id)
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
			if at.Status == status && at.RetainRemainder == retainRemainder && (used == nil || at.Used == *used) {
				return nil
			}
			return llm.ErrAllocationInvalid
		}
		if at.Units <= 0 || at.Units > t.Reserved || at.Cost < 0 || at.Cost > t.ReservedCost {
			return llm.ErrAllocationInvalid
		}
		at.Status = status
		at.RetainRemainder = retainRemainder
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
		receipt := llm.AllocationReceipt{AttemptID: id, ReservedTokens: at.Units, Status: status, ReservedCostMicroUSD: at.Cost}
		if a.MaxCostMicroUSD != nil {
			switch {
			case used != nil && *used == 0 && !retainRemainder && !breached:
				// The safety edge proves cancellation before driver entry.
				t.ReservedCost -= at.Cost
				at.MonetaryStatus = "released_before_dispatch"
			case used != nil && !retainRemainder && !breached && *used <= at.Units:
				// This is charged CAPACITY at the trusted full envelope, not
				// observed provider spend. No provider cost number is accepted.
				t.ReservedCost -= at.Cost
				t.ChargedCost += at.Cost
				receipt.ChargedCostMicroUSD = at.Cost
				at.MonetaryStatus = "charged_ceiling"
			default:
				t.UnknownCost += at.Cost
				receipt.UnknownCostMicroUSD = at.Cost
				at.MonetaryStatus = "unknown"
			}
			receipt.MonetaryStatus = at.MonetaryStatus
		}
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
		err = s.state.SaveBatchIf(ctx, []state.SlotExpectation{allocationExpectation(q, totalKind, rec.ID), allocationExpectation(q, attemptPrefix+id, ar.ID)}, []state.StateRecord{next, an})
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
	snap := llm.AllocationSnapshot{Closed: t.Closed, Receipts: t.Receipts, ReceiptsTruncated: t.ReceiptsTruncated, BoundBreached: t.BoundBreached, AllocationID: a.AllocationID, Revision: a.Revision, MaxTotalTokens: a.MaxTotalTokens, SettledTokens: t.Settled, ReservedTokens: t.Reserved, UnknownTokens: t.Unknown, AttemptCount: t.Attempts, Guarantee: "tokens", PricingStatus: "unavailable"}
	if a.MaxCostMicroUSD != nil {
		cap := *a.MaxCostMicroUSD
		snap.MaxCostMicroUSD = &cap
		snap.ChargedCostMicroUSD, snap.ReservedCostMicroUSD, snap.UnknownCostMicroUSD = t.ChargedCost, t.ReservedCost, t.UnknownCost
		snap.PricingManifestID, snap.PricingManifestRevision, snap.PricingManifestSHA256 = a.PricingManifestID, a.PricingManifestRevision, a.PricingManifestSHA256
		snap.Guarantee, snap.PricingStatus = "tokens_and_cost_micro_usd", "trusted_inclusive_ceiling"
	}
	return snap, nil
}
