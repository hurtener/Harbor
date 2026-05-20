// Package integration — Phase 73k (D-119) MCP-Connections-page
// cross-subsystem integration test.
//
// This test wires the Phase 73k surface end-to-end with REAL drivers on
// every seam (§17.3): a real mcp.Registry read API, a real
// tools/auth.Provider OAuth provider, the real protocol.MCPSurface
// dispatcher, the real REST control transport over an httptest.Server,
// a real events/drivers/inmem bus, and a real audit/drivers/patterns
// redactor. It asserts:
//
//   - End-to-end identity propagation through every `mcp.servers.*`
//     method (request → handler → driver → response).
//   - Admin-verb claim gating — `refresh_discovery` / `probe` /
//     `refresh_binding` / `revoke_binding` / `set_raw_html_trust` all
//     fail with CodeScopeMismatch without the admin scope.
//   - The raw-HTML trust toggle emits exactly one
//     `mcp.raw_html_trust_toggled` audit event with a SafePayload body.
//   - N≥16 concurrent subscribers on the `mcp.raw_html_trust_toggled`
//     filter each receive the event exactly once; no cross-talk; no
//     goroutine leak after teardown.
//   - At least one failure mode — a missing-identity request returns
//     CodeIdentityRequired.
//
// Runs under `-race`.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

const (
	mcpPageTenant  = "tenant-mcp-page"
	mcpPageUser    = "user-mcp-page"
	mcpPageSession = "session-mcp-page"
	mcpPageServer  = "github-server"
)

// mcpPageStubProvider is a deterministic MCP provider for the
// integration test — the real Phase 28 *mcp.Provider needs a live MCP
// wire; this stub satisfies the same Discover contract so the real
// mcp.Registry read API is exercised end-to-end.
type mcpPageStubProvider struct {
	id tools.ToolSourceID
}

func (p *mcpPageStubProvider) SourceID() tools.ToolSourceID { return p.id }

func (p *mcpPageStubProvider) Discover(ctx context.Context) ([]tools.ToolDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []tools.ToolDescriptor{
		{Tool: tools.Tool{Name: string(p.id) + ".issues"}},
		{Tool: tools.Tool{Name: string(p.id) + "__resource.repo://harbor"}},
		{Tool: tools.Tool{Name: string(p.id) + "__prompt.review"}},
	}, nil
}

// mcpPageEnv bundles the wired surface for the integration test.
type mcpPageEnv struct {
	srv     *httptest.Server
	bus     events.EventBus
	cleanup func()
}

// buildMCPPageEnv wires the full Phase 73k surface with real drivers.
func buildMCPPageEnv(t *testing.T) *mcpPageEnv {
	t.Helper()
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
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

	// Real MCP registry with one stub-backed server.
	reg := mcp.NewRegistry()
	if err := reg.Register(mcp.ServerRegistration{
		Provider:          &mcpPageStubProvider{id: tools.ToolSourceID(mcpPageServer)},
		Transport:         "http+sse",
		URLOrCommand:      "https://mcp.example.com/github",
		OAuthBindingCount: 1,
		InitialState:      mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	regAccessor, err := mcpconsole.NewRegistryAccessor(reg)
	if err != nil {
		t.Fatalf("NewRegistryAccessor: %v", err)
	}

	// Real OAuth provider — a real auth.Provider backed by a real
	// in-memory TokenStore. The OAuth & Auth tab's bindings.list flows
	// through it.
	sealer, err := auth.NewAESGCMSealer(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	tokenStore, err := auth.NewTokenStore(store, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	oauthProv, err := auth.NewProvider([]auth.OAuthConfig{{
		Source:       tools.ToolSourceID(mcpPageServer),
		SourceName:   "GitHub",
		BindingScope: auth.ScopeUser,
		ServerURL:    "https://auth.example.com",
		RedirectURI:  "http://localhost/callback",
		Scopes:       []string{"repo"},
	}}, auth.ProviderDeps{
		Store: tokenStore, Bus: bus, Redactor: red, Coordinator: pauseresume.New(),
	})
	if err != nil {
		t.Fatalf("auth.NewProvider: %v", err)
	}
	oauthAccessor, err := mcpconsole.NewOAuthAccessor(oauthProv)
	if err != nil {
		t.Fatalf("NewOAuthAccessor: %v", err)
	}

	// Real MCPSurface dispatcher.
	mcpSurface, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP:      regAccessor,
		OAuth:    oauthAccessor,
		Redactor: red,
		Bus:      bus,
	})
	if err != nil {
		t.Fatalf("protocol.NewMCPSurface: %v", err)
	}

	// Real control surface + mux over an httptest.Server.
	cs, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	mux, err := transports.NewMux(cs, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithoutValidator(),
		transports.WithMCPSurface(mcpSurface),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)

	return &mcpPageEnv{
		srv: srv,
		bus: bus,
		cleanup: func() {
			srv.Close()
			_ = oauthProv.Close(context.Background())
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// mcpCall POSTs a Protocol method to the control transport and returns
// the HTTP status + decoded JSON body.
func mcpCall(t *testing.T, env *mcpPageEnv, method methods.Method, body any) (int, map[string]any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s: %v", method, err)
	}
	url := fmt.Sprintf("%s/v1/control/%s", env.srv.URL, string(method))
	resp, err := http.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded
}

// scope is a helper for the request identity object.
func mcpScope() map[string]any {
	return map[string]any{
		"tenant": mcpPageTenant, "user": mcpPageUser, "session": mcpPageSession,
	}
}

func TestE2E_Phase73k_IdentityPropagation(t *testing.T) {
	env := buildMCPPageEnv(t)
	defer env.cleanup()

	// list → 200, .servers is an array with the registered server.
	status, body := mcpCall(t, env, methods.MethodMCPServersList, map[string]any{"identity": mcpScope()})
	if status != http.StatusOK {
		t.Fatalf("list: status %d, body %v", status, body)
	}
	servers, ok := body["servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("list: want 1 server, got %v", body["servers"])
	}

	// get → 200, .server.name matches.
	status, body = mcpCall(t, env, methods.MethodMCPServersGet, map[string]any{
		"identity": mcpScope(), "name": mcpPageServer,
	})
	if status != http.StatusOK {
		t.Fatalf("get: status %d, body %v", status, body)
	}
	srv, _ := body["server"].(map[string]any)
	if srv == nil || srv["name"] != mcpPageServer {
		t.Fatalf("get: server name mismatch: %v", body)
	}

	// resources / prompts / health / policy / bindings.list all 200.
	for _, m := range []methods.Method{
		methods.MethodMCPServersResources,
		methods.MethodMCPServersPrompts,
		methods.MethodMCPServersHealth,
		methods.MethodMCPServersPolicy,
		methods.MethodMCPServersBindingsList,
	} {
		status, body = mcpCall(t, env, m, map[string]any{"identity": mcpScope(), "name": mcpPageServer})
		if status != http.StatusOK {
			t.Fatalf("%s: status %d, body %v", m, status, body)
		}
	}
}

func TestE2E_Phase73k_MissingIdentity_FailsClosed(t *testing.T) {
	env := buildMCPPageEnv(t)
	defer env.cleanup()

	status, body := mcpCall(t, env, methods.MethodMCPServersList, map[string]any{})
	if status == http.StatusOK {
		t.Fatalf("missing-identity list should not be 200")
	}
	if body["code"] != "identity_required" {
		t.Fatalf("want identity_required, got %v", body["code"])
	}
}

func TestE2E_Phase73k_AdminClaimMismatch(t *testing.T) {
	env := buildMCPPageEnv(t)
	defer env.cleanup()

	// The control/admin verbs require the admin scope. The
	// httptest mux runs WithoutValidator, so no scope is ever attached
	// — every admin verb must fail with scope_mismatch.
	for _, m := range []methods.Method{
		methods.MethodMCPServersRefreshDiscovery,
		methods.MethodMCPServersProbe,
		methods.MethodMCPServersRefreshBinding,
		methods.MethodMCPServersRevokeBinding,
		methods.MethodMCPServersSetRawHTMLTrust,
	} {
		status, body := mcpCall(t, env, m, map[string]any{
			"identity": mcpScope(), "name": mcpPageServer,
		})
		if status == http.StatusOK {
			t.Errorf("%s: admin verb without admin scope should not be 200", m)
		}
		if body["code"] != "scope_mismatch" {
			t.Errorf("%s: want scope_mismatch, got %v", m, body["code"])
		}
	}
}

func TestE2E_Phase73k_NotFound(t *testing.T) {
	env := buildMCPPageEnv(t)
	defer env.cleanup()

	status, body := mcpCall(t, env, methods.MethodMCPServersGet, map[string]any{
		"identity": mcpScope(), "name": "nonexistent-server",
	})
	if status == http.StatusOK {
		t.Fatalf("get for unknown server should not be 200")
	}
	if body["code"] != "not_found" {
		t.Fatalf("want not_found, got %v", body["code"])
	}
}

// TestE2E_Phase73k_RawHTMLTrustAudit asserts the set_raw_html_trust
// path emits exactly one mcp.raw_html_trust_toggled audit event with a
// SafePayload body carrying the actor identity. Because the httptest mux
// runs WithoutValidator (no admin scope), this test exercises the audit
// path by dispatching the MCPSurface in-process with an admin-scoped ctx.
func TestE2E_Phase73k_RawHTMLTrustAudit(t *testing.T) {
	env := buildMCPPageEnv(t)
	defer env.cleanup()

	sub, err := env.bus.Subscribe(context.Background(), events.Filter{
		Tenant:  mcpPageTenant,
		User:    mcpPageUser,
		Session: mcpPageSession,
		Types:   []events.EventType{events.EventTypeMCPRawHTMLTrustToggled},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	// Toggle via the in-process MCPSurface with an admin-scoped ctx.
	mcpToggleRawHTMLTrust(t, env)

	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeMCPRawHTMLTrustToggled {
			t.Fatalf("want raw_html_trust_toggled, got %s", ev.Type)
		}
		payload, ok := ev.Payload.(events.MCPRawHTMLTrustToggledPayload)
		if !ok {
			t.Fatalf("payload type = %T, want MCPRawHTMLTrustToggledPayload", ev.Payload)
		}
		if payload.ServerName != mcpPageServer || !payload.Trusted {
			t.Fatalf("payload shape wrong: %+v", payload)
		}
		if payload.Actor.TenantID != mcpPageTenant {
			t.Fatalf("payload actor tenant = %q, want %q", payload.Actor.TenantID, mcpPageTenant)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for raw_html_trust_toggled event")
	}

	// Exactly one — no second event.
	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected second event: %s", ev.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestE2E_Phase73k_SSESubscriberStress runs N=16 concurrent subscribers
// on the mcp.raw_html_trust_toggled filter; each must receive the event
// exactly once. No cross-talk, no goroutine leak after teardown.
func TestE2E_Phase73k_SSESubscriberStress(t *testing.T) {
	env := buildMCPPageEnv(t)
	defer env.cleanup()

	const n = 16
	baseline := runtime.NumGoroutine()

	subs := make([]events.Subscription, n)
	for i := 0; i < n; i++ {
		sub, err := env.bus.Subscribe(context.Background(), events.Filter{
			Tenant:  mcpPageTenant,
			User:    mcpPageUser,
			Session: mcpPageSession,
			Types:   []events.EventType{events.EventTypeMCPRawHTMLTrustToggled},
		})
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		subs[i] = sub
	}

	// Each subscriber waits for exactly one event.
	var wg sync.WaitGroup
	got := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			deadline := time.After(3 * time.Second)
			for {
				select {
				case ev, ok := <-subs[i].Events():
					if !ok {
						return
					}
					if ev.Type == events.EventTypeMCPRawHTMLTrustToggled {
						got[i]++
					}
				case <-deadline:
					return
				}
			}
		}(i)
	}

	// One toggle → one event fanned to all N subscribers.
	mcpToggleRawHTMLTrust(t, env)

	// Give subscribers a moment to drain, then cancel.
	time.Sleep(500 * time.Millisecond)
	for _, s := range subs {
		s.Cancel()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if got[i] != 1 {
			t.Errorf("subscriber %d received %d events, want exactly 1", i, got[i])
		}
	}

	// Goroutine-leak check.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > baseline+4 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, g)
	}
}

// mcpToggleRawHTMLTrust dispatches set_raw_html_trust through a freshly
// built MCPSurface with an admin-scoped ctx — the httptest mux runs
// WithoutValidator so no scope is attachable over the wire, but the
// surface's admin gate + audit emit are exercised in-process here.
func mcpToggleRawHTMLTrust(t *testing.T, env *mcpPageEnv) {
	t.Helper()
	// The MCPSurface that env's mux holds is not directly reachable;
	// build an equivalent one over the SAME bus + a fresh registry so
	// the audit event lands on env.bus the subscribers watch.
	reg := mcp.NewRegistry()
	if err := reg.Register(mcp.ServerRegistration{
		Provider:     &mcpPageStubProvider{id: tools.ToolSourceID(mcpPageServer)},
		Transport:    "http+sse",
		InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	regAccessor, err := mcpconsole.NewRegistryAccessor(reg)
	if err != nil {
		t.Fatalf("NewRegistryAccessor: %v", err)
	}
	surface, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP:      regAccessor,
		OAuth:    &mcpPageNoopOAuth{},
		Redactor: auditpatterns.New(),
		Bus:      env.bus,
	})
	if err != nil {
		t.Fatalf("NewMCPSurface: %v", err)
	}
	adminCtx := protoauth.WithScopes(context.Background(), []protoauth.Scope{protoauth.ScopeAdmin})
	_, derr := surface.Dispatch(adminCtx, methods.MethodMCPServersSetRawHTMLTrust,
		&types.MCPServerSetRawHTMLTrustRequest{
			Identity: types.IdentityScope{
				Tenant: mcpPageTenant, User: mcpPageUser, Session: mcpPageSession,
			},
			Name:    mcpPageServer,
			Trusted: true,
		})
	if derr != nil {
		t.Fatalf("set_raw_html_trust dispatch: %v", derr)
	}
}

// mcpPageNoopOAuth is a no-OAuth accessor for the toggle helper — the
// raw-HTML toggle path never touches OAuth.
type mcpPageNoopOAuth struct{}

func (mcpPageNoopOAuth) ListBindings(context.Context, string) ([]protocol.MCPBindingRow, error) {
	return []protocol.MCPBindingRow{}, nil
}
func (mcpPageNoopOAuth) InitiateBinding(context.Context, string, string) (string, string, error) {
	return "", "", nil
}
func (mcpPageNoopOAuth) RevokeBinding(context.Context, string, string) (bool, error) {
	return false, nil
}
