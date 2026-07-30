package integration

// Phase 218 — the search cluster's user axis is a scoped boundary.
//
// Every seam below uses a PRODUCTION driver (state/inmem, events/inmem,
// audit/patterns, the real sessions.Registry, the real TaskRegistry, the
// real ArtifactStore) and the PRODUCTION ScopeChecker, so a claim travels
// from ctx through the Protocol dispatcher into each searcher exactly as
// it does at the wire edge — no bool stand-in at the boundary.
//
// The fixture is two tenants x two users x two sessions each. The
// two-users-in-ONE-tenant half is load-bearing: a cross-tenant-only
// assertion passes against code that leaks across users, which is how this
// defect survived from the cluster's first phase to now.

import (
	"context"
	"errors"
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
	"github.com/hurtener/Harbor/internal/protocol"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	artifactsearch "github.com/hurtener/Harbor/internal/search/artifacts"
	eventsearch "github.com/hurtener/Harbor/internal/search/events"
	sessionsearch "github.com/hurtener/Harbor/internal/search/sessions"
	tasksearch "github.com/hurtener/Harbor/internal/search/tasks"
	"github.com/hurtener/Harbor/internal/server"
	sessionsubsys "github.com/hurtener/Harbor/internal/sessions"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	tasksubsys "github.com/hurtener/Harbor/internal/tasks"
	taskinprocess "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const (
	p218TenantA = "tenant-alpha"
	p218TenantB = "tenant-bravo"
	p218UserA   = "user-one"
	p218UserB   = "user-two"
)

var p218Methods = []methods.Method{
	methods.MethodSearchQuery,
	methods.MethodSearchSessions,
	methods.MethodSearchTasks,
	methods.MethodSearchEvents,
	methods.MethodSearchArtifacts,
}

// userAxisStack wires the whole cluster behind the PRODUCTION
// ScopeChecker, so an admin-tier claim on ctx is the only thing that
// widens a read.
type userAxisStack struct {
	sessions *sessionsubsys.Registry
	surface  *protocol.SearchSurface
	close    func()
}

func newUserAxisStack(t *testing.T) *userAxisStack {
	t.Helper()
	red := patterns.New()
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
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	sreg, err := sessionsubsys.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	taskReg, err := taskinprocess.New(tasksubsys.Dependencies{
		Store: store, Bus: bus, Redactor: red,
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.New: %v", err)
	}
	artStore, err := artifactinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.New: %v", err)
	}

	deps := search.Deps{Redactor: red, AdminScope: server.SearchAdminScopeFromAuth}
	ss, err := sessionsearch.New(sreg, deps)
	if err != nil {
		t.Fatalf("session search: %v", err)
	}
	ts, err := tasksearch.New(sreg, taskReg, deps)
	if err != nil {
		t.Fatalf("task search: %v", err)
	}
	replayer, ok := bus.(eventsubsys.Replayer)
	if !ok {
		t.Fatal("bus does not implement Replayer")
	}
	es, err := eventsearch.New(replayer, deps)
	if err != nil {
		t.Fatalf("event search: %v", err)
	}
	as, err := artifactsearch.New(artStore, deps)
	if err != nil {
		t.Fatalf("artifact search: %v", err)
	}
	reg, err := search.NewRegistry(ss, ts, es, as)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	surf, err := protocol.NewSearchSurface(reg, server.SearchAdminScopeFromAuth)
	if err != nil {
		t.Fatalf("NewSearchSurface: %v", err)
	}

	// Two tenants x two users x two sessions, each session carrying a
	// task, an event and an artifact so every index has rows to leak.
	for _, tenant := range []string{p218TenantA, p218TenantB} {
		for _, user := range []string{p218UserA, p218UserB} {
			for i := range 2 {
				ident := identity.Identity{
					TenantID:  tenant,
					UserID:    user,
					SessionID: fmt.Sprintf("%s-%s-sess-%d", tenant, user, i),
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
					Description: "row owned by " + user,
				}); serr != nil {
					t.Fatalf("tasks.Spawn: %v", serr)
				}
				if perr := bus.Publish(ctx, eventsubsys.Event{
					Type:     eventsubsys.EventTypeRuntimeError,
					Identity: identity.Quadruple{Identity: ident},
					Payload:  eventsubsys.RedactedMap{Data: map[string]any{"msg": "row owned by " + user}},
				}); perr != nil {
					t.Fatalf("bus.Publish: %v", perr)
				}
				if _, aerr := artStore.PutText(ctx, artifactsubsys.ArtifactScope{
					TenantID: tenant, UserID: user, SessionID: ident.SessionID,
				}, "content", artifactsubsys.PutOpts{
					Filename: user + "-private.txt", MimeType: "text/plain",
				}); aerr != nil {
					t.Fatalf("artifacts.PutText: %v", aerr)
				}
			}
		}
	}

	return &userAxisStack{
		sessions: sreg,
		surface:  surf,
		close: func() {
			_ = taskReg.Close(context.Background())
			_ = sreg.CloseRegistry(context.Background())
			_ = artStore.Close(context.Background())
			_ = bus.Close(context.Background())
			_ = store.Close(context.Background())
		},
	}
}

// p218Ctx builds a request ctx with a VERIFIED identity (so the tasks
// searcher's per-row re-scope has an anchor to reconcile against) plus the
// given verified scope claims.
func p218Ctx(t *testing.T, tenant, user string, scopes ...protocolauth.Scope) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID:  tenant,
		UserID:    user,
		SessionID: fmt.Sprintf("%s-%s-sess-0", tenant, user),
	})
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	if len(scopes) > 0 {
		ctx = protocolauth.WithScopes(ctx, scopes)
	}
	return ctx
}

// TestE2E_Phase218_UnwidenedReadNeverLeavesTheCallersOwnPrincipal is the
// isolation assertion: across all five methods, no row belongs to another
// USER of the caller's own tenant and no row belongs to another TENANT.
func TestE2E_Phase218_UnwidenedReadNeverLeavesTheCallersOwnPrincipal(t *testing.T) {
	t.Parallel()
	st := newUserAxisStack(t)
	defer st.close()

	ctx := p218Ctx(t, p218TenantA, p218UserA)
	for _, m := range p218Methods {
		t.Run(string(m), func(t *testing.T) {
			resp, err := st.surface.Dispatch(ctx, m, &types.SearchRequest{})
			if err != nil {
				t.Fatalf("Dispatch(%s): %v", m, err)
			}
			if len(resp.Rows) == 0 {
				t.Fatalf("%s returned nothing for an own-scope read — a fix that empties a working surface is not a fix", m)
			}
			for _, r := range resp.Rows {
				if r.TenantID != p218TenantA {
					t.Errorf("%s CROSS-TENANT LEAK: row %s tenant=%s", m, r.ID, r.TenantID)
				}
				if r.UserID != p218UserA {
					t.Errorf("%s CROSS-USER LEAK: row %s user=%s", m, r.ID, r.UserID)
				}
			}
		})
	}
}

// TestE2E_Phase218_NamedForeignUserIsRefusedNotEmptied — the refusal is
// LOUD on every method. An empty page would be indistinguishable from
// "that user has no rows", which is the false-absence shape.
func TestE2E_Phase218_NamedForeignUserIsRefusedNotEmptied(t *testing.T) {
	t.Parallel()
	st := newUserAxisStack(t)
	defer st.close()

	ctx := p218Ctx(t, p218TenantA, p218UserA)
	req := &types.SearchRequest{Filter: types.SearchFilter{UserIDs: []string{p218UserB}}}
	for _, m := range p218Methods {
		t.Run(string(m), func(t *testing.T) {
			resp, err := st.surface.Dispatch(ctx, m, req)
			if err == nil {
				t.Fatalf("%s ANSWERED a foreign-user read with %d rows — the gate is not enforcing",
					m, len(resp.Rows))
			}
			var pe *protoerrors.Error
			if !errors.As(err, &pe) {
				t.Fatalf("%s: got %v, want *protoerrors.Error", m, err)
			}
			if pe.Code != protoerrors.CodeScopeMismatch {
				t.Errorf("%s: got code %q, want CodeScopeMismatch", m, pe.Code)
			}
		})
	}
}

// TestE2E_Phase218_BothClaimsReopenBothWidenings — under EACH claim of the
// closed admin-tier set, a named foreign user reads that user and an
// elided user fans across the tenant rather than folding.
func TestE2E_Phase218_BothClaimsReopenBothWidenings(t *testing.T) {
	t.Parallel()
	for _, scope := range []protocolauth.Scope{protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet} {
		t.Run(string(scope), func(t *testing.T) {
			t.Parallel()
			st := newUserAxisStack(t)
			defer st.close()

			ctx := p218Ctx(t, p218TenantA, p218UserA, scope)

			named, err := st.surface.Dispatch(ctx, methods.MethodSearchSessions,
				&types.SearchRequest{Filter: types.SearchFilter{UserIDs: []string{p218UserB}}})
			if err != nil {
				t.Fatalf("named foreign user under %s: %v", scope, err)
			}
			if len(named.Rows) == 0 {
				t.Fatalf("named foreign user under %s returned nothing — the widening is inert", scope)
			}
			for _, r := range named.Rows {
				if r.UserID != p218UserB {
					t.Errorf("under %s a read widened to %q returned a row owned by %q", scope, p218UserB, r.UserID)
				}
			}

			elided, err := st.surface.Dispatch(ctx, methods.MethodSearchSessions, &types.SearchRequest{})
			if err != nil {
				t.Fatalf("elided under %s: %v", scope, err)
			}
			seen := map[string]bool{}
			for _, r := range elided.Rows {
				seen[r.UserID] = true
				if r.TenantID != p218TenantA {
					t.Errorf("under %s an elided user axis crossed the TENANT axis too: %s", scope, r.TenantID)
				}
			}
			if !seen[p218UserA] || !seen[p218UserB] {
				t.Errorf("under %s a widened read must NOT fold its elided user axis: saw %v", scope, seen)
			}

			// The cross-tenant widening the surface already offered still
			// works on the same claim.
			crossTenant, err := st.surface.Dispatch(ctx, methods.MethodSearchSessions,
				&types.SearchRequest{Filter: types.SearchFilter{TenantIDs: []string{p218TenantA, p218TenantB}}})
			if err != nil {
				t.Fatalf("cross-tenant under %s: %v", scope, err)
			}
			tenants := map[string]bool{}
			for _, r := range crossTenant.Rows {
				tenants[r.TenantID] = true
			}
			if !tenants[p218TenantA] || !tenants[p218TenantB] {
				t.Errorf("under %s the cross-tenant read regressed: saw %v", scope, tenants)
			}
		})
	}
}

// TestE2E_Phase218_AggregateAgreesWithEachPerIndexSearcher — the palette
// dispatcher and the per-index methods must resolve the SAME user axis.
// A gate present in four searchers and absent from the fifth path is the
// gap shape this phase exists to close.
func TestE2E_Phase218_AggregateAgreesWithEachPerIndexSearcher(t *testing.T) {
	t.Parallel()
	st := newUserAxisStack(t)
	defer st.close()

	ctx := p218Ctx(t, p218TenantA, p218UserA)
	agg, err := st.surface.Dispatch(ctx, methods.MethodSearchQuery,
		&types.SearchRequest{PageSize: types.MaxSearchPageSize})
	if err != nil {
		t.Fatalf("search.query: %v", err)
	}
	aggIDs := map[string]bool{}
	for _, r := range agg.Rows {
		aggIDs[string(r.Index)+"/"+r.ID] = true
	}

	for _, m := range p218Methods[1:] {
		per, perr := st.surface.Dispatch(ctx, m, &types.SearchRequest{PageSize: types.MaxSearchPageSize})
		if perr != nil {
			t.Fatalf("%s: %v", m, perr)
		}
		if len(per.Rows) == 0 {
			t.Fatalf("%s returned nothing", m)
		}
		for _, r := range per.Rows {
			if r.UserID != p218UserA {
				t.Errorf("%s CROSS-USER LEAK: row %s user=%s", m, r.ID, r.UserID)
			}
			if !aggIDs[string(r.Index)+"/"+r.ID] {
				t.Errorf("%s row %s/%s is absent from the aggregate union — the two paths resolved different axes",
					m, r.Index, r.ID)
			}
		}
	}
}

// TestE2E_Phase218_FailureModes — the §17.3 failure-mode requirement,
// three of them: a redactor that errors, a foreign-user refusal that is a
// code and not an empty page, and a closed session registry whose error
// PROPAGATES instead of degrading to zero rows.
func TestE2E_Phase218_FailureModes(t *testing.T) {
	t.Parallel()

	t.Run("redactor error refuses the row rather than emitting it", func(t *testing.T) {
		t.Parallel()
		store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("state inmem: %v", err)
		}
		defer store.Close(context.Background())
		bus, err := eventinmem.New(config.EventsConfig{
			MaxSubscribersPerSession: 4, SubscriberBufferSize: 16,
			IdleTimeout: 30 * time.Second, DropWindow: time.Second, ReplayBufferSize: 64,
		}, patterns.New())
		if err != nil {
			t.Fatalf("events inmem: %v", err)
		}
		defer bus.Close(context.Background())
		sreg, err := sessionsubsys.New(store, config.SessionsConfig{}, bus)
		if err != nil {
			t.Fatalf("sessions.New: %v", err)
		}
		defer sreg.CloseRegistry(context.Background())

		ident := identity.Identity{TenantID: p218TenantA, UserID: p218UserA, SessionID: "s0"}
		ctx, err := identity.WithVerified(context.Background(), ident)
		if err != nil {
			t.Fatalf("identity.WithVerified: %v", err)
		}
		if _, oerr := sreg.Open(ctx, ident.SessionID, ident); oerr != nil {
			t.Fatalf("sessions.Open: %v", oerr)
		}

		s, err := sessionsearch.New(sreg, search.Deps{
			Redactor:   p218FailingRedactor{},
			AdminScope: server.SearchAdminScopeFromAuth,
		})
		if err != nil {
			t.Fatalf("sessionsearch.New: %v", err)
		}
		if _, serr := s.Search(ctx, types.SearchRequest{}); !errors.Is(serr, search.ErrRedactionFailed) {
			t.Fatalf("got %v, want ErrRedactionFailed — a row must never ship un-redacted", serr)
		}
	})

	t.Run("a foreign user is a code, never an empty page", func(t *testing.T) {
		t.Parallel()
		st := newUserAxisStack(t)
		defer st.close()
		ctx := p218Ctx(t, p218TenantA, p218UserA)
		_, err := st.surface.Dispatch(ctx, methods.MethodSearchQuery,
			&types.SearchRequest{Filter: types.SearchFilter{UserIDs: []string{p218UserB}}})
		var pe *protoerrors.Error
		if !errors.As(err, &pe) || pe.Code != protoerrors.CodeScopeMismatch {
			t.Fatalf("got %v, want CodeScopeMismatch", err)
		}
	})

	t.Run("a closed session registry propagates rather than degrading", func(t *testing.T) {
		t.Parallel()
		st := newUserAxisStack(t)
		defer st.close()
		if err := st.sessions.CloseRegistry(context.Background()); err != nil {
			t.Fatalf("CloseRegistry: %v", err)
		}
		ctx := p218Ctx(t, p218TenantA, p218UserA)
		_, err := st.surface.Dispatch(ctx, methods.MethodSearchSessions, &types.SearchRequest{})
		if err == nil {
			t.Fatal("a closed registry must not degrade to an empty page")
		}
		var pe *protoerrors.Error
		if !errors.As(err, &pe) || pe.Code != protoerrors.CodeRuntimeError {
			t.Fatalf("got %v, want CodeRuntimeError carrying the registry failure", err)
		}
	})
}

// TestE2E_Phase218_ConcurrencyStress — the §17.3 cross-package stress:
// N concurrent callers split across two tenants x two users, all against
// ONE shared surface, asserting no cross-talk on either principal axis
// and no goroutine leak after teardown.
func TestE2E_Phase218_ConcurrencyStress(t *testing.T) {
	st := newUserAxisStack(t)
	defer st.close()

	const N = 64
	principals := []struct{ tenant, user string }{
		{p218TenantA, p218UserA},
		{p218TenantA, p218UserB},
		{p218TenantB, p218UserA},
		{p218TenantB, p218UserB},
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	failures := make(chan string, N*4)
	for i := range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := principals[i%len(principals)]
			ctx := p218Ctx(t, p.tenant, p.user)
			resp, err := st.surface.Dispatch(ctx, p218Methods[i%len(p218Methods)], &types.SearchRequest{})
			if err != nil {
				failures <- fmt.Sprintf("g%d (%s/%s): %v", i, p.tenant, p.user, err)
				return
			}
			for _, r := range resp.Rows {
				if r.TenantID != p.tenant || r.UserID != p.user {
					failures <- fmt.Sprintf("g%d LEAK: row %s is %s/%s, caller is %s/%s",
						i, r.ID, r.TenantID, r.UserID, p.tenant, p.user)
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
		t.Fatalf("cross-package concurrency failures (%d), first %d:\n  %v",
			len(msgs), min(len(msgs), 8), msgs[:min(len(msgs), 8)])
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

// p218FailingRedactor is a real Redactor implementation that always
// errors — not a stand-in for the subsystem, just the forced failure the
// §17.3 failure-mode requirement asks for.
type p218FailingRedactor struct{}

func (p218FailingRedactor) Redact(context.Context, any) (any, error) {
	return nil, errors.New("phase218: forced redaction failure")
}
