package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	artifactsubsys "github.com/hurtener/Harbor/internal/artifacts"
	artifactinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	eventinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
	steeringsubsys "github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/search"
	artifactsearch "github.com/hurtener/Harbor/internal/search/artifacts"
	eventsearch "github.com/hurtener/Harbor/internal/search/events"
	sessionsearch "github.com/hurtener/Harbor/internal/search/sessions"
	tasksearch "github.com/hurtener/Harbor/internal/search/tasks"
	sessionsubsys "github.com/hurtener/Harbor/internal/sessions"
	statesubsys "github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	tasksubsys "github.com/hurtener/Harbor/internal/tasks"
	taskinprocess "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// searchStack assembles the full cross-subsystem set with real drivers:
// audit, state, events, sessions, tasks, artifacts, the search registry,
// and the search-aware control transport. No mocks at the boundary
// (§17.3).
type searchStack struct {
	redactor audit.Redactor
	state    statesubsys.StateStore
	bus      eventsubsys.EventBus
	sessions *sessionsubsys.Registry
	tasks    tasksubsys.TaskRegistry
	artifs   artifactsubsys.ArtifactStore

	registry *search.SearcherRegistry
	surface  *protocol.SearchSurface

	// adminScope is a settable predicate so tests can switch
	// between admin-allowed and admin-denied without rebuilding.
	adminFlag *bool

	server *httptest.Server
	close  func()
}

func newSearchStack(t *testing.T, allowAdmin bool) *searchStack {
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
		ReplayBufferSize:         512,
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

	admin := allowAdmin
	checker := func(_ context.Context) bool { return admin }
	deps := search.Deps{Redactor: red, AdminScope: checker}

	ss, err := sessionsearch.New(sreg, deps)
	if err != nil {
		t.Fatalf("session search: %v", err)
	}
	ts, err := tasksearch.New(sreg, taskReg, deps)
	if err != nil {
		t.Fatalf("task search: %v", err)
	}
	replayer, _ := bus.(eventsubsys.Replayer)
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
	surf, err := protocol.NewSearchSurface(reg, checker)
	if err != nil {
		t.Fatalf("NewSearchSurface: %v", err)
	}

	// Compose the HTTP transport for the smoke-shape integration test.
	steerReg := steeringsubsys.NewRegistry()
	ctlSurf, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		t.Fatalf("control surface: %v", err)
	}
	handler, err := control.NewHandler(ctlSurf, control.WithSearchSurface(surf))
	if err != nil {
		t.Fatalf("control.NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, handler)
	srv := httptest.NewServer(mux)

	cleanup := func() {
		srv.Close()
		_ = taskReg.Close(context.Background())
		_ = sreg.CloseRegistry(context.Background())
		_ = artStore.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	}

	return &searchStack{
		redactor:  red,
		state:     store,
		bus:       bus,
		sessions:  sreg,
		tasks:     taskReg,
		artifs:    artStore,
		registry:  reg,
		surface:   surf,
		adminFlag: &admin,
		server:    srv,
		close:     cleanup,
	}
}

func (s *searchStack) openSession(t *testing.T, ident identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), ident)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := s.sessions.Open(ctx, ident.SessionID, ident); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

func (s *searchStack) spawnTask(t *testing.T, ident identity.Identity, desc string) {
	t.Helper()
	if _, err := s.tasks.Spawn(context.Background(), tasksubsys.SpawnRequest{
		Identity:    identity.Quadruple{Identity: ident},
		Kind:        tasksubsys.KindForeground,
		Description: desc,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
}

func (s *searchStack) publishEvent(t *testing.T, q identity.Quadruple) {
	t.Helper()
	if err := s.bus.Publish(context.Background(), eventsubsys.Event{
		Type:     eventsubsys.EventTypeRuntimeError,
		Identity: q,
		Payload:  eventsubsys.RedactedMap{Data: map[string]any{"msg": "synthetic-error"}},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func (s *searchStack) putArtifact(t *testing.T, scope artifactsubsys.ArtifactScope, name string) {
	t.Helper()
	if _, err := s.artifs.PutText(context.Background(), scope, "content for "+name, artifactsubsys.PutOpts{
		Filename: name,
		MimeType: "text/plain",
	}); err != nil {
		t.Fatalf("PutText: %v", err)
	}
}

// TestE2E_SearchCluster_RoundTripsEveryMethod is the wave-end §17.1
// integration test: real sessions + tasks + events + artifacts +
// Protocol transport all wired together, exercising each of the five
// search methods end-to-end.
func TestE2E_SearchCluster_RoundTripsEveryMethod(t *testing.T) {
	t.Parallel()
	st := newSearchStack(t, false)
	defer st.close()

	ident := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "sess-1"}
	st.openSession(t, ident)
	st.spawnTask(t, ident, "deploy production")
	st.publishEvent(t, identity.Quadruple{Identity: ident})
	st.putArtifact(t, artifactsubsys.ArtifactScope{
		TenantID: ident.TenantID, UserID: ident.UserID, SessionID: ident.SessionID,
	}, "report.pdf")

	ctx, err := identity.With(context.Background(), ident)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	for _, m := range []methods.Method{
		methods.MethodSearchQuery,
		methods.MethodSearchSessions,
		methods.MethodSearchTasks,
		methods.MethodSearchEvents,
		methods.MethodSearchArtifacts,
	} {

		t.Run(string(m), func(t *testing.T) {
			resp, err := st.surface.Dispatch(ctx, m, &types.SearchRequest{})
			if err != nil {
				t.Fatalf("Dispatch(%s): %v", m, err)
			}
			if resp.ProtocolVersion == "" {
				t.Errorf("ProtocolVersion empty on %s response", m)
			}
		})
	}
}

// TestE2E_SearchCluster_MissingIdentityRejectedLoudly — §17.3 failure
// mode #1: a request without an identity in ctx fails closed with
// CodeIdentityRequired across every method.
func TestE2E_SearchCluster_MissingIdentityRejectedLoudly(t *testing.T) {
	t.Parallel()
	st := newSearchStack(t, false)
	defer st.close()

	for _, m := range []methods.Method{
		methods.MethodSearchQuery,
		methods.MethodSearchSessions,
		methods.MethodSearchTasks,
		methods.MethodSearchEvents,
		methods.MethodSearchArtifacts,
	} {

		t.Run(string(m), func(t *testing.T) {
			_, err := st.surface.Dispatch(context.Background(), m, &types.SearchRequest{})
			var pe *protoerrors.Error
			if !errors.As(err, &pe) || pe.Code != protoerrors.CodeIdentityRequired {
				t.Errorf("got %v, want CodeIdentityRequired", err)
			}
		})
	}
}

// TestE2E_SearchCluster_CrossTenantWithoutAdminRejected — §17.3
// failure mode #2: a cross-tenant request without auth.ScopeAdmin
// fails with CodeScopeMismatch (mapped to 403) across every method.
func TestE2E_SearchCluster_CrossTenantWithoutAdminRejected(t *testing.T) {
	t.Parallel()
	st := newSearchStack(t, false)
	defer st.close()

	ident := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	st.openSession(t, ident)
	ctx, _ := identity.With(context.Background(), ident)

	req := &types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	}
	for _, m := range []methods.Method{
		methods.MethodSearchQuery,
		methods.MethodSearchSessions,
		methods.MethodSearchTasks,
		methods.MethodSearchEvents,
		methods.MethodSearchArtifacts,
	} {

		t.Run(string(m), func(t *testing.T) {
			_, err := st.surface.Dispatch(ctx, m, req)
			var pe *protoerrors.Error
			if !errors.As(err, &pe) {
				t.Fatalf("got %v, want *protoerrors.Error", err)
			}
			if pe.Code != protoerrors.CodeScopeMismatch {
				t.Errorf("got %q, want CodeScopeMismatch", pe.Code)
			}
		})
	}
}

// TestE2E_SearchCluster_AdminCrossTenantAllowed — Admin scope reverses
// the rejection; cross-tenant rows surface.
func TestE2E_SearchCluster_AdminCrossTenantAllowed(t *testing.T) {
	t.Parallel()
	st := newSearchStack(t, true)
	defer st.close()

	st.openSession(t, identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s1"})
	st.openSession(t, identity.Identity{TenantID: "t2", UserID: "u", SessionID: "s2"})
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s1"})
	resp, err := st.surface.Dispatch(ctx, methods.MethodSearchSessions, &types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Errorf("admin cross-tenant: got %d rows, want 2", len(resp.Rows))
	}
}

// TestE2E_SearchCluster_HeavyPayloadBypass — §17.3 failure mode #3:
// when an artifact's preview byte-length exceeds the D-026 threshold,
// the result row ships an ArtifactRef instead of inline bytes. We
// always ship an ArtifactRef for `artifacts` rows (by-reference by
// construction), so this also pins that contract.
func TestE2E_SearchCluster_HeavyPayloadBypass(t *testing.T) {
	t.Parallel()
	st := newSearchStack(t, false)
	defer st.close()

	scope := artifactsubsys.ArtifactScope{TenantID: "t1", UserID: "u", SessionID: "s"}
	st.openSession(t, identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s"})
	st.putArtifact(t, scope, "big-report.pdf")

	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s"})
	resp, err := st.surface.Dispatch(ctx, methods.MethodSearchArtifacts, &types.SearchRequest{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(resp.Rows) == 0 {
		t.Fatal("expected at least one artifact row")
	}
	for _, r := range resp.Rows {
		if r.Ref == nil {
			t.Errorf("artifact row %s must carry a Ref (D-026); got nil", r.ID)
		}
	}
}

// TestE2E_SearchCluster_HTTP_RoundTrip exercises the wire transport
// end-to-end with the realistic POST shape the smoke script will use.
func TestE2E_SearchCluster_HTTP_RoundTrip(t *testing.T) {
	t.Parallel()
	st := newSearchStack(t, false)
	defer st.close()

	ident := identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s1"}
	st.openSession(t, ident)
	st.spawnTask(t, ident, "deploy production")

	body, _ := json.Marshal(types.SearchRequest{Query: "deploy"})
	// We do NOT attach an auth.Middleware here because the auth
	// middleware path is Phase 61's concern; the search-handler test
	// must still surface 401 when ctx carries no identity. We post
	// directly to the bare handler; identity is missing → 401.
	r, _ := http.NewRequest(http.MethodPost, st.server.URL+"/v1/control/search.tasks", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wire missing-identity: got %d, want 401", resp.StatusCode)
	}
}

// TestE2E_SearchCluster_CrossSessionIsolation_Concurrent — §17.3
// concurrency stress: N concurrent searchers across two tenants,
// asserting no cross-talk. Real drivers throughout.
func TestE2E_SearchCluster_CrossSessionIsolation_Concurrent(t *testing.T) {
	const N = 16
	st := newSearchStack(t, false)
	defer st.close()

	for _, tenant := range []string{"t1", "t2"} {
		for i := range 5 {
			ident := identity.Identity{
				TenantID:  tenant,
				UserID:    "u",
				SessionID: fmt.Sprintf("%s-s%d", tenant, i),
			}
			st.openSession(t, ident)
			st.spawnTask(t, ident, fmt.Sprintf("%s task %d", tenant, i))
			st.publishEvent(t, identity.Quadruple{Identity: ident})
			st.putArtifact(t, artifactsubsys.ArtifactScope{
				TenantID: tenant, UserID: "u", SessionID: ident.SessionID,
			}, fmt.Sprintf("%s-file%d", tenant, i))
		}
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	failures := make(chan string, N*4)
	for i := range N {

		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := "t1"
			if i%2 == 0 {
				tenant = "t2"
			}
			sessIdx := i % 5
			ident := identity.Identity{
				TenantID:  tenant,
				UserID:    "u",
				SessionID: fmt.Sprintf("%s-s%d", tenant, sessIdx),
			}
			ctx, _ := identity.With(context.Background(), ident)
			// Hit every method.
			for _, m := range []methods.Method{
				methods.MethodSearchQuery,
				methods.MethodSearchSessions,
				methods.MethodSearchTasks,
				methods.MethodSearchEvents,
				methods.MethodSearchArtifacts,
			} {
				resp, err := st.surface.Dispatch(ctx, m, &types.SearchRequest{})
				if err != nil {
					failures <- fmt.Sprintf("g%d/%s: %v", i, m, err)
					continue
				}
				for _, r := range resp.Rows {
					if r.TenantID != "" && r.TenantID != tenant {
						failures <- fmt.Sprintf("g%d/%s: LEAK tenant=%s, caller=%s", i, m, r.TenantID, tenant)
					}
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
		t.Fatalf("cross-session isolation failures (%d):\n  %v", len(msgs), msgs)
	}

	// Goroutine baseline.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	if got := runtime.NumGoroutine(); got > baseline+10 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}
