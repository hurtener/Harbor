package protocol_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

// TestList_FleetWidening_RequiresAdmin — a `tasks.list` naming tenants in
// Filter.TenantIDs is the admin-widened fleet enumeration: without the
// verified admin claim it fails LOUD with ErrScopeMismatch (never a silent
// narrowing to own scope); with the claim it enumerates every task across
// all sessions of the named tenants + emits the admin-scope audit event.
func TestList_FleetWidening_RequiresAdmin(t *testing.T) {
	svc, reg, bus := newListService(t)

	// Tenant A: two sessions (u1/s1, u2/s2), one task each.
	a1 := idFor("tenant-A", "u1", "s1")
	a2 := idFor("tenant-A", "u2", "s2")
	seedTask(t, reg, a1, tasks.KindForeground, tasks.StatusRunning, "a1 task", "q")
	seedTask(t, reg, a2, tasks.KindBackground, tasks.StatusPending, "a2 task", "q")
	// Tenant B: one task that must NEVER appear in tenant-A's fleet read.
	b1 := idFor("tenant-B", "u9", "s9")
	seedTask(t, reg, b1, tasks.KindForeground, tasks.StatusRunning, "b task", "q")

	// The caller is a synthetic observer session — its own triple has no
	// tasks, proving the widening (not the caller's own scope) is what
	// returns rows.
	observer := scopeOf("tenant-A", "observer", "obs-sess")
	req := prototypes.TaskListRequest{
		Identity: observer,
		Filter:   prototypes.TaskFilter{TenantIDs: []string{"tenant-A"}},
	}

	t.Run("non-admin widened fails closed", func(t *testing.T) {
		_, err := svc.List(context.Background(), req, false)
		if !errors.Is(err, tasksprotocol.ErrScopeMismatch) {
			t.Fatalf("want ErrScopeMismatch, got %v", err)
		}
	})

	t.Run("admin widened enumerates the tenant + emits audit", func(t *testing.T) {
		ctx := context.Background()
		sub, err := bus.Subscribe(ctx, events.Filter{
			Tenant: "tenant-A", User: "observer", Session: "obs-sess", Admin: true,
			Types: []events.EventType{events.EventTypeAdminScopeUsed},
		})
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Cancel()

		resp, err := svc.List(ctx, req, true)
		if err != nil {
			t.Fatalf("admin widened List: %v", err)
		}
		if len(resp.Rows) != 2 {
			t.Fatalf("widened row count=%d, want 2 (both tenant-A sessions)", len(resp.Rows))
		}
		for _, row := range resp.Rows {
			if row.Identity.Tenant != "tenant-A" {
				t.Errorf("widened row leaked tenant %q", row.Identity.Tenant)
			}
			// Per-row identity attribution present.
			if row.Identity.User == "" || row.Identity.Session == "" {
				t.Errorf("widened row missing identity attribution: %+v", row.Identity)
			}
		}
		select {
		case ev := <-sub.Events():
			if ev.Type != events.EventTypeAdminScopeUsed {
				t.Fatalf("want admin_scope_used, got %q", ev.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no audit.admin_scope_used event observed within 2s")
		}
	})
}

// TestList_NonWidened_ByteCompatible — a `tasks.list` with no TenantIDs is
// the identity-scoped read: it returns only the caller's own session's
// tasks even under adminScoped, and emits no audit event. This locks the
// "non-widened requests are byte-compatible with today" acceptance criterion.
func TestList_NonWidened_ByteCompatible(t *testing.T) {
	svc, reg, _ := newListService(t)
	own := idFor("t1", "u1", "s1")
	other := idFor("t1", "u2", "s2")
	h := seedTask(t, reg, own, tasks.KindForeground, tasks.StatusRunning, "own", "q")
	seedTask(t, reg, other, tasks.KindForeground, tasks.StatusRunning, "other-session", "q")

	// Even with adminScoped=true, a non-widened request stays own-scope.
	resp, err := svc.List(context.Background(), prototypes.TaskListRequest{Identity: scopeOf("t1", "u1", "s1")}, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].ID != string(h) {
		t.Fatalf("non-widened rows=%+v, want exactly the caller's own task %q", resp.Rows, h)
	}
}

// TestList_FleetWidening_MultiTenant — a widened read naming two tenants
// enumerates both; a foreign tenant not named is never returned.
func TestList_FleetWidening_MultiTenant(t *testing.T) {
	svc, reg, _ := newListService(t)
	seedTask(t, reg, idFor("tenant-A", "u1", "s1"), tasks.KindForeground, tasks.StatusRunning, "a", "q")
	seedTask(t, reg, idFor("tenant-B", "u1", "s1"), tasks.KindForeground, tasks.StatusRunning, "b", "q")
	seedTask(t, reg, idFor("tenant-C", "u1", "s1"), tasks.KindForeground, tasks.StatusRunning, "c", "q")

	resp, err := svc.List(context.Background(), prototypes.TaskListRequest{
		Identity: scopeOf("tenant-A", "obs", "obs"),
		Filter:   prototypes.TaskFilter{TenantIDs: []string{"tenant-A", "tenant-B"}},
	}, true)
	if err != nil {
		t.Fatalf("multi-tenant widened List: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("row count=%d, want 2 (A+B, never C)", len(resp.Rows))
	}
	for _, row := range resp.Rows {
		if row.Identity.Tenant == "tenant-C" {
			t.Errorf("un-named tenant-C leaked into widened read")
		}
	}
}

// TestListTenantTasks_ConcurrentReuse_D025 exercises the D-025
// concurrent-reuse contract on the aggregating fleet projector: N=128
// concurrent goroutines mixing admin-widened fleet reads and narrow
// identity-scoped reads against a SINGLE shared tasks/protocol.Service
// under `-race`. It asserts no data races, no context bleed (each widened
// read only sees the tenant it named; each narrow read only its own
// session), and no goroutine leak after the calls return.
func TestListTenantTasks_ConcurrentReuse_D025(t *testing.T) {
	svc, reg, _ := newListService(t)

	// Seed two tenants, each with a couple of sessions.
	seedTask(t, reg, idFor("tenant-A", "u1", "s1"), tasks.KindForeground, tasks.StatusRunning, "a1", "q")
	seedTask(t, reg, idFor("tenant-A", "u2", "s2"), tasks.KindForeground, tasks.StatusRunning, "a2", "q")
	seedTask(t, reg, idFor("tenant-B", "u1", "s1"), tasks.KindForeground, tasks.StatusRunning, "b1", "q")
	ownID := idFor("tenant-A", "u1", "s1")

	const n = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%2 == 0 {
				// Admin-widened fleet read of tenant-A.
				resp, err := svc.List(ctx, prototypes.TaskListRequest{
					Identity: scopeOf("tenant-A", "obs", "obs"),
					Filter:   prototypes.TaskFilter{TenantIDs: []string{"tenant-A"}},
				}, true)
				if err != nil {
					errCh <- err
					return
				}
				for _, row := range resp.Rows {
					if row.Identity.Tenant != "tenant-A" {
						errCh <- errors.New("widened read leaked a foreign tenant")
						return
					}
				}
			} else {
				// Narrow identity-scoped read of the caller's own session.
				resp, err := svc.List(ctx, prototypes.TaskListRequest{
					Identity: scopeOf(ownID.TenantID, ownID.UserID, ownID.SessionID),
				}, false)
				if err != nil {
					errCh <- err
					return
				}
				for _, row := range resp.Rows {
					if row.Identity.Session != ownID.SessionID {
						errCh <- errors.New("narrow read leaked a foreign session")
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent fleet/narrow read: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
