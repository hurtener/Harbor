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
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// This integration test drives the agent-config add-connection surface (Phase
// 92f) end-to-end with REAL drivers: the StateStore-backed registry, the
// in-memory bus, a real tool catalog + MCP registry, the unified pause/resume
// Coordinator, the real Protocol Service + wire handler, and the production
// runtime MCP-attach concrete (serve.NewMCPConnectionAttacher) driving a
// REAL stdio MCP server subprocess (the cmd/harbor-mcptest-stdio fixture,
// §17.8 — a fixture derived from a real server). It proves: an admin add
// dials → initialize → discover → registers → the server's tools reach the
// live catalog; the descriptor is recorded as a revision (diff/rollback);
// a failing dial records `failed` and registers nothing; a stdio add of an
// un-allowlisted command is rejected (fail-closed); a non-admin caller is
// rejected 403.

const (
	addcTenant  = "tenant-addc"
	addcUser    = "admin-addc"
	addcSession = "sess-addc"
	addcAgent   = "agent-addc"
)

type addcHarness struct {
	handler  http.Handler
	registry agentcfg.Registry
	catalog  tools.ToolCatalog
	mcpReg   *mcpdrv.Registry
	bus      events.EventBus
}

func newAddcHarness(t *testing.T, stdioAllowlist []string) *addcHarness {
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
	coord := pauseresume.New(pauseresume.WithBus(bus))
	// The REAL production attach concrete (the devstack D-094 mirror of
	// cmd/harbor's devMCPConnectionAttacher) — drives the real MCP attach
	// against the live catalog + registry + bus.
	attacher := serve.NewMCPConnectionAttacher(cat, mcpReg, bus, nil,
		identity.Identity{TenantID: addcTenant, UserID: addcUser, SessionID: addcSession}, nil, nil, nil)
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
	return &addcHarness{handler: h, registry: reg, catalog: cat, mcpReg: mcpReg, bus: bus}
}

func (h *addcHarness) add(t *testing.T, conn prototypes.AgentConfigMCPConnectionDescriptor, scopes []protoauth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	body := prototypes.AgentConfigAddMCPConnectionRequest{AgentID: addcAgent, Connection: conn}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/add_mcp_connection", bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, addcTenant)
	req.Header.Set(stream.HeaderUser, addcUser)
	req.Header.Set(stream.HeaderSession, addcSession)
	req = req.WithContext(protoauth.WithScopes(req.Context(), scopes))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func addcID() identity.Identity {
	return identity.Identity{TenantID: addcTenant, UserID: addcUser, SessionID: addcSession}
}

func addcQuad() identity.Quadruple { return identity.Quadruple{Identity: addcID()} }

// TestE2E_AgentConfig_AddConnection drives the full add path against a REAL
// stdio MCP subprocess fixture.
func TestE2E_AgentConfig_AddConnection(t *testing.T) {
	binPath := buildMCPTestServer(t)
	const badBin = "/nonexistent/harbor-bad-mcp-server"
	// Allowlist BOTH the good fixture and a (nonexistent) path so the
	// failing-dial case still passes the gate but fails the attach.
	h := newAddcHarness(t, []string{binPath, badBin})

	// 1) Admin add of the allowlisted stdio fixture → online; tools reach the
	//    live catalog.
	resp := decode[prototypes.AgentConfigAddMCPConnectionResponse](t, h.add(t,
		prototypes.AgentConfigMCPConnectionDescriptor{Name: "mcptest", Transport: "stdio", Command: []string{binPath}},
		adminScopes()))
	if resp.State != "online" {
		t.Fatalf("add state = %q, want online (reason=%q)", resp.State, resp.Reason)
	}
	if resp.Revision == nil {
		t.Fatal("online add recorded no revision")
	}
	if _, ok := h.catalog.Resolve("mcptest_echo"); !ok {
		t.Fatal("the fixture's echo tool did not reach the live catalog after the add")
	}
	// The MCP registry reflects the new server.
	idCtx, err := identity.With(context.Background(), addcID())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	servers, _, lerr := h.mcpReg.ListServers(idCtx, mcpRegistryListFilterAll())
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	if len(servers) != 1 || servers[0].Name != "mcptest" {
		t.Fatalf("registry servers = %+v, want one named mcptest", servers)
	}
	firstRev := resp.Revision.RevisionID

	// 2) Diff + rollback round-trip: add a second http connection, diff, roll
	//    back to the first revision.
	resp2 := decode[prototypes.AgentConfigAddMCPConnectionResponse](t, h.add(t,
		prototypes.AgentConfigMCPConnectionDescriptor{Name: "mcptest", Transport: "http", URL: "https://example.invalid/sse"},
		adminScopes()))
	// http add to an invalid host fails the dial → failed (no half-attach).
	if resp2.State != "failed" {
		t.Logf("note: http add state = %q", resp2.State)
	}
	secondConn := prototypes.AgentConfigMCPConnectionDescriptor{Name: "mcptest2", Transport: "stdio", Command: []string{binPath}}
	resp3 := decode[prototypes.AgentConfigAddMCPConnectionResponse](t, h.add(t, secondConn, adminScopes()))
	if resp3.State != "online" || resp3.Revision == nil {
		t.Fatalf("second stdio add state = %q", resp3.State)
	}
	diff, derr := h.registry.Diff(context.Background(), addcQuad(), addcAgent, firstRev, resp3.Revision.RevisionID, agentcfg.ConfigScopeAgent)
	if derr != nil {
		t.Fatalf("Diff: %v", derr)
	}
	if len(diff.Connections.Added) != 1 || diff.Connections.Added[0] != "mcptest2" {
		t.Errorf("diff connections added = %+v, want [mcptest2]", diff.Connections.Added)
	}
	if _, rerr := h.registry.Rollback(context.Background(), addcQuad(), addcAgent, firstRev, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); rerr != nil {
		t.Fatalf("Rollback: %v", rerr)
	}
	active, _, _ := h.registry.Active(context.Background(), addcQuad(), addcAgent, agentcfg.ConfigScopeAgent)
	if active.RevisionID != firstRev {
		t.Errorf("rollback active = %q, want %q", active.RevisionID, firstRev)
	}

	// 3) A failing dial (allowlisted but nonexistent binary) records failed +
	//    registers nothing new.
	failResp := decode[prototypes.AgentConfigAddMCPConnectionResponse](t, h.add(t,
		prototypes.AgentConfigMCPConnectionDescriptor{Name: "broken", Transport: "stdio", Command: []string{badBin}},
		adminScopes()))
	if failResp.State != "failed" {
		t.Fatalf("failing dial state = %q, want failed", failResp.State)
	}
	if failResp.Reason == "" {
		t.Error("failing dial carried no reason")
	}
	if failResp.Revision != nil {
		t.Error("failing dial recorded a revision")
	}
	if _, ok := h.catalog.Resolve("broken_echo"); ok {
		t.Error("failing dial registered a tool (half-attach)")
	}
}

// TestE2E_AgentConfig_AddConnection_StdioGate proves the fail-closed stdio
// allowlist gate (a command NOT in the allowlist is rejected 403) and the
// non-admin rejection.
func TestE2E_AgentConfig_AddConnection_StdioGate(t *testing.T) {
	binPath := buildMCPTestServer(t)
	// Allowlist is EMPTY → every stdio add is rejected.
	h := newAddcHarness(t, nil)

	rec := h.add(t,
		prototypes.AgentConfigMCPConnectionDescriptor{Name: "mcptest", Transport: "stdio", Command: []string{binPath}},
		adminScopes())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stdio-without-allowlist status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &perr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("error code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}
	// The catalog must not have gained the tool.
	if _, ok := h.catalog.Resolve("mcptest_echo"); ok {
		t.Error("a gated stdio add registered a tool")
	}

	// Non-admin caller → 403 (the whole agent_config family is admin-scoped).
	nonAdmin := h.add(t,
		prototypes.AgentConfigMCPConnectionDescriptor{Name: "mcptest", Transport: "stdio", Command: []string{binPath}},
		nil)
	if nonAdmin.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403; body=%s", nonAdmin.Code, nonAdmin.Body.String())
	}
}
