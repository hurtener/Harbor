// Phase 73i cross-subsystem integration test per CLAUDE.md §17 — the
// Console Flows-page Protocol surface exercised end-to-end against the
// real wire transport + the real flow.Registry + the real
// auth.Validator/Middleware (Phase 61) + the real in-mem ArtifactStore
// + the real EventBus, with no mocks at any seam.
//
// Surfaces composed:
//
//   - Phase 26a internal/runtime/flow — the flow.Registry source-of-
//     truth the Flows-page Catalog projects.
//   - Phase 73i internal/runtime/flow/protocol — the transport-agnostic
//     Flows-page Surface.
//   - Phase 60 internal/protocol/transports — the wire surface the
//     six `POST /v1/flows/*` routes are mounted on.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware
//     gating the cross-tenant + `flows.run` `admin` scope claim (D-079).
//   - Phase 17/18/19 internal/artifacts — the ArtifactStore the D-026
//     heavy-content bypass routes oversized run outputs through.
//
// This test is the §13 primitive-with-consumer discharge for Phase 73i:
// it is the first end-to-end consumer of the six flows.* wire methods —
// two-tenant identity scope, the cross-tenant reject path without the
// admin claim, the `flows.run` reject path without the claim, the
// admin-claim accept path, and the D-026 heavy-output bypass.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const phase73iKid = "phase73i-kid"

var fixedNowPhase73i = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

const phase73iHeavyThreshold = 1024

type phase73iDeps struct {
	mux      *http.ServeMux
	registry *flow.Registry
	bus      events.EventBus
	priv     *ecdsa.PrivateKey
	cleanup  func()
}

func newPhase73iDeps(t *testing.T) *phase73iDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	bus, err := events.Open(t.Context(), config.EventsConfig{
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
	store, err := state.Open(t.Context(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(t.Context())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(t.Context(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(t.Context())
		_ = bus.Close(t.Context())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(t.Context())
		_ = store.Close(t.Context())
		_ = bus.Close(t.Context())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	artStore, err := artifacts.Open(t.Context(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		_ = taskReg.Close(t.Context())
		_ = store.Close(t.Context())
		_ = bus.Close(t.Context())
		t.Fatalf("artifacts.Open: %v", err)
	}

	// The real flow.Registry seeded with one flow + two-tenant run
	// history, plus one heavy-output run for the D-026 bypass.
	registry := flow.NewRegistry()
	seedPhase73iRegistry(t, registry)

	cat, err := flowprotocol.NewRegistryCatalog(registry, artStore, phase73iHeavyThreshold)
	if err != nil {
		_ = artStore.Close(t.Context())
		_ = taskReg.Close(t.Context())
		_ = store.Close(t.Context())
		_ = bus.Close(t.Context())
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	invoker, err := flowprotocol.NewFuncInvoker(
		func(_ context.Context, id identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
			return "run-" + flowID + "-" + id.TenantID, fixedNowPhase73i, nil
		}, registry)
	if err != nil {
		_ = artStore.Close(t.Context())
		_ = taskReg.Close(t.Context())
		_ = store.Close(t.Context())
		_ = bus.Close(t.Context())
		t.Fatalf("NewFuncInvoker: %v", err)
	}
	flowsSurface, err := flowprotocol.NewSurface(cat, invoker)
	if err != nil {
		_ = artStore.Close(t.Context())
		_ = taskReg.Close(t.Context())
		_ = store.Close(t.Context())
		_ = bus.Close(t.Context())
		t.Fatalf("flowprotocol.NewSurface: %v", err)
	}

	keys := newES256KeySet(phase73iKid, pub)
	now := func() time.Time { return fixedNowPhase73i }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		_ = artStore.Close(t.Context())
		_ = taskReg.Close(t.Context())
		_ = store.Close(t.Context())
		_ = bus.Close(t.Context())
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithFlows(flowsSurface),
	)
	if err != nil {
		_ = artStore.Close(t.Context())
		_ = taskReg.Close(t.Context())
		_ = store.Close(t.Context())
		_ = bus.Close(t.Context())
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase73iDeps{
		mux:      mux,
		registry: registry,
		bus:      bus,
		priv:     priv,
		cleanup: func() {
			_ = artStore.Close(t.Context())
			_ = taskReg.Close(t.Context())
			_ = store.Close(t.Context())
			_ = bus.Close(t.Context())
		},
	}
}

// seedPhase73iRegistry registers one flow + two-tenant run history + a
// heavy-output run.
func seedPhase73iRegistry(t *testing.T, r *flow.Registry) {
	t.Helper()
	if err := r.Register(phase73iFlowDef("flow-checkout"), flow.Metadata{
		Owner: "team-checkout", Version: "v3", PlannerFamily: "graph",
		Source: "internal/flows/checkout.go",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"}
	for i, id := range []identity.Identity{idA, idA, idB} {
		if err := r.RecordRun(flow.RunRecord{
			FlowName:  "flow-checkout",
			RunID:     "run-" + id.TenantID + "-" + string(rune('a'+i)),
			Identity:  id,
			Trigger:   "user",
			Status:    "succeeded",
			StartedAt: fixedNowPhase73i.Add(-time.Duration(i+1) * time.Hour),
			Duration:  120 * time.Millisecond,
			CostUSD:   0.5,
			NodeStates: []flow.NodeRunRecord{
				{NodeID: "a", Status: "succeeded", Duration: 50 * time.Millisecond},
			},
		}); err != nil {
			t.Fatalf("RecordRun: %v", err)
		}
	}
	// A heavy-output run under tenant-A for the D-026 bypass assertion.
	if err := r.RecordRun(flow.RunRecord{
		FlowName:  "flow-checkout",
		RunID:     "run-heavy-A",
		Identity:  idA,
		Trigger:   "user",
		Status:    "succeeded",
		StartedAt: fixedNowPhase73i.Add(-30 * time.Minute),
		Duration:  200 * time.Millisecond,
		Output:    strings.Repeat("z", 4096), // exceeds the 1024 threshold
	}); err != nil {
		t.Fatalf("RecordRun(heavy): %v", err)
	}
}

func phase73iPassthrough(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return in, nil
}

func phase73iFlowDef(name string) flow.Definition {
	return flow.Definition{
		Name:  name,
		Entry: "a",
		Exit:  "b",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Name: "a", Func: phase73iPassthrough, To: []flow.NodeID{"b"}},
			"b": {Name: "b", Func: phase73iPassthrough},
		},
		Budget:    flow.Budget{Deadline: time.Minute, HopBudget: 10, CostCap: 5.0},
		InSchema:  json.RawMessage(`{}`),
		OutSchema: json.RawMessage(`{}`),
	}
}

func phase73iClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase73i.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase73i.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

func postFlows(t *testing.T, srvURL, route, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+route, strings.NewReader(body))
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

// TestE2E_Phase73i_FlowsPageTwoTenantScope is the §13 primitive-with-
// consumer binding test for the Flows page surface.
func TestE2E_Phase73i_FlowsPageTwoTenantScope(t *testing.T) {
	deps := newPhase73iDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	tokA := signES256Wave10(t, deps.priv, phase73iClaims(idA, nil), phase73iKid)

	// (1) flows.list — catalog renders one flow; non-admin run aggregate
	//     is tenant-A-scoped (2 runs, not 3).
	status, body := postFlows(t, srv.URL, "/v1/flows/list", `{}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("flows.list: status = %d, body = %s", status, body)
	}
	var listResp prototypes.FlowListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Flows) != 1 {
		t.Fatalf("flows.list: got %d flows, want 1", len(listResp.Flows))
	}
	if listResp.Flows[0].Runs24h != 3 {
		// tenant-A has 2 hourly runs + 1 heavy run = 3 within 24h.
		t.Fatalf("flows.list: Runs24h = %d, want 3 (tenant-A own runs)", listResp.Flows[0].Runs24h)
	}

	// (2) flows.describe — engine graph nodes + edges.
	status, body = postFlows(t, srv.URL, "/v1/flows/describe", `{"id":"flow-checkout"}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("flows.describe: status = %d, body = %s", status, body)
	}
	var desc prototypes.FlowDescription
	if err := json.Unmarshal(body, &desc); err != nil {
		t.Fatalf("decode describe: %v", err)
	}
	if len(desc.Nodes) != 2 || len(desc.Edges) != 1 {
		t.Fatalf("flows.describe: graph = (%d nodes, %d edges), want (2,1)", len(desc.Nodes), len(desc.Edges))
	}

	// (3) flows.runs.list — tenant-A sees only its own runs.
	status, body = postFlows(t, srv.URL, "/v1/flows/runs/list", `{"flow_id":"flow-checkout"}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("flows.runs.list: status = %d, body = %s", status, body)
	}
	var runsResp prototypes.FlowRunsListResponse
	if err := json.Unmarshal(body, &runsResp); err != nil {
		t.Fatalf("decode runs.list: %v", err)
	}
	for _, run := range runsResp.Runs {
		if run.Identity.Tenant != "tenant-A" {
			t.Fatalf("flows.runs.list: leaked tenant %q", run.Identity.Tenant)
		}
	}

	// (4) flows.runs.describe — heavy output routed via ArtifactRef
	//     (D-026), never inline.
	status, body = postFlows(t, srv.URL, "/v1/flows/runs/describe", `{"run_id":"run-heavy-A"}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("flows.runs.describe: status = %d, body = %s", status, body)
	}
	var runDesc prototypes.FlowRunDescription
	if err := json.Unmarshal(body, &runDesc); err != nil {
		t.Fatalf("decode runs.describe: %v", err)
	}
	if runDesc.OutputPreview != "" {
		t.Fatal("flows.runs.describe: heavy output inlined — D-026 violation")
	}
	if runDesc.OutputRef == nil || runDesc.OutputRef.ID == "" {
		t.Fatalf("flows.runs.describe: heavy output not routed by-reference: %+v", runDesc.OutputRef)
	}

	// (5) flows.metrics — sparkline aggregates.
	status, body = postFlows(t, srv.URL, "/v1/flows/metrics", `{"flow_id":"flow-checkout"}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("flows.metrics: status = %d, body = %s", status, body)
	}
	var metrics prototypes.FlowMetrics
	if err := json.Unmarshal(body, &metrics); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	if metrics.FlowID != "flow-checkout" {
		t.Fatalf("flows.metrics: FlowID = %q", metrics.FlowID)
	}
}

// TestE2E_Phase73i_CrossTenantRejectedWithoutAdmin proves the cross-
// tenant catalog filter fails closed (403) without the admin claim.
func TestE2E_Phase73i_CrossTenantRejectedWithoutAdmin(t *testing.T) {
	deps := newPhase73iDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}

	// Non-admin cross-tenant filter → 403.
	tokA := signES256Wave10(t, deps.priv, phase73iClaims(idA, nil), phase73iKid)
	status, body := postFlows(t, srv.URL, "/v1/flows/list",
		`{"filter":{"tenants":["tenant-A","tenant-B"]}}`, tokA)
	if status != http.StatusForbidden {
		t.Fatalf("flows.list cross-tenant non-admin: status = %d, want 403; body=%s", status, body)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("flows.list cross-tenant: code = %q, want %q", e.Code, protoerrors.CodeIdentityScopeRequired)
	}

	// Admin cross-tenant filter → 200.
	tokAdmin := signES256Wave10(t, deps.priv, phase73iClaims(idA, []string{"admin"}), phase73iKid)
	status, _ = postFlows(t, srv.URL, "/v1/flows/list",
		`{"filter":{"tenants":["tenant-A","tenant-B"]}}`, tokAdmin)
	if status != http.StatusOK {
		t.Fatalf("flows.list cross-tenant admin: status = %d, want 200", status)
	}
}

// TestE2E_Phase73i_RunRejectedWithoutClaim proves `flows.run` fails
// closed (403) without the admin scope claim and is accepted with it.
func TestE2E_Phase73i_RunRejectedWithoutClaim(t *testing.T) {
	deps := newPhase73iDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}

	// Without the claim → 403 CodeScopeMismatch.
	tokA := signES256Wave10(t, deps.priv, phase73iClaims(idA, nil), phase73iKid)
	status, body := postFlows(t, srv.URL, "/v1/flows/run",
		`{"flow_id":"flow-checkout","inputs":{}}`, tokA)
	if status != http.StatusForbidden {
		t.Fatalf("flows.run no-claim: status = %d, want 403; body=%s", status, body)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("flows.run no-claim: code = %q, want %q", e.Code, protoerrors.CodeScopeMismatch)
	}

	// With the claim → 200.
	tokAdmin := signES256Wave10(t, deps.priv, phase73iClaims(idA, []string{"admin"}), phase73iKid)
	status, body = postFlows(t, srv.URL, "/v1/flows/run",
		`{"flow_id":"flow-checkout","inputs":{}}`, tokAdmin)
	if status != http.StatusOK {
		t.Fatalf("flows.run with-claim: status = %d, want 200; body=%s", status, body)
	}
	var runResp prototypes.FlowRunResponse
	if err := json.Unmarshal(body, &runResp); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if runResp.RunID != "run-flow-checkout-tenant-A" {
		t.Fatalf("flows.run: RunID = %q", runResp.RunID)
	}
}

// TestE2E_Phase73i_ConcurrentFlowsListNoCrossTalk runs N≥10 concurrent
// flows.list calls across two tenants against the shared mux, asserting
// no cross-talk: every response's run aggregate is the caller-tenant
// slice.
func TestE2E_Phase73i_ConcurrentFlowsListNoCrossTalk(t *testing.T) {
	deps := newPhase73iDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errCh := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			var id identity.Identity
			var wantRuns int64
			if g%2 == 0 {
				id = identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
				wantRuns = 3
			} else {
				id = identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"}
				wantRuns = 1
			}
			tok := signES256Wave10(t, deps.priv, phase73iClaims(id, nil), phase73iKid)
			status, body := postFlows(t, srv.URL, "/v1/flows/list", `{}`, tok)
			if status != http.StatusOK {
				errCh <- fmt.Errorf("goroutine %d: status %d", g, status)
				return
			}
			var resp prototypes.FlowListResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				errCh <- err
				return
			}
			if len(resp.Flows) != 1 || resp.Flows[0].Runs24h != wantRuns {
				errCh <- fmt.Errorf("goroutine %d (%s): Runs24h = %d, want %d — context bleed",
					g, id.TenantID, resp.Flows[0].Runs24h, wantRuns)
				return
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
