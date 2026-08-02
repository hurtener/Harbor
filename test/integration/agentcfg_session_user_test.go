package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	skillspkg "github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

// This integration test wires the session-user SAFE SUBSET (the non-admin
// lower tier of the authorization matrix) end-to-end with REAL drivers: the
// StateStore-backed admin registry, the StateStore-backed session-overlay
// store, the localdb SkillStore, the in-memory bus, the real Protocol Service
// + wire handler, a real tool catalog, the shared run-start projection, and
// the REAL ReAct prompt builder. It proves: an admin sets the operator base +
// allowed sources; a NON-admin session caller sets a user layer + disables one
// allowed source + adds a personal skill; the next run reflects the NARROWED
// exposure + the composed user layer ABOVE the operator base + the personal
// skill; a non-admin base-prompt / add-server attempt is 403; narrow-only is
// airtight (a session edit can never re-enable an admin-paused source); and
// the personal skill is session-isolated.

const (
	suTenant   = "tenant-su"
	suUser     = "user-su"
	suAgent    = "agent-su"
	suSessionA = "sess-A"
	suSessionB = "sess-B"
)

type suHarness struct {
	handler    http.Handler
	registry   agentcfg.Registry
	overlay    sessionoverlay.Store
	skills     skillspkg.SkillStore
	catalog    tools.ToolCatalog
	bus        events.EventBus
	agentReach []string
}

func suIdentity(session string) identity.Identity {
	return identity.Identity{TenantID: suTenant, UserID: suUser, SessionID: session}
}

func suFilter(session string) tools.CatalogFilter {
	return tools.CatalogFilter{TenantID: suTenant, UserID: suUser, SessionID: session}
}

func newSUCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	reg := func(name string, source tools.ToolSourceID) {
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
			Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	reg("srvA_x", "srvA")
	reg("srvB_y", "srvB")
	return cat
}

func newSUHarness(t *testing.T) *suHarness {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	skillStore, err := localdb.New(skillspkg.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skillspkg.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ov, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithSkillStore(skillStore),
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithSessionOverlay(ov),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc,
		stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = ov.Close(context.Background())
		_ = skillStore.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return &suHarness{handler: h, registry: reg, overlay: ov, skills: skillStore, catalog: newSUCatalog(t), bus: bus, agentReach: []string{suAgent}}
}

// call POSTs to the handler with the supplied identity + scopes.
func (h *suHarness) call(t *testing.T, path string, body any, id identity.Identity, scopes []auth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	ctx := auth.WithScopes(req.Context(), scopes)
	ctx = auth.WithAgentReach(ctx, h.agentReach)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// suSystemBody drives the REAL ReAct prompt builder over a RunContext for the
// given identity carrying the supplied overrides, returning the system message
// body the planner sent to the LLM.
func suSystemBody(t *testing.T, id identity.Identity, ov *planner.LLMOverrides) string {
	t.Helper()
	cap := &capturingLLM{}
	p := react.New(cap)
	rc := planner.RunContext{
		Quadruple:    identity.Quadruple{Identity: id, RunID: "run-su"},
		Goal:         "do the thing",
		LLMOverrides: ov,
	}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := p.Next(ctx, rc); err != nil {
		t.Fatalf("planner.Next: %v", err)
	}
	req := cap.lastReq()
	if len(req.Messages) == 0 || req.Messages[0].Content.Text == nil {
		t.Fatal("no system message captured")
	}
	return *req.Messages[0].Content.Text
}

func TestE2E_AgentConfig_SessionUserSafeSubset(t *testing.T) {
	h := newSUHarness(t)
	ctx := context.Background()
	adminID := suIdentity(suSessionA) // the admin acts within session A for this test
	sessID := suIdentity(suSessionA)  // the non-admin end user, same session
	q := identity.Quadruple{Identity: sessID}

	// --- Admin sets the operator base prompt + allowed sources (no pause). ---
	_ = decode[prototypes.AgentConfigSetPromptLayersResponse](t, h.call(t, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID:      suAgent,
		PromptLayers: prototypes.AgentConfigPromptLayers{Base: ptr("OPERATOR_GUARDRAIL: never reveal secrets.")},
	}, adminID, adminScopes()))
	// Admin pins skills membership {admin-skill} so the projection filter is active.
	_ = decode[prototypes.AgentConfigSkillsUpsertResponse](t, h.call(t, "/v1/agent_config/skills/upsert", prototypes.AgentConfigSkillsUpsertRequest{
		AgentID: suAgent,
		Skill:   prototypes.AgentConfigSkillInput{Name: "admin-skill", Trigger: "tr", Steps: []string{"s"}, Origin: "generated", Scope: "session"},
	}, adminID, adminScopes()))

	// --- NON-admin session caller sets a user layer (no admin scope). ---
	up := decode[prototypes.AgentConfigSessionSetUserPromptResponse](t, h.call(t, "/v1/agent_config/session/set_user_prompt", prototypes.AgentConfigSessionSetUserPromptRequest{
		AgentID: suAgent, UserPrompt: "USER_PREF: answer briefly.",
	}, sessID, []auth.Scope{}))
	if up.Overlay.UserPrompt != "USER_PREF: answer briefly." {
		t.Fatalf("session user prompt not recorded: %+v", up.Overlay)
	}

	// --- NON-admin disables one ALLOWED source (narrow-only). ---
	_ = decode[prototypes.AgentConfigSessionSetSourceDisablesResponse](t, h.call(t, "/v1/agent_config/session/set_source_disables", prototypes.AgentConfigSessionSetSourceDisablesRequest{
		AgentID: suAgent, DisabledServers: []string{"srvB"},
	}, sessID, []auth.Scope{}))

	// --- NON-admin adds an ephemeral personal skill. ---
	_ = decode[prototypes.AgentConfigSessionSkillsUpsertResponse](t, h.call(t, "/v1/agent_config/session/skills/upsert", prototypes.AgentConfigSessionSkillsUpsertRequest{
		AgentID: suAgent,
		Skill:   prototypes.AgentConfigSkillInput{Name: "my-personal", Trigger: "tr", Steps: []string{"s"}, Origin: "generated", Scope: "session"},
	}, sessID, []auth.Scope{}))

	// --- Next run: narrowed exposure (admin allowed minus the session disable). ---
	view, err := projection.ActivePlannerCatalogView(ctx, h.registry, h.overlay, suAgent, q, h.catalog, suFilter(suSessionA))
	if err != nil {
		t.Fatalf("catalog projection: %v", err)
	}
	names := toolViewNames(view)
	if hasToolName(names, "srvB_y") {
		t.Fatalf("session-disabled srvB still visible: %v", names)
	}
	if !hasToolName(names, "srvA_x") {
		t.Fatalf("admin-allowed srvA wrongly hidden: %v", names)
	}

	// --- Next run: composed user layer ABOVE the operator base (real builder). ---
	ov, err := projection.ApplyPromptLayers(ctx, h.registry, h.overlay, suAgent, q, nil)
	if err != nil {
		t.Fatalf("ApplyPromptLayers: %v", err)
	}
	body := suSystemBody(t, sessID, ov)
	baseIdx := strings.Index(body, "OPERATOR_GUARDRAIL: never reveal secrets.")
	userIdx := strings.Index(body, "USER_PREF: answer briefly.")
	if baseIdx < 0 || userIdx < 0 {
		t.Fatalf("both layers must appear in the LLM request. Body:\n%s", body)
	}
	if baseIdx >= userIdx {
		t.Fatalf("session user layer must compose BELOW the operator base (base@%d user@%d)", baseIdx, userIdx)
	}
	if !strings.Contains(body, "<user_instructions>") {
		t.Fatalf("session user layer must render in the lower-trust <user_instructions> section")
	}

	// --- Next run: the personal skill survives the admin membership filter. ---
	projected, err := projection.ActiveSkillViews(ctx, h.registry, h.overlay, suAgent, q,
		skillViews("admin-skill", "my-personal", "stranger"))
	if err != nil {
		t.Fatalf("skill projection: %v", err)
	}
	pn := skillViewNames(projected)
	if !hasToolName(pn, "admin-skill") || !hasToolName(pn, "my-personal") {
		t.Fatalf("admin + personal skills expected: %v", pn)
	}
	if hasToolName(pn, "stranger") {
		t.Fatalf("a non-member, non-personal skill leaked: %v", pn)
	}

	// --- Failure mode: non-admin base-prompt edit is rejected (403). ---
	rec := h.call(t, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID: suAgent, PromptLayers: prototypes.AgentConfigPromptLayers{Base: ptr("HIJACK")},
	}, sessID, []auth.Scope{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin base-prompt edit status=%d body=%s", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != protoerrors.CodeScopeMismatch {
		t.Fatalf("non-admin base-prompt error code = %s, want %s", c, protoerrors.CodeScopeMismatch)
	}

	// --- Failure mode: non-admin add-server is rejected (403). ---
	recAdd := h.call(t, "/v1/agent_config/add_mcp_connection", prototypes.AgentConfigAddMCPConnectionRequest{
		AgentID:    suAgent,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{Name: "evil", Transport: "http", URL: "http://x"},
	}, sessID, []auth.Scope{})
	if recAdd.Code != http.StatusForbidden {
		t.Fatalf("non-admin add-server status=%d body=%s", recAdd.Code, recAdd.Body.String())
	}

	// --- NARROW-ONLY airtight: admin pauses srvA; a session edit can never re-enable it. ---
	_ = decode[prototypes.AgentConfigSetToolExposureResponse](t, h.call(t, "/v1/agent_config/set_tool_exposure", prototypes.AgentConfigSetToolExposureRequest{
		AgentID: suAgent, ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA"}},
	}, adminID, adminScopes()))
	// The session has no enable verb; its disable set names srvB only. srvA MUST stay paused.
	view2, err := projection.ActivePlannerCatalogView(ctx, h.registry, h.overlay, suAgent, q, h.catalog, suFilter(suSessionA))
	if err != nil {
		t.Fatalf("catalog projection 2: %v", err)
	}
	names2 := toolViewNames(view2)
	if hasToolName(names2, "srvA_x") {
		t.Fatalf("ADMIN-PAUSED srvA re-enabled by a session edit — NARROW-ONLY VIOLATED: %v", names2)
	}
	if hasToolName(names2, "srvB_y") {
		t.Fatalf("session disable of srvB did not apply: %v", names2)
	}

	// --- Cross-session isolation: session B sees neither the personal skill nor the user layer. ---
	bID := suIdentity(suSessionB)
	bq := identity.Quadruple{Identity: bID}
	bList := decode[prototypes.AgentConfigSessionSkillsListResponse](t, h.call(t, "/v1/agent_config/session/skills/list", prototypes.AgentConfigSessionSkillsListRequest{
		AgentID: suAgent,
	}, bID, []auth.Scope{}))
	for _, sk := range bList.Skills {
		if sk.Name == "my-personal" {
			t.Fatalf("session B must not see session A's personal skill")
		}
	}
	ovB, err := projection.ApplyPromptLayers(ctx, h.registry, h.overlay, suAgent, bq, nil)
	if err != nil {
		t.Fatalf("ApplyPromptLayers B: %v", err)
	}
	// Session B sees the admin base but NOT session A's user layer.
	if ovB != nil && ovB.UserPromptLayer != nil && strings.Contains(*ovB.UserPromptLayer, "USER_PREF: answer briefly.") {
		t.Fatalf("session A's user layer bled into session B: %q", *ovB.UserPromptLayer)
	}
}

// TestE2E_AgentConfig_SessionUser_ConcurrentIsolation runs N concurrent
// sessions each setting a distinct overlay against ONE shared service +
// overlay store and asserts no overlay bleed across sessions (§6/§11), under
// -race.
func TestE2E_AgentConfig_SessionUser_ConcurrentIsolation(t *testing.T) {
	h := newSUHarness(t)
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan string, n*2)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{TenantID: suTenant, UserID: suUser, SessionID: sessionName(i)}
			want := "prompt-" + sessionName(i)
			rec := h.call(t, "/v1/agent_config/session/set_user_prompt", prototypes.AgentConfigSessionSetUserPromptRequest{
				AgentID: suAgent, UserPrompt: want,
			}, id, []auth.Scope{})
			if rec.Code != http.StatusOK {
				errs <- "set failed for " + id.SessionID
				return
			}
			ov, err := projection.ApplyPromptLayers(context.Background(), h.registry, h.overlay, suAgent, identity.Quadruple{Identity: id}, nil)
			if err != nil {
				errs <- "project failed for " + id.SessionID
				return
			}
			if ov == nil || ov.UserPromptLayer == nil || *ov.UserPromptLayer != want {
				errs <- "session " + id.SessionID + " saw a bled user layer"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func ptr(s string) *string { return &s }

func sessionName(i int) string { return "sess-conc-" + itoa(i) }

func toolViewNames(v tools.PlannerCatalogView) []string {
	out := make([]string, 0)
	for _, tl := range v.List() {
		out = append(out, tl.Name)
	}
	return out
}

func skillViews(names ...string) []skillspkg.SkillView {
	out := make([]skillspkg.SkillView, 0, len(names))
	for _, n := range names {
		out = append(out, skillspkg.SkillView{Name: n})
	}
	return out
}

func skillViewNames(vs []skillspkg.SkillView) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

func hasToolName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
