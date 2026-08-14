package serve

// boot_ownership_test.go — the HA-66 boot-ownership mux-boundary wiring:
// `bootOwnershipMux` binds the immutable boot-pack index into every
// `/v1/agent_config/*` request context, and the agent-pack mutation
// guards fire through the REAL agent-config service + transport handler
// when the owner is wired — while the identical request WITHOUT the
// wrap stays fully mutable (guards inert, no boot baseline bound).
//
// This is the serve-band proof of the integration-owner seam the
// agentcfg/protocol boot-pack guards document (bootpack_guards.go:
// "The integration owner wires the concrete reader at the handler
// boundary"). The base stream-package tests prove the guard behavior
// given a reader in the request context; this test proves the serve
// band actually PUTS the reader there.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentcfg "github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

// bootOwnerFixture is the minimal boot-ownership reader for the
// middleware tests: an exact (tenant, agent, canonical-name) ownership
// set mirroring the eager bootpacks.Index OwnsName key.
type bootOwnerFixture map[string]bool

func bootOwnerKey(tenantID, agentID, name string) string {
	return tenantID + "\x00" + agentID + "\x00" + skills.CanonicalPackName(name)
}

func (o bootOwnerFixture) OwnsName(tenantID, agentID, name string) bool {
	return o[bootOwnerKey(tenantID, agentID, name)]
}

func bootOwnerFor(tenantID, agentID string, names ...string) bootOwnerFixture {
	o := bootOwnerFixture{}
	for _, n := range names {
		o[bootOwnerKey(tenantID, agentID, n)] = true
	}
	return o
}

// bootTestAgent is the agent the boot-ownership fixture activates.
const bootTestAgent = "agent-x"

// bootTestID is the fixture's verified triple.
func bootTestID() *identity.Identity {
	return &identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
}

// bootOwnershipFixture builds the REAL agent-config service stack the
// upsert route dispatches into (state + bus + registry + proposal
// ledger + catalog), with the agent's lifecycle seeded.
type bootOwnershipFixture struct {
	svc *agentcfgprotocol.Service
}

func newBootOwnershipFixture(t *testing.T) bootOwnershipFixture {
	t.Helper()
	ctx := context.Background()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(ctx)
		_ = bus.Close(ctx)
		_ = st.Close(ctx)
	})
	if _, err := reg.SetRevision(ctx, identity.Quadruple{Identity: *bootTestID()}, bootTestAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("activate agent lifecycle: %v", err)
	}
	catalog := tools.NewCatalog()
	if err := catalog.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "guard_tool", Transport: tools.TransportInProcess, SideEffects: tools.SideEffectRead, Loading: tools.LoadingAlways},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("register catalog tool: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithAgentPackProposalState(st),
		agentcfgprotocol.WithAgentPackCatalog(catalog),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return bootOwnershipFixture{svc: svc}
}

// bootRequest issues an identity-headered, admin-scoped POST through the
// given handler. The boot-ownership reader is NOT manually bound — the
// request carries ONLY the identity/scope/reach ctx the caller mints, so
// the test proves the middleware (not the request) supplies the reader.
func bootRequest(t *testing.T, h http.Handler, route, body string, id *identity.Identity) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/"+route, strings.NewReader(body))
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	req = req.WithContext(auth.WithScopes(req.Context(), []auth.Scope{auth.ScopeAdmin}))
	req = req.WithContext(auth.WithAgentReach(req.Context(), []string{bootTestAgent}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func bootErrCode(t *testing.T, body []byte) protoerrors.Code {
	t.Helper()
	var envelope struct {
		Code protoerrors.Code `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, body)
	}
	return envelope.Code
}

// TestBootOwnershipMux_BindsReaderAndGuardsFire is the load-bearing
// serve-band proof: a request routed through `bootOwnershipMux` (the
// ONLY place the reader enters the request ctx in production) makes the
// REAL upsert guard refuse a boot-owned name with the canonical typed
// 400 — and the IDENTICAL request against the unwrapped handler stays
// fully mutable, proving the middleware wrap is what wires the guard.
func TestBootOwnershipMux_BindsReaderAndGuardsFire(t *testing.T) {
	f := newBootOwnershipFixture(t)
	h, err := stream.NewAgentConfigHandler(f.svc,
		stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	owner := bootOwnerFor("t1", bootTestAgent, "playbook")
	wrapped := bootOwnershipMux(owner, h)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","skill":{"name":"playbook","trigger":"trigger","steps":["step"]}}`

	// (a) Through the middleware wrap the boot-owned upsert is the typed
	//     400 read-only refusal naming the pack — the reader reached the
	//     service guard via the request ctx.
	code, resp := bootRequest(t, wrapped, "agent_packs/upsert", body, bootTestID())
	if code != http.StatusBadRequest {
		t.Fatalf("boot-owned upsert through bootOwnershipMux status = %d body=%s, want 400", code, resp)
	}
	if c := bootErrCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("boot-owned upsert code = %q, want %q", c, protoerrors.CodeInvalidRequest)
	}
	if !strings.Contains(string(resp), "playbook") {
		t.Fatalf("boot-owned upsert message %s does not name the owned pack", resp)
	}

	// (b) Control — the SAME request against the UNWRAPPED handler (no
	//     reader bound) stays fully mutable: the guards are inert when no
	//     boot baseline is wired. This is the "nil owner keeps the guards
	//     inert" posture, proven against the real service path.
	code, resp = bootRequest(t, h, "agent_packs/upsert", body, bootTestID())
	if code != http.StatusOK {
		t.Fatalf("boot-owned upsert WITHOUT the middleware wrap status = %d body=%s, want 200 (guards inert when no reader is bound)", code, resp)
	}

	// (c) Control — a name the baseline does not own stays mutable through
	//     the wrapped mux (the reader is bound but only refuses owned
	//     names; a free name is never blocked).
	free := strings.Replace(body, "playbook", "free-pack", 1)
	if code, resp := bootRequest(t, wrapped, "agent_packs/upsert", free, bootTestID()); code != http.StatusOK {
		t.Fatalf("free-name upsert through bootOwnershipMux status = %d body=%s, want 200", code, resp)
	}
}

// markerHandler is a comparable http.Handler used to assert handler
// identity (http.HandlerFunc values are funcs, hence uncomparable).
type markerHandler struct{ reached *bool }

func (m markerHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	*m.reached = true
	w.WriteHeader(http.StatusNoContent)
}

// TestBootOwnershipMux_PassthroughAndNilOwner pins the wrapper's
// structural contract: requests OUTSIDE the agent_config prefix reach
// the inner handler untouched, and a nil owner returns the inner
// handler unchanged (no wrap, no behavior change).
func TestBootOwnershipMux_PassthroughAndNilOwner(t *testing.T) {
	// (a) Non-agent_config paths are passed through — the wrapper still
	//     forwards, and the handler sees no reader-related disturbance
	//     (the binding is scoped to the agent_config prefix).
	owner := bootOwnerFor("t1", bootTestAgent, "playbook")
	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	wrapped := bootOwnershipMux(owner, inner)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/list", nil))
	if !reached || rec.Code != http.StatusNoContent {
		t.Fatalf("non-agent_config request through wrapper: reached=%v code=%d, want true/204", reached, rec.Code)
	}

	// (b) A nil owner is a no-op: the exact same handler value is
	//     returned, so an unwired boot baseline adds no middleware layer.
	marker := markerHandler{reached: new(bool)}
	if got := bootOwnershipMux(nil, marker); got != http.Handler(marker) {
		t.Fatal("bootOwnershipMux(nil, next) must return next unchanged")
	}
	if got := bootOwnershipMux(owner, nil); got != nil {
		t.Fatal("bootOwnershipMux(owner, nil) must return nil")
	}
}
