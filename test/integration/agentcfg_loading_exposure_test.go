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
	"strings"
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
	"github.com/hurtener/Harbor/internal/tools/builtin"
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
	mcpReg   *mcpdrv.Registry
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
	if err := builtin.RegisterWith(builtin.RegistryContext{Catalog: cat}, []string{"tool_search"}); err != nil {
		t.Fatalf("register tool_search: %v", err)
	}

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
	return &leHarness{handler: h, registry: reg, catalog: cat, bus: bus, mcpReg: mcpReg}
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
	if _, ok := h.catalog.Resolve(ownedSource("mcptest", leTenant, leAgent) + "_echo"); !ok {
		t.Fatal("the fixture's echo tool did not reach the live catalog")
	}

	// --- Boot-effective: the tool is prompt-visible (LoadingAlways) before
	//     any override. ---
	view, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(leTenant), h.catalog, leFilter(leTenant), h.mcpReg)
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
	view, err = projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(leTenant), h.catalog, leFilter(leTenant), h.mcpReg)
	if err != nil {
		t.Fatalf("projection (deferred): %v", err)
	}
	if hasEchoTool(view) {
		t.Fatal("mcptest_echo must be absent from List() after the deferred override")
	}
	if tl, ok := view.Resolve(ownedSource("mcptest", leTenant, leAgent) + "_echo"); !ok {
		t.Fatal("mcptest_echo must still resolve (discovery-callable) after the deferred override")
	} else if tl.Loading != tools.LoadingDeferred {
		t.Fatalf("Resolve(mcptest_echo).Loading = %q, want deferred", tl.Loading)
	}
	searchCtx, err := identity.WithRun(ctx, leID(leTenant), "run-le")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	if !searchFinds(t, searchCtx, h.catalog, view, "echo", ownedSource("mcptest", leTenant, leAgent)+"_echo") {
		t.Fatal("tool_search must still surface the deferred-overridden tool within the run's view")
	}

	// --- Flip back: desired-state REMOVAL restores boot-effective. ---
	backResp := h.setToolExposure(t, prototypes.AgentConfigToolExposure{}, adminScopes())
	if backResp.Code != http.StatusOK {
		t.Fatalf("flip back: status=%d body=%s", backResp.Code, backResp.Body.String())
	}
	view, err = projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(leTenant), h.catalog, leFilter(leTenant), h.mcpReg)
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

	// Another agent in the same tenant cannot see the owner's source.
	crossAgentView, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, leOtherAgent, leQuad(leTenant), h.catalog, leFilter(leTenant), h.mcpReg)
	if err != nil {
		t.Fatalf("projection (cross-agent): %v", err)
	}
	if hasEchoTool(crossAgentView) {
		t.Fatal("another agent must not see the owned source")
	}

	// Re-apply the deferred override on leAgent for the cross-tenant check.
	if resp := h.setToolExposure(t, prototypes.AgentConfigToolExposure{
		ToolLoadingModes: map[string]string{"mcptest_echo": "deferred"},
	}, adminScopes()); resp.Code != http.StatusOK {
		t.Fatalf("re-apply deferred: status=%d body=%s", resp.Code, resp.Body.String())
	}

	// Another tenant cannot see the source even with the identical agent id.
	crossTenantView, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(leOtherTenant), h.catalog, leFilter(leOtherTenant), h.mcpReg)
	if err != nil {
		t.Fatalf("projection (cross-tenant): %v", err)
	}
	if hasEchoTool(crossTenantView) {
		t.Fatal("another tenant must not see the owned source")
	}
}

// hasEchoTool reports whether the fixture's "mcptest_echo" tool is present
// in v.List().
func hasEchoTool(v tools.PlannerCatalogView) bool {
	for _, tl := range v.List() {
		if tl.Name == ownedSource("mcptest", leTenant, leAgent)+"_echo" {
			return true
		}
	}
	return false
}

// searchFinds reports whether the physical tool name appears in the REAL
// registered `tool_search` meta-tool's results for query, invoked under the
// given run view exactly as the dispatcher seats it — proving loading
// overrides never gate discovery within the run's own view.
func searchFinds(t *testing.T, ctx context.Context, cat tools.ToolCatalog, view tools.PlannerCatalogView, query, name string) bool {
	t.Helper()
	for _, r := range runToolSearch(t, ctx, cat, view, query).Tools {
		if r.Name == declaredToolName(name) {
			return true
		}
	}
	return false
}

func runToolSearch(t *testing.T, ctx context.Context, cat tools.ToolCatalog, view tools.PlannerCatalogView, query string) builtin.ToolSearchOut {
	t.Helper()
	desc, ok := cat.Resolve("tool_search")
	if !ok {
		t.Fatal("tool_search is not registered")
	}
	raw, err := json.Marshal(builtin.ToolSearchArgs{Query: query, Limit: 50})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := desc.Invoke(tools.WithRunToolView(ctx, view), raw)
	if err != nil {
		t.Fatalf("tool_search Invoke: %v", err)
	}
	out, ok := res.Value.(builtin.ToolSearchOut)
	if !ok {
		t.Fatalf("tool_search result %T", res.Value)
	}
	return out
}

func declaredToolName(physical string) string {
	name, _ := tools.NewModelToolNameProjectionWithReservedControls([]string{physical}).DeclaredName(physical)
	return name
}

// TestE2E_ToolSearch_CrossTenantIsolation attaches the SAME real MCP server
// for two tenants (identical agent id, identical tool names) into one
// process catalog + one FTS5 index, then runs the real tool_search under each
// tenant's real run-start projection. SECURITY: a tenant must never see the
// other tenant's tool names or descriptions, and must still discover its own.
func TestE2E_ToolSearch_CrossTenantIsolation(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newLEHarness(t, []string{binPath})
	ctx := context.Background()
	for _, tenant := range []string{leTenant, leOtherTenant} {
		resp := decode[prototypes.AgentConfigAddMCPConnectionResponse](t, h.call(t, "/v1/agent_config/add_mcp_connection", tenant,
			prototypes.AgentConfigAddMCPConnectionRequest{
				AgentID:    leAgent,
				Connection: prototypes.AgentConfigMCPConnectionDescriptor{Name: "mcptest", Transport: "stdio", Command: []string{binPath}},
			}, adminScopes()))
		if resp.State != "online" {
			t.Fatalf("attach for %s: state=%q reason=%q", tenant, resp.State, resp.Reason)
		}
	}
	for _, tenant := range []string{leTenant, leOtherTenant} {
		other := leOtherTenant
		if tenant == leOtherTenant {
			other = leTenant
		}
		own := ownedSource("mcptest", tenant, leAgent)
		foreign := ownedSource("mcptest", other, leAgent)
		if _, ok := h.catalog.Resolve(foreign + "_echo"); !ok {
			t.Fatalf("precondition: %s's echo is not in the shared catalog", other)
		}
		view, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, leAgent, leQuad(tenant), h.catalog, leFilter(tenant), h.mcpReg)
		if err != nil {
			t.Fatalf("projection(%s): %v", tenant, err)
		}
		searchCtx, err := identity.WithRun(ctx, leID(tenant), "run-"+tenant)
		if err != nil {
			t.Fatalf("identity.WithRun: %v", err)
		}
		for _, q := range []string{"echo", "mcptest", ""} {
			out := runToolSearch(t, searchCtx, h.catalog, view, q)
			foundOwn := false
			for _, r := range out.Tools {
				if strings.HasPrefix(r.Name, declaredToolName(foreign)) {
					t.Fatalf("tenant %s tool_search(%q) disclosed tenant %s's tool %q", tenant, q, other, r.Name)
				}
				if r.Name == declaredToolName(own+"_echo") {
					foundOwn = true
					// The server's self-declared serverInfo labels the result.
					if r.Server == "" {
						t.Fatalf("tenant %s: own echo result carries no server label: %+v", tenant, r)
					}
				}
			}
			if q != "" && !foundOwn {
				t.Fatalf("tenant %s tool_search(%q) missed its own echo tool: %+v", tenant, q, out.Tools)
			}
		}
	}
}
