package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// phase168_discovery_allowance_test.go — the MCP OAuth discovery-allowance write
// wired end to end with REAL drivers (§17.1): the StateStore-backed agent-config
// registry, the in-memory bus + real audit redactor, the real process-global MCP
// registry, the production discovery-origin applier (serve.MCPConnectionAttacher)
// and reconciler (serve.MCPConnectionDetacher), and the real Protocol Service +
// wire handler. It proves: the live write records a revision AND flips the live
// allow-list; a revoke prunes the recorded requirement's now-unallowed
// authorization-server entries; the run-start allowance-reconcile revokes an
// origin live (the rollback path) via the OWNER-scoped view; a second owner's
// reconcile leaves this owner's allow-list untouched (identity/owner isolation);
// and a non-admin caller is refused. Runs under -race.

const (
	daTenantA = "tenant-a"
	daTenantB = "tenant-b"
	daUser    = "admin"
	daSession = "sess"
	daAgentA  = "agent-a"
	daAgentB  = "agent-b"
)

type daHarness struct {
	handler http.Handler
	reg     agentcfg.Registry
	mcpReg  *mcpdrv.Registry
	det     *serve.MCPConnectionDetacher
	bus     events.EventBus
}

// daStubProvider satisfies the MCP registry's (unexported) serverProvider
// interface structurally — a minimal read-surface stub so Register succeeds
// (the discovery-allowance path exercises the registry's allow-list + requirement
// state, not the provider's transport).
type daStubProvider struct{ id tools.ToolSourceID }

func (p daStubProvider) SourceID() tools.ToolSourceID { return p.id }
func (p daStubProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return nil, nil
}
func (p daStubProvider) DisplayModes() []string { return nil }
func (p daStubProvider) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", nil
}
func (p daStubProvider) Close(context.Context) error { return nil }

func newDaHarness(t *testing.T) *daHarness {
	t.Helper()
	return newDaHarnessWithBootServers(t)
}

// newDaHarnessWithBootServers builds the harness with the given names declared
// as BOOT MCP servers (the yaml-declared set the control-plane verbs refuse).
// Callers that need no boot declaration use newDaHarness.
func newDaHarnessWithBootServers(t *testing.T, bootServers ...string) *daHarness {
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
	cat := tools.NewCatalog()
	mcpReg := mcpdrv.NewRegistry()
	attacher := serve.NewMCPConnectionAttacher(cat, mcpReg, bus, nil,
		identity.Identity{TenantID: daTenantA, UserID: daUser, SessionID: daSession}, nil, nil, nil)
	det := serve.NewMCPConnectionDetacher(cat, mcpReg, nil)
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithDiscoveryOriginApplier(attacher),
		agentcfgprotocol.WithBootDeclaredMCPServers(bootServers),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return &daHarness{handler: h, reg: reg, mcpReg: mcpReg, det: det, bus: bus}
}

// registerServer mirrors a completed runtime add: it registers an http server
// on the process-global MCP registry with an owner tag + allow-list (the state
// mcpdrv.Attach leaves behind), and records the matching config revision so the
// agent's active revision declares it.
func (h *daHarness) registerServer(t *testing.T, tenant, agentID, name string, origins []string) {
	t.Helper()
	if err := h.mcpReg.Register(context.Background(), mcpdrv.ServerRegistration{
		Provider:                     daStubProvider{id: tools.ToolSourceID(name)},
		Transport:                    "streamable-http",
		URLOrCommand:                 "https://" + name + ".invalid/rpc",
		InitialState:                 mcpdrv.ServerStateOnline,
		OAuthDiscoveryAllowedOrigins: origins,
		Owner:                        auth.Owner{Tenant: tenant, Agent: agentID},
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: daUser, SessionID: daSession}}
	payload := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
		{Name: name, Transport: agentcfg.MCPTransportHTTP, URL: "https://" + name + ".invalid/rpc", OAuthDiscoveryAllowedOrigins: origins},
	}}}
	if _, err := h.reg.SetRevision(context.Background(), q, agentID, agentcfg.ConfigScopeAgent, payload); err != nil {
		t.Fatalf("seed revision %s: %v", name, err)
	}
}

func (h *daHarness) setOrigins(t *testing.T, tenant, agentID, name string, origins []string, scopes []protoauth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	body := prototypes.AgentConfigSetMCPDiscoveryOriginsRequest{AgentID: agentID, Name: name, AllowedOrigins: origins}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/set_mcp_discovery_origins", bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, tenant)
	req.Header.Set(stream.HeaderUser, daUser)
	req.Header.Set(stream.HeaderSession, daSession)
	req = req.WithContext(protoauth.WithScopes(req.Context(), scopes))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func idCtxFor(t *testing.T, tenant string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: tenant, UserID: daUser, SessionID: daSession})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestE2E_DiscoveryAllowance_LiveWrite_RevokePrune_RollbackReconcile(t *testing.T) {
	h := newDaHarness(t)
	h.registerServer(t, daTenantA, daAgentA, "srv", []string{"https://as.example.net"})

	// A discovered requirement whose AS entry came from the granted origin.
	if err := h.mcpReg.RecordOAuthRequirement("srv", &auth.OAuthRequirement{
		ResourceMetadataURL: "https://srv.invalid/.well-known/oauth-protected-resource",
		Source:              "probe",
		AuthorizationServers: []auth.AuthorizationServerMeta{{
			Issuer:    "https://as.example.net",
			SourceURL: "https://as.example.net/.well-known/oauth-authorization-server",
		}},
	}); err != nil {
		t.Fatalf("record requirement: %v", err)
	}

	admin := []protoauth.Scope{protoauth.ScopeAdmin}

	// --- Live REVOKE via the Protocol write: allow-list cleared, requirement pruned.
	if rec := h.setOrigins(t, daTenantA, daAgentA, "srv", nil, admin); rec.Code != http.StatusOK {
		t.Fatalf("revoke write: %d body=%s", rec.Code, rec.Body.String())
	}
	_, _, live, err := h.mcpReg.OAuthDiscoveryTarget("srv")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live allow-list = %v after revoke, want empty", live)
	}
	v, err := h.mcpReg.GetServer(idCtxFor(t, daTenantA), "srv")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.OAuthRequirement == nil || len(v.OAuthRequirement.AuthorizationServers) != 0 {
		t.Fatalf("requirement AS entries not pruned on revoke: %+v", v.OAuthRequirement)
	}

	// --- ROLLBACK path: a stale live grant is corrected by the run-start
	// allowance-reconcile, which re-derives from the CURRENT revision (empty).
	if _, err := h.mcpReg.SetOAuthDiscoveryOrigins(idCtxFor(t, daTenantA), "srv", auth.Owner{Tenant: daTenantA, Agent: daAgentA}, []string{"https://stale.example.net"}); err != nil {
		t.Fatalf("inject stale grant: %v", err)
	}
	qa := identity.Quadruple{Identity: identity.Identity{TenantID: daTenantA, UserID: daUser, SessionID: daSession}}
	n, err := projection.ReconcileDiscoveryOrigins(idCtxFor(t, daTenantA), h.reg, daAgentA, qa, h.det)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconcile reapplied = %d, want 1", n)
	}
	_, _, live, _ = h.mcpReg.OAuthDiscoveryTarget("srv")
	if len(live) != 0 {
		t.Fatalf("live allow-list = %v after rollback reconcile, want empty (revoked)", live)
	}
}

func TestE2E_DiscoveryAllowance_OwnerScopedReconcile_LeavesOtherOwnerUntouched(t *testing.T) {
	h := newDaHarness(t)
	h.registerServer(t, daTenantA, daAgentA, "srv-a", []string{"https://as-a.example.net"})
	h.registerServer(t, daTenantB, daAgentB, "srv-b", []string{"https://as-b.example.net"})

	// Owner A's run-start reconcile must never touch owner B's server.
	qa := identity.Quadruple{Identity: identity.Identity{TenantID: daTenantA, UserID: daUser, SessionID: daSession}}
	if _, err := projection.ReconcileDiscoveryOrigins(idCtxFor(t, daTenantA), h.reg, daAgentA, qa, h.det); err != nil {
		t.Fatalf("reconcile owner A: %v", err)
	}
	_, _, liveB, err := h.mcpReg.OAuthDiscoveryTarget("srv-b")
	if err != nil {
		t.Fatalf("target srv-b: %v", err)
	}
	if len(liveB) != 1 || liveB[0] != "https://as-b.example.net" {
		t.Fatalf("owner B allow-list changed by owner A reconcile: %v", liveB)
	}
}

func TestE2E_DiscoveryAllowance_NonAdminRefused(t *testing.T) {
	h := newDaHarness(t)
	h.registerServer(t, daTenantA, daAgentA, "srv", []string{"https://as.example.net"})
	rec := h.setOrigins(t, daTenantA, daAgentA, "srv", []string{"https://as2.example.net"}, []protoauth.Scope{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin write = %d, want 403", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(protoerrors.CodeScopeMismatch)) {
		t.Fatalf("non-admin body = %s, want CodeScopeMismatch", rec.Body.String())
	}
}

func TestE2E_DiscoveryAllowance_ConcurrentWritesReadsReconciles(t *testing.T) {
	h := newDaHarness(t)
	h.registerServer(t, daTenantA, daAgentA, "srv", []string{"https://as.example.net"})
	if err := h.mcpReg.RecordOAuthRequirement("srv", &auth.OAuthRequirement{
		AuthorizationServers: []auth.AuthorizationServerMeta{{SourceURL: "https://as.example.net/.well-known/oauth-authorization-server"}},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	qa := identity.Quadruple{Identity: identity.Identity{TenantID: daTenantA, UserID: daUser, SessionID: daSession}}
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n * 3)
	for i := range n {
		origins := []string{"https://as.example.net"}
		if i%2 == 0 {
			origins = nil
		}
		go func() {
			defer wg.Done()
			_, _ = h.mcpReg.SetOAuthDiscoveryOrigins(idCtxFor(t, daTenantA), "srv", auth.Owner{Tenant: daTenantA, Agent: daAgentA}, origins)
		}()
		go func() { defer wg.Done(); _, _, _, _ = h.mcpReg.OAuthDiscoveryTarget("srv") }()
		go func() {
			defer wg.Done()
			_, _ = projection.ReconcileDiscoveryOrigins(idCtxFor(t, daTenantA), h.reg, daAgentA, qa, h.det)
		}()
	}
	wg.Wait()
}
