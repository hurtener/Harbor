package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/artifacts"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	skillspkg "github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// This integration test wires the agent-config MCP-exposure / per-tool
// policy surface (Phase 92d) end-to-end with REAL drivers: the
// StateStore-backed registry, the in-memory bus, the localdb SkillStore, the
// real Protocol Service + wire handler, a real tool catalog, the shared
// run-start projection, and the real MCP Apps AppsAccessor + AppsSurface for
// the app→host gate. It proves: set_tool_exposure records a revision +
// emits the overlay events; the run-start projection excludes a paused
// server's tools while an in-flight snapshot keeps them (the asymmetry); the
// app→host CallTool is rejected against CURRENT state with CodeScopeMismatch;
// diff/rollback round-trip; resume restores; non-admin is rejected.

const (
	mpTenant  = "tenant-mcp"
	mpUser    = "admin-mcp"
	mpSession = "sess-mcp"
	mpAgent   = "agent-mcp"
)

type mpHarness struct {
	handler  http.Handler
	registry agentcfg.Registry
	catalog  tools.ToolCatalog
	apps     *mcpconsole.AppsAccessor
	surface  *protocol.AppsSurface
	bus      events.EventBus
}

func mpID() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: mpTenant, UserID: mpUser, SessionID: mpSession}}
}

func mpFilter() tools.CatalogFilter {
	return tools.CatalogFilter{TenantID: mpTenant, UserID: mpUser, SessionID: mpSession}
}

func newMPCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	reg := func(name string, source tools.ToolSourceID) {
		t.Helper()
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
			Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: map[string]any{"echo": string(args)}}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	reg("srvA_x", "srvA")
	reg("srvA_y", "srvA")
	reg("srvB_z", "srvB")
	return cat
}

func newMPArtifacts(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	s, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func newMPState(t *testing.T) state.StateStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func newMPHarness(t *testing.T) *mpHarness {
	t.Helper()
	st := newMPState(t)
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
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithSkillStore(skillStore),
		agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	cat := newMPCatalog(t)
	toolCtx, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{State: newMPState(t), Store: newMPArtifacts(t), Bus: bus})
	if err != nil {
		t.Fatalf("toolctx: %v", err)
	}
	apps, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    mcp.NewRegistry(),
		Catalog:     cat,
		Store:       newMPArtifacts(t),
		Bus:         bus,
		ToolContext: toolCtx,
		AgentConfig: reg,
		AgentID:     mpAgent,
		Threshold:   4096,
	})
	if err != nil {
		t.Fatalf("apps accessor: %v", err)
	}
	surface, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: apps, Invoker: apps, ToolContext: apps,
		AgentResolver: explicitAgentReachResolver{effective: mpAgent},
		AgentReach:    auth.NewAgentReachAuthorizer(),
	})
	if err != nil {
		t.Fatalf("apps surface: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = skillStore.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return &mpHarness{handler: h, registry: reg, catalog: cat, apps: apps, surface: surface, bus: bus}
}

// mpCall POSTs to the agent-config handler with the mp identity headers and
// the given scopes in ctx (mirrors acHarness.call for the mp constants).
func mpCall(t *testing.T, handler http.Handler, path string, body any, scopes []auth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, mpTenant)
	req.Header.Set(stream.HeaderUser, mpUser)
	req.Header.Set(stream.HeaderSession, mpSession)
	req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func (h *mpHarness) setToolExposure(t *testing.T, te prototypes.AgentConfigToolExposure, scopes []auth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	return mpCall(t, h.handler, "/v1/agent_config/set_tool_exposure", prototypes.AgentConfigSetToolExposureRequest{
		AgentID: mpAgent, ToolExposure: te,
	}, scopes)
}

func TestE2E_AgentConfig_MCPPolicy(t *testing.T) {
	h := newMPHarness(t)
	ctx := context.Background()

	sub, err := h.bus.Subscribe(ctx, events.Filter{
		Admin: true,
		Types: []events.EventType{
			agentcfg.EventTypeMCPConnectionPaused,
			agentcfg.EventTypeMCPConnectionResumed,
			agentcfg.EventTypeConfigRevised,
		},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	paused := make(chan events.Event, 16)
	resumed := make(chan events.Event, 16)
	go func() {
		for ev := range sub.Events() {
			switch ev.Type {
			case agentcfg.EventTypeMCPConnectionPaused:
				paused <- ev
			case agentcfg.EventTypeMCPConnectionResumed:
				resumed <- ev
			}
		}
	}()

	// Seed a skills revision first, to prove tool-exposure preserves it.
	_ = decode[prototypes.AgentConfigSkillsUpsertResponse](t, mpCall(t, h.handler, "/v1/agent_config/skills/upsert", prototypes.AgentConfigSkillsUpsertRequest{
		AgentID: mpAgent,
		Skill:   prototypes.AgentConfigSkillInput{Name: "alpha", Trigger: "tr", Steps: []string{"s"}, Origin: "generated", Scope: "session"},
	}, adminScopes()))

	// Capture a pre-pause planner snapshot (run-start view) — it must keep the
	// tool after the pause (the planner-snapshot / app-call-current asymmetry).
	snapshot, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, mpAgent, mpID(), h.catalog, mpFilter())
	if err != nil {
		t.Fatalf("snapshot projection: %v", err)
	}
	if _, ok := snapshot.Resolve("srvA_x"); !ok {
		t.Fatal("pre-pause snapshot missing srvA_x")
	}

	// --- Pause srvA + disable srvB_z via the admin Protocol surface. ---
	set := decode[prototypes.AgentConfigSetToolExposureResponse](t, h.setToolExposure(t, prototypes.AgentConfigToolExposure{
		PausedServers: []string{"srvA"}, DisabledTools: []string{"srvB_z"},
	}, adminScopes()))
	if set.Revision.Payload.Skills == nil || len(set.Revision.Payload.Skills.Names) != 1 {
		t.Fatalf("skills section not preserved across tool-exposure edit: %+v", set.Revision.Payload.Skills)
	}

	pe := waitEvent(t, paused)
	if pp, ok := pe.Payload.(agentcfg.MCPConnectionPausedPayload); !ok || pp.ServerID != "srvA" || pp.AgentID != mpAgent {
		t.Fatalf("paused payload=%+v", pe.Payload)
	}
	if pe.Identity.TenantID != mpTenant || pe.Identity.UserID != mpUser {
		t.Fatalf("paused event identity=%+v", pe.Identity)
	}

	// --- Next-run projection excludes the paused server's tools + disabled tool. ---
	view, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, mpAgent, mpID(), h.catalog, mpFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range view.List() {
		got[tl.Name] = true
	}
	if got["srvA_x"] || got["srvA_y"] {
		t.Fatalf("paused server srvA tools still projected: %v", got)
	}
	if got["srvB_z"] {
		t.Fatalf("disabled tool srvB_z still projected: %v", got)
	}

	// --- Asymmetry: the pre-pause snapshot still has srvA_x. ---
	if _, ok := snapshot.Resolve("srvA_x"); !ok {
		t.Fatal("in-flight planner snapshot lost srvA_x after a pause — asymmetry violated")
	}

	// --- App→host CallTool is rejected against CURRENT state (CodeScopeMismatch). ---
	idCtx, err := identity.WithVerified(ctx, mpID().Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	idCtx = auth.WithAgentReach(idCtx, []string{mpAgent})
	_, dErr := h.surface.Dispatch(idCtx, methods.MethodMCPAppsCallTool, &prototypes.MCPAppCallToolRequest{
		Identity:  prototypes.IdentityScope{Tenant: mpTenant, User: mpUser, Session: mpSession},
		Tool:      "srvA_x",
		Arguments: json.RawMessage(`{}`),
	})
	var perr *protoerrors.Error
	if !errors.As(dErr, &perr) || perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("app call against paused server err=%v, want CodeScopeMismatch", dErr)
	}
	// A non-paused server's tool still invokes through the app surface.
	if _, err := h.surface.Dispatch(idCtx, methods.MethodMCPAppsCallTool, &prototypes.MCPAppCallToolRequest{
		Identity: prototypes.IdentityScope{Tenant: mpTenant, User: mpUser, Session: mpSession},
		Tool:     "srvB_z",
	}); err == nil {
		t.Fatal("srvB_z is disabled — app call should be rejected")
	}

	// --- Diff across the seed-skills revision-free state and now: rollback. ---
	list := decode[prototypes.AgentConfigListRevisionsResponse](t, mpCall(t, h.handler, "/v1/agent_config/list_revisions", prototypes.AgentConfigListRevisionsRequest{AgentID: mpAgent}, adminScopes()))
	if len(list.Revisions) < 2 {
		t.Fatalf("expected ≥2 revisions, got %d", len(list.Revisions))
	}
	// Newest is the tool-exposure rev; find the skills-only parent (the seed).
	toRev := list.Revisions[0].RevisionID
	fromRev := list.Revisions[len(list.Revisions)-1].RevisionID
	diff := decode[prototypes.AgentConfigDiffResponse](t, mpCall(t, h.handler, "/v1/agent_config/diff", prototypes.AgentConfigDiffRequest{
		AgentID: mpAgent, FromRevision: fromRev, ToRevision: toRev,
	}, adminScopes()))
	if len(diff.Diff.ToolExposure.PausedAdded) != 1 || diff.Diff.ToolExposure.PausedAdded[0] != "srvA" {
		t.Fatalf("diff paused_added=%v want [srvA]", diff.Diff.ToolExposure.PausedAdded)
	}

	// --- Resume restores projection (a new revision clearing the paused set). ---
	_ = decode[prototypes.AgentConfigSetToolExposureResponse](t, h.setToolExposure(t, prototypes.AgentConfigToolExposure{}, adminScopes()))
	re := waitEvent(t, resumed)
	if rp, ok := re.Payload.(agentcfg.MCPConnectionResumedPayload); !ok || rp.ServerID != "srvA" {
		t.Fatalf("resumed payload=%+v", re.Payload)
	}
	view2, err := projection.ActivePlannerCatalogView(ctx, h.registry, nil, mpAgent, mpID(), h.catalog, mpFilter())
	if err != nil {
		t.Fatalf("post-resume projection: %v", err)
	}
	if len(view2.List()) != 3 {
		t.Fatalf("resume did not restore all tools: %d", len(view2.List()))
	}
	// The app→host call to the resumed server now succeeds.
	if _, err := h.surface.Dispatch(idCtx, methods.MethodMCPAppsCallTool, &prototypes.MCPAppCallToolRequest{
		Identity: prototypes.IdentityScope{Tenant: mpTenant, User: mpUser, Session: mpSession},
		Tool:     "srvA_x",
	}); err != nil {
		t.Fatalf("post-resume app call rejected: %v", err)
	}

	// --- Failure mode: non-admin set_tool_exposure → 403 CodeScopeMismatch. ---
	rec := h.setToolExposure(t, prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA"}}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != protoerrors.CodeScopeMismatch {
		t.Fatalf("non-admin code=%q want %q", errCode(t, rec), protoerrors.CodeScopeMismatch)
	}
}

// TestE2E_AgentConfig_MCPPolicy_Concurrency stresses the shared registry +
// catalog + projection under N≥12 concurrent projections + app-call gate
// checks while a pause is active — proving no cross-run bleed and no race
// (the gate is run under -race in CI).
func TestE2E_AgentConfig_MCPPolicy_Concurrency(t *testing.T) {
	h := newMPHarness(t)
	ctx := context.Background()
	if _, err := h.registry.SetRevision(ctx, mpID(), mpAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srvA"}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed pause: %v", err)
	}
	idCtx, err := identity.WithVerified(ctx, mpID().Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	const n = 24
	var wg sync.WaitGroup
	errCh := make(chan error, n*2)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, vErr := projection.ActivePlannerCatalogView(ctx, h.registry, nil, mpAgent, mpID(), h.catalog, mpFilter())
			if vErr != nil {
				errCh <- vErr
				return
			}
			for _, tl := range v.List() {
				if tl.Source == "srvA" {
					errCh <- errors.New("paused srvA tool leaked into a concurrent projection")
					return
				}
			}
			if _, cErr := h.apps.CallTool(idCtx, "srvA_x", json.RawMessage(`{}`)); !errors.Is(cErr, mcpconsole.ErrAppToolExposureDenied) {
				errCh <- errors.New("concurrent app call to paused server was not rejected")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatal(e)
	}
}
