package search_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	artifactsubsys "github.com/hurtener/Harbor/internal/artifacts"
	artifactinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	eventinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	artifactsearch "github.com/hurtener/Harbor/internal/search/artifacts"
	eventsearch "github.com/hurtener/Harbor/internal/search/events"
	sessionsearch "github.com/hurtener/Harbor/internal/search/sessions"
	tasksearch "github.com/hurtener/Harbor/internal/search/tasks"
	sessionsubsys "github.com/hurtener/Harbor/internal/sessions"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	tasksubsys "github.com/hurtener/Harbor/internal/tasks"
	taskinprocess "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// twoPrincipalStack is ONE shared instance of every real dependency plus
// ONE shared Searcher per index — the compiled artifacts the D-025
// contract governs. Nothing per-call lives on any of them.
type twoPrincipalStack struct {
	registry *search.SearcherRegistry
	cleanup  func()
}

const (
	axisTenant = "t-shared"
	axisUserA  = "u-alpha"
	axisUserB  = "u-bravo"
)

func newTwoPrincipalStack(t *testing.T) *twoPrincipalStack {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := eventinmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
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
	artStore, err := artifactinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}

	// Two users under ONE tenant, each with two sessions, each session
	// carrying a task and an artifact. A cross-TENANT assertion would pass
	// today against code that leaks across users — which is exactly how
	// this defect survived — so the stress runs inside one tenant.
	deps := search.Deps{Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false }}
	for _, user := range []string{axisUserA, axisUserB} {
		for i := range 2 {
			ident := identity.Identity{
				TenantID:  axisTenant,
				UserID:    user,
				SessionID: fmt.Sprintf("%s-sess-%d", user, i),
			}
			ctx, werr := identity.With(context.Background(), ident)
			if werr != nil {
				t.Fatalf("identity.With: %v", werr)
			}
			if _, oerr := sreg.Open(ctx, ident.SessionID, ident); oerr != nil {
				t.Fatalf("sessions.Open: %v", oerr)
			}
			if _, serr := taskReg.Spawn(ctx, tasksubsys.SpawnRequest{
				Identity:    identity.Quadruple{Identity: ident},
				Kind:        tasksubsys.KindForeground,
				Description: "row for " + user,
			}); serr != nil {
				t.Fatalf("tasks.Spawn: %v", serr)
			}
			if perr := bus.Publish(ctx, eventsubsys.Event{
				Type:     eventsubsys.EventTypeRuntimeError,
				Identity: identity.Quadruple{Identity: ident},
				Payload:  eventsubsys.RedactedMap{Data: map[string]any{"msg": "row for " + user}},
			}); perr != nil {
				t.Fatalf("bus.Publish: %v", perr)
			}
			if _, aerr := artStore.PutText(ctx, artifactsubsys.ArtifactScope{
				TenantID: ident.TenantID, UserID: ident.UserID, SessionID: ident.SessionID,
			}, "content", artifactsubsys.PutOpts{Filename: user + ".txt"}); aerr != nil {
				t.Fatalf("artifacts.PutText: %v", aerr)
			}
		}
	}

	sessionsS, err := sessionsearch.New(sreg, deps)
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	tasksS, err := tasksearch.New(sreg, taskReg, deps)
	if err != nil {
		t.Fatalf("tasksearch.New: %v", err)
	}
	eventsS, err := eventsearch.New(bus.(eventsubsys.Replayer), deps)
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}
	artifactsS, err := artifactsearch.New(artStore, deps)
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}
	reg, err := search.NewRegistry(sessionsS, tasksS, eventsS, artifactsS)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	return &twoPrincipalStack{
		registry: reg,
		cleanup: func() {
			_ = artStore.Close(context.Background())
			_ = taskReg.Close(context.Background())
			_ = sreg.CloseRegistry(context.Background())
			_ = bus.Close(context.Background())
			_ = store.Close(context.Background())
		},
	}
}

// TestSearchers_ConcurrentReuse_TwoPrincipalsOneTenant_NoContextBleed is
// the D-025 witness for the USER axis, and the assertion that would have
// caught this defect when the cluster shipped: N=128 concurrent searches
// split between two users of ONE tenant, against ONE shared Searcher per
// index and one shared registry, asserting every returned row belongs to
// the requesting caller.
func TestSearchers_ConcurrentReuse_TwoPrincipalsOneTenant_NoContextBleed(t *testing.T) {
	const N = 128
	stack := newTwoPrincipalStack(t)
	defer stack.cleanup()

	runtime.GC()
	baseline := runtime.NumGoroutine()

	perIndex := []types.SearchIndex{
		types.SearchIndexSessions,
		types.SearchIndexTasks,
		types.SearchIndexEvents,
		types.SearchIndexArtifacts,
	}

	var wg sync.WaitGroup
	failures := make(chan string, N*4)
	for i := range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := axisUserA
			if i%2 == 1 {
				user = axisUserB
			}
			ident := identity.Identity{
				TenantID:  axisTenant,
				UserID:    user,
				SessionID: fmt.Sprintf("%s-sess-%d", user, i%2),
			}
			ctx, werr := identity.With(context.Background(), ident)
			if werr != nil {
				failures <- fmt.Sprintf("g%d: identity.With: %v", i, werr)
				return
			}

			var rows []types.SearchResultRow
			if i%5 == 0 {
				resp, qerr := search.Query(ctx, stack.registry, ident, denyAdmin, types.SearchRequest{})
				if qerr != nil {
					failures <- fmt.Sprintf("g%d/Query: %v", i, qerr)
					return
				}
				rows = resp.Rows
			} else {
				s, ok := stack.registry.Get(perIndex[i%4])
				if !ok {
					failures <- fmt.Sprintf("g%d: index %s unregistered", i, perIndex[i%4])
					return
				}
				resp, qerr := s.Search(ctx, types.SearchRequest{})
				if qerr != nil {
					failures <- fmt.Sprintf("g%d/%s: %v", i, s.Index(), qerr)
					return
				}
				rows = resp.Rows
			}
			for _, r := range rows {
				if r.UserID != ident.UserID {
					failures <- fmt.Sprintf("g%d: CROSS-USER LEAK row %s/%s user=%s, caller=%s",
						i, r.Index, r.ID, r.UserID, ident.UserID)
				}
				if r.TenantID != ident.TenantID {
					failures <- fmt.Sprintf("g%d: CROSS-TENANT LEAK row %s tenant=%s, caller=%s",
						i, r.ID, r.TenantID, ident.TenantID)
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
		limit := min(len(msgs), 8)
		t.Fatalf("two-principal concurrent-reuse failures (%d), first %d:\n  %v",
			len(msgs), limit, msgs[:limit])
	}

	// Bounded drain-poll (§17.4): re-sample after GC until the count settles
	// within tolerance or a deadline elapses — a genuine leak still fails.
	settleDeadline := time.Now().Add(5 * time.Second)
	var got int
	for {
		runtime.GC()
		got = runtime.NumGoroutine()
		if got <= baseline+5 || !time.Now().Before(settleDeadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}

// TestSearchers_ConcurrentReuse_TwoPrincipalCancellation_NoCrossTalk —
// cancelling user A's ctx must leave user B's rows intact AND still
// scoped to B, against the same shared Searchers.
func TestSearchers_ConcurrentReuse_TwoPrincipalCancellation_NoCrossTalk(t *testing.T) {
	stack := newTwoPrincipalStack(t)
	defer stack.cleanup()

	identA := identity.Identity{TenantID: axisTenant, UserID: axisUserA, SessionID: axisUserA + "-sess-0"}
	identB := identity.Identity{TenantID: axisTenant, UserID: axisUserB, SessionID: axisUserB + "-sess-0"}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxA, err := identity.With(ctxA, identA)
	if err != nil {
		t.Fatalf("identity.With A: %v", err)
	}
	ctxB, err := identity.With(context.Background(), identB)
	if err != nil {
		t.Fatalf("identity.With B: %v", err)
	}

	var wg sync.WaitGroup
	var respB types.SearchResponse
	var errB error

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = search.Query(ctxA, stack.registry, identA, denyAdmin, types.SearchRequest{})
	}()
	cancelA()

	wg.Add(1)
	go func() {
		defer wg.Done()
		respB, errB = search.Query(ctxB, stack.registry, identB, denyAdmin, types.SearchRequest{})
	}()
	wg.Wait()

	if errB != nil {
		t.Fatalf("caller B was affected by A's cancellation: %v", errB)
	}
	if len(respB.Rows) == 0 {
		t.Fatal("caller B got no rows — the cancellation cross-talked or the fold over-reached")
	}
	for _, r := range respB.Rows {
		if r.UserID != axisUserB {
			t.Errorf("CROSS-USER LEAK: B got row %s owned by %s", r.ID, r.UserID)
		}
	}
}
