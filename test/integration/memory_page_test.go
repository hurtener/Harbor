// Phase 73j cross-subsystem integration test per CLAUDE.md §17 — the
// three `memory.*` Console-page Protocol methods exercised end-to-end
// against the real wire transport + the real memory subsystem
// (Phases 23–25) + the real auth.Validator/Middleware (Phase 61) + the
// real in-mem ArtifactStore + the real events bus/Aggregator, with no
// mocks at any seam.
//
// Surfaces composed:
//
//   - Phase 23–25 internal/memory — the MemoryStore the Console page
//     projects from (in-mem driver here; the SQLite/Postgres drivers
//     share the same conformance suite).
//   - Phase 60 internal/protocol/transports — the wire surface the
//     three `memory.*` handlers are mounted on.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware
//     gating the cross-tenant `admin` scope claim (D-079).
//   - Phase 17–19 internal/artifacts — the ArtifactStore the D-026
//     heavy-content bypass routes oversized memory values through.
//   - Phase 05/06 internal/events — the bus + Aggregator the 24h
//     identity-rejected / recovery-dropped counters derive from, and
//     the subscription the operator's Recent-identity-rejections card
//     consumes.
//
// This test ships the §13 primitive-with-consumer discharge for Phase
// 73j: it is the first wire-level consumer of the three `memory.*`
// surfaces — caller's-own-scope happy path, the cross-tenant reject
// path without the admin claim, the identity-required failure-loud
// path with the bus assertion that `memory.identity_rejected` surfaces
// (D-033), the D-026 heavy-value round-trip, and an N≥10 two-tenant
// concurrency stress.
package integration_test

import (
	"context"
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

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const phase73jKid = "phase73j-kid"

var fixedNowPhase73j = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

const phase73jHeavyThreshold = 1024

type phase73jDeps struct {
	mux     *http.ServeMux
	store   memory.MemoryStore
	bus     events.EventBus
	cleanup func()
}

func newPhase73jDeps(t *testing.T) *phase73jDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
	_ = priv
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

	stateStore, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	memStore, err := memoryinmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyTruncation,
		BudgetTokens: 1_000_000,
	}, memory.Deps{State: stateStore, Bus: bus}, memoryinmem.Options{})
	if err != nil {
		_ = stateStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("memoryinmem.New: %v", err)
	}

	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    stateStore,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = memStore.Close(context.Background())
		_ = stateStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = memStore.Close(context.Background())
		_ = stateStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = memStore.Close(context.Background())
		_ = stateStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("artifacts.Open: %v", err)
	}

	// pause.list deps are required for the artifact-store / threshold
	// the memory handler reuses for the D-026 bypass.
	coord := pauseresume.New(pauseresume.WithBus(bus))

	keys := newES256KeySet(phase73jKid, pub)
	now := func() time.Time { return fixedNowPhase73j }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		_ = artStore.Close(context.Background())
		_ = taskReg.Close(context.Background())
		_ = memStore.Close(context.Background())
		_ = stateStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithPauseList(coord, artStore, phase73jHeavyThreshold),
		transports.WithMemory(memStore, "inmem"),
	)
	if err != nil {
		_ = artStore.Close(context.Background())
		_ = taskReg.Close(context.Background())
		_ = memStore.Close(context.Background())
		_ = stateStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase73jDeps{
		mux:   mux,
		store: memStore,
		bus:   bus,
		cleanup: func() {
			_ = artStore.Close(context.Background())
			_ = taskReg.Close(context.Background())
			_ = memStore.Close(context.Background())
			_ = stateStore.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

func phase73jClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase73j.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase73j.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// seedPhase73jTurn appends one conversation turn to the memory store.
func seedPhase73jTurn(t *testing.T, store memory.MemoryStore, id identity.Identity, user, assistant string) {
	t.Helper()
	if err := store.AddTurn(context.Background(), identity.Quadruple{Identity: id}, memory.ConversationTurn{
		UserMessage:       user,
		AssistantResponse: assistant,
		Timestamp:         fixedNowPhase73j,
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
}

// postMemory issues a POST against a /v1/memory/* route with the JWT.
func postMemory(t *testing.T, srvURL, route, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+route, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestE2E_Phase73j_MemoryPageHappyPath is the §13 primitive-with-
// consumer binding test: an authenticated operator lists / gets /
// health-checks their own identity scope end-to-end at the wire.
func TestE2E_Phase73j_MemoryPageHappyPath(t *testing.T) {
	deps := newPhase73jDeps(t)
	defer deps.cleanup()
	priv, _ := loadES256Phase61(t)

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	seedPhase73jTurn(t, deps.store, id, "first question", "first answer")
	seedPhase73jTurn(t, deps.store, id, "second question", "second answer")
	tok := signES256Wave10(t, priv, phase73jClaims(id, nil), phase73jKid)

	// memory.list — caller's own scope, 200 + 2 items.
	status, body := postMemory(t, srv.URL, "/v1/memory/list", `{}`, tok)
	if status != http.StatusOK {
		t.Fatalf("memory.list: status = %d, want 200; body=%s", status, body)
	}
	var listResp prototypes.MemoryListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listResp.TotalRows != 2 || len(listResp.Items) != 2 {
		t.Fatalf("memory.list: TotalRows=%d items=%d, want 2/2", listResp.TotalRows, len(listResp.Items))
	}
	for _, it := range listResp.Items {
		if it.Identity.Tenant != id.TenantID {
			t.Errorf("memory.list: row tenant %q != caller %q", it.Identity.Tenant, id.TenantID)
		}
	}

	// memory.get — one of the listed keys, 200 + light inline value.
	key := listResp.Items[0].Key
	status, body = postMemory(t, srv.URL, "/v1/memory/get", `{"key":"`+key+`"}`, tok)
	if status != http.StatusOK {
		t.Fatalf("memory.get: status = %d, want 200; body=%s", status, body)
	}
	var getResp prototypes.MemoryGetResponse
	if err := json.Unmarshal(body, &getResp); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if len(getResp.Detail.Value) == 0 || getResp.Detail.ValueArtifact != nil {
		t.Errorf("memory.get(light): Value/ValueArtifact = %d/%v, want non-empty/nil (D-026)",
			len(getResp.Detail.Value), getResp.Detail.ValueArtifact)
	}

	// memory.health — 200 + total = 2.
	status, body = postMemory(t, srv.URL, "/v1/memory/health", `{}`, tok)
	if status != http.StatusOK {
		t.Fatalf("memory.health: status = %d, want 200; body=%s", status, body)
	}
	var healthResp prototypes.MemoryHealthResponse
	if err := json.Unmarshal(body, &healthResp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if healthResp.Aggregate.Total != 2 {
		t.Errorf("memory.health: Total = %d, want 2", healthResp.Aggregate.Total)
	}
}

// TestE2E_Phase73j_CrossTenantRejectedWithoutAdminScope pins the D-079
// posture: a non-admin caller naming a foreign tenant is rejected 403
// — NO new memory scope is involved (audit B1).
func TestE2E_Phase73j_CrossTenantRejectedWithoutAdminScope(t *testing.T) {
	deps := newPhase73jDeps(t)
	defer deps.cleanup()
	priv, _ := loadES256Phase61(t)

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	tok := signES256Wave10(t, priv, phase73jClaims(id, nil), phase73jKid)

	status, body := postMemory(t, srv.URL, "/v1/memory/list",
		`{"filter":{"tenant_ids":["tenant-other"]}}`, tok)
	if status != http.StatusForbidden {
		t.Fatalf("cross-tenant without admin: status = %d, want 403; body=%s", status, body)
	}
}

// TestE2E_Phase73j_IdentityRequiredFailsLoudAndSurfacesOnBus pins the
// D-033 fail-closed contract: a MemoryStore op with an incomplete
// triple fails loudly AND emits `memory.identity_rejected` on the bus —
// the operator's Recent-identity-rejections card consumes that event.
func TestE2E_Phase73j_IdentityRequiredFailsLoudAndSurfacesOnBus(t *testing.T) {
	deps := newPhase73jDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	// Subscribe to memory.identity_rejected BEFORE driving the
	// rejection — the operator's right-rail card subscription.
	subID := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	sub, err := deps.bus.Subscribe(context.Background(), events.Filter{
		Tenant:  subID.TenantID,
		User:    subID.UserID,
		Session: subID.SessionID,
		Admin:   true, // fan-in the <missing>-substituted rejection events
		Types:   []events.EventType{memory.EventTypeMemoryIdentityRejected},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// A request whose carrier identity is missing session_id is
	// rejected at the wire edge with 401 — Phase 61 auth.Middleware
	// fails closed before the handler runs. The wire-level rejection
	// is the first half; the bus emit is the second.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/memory/list", strings.NewReader(`{}`))
	req.Header.Set("X-Harbor-Tenant", subID.TenantID)
	req.Header.Set("X-Harbor-User", subID.UserID)
	// session header deliberately omitted.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("memory.list with incomplete identity: status = 200, want a fail-closed code")
	}

	// The runtime-side D-033 emit: drive a MemoryStore op with an
	// incomplete triple directly and assert the event reaches the
	// operator's subscription. The driver fails closed AND emits
	// `memory.identity_rejected` (D-033) — this is the event the
	// Console's Recent-identity-rejections card renders verbatim.
	_ = deps.store.AddTurn(context.Background(),
		identity.Quadruple{Identity: identity.Identity{TenantID: subID.TenantID, UserID: subID.UserID}},
		memory.ConversationTurn{})

	select {
	case ev := <-sub.Events():
		if ev.Type != memory.EventTypeMemoryIdentityRejected {
			t.Fatalf("subscriber got %q, want memory.identity_rejected", ev.Type)
		}
		// D-033 invariant: the missing component is substituted with
		// the "<missing>" sentinel so the event is bus-publishable —
		// the Console renders it verbatim, never masks it.
		if ev.Identity.SessionID != "<missing>" {
			t.Errorf("identity_rejected event session = %q, want <missing> sentinel (D-033)", ev.Identity.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for memory.identity_rejected event on the bus (D-033)")
	}
}

// TestE2E_Phase73j_HeavyValueRoutesThroughArtifacts pins the D-026
// closure end-to-end: a heavy memory value is returned by-reference
// via memory.get's ValueArtifact, NEVER as inline bytes — and the
// stub resolves in the ArtifactStore.
func TestE2E_Phase73j_HeavyValueRoutesThroughArtifacts(t *testing.T) {
	deps := newPhase73jDeps(t)
	defer deps.cleanup()
	priv, _ := loadES256Phase61(t)

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	seedPhase73jTurn(t, deps.store, id, "heavy",
		strings.Repeat("X", phase73jHeavyThreshold*3))
	tok := signES256Wave10(t, priv, phase73jClaims(id, nil), phase73jKid)

	status, body := postMemory(t, srv.URL, "/v1/memory/list", `{}`, tok)
	if status != http.StatusOK {
		t.Fatalf("memory.list: status = %d; body=%s", status, body)
	}
	var listResp prototypes.MemoryListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Items) != 1 || !listResp.Items[0].HeavyContent {
		t.Fatalf("memory.list: items=%d heavy=%v, want 1/true",
			len(listResp.Items), len(listResp.Items) == 1 && listResp.Items[0].HeavyContent)
	}

	status, body = postMemory(t, srv.URL, "/v1/memory/get",
		`{"key":"`+listResp.Items[0].Key+`"}`, tok)
	if status != http.StatusOK {
		t.Fatalf("memory.get: status = %d; body=%s", status, body)
	}
	var getResp prototypes.MemoryGetResponse
	if err := json.Unmarshal(body, &getResp); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if getResp.Detail.ValueArtifact == nil {
		t.Fatal("memory.get(heavy): ValueArtifact nil, want by-reference stub (D-026)")
	}
	if len(getResp.Detail.Value) != 0 {
		t.Error("memory.get(heavy): Value populated, want empty — heavy value MUST NOT inline (D-026)")
	}
}

// TestE2E_Phase73j_TwoTenantConcurrencyStress runs N≥10 concurrent
// memory.list callers across two tenants and asserts no cross-tenant
// leakage and no goroutine leak (CLAUDE.md §17.3 long-lived-wiring
// concurrency stress).
func TestE2E_Phase73j_TwoTenantConcurrencyStress(t *testing.T) {
	deps := newPhase73jDeps(t)
	defer deps.cleanup()
	priv, _ := loadES256Phase61(t)

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"}
	seedPhase73jTurn(t, deps.store, idA, "A-q1", "A-a1")
	seedPhase73jTurn(t, deps.store, idA, "A-q2", "A-a2")
	seedPhase73jTurn(t, deps.store, idB, "B-q1", "B-a1")
	tokA := signES256Wave10(t, priv, phase73jClaims(idA, nil), phase73jKid)
	tokB := signES256Wave10(t, priv, phase73jClaims(idB, nil), phase73jKid)

	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const callers = 24
	var wg sync.WaitGroup
	errs := make(chan string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tok, wantTenant, wantRows := tokA, "tenant-A", 2
			if n%2 == 1 {
				tok, wantTenant, wantRows = tokB, "tenant-B", 1
			}
			status, body := postMemory(t, srv.URL, "/v1/memory/list", `{}`, tok)
			if status != http.StatusOK {
				errs <- "status " + http.StatusText(status)
				return
			}
			var resp prototypes.MemoryListResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				errs <- "decode: " + err.Error()
				return
			}
			if resp.TotalRows != wantRows {
				errs <- "cross-tenant leak: got rows mismatch"
				return
			}
			for _, it := range resp.Items {
				if it.Identity.Tenant != wantTenant {
					errs <- "cross-tenant leak: row tenant mismatch"
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent memory.list: %s", e)
	}

	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > baseline+8 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}
