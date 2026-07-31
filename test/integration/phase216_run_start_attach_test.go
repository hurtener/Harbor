package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// phase216_run_start_attach_test.go drives the run-start ATTACH leg end-to-end
// with REAL drivers (in-memory state, the real agent-config registry over it,
// the real MCP registry + tool catalog, the real production attacher / detacher,
// a real in-memory bus, a real patterns redactor) against a SPEC-DERIVED MCP
// fixture built on the official go-sdk (§17.8 — never a hand-authored
// transcript).
//
// Legs:
//  1. restart survival — add through the REAL wire handler, then drop and
//     rebuild the runtime-side registry + catalog on the SAME state store; the
//     first run-start reconcile re-attaches and the tool is back;
//  2. rollback re-declare — add → remove → reconcile (detaches) → rollback →
//     reconcile (re-attaches), through the SAME leg;
//  3. identity propagation — the registration carries the reconciling
//     (tenant, agent) owner, the events carry the run quadruple, and a
//     cross-tenant run's sweep touches neither;
//  4. failure mode A — the fixture is stopped: the re-attach fails, the event
//     fires with a scrubbed reason, and the sweep still completes;
//  5. failure mode B — boot policy tightened (the injection opt-in flipped off):
//     the re-attach is refused with the typed sentinel, nothing is registered;
//  6. an N=16 concurrent cross-owner stress through the real seam.

const (
	raTenant  = "tenant-ra"
	raUser    = "admin-ra"
	raSession = "sess-ra"
	raAgent   = "agent-ra"
	raSrv     = "ra-fixture"
)

func raID() identity.Identity {
	return identity.Identity{TenantID: raTenant, UserID: raUser, SessionID: raSession}
}

func raQuad() identity.Quadruple {
	return identity.Quadruple{Identity: raID(), RunID: "run-ra-1"}
}

// raHarness holds the REAL collaborators. The state store is deliberately held
// separately from the runtime-side registry + catalog, because the restart leg
// rebuilds the latter while keeping the former.
type raHarness struct {
	state   state.StateStore
	bus     events.EventBus
	cfgReg  agentcfg.Registry
	handler http.Handler

	// The runtime-side pair the restart leg rebuilds.
	catalog   tools.ToolCatalog
	mcpReg    *mcpdrv.Registry
	attacher  *serve.MCPConnectionAttacher
	detacher  *serve.MCPConnectionDetacher
	stdioAllw []string
	allowInj  bool
}

// raFixture spins a spec-derived MCP server (official go-sdk) behind a
// streamable-HTTP httptest server exposing one `echo` tool.
//
// CREATE EVERY FIXTURE BEFORE newRaHarness. httptest's Close waits for its
// outstanding connections, and an attached MCP provider holds a long-lived
// server→client stream open. Because cleanups run LIFO, the harness's cleanup
// (which drains the attacher) must be registered LAST so it runs FIRST.
func raFixture(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-phase216-fixture", Version: "v0"}, nil)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "echo",
			Description: "echo",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"text": map[string]any{"type": "string"}},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in struct {
			Text string `json:"text"`
		}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Text}}}, nil, nil
		},
	)
	hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
	return hs
}

func newRaHarness(t *testing.T) *raHarness {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 512,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	cfgReg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg registry: %v", err)
	}
	h := &raHarness{state: st, bus: bus, cfgReg: cfgReg, allowInj: true}
	// Registered AFTER every fixture server's own Close (callers create fixtures
	// FIRST — see the note on raFixture): cleanups run LIFO, so the attacher
	// drains its live transports here BEFORE httptest waits on those same
	// connections in its Close.
	t.Cleanup(func() {
		if h.attacher != nil {
			_ = h.attacher.Close(context.Background())
		}
		_ = cfgReg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	h.bootRuntimeSide(t)
	return h
}

// bootRuntimeSide builds (or REBUILDS) the process-local runtime side — the MCP
// registry, the tool catalog, the attacher and the detacher — over the SAME
// state store and agent-config registry. Calling it a second time is the
// restart: everything the live registry held is gone; only the revision spine
// survives.
func (h *raHarness) bootRuntimeSide(t *testing.T) {
	t.Helper()
	if h.attacher != nil {
		_ = h.attacher.Close(context.Background())
	}
	h.catalog = tools.NewCatalog()
	h.mcpReg = mcpdrv.NewRegistry()
	h.attacher = serve.NewMCPConnectionAttacher(h.catalog, h.mcpReg, h.bus, nil, raID(), nil, nil, nil,
		serve.WithReattachGates(h.stdioAllw, h.allowInj),
		serve.WithReattachTimeout(20*time.Second))
	h.detacher = serve.NewMCPConnectionDetacher(h.catalog, h.mcpReg, nil)

	svc, err := agentcfgprotocol.NewService(h.cfgReg,
		agentcfgprotocol.WithBus(h.bus),
		agentcfgprotocol.WithConnectionAttacher(h.attacher),
		agentcfgprotocol.WithCoordinator(pauseresume.New(pauseresume.WithBus(h.bus))),
		agentcfgprotocol.WithStdioAllowlist(h.stdioAllw),
		agentcfgprotocol.WithAllowWireInjection(h.allowInj),
	)
	if err != nil {
		t.Fatalf("agent-config service: %v", err)
	}
	handler, err := stream.NewAgentConfigHandler(svc)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	h.handler = handler
}

func (h *raHarness) post(t *testing.T, route string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/"+route, bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, raTenant)
	req.Header.Set(stream.HeaderUser, raUser)
	req.Header.Set(stream.HeaderSession, raSession)
	req = req.WithContext(protoauth.WithScopes(req.Context(), adminScopes()))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// addHTTP adds a connection through the REAL admin wire handler.
func (h *raHarness) addHTTP(t *testing.T, name, url string) prototypes.AgentConfigAddMCPConnectionResponse {
	t.Helper()
	rec := h.post(t, "add_mcp_connection", prototypes.AgentConfigAddMCPConnectionRequest{
		AgentID: raAgent,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{
			Name: name, Transport: "http", URL: url,
		},
	})
	resp := decode[prototypes.AgentConfigAddMCPConnectionResponse](t, rec)
	if resp.State != "online" {
		t.Fatalf("add state = %q (reason=%q), want online", resp.State, resp.Reason)
	}
	return resp
}

// reconcile runs the run-start reconcile exactly as the run loop does — the SAME
// projection helper both drivers call, with BOTH concretes wired.
func (h *raHarness) reconcile(t *testing.T, q identity.Quadruple, agentID string) (detached, attached int, err error) {
	t.Helper()
	return projection.ReconcileConnections(context.Background(), h.cfgReg, agentID, q,
		h.detacher, h.attacher, nil)
}

// raCollector subscribes to the bus for one identity and collects events.
type raCollector struct {
	mu   sync.Mutex
	seen []events.Event
}

func newRaCollector(t *testing.T, bus events.EventBus, id identity.Identity) *raCollector {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	c := &raCollector{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sub.Events() {
			c.mu.Lock()
			c.seen = append(c.seen, ev)
			c.mu.Unlock()
		}
	}()
	t.Cleanup(func() { sub.Cancel(); <-done })
	return c
}

func (c *raCollector) await(t *testing.T, what string, pred func([]events.Event) bool) []events.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		snap := append([]events.Event(nil), c.seen...)
		c.mu.Unlock()
		if pred(snap) {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return nil
}

func raPayloads(evs []events.Event, typ events.EventType) []agentcfg.MCPConnectionLifecyclePayload {
	var out []agentcfg.MCPConnectionLifecyclePayload
	for _, e := range evs {
		if e.Type != typ {
			continue
		}
		if p, ok := e.Payload.(agentcfg.MCPConnectionLifecyclePayload); ok {
			out = append(out, p)
		}
	}
	return out
}

// TestE2E_Phase216_RestartSurvival — a connection added through the real admin
// verb comes BACK at the first run start of a fresh runtime on the same state
// store: its tools are in the catalog and the MCP registry lists it.
func TestE2E_Phase216_RestartSurvival(t *testing.T) {
	fixture := raFixture(t)
	t.Cleanup(fixture.Close)
	h := newRaHarness(t)

	h.addHTTP(t, raSrv, fixture.URL)
	if _, ok := h.catalog.Resolve(raSrv + "_echo"); !ok {
		t.Fatal("the add did not register the fixture's tool")
	}

	// THE RESTART: a fresh process — new MCP registry, new catalog, new attacher
	// — over the SAME state store. Nothing but the revision spine survives.
	h.bootRuntimeSide(t)
	if _, ok := h.catalog.Resolve(raSrv + "_echo"); ok {
		t.Fatal("the rebuilt catalog is not actually fresh — the restart is not being simulated")
	}
	if _, exists := h.mcpReg.OwnerOf(raSrv); exists {
		t.Fatal("the rebuilt MCP registry is not actually fresh")
	}

	coll := newRaCollector(t, h.bus, raID())

	// The FIRST run start re-attaches.
	detached, attached, err := h.reconcile(t, raQuad(), raAgent)
	if err != nil {
		t.Fatalf("first run-start reconcile after restart: %v", err)
	}
	if detached != 0 || attached != 1 {
		t.Fatalf("detached=%d attached=%d, want 0/1", detached, attached)
	}

	// The tool is back in the projected catalog.
	d, ok := h.catalog.Resolve(raSrv + "_echo")
	if !ok {
		t.Fatal("the re-attached server's tool is NOT back in the catalog — restart survival is broken")
	}
	if d.Tool.Transport != tools.TransportMCP {
		t.Fatalf("resolved tool transport = %q, want mcp", d.Tool.Transport)
	}
	// And it is genuinely live: the tool answers.
	callCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	callCtx, idErr := identity.With(callCtx, raID())
	if idErr != nil {
		t.Fatalf("identity.With: %v", idErr)
	}
	res, callErr := d.Invoke(callCtx, []byte(`{"text":"back online"}`))
	if callErr != nil {
		t.Fatalf("calling the re-attached tool: %v", callErr)
	}
	if v, ok := res.Value.(mcpdrv.MCPToolValue); !ok || v.Text != "back online" {
		t.Fatalf("re-attached tool returned %+v, want the echoed text", res.Value)
	}

	// mcp.servers.list's underlying registry read reports it.
	servers, _, lerr := h.mcpReg.ListServers(mustIdentityCtx(t, raTenant, raUser, raSession), mcpdrv.ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	found := false
	for _, s := range servers {
		if s.Name == raSrv {
			found = true
			if s.State != mcpdrv.ServerStateOnline {
				t.Fatalf("re-attached server state = %q, want online", s.State)
			}
			if s.ToolCount == 0 {
				t.Fatal("re-attached server reports zero tools")
			}
		}
	}
	if !found {
		t.Fatal("the re-attached server is not in the registry listing")
	}

	// IDENTITY PROPAGATION: the registration carries the reconciling owner, and
	// the event carries the reconciling RUN.
	owner, ok := h.mcpReg.OwnerOf(raSrv)
	if !ok {
		t.Fatal("no owner tag on the re-attached registration")
	}
	if want := (toolauth.Owner{Tenant: raTenant, Agent: raAgent}); owner != want {
		t.Fatalf("owner = %+v, want %+v", owner, want)
	}
	seen := coll.await(t, "the reattached event", func(e []events.Event) bool {
		return len(raPayloads(e, agentcfg.EventTypeMCPConnectionReattached)) >= 1
	})
	p := raPayloads(seen, agentcfg.EventTypeMCPConnectionReattached)[0]
	if p.Author.RunID != raQuad().RunID {
		t.Fatalf("event Author.RunID = %q, want the reconciling run's %q", p.Author.RunID, raQuad().RunID)
	}
	if p.Author.TenantID != raTenant || p.AgentID != raAgent || p.ServerID != raSrv {
		t.Fatalf("event identity = %+v, want (tenant=%s agent=%s server=%s)", p, raTenant, raAgent, raSrv)
	}

	// A SECOND run start is a clean no-op — no transport churn, no second event.
	if _, attached2, err := h.reconcile(t, raQuad(), raAgent); err != nil || attached2 != 0 {
		t.Fatalf("second reconcile: attached=%d err=%v, want 0,nil (already live)", attached2, err)
	}
}

// TestE2E_Phase216_CrossTenantSweepTouchesNothing — a DIFFERENT tenant's run
// start neither attaches nor detaches the first tenant's connection.
func TestE2E_Phase216_CrossTenantSweepTouchesNothing(t *testing.T) {
	fixture := raFixture(t)
	t.Cleanup(fixture.Close)
	h := newRaHarness(t)
	h.addHTTP(t, raSrv, fixture.URL)
	h.bootRuntimeSide(t)

	// Tenant B reconciles the SAME agent name over the SAME live registry.
	other := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-other-ra", UserID: raUser, SessionID: raSession},
		RunID:    "run-other",
	}
	detached, attached, err := h.reconcile(t, other, raAgent)
	if err != nil {
		t.Fatalf("cross-tenant reconcile: %v", err)
	}
	if detached != 0 || attached != 0 {
		t.Fatalf("cross-tenant sweep detached=%d attached=%d, want 0/0 (owner isolation)", detached, attached)
	}
	if _, exists := h.mcpReg.OwnerOf(raSrv); exists {
		t.Fatal("the cross-tenant sweep attached tenant-ra's connection")
	}

	// Tenant A's own run start still works after B's sweep.
	if _, attachedA, err := h.reconcile(t, raQuad(), raAgent); err != nil || attachedA != 1 {
		t.Fatalf("owner A reconcile after B's sweep: attached=%d err=%v, want 1,nil", attachedA, err)
	}
	owner, _ := h.mcpReg.OwnerOf(raSrv)
	if want := (toolauth.Owner{Tenant: raTenant, Agent: raAgent}); owner != want {
		t.Fatalf("owner = %+v, want %+v", owner, want)
	}
}

// TestE2E_Phase216_RollbackReDeclareReAttaches — add → remove → reconcile
// (detaches) → rollback → reconcile (re-attaches). One mechanism, N triggers.
func TestE2E_Phase216_RollbackReDeclareReAttaches(t *testing.T) {
	fixture := raFixture(t)
	t.Cleanup(fixture.Close)
	h := newRaHarness(t)

	addResp := h.addHTTP(t, raSrv, fixture.URL)
	if addResp.Revision == nil {
		t.Fatal("the add recorded no revision")
	}
	declaringRevision := addResp.Revision.RevisionID

	// REMOVE — a new revision dropping the descriptor.
	rec := h.post(t, "remove_mcp_connection", prototypes.AgentConfigRemoveMCPConnectionRequest{
		AgentID: raAgent, Name: raSrv,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: status %d body %s", rec.Code, rec.Body.String())
	}

	// The next run start DETACHES.
	detached, attached, err := h.reconcile(t, raQuad(), raAgent)
	if err != nil {
		t.Fatalf("reconcile after remove: %v", err)
	}
	if detached != 1 || attached != 0 {
		t.Fatalf("after remove: detached=%d attached=%d, want 1/0", detached, attached)
	}
	if _, ok := h.catalog.Resolve(raSrv + "_echo"); ok {
		t.Fatal("the removed server's tool is still in the catalog")
	}

	// ROLLBACK to the revision that DECLARED it.
	rec = h.post(t, "rollback", prototypes.AgentConfigRollbackRequest{
		AgentID: raAgent, RevisionID: declaringRevision,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback: status %d body %s", rec.Code, rec.Body.String())
	}

	// The following run start RE-ATTACHES through the SAME leg.
	detached, attached, err = h.reconcile(t, raQuad(), raAgent)
	if err != nil {
		t.Fatalf("reconcile after rollback: %v", err)
	}
	if detached != 0 || attached != 1 {
		t.Fatalf("after rollback: detached=%d attached=%d, want 0/1", detached, attached)
	}
	if _, ok := h.catalog.Resolve(raSrv + "_echo"); !ok {
		t.Fatal("the rolled-back-to connection's tool is NOT back in the catalog")
	}
}

// TestE2E_Phase216_FailureModes — two failure modes, each loud, classified,
// reported, and NON-FATAL: the sweep completes and nothing is half-registered.
func TestE2E_Phase216_FailureModes(t *testing.T) {
	t.Run("failure mode A: the declared server is gone", func(t *testing.T) {
		// Both fixtures first (see raFixture's note on cleanup ordering). The
		// second one WILL come back, so the sweep's "one failure must not strand
		// the rest" property is observable.
		fixture := raFixture(t)
		alive := raFixture(t)
		t.Cleanup(alive.Close)
		h := newRaHarness(t)
		h.addHTTP(t, raSrv, fixture.URL)
		h.addHTTP(t, "ra-alive", alive.URL)

		h.bootRuntimeSide(t)
		fixture.Close() // the third party is now gone.

		coll := newRaCollector(t, h.bus, raID())
		_, attached, err := h.reconcile(t, raQuad(), raAgent)
		if err == nil {
			t.Fatal("an unreachable declared server must produce a loud (non-fatal) error")
		}
		if !errors.Is(err, projection.ErrReconcileReattach) {
			t.Fatalf("err = %v, want it marked as a re-attach failure", err)
		}
		// The OTHER connection still came back — no early abort.
		if attached != 1 {
			t.Fatalf("attached = %d, want 1 (one refused server must not strand the rest)", attached)
		}
		if _, ok := h.catalog.Resolve("ra-alive_echo"); !ok {
			t.Fatal("the reachable connection did not come back")
		}
		// Nothing half-registered for the dead one.
		if _, exists := h.mcpReg.OwnerOf(raSrv); exists {
			t.Fatal("a failed re-attach left a registration behind")
		}
		// REPORTED with a scrubbed reason.
		seen := coll.await(t, "the reattach_failed event", func(e []events.Event) bool {
			return len(raPayloads(e, agentcfg.EventTypeMCPConnectionReattachFailed)) >= 1
		})
		p := raPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)[0]
		if p.State != agentcfg.MCPReattachClassTransportFailed {
			t.Fatalf("class = %q, want %q", p.State, agentcfg.MCPReattachClassTransportFailed)
		}
		if p.Reason == "" {
			t.Fatal("a reported failure carries no reason")
		}
	})

	t.Run("failure mode B: boot policy tightened", func(t *testing.T) {
		alive := raFixture(t)
		t.Cleanup(alive.Close)
		h := newRaHarness(t)

		// A revision declaring BOTH a plain connection and one carrying a
		// credential-injection mapping — the state an admin left behind while the
		// fail-closed opt-in was ON. It is written on the real registry rather than
		// through the add door, because the add door additionally requires the
		// named broker to resolve live; what this leg re-reads is the SPINE, and
		// the spine is what a restart with a tightened policy meets.
		if _, err := h.cfgReg.SetRevision(context.Background(), identity.Quadruple{Identity: raID()},
			raAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
				Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
					{Name: "ra-plain", Transport: agentcfg.MCPTransportHTTP, URL: alive.URL},
					{
						Name: "ra-inj", Transport: agentcfg.MCPTransportHTTP, URL: alive.URL,
						Injection: &agentcfg.MCPCredentialInjectionDescriptor{
							Provider: "some-broker", Form: "header", Header: "x-downstream-api-key",
						},
					},
				}},
			}, agentcfg.SetOptions{}); err != nil {
			t.Fatalf("seed the injection-carrying revision: %v", err)
		}

		// RESTART with the opt-in flipped OFF — the kill-switch case.
		h.allowInj = false
		h.bootRuntimeSide(t)

		coll := newRaCollector(t, h.bus, raID())
		_, attached, err := h.reconcile(t, raQuad(), raAgent)
		if err == nil {
			t.Fatal("a kill-switched descriptor must be refused loud")
		}
		if !errors.Is(err, serve.ErrReattachInjectionDisabled) {
			t.Fatalf("err = %v, want ErrReattachInjectionDisabled", err)
		}
		// The kill-switch is SURGICAL: the sibling plain connection still came
		// back. A gate that refused the whole sweep would be a different bug.
		if attached != 1 {
			t.Fatalf("attached = %d, want 1 (the plain sibling must still re-attach)", attached)
		}
		if _, exists := h.mcpReg.OwnerOf("ra-inj"); exists {
			t.Fatal("a kill-switched re-attach registered the server anyway")
		}
		if _, exists := h.mcpReg.OwnerOf("ra-plain"); !exists {
			t.Fatal("the plain sibling did not re-attach")
		}
		seen := coll.await(t, "the reattach_failed event", func(e []events.Event) bool {
			return len(raPayloads(e, agentcfg.EventTypeMCPConnectionReattachFailed)) >= 1
		})
		if got := raPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)[0].State; got != agentcfg.MCPReattachClassInjectionDisabled {
			t.Fatalf("class = %q, want %q", got, agentcfg.MCPReattachClassInjectionDisabled)
		}
	})
}

// TestE2E_Phase216_ConcurrentCrossOwnerSweeps is the concurrency stress through
// the REAL seam: N=16 interleaved run starts for TWO owners against ONE shared
// attacher + registry. Each owner's connection is attached exactly once, under
// its own owner tag, with no cross-owner bleed.
func TestE2E_Phase216_ConcurrentCrossOwnerSweeps(t *testing.T) {
	fixA := raFixture(t)
	t.Cleanup(fixA.Close)
	fixB := raFixture(t)
	t.Cleanup(fixB.Close)

	h := newRaHarness(t)
	// Two agents under one tenant — the co-tenant cross-owner case.
	h.addHTTP(t, "ra-conc-a", fixA.URL)
	seedAgentConnection(t, h, "agent-ra-b", "ra-conc-b", fixB.URL)

	h.bootRuntimeSide(t)

	const N = 16
	var wg sync.WaitGroup
	errsCh := make(chan error, 2*N)
	for range N {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, _, err := h.reconcile(t, raQuad(), raAgent); err != nil {
				errsCh <- fmt.Errorf("owner A: %w", err)
			}
		}()
		go func() {
			defer wg.Done()
			q := identity.Quadruple{Identity: raID(), RunID: "run-b"}
			if _, _, err := h.reconcile(t, q, "agent-ra-b"); err != nil {
				errsCh <- fmt.Errorf("owner B: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		t.Fatalf("concurrent cross-owner sweep failed: %v", err)
	}

	// Exactly one live registration per connection, each under its OWN owner.
	for name, agent := range map[string]string{"ra-conc-a": raAgent, "ra-conc-b": "agent-ra-b"} {
		owner, ok := h.mcpReg.OwnerOf(name)
		if !ok {
			t.Fatalf("%q is not live after the concurrent sweeps", name)
		}
		if want := (toolauth.Owner{Tenant: raTenant, Agent: agent}); owner != want {
			t.Fatalf("%q owner = %+v, want %+v (cross-owner bleed)", name, owner, want)
		}
		if _, ok := h.catalog.Resolve(name + "_echo"); !ok {
			t.Fatalf("%q's tool is not in the catalog", name)
		}
	}
	servers, _, lerr := h.mcpReg.ListServers(mustIdentityCtx(t, raTenant, raUser, raSession), mcpdrv.ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	counts := map[string]int{}
	for _, s := range servers {
		counts[s.Name]++
	}
	if counts["ra-conc-a"] != 1 || counts["ra-conc-b"] != 1 {
		t.Fatalf("registration counts = %v, want exactly 1 each across %d concurrent run starts", counts, N)
	}
}

// seedAgentConnection writes a declaring revision for a SECOND agent directly on
// the real registry (the admin wire handler is bound to one agent per request;
// this is the same descriptor shape it would persist).
func seedAgentConnection(t *testing.T, h *raHarness, agentID, name, url string) {
	t.Helper()
	if _, err := h.cfgReg.SetRevision(context.Background(), identity.Quadruple{Identity: raID()},
		agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{
				Name: name, Transport: agentcfg.MCPTransportHTTP, URL: url,
			}}},
		}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed %q for %q: %v", name, agentID, err)
	}
}
