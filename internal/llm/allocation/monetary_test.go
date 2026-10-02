package allocation_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/llm/pricing"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func monetaryManifest() pricing.Manifest {
	return pricing.Manifest{ID: "synthetic-monetary", Revision: 1, Currency: "USD", Tariffs: []pricing.Tariff{{EndpointBinding: "provider_default", Provider: "openai", Model: "fixture-v1", ModelVersion: "fixture-v1", ImmutableModelVersion: true, IncludesAllCharges: true, InputMicroUSDPerMillion: new(int64(1)), OutputMicroUSDPerMillion: new(int64(1)), CacheReadMicroUSDPerMillion: new(int64(0)), CacheWriteMicroUSDPerMillion: new(int64(0)), ReasoningMicroUSDPerMillion: new(int64(0)), RequestMicroUSD: new(int64(1)), AncillaryMicroUSD: new(int64(1))}}}
}

func monetaryAllocation(t *testing.T, money int64) (llm.InferenceAllocation, *pricing.Catalog) {
	t.Helper()
	c, err := pricing.New([]pricing.Manifest{monetaryManifest()})
	if err != nil {
		t.Fatal(err)
	}
	r := c.References()[0]
	return llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 1000000, MaxCostMicroUSD: &money, PricingManifestID: r.ID, PricingManifestRevision: r.Revision, PricingManifestSHA256: r.SHA256}, c
}

func TestMonetaryAllocation_ConcurrentAtomicCapsAndDurableLiability(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			var st state.StateStore
			var err error
			switch driver {
			case "inmem":
				st, err = inmem.New(config.StateConfig{})
			case "sqlite":
				st, err = sqlite.New(config.StateConfig{DSN: filepath.Join(t.TempDir(), "state.db")})
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
			defer func() { _ = st.Close(context.Background()) }()
			a, c := monetaryAllocation(t, 100)
			ctx := t.Context()
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "money-" + string(state.NewEventID()), UserID: "owner", SessionID: "session"}, RunID: "task"}
			if err = allocation.BindPricingCatalog(ctx, st, c); err != nil {
				t.Fatal(err)
			}
			var count atomic.Int64
			var wg sync.WaitGroup
			for i := range 100 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := allocation.New(st).ReserveMonetary(ctx, q, a, fmt.Sprint(i), 1, 10)
					if err == nil {
						count.Add(1)
					} else if !errors.Is(err, llm.ErrAllocationExhausted) {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			snap, err := allocation.New(st).Snapshot(ctx, q, a)
			if err != nil || count.Load() != 10 || snap.ReservedCostMicroUSD != 100 || snap.AttemptCount != 10 || snap.ReservedTokens != 10 {
				t.Fatalf("count=%d snapshot=%+v err=%v", count.Load(), snap, err)
			}
			if err = allocation.New(st).ReserveMonetary(ctx, q, a, "restart", 1, 1); !errors.Is(err, llm.ErrAllocationExhausted) {
				t.Fatal("restart released hold", err)
			}
			other := q
			other.UserID = "different-owner"
			if err = allocation.New(st).ReserveMonetary(ctx, other, a, "separate", 1, 100); err != nil {
				t.Fatal("scope bleed", err)
			}
			changed := llm.CloneInferenceAllocation(&a)
			*changed.MaxCostMicroUSD = 101
			if _, err = allocation.New(st).Snapshot(ctx, q, *changed); !errors.Is(err, llm.ErrAllocationInvalid) {
				t.Fatal("cap changed", err)
			}
			if err = allocation.New(st).Reserve(ctx, q, a, "unpriced", 1); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
				t.Fatal("token-only bypass", err)
			}
		})
	}
}

func TestMonetaryAllocation_SettlementCeilingsAreNotSpend(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	a, _ := monetaryAllocation(t, 100)
	ctx := t.Context()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	mgr := allocation.New(st)
	for _, tc := range []struct {
		id               string
		used             *int64
		retain           bool
		charged, unknown int64
		status           string
	}{
		{"known", new(int64(7)), false, 10, 0, "charged_ceiling"},
		{"retry", new(int64(7)), true, 0, 10, "unknown"},
		{"missing", nil, false, 0, 10, "unknown"},
		{"cancelled-before-entry", new(int64(0)), false, 0, 0, "released_before_dispatch"},
	} {
		q.RunID = tc.id
		if err = mgr.ReserveMonetary(ctx, q, a, tc.id, 100, 10); err != nil {
			t.Fatal(err)
		}
		if err = mgr.Settle(ctx, q, a, tc.id, tc.used, tc.retain); err != nil {
			t.Fatal(err)
		}
		if err = mgr.Settle(ctx, q, a, tc.id, tc.used, tc.retain); err != nil {
			t.Fatal("exact replay", err)
		}
		snap, err := mgr.Snapshot(ctx, q, a)
		if err != nil || snap.ChargedCostMicroUSD != tc.charged || snap.UnknownCostMicroUSD != tc.unknown || snap.Receipts[0].MonetaryStatus != tc.status {
			t.Fatalf("%s %+v %v", tc.id, snap, err)
		}
		if err = mgr.Settle(ctx, q, a, tc.id, tc.used, !tc.retain); !errors.Is(err, llm.ErrAllocationInvalid) {
			t.Fatal("changed settlement", err)
		}
	}
	q.RunID = "breach"
	if err = mgr.ReserveMonetary(ctx, q, a, "bad", 100, 10); err != nil {
		t.Fatal(err)
	}
	if err = mgr.ReportBoundViolation(ctx, q, a, "bad"); err != nil {
		t.Fatal(err)
	}
	snap, err := mgr.Snapshot(ctx, q, a)
	if err != nil || !snap.BoundBreached || snap.UnknownCostMicroUSD != 10 {
		t.Fatal(snap, err)
	}
	if err = mgr.ReserveMonetary(ctx, q, a, "after", 1, 1); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatal(err)
	}
}

func TestMonetaryAllocation_SQLiteRestartAndCatalogDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := sqlite.New(config.StateConfig{DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	a, c := monetaryAllocation(t, math.MaxInt64)
	ctx := t.Context()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	if err = allocation.BindPricingCatalog(ctx, st, c); err != nil {
		t.Fatal(err)
	}
	if err = allocation.New(st).ReserveMonetary(ctx, q, a, "lost-response", 100, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err = sqlite.New(config.StateConfig{DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	if err = allocation.BindPricingCatalog(ctx, st, c); err != nil {
		t.Fatal("same catalog", err)
	}
	if err = allocation.New(st).ReserveMonetary(ctx, q, a, "retry", 1, 1); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatal("overflow/restart", err)
	}
	if err = allocation.New(st).Settle(ctx, q, a, "lost-response", nil, false); err != nil {
		t.Fatal(err)
	}
	snap, err := allocation.New(st).Snapshot(ctx, q, a)
	if err != nil || snap.UnknownCostMicroUSD != math.MaxInt64 {
		t.Fatal(snap, err)
	}
	m := monetaryManifest()
	m.Tariffs[0].AncillaryMicroUSD = new(int64(99))
	changed, err := pricing.New([]pricing.Manifest{m})
	if err != nil {
		t.Fatal(err)
	}
	if err = allocation.BindPricingCatalog(ctx, st, changed); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatal("changed revision at restart", err)
	}
	m.Revision++
	next, err := pricing.New([]pricing.Manifest{m})
	if err != nil {
		t.Fatal(err)
	}
	if err = allocation.BindPricingCatalog(ctx, st, next); err != nil {
		t.Fatal("new revision", err)
	}
	if err = next.ValidateReference(a.PricingReference()); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatal("old task silently repriced", err)
	}
}

func TestMonetaryAllocation_LostAcknowledgmentsDoNotRefund(t *testing.T) {
	raw, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close(context.Background()) }()
	st := &lostAcknowledgment{StateStore: raw}
	mgr := allocation.New(st)
	a, _ := monetaryAllocation(t, 10)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	if err = mgr.Ensure(t.Context(), q, a); err != nil {
		t.Fatal(err)
	}
	st.lose.Store(true)
	if err = mgr.ReserveMonetary(t.Context(), q, a, "one", 100, 10); err == nil {
		t.Fatal("expected committed lost acknowledgment")
	}
	if err = allocation.New(raw).ReserveMonetary(t.Context(), q, a, "two", 1, 1); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatal("lost money hold refunded", err)
	}
	st.lose.Store(true)
	if err = mgr.Settle(t.Context(), q, a, "one", nil, false); err == nil {
		t.Fatal("expected lost settlement acknowledgment")
	}
	if err = mgr.Settle(t.Context(), q, a, "one", nil, false); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || got.UnknownCostMicroUSD != 10 || got.ReservedCostMicroUSD != 10 || len(got.Receipts) != 1 {
		t.Fatal(got, err)
	}
}
