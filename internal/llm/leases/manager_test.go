package leases_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/leases"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func TestStore_AtomicReservationSettlementAndReplayAcrossDrivers(t *testing.T) {
	cases := []struct {
		name string
		open func(t *testing.T) (interface{ Close(context.Context) error }, llm.LeaseReservationManager, func() error)
	}{
		{name: "inmem", open: func(t *testing.T) (interface{ Close(context.Context) error }, llm.LeaseReservationManager, func() error) {
			st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			return st, mgr, func() error { return st.Close(context.Background()) }
		}},
		{name: "sqlite", open: func(t *testing.T) (interface{ Close(context.Context) error }, llm.LeaseReservationManager, func() error) {
			st, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "leases.sqlite")})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			return st, mgr, func() error { return st.Close(context.Background()) }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, manager, closeStore := tc.open(t)
			t.Cleanup(func() { _ = closeStore() })
			mgr := manager.(*leases.Store)
			now := time.Now().UTC()
			identityScope := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
			mk := func(id string) llm.LeaseReservationRequest {
				return llm.LeaseReservationRequest{AttemptID: id, GrantID: "grant", LeaseID: "lease", Epoch: 1, Capacity: 100, Units: 100, ExpiresAt: now.Add(time.Minute), Identity: identityScope}
			}
			var wg sync.WaitGroup
			results := make(chan error, 2)
			for _, id := range []string{"attempt-a", "attempt-b"} {
				wg.Add(1)
				go func(id string) { defer wg.Done(); _, err := mgr.Reserve(context.Background(), mk(id)); results <- err }(id)
			}
			wg.Wait()
			close(results)
			var successes, insufficient int
			for err := range results {
				if err == nil {
					successes++
				} else if errors.Is(err, leases.ErrInsufficient) || errors.Is(err, leases.ErrAttemptConflict) {
					insufficient++
				} else {
					t.Errorf("Reserve: %v", err)
				}
			}
			if successes != 1 || insufficient != 1 {
				t.Fatalf("concurrent reserve successes=%d insufficient=%d", successes, insufficient)
			}
			// A replay of the winning attempt is stable rather than a second claim.
			winning := "attempt-a"
			if _, err := mgr.Reserve(context.Background(), mk(winning)); err != nil {
				winning = "attempt-b"
				if _, err := mgr.Reserve(context.Background(), mk(winning)); err != nil {
					t.Fatal(err)
				}
			}
			receipt := testReceipt(winning, now)
			if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: winning, Receipt: receipt, Units: 7, Now: now}); err != nil {
				t.Fatalf("Settle: %v", err)
			}
			if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: winning, Receipt: receipt, Units: 7, Now: now}); err != nil {
				t.Fatalf("idempotent Settle: %v", err)
			}
			pending, err := mgr.PendingReceipts(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].ReceiptID != winning {
				t.Fatalf("pending=%+v", pending)
			}
			if err := mgr.TopUp(context.Background(), leases.TopUpRequest{LeaseID: "lease", Epoch: 2, Capacity: 200, ExpiresAt: now.Add(2 * time.Minute)}); err != nil {
				t.Fatalf("TopUp: %v", err)
			}
			if _, err := mgr.Reserve(context.Background(), llm.LeaseReservationRequest{AttemptID: "attempt-c", GrantID: "grant-2", LeaseID: "lease", Epoch: 2, Capacity: 200, Units: 100, ExpiresAt: now.Add(2 * time.Minute), Identity: identityScope}); err != nil {
				t.Fatalf("epoch top-up reserve: %v", err)
			}
			if err := mgr.Release(context.Background(), "attempt-c"); err != nil {
				t.Fatalf("Release: %v", err)
			}
		})
	}
}

func TestStore_ExpiryReleasesUnsettledReservation(t *testing.T) {
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	mgr, err := leases.New(st, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	_, err = mgr.Reserve(context.Background(), llm.LeaseReservationRequest{AttemptID: "expired", GrantID: "g", LeaseID: "l", Epoch: 1, Capacity: 4, Units: 4, ExpiresAt: now.Add(-time.Second), Identity: q})
	if !errors.Is(err, leases.ErrInsufficient) {
		t.Fatalf("expired reserve=%v", err)
	}
	_, err = mgr.Reserve(context.Background(), llm.LeaseReservationRequest{AttemptID: "will-expire", GrantID: "g", LeaseID: "l2", Epoch: 1, Capacity: 4, Units: 4, ExpiresAt: now.Add(time.Second), Identity: q})
	if err != nil {
		t.Fatal(err)
	}
	n, err := mgr.Expire(context.Background(), now.Add(2*time.Second), 10)
	if err != nil || n != 1 {
		t.Fatalf("Expire n=%d err=%v", n, err)
	}
}

func TestStore_FailedSettlementReleasesWithoutConsuming(t *testing.T) {
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	mgr, err := leases.New(st, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	request := llm.LeaseReservationRequest{AttemptID: "failed", GrantID: "g", LeaseID: "failed-lease", Epoch: 1, Capacity: 4, Units: 4, ExpiresAt: now.Add(time.Minute), Identity: q}
	if _, err := mgr.Reserve(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt("failed", now)
	receipt.Status = "error"
	hash, err := llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.CanonicalBodyHash = hash
	if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: request.AttemptID, Receipt: receipt, Units: request.Units, Now: now}); err != nil {
		t.Fatal(err)
	}
	// The released capacity can be reserved again in the same lease epoch.
	next := request
	next.AttemptID = "after-failure"
	if _, err := mgr.Reserve(context.Background(), next); err != nil {
		t.Fatalf("released capacity was not returned: %v", err)
	}
}

func testReceipt(id string, now time.Time) llm.AttemptUsageReceipt {
	r := llm.AttemptUsageReceipt{ReceiptID: id, GrantID: "grant", OrganizationID: "org", RuntimeID: "runtime", TenantID: "tenant", UserID: "user", SessionID: "session", LogicalRunID: "run", Provider: "openai", ProviderModelID: "model", ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, RouteID: "route", CredentialAssetGeneration: 1, PolicyGeneration: 1, AttemptNumber: 1, Status: "success", StartedAt: now, CompletedAt: now.Add(time.Millisecond), IdempotencyKey: id, TotalTokens: 7}
	h, _ := llm.CanonicalAttemptUsageReceiptBodyHash(r)
	r.CanonicalBodyHash = h
	return r
}
