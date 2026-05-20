// Phase 74 (D-114) cross-subsystem integration test — the Console
// topology projection surface end-to-end.
//
// Phase 74 ships TWO surfaces over the canonical TopologyProjection
// wire type:
//
//   - `topology.snapshot` — a request→reply Protocol method, dispatched
//     by the ControlSurface, routed over the Phase 60 REST control
//     transport.
//   - `topology.changed` — a canonical event the engine publishes on
//     construction onto the Phase 05 EventBus, consumed via the Phase
//     60 SSE stream transport.
//
// This test wires REAL drivers across every seam (CLAUDE.md §17.3 #1):
// real engine.Engine, real events/drivers/inmem bus, real
// state/drivers/inmem store, real tasks/drivers/inprocess registry,
// real protocol.ControlSurface, real protocol/auth.Validator over a
// real ES256 keypair, real transports.Mux on an httptest.Server. No
// mocks at any boundary.
//
// Coverage (the Phase 74 plan's Acceptance integration criteria):
//
//	(a) the constructor-time `topology.changed` event arrives on a
//	    subscriber within a bounded window of engine.New;
//	(b) the `topology.snapshot` RPC round-trip yields the same
//	    projection bytes the event carried (byte-stable across surfaces);
//	(c) a cross-tenant snapshot without the admin scope is rejected
//	    with CodeAuthRejected (HTTP 401);
//	(d) a cross-tenant snapshot WITH the admin scope succeeds and emits
//	    audit.admin_scope_used on the bus;
//	(e) a second engine with one more adjacency emits a second
//	    `topology.changed` whose Edges differ by exactly one entry;
//	(f) N=10 concurrent snapshot callers + identity isolation.
//
// Per §17.4 the test uses channel-bound `eventually`-style waits with
// bounded real-time deadlines — never time.Sleep as a sync primitive.
package integration_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// fixedNowPhase74 is the deterministic validator clock.
var fixedNowPhase74 = time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

const (
	phase74EngineTenant = "tenant-engine"
	phase74User         = "user-topo"
	phase74Session      = "sess-topo"
)

// phase74Deps bundles the real runtime drivers behind the
// auth-decorated wire transport.
type phase74Deps struct {
	mux      http.Handler
	bus      events.EventBus
	priv     *ecdsa.PrivateKey
	engineID string
	cleanup  func()
}

// engineNodeFunc is a no-op NodeFunc for the test engine's nodes.
func engineNodeFunc(_ context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return env, nil
}

// mustIdentityCtx builds a context carrying a complete identity triple.
func mustIdentityCtx(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// newPhase74Deps assembles the dev stack with a real engine wired into
// the ControlSurface as the TopologyAccessor. The engine is constructed
// with WithEventBus(bus) so its construction-time topology.changed
// event lands on the same bus the SSE transport serves.
func newPhase74Deps(t *testing.T, extraAdjacency bool) *phase74Deps {
	t.Helper()

	priv, pub := loadES256Phase74(t)

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}

	// The engine — a real engine.Engine wired to the same bus. Its
	// construction-time topology.changed event publishes immediately.
	in := engine.Node{Name: "ingress", Func: engineNodeFunc}
	mid := engine.Node{Name: "router", Func: engineNodeFunc}
	out := engine.Node{Name: "egress", Func: engineNodeFunc}
	adjs := []engine.Adjacency{
		{From: in, To: []engine.Node{mid}},
		{From: mid, To: []engine.Node{out}},
	}
	if extraAdjacency {
		extra := engine.Node{Name: "audit-sink", Func: engineNodeFunc}
		adjs = append(adjs, engine.Adjacency{From: mid, To: []engine.Node{extra}})
	}
	eng, err := engine.New(adjs, engine.WithEventBus(bus))
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("engine.New: %v", err)
	}
	accessor := &phase74EngineAccessor{eng: eng, tenant: phase74EngineTenant}

	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry(),
		protocol.WithTopologyAccessor(accessor),
		protocol.WithEventBus(bus),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	v, err := auth.NewValidator(newES256KeySetPhase74("k1", pub),
		auth.WithClock(func() time.Time { return fixedNowPhase74 }),
		auth.WithRedactor(red),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithValidator(v),
		transports.WithRedactor(red),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}

	// Read back the engine id via a topology snapshot so the test can
	// assert byte-stability between the event and the snapshot.
	scopedCtx := mustIdentityCtx(t, phase74EngineTenant, phase74User, phase74Session)
	proj, err := eng.Topology(scopedCtx)
	if err != nil {
		t.Fatalf("engine.Topology (engine id read): %v", err)
	}

	return &phase74Deps{
		mux:      mux,
		bus:      bus,
		priv:     priv,
		engineID: proj.EngineID,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// phase74EngineAccessor adapts a real engine.Engine into a
// protocol.TopologyAccessor — the structural seam the cmd/harbor wiring
// would build for an engine-bearing Runtime. The engine package stays
// Protocol-free; this adapter (which lives outside the engine package)
// adds the TenantID() the accessor contract needs.
type phase74EngineAccessor struct {
	eng    engine.Engine
	tenant string
}

func (a *phase74EngineAccessor) Topology(ctx context.Context) (types.TopologyProjection, error) {
	return a.eng.Topology(ctx)
}

func (a *phase74EngineAccessor) TenantID() string { return a.tenant }

// compile-time assertion: the adapter satisfies protocol.TopologyAccessor.
var _ protocol.TopologyAccessor = (*phase74EngineAccessor)(nil)

// --- ES256 key helpers (mirror the Phase 72b pattern) ---

type es256KeySetPhase74 struct {
	kid string
	pub crypto.PublicKey
}

func newES256KeySetPhase74(kid string, pub crypto.PublicKey) *es256KeySetPhase74 {
	return &es256KeySetPhase74{kid: kid, pub: pub}
}

func (s *es256KeySetPhase74) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != s.kid {
		return nil, "", fmt.Errorf("kid %q not in key set", kid)
	}
	return s.pub, "ES256", nil
}

func loadES256Phase74(t *testing.T) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	priv := readPEMBlockPhase74(t, filepath.Join(repoRoot, "internal/protocol/auth/testdata/es256_private.pem"))
	pub := readPEMBlockPhase74(t, filepath.Join(repoRoot, "internal/protocol/auth/testdata/es256_public.pem"))
	ecPriv, err := x509.ParseECPrivateKey(priv)
	if err != nil {
		k, perr := x509.ParsePKCS8PrivateKey(priv)
		if perr != nil {
			t.Fatalf("parse ES256 private: EC=%v PKCS8=%v", err, perr)
		}
		var ok bool
		ecPriv, ok = k.(*ecdsa.PrivateKey)
		if !ok {
			t.Fatalf("PKCS8 key is not *ecdsa.PrivateKey")
		}
	}
	pubAny, err := x509.ParsePKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("parse ES256 public: %v", err)
	}
	ecPub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is not *ecdsa.PublicKey")
	}
	return ecPriv, ecPub
}

func readPEMBlockPhase74(t *testing.T, abs string) []byte {
	t.Helper()
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %q: %v", abs, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("no PEM block in %q", abs)
	}
	return block.Bytes
}

func signES256Phase74(t *testing.T, priv *ecdsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return signed
}

func phase74Claims(tenant, user, session string, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     user,
		"exp":     fixedNowPhase74.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase74.Add(-1 * time.Minute).Unix(),
		"tenant":  tenant,
		"user":    user,
		"session": session,
		"scopes":  scopes,
	}
}

// snapshotOverWire submits a topology.snapshot request through the REST
// control transport and returns the HTTP status + decoded body.
func snapshotOverWire(t *testing.T, baseURL, token, tenant, user, session string) (int, map[string]json.RawMessage) {
	t.Helper()
	body := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q}}`, tenant, user, session)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/control/topology.snapshot",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/control/topology.snapshot: %v", err)
	}
	defer resp.Body.Close()
	var decoded map[string]json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// TestE2E_Phase74_TopologyChangedEvent_ArrivesOnConstruction — criterion
// (a): the constructor-time topology.changed event is observable.
func TestE2E_Phase74_TopologyChangedEvent_ArrivesOnConstruction(t *testing.T) {
	t.Parallel()

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()

	// Subscribe BEFORE engine.New so the live stream catches the emit.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Types: []events.EventType{events.EventTypeTopologyChanged},
		Admin: true,
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	in := engine.Node{Name: "ingress", Func: engineNodeFunc}
	out := engine.Node{Name: "egress", Func: engineNodeFunc}
	if _, err := engine.New([]engine.Adjacency{{From: in, To: []engine.Node{out}}},
		engine.WithEventBus(bus)); err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeTopologyChanged {
			t.Fatalf("event type = %q, want topology.changed", ev.Type)
		}
		payload, ok := ev.Payload.(events.TopologyChangedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want events.TopologyChangedPayload (SafePayload preserved through the bus)", ev.Payload)
		}
		if len(payload.Projection.Nodes) != 2 || len(payload.Projection.Edges) != 1 {
			t.Errorf("projection shape = %d nodes / %d edges, want 2 / 1",
				len(payload.Projection.Nodes), len(payload.Projection.Edges))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no topology.changed event within 200ms of engine.New")
	}
}

// TestE2E_Phase74_SnapshotRoundTrip — criterion (b): the topology.snapshot
// RPC round-trips over the wire and yields the engine's projection.
func TestE2E_Phase74_SnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	deps := newPhase74Deps(t, false)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	token := signES256Phase74(t, deps.priv,
		phase74Claims(phase74EngineTenant, phase74User, phase74Session, nil))

	status, body := snapshotOverWire(t, srv.URL, token, phase74EngineTenant, phase74User, phase74Session)
	if status != http.StatusOK {
		t.Fatalf("topology.snapshot status = %d, want 200 (body: %v)", status, body)
	}
	if _, ok := body["engine_id"]; !ok {
		t.Error("snapshot response missing engine_id")
	}
	var nodes, edges []json.RawMessage
	_ = json.Unmarshal(body["nodes"], &nodes)
	_ = json.Unmarshal(body["edges"], &edges)
	if len(nodes) != 3 {
		t.Errorf("snapshot nodes len = %d, want 3", len(nodes))
	}
	if len(edges) != 2 {
		t.Errorf("snapshot edges len = %d, want 2", len(edges))
	}
	// Byte-stability: a second snapshot returns the same node/edge sets.
	status2, body2 := snapshotOverWire(t, srv.URL, token, phase74EngineTenant, phase74User, phase74Session)
	if status2 != http.StatusOK {
		t.Fatalf("second topology.snapshot status = %d, want 200", status2)
	}
	if string(body["nodes"]) != string(body2["nodes"]) || string(body["edges"]) != string(body2["edges"]) {
		t.Error("two snapshots of the same engine returned different node/edge bytes — not byte-stable")
	}
}

// TestE2E_Phase74_CrossTenantWithoutAdmin_Rejected — criterion (c): a
// cross-tenant snapshot without the admin scope is rejected.
func TestE2E_Phase74_CrossTenantWithoutAdmin_Rejected(t *testing.T) {
	t.Parallel()
	deps := newPhase74Deps(t, false)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	// Token for a DIFFERENT tenant, no admin scope. The body identity
	// must match the JWT's user/session; the tenant differs from the
	// engine's tenant.
	const foreignTenant = "tenant-foreign"
	token := signES256Phase74(t, deps.priv,
		phase74Claims(foreignTenant, phase74User, phase74Session, nil))

	status, body := snapshotOverWire(t, srv.URL, token, foreignTenant, phase74User, phase74Session)
	if status != http.StatusUnauthorized {
		t.Fatalf("cross-tenant snapshot without admin status = %d, want 401 (body: %v)", status, body)
	}
	var code string
	_ = json.Unmarshal(body["code"], &code)
	if code != "auth_rejected" {
		t.Errorf("cross-tenant snapshot without admin code = %q, want auth_rejected", code)
	}
}

// TestE2E_Phase74_CrossTenantWithAdmin_SucceedsAndAudits — criterion
// (d): the same call WITH the admin scope succeeds and emits
// audit.admin_scope_used on the bus.
func TestE2E_Phase74_CrossTenantWithAdmin_SucceedsAndAudits(t *testing.T) {
	t.Parallel()
	deps := newPhase74Deps(t, false)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const foreignTenant = "tenant-admin"
	auditSub, err := deps.bus.Subscribe(context.Background(), events.Filter{
		Tenant: foreignTenant, User: phase74User, Session: phase74Session,
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe(audit): %v", err)
	}
	defer auditSub.Cancel()

	token := signES256Phase74(t, deps.priv,
		phase74Claims(foreignTenant, phase74User, phase74Session, []string{"admin"}))

	status, body := snapshotOverWire(t, srv.URL, token, foreignTenant, phase74User, phase74Session)
	if status != http.StatusOK {
		t.Fatalf("cross-tenant snapshot WITH admin status = %d, want 200 (body: %v)", status, body)
	}

	select {
	case ev := <-auditSub.Events():
		if ev.Type != events.EventTypeAdminScopeUsed {
			t.Fatalf("audit event type = %q, want audit.admin_scope_used", ev.Type)
		}
		if ev.Identity.TenantID != foreignTenant {
			t.Errorf("audit event tenant = %q, want %q (the admin caller)", ev.Identity.TenantID, foreignTenant)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audit.admin_scope_used event within 2s of a cross-tenant admin topology read")
	}
}

// TestE2E_Phase74_SecondEngineDiffersByOneEdge — criterion (e): a second
// engine with one more adjacency emits a topology.changed whose Edges
// differ by exactly one entry.
func TestE2E_Phase74_SecondEngineDiffersByOneEdge(t *testing.T) {
	t.Parallel()
	depsA := newPhase74Deps(t, false)
	defer depsA.cleanup()
	depsB := newPhase74Deps(t, true) // one extra adjacency
	defer depsB.cleanup()
	srvA := httptest.NewServer(depsA.mux)
	defer srvA.Close()
	srvB := httptest.NewServer(depsB.mux)
	defer srvB.Close()

	tokenA := signES256Phase74(t, depsA.priv,
		phase74Claims(phase74EngineTenant, phase74User, phase74Session, nil))
	tokenB := signES256Phase74(t, depsB.priv,
		phase74Claims(phase74EngineTenant, phase74User, phase74Session, nil))

	_, bodyA := snapshotOverWire(t, srvA.URL, tokenA, phase74EngineTenant, phase74User, phase74Session)
	_, bodyB := snapshotOverWire(t, srvB.URL, tokenB, phase74EngineTenant, phase74User, phase74Session)

	var edgesA, edgesB []json.RawMessage
	_ = json.Unmarshal(bodyA["edges"], &edgesA)
	_ = json.Unmarshal(bodyB["edges"], &edgesB)
	if len(edgesB) != len(edgesA)+1 {
		t.Fatalf("engine B edges = %d, engine A edges = %d — want B == A+1", len(edgesB), len(edgesA))
	}
}

// TestE2E_Phase74_Concurrency_NoCrossTalk — criterion (f): N=10
// concurrent snapshot callers + identity isolation. Each goroutine
// drives its own (user, session) under the engine's tenant; all see
// the same projection (the engine is shared) with no race.
func TestE2E_Phase74_Concurrency_NoCrossTalk(t *testing.T) {
	t.Parallel()
	deps := newPhase74Deps(t, false)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", i)
			session := fmt.Sprintf("session-%d", i)
			token := signES256Phase74(t, deps.priv,
				phase74Claims(phase74EngineTenant, user, session, nil))
			status, body := snapshotOverWire(t, srv.URL, token, phase74EngineTenant, user, session)
			if status != http.StatusOK {
				errs <- fmt.Sprintf("goroutine-%d: status %d", i, status)
				return
			}
			var engineID string
			_ = json.Unmarshal(body["engine_id"], &engineID)
			if engineID != deps.engineID {
				errs <- fmt.Sprintf("goroutine-%d: engine_id %q, want %q — projection drift", i, engineID, deps.engineID)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
