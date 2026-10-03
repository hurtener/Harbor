package allocation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type accountingInterleavingStore struct {
	state.StateStore
	status string
	before func(context.Context) error
}

func (s *accountingInterleavingStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	if len(writes) == 2 {
		var attempt struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(writes[1].Bytes, &attempt); err == nil && attempt.Status == s.status {
			if err := s.before(ctx); err != nil {
				return err
			}
		}
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

func TestAllocation_AccountingWaitsForFiniteConcurrentProgress(t *testing.T) {
	for _, status := range []string{"reserved", "settled"} {
		t.Run(status, func(t *testing.T) {
			st, err := inmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "accounting-progress", UserID: "owner", SessionID: status}, RunID: "root"}
			a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 1000}
			other := allocation.New(st)
			if err := other.Reserve(t.Context(), q, a, "in-flight", 10); err != nil {
				t.Fatal(err)
			}
			// All these attempts were accepted before closure; late authoritative
			// settlement must remain possible after the barrier.
			for i := range 160 {
				if err := other.Reserve(t.Context(), q, a, fmt.Sprint(i), 1); err != nil {
					t.Fatal(err)
				}
			}
			if status == "settled" {
				if err := other.Close(t.Context(), q, a); err != nil {
					t.Fatal(err)
				}
			}
			interleavings := 0
			wrapped := &accountingInterleavingStore{StateStore: st, status: status, before: func(ctx context.Context) error {
				if interleavings >= 160 {
					return nil
				}
				id := fmt.Sprint(interleavings)
				interleavings++
				return other.Settle(ctx, q, a, id, new(int64(1)), false)
			}}
			manager := allocation.New(wrapped)
			if status == "reserved" {
				err = manager.Reserve(t.Context(), q, a, "new", 10)
			} else {
				err = manager.Settle(t.Context(), q, a, "in-flight", new(int64(3)), false)
			}
			if err != nil {
				t.Fatalf("finite concurrent progress prevented %s: %v", status, err)
			}
			snapshot, err := other.Snapshot(t.Context(), q, a)
			if err != nil {
				t.Fatal(err)
			}
			if status == "reserved" && (snapshot.Closed || snapshot.ReservedTokens != 20 || snapshot.SettledTokens != 160 || snapshot.AttemptCount != 162) {
				t.Fatalf("reservation accounting: %+v", snapshot)
			}
			if status == "settled" && (!snapshot.Closed || snapshot.ReservedTokens != 0 || snapshot.SettledTokens != 163 || snapshot.AttemptCount != 161) {
				t.Fatalf("settlement accounting: %+v", snapshot)
			}
		})
	}
}

func TestAllocation_AccountingContentionHonorsCancellationWithoutRefund(t *testing.T) {
	for _, status := range []string{"reserved", "settled"} {
		for _, mode := range []string{"cancel", "deadline"} {
			t.Run(status+"/"+mode, func(t *testing.T) {
				st, err := inmem.New(config.StateConfig{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close(context.Background()) })
				q := identity.Quadruple{Identity: identity.Identity{TenantID: "accounting-cancel", UserID: "owner", SessionID: status + mode}, RunID: "root"}
				a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 100}
				other := allocation.New(st)
				if err := other.Reserve(t.Context(), q, a, "in-flight", 10); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				want := context.Canceled
				if mode == "deadline" {
					var deadlineCancel context.CancelFunc
					ctx, deadlineCancel = context.WithTimeout(ctx, 20*time.Millisecond)
					defer deadlineCancel()
					want = context.DeadlineExceeded
				}
				attempts := 0
				wrapped := &accountingInterleavingStore{StateStore: st, status: status, before: func(context.Context) error {
					attempts++
					if mode == "cancel" {
						cancel()
					}
					return state.ErrConditionFailed
				}}
				manager := allocation.New(wrapped)
				if status == "reserved" {
					err = manager.Reserve(ctx, q, a, "new", 10)
				} else {
					err = manager.Settle(ctx, q, a, "in-flight", new(int64(3)), false)
				}
				if !errors.Is(err, want) {
					t.Fatalf("cancellation cause: got %v want %v", err, want)
				}
				if mode == "cancel" && attempts != 1 {
					t.Fatalf("continued after cancellation: %d", attempts)
				}
				snapshot, err := other.Snapshot(t.Context(), q, a)
				if err != nil || snapshot.ReservedTokens != 10 || snapshot.SettledTokens != 0 || snapshot.UnknownTokens != 0 || snapshot.AttemptCount != 1 {
					t.Fatalf("cancelled accounting changed liability: %+v %v", snapshot, err)
				}
			})
		}
	}
}

func TestAllocation_AccountingStopsWithoutGenerationProgress(t *testing.T) {
	for _, status := range []string{"reserved", "settled"} {
		t.Run(status, func(t *testing.T) {
			st, err := inmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "no-progress", UserID: "owner", SessionID: status}, RunID: "root"}
			a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 100}
			other := allocation.New(st)
			if err := other.Reserve(t.Context(), q, a, "in-flight", 10); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			wrapped := &accountingInterleavingStore{StateStore: st, status: status, before: func(context.Context) error {
				attempts++
				if attempts > 128 {
					t.Fatal("unchanged predicate exceeded original retry budget")
				}
				return state.ErrConditionFailed
			}}
			manager := allocation.New(wrapped)
			// Deliberately has no deadline: a broken predicate must still be bounded.
			if status == "reserved" {
				err = manager.Reserve(context.Background(), q, a, "new", 10)
			} else {
				err = manager.Settle(context.Background(), q, a, "in-flight", new(int64(3)), false)
			}
			if !errors.Is(err, state.ErrConditionFailed) || attempts != 128 {
				t.Fatalf("no-progress result: attempts=%d err=%v", attempts, err)
			}
			snapshot, err := other.Snapshot(t.Context(), q, a)
			if err != nil || snapshot.ReservedTokens != 10 || snapshot.SettledTokens != 0 || snapshot.AttemptCount != 1 {
				t.Fatalf("stalled accounting changed liability: %+v %v", snapshot, err)
			}
			if err := other.Settle(t.Context(), q, a, "in-flight", new(int64(3)), false); err != nil {
				t.Fatal(err)
			}
			if err := other.Settle(t.Context(), q, a, "in-flight", new(int64(3)), false); err != nil {
				t.Fatal(err)
			}
			snapshot, err = other.Snapshot(t.Context(), q, a)
			if err != nil || snapshot.ReservedTokens != 0 || snapshot.SettledTokens != 3 || snapshot.AttemptCount != 1 {
				t.Fatalf("retry settlement was not exact-once: %+v %v", snapshot, err)
			}
		})
	}
}
