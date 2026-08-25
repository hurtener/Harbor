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

func TestStore_ExpirePreservesLateUsageAcrossDrivers(t *testing.T) {
	cases := []struct {
		name string
		open func(*testing.T) (*leases.Store, func() error)
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
			st, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "expire-late.sqlite")})
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
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
			expired := llm.LeaseReservationRequest{AttemptID: "expired", LogicalCallID: "expired", AttemptNonce: "nonce-expired", GrantID: "g", LeaseID: "l", OrganizationID: "org", RuntimeID: "runtime", Epoch: 1, Capacity: 4, Units: 4, ExpiresAt: now.Add(-time.Second), Identity: q}
			if _, err := mgr.Reserve(context.Background(), expired); !errors.Is(err, leases.ErrInsufficient) {
				t.Fatalf("expired reserve=%v", err)
			}

			late := expired
			late.AttemptID, late.LogicalCallID, late.AttemptNonce = "will-expire", "will-expire", "nonce-will-expire"
			late.LeaseID, late.ExpiresAt = "late-lease", now.Add(time.Second)
			if _, err := mgr.Reserve(context.Background(), late); err != nil {
				t.Fatal(err)
			}
			if n, err := mgr.Expire(context.Background(), now.Add(2*time.Second), 10); err != nil || n != 1 {
				t.Fatalf("Expire n=%d err=%v", n, err)
			}
			if n, err := mgr.Expire(context.Background(), now.Add(2*time.Second), 10); err != nil || n != 0 {
				t.Fatalf("duplicate Expire n=%d err=%v", n, err)
			}
			replayed, err := mgr.Reserve(context.Background(), late)
			if err != nil || !replayed.Existing || replayed.Status != "expired" {
				t.Fatalf("expired replay=%+v err=%v", replayed, err)
			}
			receipt := testReceipt(late.AttemptID, now.Add(2*time.Second))
			receipt.GrantID, receipt.OrganizationID, receipt.RuntimeID = late.GrantID, late.OrganizationID, late.RuntimeID
			receipt.TenantID, receipt.UserID, receipt.SessionID, receipt.LogicalRunID = q.TenantID, q.UserID, q.SessionID, q.RunID
			receipt.Status, receipt.TotalTokens = "error", 6
			receipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
			settlement := llm.LeaseSettlement{AttemptID: late.AttemptID, LogicalCallID: late.LogicalCallID, AttemptNonce: late.AttemptNonce, Receipt: receipt, Units: 6, Now: now.Add(2 * time.Second)}
			if err := mgr.Settle(context.Background(), settlement); err != nil {
				t.Fatalf("late settlement: %v", err)
			}
			if err := mgr.Settle(context.Background(), settlement); err != nil {
				t.Fatalf("late settlement replay: %v", err)
			}
			pending, err := mgr.PendingReceipts(context.Background(), 10)
			if err != nil || len(pending) != 1 || pending[0].TotalTokens != 6 {
				t.Fatalf("late pending=%+v err=%v", pending, err)
			}

			released := late
			released.AttemptID, released.LogicalCallID, released.AttemptNonce = "released", "released", "nonce-released"
			released.LeaseID, released.ExpiresAt = "released-lease", now.Add(time.Minute)
			if _, err := mgr.Reserve(context.Background(), released); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Release(context.Background(), released.AttemptID); err != nil {
				t.Fatal(err)
			}
			releasedReceipt := testReceipt(released.AttemptID, now)
			releasedReceipt.GrantID, releasedReceipt.OrganizationID, releasedReceipt.RuntimeID = released.GrantID, released.OrganizationID, released.RuntimeID
			releasedReceipt.TenantID, releasedReceipt.UserID, releasedReceipt.SessionID, releasedReceipt.LogicalRunID = q.TenantID, q.UserID, q.SessionID, q.RunID
			releasedReceipt.TotalTokens = 2
			releasedReceipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(releasedReceipt)
			if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: released.AttemptID, LogicalCallID: released.LogicalCallID, AttemptNonce: released.AttemptNonce, Receipt: releasedReceipt, Units: 2, Now: now}); !errors.Is(err, leases.ErrAttemptConflict) {
				t.Fatalf("explicit release settlement=%v, want ErrAttemptConflict", err)
			}
		})
	}
}

func TestStore_ResolveAttemptExpiresInFlightAfterLeaseAdvances(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	mgr, err := leases.New(st, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	reserve := llm.LeaseReservationRequest{
		AttemptID: "attempt", LogicalCallID: "call", AttemptNonce: "nonce", GrantID: "grant", LeaseID: "lease",
		OrganizationID: "org", RuntimeID: "runtime", Epoch: 1, Capacity: 10, Units: 4,
		ExpiresAt: now.Add(time.Second), GrantFingerprint: "current-fingerprint", Identity: q,
	}
	if _, err := mgr.Reserve(context.Background(), reserve); err != nil {
		t.Fatal(err)
	}
	if err := mgr.TopUp(context.Background(), leases.TopUpRequest{
		GrantID: "grant", LeaseID: "lease", OrganizationID: "org", RuntimeID: "runtime", Identity: q,
		Epoch: 2, Capacity: 20, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	lookup := llm.LeaseAttemptLookup{
		AttemptID: "attempt", LogicalCallID: "call", AttemptNonce: "nonce", GrantID: "grant", LeaseID: "lease",
		OrganizationID: "org", RuntimeID: "runtime", Units: 4, CurrentGrantFingerprint: "current-fingerprint", Identity: q,
	}
	inFlight, found, err := mgr.ResolveAttempt(context.Background(), lookup)
	if err != nil || !found || inFlight.Status != "reserved" {
		t.Fatalf("in-flight resolved=%+v found=%v err=%v", inFlight, found, err)
	}
	now = now.Add(2 * time.Second)
	resolved, found, err := mgr.ResolveAttempt(context.Background(), lookup)
	if err != nil || !found || resolved.Status != "expired" {
		t.Fatalf("resolved=%+v found=%v err=%v", resolved, found, err)
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

func TestStore_ApplySuccessorConformanceAndConcurrentReplay(t *testing.T) {
	type closer interface{ Close(context.Context) error }
	cases := []struct {
		name string
		open func(*testing.T) (closer, *leases.Store)
	}{
		{name: "inmem", open: func(t *testing.T) (closer, *leases.Store) {
			st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			return st, mgr
		}},
		{name: "sqlite", open: func(t *testing.T) (closer, *leases.Store) {
			st, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "successor.sqlite")})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			return st, mgr
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, mgr := tc.open(t)
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			predecessor, successor, scope := successorGrantPair(time.Now().UTC())
			predecessorHash, err := llm.CanonicalExternalGrantHash(predecessor)
			if err != nil {
				t.Fatal(err)
			}
			seed := llm.LeaseReservationRequest{
				AttemptID: "seed", LogicalCallID: "seed", AttemptNonce: "nonce-seed",
				GrantID: predecessor.GrantID, LeaseID: predecessor.Lease.LeaseID,
				OrganizationID: predecessor.OrganizationID, RuntimeID: predecessor.RuntimeID, AgentID: predecessor.AgentID,
				Epoch: predecessor.Lease.Epoch, Capacity: predecessor.Lease.RemainingTokens(), Units: 20,
				ExpiresAt: predecessor.Lease.ExpiresAt, Identity: scope, GrantFingerprint: predecessorHash,
			}
			if _, err := mgr.Reserve(context.Background(), seed); err != nil {
				t.Fatal(err)
			}
			settled := seed
			settled.AttemptID, settled.LogicalCallID, settled.AttemptNonce, settled.Units = "settled", "settled", "nonce-settled", 10
			if _, err := mgr.Reserve(context.Background(), settled); err != nil {
				t.Fatal(err)
			}
			receipt := testReceipt(settled.AttemptID, predecessor.IssuedAt)
			receipt.GrantID, receipt.LogicalCallID, receipt.AttemptNonce = settled.GrantID, settled.LogicalCallID, settled.AttemptNonce
			receipt.OrganizationID, receipt.RuntimeID, receipt.AgentID = settled.OrganizationID, settled.RuntimeID, settled.AgentID
			receipt.TenantID, receipt.UserID, receipt.SessionID, receipt.LogicalRunID = scope.TenantID, scope.UserID, scope.SessionID, scope.RunID
			receipt.TotalTokens = 7
			receipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
			if err := mgr.Settle(context.Background(), llm.LeaseSettlement{
				AttemptID: settled.AttemptID, LogicalCallID: settled.LogicalCallID, AttemptNonce: settled.AttemptNonce,
				Receipt: receipt, Units: 7, Now: predecessor.IssuedAt,
			}); err != nil {
				t.Fatal(err)
			}

			const concurrent = 128
			errs := make(chan error, concurrent)
			var wg sync.WaitGroup
			for range concurrent {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- mgr.ApplySuccessor(context.Background(), predecessor, successor)
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("concurrent exact successor: %v", err)
				}
			}
			if err := mgr.ApplySuccessor(context.Background(), predecessor, successor); err != nil {
				t.Fatalf("response-loss replay: %v", err)
			}
			resolved, ok, err := mgr.ResolveSuccessor(context.Background(), predecessor)
			if err != nil || !ok || resolved != successor {
				t.Fatalf("resolved successor=%+v ok=%v err=%v", resolved, ok, err)
			}
			mutatedRoot := predecessor
			mutatedRoot.Signature = "mutated-root-signature"
			if _, _, err := mgr.ResolveSuccessor(context.Background(), mutatedRoot); !errors.Is(err, leases.ErrAttemptConflict) {
				t.Fatalf("mutated root resolve=%v, want conflict", err)
			}

			successorHash, err := llm.CanonicalExternalGrantHash(successor)
			if err != nil {
				t.Fatal(err)
			}
			remaining := seed
			remaining.AttemptID, remaining.LogicalCallID, remaining.AttemptNonce = "remaining", "remaining", "nonce-remaining"
			remaining.Epoch, remaining.Capacity, remaining.Units = successor.Lease.Epoch, successor.Lease.RemainingTokens(), 116
			remaining.ExpiresAt, remaining.GrantFingerprint = successor.Lease.ExpiresAt, successorHash
			if _, err := mgr.Reserve(context.Background(), remaining); err != nil {
				t.Fatalf("preserved reservation/consumption capacity: %v", err)
			}
			over := remaining
			over.AttemptID, over.LogicalCallID, over.AttemptNonce, over.Units = "over", "over", "nonce-over", 1
			if _, err := mgr.Reserve(context.Background(), over); !errors.Is(err, leases.ErrInsufficient) {
				t.Fatalf("capacity after preserved seed/consumption = %v, want ErrInsufficient", err)
			}

			changed := successor
			changed.Signature = "different-successor-signature"
			if err := mgr.ApplySuccessor(context.Background(), predecessor, changed); !errors.Is(err, leases.ErrAttemptConflict) {
				t.Fatalf("different successor at same epoch = %v, want ErrAttemptConflict", err)
			}

			absentPredecessor, absentSuccessor, _ := successorGrantPair(time.Now().UTC().Add(time.Second))
			absentPredecessor.GrantID, absentPredecessor.Lease.LeaseID = "grant-absent", "lease-absent"
			absentSuccessor.GrantID, absentSuccessor.Lease.LeaseID = "grant-absent", "lease-absent"
			if err := mgr.ApplySuccessor(context.Background(), absentPredecessor, absentSuccessor); err != nil {
				t.Fatalf("absent successor create: %v", err)
			}
		})
	}
}

func TestStore_ResolveSuccessorSurvivesSQLiteRestart(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "successor-restart.sqlite")
	open := func() (*leases.Store, interface{ Close(context.Context) error }) {
		st, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		mgr, err := leases.New(st, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return mgr, st
	}
	predecessor, successor, _ := successorGrantPair(time.Now().UTC())
	mgr, st := open()
	if err := mgr.ApplySuccessor(context.Background(), predecessor, successor); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr, st = open()
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	resolved, ok, err := mgr.ResolveSuccessor(context.Background(), predecessor)
	if err != nil || !ok || resolved != successor {
		t.Fatalf("restart resolved=%+v ok=%v err=%v", resolved, ok, err)
	}
}

func TestStore_MixedRequestedUnitSuccessorsChooseOneAndLoserReloads(t *testing.T) {
	type closer interface{ Close(context.Context) error }
	cases := []struct {
		name string
		open func(*testing.T) (closer, *leases.Store)
	}{
		{name: "inmem", open: func(t *testing.T) (closer, *leases.Store) {
			st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			return st, mgr
		}},
		{name: "sqlite", open: func(t *testing.T) (closer, *leases.Store) {
			st, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "mixed-successor.sqlite")})
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := leases.New(st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			return st, mgr
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, mgr := tc.open(t)
			defer func() { _ = st.Close(context.Background()) }()
			root, smaller, _ := successorGrantPair(time.Now().UTC())
			smaller.Lease.TokenUnits = root.Lease.TokenUnits + 10
			larger := smaller
			larger.Lease.TokenUnits = root.Lease.TokenUnits + 20
			larger.Signature = "signature-larger-request"

			start := make(chan struct{})
			errs := make(chan error, 2)
			for _, candidate := range []llm.ExternalGrant{smaller, larger} {
				candidate := candidate
				go func() {
					<-start
					errs <- mgr.ApplySuccessor(context.Background(), root, candidate)
				}()
			}
			close(start)
			var success, conflict int
			for range 2 {
				err := <-errs
				switch {
				case err == nil:
					success++
				case errors.Is(err, leases.ErrAttemptConflict):
					conflict++
				default:
					t.Fatalf("mixed successor apply=%v", err)
				}
			}
			if success != 1 || conflict != 1 {
				t.Fatalf("mixed successor outcomes success=%d conflict=%d", success, conflict)
			}
			resolved, ok, err := mgr.ResolveSuccessor(context.Background(), root)
			if err != nil || !ok || (resolved != smaller && resolved != larger) {
				t.Fatalf("loser reload resolved=%+v ok=%v err=%v", resolved, ok, err)
			}
		})
	}
}

func successorGrantPair(now time.Time) (llm.ExternalGrant, llm.ExternalGrant, identity.Quadruple) {
	scope := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-successor", UserID: "user-successor", SessionID: "session-successor"}, RunID: "run-successor"}
	predecessor := llm.ExternalGrant{
		Version: llm.ExternalGrantVersionAgentBound, KeyID: "key-a", Audience: "harbor-runtime", GrantID: "grant-successor",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, OrganizationID: "org-successor", RuntimeID: "runtime-successor", AgentID: "agent-successor",
		TenantID: scope.TenantID, UserID: scope.UserID, SessionID: scope.SessionID, LogicalRunID: scope.RunID,
		LogicalCallID: "call-successor", AttemptNonce: "nonce-successor", PolicyGeneration: 1,
		MaxReasoning: llm.ReasoningMedium, MaxOutputTokens: 100,
		Lease:    llm.ComputeLease{LeaseID: "lease-successor", Epoch: 1, TokenUnits: 100, ConsumedUnits: 7, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(30 * time.Second), Signature: "signature-a",
	}
	successor := predecessor
	successor.KeyID, successor.Signature = "key-b", "signature-b"
	successor.Lease.Epoch, successor.Lease.TokenUnits = 2, 150
	successor.IssuedAt, successor.ExpiresAt, successor.Lease.ExpiresAt = now.Add(10*time.Second), now.Add(40*time.Second), now.Add(70*time.Second)
	return predecessor, successor, scope
}

func testReceipt(id string, now time.Time) llm.AttemptUsageReceipt {
	r := llm.AttemptUsageReceipt{ReceiptID: id, GrantID: "grant", LogicalCallID: id, AttemptNonce: "nonce-" + id, OrganizationID: "org", RuntimeID: "runtime", TenantID: "tenant", UserID: "user", SessionID: "session", LogicalRunID: "run", Provider: "openai", ProviderModelID: "model", ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, RouteID: "route", CredentialAssetGeneration: 1, PolicyGeneration: 1, AttemptNumber: 1, Status: "success", StartedAt: now, CompletedAt: now.Add(time.Millisecond), IdempotencyKey: id, TotalTokens: 7}
	h, _ := llm.CanonicalAttemptUsageReceiptBodyHash(r)
	r.CanonicalBodyHash = h
	return r
}
