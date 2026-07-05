// Phase 153 cross-subsystem integration test per CLAUDE.md §17 — the
// admin-widened fleet enumeration for `tasks.list` + `agents.list`,
// exercised end-to-end against real drivers at every seam: the real
// tasks.TaskRegistry (BOTH the inprocess and durable drivers, for
// conformance parity of the widened read), the real Agent Registry, the
// real in-mem StateStore + event bus + patterns redactor, and the real
// tasks/protocol + registry/protocol Services + RegistryProjectors — no
// mocks at any seam.
//
// It discharges the §6 rule 10 cross-session isolation proof:
//
//   - Seeds tasks + agents across 2 tenants × 2 users × 2 sessions.
//   - A non-admin caller sees exactly its own triple's rows.
//   - An admin widened to tenant A never receives tenant B rows, and
//     every widened row carries correct per-(tenant, user, session)
//     attribution.
//   - The admin-widened call emits `audit.admin_scope_used` on the real
//     bus; the non-admin widened call fails LOUD (ErrScopeMismatch).
//   - A closed-registry read is the second forced failure mode.
//   - N≥10 concurrent mixed admin/non-admin listers run under `-race`
//     with no cross-tenant bleed and no goroutine growth after teardown.
package integration_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/durable"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

// fleetMatrix is the 2 tenants × 2 users × 2 sessions identity grid the
// isolation proof seeds.
func fleetMatrix() []identity.Identity {
	var out []identity.Identity
	for _, tn := range []string{"tenant-A", "tenant-B"} {
		for _, u := range []string{"u1", "u2"} {
			for _, s := range []string{"s1", "s2"} {
				out = append(out, identity.Identity{TenantID: tn, UserID: u, SessionID: s})
			}
		}
	}
	return out
}

func fleetStack(t *testing.T) (state.StateStore, events.EventBus, audit.Redactor) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         2048,
	}, red)
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	})
	return store, bus, red
}

// TestFleetEnumeration_Tasks_Isolation exercises the widened `tasks.list`
// read across BOTH task drivers with the full isolation matrix.
func TestFleetEnumeration_Tasks_Isolation(t *testing.T) {
	for _, driver := range []string{"inprocess", "durable"} {
		t.Run(driver, func(t *testing.T) {
			store, bus, red := fleetStack(t)
			reg, err := tasks.OpenDriver(driver, tasks.Dependencies{
				Store: store, Bus: bus, Redactor: red, Cfg: config.TasksConfig{Driver: driver},
			})
			if err != nil {
				t.Fatalf("OpenDriver(%s): %v", driver, err)
			}
			t.Cleanup(func() { _ = reg.Close(context.Background()) })

			// Seed one task per identity in the 2×2×2 matrix.
			for _, id := range fleetMatrix() {
				ctx, cerr := identity.With(context.Background(), id)
				if cerr != nil {
					t.Fatalf("identity.With: %v", cerr)
				}
				if _, serr := reg.Spawn(ctx, tasks.SpawnRequest{
					Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground,
					Description: "fleet task", Query: "q",
				}); serr != nil {
					t.Fatalf("Spawn(%v): %v", id, serr)
				}
			}

			proj, err := tasksprotocol.NewRegistryProjector(reg)
			if err != nil {
				t.Fatalf("NewRegistryProjector: %v", err)
			}
			svc, err := tasksprotocol.NewService(proj, tasksprotocol.WithBus(bus), tasksprotocol.WithRedactor(red))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}

			// (a) Non-admin caller sees exactly its own triple's rows.
			own := identity.Identity{TenantID: "tenant-A", UserID: "u1", SessionID: "s1"}
			ownCtx, _ := identity.With(context.Background(), own)
			narrow, err := svc.List(ownCtx, prototypes.TaskListRequest{
				Identity: prototypes.IdentityScope{Tenant: own.TenantID, User: own.UserID, Session: own.SessionID},
			}, false)
			if err != nil {
				t.Fatalf("narrow List: %v", err)
			}
			if len(narrow.Rows) != 1 || narrow.Rows[0].Identity.Session != "s1" {
				t.Fatalf("narrow rows=%+v, want exactly own (tenant-A/u1/s1)", narrow.Rows)
			}

			// (b) Non-admin widened fails LOUD.
			widenReq := prototypes.TaskListRequest{
				Identity: prototypes.IdentityScope{Tenant: "tenant-A", User: "obs", Session: "obs"},
				Filter:   prototypes.TaskFilter{TenantIDs: []string{"tenant-A"}},
			}
			if _, err := svc.List(context.Background(), widenReq, false); !errors.Is(err, tasksprotocol.ErrScopeMismatch) {
				t.Fatalf("non-admin widened err=%v, want ErrScopeMismatch", err)
			}

			// (c) Admin widened to tenant A returns exactly tenant-A's 4 rows,
			//     never tenant B, with correct attribution + audit event.
			sub, err := bus.Subscribe(context.Background(), events.Filter{
				Tenant: "tenant-A", User: "obs", Session: "obs", Admin: true,
				Types: []events.EventType{events.EventTypeAdminScopeUsed},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			resp, err := svc.List(context.Background(), widenReq, true)
			if err != nil {
				t.Fatalf("admin widened List: %v", err)
			}
			if len(resp.Rows) != 4 {
				t.Fatalf("admin widened count=%d, want 4 (tenant-A: 2 users × 2 sessions)", len(resp.Rows))
			}
			seen := map[string]bool{}
			for _, row := range resp.Rows {
				if row.Identity.Tenant != "tenant-A" {
					t.Errorf("widened read leaked tenant %q", row.Identity.Tenant)
				}
				seen[row.Identity.User+"/"+row.Identity.Session] = true
			}
			for _, want := range []string{"u1/s1", "u1/s2", "u2/s1", "u2/s2"} {
				if !seen[want] {
					t.Errorf("widened read missing tenant-A row %q", want)
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

			// (d) Concurrency stress: N≥10 mixed admin/non-admin listers.
			fleetConcurrencyStress(t, func(admin bool) (int, string, error) {
				if admin {
					r, e := svc.List(context.Background(), widenReq, true)
					if e != nil {
						return 0, "", e
					}
					for _, row := range r.Rows {
						if row.Identity.Tenant != "tenant-A" {
							return 0, row.Identity.Tenant, nil
						}
					}
					return len(r.Rows), "", nil
				}
				r, e := svc.List(ownCtx, prototypes.TaskListRequest{
					Identity: prototypes.IdentityScope{Tenant: own.TenantID, User: own.UserID, Session: own.SessionID},
				}, false)
				if e != nil {
					return 0, "", e
				}
				for _, row := range r.Rows {
					if row.Identity.Session != "s1" {
						return 0, row.Identity.Session, nil
					}
				}
				return len(r.Rows), "", nil
			})

			// (e) Second forced failure mode: a read against a closed
			//     registry surfaces loudly through the widened path.
			_ = reg.Close(context.Background())
			if _, err := reg.ListTenant(context.Background(), "tenant-A", tasks.TaskFilter{}); !errors.Is(err, tasks.ErrRegistryClosed) {
				t.Errorf("closed-registry ListTenant err=%v, want ErrRegistryClosed", err)
			}
		})
	}
}

// TestFleetEnumeration_Agents_Isolation exercises the widened
// `agents.list` read across the full isolation matrix.
func TestFleetEnumeration_Agents_Isolation(t *testing.T) {
	store, bus, red := fleetStack(t)
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	cfg := registry.AgentConfig{Prompts: []string{"p"}, PlannerConfig: map[string]string{"type": "react"}}
	for i, id := range fleetMatrix() {
		ctx, cerr := identity.With(context.Background(), id)
		if cerr != nil {
			t.Fatalf("identity.With: %v", cerr)
		}
		if _, rerr := reg.Register(ctx, "agent-key", cfg, registry.RegisterOptions{}); rerr != nil {
			t.Fatalf("Register(%d): %v", i, rerr)
		}
	}

	proj, err := agentsprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := agentsprotocol.NewService(proj, agentsprotocol.WithBus(bus), agentsprotocol.WithRedactor(red))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// (a) Non-admin caller sees exactly its own triple's agent.
	own := identity.Identity{TenantID: "tenant-A", UserID: "u1", SessionID: "s1"}
	ownCtx, _ := identity.With(context.Background(), own)
	narrow, err := svc.List(ownCtx, prototypes.AgentListRequest{
		Identity: prototypes.IdentityScope{Tenant: own.TenantID, User: own.UserID, Session: own.SessionID},
	}, false)
	if err != nil {
		t.Fatalf("narrow List: %v", err)
	}
	if len(narrow.Agents) != 1 || narrow.Agents[0].Identity.Session != "s1" {
		t.Fatalf("narrow agents=%+v, want exactly own", narrow.Agents)
	}

	// (b) Non-admin widened fails LOUD.
	widenReq := prototypes.AgentListRequest{
		Identity: prototypes.IdentityScope{Tenant: "tenant-A", User: "obs", Session: "obs"},
		Filter:   prototypes.AgentFilter{TenantIDs: []string{"tenant-A"}},
	}
	if _, err := svc.List(context.Background(), widenReq, false); !errors.Is(err, agentsprotocol.ErrScopeMismatch) {
		t.Fatalf("non-admin widened err=%v, want ErrScopeMismatch", err)
	}

	// (c) Admin widened returns exactly tenant-A's 4 agents + audit event.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "tenant-A", User: "obs", Session: "obs", Admin: true,
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	resp, err := svc.List(context.Background(), widenReq, true)
	if err != nil {
		t.Fatalf("admin widened List: %v", err)
	}
	if len(resp.Agents) != 4 {
		t.Fatalf("admin widened count=%d, want 4", len(resp.Agents))
	}
	for _, a := range resp.Agents {
		if a.Identity.Tenant != "tenant-A" {
			t.Errorf("widened read leaked tenant %q", a.Identity.Tenant)
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

	// (d) Concurrency stress.
	fleetConcurrencyStress(t, func(admin bool) (int, string, error) {
		if admin {
			r, e := svc.List(context.Background(), widenReq, true)
			if e != nil {
				return 0, "", e
			}
			for _, a := range r.Agents {
				if a.Identity.Tenant != "tenant-A" {
					return 0, a.Identity.Tenant, nil
				}
			}
			return len(r.Agents), "", nil
		}
		r, e := svc.List(ownCtx, prototypes.AgentListRequest{
			Identity: prototypes.IdentityScope{Tenant: own.TenantID, User: own.UserID, Session: own.SessionID},
		}, false)
		if e != nil {
			return 0, "", e
		}
		return len(r.Agents), "", nil
	})

	// (e) Second forced failure mode: a closed-registry fleet read fails loud.
	_ = reg.Close(context.Background())
	if _, err := reg.ListTenant(context.Background(), "tenant-A"); !errors.Is(err, registry.ErrRegistryClosed) {
		t.Errorf("closed-registry ListTenant err=%v, want ErrRegistryClosed", err)
	}
}

// fleetConcurrencyStress runs N≥10 concurrent mixed admin/non-admin
// listers against a single shared Service. lister returns (rowCount,
// leakedScope, err): a non-empty leakedScope means a foreign
// tenant/session bled into the result. It asserts no error, no bleed, and
// no goroutine growth after the run.
func fleetConcurrencyStress(t *testing.T, lister func(admin bool) (int, string, error)) {
	t.Helper()
	const n = 32
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	leakCh := make(chan string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, leaked, err := lister(i%2 == 0)
			if err != nil {
				errCh <- err
				return
			}
			if leaked != "" {
				leakCh <- leaked
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	close(leakCh)
	for err := range errCh {
		t.Fatalf("concurrent lister: %v", err)
	}
	for leaked := range leakCh {
		t.Fatalf("concurrent lister leaked foreign scope %q", leaked)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
