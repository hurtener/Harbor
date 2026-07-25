// Phase 151 cross-subsystem integration test per CLAUDE.md §17 / §17.8.
//
// This test drives the D-281 runtime loading-mode control surface
// end-to-end with REAL drivers on every seam: the StateStore-backed
// agent-config registry, the in-memory bus, the real Protocol Service +
// wire handler, a real tool catalog (with a real FTS5 search cache backing
// `tool_search`), the shared run-start projection
// (`projection.ActivePlannerCatalogView`), and — per §17.8 — the REAL
// `cmd/harbor-mcptest-stdio` MCP subprocess fixture attached through the
// production `serve.NewMCPConnectionAttacher` concrete, so the fixture's
// `echo` tool reaches the catalog exactly as it would in `harbor dev`.
//
// What this test proves:
//
//   - Flipping a live MCP server's tool to `deferred` via
//     `agent_config.set_tool_exposure` hides it from the next run's
//     prompt-time `List()` while `Resolve()` (dispatch) and `tool_search`
//     (the raw-catalog meta-tool) still surface it — the two-turn D-167
//     discovery cycle survives the override.
//   - Flipping back to `always` restores prompt visibility.
//   - An unknown loading value fails loud (400 invalid_request) BEFORE any
//     registry write — no revision, no event.
//   - Identity propagation: a second agent_id (same tenant) and a second
//     tenant using the IDENTICAL agent_id both see the boot-effective mode,
//     unaffected by the first agent's override (CLAUDE.md §6 rule 10).

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
	"github.com/hurtener/Harbor/internal/tools/drivers/searchcache"
)

const (
	leTenant      = "tenant-le"
	leUser        = "admin-le"
	leSession     = "sess-le"
	leAgent       = "agent-le"
	leOtherAgent  = "agent-le-other"  // cross-agent, SAME tenant
	leOtherTenant = "tenant-le-other" // cross-tenant, SAME agent id as leAgent
)

type leHarness struct {
	handler  http.Handler
	registry agentcfg.Registry
	catalog  tools.ToolCatalog
	bus      events.EventBus
}

func leID(tenant string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: leUser, SessionID: leSession}
}

func leQuad(tenant string) identity.Quadruple { return identity.Quadruple{Identity: leID(tenant)} }

func leFilter(tenant string) tools.CatalogFilter {
	return tools.CatalogFilter{TenantID: tenant, UserID: leUser, SessionID: leSession}
}

func newLEHarness(t *testing.T, stdioAllowlist []string) *leHarness {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	// A REAL FTS5 search cache backs `tool_search` (the raw-catalog meta-tool
	// this test proves stays unaffected by a loading-mode override).
	sc, err := searchcache.New(searchcache.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("search cache: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	cat := tools.NewCatalog(tools.WithSearchCache(sc))

	mcpReg := mcpdrv.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	attacher := serve.NewMCPConnectionAttacher(cat, mcpReg, bus, nil, leID(leTenant), nil, nil, nil)
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(coord),
		agentcfgprotocol.WithStdioAllowlist(stdioAllowlist),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Cleanup(func() {
		_ = attacher.Close(context.Background())
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return &leHarness{handler: h, registry: reg, catalog: cat, bus: bus}
}

func (h *leHarness) call(t *testing.T, path string, tenant string, body any, scopes []protoauth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, tenant)
	req.Header.Set(stream.HeaderUser, leUser)
	req.Header.Set(stream.HeaderSession, leSession)
	req = req.WithContext(protoauth.WithScopes(req.Context(), scopes))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// setToolExposure calls set_tool_exposure for leAgent under leTenant — every
// write in this test targets leAgent's config under the primary tenant; the
// cross-agent / cross-tenant assertions read the OTHER identity's projection
// directly rather than writing through this helper.
func (h *leHarness) setToolExposure(t *testing.T, te prototypes.AgentConfigToolExposure, scopes []protoauth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	return h.call(t, "/v1/agent_config/set_tool_exposure", leTenant, prototypes.AgentConfigSetToolExposureRequest{
		AgentID: leAgent, ToolExposure: te,
	}, scopes)
}

// TestE2E_AgentConfig_LoadingModeExposure drives the full D-281 flip / flip
// back / invalid-mode / isolation scenario against a REAL MCP stdio fixture.
func TestE2E_AgentConfig_LoadingModeExposure(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newLEHarness(t, []string{binPath})
	ctx := context.Background()

	// --- Attach the real fixture server so `mcptest_echo` reaches the live
	//     catalog with the MCP driver's LoadingAlways default. ---
	addResp := decode[prototypes.AgentConfigAddMCPConnectionResponse](t, h.call(t, "/v1/agent_config/add_mcp_connection", leTenant,
		prototypes.AgentConfigAddMCPConnectionRequest{
			AgentID:    leAgent,
			Connection: prototypes.AgentConfigMCPConnectionDescriptor{Name: "mcptest", Transport: "stdio", Command: []string{binPath}},
		}, adminScopes()))
	if addResp.State != "online" {
		t.Fatalf("attach state = %q, want online (reason=%q)", addResp.State, addResp.Reason)
	}
	if _, ok := h.catalog.Resolve("mcptest_echo"); !ok {
		t.Fatal("the fixture's echo tool did not reach the live catalog")
	}

	// --- Boot-effective: the tool is prompt-visible (LoadingAlways) before
	//     any override. ---
	view, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(leTenant), h.catalog, leFilter(leTenant))
	if err != nil {
		t.Fatalf("projection (boot-effective): %v", err)
	}
	if !hasEchoTool(view) {
		t.Fatal("mcptest_echo should be prompt-visible before any override")
	}

	// --- Flip to deferred via set_tool_exposure. ---
	flipResp := h.setToolExposure(t, prototypes.AgentConfigToolExposure{
		ToolLoadingModes: map[string]string{"mcptest_echo": "deferred"},
	}, adminScopes())
	if flipResp.Code != http.StatusOK {
		t.Fatalf("flip to deferred: status=%d body=%s", flipResp.Code, flipResp.Body.String())
	}

	// --- Next run's List() excludes it; Resolve() still returns it
	//     (dispatch-callable); tool_search (the raw-catalog meta-tool) still
	//     surfaces it — the D-167 two-turn discovery cycle survives. ---
	view, err = projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(leTenant), h.catalog, leFilter(leTenant))
	if err != nil {
		t.Fatalf("projection (deferred): %v", err)
	}
	if hasEchoTool(view) {
		t.Fatal("mcptest_echo must be absent from List() after the deferred override")
	}
	if tl, ok := view.Resolve("mcptest_echo"); !ok {
		t.Fatal("mcptest_echo must still resolve (discovery-callable) after the deferred override")
	} else if tl.Loading != tools.LoadingDeferred {
		t.Fatalf("Resolve(mcptest_echo).Loading = %q, want deferred", tl.Loading)
	}
	searchCtx, err := identity.With(ctx, leID(leTenant))
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if !searchFinds(searchCtx, h.catalog, "echo", "mcptest_echo") {
		t.Fatal("tool_search (raw catalog) must still surface the deferred-overridden tool")
	}

	// --- Flip back: desired-state REMOVAL restores boot-effective. ---
	backResp := h.setToolExposure(t, prototypes.AgentConfigToolExposure{}, adminScopes())
	if backResp.Code != http.StatusOK {
		t.Fatalf("flip back: status=%d body=%s", backResp.Code, backResp.Body.String())
	}
	view, err = projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(leTenant), h.catalog, leFilter(leTenant))
	if err != nil {
		t.Fatalf("projection (restored): %v", err)
	}
	if !hasEchoTool(view) {
		t.Fatal("removing the override must restore prompt visibility")
	}

	// --- Invalid mode -> loud 400, NO revision recorded. ---
	activeBeforeInvalid, _, err := h.registry.Active(ctx, leQuad(leTenant), leAgent, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active (before invalid): %v", err)
	}
	sub, err := h.bus.Subscribe(ctx, events.Filter{Admin: true, Types: []events.EventType{agentcfg.EventTypeConfigRevised}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()
	invalidResp := h.setToolExposure(t, prototypes.AgentConfigToolExposure{
		ToolLoadingModes: map[string]string{"mcptest_echo": "sometimes"},
	}, adminScopes())
	if invalidResp.Code != http.StatusBadRequest {
		t.Fatalf("invalid loading mode: status=%d, want 400; body=%s", invalidResp.Code, invalidResp.Body.String())
	}
	if c := errCode(t, invalidResp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("invalid loading mode error code = %q, want %q", c, protoerrors.CodeInvalidRequest)
	}
	activeAfterInvalid, _, err := h.registry.Active(ctx, leQuad(leTenant), leAgent, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active (after invalid): %v", err)
	}
	if activeAfterInvalid.RevisionID != activeBeforeInvalid.RevisionID {
		t.Fatalf("a revision was recorded on an invalid loading value: before=%q after=%q",
			activeBeforeInvalid.RevisionID, activeAfterInvalid.RevisionID)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("no agent.config.revised event should fire on a rejected set: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// --- Identity propagation + cross-agent isolation: a second agent_id
	//     under the SAME tenant, never touched by the override, sees the
	//     boot-effective mode. ---
	crossAgentView, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, leOtherAgent, leQuad(leTenant), h.catalog, leFilter(leTenant))
	if err != nil {
		t.Fatalf("projection (cross-agent): %v", err)
	}
	if !hasEchoTool(crossAgentView) {
		t.Fatal("a second agent_id under the same tenant must see the boot-effective (unaffected) mode")
	}

	// Re-apply the deferred override on leAgent for the cross-tenant check.
	if resp := h.setToolExposure(t, prototypes.AgentConfigToolExposure{
		ToolLoadingModes: map[string]string{"mcptest_echo": "deferred"},
	}, adminScopes()); resp.Code != http.StatusOK {
		t.Fatalf("re-apply deferred: status=%d body=%s", resp.Code, resp.Body.String())
	}

	// --- Cross-tenant isolation: a DIFFERENT tenant using the IDENTICAL
	//     agent_id string sees NO effect from leTenant's override (agent_id
	//     is a key, never an isolation widener — CLAUDE.md §6 rule 10). ---
	crossTenantView, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(leOtherTenant), h.catalog, leFilter(leOtherTenant))
	if err != nil {
		t.Fatalf("projection (cross-tenant): %v", err)
	}
	if !hasEchoTool(crossTenantView) {
		t.Fatal("a different tenant using the identical agent_id must be unaffected by leTenant's override — isolation breach")
	}
}

// hasEchoTool reports whether the fixture's "mcptest_echo" tool is present
// in v.List().
func hasEchoTool(v tools.PlannerCatalogView) bool {
	for _, tl := range v.List() {
		if tl.Name == "mcptest_echo" {
			return true
		}
	}
	return false
}

// searchFinds reports whether name appears in a `tool_search`-style raw
// catalog search for query — proving loading overrides never gate the
// meta-tool's discovery surface.
func searchFinds(ctx context.Context, cat tools.ToolCatalog, query, name string) bool {
	for _, tl := range cat.Search(ctx, query, nil, 10) {
		if tl.Name == name {
			return true
		}
	}
	return false
}
