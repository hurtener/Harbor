// Cross-subsystem integration test (CLAUDE.md §17.1) for the
// projection-completeness band (Phase 177 / D-313). It proves — with REAL
// drivers at every seam, no mocks at the boundary — that:
//
//   - tasks.list `has_pending_approval` is populated from the REAL pause
//     coordinator through the serve ApprovalChecker wired the way mux.go
//     wires it, so `filter.has_pending_approval=true` narrows to real gated
//     tasks instead of returning an empty page (the sharp false-absence
//     this band closes);
//   - the population is identity-scoped: session A's open approval gate
//     never bleeds onto session B's row (cross-session isolation);
//   - the NEVER-WIRED variant is real: a projector assembled WITHOUT the
//     ApprovalChecker (a forgotten WithApprovalChecker in mux.go) ships a
//     false absence — the exact Half-B bug the gate catches by construction;
//   - a failure mode: memory.list `filter.agent_ids` over the unpopulated
//     producer identity LOUD-REJECTS rather than returning a false-empty
//     page.
//
// Runs under `-race`; includes an N≥10 concurrency stress over the shared
// projector.
package integration_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

func projCompTaskReg(t *testing.T) (tasks.TaskRegistry, func()) {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 64, SubscriberBufferSize: 512, IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 512}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	reg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	cleanup := func() {
		_ = reg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
	}
	return reg, cleanup
}

func projCompSpawnRunning(t *testing.T, reg tasks.TaskRegistry, id identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: id},
		Kind:        tasks.KindBackground,
		Description: "projection-completeness task",
		Query:       "q",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
}

func TestE2E_ProjectionCompleteness_TasksApprovalTruthfulAndIsolated(t *testing.T) {
	reg, cleanup := projCompTaskReg(t)
	defer cleanup()

	idA := identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-A"}
	idB := identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-B"}
	projCompSpawnRunning(t, reg, idA)
	projCompSpawnRunning(t, reg, idB)

	// Open a REAL approval gate on session A via the pause coordinator.
	coord := pauseresume.New()
	ctxA, err := identity.With(context.Background(), idA)
	if err != nil {
		t.Fatalf("identity.With(A): %v", err)
	}
	if _, err := coord.Request(ctxA, pauseresume.PauseRequest{
		Identity: idA,
		Reason:   pauseresume.ReasonApprovalRequired,
	}); err != nil {
		t.Fatalf("coord.Request: %v", err)
	}

	// Build the tasks projector the way mux.go does — WITH the serve
	// ApprovalChecker over the real coordinator.
	checker := serve.NewApprovalChecker(coord)
	if checker == nil {
		t.Fatal("NewApprovalChecker returned nil for a non-nil coordinator")
	}
	projector, err := tasksprotocol.NewRegistryProjector(reg, tasksprotocol.WithApprovalChecker(checker))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := tasksprotocol.NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Session A: the task reports has_pending_approval=true, and the facet
	// narrows to it.
	wantTrue := true
	respA, err := svc.List(context.Background(), prototypes.TaskListRequest{
		Identity: prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID},
	}, false)
	if err != nil {
		t.Fatalf("List(A): %v", err)
	}
	if len(respA.Rows) != 1 || !respA.Rows[0].HasPendingApproval {
		t.Fatalf("session A: want 1 row with has_pending_approval=true, got %+v", respA.Rows)
	}
	respAFacet, err := svc.List(context.Background(), prototypes.TaskListRequest{
		Identity: prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID},
		Filter:   prototypes.TaskFilter{HasPendingApproval: &wantTrue},
	}, false)
	if err != nil {
		t.Fatalf("List(A, facet): %v", err)
	}
	if len(respAFacet.Rows) != 1 {
		t.Fatalf("has_pending_approval=true facet on A: want 1 row, got %d", len(respAFacet.Rows))
	}

	// Session B: no gate → false, and the facet excludes it (isolation —
	// A's gate never bleeds into B).
	respB, err := svc.List(context.Background(), prototypes.TaskListRequest{
		Identity: prototypes.IdentityScope{Tenant: idB.TenantID, User: idB.UserID, Session: idB.SessionID},
	}, false)
	if err != nil {
		t.Fatalf("List(B): %v", err)
	}
	if len(respB.Rows) != 1 || respB.Rows[0].HasPendingApproval {
		t.Fatalf("session B: want 1 row with has_pending_approval=false, got %+v", respB.Rows)
	}
	respBFacet, err := svc.List(context.Background(), prototypes.TaskListRequest{
		Identity: prototypes.IdentityScope{Tenant: idB.TenantID, User: idB.UserID, Session: idB.SessionID},
		Filter:   prototypes.TaskFilter{HasPendingApproval: &wantTrue},
	}, false)
	if err != nil {
		t.Fatalf("List(B, facet): %v", err)
	}
	if len(respBFacet.Rows) != 0 {
		t.Fatalf("has_pending_approval=true facet on B: want 0 rows (isolation), got %d", len(respBFacet.Rows))
	}

	// Half-B never-wired variant: a projector assembled WITHOUT the checker
	// (a forgotten WithApprovalChecker) ships false absence — the gate
	// catches this by construction.
	unwiredProj, err := tasksprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector(unwired): %v", err)
	}
	unwiredSvc, err := tasksprotocol.NewService(unwiredProj)
	if err != nil {
		t.Fatalf("NewService(unwired): %v", err)
	}
	respUnwired, err := unwiredSvc.List(context.Background(), prototypes.TaskListRequest{
		Identity: prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID},
	}, false)
	if err != nil {
		t.Fatalf("List(unwired): %v", err)
	}
	if len(respUnwired.Rows) != 1 || respUnwired.Rows[0].HasPendingApproval {
		t.Fatalf("unwired projector should ship false absence, got %+v", respUnwired.Rows)
	}

	// Concurrency stress: N≥10 concurrent lists against the shared projector.
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := svc.List(context.Background(), prototypes.TaskListRequest{
				Identity: prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID},
			}, false)
			if e != nil {
				errs <- e
				return
			}
			if len(r.Rows) != 1 || !r.Rows[0].HasPendingApproval {
				errs <- errors.New("concurrent list saw inconsistent has_pending_approval")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent list: %v", e)
	}
}

func TestE2E_ProjectionCompleteness_MemoryAgentFacetLoudRejects(t *testing.T) {
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 64, SubscriberBufferSize: 512, IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 512}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	stateStore, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = stateStore.Close(context.Background()) }()
	store, err := memoryinmem.New(memory.ConfigSnapshot{Strategy: memory.StrategyTruncation},
		memory.Deps{State: stateStore, Bus: bus}, memoryinmem.Options{})
	if err != nil {
		t.Fatalf("memoryinmem.New: %v", err)
	}
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if err := store.AddTurn(context.Background(), id, memory.ConversationTurn{
		UserMessage: "hello", AssistantResponse: "hi",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	// Failure mode: agent_ids over the unpopulated producer identity loud-rejects.
	_, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{AgentIDs: []string{"agent-x"}}}, id)
	if !errors.Is(err, memprotocol.ErrInvalidFilter) {
		t.Fatalf("memory agent_ids facet: err = %v, want ErrInvalidFilter (loud-reject, never a false-empty page)", err)
	}
}

// TestE2E_ProjectionCompleteness_NoGoroutineLeak asserts the tasks projection
// path returns goroutines to baseline after use.
func TestE2E_ProjectionCompleteness_NoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	reg, cleanup := projCompTaskReg(t)
	idA := identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-A"}
	projCompSpawnRunning(t, reg, idA)
	proj, err := tasksprotocol.NewRegistryProjector(reg, tasksprotocol.WithApprovalChecker(serve.NewApprovalChecker(pauseresume.New())))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := tasksprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	for range 50 {
		if _, err := svc.List(context.Background(), prototypes.TaskListRequest{
			Identity: prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID},
		}, false); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	cleanup()
	// Allow the registry's teardown goroutines to unwind.
	for i := 0; i < 50 && runtime.NumGoroutine() > base+5; i++ {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > base+5 {
		t.Errorf("goroutine leak: baseline %d, after %d", base, got)
	}
}
