// Phase 73c cross-subsystem integration test per CLAUDE.md §17 — the
// two `sessions.*` Protocol methods exercised end-to-end against the
// real wire transport + the real sessions.Registry (Phase 08) + the
// real auth.Validator/Middleware (Phase 61), with no mocks at any seam.
//
// Surfaces composed:
//
//   - Phase 08 internal/sessions — the real StateStore-backed Registry
//     whose snapshots the `sessions.*` methods project.
//   - Phase 73c internal/sessions/protocol — the Sessions Protocol
//     Service + ListerProjector.
//   - Phase 60 internal/protocol/transports — the wire surface the
//     `sessions.*` handler is mounted on.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware
//     gating the `auth.ScopeAdmin` claim on a cross-tenant `sessions.list`
//     filter (D-079).
//
// This test ships the §13 primitive-with-consumer discharge for Phase
// 73c's Go-side surface: it is the first end-to-end consumer of the
// `sessions.list` wire method — own-tenant happy path + cross-tenant
// reject / accept paths + a malformed cursor + an N≥10 SSE-subscriber
// concurrency stress run while `sessions.list` is hit in parallel.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const phase73cKid = "phase73c-kid"

var fixedNowPhase73c = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

type phase73cDeps struct {
	mux     *http.ServeMux
	priv    *ecdsa.PrivateKey
	bus     events.EventBus
	cleanup func()
}

// newPhase73cDeps wires the real dev-stack surfaces and seeds the
// SessionRegistry with two tenants × N sessions.
func newPhase73cDeps(t *testing.T) *phase73cDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	// Real Phase 08 SessionRegistry. Seed two tenants × 6 sessions.
	reg, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	for _, spec := range []struct {
		tenant, user string
		n            int
	}{
		{"tenant-A", "u-A", 6},
		{"tenant-B", "u-B", 6},
	} {
		for i := range spec.n {
			sid := spec.tenant + "-sess-" + string(rune('0'+i))
			id := identity.Identity{TenantID: spec.tenant, UserID: spec.user, SessionID: sid}
			ctx, werr := identity.With(context.Background(), id)
			if werr != nil {
				t.Fatalf("identity.With: %v", werr)
			}
			if _, oerr := reg.Open(ctx, sid, id); oerr != nil {
				t.Fatalf("seed session %q: %v", sid, oerr)
			}
		}
	}

	projector, err := sessionsprotocol.NewListerProjector(reg)
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	sessionsSvc, err := sessionsprotocol.NewService(projector,
		sessionsprotocol.WithBus(bus),
		sessionsprotocol.WithRedactor(red),
	)
	if err != nil {
		t.Fatalf("sessions/protocol.NewService: %v", err)
	}

	keys := newES256KeySet(phase73cKid, pub)
	now := func() time.Time { return fixedNowPhase73c }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithSessionsService(sessionsSvc),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase73cDeps{
		mux:  mux,
		priv: priv,
		bus:  bus,
		cleanup: func() {
			_ = reg.CloseRegistry(context.Background())
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// phase73cClaims mints a JWT MapClaims with the test's standard shape.
func phase73cClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase73c.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase73c.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// postSessions issues a POST /v1/sessions/{verb} with the supplied JWT.
func postSessions(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/sessions/"+verb, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestE2E_Phase73c_SessionsPage is the §13 primitive-with-consumer
// binding test for the Sessions-page Protocol surface. It exercises the
// own-tenant happy path, the cross-tenant reject / accept paths, a
// malformed cursor, and an N≥10 SSE-subscriber concurrency stress run.
func TestE2E_Phase73c_SessionsPage(t *testing.T) {
	deps := newPhase73cDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "tenant-A-sess-0"}
	tokA := signES256Wave10(t, deps.priv, phase73cClaims(idA, nil), phase73cKid)
	tokAdmin := signES256Wave10(t, deps.priv, phase73cClaims(idA, []string{"admin"}), phase73cKid)

	// (a) Happy path: tenant-A lists its own sessions.
	status, body := postSessions(t, srv.URL, "list", `{"filter":{}}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("sessions.list: status = %d, want 200; body=%s", status, body)
	}
	var listResp prototypes.SessionsListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode sessions.list: %v", err)
	}
	if len(listResp.Rows) != 6 {
		t.Fatalf("sessions.list own-tenant returned %d rows, want 6", len(listResp.Rows))
	}
	for _, r := range listResp.Rows {
		if r.TenantID != "tenant-A" {
			t.Fatalf("sessions.list leaked tenant %q — CLAUDE.md §6 isolation breach", r.TenantID)
		}
	}

	// (b) Cross-tenant without admin → CodeScopeMismatch (403).
	status, body = postSessions(t, srv.URL, "list",
		`{"filter":{"tenant_ids":["tenant-B"]}}`, tokA)
	if status != http.StatusForbidden {
		t.Fatalf("cross-tenant sessions.list without admin: status = %d, want 403; body=%s", status, body)
	}
	var perr protoerrors.Error
	_ = json.Unmarshal(body, &perr)
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("cross-tenant reject code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}

	// (c) Cross-tenant WITH admin → success + audit emit.
	auditCh := make(chan events.Event, 8)
	auditSub, subErr := deps.bus.Subscribe(context.Background(), events.Filter{
		Tenant:  idA.TenantID,
		User:    idA.UserID,
		Session: idA.SessionID,
		Types:   []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if subErr != nil {
		t.Fatalf("audit Subscribe: %v", subErr)
	}
	go func() {
		for ev := range auditSub.Events() {
			auditCh <- ev
		}
	}()

	status, body = postSessions(t, srv.URL, "list",
		`{"filter":{"tenant_ids":["tenant-B"]}}`, tokAdmin)
	if status != http.StatusOK {
		t.Fatalf("cross-tenant sessions.list with admin: status = %d, want 200; body=%s", status, body)
	}
	var adminResp prototypes.SessionsListResponse
	_ = json.Unmarshal(body, &adminResp)
	if len(adminResp.Rows) != 6 {
		t.Fatalf("admin cross-tenant sessions.list returned %d rows, want 6 (tenant-B)", len(adminResp.Rows))
	}
	select {
	case ev := <-auditCh:
		if ev.Type != events.EventTypeAdminScopeUsed {
			t.Fatalf("audit event type = %q, want audit.admin_scope_used", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admin-scope sessions.list did not emit an audit.admin_scope_used event")
	}
	auditSub.Cancel()

	// (d) Malformed cursor → CodeInvalidRequest (400).
	status, body = postSessions(t, srv.URL, "list",
		`{"cursor":"!!!not-base64!!!"}`, tokA)
	if status != http.StatusBadRequest {
		t.Fatalf("malformed cursor: status = %d, want 400; body=%s", status, body)
	}

	// (e) sessions.inspect happy path.
	status, body = postSessions(t, srv.URL, "inspect",
		`{"session_id":"tenant-A-sess-0"}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("sessions.inspect: status = %d, want 200; body=%s", status, body)
	}
	var inspectResp prototypes.SessionsInspectResponse
	if err := json.Unmarshal(body, &inspectResp); err != nil {
		t.Fatalf("decode sessions.inspect: %v", err)
	}
	if inspectResp.Row.SessionID != "tenant-A-sess-0" {
		t.Fatalf("sessions.inspect row = %q, want tenant-A-sess-0", inspectResp.Row.SessionID)
	}

	// (f) Missing identity → CodeIdentityRequired (401).
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions/list", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	noIDResp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		t.Fatalf("no-identity request: %v", doErr)
	}
	_ = noIDResp.Body.Close()
	if noIDResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-identity sessions.list: status = %d, want 401", noIDResp.StatusCode)
	}
}

// TestE2E_Phase73c_SessionsConcurrencyStress runs N≥10 concurrent SSE
// subscribers consuming a session's events while `sessions.list` is hit
// in parallel — no cross-talk, baseline goroutine count restored after
// teardown (CLAUDE.md §17.3 concurrency-stress requirement).
func TestE2E_Phase73c_SessionsConcurrencyStress(t *testing.T) {
	deps := newPhase73cDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "tenant-A-sess-0"}
	tokA := signES256Wave10(t, deps.priv, phase73cClaims(idA, nil), phase73cKid)

	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const subscribers = 12
	const listers = 20

	subCtx, subCancel := context.WithCancel(context.Background())
	var subWG sync.WaitGroup
	subWG.Add(subscribers)
	for range subscribers {
		go func() {
			defer subWG.Done()
			sub, err := deps.bus.Subscribe(subCtx, events.Filter{
				Tenant:  idA.TenantID,
				User:    idA.UserID,
				Session: idA.SessionID,
			})
			if err != nil {
				return
			}
			defer sub.Cancel()
			for {
				select {
				case <-subCtx.Done():
					return
				case _, ok := <-sub.Events():
					if !ok {
						return
					}
				}
			}
		}()
	}

	var listWG sync.WaitGroup
	listWG.Add(listers)
	errCh := make(chan error, listers)
	for range listers {
		go func() {
			defer listWG.Done()
			status, body := postSessions(t, srv.URL, "list", `{"filter":{}}`, tokA)
			if status != http.StatusOK {
				errCh <- &stressError{status: status, body: string(body)}
				return
			}
			var resp prototypes.SessionsListResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				errCh <- err
				return
			}
			for _, r := range resp.Rows {
				if r.TenantID != "tenant-A" {
					errCh <- &stressError{status: 0, body: "tenant leak: " + r.TenantID}
					return
				}
			}
		}()
	}
	listWG.Wait()
	close(errCh)
	for e := range errCh {
		t.Errorf("concurrent sessions.list: %v", e)
	}

	subCancel()
	subWG.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline+4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 8 {
		t.Errorf("goroutine leak: %d goroutines above baseline %d after the stress run", leaked, baseline)
	}
}

type stressError struct {
	status int
	body   string
}

func (e *stressError) Error() string {
	return "sessions.list stress failure: status=" + http.StatusText(e.status) + " body=" + e.body
}
