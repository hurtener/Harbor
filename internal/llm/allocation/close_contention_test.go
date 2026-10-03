package allocation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type closeInterleavingStore struct {
	state.StateStore
	before func(context.Context) error
}

func (s *closeInterleavingStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	if len(writes) == 1 {
		var envelope struct {
			Total *struct {
				Closed bool `json:"closed"`
			} `json:"total"`
		}
		if err := json.Unmarshal(writes[0].Bytes, &envelope); err == nil && envelope.Total != nil && envelope.Total.Closed {
			if err := s.before(ctx); err != nil {
				return err
			}
		}
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

func TestAllocation_CloseWaitsForFiniteConcurrentProgress(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "close-progress", UserID: "owner", SessionID: "session"}, RunID: "root"}
	a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 1000}
	other := allocation.New(st)
	var interleavings atomic.Int64
	wrapped := &closeInterleavingStore{StateStore: st}
	wrapped.before = func(ctx context.Context) error {
		n := interleavings.Add(1)
		if n > 160 {
			return nil
		}
		id := fmt.Sprintf("accepted-%d", n)
		if err := other.Reserve(ctx, q, a, id, 1); err != nil {
			return err
		}
		// Two real generation changes land between Close's read and write.
		return other.Settle(ctx, q, a, id, new(int64(1)), false)
	}
	if err := allocation.New(wrapped).Close(t.Context(), q, a); err != nil {
		t.Fatalf("finite concurrent progress prevented closure: %v", err)
	}
	snapshot, err := other.Snapshot(t.Context(), q, a)
	if err != nil || !snapshot.Closed || snapshot.AttemptCount != 160 || snapshot.SettledTokens != 160 || snapshot.ReservedTokens != 0 {
		t.Fatalf("closed accounting: %+v %v", snapshot, err)
	}
	if err := other.Reserve(t.Context(), q, a, "after-close", 1); !errors.Is(err, llm.ErrAllocationClosed) {
		t.Fatalf("late reservation: %v", err)
	}
}

func TestAllocation_CloseContentionHonorsCancellationWithoutRefund(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			st, err := inmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "close-cancel", UserID: "owner", SessionID: mode}, RunID: "root"}
			a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 100}
			mgr := allocation.New(st)
			if err := mgr.Reserve(t.Context(), q, a, "unknown", 10); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Settle(t.Context(), q, a, "unknown", nil, false); err != nil {
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
			var attempts atomic.Int64
			wrapped := &closeInterleavingStore{StateStore: st, before: func(context.Context) error {
				attempts.Add(1)
				if mode == "cancel" {
					cancel()
				}
				return state.ErrConditionFailed
			}}
			if err := allocation.New(wrapped).Close(ctx, q, a); !errors.Is(err, want) {
				t.Fatalf("cancellation cause: got %v want %v", err, want)
			}
			if mode == "cancel" && attempts.Load() != 1 {
				t.Fatalf("continued after cancellation: %d attempts", attempts.Load())
			}
			snapshot, err := mgr.Snapshot(t.Context(), q, a)
			if err != nil || snapshot.Closed || snapshot.ReservedTokens != 10 || snapshot.UnknownTokens != 10 || snapshot.SettledTokens != 0 {
				t.Fatalf("cancelled close changed liability: %+v %v", snapshot, err)
			}
		})
	}
}
