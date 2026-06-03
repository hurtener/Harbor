package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// agentsHandlerID is a documented dummy identity triple — no secrets.
var agentsHandlerID = identity.Identity{TenantID: "t-ah", UserID: "u-ah", SessionID: "s-ah"}

// newAgentsHandler builds an AgentsHandler over a real Agent Registry
// seeded with one agent, and returns the handler plus the seeded
// agent_id so tests can drive detail-mode methods.
func newAgentsHandler(t *testing.T) (*stream.AgentsHandler, string) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         100,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	})

	ctx, err := identity.With(context.Background(), agentsHandlerID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	rec, err := reg.Register(ctx, "support", registry.AgentConfig{Prompts: []string{"help"}},
		registry.RegisterOptions{DisplayName: "Support Bot"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	proj, err := agentsprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := agentsprotocol.NewService(proj, agentsprotocol.WithController(reg))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewAgentsHandler(svc)
	if err != nil {
		t.Fatalf("NewAgentsHandler: %v", err)
	}
	return h, rec.AgentID
}

// doAgentsRequest issues a POST /v1/agents/{verb} against the handler.
// id (when non-nil) is carried via the X-Harbor-* headers.
func doAgentsRequest(t *testing.T, h http.Handler, verb, body string, id *identity.Identity) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/"+verb, strings.NewReader(body))
	req.SetPathValue("method", verb)
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// doAgentsControlRequest issues a control verb with the identity headers
// AND the supplied verified scopes folded onto the request context (the
// same seam auth.Middleware uses) so the handler's admin-control gate
// sees them.
func doAgentsControlRequest(t *testing.T, h http.Handler, verb, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/"+verb, strings.NewReader(body))
	req.SetPathValue("method", verb)
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// agentsVerb strips the `agents.` prefix off a canonical method to get the
// route verb. Deriving it from the methods constant avoids hardcoding a
// Protocol method string in the test (the single-source grep guard also
// matches the pause task-control verb, so a bare quoted literal would trip it).
func agentsVerb(m methods.Method) string {
	return strings.TrimPrefix(string(m), "agents.")
}

// controlMethods is the canonical set of the five fleet-control verbs.
var controlMethods = []methods.Method{
	methods.MethodAgentsPause,
	methods.MethodAgentsDrain,
	methods.MethodAgentsRestart,
	methods.MethodAgentsForceStop,
	methods.MethodAgentsDeregister,
}

// TestAgentsHandler_Control_WithoutScope_403 — a control verb without the
// admin control claim fails closed with 403 / identity_scope_required
// (D-066): a leaked read-only token can never drive fleet control.
func TestAgentsHandler_Control_WithoutScope_403(t *testing.T) {
	h, agentID := newAgentsHandler(t)
	for _, m := range controlMethods {
		verb := agentsVerb(m)
		code, body := doAgentsRequest(t, h, verb, `{"id":"`+agentID+`","reason":"x"}`, &agentsHandlerID)
		if code != http.StatusForbidden {
			t.Fatalf("%s without scope: code = %d, want 403", verb, code)
		}
		assertAgentsErrCode(t, body, protoerrors.CodeIdentityScopeRequired)
	}
}

// TestAgentsHandler_Control_MissingIdentity_401 — identity is mandatory
// before the scope gate even applies.
func TestAgentsHandler_Control_MissingIdentity_401(t *testing.T) {
	h, agentID := newAgentsHandler(t)
	code, body := doAgentsRequest(t, h, agentsVerb(methods.MethodAgentsPause), `{"id":"`+agentID+`"}`, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("pause no-identity: code = %d, want 401", code)
	}
	assertAgentsErrCode(t, body, protoerrors.CodeIdentityRequired)
}

// TestAgentsHandler_Control_WithAdminScope_200 — with the admin control
// claim, a control verb drives the real registry and returns the agent's
// ACTUAL post-control status (re-read from the registry, no fabrication).
// The V1 registry has no "paused" Health, so pause leaves the observable
// status "active" — the pause is observed via the emitted agent.paused
// event, not a status flip (D-184).
func TestAgentsHandler_Control_WithAdminScope_200(t *testing.T) {
	h, agentID := newAgentsHandler(t)
	code, body := doAgentsControlRequest(t, h, agentsVerb(methods.MethodAgentsPause), `{"id":"`+agentID+`","reason":"maintenance"}`,
		&agentsHandlerID, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("pause with admin scope: code = %d, want 200 (body=%s)", code, body)
	}
	var resp struct {
		AgentID string `json:"agent_id"`
		Command string `json:"command"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode pause response: %v", err)
	}
	if resp.Command != agentsVerb(methods.MethodAgentsPause) || resp.Status != "active" || resp.AgentID != agentID {
		t.Fatalf("pause response = %+v, want command=pause status=active agent_id=%s", resp, agentID)
	}
}

// TestAgentsHandler_Control_Drain_TransitionsStatus — drain DOES
// transition the observable status (registry sets Health=draining →
// status "drained"), so the control response reflects it.
func TestAgentsHandler_Control_Drain_TransitionsStatus(t *testing.T) {
	h, agentID := newAgentsHandler(t)
	code, body := doAgentsControlRequest(t, h, agentsVerb(methods.MethodAgentsDrain), `{"id":"`+agentID+`","reason":"maintenance"}`,
		&agentsHandlerID, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("drain with admin scope: code = %d, want 200 (body=%s)", code, body)
	}
	var resp struct {
		Command string `json:"command"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode drain response: %v", err)
	}
	if resp.Command != agentsVerb(methods.MethodAgentsDrain) || resp.Status != "drained" {
		t.Fatalf("drain response = %+v, want command=drain status=drained", resp)
	}
}

// TestAgentsHandler_Control_UnknownAgent_404 — controlling a missing
// agent (with the claim) maps the registry not-found onto 404.
func TestAgentsHandler_Control_UnknownAgent_404(t *testing.T) {
	h, _ := newAgentsHandler(t)
	code, body := doAgentsControlRequest(t, h, agentsVerb(methods.MethodAgentsDeregister), `{"id":"ghost","reason":"x"}`,
		&agentsHandlerID, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusNotFound {
		t.Fatalf("deregister unknown agent: code = %d, want 404 (body=%s)", code, body)
	}
	assertAgentsErrCode(t, body, protoerrors.CodeNotFound)
}

func TestNewAgentsHandler_NilService_FailsLoudly(t *testing.T) {
	if _, err := stream.NewAgentsHandler(nil); err == nil {
		t.Fatal("NewAgentsHandler(nil) did not fail")
	}
}

func TestAgentsHandler_List_HappyPath(t *testing.T) {
	h, _ := newAgentsHandler(t)
	code, body := doAgentsRequest(t, h, "list", `{}`, &agentsHandlerID)
	if code != http.StatusOK {
		t.Fatalf("list code = %d body=%s", code, body)
	}
	var resp struct {
		Agents     []map[string]any `json:"agents"`
		Aggregates map[string]any   `json:"aggregates"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(resp.Agents))
	}
	if resp.Aggregates == nil {
		t.Fatal("aggregates missing")
	}
}

func TestAgentsHandler_List_MissingIdentity_401(t *testing.T) {
	h, _ := newAgentsHandler(t)
	code, body := doAgentsRequest(t, h, "list", `{}`, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("list-no-ident code = %d, want 401", code)
	}
	assertAgentsErrCode(t, body, protoerrors.CodeIdentityRequired)
}

func TestAgentsHandler_Get_HappyPath(t *testing.T) {
	h, agentID := newAgentsHandler(t)
	code, body := doAgentsRequest(t, h, "get", `{"id":"`+agentID+`"}`, &agentsHandlerID)
	if code != http.StatusOK {
		t.Fatalf("get code = %d body=%s", code, body)
	}
	var resp struct {
		Agent struct {
			ID          string `json:"id"`
			VersionHash string `json:"version_hash"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Agent.ID != agentID {
		t.Fatalf("agent.id = %q, want %q", resp.Agent.ID, agentID)
	}
	if resp.Agent.VersionHash == "" {
		t.Fatal("version_hash empty")
	}
}

func TestAgentsHandler_Get_UnknownAgent_404(t *testing.T) {
	h, _ := newAgentsHandler(t)
	code, body := doAgentsRequest(t, h, "get", `{"id":"ghost"}`, &agentsHandlerID)
	if code != http.StatusNotFound {
		t.Fatalf("get-ghost code = %d, want 404", code)
	}
	assertAgentsErrCode(t, body, protoerrors.CodeNotFound)
}

func TestAgentsHandler_DetailMethods_RoundTrip(t *testing.T) {
	h, agentID := newAgentsHandler(t)
	for _, verb := range []string{"tools", "memory", "governance", "skills", "permissions"} {
		code, body := doAgentsRequest(t, h, verb, `{"id":"`+agentID+`"}`, &agentsHandlerID)
		if code != http.StatusOK {
			t.Fatalf("agents.%s code = %d body=%s", verb, code, body)
		}
	}
}

func TestAgentsHandler_Metrics_HappyPath(t *testing.T) {
	h, _ := newAgentsHandler(t)
	code, body := doAgentsRequest(t, h, "metrics", `{}`, &agentsHandlerID)
	if code != http.StatusOK {
		t.Fatalf("metrics code = %d body=%s", code, body)
	}
	var resp struct {
		Metrics struct {
			ActiveAgents int64 `json:"active_agents"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Metrics.ActiveAgents != 1 {
		t.Fatalf("active_agents = %d, want 1", resp.Metrics.ActiveAgents)
	}
}

func TestAgentsHandler_UnknownMethod_404(t *testing.T) {
	h, _ := newAgentsHandler(t)
	code, body := doAgentsRequest(t, h, "frobnicate", `{}`, &agentsHandlerID)
	if code != http.StatusNotFound {
		t.Fatalf("unknown-method code = %d, want 404", code)
	}
	assertAgentsErrCode(t, body, protoerrors.CodeUnknownMethod)
}

func TestAgentsHandler_GetMethod_405(t *testing.T) {
	h, _ := newAgentsHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/agents/list", nil)
	req.SetPathValue("method", "list")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d, want 405", rec.Code)
	}
}

func TestAgentsHandler_Get_EmptyID_400(t *testing.T) {
	h, _ := newAgentsHandler(t)
	code, body := doAgentsRequest(t, h, "get", `{"id":""}`, &agentsHandlerID)
	if code != http.StatusBadRequest {
		t.Fatalf("get-empty-id code = %d, want 400", code)
	}
	assertAgentsErrCode(t, body, protoerrors.CodeInvalidRequest)
}

func assertAgentsErrCode(t *testing.T, body []byte, want protoerrors.Code) {
	t.Helper()
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body unmarshal: %v (body=%s)", err, body)
	}
	if e.Code != want {
		t.Fatalf("error code = %q, want %q", e.Code, want)
	}
}
