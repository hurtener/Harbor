package allocation_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func TestAllocation_ConcurrentDurableLiability(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			var st state.StateStore
			var err error
			switch driver {
			case "inmem":
				st, err = inmem.New(config.StateConfig{})
			case "sqlite":
				st, err = sqlite.New(config.StateConfig{DSN: filepath.Join(t.TempDir(), "state.sqlite")})
			case "postgres":
				dsn := os.Getenv("HARBOR_PG_DSN")
				if dsn == "" {
					t.Skip("HARBOR_PG_DSN not set")
				}
				st, err = postgres.New(config.StateConfig{DSN: dsn})
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			ctx := t.Context()
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "allocation-" + string(state.NewEventID()), UserID: "user", SessionID: "session"}, RunID: "task"}
			a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 100}
			var successes atomic.Int64
			var wg sync.WaitGroup
			for i := range 100 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s := allocation.New(st)
					err := s.Reserve(ctx, q, a, fmt.Sprint(i), 10)
					if err == nil {
						successes.Add(1)
					} else if !errors.Is(err, llm.ErrAllocationExhausted) {
						t.Errorf("reserve: %v", err)
					}
				}()
			}
			wg.Wait()
			if successes.Load() != 10 {
				t.Fatalf("accepted %d want10", successes.Load())
			}
			snap, err := allocation.New(st).Snapshot(ctx, q, a)
			if err != nil || snap.ReservedTokens != 100 || snap.AttemptCount != 10 {
				t.Fatalf("snapshot %+v %v", snap, err)
			}
			// Restarting the manager cannot release a crash's outstanding liability.
			if err := allocation.New(st).Reserve(ctx, q, a, "after-restart", 1); !errors.Is(err, llm.ErrAllocationExhausted) {
				t.Fatalf("restart: %v", err)
			}
			q.RunID = "settlement"
			s := allocation.New(st)
			if err = s.Reserve(ctx, q, a, "known", 50); err != nil {
				t.Fatal(err)
			}
			used := int64(7)
			if err = s.Settle(ctx, q, a, "known", &used, false); err != nil {
				t.Fatal(err)
			}
			if err = s.Settle(ctx, q, a, "known", &used, false); err != nil {
				t.Fatal(err)
			}
			if err = s.Reserve(ctx, q, a, "unknown", 50); err != nil {
				t.Fatal(err)
			}
			if err = s.Settle(ctx, q, a, "unknown", nil, false); err != nil {
				t.Fatal(err)
			}
			snap, err = s.Snapshot(ctx, q, a)
			if err != nil || snap.SettledTokens != 7 || snap.ReservedTokens != 50 || snap.UnknownTokens != 50 {
				t.Fatalf("settlement %+v %v", snap, err)
			}
			altered := a
			altered.MaxTotalTokens++
			if _, err = s.Snapshot(ctx, q, altered); !errors.Is(err, llm.ErrAllocationInvalid) {
				t.Fatalf("changed allocation %v", err)
			}
			foreign := q
			foreign.UserID = "other"
			other, err := s.Snapshot(ctx, foreign, a)
			if err != nil || other.SettledTokens != 0 || other.ReservedTokens != 0 {
				t.Fatalf("foreign %+v %v", other, err)
			}
			// An SDK's final reported response cannot refund unseen intermediate calls.
			q.RunID = "hidden-retry"
			if err = s.Reserve(ctx, q, a, "envelope", 100); err != nil {
				t.Fatal(err)
			}
			if err = s.Settle(ctx, q, a, "envelope", &used, true); err != nil {
				t.Fatal(err)
			}
			snap, err = s.Snapshot(ctx, q, a)
			if err != nil || snap.SettledTokens != 7 || snap.ReservedTokens != 93 || snap.UnknownTokens != 93 {
				t.Fatalf("hidden %+v %v", snap, err)
			}
		})
	}
}

// A committed reservation with a lost response still owns its capacity. A
// later provider attempt may not treat an error return as proof of rollback.
type lostAcknowledgment struct {
	state.StateStore
	lose atomic.Bool
}

func (s *lostAcknowledgment) SaveBatchIf(ctx context.Context, e []state.SlotExpectation, w []state.StateRecord) error {
	if err := s.StateStore.SaveBatchIf(ctx, e, w); err != nil {
		return err
	}
	if s.lose.Swap(false) {
		return errors.New("synthetic committed acknowledgment loss")
	}
	return nil
}
func TestAllocation_CommitLossCannotRefundOrRepeat(t *testing.T) {
	raw, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close(context.Background()) }()
	st := &lostAcknowledgment{StateStore: raw}
	st.lose.Store(true)
	mgr := allocation.New(st)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	a := llm.InferenceAllocation{AllocationID: "a", Revision: 1, MaxTotalTokens: 100}
	if err = mgr.Reserve(t.Context(), q, a, "one", 100); err == nil {
		t.Fatal("expected lost acknowledgment")
	}
	if err = allocation.New(raw).Reserve(t.Context(), q, a, "two", 1); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatalf("lost reservation refunded: %v", err)
	}
	used := int64(10)
	st.lose.Store(true)
	if err = mgr.Settle(t.Context(), q, a, "one", &used, false); err == nil {
		t.Fatal("expected settlement acknowledgment loss")
	}
	if err = mgr.Settle(t.Context(), q, a, "one", &used, false); err != nil {
		t.Fatal(err)
	}
	snap, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || snap.SettledTokens != 10 || snap.ReservedTokens != 0 || len(snap.Receipts) != 1 {
		t.Fatalf("settlement %+v %v", snap, err)
	}
}

func TestAllocation_SQLiteReopenPreservesLiability(t *testing.T) {
	cfg := config.StateConfig{DSN: filepath.Join(t.TempDir(), "allocation.sqlite")}
	st, err := sqlite.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	a := llm.InferenceAllocation{AllocationID: "a", Revision: 1, MaxTotalTokens: 100}
	if err = allocation.New(st).Reserve(t.Context(), q, a, "before-crash", 100); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err = sqlite.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	if err = allocation.New(st).Reserve(t.Context(), q, a, "after-restart", 1); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatalf("restart released liability: %v", err)
	}
}

func TestAllocation_ReportedOverrunLatchesBreach(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	mgr := allocation.New(st)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	a := llm.InferenceAllocation{AllocationID: "a", Revision: 1, MaxTotalTokens: 1000}
	if err = mgr.Reserve(t.Context(), q, a, "one", 100); err != nil {
		t.Fatal(err)
	}
	actual := int64(101)
	if err = mgr.Settle(t.Context(), q, a, "one", &actual, false); err != nil {
		t.Fatal(err)
	}
	snap, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || !snap.BoundBreached || snap.SettledTokens != 101 {
		t.Fatalf("breach %+v %v", snap, err)
	}
	if err = mgr.Reserve(t.Context(), q, a, "two", 1); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatalf("breached profile reused %v", err)
	}
}

func TestAllocation_ConcurrentIdentityCancellationIsolation(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	mgr := allocation.New(st)
	a := llm.InferenceAllocation{AllocationID: "shared-label", Revision: 1, MaxTotalTokens: 10}
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: fmt.Sprint(i), SessionID: "same"}, RunID: "same"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if i%2 == 0 {
				cancel()
			}
			err := mgr.Reserve(ctx, q, a, "same-attempt", 10)
			if i%2 == 0 {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("cancel %d: %v", i, err)
				}
			} else if err != nil {
				t.Errorf("uncancelled %d: %v", i, err)
			}
			snap, err := mgr.Snapshot(t.Context(), q, a)
			want := int64(10)
			if i%2 == 0 {
				want = 0
			}
			if err != nil || snap.ReservedTokens != want {
				t.Errorf("scope %d: %+v %v", i, snap, err)
			}
		}()
	}
	wg.Wait()
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Fatalf("allocation leaked goroutines: before%d after%d", baseline, got)
	}
}

func TestAllocation_InvalidInputsRetainedRemainderAndReceiptBound(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	s := allocation.New(st)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 10000}
	if _, err = s.Snapshot(t.Context(), identity.Quadruple{}, a); err == nil {
		t.Fatal("empty scope admitted")
	}
	bad := q
	bad.RunID = ""
	if err = s.Reserve(t.Context(), bad, a, "id", 1); !errors.Is(err, llm.ErrAllocationInvalid) {
		t.Fatal(err)
	}
	if _, err = allocation.New(nil).Snapshot(t.Context(), q, a); !errors.Is(err, llm.ErrAllocationUnavailable) {
		t.Fatal(err)
	}
	if err = s.Reserve(t.Context(), q, a, "", 1); !errors.Is(err, llm.ErrAllocationInvalid) {
		t.Fatal(err)
	}
	if err = s.Reserve(t.Context(), q, a, "id", 0); !errors.Is(err, llm.ErrAllocationInvalid) {
		t.Fatal(err)
	}
	negative := int64(-1)
	if err = s.Settle(t.Context(), q, a, "id", &negative, false); !errors.Is(err, llm.ErrAllocationInvalid) {
		t.Fatal(err)
	}
	if err = s.Settle(t.Context(), q, a, "missing", nil, false); err == nil {
		t.Fatal("missing reservation settled")
	}
	used := int64(1)
	for i := range 40 {
		id := fmt.Sprint(i)
		if err = s.Reserve(t.Context(), q, a, id, 10); err != nil {
			t.Fatal(err)
		}
		if err = s.Settle(t.Context(), q, a, id, &used, true); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Snapshot(t.Context(), q, a)
	if err != nil || len(got.Receipts) != 32 || !got.ReceiptsTruncated || got.SettledTokens != 40 || got.UnknownTokens != 360 || got.ReservedTokens != 360 {
		t.Fatalf("bounded receipts=%+v err=%v", got, err)
	}
	changed := int64(2)
	if err = s.Settle(t.Context(), q, a, "39", &changed, true); !errors.Is(err, llm.ErrAllocationInvalid) {
		t.Fatalf("changed settlement=%v", err)
	}
	if err = st.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.Reserve(t.Context(), q, a, "closed", 1); err == nil {
		t.Fatal("closed state admitted")
	}
}
