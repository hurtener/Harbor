package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	tasksearch "github.com/hurtener/Harbor/internal/search/tasks"
	sessionsubsys "github.com/hurtener/Harbor/internal/sessions"
	statesubsys "github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	tasksubsys "github.com/hurtener/Harbor/internal/tasks"
	taskinprocess "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

type harness struct {
	store    statesubsys.StateStore
	bus      eventsubsys.EventBus
	sessions *sessionsubsys.Registry
	tasks    tasksubsys.TaskRegistry
	cleanup  func()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	sreg, err := sessionsubsys.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	taskReg, err := taskinprocess.New(tasksubsys.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: patterns.New(),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.New: %v", err)
	}
	return &harness{
		store:    store,
		bus:      bus,
		sessions: sreg,
		tasks:    taskReg,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = sreg.CloseRegistry(context.Background())
			_ = bus.Close(context.Background())
			_ = store.Close(context.Background())
		},
	}
}

func openSession(t *testing.T, h *harness, ident identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), ident)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := h.sessions.Open(ctx, ident.SessionID, ident); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

func spawnTask(t *testing.T, h *harness, ident identity.Identity, desc string) tasksubsys.TaskID {
	t.Helper()
	q := identity.Quadruple{Identity: ident}
	handle, err := h.tasks.Spawn(context.Background(), tasksubsys.SpawnRequest{
		Identity:    q,
		Kind:        tasksubsys.KindForeground,
		Description: desc,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return handle.ID
}

func callerCtx(t *testing.T, ident identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), ident)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestTasksSearcher_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	s, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("tasksearch.New: %v", err)
	}
	_, err = s.Search(context.Background(), types.SearchRequest{})
	if !errors.Is(err, search.ErrIdentityRequired) {
		t.Fatalf("got %v, want ErrIdentityRequired", err)
	}
}

func TestTasksSearcher_CrossTenantIsolation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	// Two tenants, one task each.
	for _, tenant := range []string{"t1", "t2"} {
		ident := identity.Identity{TenantID: tenant, UserID: "u", SessionID: "s-" + tenant}
		openSession(t, h, ident)
		spawnTask(t, h, ident, "task for "+tenant)
	}

	s, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("tasksearch.New: %v", err)
	}

	// Caller in t1: should only see t1's task.
	ctx := callerCtx(t, identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s-t1"})
	resp, err := s.Search(ctx, types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range resp.Rows {
		if r.TenantID != "t1" {
			t.Errorf("CROSS-TENANT LEAK: row tenant=%s, caller t1", r.TenantID)
		}
	}
	if len(resp.Rows) == 0 {
		t.Errorf("expected at least one task row for t1")
	}
}

func TestTasksSearcher_QueryAndFacets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	ident := identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s1"}
	openSession(t, h, ident)
	id1 := spawnTask(t, h, ident, "deploy production")
	_ = spawnTask(t, h, ident, "send email")

	// Force id1 to RUNNING for facet test.
	identCtx, _ := identity.With(context.Background(), ident)
	if err := h.tasks.MarkRunning(identCtx, id1); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	s, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("tasksearch.New: %v", err)
	}
	ctx := callerCtx(t, ident)

	// Query: "deploy" should match the first task only.
	resp, err := s.Search(ctx, types.SearchRequest{Query: "deploy"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].ID != string(id1) {
		t.Errorf("Query 'deploy' rows: got %v, want [%s]", resp.Rows, id1)
	}

	// Facet: tasks.status=running.
	resp, err = s.Search(ctx, types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "tasks.status", Value: "running"}},
	})
	if err != nil {
		t.Fatalf("Search status running: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Errorf("Status running rows: got %d, want 1", len(resp.Rows))
	}
	if resp.Rows[0].Facets["status"] != "running" {
		t.Errorf("Facets[status]: got %q, want running", resp.Rows[0].Facets["status"])
	}
}

func TestTasksSearcher_Concurrent_NoCrossTalk(t *testing.T) {
	const N = 100
	h := newHarness(t)
	defer h.cleanup()

	for i := range 10 {
		ident := identity.Identity{TenantID: fmt.Sprintf("t-%d", i), UserID: "u", SessionID: fmt.Sprintf("s-%d", i)}
		openSession(t, h, ident)
		spawnTask(t, h, ident, fmt.Sprintf("task-%d", i))
	}

	s, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("tasksearch.New: %v", err)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	failures := make(chan string, N)
	for i := range N {

		wg.Add(1)
		go func() {
			defer wg.Done()
			tIdx := i % 10
			ident := identity.Identity{TenantID: fmt.Sprintf("t-%d", tIdx), UserID: "u", SessionID: fmt.Sprintf("s-%d", tIdx)}
			ctx, _ := identity.With(context.Background(), ident)
			resp, qerr := s.Search(ctx, types.SearchRequest{})
			if qerr != nil {
				failures <- fmt.Sprintf("g%d: %v", i, qerr)
				return
			}
			for _, r := range resp.Rows {
				if r.TenantID != ident.TenantID {
					failures <- fmt.Sprintf("g%d: LEAK tenant=%s caller=%s", i, r.TenantID, ident.TenantID)
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	var msgs []string
	for f := range failures {
		msgs = append(msgs, f)
	}
	if len(msgs) > 0 {
		t.Fatalf("concurrent-reuse failures: %v", msgs)
	}
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}
