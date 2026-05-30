// D-171 cross-subsystem integration test per CLAUDE.md §17 — the
// per-request session model exercised end-to-end against the real wire
// transport + the real sessions.Registry + the real
// auth.Validator/Middleware, with no mocks at any seam.
//
// What it pins (the D-171 corrected model):
//
//   - The connection token authenticates (tenant, user, scopes); the
//     SESSION is chosen per-request via the X-Harbor-Session header, NOT
//     pinned by the token's session claim.
//   - Create-on-first-use: a `control.start` on a not-yet-existing
//     session id materialises the session row.
//   - Multiple sessions coexist under ONE token, fully isolated (§6).
//   - sessions.list returns every session under the (tenant, user).
//   - A past session is reloadable (sessions.inspect) — including across
//     a RESTART of the runtime against the same SQLite state dir (the
//     persistent catalog re-discovers prior sessions).
//
// The test wires the surfaces by hand (not via devstack) so it can tear
// the whole stack down and re-open over the same SQLite file to simulate
// a process restart — the headline regression the boot-crash bug
// required.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const d171Kid = "d171-kid"

var fixedNowD171 = time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

// d171Stack mirrors the production `cmd/harbor` boot surfaces relevant to
// the session model: SessionRegistry + ControlSurface (with the
// create-on-first-use ensurer) + sessions.* Protocol service + auth.
type d171Stack struct {
	mux     http.Handler
	priv    *ecdsa.PrivateKey
	reg     *sessions.Registry
	cleanup func()
}

// d171Ensurer adapts the registry to the protocol.SessionEnsurer seam —
// the same shape cmd/harbor + devstack use.
type d171Ensurer struct{ reg *sessions.Registry }

func (e d171Ensurer) EnsureSession(ctx context.Context, id identity.Identity) error {
	_, err := e.reg.EnsureOpen(ctx, id)
	return err
}

// newD171Stack builds a stack over a SQLite store at `dsn`. Calling it
// twice with the same dsn simulates a restart.
func newD171Stack(t *testing.T, dsn string, priv *ecdsa.PrivateKey, pub *ecdsa.PublicKey) *d171Stack {
	t.Helper()
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
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("state.Open(sqlite): %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	reg, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	// NOTE (D-171): no boot-time Open of a fixed session — that path
	// crashed against a persisted-closed session. This stack boots clean
	// over an existing state dir regardless of session state.
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry(),
		protocol.WithSessionEnsurer(d171Ensurer{reg: reg}),
	)
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	projector, err := sessionsprotocol.NewListerProjector(reg)
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	sessionsSvc, err := sessionsprotocol.NewService(projector,
		sessionsprotocol.WithBus(bus), sessionsprotocol.WithRedactor(red))
	if err != nil {
		t.Fatalf("sessions/protocol.NewService: %v", err)
	}
	keys := newES256KeySet(d171Kid, pub)
	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNowD171 }),
		auth.WithRedactor(red))
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
	return &d171Stack{
		mux:  mux,
		priv: priv,
		reg:  reg,
		cleanup: func() {
			_ = reg.CloseRegistry(context.Background())
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// d171Token mints a connection token. Per D-171 the session claim is a
// DEFAULT only — the per-request session comes from the X-Harbor-Session
// header. We give the token a benign default session.
func d171Token(t *testing.T, priv *ecdsa.PrivateKey, tenant, user string, scopes []string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     user,
		"exp":     fixedNowD171.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowD171.Add(-1 * time.Minute).Unix(),
		"tenant":  tenant,
		"user":    user,
		"session": "default", // DEFAULT only; never a hard pin
		"scopes":  scopes,
	}
	return signES256Wave10(t, priv, claims, d171Kid)
}

// d171Post issues a POST with the connection token and a per-request
// session header. body is the raw JSON request body.
func d171Post(t *testing.T, srvURL, path, sessionHeader, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if sessionHeader != "" {
		req.Header.Set("X-Harbor-Session", sessionHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestE2E_D171_PerRequestSession_FullModel is the headline test. One
// connection token, two sessions chosen per-request, create-on-first-use,
// list + reload, and a restart against the same state dir.
func TestE2E_D171_PerRequestSession_FullModel(t *testing.T) {
	priv, pub := loadES256Phase61(t)
	dsn := filepath.Join(t.TempDir(), "state.sqlite")

	// ---- boot 1 ----
	st := newD171Stack(t, dsn, priv, pub)
	srv := httptest.NewServer(st.mux)

	// The connection token: tenant=dev, user=dev. NO session pin.
	tok := d171Token(t, priv, "dev", "dev", []string{"admin"})

	// (1) Create-on-first-use: start a turn in session "A". The body's
	// identity is left empty — the handler backfills it from the
	// per-request (header-chosen) session.
	status, body := d171Post(t, srv.URL, "/v1/control/start", "A",
		`{"identity":{},"query":"hello from A"}`, tok)
	if status != http.StatusOK {
		t.Fatalf("start A: status=%d body=%s", status, body)
	}
	// (2) Create a SECOND session "B" under the SAME token.
	status, body = d171Post(t, srv.URL, "/v1/control/start", "B",
		`{"identity":{},"query":"hello from B"}`, tok)
	if status != http.StatusOK {
		t.Fatalf("start B: status=%d body=%s", status, body)
	}

	// (3) sessions.list returns BOTH sessions under the one connection.
	status, body = d171Post(t, srv.URL, "/v1/sessions/list", "A", `{"filter":{}}`, tok)
	if status != http.StatusOK {
		t.Fatalf("sessions.list: status=%d body=%s", status, body)
	}
	gotIDs := decodeSessionListIDs(t, body)
	if !gotIDs["A"] || !gotIDs["B"] {
		t.Fatalf("sessions.list missing a session under one token: got %v", gotIDs)
	}

	// (4) Reload a past session via sessions.inspect.
	status, body = d171Post(t, srv.URL, "/v1/sessions/inspect", "A",
		`{"identity":{},"session_id":"A"}`, tok)
	if status != http.StatusOK {
		t.Fatalf("sessions.inspect A: status=%d body=%s", status, body)
	}
	var insp prototypes.SessionsInspectResponse
	if err := json.Unmarshal(body, &insp); err != nil {
		t.Fatalf("decode inspect: %v", err)
	}
	if insp.Row.SessionID != "A" {
		t.Fatalf("inspect returned session %q, want A", insp.Row.SessionID)
	}

	// ---- RESTART: tear the stack down, re-open over the SAME dsn ----
	srv.Close()
	st.cleanup()

	st2 := newD171Stack(t, dsn, priv, pub)
	defer st2.cleanup()
	srv2 := httptest.NewServer(st2.mux)
	defer srv2.Close()

	// (5) Boot is healthy AND sessions.list still re-discovers A and B
	// from the persistent catalog (the in-memory idIndex started empty).
	status, body = d171Post(t, srv2.URL, "/v1/sessions/list", "A", `{"filter":{}}`, tok)
	if status != http.StatusOK {
		t.Fatalf("post-restart sessions.list: status=%d body=%s", status, body)
	}
	gotIDs = decodeSessionListIDs(t, body)
	if !gotIDs["A"] || !gotIDs["B"] {
		t.Fatalf("post-restart sessions.list did not re-discover prior sessions: got %v", gotIDs)
	}

	// (6) A's history is still inspectable after the restart.
	status, body = d171Post(t, srv2.URL, "/v1/sessions/inspect", "A",
		`{"identity":{},"session_id":"A"}`, tok)
	if status != http.StatusOK {
		t.Fatalf("post-restart inspect A: status=%d body=%s", status, body)
	}
}

// decodeSessionListIDs reads a sessions.list response into a set of
// session ids.
func decodeSessionListIDs(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var resp prototypes.SessionsListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode sessions.list: %v (body=%s)", err, body)
	}
	ids := map[string]bool{}
	for _, r := range resp.Rows {
		ids[r.SessionID] = true
	}
	return ids
}

// TestE2E_D171_MultiSessionIsolation_NoCrossTalk — §6 rule 10: N
// concurrent sessions under ONE connection token, each started via its
// own X-Harbor-Session header, race-clean with no cross-talk. Every
// session ends up as its own isolated row; no session leaks another's id.
func TestE2E_D171_MultiSessionIsolation_NoCrossTalk(t *testing.T) {
	priv, pub := loadES256Phase61(t)
	dsn := filepath.Join(t.TempDir(), "state.sqlite")
	st := newD171Stack(t, dsn, priv, pub)
	defer st.cleanup()
	srv := httptest.NewServer(st.mux)
	defer srv.Close()

	tok := d171Token(t, priv, "dev", "dev", []string{"admin"})

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	statuses := make([]int, n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("conv-%d", i)
			body := fmt.Sprintf(`{"identity":{},"query":"hi from %s"}`, sid)
			statuses[i], _ = d171Post(t, srv.URL, "/v1/control/start", sid, body, tok)
		}(i)
	}
	wg.Wait()
	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("concurrent start conv-%d: status=%d", i, s)
		}
	}

	// Every distinct session must be its own isolated row.
	_, body := d171Post(t, srv.URL, "/v1/sessions/list", "conv-0", `{"filter":{}}`, tok)
	ids := decodeSessionListIDs(t, body)
	for i := range n {
		if !ids[fmt.Sprintf("conv-%d", i)] {
			t.Fatalf("session conv-%d missing from list — cross-talk or lost write: got %v", i, ids)
		}
	}
}

// TestE2E_D171_ClosedSessionStart_FailsLoud — a `start` on a closed
// session id is rejected (not silently revived). The client must pick a
// fresh session id for a new conversation.
func TestE2E_D171_ClosedSessionStart_FailsLoud(t *testing.T) {
	priv, pub := loadES256Phase61(t)
	dsn := filepath.Join(t.TempDir(), "state.sqlite")
	st := newD171Stack(t, dsn, priv, pub)
	defer st.cleanup()
	srv := httptest.NewServer(st.mux)
	defer srv.Close()

	tok := d171Token(t, priv, "dev", "dev", []string{"admin"})

	// Create session "C" via the HTTP create-on-first-use path.
	status, body := d171Post(t, srv.URL, "/v1/control/start", "C",
		`{"identity":{},"query":"first turn"}`, tok)
	if status != http.StatusOK {
		t.Fatalf("start C (first turn): status=%d body=%s", status, body)
	}
	// A second start in the SAME open session is fine (no spurious
	// already-open error surfaced to the client).
	status, body = d171Post(t, srv.URL, "/v1/control/start", "C",
		`{"identity":{},"query":"second turn"}`, tok)
	if status != http.StatusOK {
		t.Fatalf("start C (second turn, same open session): status=%d body=%s", status, body)
	}

	// Now CLOSE session C directly via the registry (simulating a GC
	// reap / operator close), then assert a NEW start on the closed id is
	// rejected — never silently revived (RFC §6.9 + §13 fail-loud).
	cID := identity.Identity{TenantID: "dev", UserID: "dev", SessionID: "C"}
	cCtx, _ := identity.With(context.Background(), cID)
	if err := st.reg.Close(cCtx, "C", "test:gc"); err != nil {
		t.Fatalf("close C: %v", err)
	}
	status, body = d171Post(t, srv.URL, "/v1/control/start", "C",
		`{"identity":{},"query":"revive attempt"}`, tok)
	if status == http.StatusOK {
		t.Fatalf("start on a CLOSED session must be rejected, got 200 (silent revive); body=%s", body)
	}
}
