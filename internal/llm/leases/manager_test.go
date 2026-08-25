package leases_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
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
				return llm.LeaseReservationRequest{AttemptID: id, LogicalCallID: id, AttemptNonce: "nonce-" + id, GrantID: "grant", LeaseID: "lease", OrganizationID: "org", RuntimeID: "runtime", Epoch: 1, Capacity: 100, Units: 100, ExpiresAt: now.Add(time.Minute), Identity: identityScope}
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
				switch {
				case err == nil:
					successes++
				case errors.Is(err, leases.ErrInsufficient) || errors.Is(err, leases.ErrAttemptConflict):
					insufficient++
				default:
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
			request := mk(winning)
			if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: winning, LogicalCallID: request.LogicalCallID, AttemptNonce: request.AttemptNonce, Receipt: receipt, Units: 7, Now: now}); err != nil {
				t.Fatalf("Settle: %v", err)
			}
			if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: winning, LogicalCallID: request.LogicalCallID, AttemptNonce: request.AttemptNonce, Receipt: receipt, Units: 7, Now: now}); err != nil {
				t.Fatalf("idempotent Settle: %v", err)
			}
			pending, err := mgr.PendingReceipts(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].ReceiptID != winning {
				t.Fatalf("pending=%+v", pending)
			}
			if err := mgr.TopUp(context.Background(), leases.TopUpRequest{GrantID: "grant", LeaseID: "lease", OrganizationID: "org", RuntimeID: "runtime", Identity: identityScope, Epoch: 2, Capacity: 200, ExpiresAt: now.Add(2 * time.Minute)}); err != nil {
				t.Fatalf("TopUp: %v", err)
			}
			if _, err := mgr.Reserve(context.Background(), llm.LeaseReservationRequest{AttemptID: "attempt-c", LogicalCallID: "attempt-c", AttemptNonce: "nonce-attempt-c", GrantID: "grant", LeaseID: "lease", OrganizationID: "org", RuntimeID: "runtime", Epoch: 2, Capacity: 200, Units: 100, ExpiresAt: now.Add(2 * time.Minute), Identity: identityScope}); err != nil {
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
	_, err = mgr.Reserve(context.Background(), llm.LeaseReservationRequest{AttemptID: "expired", LogicalCallID: "expired", AttemptNonce: "nonce-expired", GrantID: "g", LeaseID: "l", OrganizationID: "org", RuntimeID: "runtime", Epoch: 1, Capacity: 4, Units: 4, ExpiresAt: now.Add(-time.Second), Identity: q})
	if !errors.Is(err, leases.ErrInsufficient) {
		t.Fatalf("expired reserve=%v", err)
	}
	_, err = mgr.Reserve(context.Background(), llm.LeaseReservationRequest{AttemptID: "will-expire", LogicalCallID: "will-expire", AttemptNonce: "nonce-will-expire", GrantID: "g", LeaseID: "l2", OrganizationID: "org", RuntimeID: "runtime", Epoch: 1, Capacity: 4, Units: 4, ExpiresAt: now.Add(time.Second), Identity: q})
	if err != nil {
		t.Fatal(err)
	}
	n, err := mgr.Expire(context.Background(), now.Add(2*time.Second), 10)
	if err != nil || n != 1 {
		t.Fatalf("Expire n=%d err=%v", n, err)
	}
}

func TestStore_FailedSettlementChargesObservedUsageAndReleasesOnlyUnused(t *testing.T) {
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
	request := llm.LeaseReservationRequest{AttemptID: "failed", LogicalCallID: "failed", AttemptNonce: "nonce-failed", GrantID: "g", LeaseID: "failed-lease", OrganizationID: "org", RuntimeID: "runtime", Epoch: 1, Capacity: 4, Units: 4, ExpiresAt: now.Add(time.Minute), Identity: q}
	if _, err := mgr.Reserve(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt("failed", now)
	receipt.GrantID = request.GrantID
	receipt.TenantID, receipt.UserID, receipt.SessionID, receipt.LogicalRunID = q.TenantID, q.UserID, q.SessionID, q.RunID
	receipt.Status = "error"
	receipt.TotalTokens = 3
	hash, err := llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.CanonicalBodyHash = hash
	if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: request.AttemptID, LogicalCallID: request.LogicalCallID, AttemptNonce: request.AttemptNonce, Receipt: receipt, Units: 3, Now: now}); err != nil {
		t.Fatal(err)
	}
	// The released capacity can be reserved again in the same lease epoch.
	next := request
	next.AttemptID = "after-failure"
	next.LogicalCallID = "after-failure"
	next.AttemptNonce = "nonce-after-failure"
	next.Units = 1
	if _, err := mgr.Reserve(context.Background(), next); err != nil {
		t.Fatalf("released capacity was not returned: %v", err)
	}
}

func TestStore_PendingReceiptHandoffRecoversAllTerminalOutcomesAcrossDrivers(t *testing.T) {
	cases := []struct {
		name string
		open func(t *testing.T) (*leases.Store, func() error)
	}{
		{name: "inmem", open: func(t *testing.T) (*leases.Store, func() error) {
			st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			return mgr, func() error { return st.Close(context.Background()) }
		}},
		{name: "sqlite", open: func(t *testing.T) (*leases.Store, func() error) {
			st, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "pending-receipts.sqlite")})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			return mgr, func() error { return st.Close(context.Background()) }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, closeStore := tc.open(t)
			t.Cleanup(func() { _ = closeStore() })
			now := time.Now().UTC()
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
			want := make(map[string]string)
			statuses := []string{"success", "error", "canceled", "success", "error", "canceled"}
			for i, status := range statuses {
				id := fmt.Sprintf("pending-%d", i)
				req := llm.LeaseReservationRequest{AttemptID: id, LogicalCallID: id, AttemptNonce: "nonce-" + id, GrantID: "grant", LeaseID: "lease-" + id, OrganizationID: "org", RuntimeID: "runtime", Epoch: 1, Capacity: 10, Units: 10, ExpiresAt: now.Add(time.Minute), Identity: q}
				if _, err := mgr.Reserve(context.Background(), req); err != nil {
					t.Fatalf("Reserve %s: %v", id, err)
				}
				receipt := testReceipt(id, now)
				receipt.Status = status
				receipt.TotalTokens = 3
				receipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
				if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: id, LogicalCallID: id, AttemptNonce: req.AttemptNonce, Receipt: receipt, Units: 3, Now: now}); err != nil {
					t.Fatalf("Settle %s: %v", id, err)
				}
				want[id] = status
			}

			// A crash after the outbox enqueue but before acknowledgement leaves
			// the same exact handoff visible; acknowledgement itself is replay-safe.
			first, err := mgr.PendingReceipts(context.Background(), 2)
			if err != nil || len(first) != 2 {
				t.Fatalf("first pending page len=%d err=%v", len(first), err)
			}
			replayed, err := mgr.PendingReceipts(context.Background(), 2)
			if err != nil || len(replayed) != len(first) {
				t.Fatalf("replayed pending page len=%d err=%v", len(replayed), err)
			}

			got := make(map[string]string)
			for len(got) < len(want) {
				page, err := mgr.PendingReceipts(context.Background(), 2)
				if err != nil {
					t.Fatal(err)
				}
				if len(page) == 0 {
					t.Fatalf("pending handoff ended early: got=%v want=%v", got, want)
				}
				for _, receipt := range page {
					got[receipt.ReceiptID] = receipt.Status
					if err := mgr.AcknowledgePendingReceipt(context.Background(), receipt); err != nil {
						t.Fatalf("acknowledge %s: %v", receipt.ReceiptID, err)
					}
					if err := mgr.AcknowledgePendingReceipt(context.Background(), receipt); err != nil {
						t.Fatalf("idempotent acknowledge %s: %v", receipt.ReceiptID, err)
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("terminal receipts=%v want=%v", got, want)
			}
			remaining, err := mgr.PendingReceipts(context.Background(), 2)
			if err != nil || len(remaining) != 0 {
				t.Fatalf("remaining pending=%+v err=%v", remaining, err)
			}
		})
	}
}

func TestStore_BindingOvershootAndCrashRecoveryAcrossDrivers(t *testing.T) {
	type openManager func(*testing.T, func() time.Time) (*leases.Store, func() error)
	cases := []struct {
		name string
		open openManager
	}{
		{name: "inmem", open: func(t *testing.T, clock func() time.Time) (*leases.Store, func() error) {
			st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, clock)
			if err != nil {
				t.Fatal(err)
			}
			return mgr, func() error { return st.Close(context.Background()) }
		}},
		{name: "sqlite", open: func(t *testing.T, clock func() time.Time) (*leases.Store, func() error) {
			st, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "lease-integrity.sqlite")})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, clock)
			if err != nil {
				t.Fatal(err)
			}
			return mgr, func() error { return st.Close(context.Background()) }
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			current := now
			mgr, closeStore := tc.open(t, func() time.Time { return current })
			t.Cleanup(func() { _ = closeStore() })
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}, RunID: "run-a"}
			base := llm.LeaseReservationRequest{
				AttemptID: "bound-a", LogicalCallID: "bound-a", AttemptNonce: "nonce-bound-a",
				GrantID: "grant-a", LeaseID: "shared-lease", OrganizationID: "org-a", RuntimeID: "runtime-a", AgentID: "agent-a",
				Epoch: 1, Capacity: 10, Units: 5, ExpiresAt: now.Add(time.Minute), Identity: q,
			}
			if _, err := mgr.Reserve(context.Background(), base); err != nil {
				t.Fatal(err)
			}
			for name, mutate := range map[string]func(*llm.LeaseReservationRequest){
				"capacity": func(r *llm.LeaseReservationRequest) { r.Capacity++ },
				"expiry":   func(r *llm.LeaseReservationRequest) { r.ExpiresAt = r.ExpiresAt.Add(time.Second) },
			} {
				t.Run("same-attempt-"+name+"-conflict", func(t *testing.T) {
					changed := base
					mutate(&changed)
					if _, err := mgr.Reserve(context.Background(), changed); !errors.Is(err, leases.ErrAttemptConflict) {
						t.Fatalf("same attempt with changed %s = %v, want ErrAttemptConflict", name, err)
					}
				})
			}

			for name, mutate := range map[string]func(*llm.LeaseReservationRequest){
				"grant":        func(r *llm.LeaseReservationRequest) { r.GrantID = "grant-b" },
				"organization": func(r *llm.LeaseReservationRequest) { r.OrganizationID = "org-b" },
				"runtime":      func(r *llm.LeaseReservationRequest) { r.RuntimeID = "runtime-b" },
				"identity": func(r *llm.LeaseReservationRequest) {
					r.Identity.UserID = "user-b"
				},
				"agent": func(r *llm.LeaseReservationRequest) { r.AgentID = "agent-b" },
			} {
				t.Run(name+"-collision", func(t *testing.T) {
					other := base
					other.AttemptID, other.LogicalCallID, other.AttemptNonce = "collision-"+name, "collision-"+name, "nonce-collision-"+name
					mutate(&other)
					if _, err := mgr.Reserve(context.Background(), other); !errors.Is(err, leases.ErrAttemptConflict) {
						t.Fatalf("same LeaseID collision = %v, want ErrAttemptConflict", err)
					}
				})
			}
			if err := mgr.TopUp(context.Background(), leases.TopUpRequest{
				GrantID: base.GrantID, LeaseID: base.LeaseID, OrganizationID: base.OrganizationID,
				RuntimeID: base.RuntimeID, AgentID: "agent-b", Identity: base.Identity,
				Epoch: 2, Capacity: 20, ExpiresAt: now.Add(2 * time.Minute),
			}); !errors.Is(err, leases.ErrAttemptConflict) {
				t.Fatalf("cross-agent top-up = %v, want ErrAttemptConflict", err)
			}

			receipt := testReceipt("bound-a", now)
			receipt.GrantID, receipt.OrganizationID, receipt.RuntimeID, receipt.AgentID = base.GrantID, base.OrganizationID, base.RuntimeID, base.AgentID
			receipt.TenantID, receipt.UserID, receipt.SessionID, receipt.LogicalRunID = q.TenantID, q.UserID, q.SessionID, q.RunID
			receipt.TotalTokens = 7 // provider truth may exceed the estimated reservation
			receipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
			if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: base.AttemptID, LogicalCallID: base.LogicalCallID, AttemptNonce: base.AttemptNonce, Receipt: receipt, Units: 7, Now: now}); err != nil {
				t.Fatalf("overshoot settlement: %v", err)
			}
			remaining := base
			remaining.AttemptID, remaining.LogicalCallID, remaining.AttemptNonce = "remaining", "remaining", "nonce-remaining"
			remaining.Units = 3
			if _, err := mgr.Reserve(context.Background(), remaining); err != nil {
				t.Fatalf("actual usage was not charged truthfully: %v", err)
			}
			tooMuch := remaining
			tooMuch.AttemptID, tooMuch.LogicalCallID, tooMuch.AttemptNonce = "too-much", "too-much", "nonce-too-much"
			tooMuch.Units = 1
			if _, err := mgr.Reserve(context.Background(), tooMuch); !errors.Is(err, leases.ErrInsufficient) {
				t.Fatalf("post-overshoot reserve = %v, want ErrInsufficient", err)
			}

			crash := base
			crash.AttemptID, crash.LogicalCallID, crash.AttemptNonce = "crash", "crash", "nonce-crash"
			crash.LeaseID, crash.Capacity, crash.Units, crash.ExpiresAt = "crash-lease", 4, 4, now.Add(time.Second)
			if _, err := mgr.Reserve(context.Background(), crash); err != nil {
				t.Fatal(err)
			}
			if err := mgr.TopUp(context.Background(), leases.TopUpRequest{
				GrantID: crash.GrantID, LeaseID: crash.LeaseID, OrganizationID: crash.OrganizationID,
				RuntimeID: crash.RuntimeID, AgentID: crash.AgentID, Identity: crash.Identity,
				Epoch: 2, Capacity: 8, ExpiresAt: now.Add(2 * time.Minute),
			}); err != nil {
				t.Fatalf("top up around in-flight attempt: %v", err)
			}
			current = now.Add(2 * time.Second)
			replayed, err := mgr.Reserve(context.Background(), crash)
			if err != nil || !replayed.Existing || replayed.Status != "expired" {
				t.Fatalf("crash replay = %+v, %v, want existing expired", replayed, err)
			}
			crashReceipt := testReceipt("crash", current)
			crashReceipt.GrantID, crashReceipt.OrganizationID, crashReceipt.RuntimeID, crashReceipt.AgentID = crash.GrantID, crash.OrganizationID, crash.RuntimeID, crash.AgentID
			crashReceipt.TenantID, crashReceipt.UserID, crashReceipt.SessionID, crashReceipt.LogicalRunID = q.TenantID, q.UserID, q.SessionID, q.RunID
			crashReceipt.TotalTokens = 2
			crashReceipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(crashReceipt)
			settlement := llm.LeaseSettlement{AttemptID: crash.AttemptID, LogicalCallID: crash.LogicalCallID, AttemptNonce: crash.AttemptNonce, Receipt: crashReceipt, Units: 2, Now: current}
			if err := mgr.Settle(context.Background(), settlement); err != nil {
				t.Fatalf("late provider usage after expiry was lost: %v", err)
			}
			if err := mgr.Settle(context.Background(), settlement); err != nil {
				t.Fatalf("late settlement replay: %v", err)
			}
		})
	}
}

func TestStore_ConcurrentReuse128IndependentLeases(t *testing.T) {
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	now := time.Now().UTC()
	mgr, err := leases.New(st, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	const workers = 128
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%03d", i)
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-" + id, UserID: "user", SessionID: "session"}, RunID: "run"}
			req := llm.LeaseReservationRequest{AttemptID: id, LogicalCallID: id, AttemptNonce: "nonce-" + id, GrantID: "grant-" + id, LeaseID: "lease-" + id, OrganizationID: "org-" + id, RuntimeID: "runtime", AgentID: "agent", Epoch: 1, Capacity: 2, Units: 2, ExpiresAt: now.Add(time.Minute), Identity: q}
			if _, err := mgr.Reserve(context.Background(), req); err != nil {
				errs <- err
				return
			}
			receipt := testReceipt(id, now)
			receipt.GrantID, receipt.OrganizationID, receipt.RuntimeID, receipt.AgentID = req.GrantID, req.OrganizationID, req.RuntimeID, req.AgentID
			receipt.TenantID, receipt.UserID, receipt.SessionID, receipt.LogicalRunID = q.TenantID, q.UserID, q.SessionID, q.RunID
			receipt.TotalTokens = 1
			receipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
			if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: id, LogicalCallID: id, AttemptNonce: req.AttemptNonce, Receipt: receipt, Units: 1, Now: now}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent reserve/settle: %v", err)
	}
}

func testReceipt(id string, now time.Time) llm.AttemptUsageReceipt {
	r := llm.AttemptUsageReceipt{ReceiptID: id, GrantID: "grant", LogicalCallID: id, AttemptNonce: "nonce-" + id, OrganizationID: "org", RuntimeID: "runtime", TenantID: "tenant", UserID: "user", SessionID: "session", LogicalRunID: "run", Provider: "openai", ProviderModelID: "model", ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, RouteID: "route", CredentialAssetGeneration: 1, PolicyGeneration: 1, AttemptNumber: 1, Status: "success", StartedAt: now, CompletedAt: now.Add(time.Millisecond), IdempotencyKey: id, TotalTokens: 7}
	h, _ := llm.CanonicalAttemptUsageReceiptBodyHash(r)
	r.CanonicalBodyHash = h
	return r
}
