package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
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
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// phase156_remove_connection_test.go drives `agent_config.remove_mcp_connection`
// + the run-start reconcile DETACH leg end-to-end with REAL drivers and a REAL
// stdio MCP subprocess fixture (cmd/harbor-mcptest-stdio, §17.8): add → tools
// visible → remove (a revision dropping the descriptor + pruning residue) →
// the next run's reconcile deregisters the server's tools from the catalog +
// MCP registry and closes the transport (goroutine baseline asserted).
// Rollback past an add detaches through the SAME reconcile path. Re-add after
// remove works end-to-end. Removing an unknown name fails loud (typed error).

const (
	rmTenant  = "tenant-rm"
	rmUser    = "admin-rm"
	rmSession = "sess-rm"
	rmAgent   = "agent-rm"
)

type rmHarness struct {
	handler  http.Handler
	registry agentcfg.Registry
	catalog  tools.ToolCatalog
	mcpReg   *mcpdrv.Registry
	detacher projection.ConnectionDetacher
	bus      events.EventBus
}

func newRmHarness(t *testing.T, binPath string) *rmHarness {
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
	// REAL attach + detach concretes (the devstack D-094 mirrors of cmd/harbor).
	attacher := devstack.NewMCPConnectionAttacher(cat, mcpReg, bus, nil,
		identity.Identity{TenantID: rmTenant, UserID: rmUser, SessionID: rmSession}, nil)
	detacher := devstack.NewMCPConnectionDetacher(cat, mcpReg, nil)
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(coord),
		agentcfgprotocol.WithStdioAllowlist([]string{binPath}),
		// A boot-declared server the remove verb must reject distinctly.
		agentcfgprotocol.WithBootDeclaredMCPServers([]string{"yaml-boot-srv"}),
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
	return &rmHarness{handler: h, registry: reg, catalog: cat, mcpReg: mcpReg, detacher: detacher, bus: bus}
}

func rmID() identity.Identity {
	return identity.Identity{TenantID: rmTenant, UserID: rmUser, SessionID: rmSession}
}
func rmQuad() identity.Quadruple { return identity.Quadruple{Identity: rmID()} }

func (h *rmHarness) post(t *testing.T, route string, body any, scopes []protoauth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/"+route, bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, rmTenant)
	req.Header.Set(stream.HeaderUser, rmUser)
	req.Header.Set(stream.HeaderSession, rmSession)
	req = req.WithContext(protoauth.WithScopes(req.Context(), scopes))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// rmFixtureSrv is the fixed source id the stdio fixture registers under.
const rmFixtureSrv = "mcptest"

func (h *rmHarness) addStdio(t *testing.T, binPath string) prototypes.AgentConfigAddMCPConnectionResponse {
	t.Helper()
	rec := h.post(t, "add_mcp_connection", prototypes.AgentConfigAddMCPConnectionRequest{
		AgentID:    rmAgent,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{Name: rmFixtureSrv, Transport: "stdio", Command: []string{binPath}},
	}, adminScopes())
	return decode[prototypes.AgentConfigAddMCPConnectionResponse](t, rec)
}

// reconcile runs the run-start detach leg exactly as the run loop would (the
// SAME projection helper both drivers call), with the boot-declared set.
func (h *rmHarness) reconcile(t *testing.T) int {
	t.Helper()
	n, err := projection.ReconcileConnections(context.Background(), h.registry, rmAgent, rmQuad(),
		h.detacher, map[string]struct{}{"yaml-boot-srv": {}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return n
}

// registryLists reports whether the MCP registry currently lists the fixture
// server (rmFixtureSrv).
func (h *rmHarness) registryLists(t *testing.T) bool {
	t.Helper()
	idCtx, err := identity.With(context.Background(), rmID())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	servers, _, lerr := h.mcpReg.ListServers(idCtx, mcpRegistryListFilterAll())
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	for _, s := range servers {
		if s.Name == rmFixtureSrv {
			return true
		}
	}
	return false
}

func (h *rmHarness) activePayload(t *testing.T) agentcfg.ConfigPayload {
	t.Helper()
	rev, _, err := h.registry.Active(context.Background(), rmQuad(), rmAgent, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	return rev.Payload
}

// TestE2E_AgentConfig_RemoveConnection_DetachOnReconcile is the headline E2E:
// add → visible → remove → next-turn reconcile detaches (catalog excludes +
// registry empty + transport closed, goroutine baseline) → re-add works.
func TestE2E_AgentConfig_RemoveConnection_DetachOnReconcile(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newRmHarness(t, binPath)

	baseGoroutines := runtime.NumGoroutine()

	// 1) Add + pause the server (residue), so remove has residue to prune.
	add := h.addStdio(t, binPath)
	if add.State != "online" || add.Revision == nil {
		t.Fatalf("add state = %q (reason=%q)", add.State, add.Reason)
	}
	if _, ok := h.catalog.Resolve("mcptest_echo"); !ok {
		t.Fatal("echo tool did not reach the catalog after add")
	}
	if !h.registryLists(t) {
		t.Fatal("registry does not list mcptest after add")
	}
	// Pin exposure residue that belongs to mcptest (a paused entry + the tool).
	expRec := h.post(t, "set_tool_exposure", prototypes.AgentConfigSetToolExposureRequest{
		AgentID: rmAgent,
		ToolExposure: prototypes.AgentConfigToolExposure{
			PausedServers: []string{"mcptest"},
			DisabledTools: []string{"mcptest_echo"},
		},
	}, adminScopes())
	if expRec.Code != http.StatusOK {
		t.Fatalf("set_tool_exposure: %d %s", expRec.Code, expRec.Body.String())
	}

	// 2) Remove the connection → a revision drops the descriptor + prunes residue.
	rmRec := h.post(t, "remove_mcp_connection",
		prototypes.AgentConfigRemoveMCPConnectionRequest{AgentID: rmAgent, Name: "mcptest"}, adminScopes())
	if rmRec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rmRec.Code, rmRec.Body.String())
	}
	p := h.activePayload(t)
	if p.Connections != nil && len(p.Connections.Servers) != 0 {
		t.Fatalf("descriptor not dropped: %#v", p.Connections)
	}
	if p.ToolExposure != nil && (len(p.ToolExposure.PausedServers) != 0 || len(p.ToolExposure.DisabledTools) != 0) {
		t.Fatalf("exposure residue not pruned: %#v", p.ToolExposure)
	}
	// Before the next run, the server is STILL attached (warm transport) — the
	// verb is a revision, not an imperative teardown.
	if !h.registryLists(t) {
		t.Fatal("remove verb tore down the transport (should be next-turn only)")
	}

	// 3) Next run's reconcile detaches: catalog excludes, registry empty,
	//    transport closed.
	if n := h.reconcile(t); n != 1 {
		t.Fatalf("reconcile detached %d, want 1", n)
	}
	if _, ok := h.catalog.Resolve("mcptest_echo"); ok {
		t.Error("echo tool still in catalog after detach")
	}
	if h.registryLists(t) {
		t.Error("registry still lists mcptest after detach")
	}
	// The subprocess drained: goroutines return to baseline.
	assertGoroutinesSettle(t, baseGoroutines)

	// A second reconcile is a no-op (idempotent).
	if n := h.reconcile(t); n != 0 {
		t.Fatalf("second reconcile detached %d, want 0 (idempotent)", n)
	}

	// 4) Re-add after remove works end-to-end (agent-bound state is not
	//    destroyed by remove — D-287 call 3; a re-add reuses completed consent
	//    where OAuth applies, here it simply re-attaches).
	readd := h.addStdio(t, binPath)
	if readd.State != "online" {
		t.Fatalf("re-add state = %q", readd.State)
	}
	if _, ok := h.catalog.Resolve("mcptest_echo"); !ok {
		t.Fatal("echo tool did not return to the catalog after re-add")
	}
}

// TestE2E_AgentConfig_RemoveConnection_RollbackPastAddDetaches proves the
// rollback-past-an-add path detaches through the SAME reconcile mechanism.
func TestE2E_AgentConfig_RemoveConnection_RollbackPastAddDetaches(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newRmHarness(t, binPath)

	// A pre-add baseline revision (empty connections) to roll back to.
	baseRec := h.post(t, "set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID: rmAgent, PromptLayers: prototypes.AgentConfigPromptLayers{Base: ptrStr("base")},
	}, adminScopes())
	baseRev := decode[prototypes.AgentConfigSetPromptLayersResponse](t, baseRec).Revision.RevisionID

	add := h.addStdio(t, binPath)
	if add.State != "online" {
		t.Fatalf("add state = %q", add.State)
	}
	if !h.registryLists(t) {
		t.Fatal("registry does not list mcptest after add")
	}

	// Roll back PAST the add → the descriptor is no longer declared.
	if _, err := h.registry.Rollback(context.Background(), rmQuad(), rmAgent, baseRev, agentcfg.ConfigScopeAgent); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// The next run's reconcile detaches through the same path.
	if n := h.reconcile(t); n != 1 {
		t.Fatalf("reconcile after rollback detached %d, want 1", n)
	}
	if h.registryLists(t) {
		t.Error("registry still lists mcptest after rollback-past-add detach")
	}
	if _, ok := h.catalog.Resolve("mcptest_echo"); ok {
		t.Error("echo tool still in catalog after rollback-past-add detach")
	}
}

// TestE2E_AgentConfig_RemoveConnection_FailureModes covers the two distinct
// loud errors (unknown name → 404, boot-declared name → 400) over the wire.
func TestE2E_AgentConfig_RemoveConnection_FailureModes(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newRmHarness(t, binPath)

	// Unknown name → 404 not-found.
	unk := h.post(t, "remove_mcp_connection",
		prototypes.AgentConfigRemoveMCPConnectionRequest{AgentID: rmAgent, Name: "ghost"}, adminScopes())
	if unk.Code != http.StatusNotFound {
		t.Fatalf("unknown-name status = %d, want 404; body=%s", unk.Code, unk.Body.String())
	}
	var uerr protoerrors.Error
	_ = json.Unmarshal(unk.Body.Bytes(), &uerr)
	if uerr.Code != protoerrors.CodeNotFound {
		t.Errorf("unknown-name code = %q, want %q", uerr.Code, protoerrors.CodeNotFound)
	}

	// Boot-declared name → 400 (a DISTINCT status from the unknown-name 404).
	boot := h.post(t, "remove_mcp_connection",
		prototypes.AgentConfigRemoveMCPConnectionRequest{AgentID: rmAgent, Name: "yaml-boot-srv"}, adminScopes())
	if boot.Code != http.StatusBadRequest {
		t.Fatalf("boot-declared status = %d, want 400; body=%s", boot.Code, boot.Body.String())
	}

	// Non-admin caller → 403 (the whole family is admin-scoped).
	nonAdmin := h.post(t, "remove_mcp_connection",
		prototypes.AgentConfigRemoveMCPConnectionRequest{AgentID: rmAgent, Name: "x"}, nil)
	if nonAdmin.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", nonAdmin.Code)
	}
}

// TestE2E_AgentConfig_RemoveConnection_InFlightCallFailsLoud pins the HONEST
// in-flight semantics (the PR-464 adversarial-review amendment to the "keeps
// its snapshot" claim): teardown is process-global, so a run mid-flight when
// its server is removed + reconciled from ANOTHER session sees its next call
// fail LOUDLY — a typed catalog not-found at dispatch, and a typed
// closed-transport error on an already-resolved descriptor — never a hang, a
// panic, or a silent success.
func TestE2E_AgentConfig_RemoveConnection_InFlightCallFailsLoud(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newRmHarness(t, binPath)

	if add := h.addStdio(t, binPath); add.State != "online" {
		t.Fatalf("add state = %q", add.State)
	}

	// Simulate an in-flight run's dispatch state: the run has already resolved
	// the descriptor from the live catalog (as the dispatch executor does per
	// step) and holds the run-scoped identity ctx.
	desc, ok := h.catalog.Resolve("mcptest_echo")
	if !ok {
		t.Fatal("echo tool not resolvable before remove")
	}
	inFlightCtx, err := identity.With(context.Background(), rmID())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	// Prove the captured descriptor works BEFORE the detach.
	if _, ierr := desc.Invoke(inFlightCtx, json.RawMessage(`{"message":"pre-detach"}`)); ierr != nil {
		t.Fatalf("pre-detach invoke failed: %v", ierr)
	}

	// Remove the connection, then reconcile from a DIFFERENT session (the
	// cross-session teardown: the catalog + registry are process-global).
	if rec := h.post(t, "remove_mcp_connection",
		prototypes.AgentConfigRemoveMCPConnectionRequest{AgentID: rmAgent, Name: rmFixtureSrv}, adminScopes()); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	otherSession := identity.Quadruple{Identity: identity.Identity{TenantID: rmTenant, UserID: rmUser, SessionID: "sess-rm-other"}}
	if _, rerr := projection.ReconcileConnections(context.Background(), h.registry, rmAgent, otherSession, h.detacher, nil); rerr != nil {
		t.Fatalf("cross-session reconcile: %v", rerr)
	}

	// Loud shape 1 — dispatch-time resolve: the tool is gone from the shared
	// catalog, so the executor's Resolve misses (its loud tool-not-found path).
	if _, stillThere := h.catalog.Resolve("mcptest_echo"); stillThere {
		t.Fatal("echo tool still resolvable after cross-session detach")
	}

	// Loud shape 2 — an already-resolved descriptor (the in-flight run's
	// current step): the invoke returns a TYPED closed-transport error,
	// bounded in time — never a hang, a panic, or a silent success.
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("PANIC invoking detached server: %v", r)
			}
		}()
		_, ierr := desc.Invoke(inFlightCtx, json.RawMessage(`{"message":"post-detach"}`))
		done <- ierr
	}()
	select {
	case ierr := <-done:
		if ierr == nil {
			t.Fatal("post-detach invoke silently succeeded — want a loud typed error")
		}
		if !errors.Is(ierr, mcpdrv.ErrNotConnected) {
			t.Fatalf("post-detach invoke error = %v, want a typed mcpdrv.ErrNotConnected", ierr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("post-detach invoke HUNG — the loud-failure contract requires a prompt typed error")
	}
}

// TestE2E_AgentConfig_RemoveConnection_ConcurrentReconciles proves N≥10
// concurrent run-start reconciles during/after a remove are race-free and
// converge (at-most-once effective detach; the registry + catalog stay
// consistent). Exposure is next-turn; an in-flight run calling the detached
// server fails loudly (see ..._InFlightCallFailsLoud).
func TestE2E_AgentConfig_RemoveConnection_ConcurrentReconciles(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newRmHarness(t, binPath)

	if add := h.addStdio(t, binPath); add.State != "online" {
		t.Fatalf("add state = %q", add.State)
	}
	// Remove (revision only; transport still warm).
	if rec := h.post(t, "remove_mcp_connection",
		prototypes.AgentConfigRemoveMCPConnectionRequest{AgentID: rmAgent, Name: "mcptest"}, adminScopes()); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d", rec.Code)
	}

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			if _, err := projection.ReconcileConnections(context.Background(), h.registry, rmAgent, rmQuad(),
				h.detacher, nil); err != nil {
				t.Errorf("concurrent reconcile: %v", err)
			}
		}()
	}
	wg.Wait()

	// Final state converged: the server is fully detached.
	if h.registryLists(t) {
		t.Error("registry still lists mcptest after concurrent reconciles")
	}
	if _, ok := h.catalog.Resolve("mcptest_echo"); ok {
		t.Error("echo tool still in catalog after concurrent reconciles")
	}
}

// assertGoroutinesSettle waits for goroutines to return near a baseline (the
// stdio subprocess drains on transport close — the teardown proof).
func assertGoroutinesSettle(t *testing.T, base int) {
	t.Helper()
	for range 100 {
		runtime.GC()
		if runtime.NumGoroutine() <= base+3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutines did not settle to baseline after detach: base=%d now=%d", base, runtime.NumGoroutine())
}

func ptrStr(s string) *string { return &s }
