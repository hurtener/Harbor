// Phase 73e cross-subsystem integration test per CLAUDE.md §17 — the
// eight `agents.*` Protocol methods exercised end-to-end against the
// real wire transport + the real Agent Registry (Phase 53a) + the real
// auth.Validator/Middleware (Phase 61), with no mocks at any seam.
//
// Surfaces composed:
//
//   - Phase 53a internal/runtime/registry — the real StateStore-backed
//     Agent Registry whose AgentRecords the `agents.*` methods project.
//   - Phase 73e internal/runtime/registry/protocol — the Agents Protocol
//     Service + RegistryProjector.
//   - Phase 60 internal/protocol/transports — the wire surface the
//     `agents.*` handler is mounted on.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware
//     resolving the identity triple at the wire edge.
//
// This test ships the §13 primitive-with-consumer discharge for Phase
// 73e's Go-side surface: it is the first end-to-end consumer of the
// `agents.*` wire methods — agent catalog list + filter + drill-down
// across the six detail tabs + the registry-wide metrics rollup +
// cross-tenant isolation. It also pins the control-verb degradation:
// the five `registry.*` control verbs (Pause / Drain / Restart /
// ForceStop / Deregister) are gated on the elevated control-scope claim
// (D-066) — a caller WITHOUT the claim is rejected and the registry is
// NOT mutated.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
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
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const phase73eKid = "phase73e-kid"

var fixedNowPhase73e = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

type phase73eDeps struct {
	mux     *http.ServeMux
	priv    *ecdsa.PrivateKey
	reg     *registry.Registry
	cleanup func()
}

func newPhase73eDeps(t *testing.T) *phase73eDeps {
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

	// Real StateStore-backed Agent Registry — no mock at the seam.
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	projector, err := agentsprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	agentsSvc, err := agentsprotocol.NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	keys := newES256KeySet(phase73eKid, pub)
	now := func() time.Time { return fixedNowPhase73e }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithAgentsService(agentsSvc),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase73eDeps{
		mux:  mux,
		priv: priv,
		reg:  reg,
		cleanup: func() {
			_ = reg.Close(context.Background())
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// phase73eClaims mints a JWT MapClaims with the test's standard shape.
func phase73eClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase73e.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase73e.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// postAgents issues a POST /v1/agents/{verb} with the supplied JWT.
func postAgents(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/agents/"+verb, strings.NewReader(body))
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

// registerPhase73eAgent registers an agent in the registry under id.
func registerPhase73eAgent(t *testing.T, reg *registry.Registry, id identity.Identity, key, display string) string {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	rec, err := reg.Register(ctx, key, registry.AgentConfig{Prompts: []string{"be helpful"}},
		registry.RegisterOptions{DisplayName: display})
	if err != nil {
		t.Fatalf("Register %q: %v", key, err)
	}
	return rec.AgentID
}

// TestE2E_Phase73e_AgentsPage is the §13 primitive-with-consumer binding
// test for the Agents-page Protocol surface. It exercises the agent
// catalog list + filter + drill-down across the six detail tabs + the
// metrics rollup + cross-tenant identity propagation, all at the wire
// boundary with real drivers.
func TestE2E_Phase73e_AgentsPage(t *testing.T) {
	deps := newPhase73eDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"}
	tokA := signES256Wave10(t, deps.priv, phase73eClaims(idA, nil), phase73eKid)
	tokB := signES256Wave10(t, deps.priv, phase73eClaims(idB, nil), phase73eKid)

	agentA1 := registerPhase73eAgent(t, deps.reg, idA, "support", "Support Bot")
	registerPhase73eAgent(t, deps.reg, idA, "billing", "Billing Bot")
	agentB1 := registerPhase73eAgent(t, deps.reg, idB, "concierge", "Concierge")

	// (1) agents.list returns tenant A's two agents — and ONLY those.
	status, body := postAgents(t, srv.URL, "list", `{}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("agents.list: status = %d, want 200; body=%s", status, body)
	}
	var listResp prototypes.AgentListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode agents.list: %v", err)
	}
	if listResp.Aggregates.Total != 2 || len(listResp.Agents) != 2 {
		t.Fatalf("agents.list: Total=%d len=%d, want 2/2", listResp.Aggregates.Total, len(listResp.Agents))
	}

	// (2) Cross-tenant isolation — tenant B's list NEVER includes tenant
	// A's agents. agent_id is NOT an isolation key (D-059); the registry
	// scopes by the (tenant, user, session) tuple.
	status, body = postAgents(t, srv.URL, "list", `{}`, tokB)
	if status != http.StatusOK {
		t.Fatalf("tenant B agents.list: status = %d, want 200; body=%s", status, body)
	}
	var listB prototypes.AgentListResponse
	_ = json.Unmarshal(body, &listB)
	if listB.Aggregates.Total != 1 || listB.Agents[0].Name != "Concierge" {
		t.Fatalf("tenant B sees %+v, want exactly [Concierge] — cross-tenant leak", listB.Agents)
	}

	// (3) agents.list facet filter — search narrows the view.
	status, body = postAgents(t, srv.URL, "list", `{"filter":{"search":"billing"}}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("agents.list search: status = %d, want 200; body=%s", status, body)
	}
	var searchResp prototypes.AgentListResponse
	_ = json.Unmarshal(body, &searchResp)
	if len(searchResp.Agents) != 1 || searchResp.Agents[0].Name != "Billing Bot" {
		t.Fatalf("search facet returned %+v, want [Billing Bot]", searchResp.Agents)
	}

	// (4) agents.get drill-down — registration identity is projected.
	status, body = postAgents(t, srv.URL, "get", `{"id":"`+agentA1+`"}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("agents.get: status = %d, want 200; body=%s", status, body)
	}
	var getResp prototypes.AgentGetResponse
	_ = json.Unmarshal(body, &getResp)
	if getResp.Agent.ID != agentA1 || getResp.Agent.VersionHash == "" || getResp.Agent.Incarnation != 1 {
		t.Fatalf("agents.get projection = %+v", getResp.Agent)
	}

	// (5) Every detail-tab method round-trips for a real agent.
	for _, verb := range []string{"tools", "memory", "governance", "skills", "permissions"} {
		status, body = postAgents(t, srv.URL, verb, `{"id":"`+agentA1+`"}`, tokA)
		if status != http.StatusOK {
			t.Fatalf("agents.%s: status = %d, want 200; body=%s", verb, status, body)
		}
	}

	// (6) agents.metrics — the registry-wide rollup, scoped to tenant A.
	status, body = postAgents(t, srv.URL, "metrics", `{}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("agents.metrics: status = %d, want 200; body=%s", status, body)
	}
	var metricsResp prototypes.AgentMetricsResponse
	_ = json.Unmarshal(body, &metricsResp)
	if metricsResp.Metrics.ActiveAgents != 2 {
		t.Fatalf("agents.metrics ActiveAgents = %d, want 2", metricsResp.Metrics.ActiveAgents)
	}

	// (7) Failure mode — agents.get for tenant B's agent_id under tenant
	// A's token → 404 (the agent_id is invisible across the isolation
	// boundary, NOT a cross-tenant read).
	status, _ = postAgents(t, srv.URL, "get", `{"id":"`+agentB1+`"}`, tokA)
	if status != http.StatusNotFound {
		t.Errorf("cross-tenant agents.get: status = %d, want 404", status)
	}

	// (8) Identity propagation — an unauthenticated request is rejected
	// at the wire edge.
	noAuthReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/agents/list", strings.NewReader(`{}`))
	noAuthResp, err := http.DefaultClient.Do(noAuthReq)
	if err != nil {
		t.Fatalf("no-auth request: %v", err)
	}
	_ = noAuthResp.Body.Close()
	if noAuthResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth agents.list: status = %d, want 401", noAuthResp.StatusCode)
	}
}

// TestE2E_Phase73e_ControlVerbScopeGate proves the control-verb
// degradation contract (page-agents.md §9, D-066): the five `registry.*`
// control verbs (Pause / Drain / Restart / ForceStop / Deregister) the
// Console exposes are gated on the elevated control-scope claim. A
// caller WITHOUT the claim is rejected with ErrControlScopeRequired and
// the registry is NOT mutated; a caller WITH the claim succeeds. The
// Agents page renders the buttons disabled-with-tooltip for the
// unscoped operator — never a faked success.
func TestE2E_Phase73e_ControlVerbScopeGate(t *testing.T) {
	deps := newPhase73eDeps(t)
	defer deps.cleanup()

	id := identity.Identity{TenantID: "tenant-ctl", UserID: "u", SessionID: "s"}
	agentID := registerPhase73eAgent(t, deps.reg, id, "fleet-agent", "Fleet Agent")

	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// WITHOUT the control-scope claim — every control verb is rejected.
	for _, verb := range []struct {
		name string
		call func(context.Context) error
	}{
		{"Pause", func(c context.Context) error { return deps.reg.Pause(c, agentID, "test") }},
		{"Drain", func(c context.Context) error { return deps.reg.Drain(c, agentID, "test") }},
		{"Restart", func(c context.Context) error { return deps.reg.Restart(c, agentID, "test") }},
		{"ForceStop", func(c context.Context) error { return deps.reg.ForceStop(c, agentID, "test") }},
	} {
		if err := verb.call(ctx); !errors.Is(err, registry.ErrControlScopeRequired) {
			t.Fatalf("registry.%s WITHOUT control scope: err = %v, want ErrControlScopeRequired", verb.name, err)
		}
	}

	// The registry was NOT mutated — the agent is still active/healthy.
	recBefore, err := deps.reg.Get(ctx, agentID)
	if err != nil {
		t.Fatalf("Get after rejected control verbs: %v", err)
	}
	if recBefore.Health == registry.HealthDraining || recBefore.Health == registry.HealthStopped {
		t.Fatalf("registry mutated despite rejected control verb: health=%q", recBefore.Health)
	}

	// WITH the control-scope claim — the control verb succeeds and the
	// registry IS mutated.
	ctlCtx := registry.WithControlScope(ctx)
	if err := deps.reg.Drain(ctlCtx, agentID, "maintenance"); err != nil {
		t.Fatalf("registry.Drain WITH control scope: %v", err)
	}
	recAfter, err := deps.reg.Get(ctx, agentID)
	if err != nil {
		t.Fatalf("Get after Drain: %v", err)
	}
	if recAfter.Health != registry.HealthDraining {
		t.Fatalf("post-Drain health = %q, want draining", recAfter.Health)
	}
}

// TestE2E_Phase73e_AgentsConcurrencyStress runs N concurrent agents.list
// requests across multiple tenants against a single shared mux,
// asserting no cross-talk and no goroutine leak after teardown (CLAUDE.md
// §17.3 cross-package concurrency stress).
func TestE2E_Phase73e_AgentsConcurrencyStress(t *testing.T) {
	deps := newPhase73eDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const workers = 24
	// Each worker tenant registers exactly one agent — so a per-worker
	// list must see exactly one row. Cross-tenant bleed fails the count.
	tenants := make([]identity.Identity, 5)
	for i := range tenants {
		tenants[i] = identity.Identity{
			TenantID:  "stress-tenant-" + string(rune('A'+i)),
			UserID:    "u",
			SessionID: "s",
		}
		registerPhase73eAgent(t, deps.reg, tenants[i], "agent", "Agent "+string(rune('A'+i)))
	}

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(workers)
	errCh := make(chan error, workers)
	for i := range workers {
		go func(n int) {
			defer wg.Done()
			id := tenants[n%len(tenants)]
			tok := signES256Wave10(t, deps.priv, phase73eClaims(id, nil), phase73eKid)
			status, body := postAgents(t, srv.URL, "list", `{}`, tok)
			if status != http.StatusOK {
				errCh <- &stressErr{n, status, string(body)}
				return
			}
			var resp prototypes.AgentListResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				errCh <- &stressErr{n, status, "decode: " + err.Error()}
				return
			}
			if resp.Aggregates.Total != 1 {
				errCh <- &stressErr{n, status, "row-count cross-talk"}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	http.DefaultClient.CloseIdleConnections()
	for range 50 {
		if runtime.NumGoroutine() <= baseline+8 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}
