// Phase 203 — Wire-carried per-user credential INJECTION for dynamically-added
// receiver-style MCP servers (RFC §6.4; D-346; the wire-plumbing sibling of the
// HA-34 injection engine D-341, mirroring the HA-32 wire-OAuth posture D-340).
//
// This test wires the FULL runtime-add wire path with REAL drivers across every
// seam (CLAUDE.md §17.3) — no mocks at the boundary:
//
//   - the agent-config Protocol Service (agentcfgprotocol) + its StateStore-backed
//     revision registry;
//   - the PRODUCTION runtime MCP-attach concrete (serve.NewMCPConnectionAttacher),
//     which imports the real MCP driver and drives dial → initialize → discover →
//     register against a live catalog + registry;
//   - the same RFC-8693 broker fixture + real `tokenexchange` provider the boot
//     path uses (newP142Broker / newP148ProviderAndBus), wired into a real
//     ProviderSet the injection binding resolves `vendor-broker` against;
//   - an httptest-hosted receiver-style MCP server on the OFFICIAL go-sdk
//     streamable-HTTP handler, fronted by the p200 recorder that captures every
//     request header + `_meta` map (the §17.8 note in phase200 applies: the
//     recorder observes the ACTUAL injected value, so a wrong-field wiring cannot
//     pass);
//   - the real `audit.Redactor` (patterns driver) on the seam.
//
// It proves the WIRE-PATH-specific behaviors phase 200 (engine-only) does not:
// (a) opt-in OFF → an add carrying an injection mapping is REJECTED loud, no
// attach; (b) opt-in ON → the add comes online, the revision PERSISTS the
// injection mapping, and the attach actually wired injection so an invocation
// injects the per-user pulled value; (c) two acting users get DISTINCT injected
// values (isolation) and the redactor holds every form to `***`; (d) a broker
// outage on the wired connection fails the call loud with NO wire request.
// `-race` throughout.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

const (
	p203Tenant  = "tenant-203"
	p203User    = "admin-203"
	p203Session = "sess-203"
	p203Agent   = "agent-203"
	p203Broker  = "vendor-broker"
)

type p203Harness struct {
	svc     *agentcfgprotocol.Service
	catalog tools.ToolCatalog
	rec     *p200Recorder
	broker  *p142Broker
}

// newP203Harness builds the full runtime-add wire path against a real receiver
// server + broker + provider set, with the wire-injection opt-in set to `allow`.
func newP203Harness(t *testing.T, allow bool) *p203Harness {
	t.Helper()
	broker := newP142Broker(t)
	hs, rec := p200MCPServer(t)
	prov, bus := newP148ProviderAndBus(t, broker, hs.URL)

	// The ProviderSet the injection binding resolves `vendor-broker` against — the
	// real broker-pull provider with the receiver's host in its allow-list.
	providerSet := toolauth.NewProviderSet(map[string]toolauth.OAuthProvider{p203Broker: prov})

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
	attacher := serve.NewMCPConnectionAttacher(cat, mcpReg, bus, nil,
		identity.Identity{TenantID: p203Tenant, UserID: p203User, SessionID: p203Session}, nil, providerSet)
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(coord),
		agentcfgprotocol.WithAllowWireInjection(allow),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(func() {
		_ = attacher.Close(context.Background())
		_ = reg.Close(context.Background())
	})
	// The receiver connection URL is hs.URL; carry it via a closure the tests use.
	p203URL = hs.URL
	return &p203Harness{svc: svc, catalog: cat, rec: rec, broker: broker}
}

// p203URL is the current harness's receiver URL (set at build; each test builds
// its own harness serially, so this is not shared across parallel tests — the
// tests that use it do NOT call t.Parallel).
var p203URL string

func p203Scope() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: p203Tenant, User: p203User, Session: p203Session}
}

func p203InjConn(inj *prototypes.AgentConfigMCPCredentialInjectionDescriptor) prototypes.AgentConfigMCPConnectionDescriptor {
	return prototypes.AgentConfigMCPConnectionDescriptor{
		Name:      "recv",
		Transport: "http",
		URL:       p203URL,
		Injection: inj,
	}
}

// TestE2E_Phase203_OptInOff_InjectionRejected proves the fail-closed gate at the
// real wire surface: with the opt-in off, an add carrying an injection mapping is
// rejected loud and NOTHING is attached (no tool reaches the catalog).
func TestE2E_Phase203_OptInOff_InjectionRejected(t *testing.T) {
	h := newP203Harness(t, false)
	_, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: p203Scope(), AgentID: p203Agent,
		Connection: p203InjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
			Provider: p203Broker, Form: "header", Header: "x-vendor-api-key",
		}),
	})
	if !errors.Is(err, agentcfgprotocol.ErrWireInjectionNotAllowed) {
		t.Fatalf("want ErrWireInjectionNotAllowed with the opt-in off, got %v", err)
	}
	if _, ok := h.catalog.Resolve("recv_echo"); ok {
		t.Fatal("a gated-off add must register no tool")
	}
}

// TestE2E_Phase203_OptInOn_HeaderForm_OnlinePersistsInjectsRedacts is the
// happy-path end-to-end: the add comes online, the revision persists the
// injection mapping, and an invocation of the registered tool injects the
// per-user pulled value into the declared header — which the redactor holds to
// `***`.
func TestE2E_Phase203_OptInOn_HeaderForm_OnlinePersistsInjectsRedacts(t *testing.T) {
	h := newP203Harness(t, true)
	resp, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: p203Scope(), AgentID: p203Agent,
		Connection: p203InjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
			Provider: p203Broker, Form: "header", Header: "x-vendor-api-key",
		}),
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if resp.State != "online" {
		t.Fatalf("state = %q, want online (reason: %s)", resp.State, resp.Reason)
	}
	// The revision persists the non-secret injection mapping (diff / rollback).
	if resp.Revision == nil || resp.Revision.Payload.Connections == nil ||
		len(resp.Revision.Payload.Connections.Servers) != 1 ||
		resp.Revision.Payload.Connections.Servers[0].Injection == nil ||
		resp.Revision.Payload.Connections.Servers[0].Injection.Header != "x-vendor-api-key" {
		t.Fatalf("revision did not persist the injection mapping: %+v", resp.Revision)
	}

	// The attach actually wired injection: invoke the registered tool and observe
	// the per-user pulled value at the receiver.
	echo, ok := h.catalog.Resolve("recv_echo")
	if !ok {
		t.Fatal("recv_echo not registered after online add")
	}
	id := identity.Identity{TenantID: "t1", UserID: "alice", SessionID: "s1"}
	if _, err := echo.Invoke(p148Ctx(t, id, p203Agent), json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	calls := h.rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("receiver saw %d calls, want 1", len(calls))
	}
	if got := calls[0].headers["X-Vendor-Api-Key"]; got != "brokered-t1-alice" {
		t.Fatalf("injected header = %q, want brokered-t1-alice", got)
	}
	red := redactedPayload(t, calls[0])
	if v := red["headers"].(map[string]any)["X-Vendor-Api-Key"]; v != "***" {
		t.Fatalf("injected header not redacted: %v", v)
	}
}

// TestE2E_Phase203_OptInOn_PerUserIsolation proves two acting users get DISTINCT
// injected values through the wired-at-runtime connection (no cross-user bleed).
func TestE2E_Phase203_OptInOn_PerUserIsolation(t *testing.T) {
	h := newP203Harness(t, true)
	resp, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: p203Scope(), AgentID: p203Agent,
		Connection: p203InjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
			Provider: p203Broker, Form: "meta", MetaKey: "vendor.api_key",
		}),
	})
	if err != nil || resp.State != "online" {
		t.Fatalf("add: err=%v state=%s", err, resp.State)
	}
	echo, ok := h.catalog.Resolve("recv_echo")
	if !ok {
		t.Fatal("recv_echo not registered")
	}
	for _, id := range []identity.Identity{
		{TenantID: "t-a", UserID: "amy", SessionID: "s"},
		{TenantID: "t-b", UserID: "bob", SessionID: "s"},
	} {
		if _, err := echo.Invoke(p148Ctx(t, id, p203Agent), json.RawMessage(`{"text":"hi"}`)); err != nil {
			t.Fatalf("invoke(%s): %v", id.UserID, err)
		}
	}
	want := map[string]string{"amy": "brokered-t-a-amy", "bob": "brokered-t-b-bob"}
	for _, c := range h.rec.snapshot() {
		user, _ := c.meta["user"].(string)
		exp, ok := want[user]
		if !ok {
			t.Fatalf("unexpected user in _meta: %+v", c.meta)
		}
		vendor, _ := c.meta["vendor"].(map[string]any)
		if vendor == nil || vendor["api_key"] != exp {
			t.Fatalf("user %s: injected _meta = %v, want %q (cross-user bleed)", user, c.meta["vendor"], exp)
		}
	}
}

// TestE2E_Phase203_OptInOn_BrokerOutage_FailsLoud_NoWireCall proves the
// wired-at-runtime injection connection fails loud on a broker outage — no
// unauthenticated call leaks to the receiver.
func TestE2E_Phase203_OptInOn_BrokerOutage_FailsLoud_NoWireCall(t *testing.T) {
	h := newP203Harness(t, true)
	resp, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: p203Scope(), AgentID: p203Agent,
		Connection: p203InjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
			Provider: p203Broker, Form: "header", Header: "x-vendor-api-key",
		}),
	})
	if err != nil || resp.State != "online" {
		t.Fatalf("add: err=%v state=%s", err, resp.State)
	}
	echo, ok := h.catalog.Resolve("recv_echo")
	if !ok {
		t.Fatal("recv_echo not registered")
	}
	h.broker.setPosture("error500")
	id := identity.Identity{TenantID: "t1", UserID: "dave", SessionID: "s1"}
	if _, err := echo.Invoke(p148Ctx(t, id, p203Agent), json.RawMessage(`{"text":"hi"}`)); err == nil {
		t.Fatal("want error on broker outage, got nil (an unauthenticated call would leak)")
	}
	if calls := h.rec.snapshot(); len(calls) != 0 {
		t.Fatalf("broker refused but the receiver saw %d calls — an unauthenticated call leaked", len(calls))
	}
}
