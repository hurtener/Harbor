package allocation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

// Scope digests retain no raw tenant/user/session/task strings. Separate tenant
// and full-owner partitions support explicitly quiesced tenant maintenance.
func scopeDigest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func allocationLocation(q identity.Quadruple, kind string) (identity.Quadruple, string) {
	base := totalKind + ".v2/" + scopeDigest(q.TenantID) + "/" + scopeDigest(q.TenantID, q.UserID, q.SessionID, q.RunID)
	if kind == totalKind {
		return identity.InternalCoordinationQuadruple(), base + "/total"
	}
	return identity.InternalCoordinationQuadruple(), base + "/attempt/" + scopeDigest(strings.TrimPrefix(kind, attemptPrefix))
}

func allocationExpectation(q identity.Quadruple, kind string, event state.EventID) state.SlotExpectation {
	storedQ, storedKind := allocationLocation(q, kind)
	return state.InternalSlotExpectation(storedQ, storedKind, event)
}

func allocationFingerprint(a llm.InferenceAllocation) (string, error) {
	raw, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("allocation fingerprint: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) loadAttempt(ctx context.Context, q identity.Quadruple, id string) (state.StateRecord, error) {
	storedQ, kind := allocationLocation(q, attemptPrefix+id)
	return s.state.Load(ctx, storedQ, kind)
}

// Ensure pins immutable funding before native task acceptance. Totals and
// attempts are content-free accounting, retained outside ordinary session data
// so erasure cannot reopen a spent or closed allocation for a delayed helper.
// Legacy migration atomically copies bounded history and fences its old total.
// A lost migration acknowledgment is recovered by the exact protected record.
func (s *Store) Ensure(ctx context.Context, q identity.Quadruple, a llm.InferenceAllocation) error {
	if err := validate(q, a); err != nil {
		return err
	}
	if s.state == nil {
		return llm.ErrAllocationUnavailable
	}
	storedQ, kind := allocationLocation(q, totalKind)
	for range 128 {
		existing, err := s.state.Load(ctx, storedQ, kind)
		if err == nil {
			_, _, err = decodeTotal(existing, a)
			return err
		}
		if !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("load protected allocation: %w", err)
		}
		legacy, err := s.state.Load(ctx, q, totalKind)
		t := total{Allocation: a}
		var attempts []state.StateRecord
		if err == nil {
			_, t, err = decodeTotal(legacy, a)
			if err != nil {
				current, currentErr := s.state.Load(ctx, storedQ, kind)
				if currentErr == nil {
					_, _, currentErr = decodeTotal(current, a)
					return currentErr
				}
				if !errors.Is(currentErr, state.ErrNotFound) {
					return currentErr
				}
				return err
			}
			attempts, err = s.state.ListKindForIdentityBounded(ctx, q, attemptPrefix, state.MaxStateIdentityListLimit)
			if err != nil {
				return fmt.Errorf("load legacy allocation attempts: %w", err)
			}
			if len(attempts) == state.MaxStateIdentityListLimit {
				return fmt.Errorf("%w: legacy allocation migration requires quiesced maintenance beyond %d attempts", llm.ErrAllocationUnavailable, state.MaxStateIdentityListLimit-1)
			}
			if int64(len(attempts)) != t.Attempts {
				return llm.ErrAllocationInvalid
			}
		} else if !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("load legacy allocation: %w", err)
		}
		next, err := record(q, totalKind, t)
		if err != nil {
			return err
		}
		expectations := []state.SlotExpectation{state.InternalSlotExpectation(storedQ, kind, ""), state.InternalSlotExpectation(q, totalKind, legacy.ID)}
		// The marker has no top-level allocation; every older reader rejects it.
		marker := state.NewInternalRecord(state.NewEventID(), q, totalKind, []byte(`{"schema_version":2,"migrated":true}`))
		writes := []state.StateRecord{next, marker}
		for _, prior := range attempts {
			id := strings.TrimPrefix(prior.Kind, attemptPrefix)
			if id == "" || len(id) > 128 {
				return llm.ErrAllocationInvalid
			}
			var value attempt
			if err := json.Unmarshal(prior.Bytes, &value); err != nil {
				return fmt.Errorf("decode legacy attempt: %w", err)
			}
			copied, err := record(q, attemptPrefix+id, value)
			if err != nil {
				return err
			}
			expectations = append(expectations, allocationExpectation(q, attemptPrefix+id, ""), state.InternalSlotExpectation(q, prior.Kind, prior.ID))
			writes = append(writes, copied)
		}
		if err := s.state.SaveBatchIf(ctx, expectations, writes); errors.Is(err, state.ErrConditionFailed) {
			continue
		} else if err != nil {
			return fmt.Errorf("pin protected allocation: %w", err)
		}
		return nil
	}
	return fmt.Errorf("allocation migration contention: %w", state.ErrConditionFailed)
}
