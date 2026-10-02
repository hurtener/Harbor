package allocation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestAllocation_CloseSerializesReservationsAndRetainsLiability(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			var st state.StateStore
			var err error
			switch driver {
			case "inmem":
				st, err = inmem.New(config.StateConfig{})
			case "sqlite":
				st, err = sqlite.New(config.StateConfig{DSN: filepath.Join(t.TempDir(), "finality.sqlite")})
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
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "close-" + string(state.NewEventID()), UserID: "owner", SessionID: "session"}, RunID: "root"}
			a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 10000}
			mgr := allocation.New(st)
			if err = mgr.Reserve(t.Context(), q, a, "already-in-flight", 100); err != nil {
				t.Fatal(err)
			}
			if err = mgr.Reserve(t.Context(), q, a, "unknown", 100); err != nil {
				t.Fatal(err)
			}
			if err = mgr.Settle(t.Context(), q, a, "unknown", nil, false); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var accepted atomic.Int64
			var wg sync.WaitGroup
			for i := range 100 {
				wg.Go(func() {
					<-start
					manager := allocation.New(st)
					attempt := fmt.Sprint(i)
					reserveErr := manager.Reserve(t.Context(), q, a, attempt, 10)
					if errors.Is(reserveErr, llm.ErrAllocationClosed) {
						return
					}
					if reserveErr != nil {
						t.Errorf("reserve: %v", reserveErr)
						return
					}
					accepted.Add(1)
					if settleErr := manager.Settle(t.Context(), q, a, attempt, new(int64(1)), false); settleErr != nil {
						t.Errorf("settle: %v", settleErr)
					}
				})
			}
			close(start)
			if err = mgr.Close(t.Context(), q, a); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			for range 100 {
				if err = allocation.New(st).Reserve(t.Context(), q, a, "late-helper", 1); !errors.Is(err, llm.ErrAllocationClosed) {
					t.Fatalf("closed helper: %v", err)
				}
			}
			if err = mgr.Settle(t.Context(), q, a, "already-in-flight", new(int64(3)), false); err != nil {
				t.Fatal(err)
			}
			snapshot, err := allocation.New(st).Snapshot(t.Context(), q, a)
			if err != nil || !snapshot.Closed || snapshot.ReservedTokens != 100 || snapshot.UnknownTokens != 100 || snapshot.SettledTokens != 3+accepted.Load() {
				t.Fatalf("closed snapshot: %+v %v", snapshot, err)
			}
			if _, err = st.DeleteScope(t.Context(), q.Identity); err != nil {
				t.Fatal(err)
			}
			erased, err := allocation.New(st).Snapshot(t.Context(), q, a)
			if err != nil || !erased.Closed || erased.ReservedTokens != 100 || erased.UnknownTokens != 100 || erased.SettledTokens != snapshot.SettledTokens {
				t.Fatalf("session erasure lost funding barrier: %+v %v", erased, err)
			}
			if err = allocation.New(st).Reserve(t.Context(), q, a, "erased-session-helper", 1); !errors.Is(err, llm.ErrAllocationClosed) {
				t.Fatalf("erased helper reopened funding: %v", err)
			}
			if err = mgr.Close(t.Context(), q, a); err != nil {
				t.Fatal(err)
			}
			changed := a
			changed.Revision++
			if err = mgr.Close(t.Context(), q, changed); !errors.Is(err, llm.ErrAllocationInvalid) {
				t.Fatalf("changed funding: %v", err)
			}
			for _, part := range []string{"tenant", "user", "session", "task"} {
				foreign := q
				switch part {
				case "tenant":
					foreign.TenantID += "-other"
				case "user":
					foreign.UserID += "-other"
				case "session":
					foreign.SessionID += "-other"
				case "task":
					foreign.RunID += "-other"
				}
				if err = mgr.Reserve(t.Context(), foreign, a, "foreign", 1); err != nil {
					t.Fatalf("cross-%s closure: %v", part, err)
				}
			}
		})
	}
}

func TestAllocation_CloseACKLossAndSQLiteReopen(t *testing.T) {
	cfg := config.StateConfig{DSN: filepath.Join(t.TempDir(), "closed.sqlite")}
	st, err := sqlite.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "root"}
	a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 100}
	if err = allocation.New(st).Ensure(t.Context(), q, a); err != nil {
		t.Fatal(err)
	}
	lost := &lostAcknowledgment{StateStore: st}
	lost.lose.Store(true)
	if err = allocation.New(lost).Close(t.Context(), q, a); err == nil {
		t.Fatal("expected committed close acknowledgment loss")
	}
	if err = st.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err = sqlite.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	mgr := allocation.New(st)
	if err = mgr.Close(t.Context(), q, a); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Reserve(t.Context(), q, a, "after-reopen", 1); !errors.Is(err, llm.ErrAllocationClosed) {
		t.Fatalf("reopened funding: %v", err)
	}
	snapshot, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || !snapshot.Closed || snapshot.AttemptCount != 0 || snapshot.ReservedTokens != 0 {
		t.Fatalf("empty closed allocation: %+v %v", snapshot, err)
	}
}

func TestAllocation_CloseUpgradeFencesLegacyReaderAndLoadedWriter(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "root"}
	a := llm.InferenceAllocation{AllocationID: "funding", Revision: 1, MaxTotalTokens: 100}
	const kind = "harbor.internal/inference.allocation"
	// Exact legacy funding placement: older readers validate this top-level
	// allocation before a provider call or any attempt to write accounting.
	legacy, err := json.Marshal(map[string]any{"allocation": a, "settled": 7})
	if err != nil {
		t.Fatal(err)
	}
	old := state.NewInternalRecord(state.NewEventID(), q, kind, legacy)
	if err = st.Save(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err = allocation.New(st).Close(t.Context(), q, a); err != nil {
		t.Fatal(err)
	}
	current, err := st.Load(t.Context(), q, kind)
	if err != nil {
		t.Fatal(err)
	}
	var oldReader struct {
		Allocation llm.InferenceAllocation `json:"allocation"`
	}
	if err = json.Unmarshal(current.Bytes, &oldReader); err != nil {
		t.Fatal(err)
	}
	if llm.EqualInferenceAllocation(&oldReader.Allocation, &a) {
		t.Fatal("legacy reader can ignore closure")
	}
	stale := state.NewInternalRecord(state.NewEventID(), q, kind, legacy)
	if err = st.SaveBatchIf(t.Context(), []state.SlotExpectation{state.InternalSlotExpectation(q, kind, old.ID)}, []state.StateRecord{stale}); !errors.Is(err, state.ErrConditionFailed) {
		t.Fatalf("loaded old writer bypassed close: %v", err)
	}
	snapshot, err := allocation.New(st).Snapshot(t.Context(), q, a)
	if err != nil || !snapshot.Closed || snapshot.SettledTokens != 7 {
		t.Fatalf("legacy migration: %+v %v", snapshot, err)
	}
}

func TestAllocation_CloseRetainsMonetaryEnvelopes(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	a, _ := monetaryAllocation(t, 100)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "root"}
	mgr := allocation.New(st)
	if err = mgr.ReserveMonetary(t.Context(), q, a, "known", 20, 30); err != nil {
		t.Fatal(err)
	}
	if err = mgr.ReserveMonetary(t.Context(), q, a, "unknown", 20, 40); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Close(t.Context(), q, a); err != nil {
		t.Fatal(err)
	}
	if err = mgr.ReserveMonetary(t.Context(), q, a, "late", 1, 1); !errors.Is(err, llm.ErrAllocationClosed) {
		t.Fatalf("closed monetary reserve: %v", err)
	}
	if err = mgr.Settle(t.Context(), q, a, "known", new(int64(3)), false); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Settle(t.Context(), q, a, "unknown", nil, false); err != nil {
		t.Fatal(err)
	}
	snapshot, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || !snapshot.Closed || snapshot.ChargedCostMicroUSD != 30 || snapshot.ReservedCostMicroUSD != 40 || snapshot.UnknownCostMicroUSD != 40 || snapshot.ReservedTokens != 20 {
		t.Fatalf("monetary close refunded liability: %+v %v", snapshot, err)
	}
}

func TestAllocation_CloseRejectsUnknownOrLegacyCompatibleEnvelope(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "root"}
	a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 100}
	for _, body := range []map[string]any{
		{"schema_version": 3, "total": map[string]any{"allocation": a, "closed": true}},
		{"allocation": a, "closed": true},
		{"schema_version": 2, "allocation": a, "total": map[string]any{"allocation": a, "closed": true}},
	} {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err = st.Save(t.Context(), state.NewInternalRecord(state.NewEventID(), q, "harbor.internal/inference.allocation", raw)); err != nil {
			t.Fatal(err)
		}
		if _, err = allocation.New(st).Snapshot(t.Context(), q, a); !errors.Is(err, llm.ErrAllocationInvalid) {
			t.Fatalf("unproven closed snapshot: %v", err)
		}
	}
}

func TestAllocation_ProtectedFundingAndSettlementSurviveSessionErasure(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "private-tenant-marker", UserID: "private-user-marker", SessionID: "private-session-marker"}, RunID: "private-task-marker"}
	a := llm.InferenceAllocation{AllocationID: "private-funding-marker", Revision: 1, MaxTotalTokens: 100}
	mgr := allocation.New(st)
	if err = mgr.Reserve(t.Context(), q, a, "attempt", 100); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DeleteScope(t.Context(), q.Identity); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Reserve(t.Context(), q, a, "late-before-close", 1); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatalf("erasure refunded open funding: %v", err)
	}
	if err = mgr.Close(t.Context(), q, a); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Settle(t.Context(), q, a, "attempt", new(int64(7)), false); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || !got.Closed || got.SettledTokens != 7 || got.ReservedTokens != 0 {
		t.Fatalf("erased settlement: %+v %v", got, err)
	}
	records, err := st.ListKind(t.Context(), state.ListScope{MaintenanceScoped: true}, "harbor.internal/inference.allocation.v2/")
	if err != nil || len(records) != 2 {
		t.Fatalf("protected record count %d: %v", len(records), err)
	}
	for _, record := range records {
		if !identity.IsInternalCoordination(record.Identity.Identity) {
			t.Fatal("accounting still session-local")
		}
		for _, marker := range []string{q.TenantID, q.UserID, q.SessionID, q.RunID, a.AllocationID} {
			if strings.Contains(string(record.Bytes), marker) || strings.Contains(record.Kind, marker) {
				t.Fatal("protected record retained raw scope or caller funding identity")
			}
		}
	}
}

func TestAllocation_LegacyAttemptMigrationSurvivesErasure(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "root"}
	a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 100}
	raw, err := json.Marshal(map[string]any{"allocation": a, "reserved": 40, "attempts": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Save(t.Context(), state.NewInternalRecord(state.NewEventID(), q, "harbor.internal/inference.allocation", raw)); err != nil {
		t.Fatal(err)
	}
	if err = st.Save(t.Context(), state.NewInternalRecord(state.NewEventID(), q, "harbor.internal/inference.allocation.attempt/original", []byte(`{"units":40,"status":"reserved"}`))); err != nil {
		t.Fatal(err)
	}
	mgr := allocation.New(st)
	if err = mgr.Close(t.Context(), q, a); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DeleteScope(t.Context(), q.Identity); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Settle(t.Context(), q, a, "original", new(int64(9)), false); err != nil {
		t.Fatal("legacy in-flight settlement lost", err)
	}
	if err = mgr.Reserve(t.Context(), q, a, "original", 40); !errors.Is(err, llm.ErrAllocationClosed) {
		t.Fatalf("legacy attempt repeated after erase: %v", err)
	}
	got, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || !got.Closed || got.SettledTokens != 9 || got.ReservedTokens != 0 {
		t.Fatalf("legacy erased snapshot: %+v %v", got, err)
	}
}

type migrateBetweenReads struct {
	state.StateStore
	once    sync.Once
	migrate func()
}

func (s *migrateBetweenReads) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	record, err := s.StateStore.Load(ctx, q, kind)
	if identity.IsInternalCoordination(q.Identity) && strings.HasSuffix(kind, "/total") && errors.Is(err, state.ErrNotFound) {
		s.once.Do(s.migrate)
	}
	return record, err
}
func TestAllocation_MigrationBetweenAbsentReadAndLegacyRead(t *testing.T) {
	for _, operation := range []string{"ensure", "snapshot", "reserve"} {
		t.Run(operation, func(t *testing.T) {
			raw, err := inmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close(context.Background()) }()
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "root"}
			a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 100}
			interleave := &migrateBetweenReads{StateStore: raw}
			interleave.migrate = func() {
				if err := allocation.New(raw).Ensure(t.Context(), q, a); err != nil {
					t.Fatal(err)
				}
			}
			mgr := allocation.New(interleave)
			switch operation {
			case "ensure":
				err = mgr.Ensure(t.Context(), q, a)
			case "snapshot":
				_, err = mgr.Snapshot(t.Context(), q, a)
			case "reserve":
				err = mgr.Reserve(t.Context(), q, a, "attempt", 10)
			}
			if err != nil {
				t.Fatalf("migration observation gap: %v", err)
			}
		})
	}
}

func TestAllocation_FingerprintRejectsInvalidUTF8Aliases(t *testing.T) {
	for _, bad := range []string{string([]byte{0xff}), string([]byte{0xfe})} {
		a := llm.InferenceAllocation{AllocationID: bad, Revision: 1, MaxTotalTokens: 100}
		if err := llm.ValidateInferenceAllocation(&a); !errors.Is(err, llm.ErrAllocationInvalid) {
			t.Fatal("invalid funding ID could alias canonical JSON", err)
		}
		a, _ = monetaryAllocation(t, 100)
		a.PricingManifestID = bad
		if err := llm.ValidateInferenceAllocation(&a); !errors.Is(err, llm.ErrAllocationInvalid) {
			t.Fatal("invalid pricing ID could alias canonical JSON", err)
		}
	}
}
