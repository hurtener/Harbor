package stream_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

func flowsPassthrough(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return in, nil
}

func flowsFixtureDef(name string) flow.Definition {
	return flow.Definition{
		Name:  name,
		Entry: "a",
		Exit:  "b",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Name: "a", Func: flowsPassthrough, To: []flow.NodeID{"b"}},
			"b": {Name: "b", Func: flowsPassthrough},
		},
		Budget:    flow.Budget{Deadline: time.Minute, HopBudget: 8, CostCap: 2.0},
		InSchema:  json.RawMessage(`{}`),
		OutSchema: json.RawMessage(`{}`),
	}
}

// newFlowsHandler builds a FlowsHandler over a registry seeded with one
// flow + one t1 run.
func newFlowsHandler(t *testing.T) http.Handler {
	t.Helper()
	registry := flow.NewRegistry()
	if err := registry.Register(flowsFixtureDef("flow-demo"), flow.Metadata{
		Owner: "team", PlannerFamily: "graph", Source: "internal/flows/demo.go",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.RecordRun(flow.RunRecord{
		FlowName: "flow-demo", RunID: "demo-run",
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Status:   "succeeded", Trigger: "user", StartedAt: time.Now(),
		Duration: 90 * time.Millisecond, Output: "ok",
	}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	store, err := artifactsinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	cat, err := flowprotocol.NewRegistryCatalog(registry, store, 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	launch := func(_ context.Context, id identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
		return "new-run-" + flowID, time.Now(), nil
	}
	inv, err := flowprotocol.NewFuncInvoker(launch, registry)
	if err != nil {
		t.Fatalf("NewFuncInvoker: %v", err)
	}
	surface, err := flowprotocol.NewSurface(cat, inv)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	h, err := stream.NewFlowsHandler(surface)
	if err != nil {
		t.Fatalf("NewFlowsHandler: %v", err)
	}
	return h
}

func doFlowsRequest(t *testing.T, h http.Handler, path, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

var flowsID = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

// captureBus is a minimal events.EventBus that records published event
// types — a *_test.go fixture for the audit-emit assertion.
type captureBus struct {
	mu  sync.Mutex
	evs []events.EventType
}

func (b *captureBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.evs = append(b.evs, ev.Type)
	return nil
}

func (b *captureBus) Subscribe(_ context.Context, _ events.Filter) (events.Subscription, error) {
	return nil, errors.New("captureBus: Subscribe unsupported")
}

func (b *captureBus) Close(_ context.Context) error { return nil }

func (b *captureBus) events() []events.EventType {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]events.EventType, len(b.evs))
	copy(out, b.evs)
	return out
}

func TestNewFlowsHandler_NilSurfaceFailsLoud(t *testing.T) {
	if _, err := stream.NewFlowsHandler(nil); err == nil {
		t.Fatal("NewFlowsHandler(nil): expected error, got nil")
	}
}

func TestFlows_List_HappyPath(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/list", `{}`, &flowsID, nil)
	if status != http.StatusOK {
		t.Fatalf("flows/list status = %d, body = %s", status, body)
	}
	var resp prototypes.FlowListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Flows) != 1 || resp.Flows[0].ID != "flow-demo" {
		t.Fatalf("flows/list = %+v", resp.Flows)
	}
}

func TestFlows_List_IdentityMandatory(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/list", `{}`, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("flows/list (no identity) status = %d, want 401, body = %s", status, body)
	}
}

func TestFlows_List_CrossTenantWithoutAdmin403(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/list",
		`{"filter":{"tenants":["t1","t-other"]}}`, &flowsID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("flows/list (cross-tenant) status = %d, want 403, body = %s", status, body)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("flows/list (cross-tenant) code = %q, want %q", e.Code, protoerrors.CodeIdentityScopeRequired)
	}
}

func TestFlows_List_CrossTenantWithAdminAllowed(t *testing.T) {
	h := newFlowsHandler(t)
	status, _ := doFlowsRequest(t, h, "/v1/flows/list",
		`{"filter":{"tenants":["t1","t-other"]}}`, &flowsID, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("flows/list (cross-tenant, admin) status = %d, want 200", status)
	}
}

func TestFlows_Describe_HappyPath(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/describe",
		`{"id":"flow-demo"}`, &flowsID, nil)
	if status != http.StatusOK {
		t.Fatalf("flows/describe status = %d, body = %s", status, body)
	}
	var resp prototypes.FlowDescription
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 2 || len(resp.Edges) != 1 {
		t.Fatalf("flows/describe graph = (%d,%d)", len(resp.Nodes), len(resp.Edges))
	}
}

func TestFlows_Describe_NotFound(t *testing.T) {
	h := newFlowsHandler(t)
	status, _ := doFlowsRequest(t, h, "/v1/flows/describe",
		`{"id":"ghost"}`, &flowsID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("flows/describe (ghost) status = %d, want 404", status)
	}
}

func TestFlows_RunsList_HappyPath(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/runs/list",
		`{"flow_id":"flow-demo"}`, &flowsID, nil)
	if status != http.StatusOK {
		t.Fatalf("flows/runs/list status = %d, body = %s", status, body)
	}
	var resp prototypes.FlowRunsListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs) != 1 || resp.Runs[0].RunID != "demo-run" {
		t.Fatalf("flows/runs/list = %+v", resp.Runs)
	}
}

func TestFlows_RunsDescribe_HappyPath(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/runs/describe",
		`{"run_id":"demo-run"}`, &flowsID, nil)
	if status != http.StatusOK {
		t.Fatalf("flows/runs/describe status = %d, body = %s", status, body)
	}
	var resp prototypes.FlowRunDescription
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Run.RunID != "demo-run" {
		t.Fatalf("flows/runs/describe run = %+v", resp.Run)
	}
}

func TestFlows_Run_RequiresAdminScope(t *testing.T) {
	h := newFlowsHandler(t)
	// Without the admin scope claim → 403 CodeScopeMismatch.
	status, body := doFlowsRequest(t, h, "/v1/flows/run",
		`{"flow_id":"flow-demo","inputs":{}}`, &flowsID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("flows/run (no scope) status = %d, want 403, body = %s", status, body)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("flows/run (no scope) code = %q, want %q", e.Code, protoerrors.CodeScopeMismatch)
	}
}

func TestFlows_Run_WithAdminScopeAccepted(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/run",
		`{"flow_id":"flow-demo","inputs":{"k":"v"}}`, &flowsID, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("flows/run (admin) status = %d, want 200, body = %s", status, body)
	}
	var resp prototypes.FlowRunResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RunID != "new-run-flow-demo" {
		t.Fatalf("flows/run RunID = %q", resp.RunID)
	}
}

func TestFlows_Metrics_HappyPath(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/metrics",
		`{"flow_id":"flow-demo"}`, &flowsID, nil)
	if status != http.StatusOK {
		t.Fatalf("flows/metrics status = %d, body = %s", status, body)
	}
	var resp prototypes.FlowMetrics
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.FlowID != "flow-demo" {
		t.Fatalf("flows/metrics FlowID = %q", resp.FlowID)
	}
}

func TestFlows_UnknownRouteSuffix404(t *testing.T) {
	h := newFlowsHandler(t)
	status, _ := doFlowsRequest(t, h, "/v1/flows/bogus", `{}`, &flowsID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("flows/bogus status = %d, want 404", status)
	}
}

func TestFlows_RejectsGET(t *testing.T) {
	h := newFlowsHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/flows/list", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("flows GET status = %d, want 405", rec.Code)
	}
}

func TestFlows_BodyIdentityMismatchRejected(t *testing.T) {
	h := newFlowsHandler(t)
	// Body claims a different tenant than the verified identity.
	status, _ := doFlowsRequest(t, h, "/v1/flows/list",
		`{"identity":{"tenant":"t-evil","user":"u1","session":"s1"}}`, &flowsID, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("flows/list (body identity mismatch) status = %d, want 401", status)
	}
}

func TestFlows_MalformedBodyRejected(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/describe",
		`{not json`, &flowsID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("flows/describe (malformed body) status = %d, want 400, body = %s", status, body)
	}
}

func TestFlows_RunsList_CrossTenantWithoutAdmin403(t *testing.T) {
	h := newFlowsHandler(t)
	status, body := doFlowsRequest(t, h, "/v1/flows/runs/list",
		`{"flow_id":"flow-demo","tenants":["t-other"]}`, &flowsID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("flows/runs/list (cross-tenant) status = %d, want 403, body = %s", status, body)
	}
}

func TestFlows_Metrics_EmptyFlowIDRejected(t *testing.T) {
	h := newFlowsHandler(t)
	status, _ := doFlowsRequest(t, h, "/v1/flows/metrics", `{}`, &flowsID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("flows/metrics (empty flow_id) status = %d, want 400", status)
	}
}

func TestFlows_RunsDescribe_NotFound(t *testing.T) {
	h := newFlowsHandler(t)
	status, _ := doFlowsRequest(t, h, "/v1/flows/runs/describe",
		`{"run_id":"ghost"}`, &flowsID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("flows/runs/describe (ghost) status = %d, want 404", status)
	}
}

// TestFlows_WithBus_EmitsAuditEvents proves the handler emits the
// per-page audit events onto a wired bus.
func TestFlows_WithBus_EmitsAuditEvents(t *testing.T) {
	registry := flow.NewRegistry()
	if err := registry.Register(flowsFixtureDef("flow-bus"), flow.Metadata{
		PlannerFamily: "graph",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	store, err := artifactsinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	cat, err := flowprotocol.NewRegistryCatalog(registry, store, 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	inv, err := flowprotocol.NewFuncInvoker(
		func(_ context.Context, _ identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
			return "run-" + flowID, time.Now(), nil
		}, registry)
	if err != nil {
		t.Fatalf("NewFuncInvoker: %v", err)
	}
	surface, err := flowprotocol.NewSurface(cat, inv)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	bus := &captureBus{}
	h, err := stream.NewFlowsHandler(surface, stream.WithFlowsBus(bus), stream.WithFlowsLogger(nil))
	if err != nil {
		t.Fatalf("NewFlowsHandler: %v", err)
	}
	// A read dispatch emits flows.page_viewed.
	status, _ := doFlowsRequest(t, h, "/v1/flows/list", `{}`, &flowsID, nil)
	if status != http.StatusOK {
		t.Fatalf("flows/list status = %d", status)
	}
	// A run dispatch emits flows.run_invoked.
	status, _ = doFlowsRequest(t, h, "/v1/flows/run",
		`{"flow_id":"flow-bus","inputs":{}}`, &flowsID, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("flows/run status = %d", status)
	}
	var sawViewed, sawInvoked bool
	for _, ev := range bus.events() {
		if ev == stream.EventTypeFlowsPageViewed {
			sawViewed = true
		}
		if ev == stream.EventTypeFlowsRunInvoked {
			sawInvoked = true
		}
	}
	if !sawViewed {
		t.Error("flows.page_viewed event not emitted")
	}
	if !sawInvoked {
		t.Error("flows.run_invoked event not emitted")
	}
}
