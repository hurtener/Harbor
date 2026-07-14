// Phase 148 — MCP southbound per-identity OAuth bearer + `_meta` provenance
// (RFC §6.4 + §3.3 + §6.16; D-278).
//
// This test wires the REAL artifacts across the phase seam — no mocks at any
// boundary (CLAUDE.md §17.3):
//
//   - Phase 142's RFC-8693 httptest broker fixture (`newP142Broker`,
//     spec-derived per §17.8: it asserts grant_type / subject_token_type /
//     audience / client-auth broker-side), driving the real `tokenexchange`
//     OAuth provider;
//   - an httptest-hosted MCP fixture server built on the OFFICIAL go-sdk
//     streamable-HTTP handler (real SDK wire shapes, the D-224 pattern),
//     fronted by a recorder that captures, per request, the Authorization
//     bearer and the `_meta` map;
//   - the real MCP driver Provider bound to the provider via
//     Config.OAuthProvider.
//
// It exercises: (a) per-call Authorization arrives and ROTATES across
// identities; (b) `_meta` carries triple + agent_id + annotations; (c) the
// cold-path token-miss → exchange → inject round-trip; (d) broker refusal →
// the tool call fails loud with NO unauthenticated wire call; (e) a
// consent_required refusal surfaces the typed *auth.ErrAuthRequired. `-race`
// throughout.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateInmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// p148Capture is one recorded server-side request observation.
type p148Capture struct {
	authz string
	meta  map[string]string
}

// p148Recorder fronts the go-sdk streamable-HTTP MCP handler and records the
// Authorization header + `_meta` map of every tools/call POST.
type p148Recorder struct {
	inner http.Handler
	mu    sync.Mutex
	calls []p148Capture
}

func (r *p148Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err == nil {
			r.record(req.Header.Get("Authorization"), body)
		}
		req.Body = io.NopCloser(p148Bytes(body))
		req.ContentLength = int64(len(body))
	}
	r.inner.ServeHTTP(w, req)
}

func (r *p148Recorder) record(authz string, body []byte) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Meta map[string]string `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method != "tools/call" {
		return
	}
	r.mu.Lock()
	r.calls = append(r.calls, p148Capture{authz: authz, meta: msg.Params.Meta})
	r.mu.Unlock()
}

func (r *p148Recorder) snapshot() []p148Capture {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]p148Capture, len(r.calls))
	copy(out, r.calls)
	return out
}

type p148BytesReader struct {
	b   []byte
	pos int
}

func p148Bytes(b []byte) io.Reader { return &p148BytesReader{b: b} }

func (r *p148BytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// p148MCPServer builds the streamable-HTTP MCP fixture with one echo tool.
func p148MCPServer(t *testing.T) (*httptest.Server, *p148Recorder) {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-p148-fixture", Version: "v0"}, nil)
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
	rec := &p148Recorder{inner: mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)}
	hs := httptest.NewServer(rec)
	t.Cleanup(hs.Close)
	return hs, rec
}

// p148Env bundles the broker + provider + bound MCP echo descriptor.
type p148Env struct {
	broker *p142Broker
	rec    *p148Recorder
	echo   tools.ToolDescriptor
}

// newP148ProviderAndBus builds the real tokenexchange provider against the
// RFC-8693 broker fixture plus the shared inmem bus — the seam both the
// direct-driver env and the devstack-attacher twin test construct from.
func newP148ProviderAndBus(t *testing.T, broker *p142Broker, downstreamHosts ...string) (auth.OAuthProvider, events.EventBus) {
	t.Helper()
	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// Real StateStore-backed TokenStore — the tokenexchange driver holds it
	// but never Puts (brokered tokens are TTL-cached in memory, D-271).
	rawState, err := stateInmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = rawState.Close(context.Background()) })
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*7 + 3)
	}
	sealer, err := auth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	store, err := auth.NewTokenStore(rawState, sealer)
	if err != nil {
		t.Fatalf("token store: %v", err)
	}
	coord := pauseresume.New()

	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:                   p142Provider,
		CredentialSource:       credsource.Static(p142BrokerClient, p142BrokerSecret),
		Scopes:                 []string{"Mail.Read"},
		TokenURL:               broker.tokenURL(),
		Extra:                  map[string]string{"audience": p142Audience},
		AllowedDownstreamHosts: downstreamHosts,
	}, auth.FactoryDeps{
		Store:       store,
		Bus:         bus,
		Redactor:    red,
		Coordinator: coord,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	return prov, bus
}

func newP148Env(t *testing.T) *p148Env {
	t.Helper()
	broker := newP142Broker(t)
	hs, rec := p148MCPServer(t)
	prov, bus := newP148ProviderAndBus(t, broker, hs.URL)

	p, err := mcpdrv.New(mcpdrv.Config{
		Name:            "graph",
		TransportMode:   mcpdrv.TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OAuthProvider:   prov,
		MetaAnnotations: map[string]string{"deployment": "prod"},
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var echo tools.ToolDescriptor
	for _, d := range descs {
		if d.Tool.Name == "graph_echo" {
			echo = d
		}
	}
	if echo.Invoke == nil {
		t.Fatalf("echo descriptor not discovered")
	}
	return &p148Env{broker: broker, rec: rec, echo: echo}
}

func p148Ctx(t *testing.T, id identity.Identity, agentID string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return tools.WithInvokingAgent(ctx, agentID)
}

// TestE2E_Phase148_ColdPathInjectAndProvenance drives the cold-path token
// exchange, asserts the bearer + `_meta` (triple + agent_id + annotation),
// and that the exchange fired exactly once (broker call count).
func TestE2E_Phase148_ColdPathInjectAndProvenance(t *testing.T) {
	t.Parallel()
	env := newP148Env(t)
	id := identity.Identity{TenantID: "tenant-1", UserID: "alice", SessionID: "s1"}
	ctx := p148Ctx(t, id, "agent-77")

	if _, err := env.echo.Invoke(ctx, json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if env.broker.callCount() != 1 {
		t.Fatalf("cold-path exchange: broker calls = %d, want 1", env.broker.callCount())
	}
	calls := env.rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("server saw %d tools/call, want 1", len(calls))
	}
	c := calls[0]
	if c.authz != "Bearer brokered-tenant-1-alice" {
		t.Fatalf("Authorization = %q, want Bearer brokered-tenant-1-alice", c.authz)
	}
	if c.meta["tenant"] != "tenant-1" || c.meta["user"] != "alice" || c.meta["session"] != "s1" {
		t.Fatalf("_meta triple wrong: %+v", c.meta)
	}
	if c.meta["agent_id"] != "agent-77" {
		t.Fatalf("_meta agent_id = %q, want agent-77", c.meta["agent_id"])
	}
	if c.meta["deployment"] != "prod" {
		t.Fatalf("_meta annotation missing: %+v", c.meta)
	}

	// Cache hit — a second call for the same identity does NOT re-exchange.
	if _, err := env.echo.Invoke(ctx, json.RawMessage(`{"text":"again"}`)); err != nil {
		t.Fatalf("Invoke (cache): %v", err)
	}
	if env.broker.callCount() != 1 {
		t.Fatalf("cache-hit expected; broker calls = %d", env.broker.callCount())
	}
}

// TestE2E_Phase148_BearerRotatesAcrossIdentities proves the per-identity
// bearer rotates when the calling triple changes (no bleed).
func TestE2E_Phase148_BearerRotatesAcrossIdentities(t *testing.T) {
	t.Parallel()
	env := newP148Env(t)

	for _, id := range []identity.Identity{
		{TenantID: "t-a", UserID: "amy", SessionID: "s"},
		{TenantID: "t-b", UserID: "bob", SessionID: "s"},
	} {
		if _, err := env.echo.Invoke(p148Ctx(t, id, "agent-x"), json.RawMessage(`{"text":"hi"}`)); err != nil {
			t.Fatalf("Invoke(%s): %v", id.UserID, err)
		}
	}
	calls := env.rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("server saw %d calls, want 2", len(calls))
	}
	want := map[string]string{"amy": "Bearer brokered-t-a-amy", "bob": "Bearer brokered-t-b-bob"}
	for _, c := range calls {
		exp, ok := want[c.meta["user"]]
		if !ok {
			t.Fatalf("unexpected user in _meta: %+v", c.meta)
		}
		if c.authz != exp {
			t.Fatalf("user %s: Authorization %q, want %q (bearer bleed)", c.meta["user"], c.authz, exp)
		}
	}
}

// TestE2E_Phase148_BrokerRefusal_FailsLoud_NoWireCall proves a broker outage
// aborts the tool call BEFORE any wire request — the server records ZERO
// tools/call for the refused identity.
func TestE2E_Phase148_BrokerRefusal_FailsLoud_NoWireCall(t *testing.T) {
	t.Parallel()
	env := newP148Env(t)
	env.broker.setPosture("error500")
	id := identity.Identity{TenantID: "tenant-1", UserID: "carol", SessionID: "s1"}

	_, err := env.echo.Invoke(p148Ctx(t, id, "agent-x"), json.RawMessage(`{"text":"hi"}`))
	if err == nil {
		t.Fatal("want error on broker outage, got nil (an unauthenticated call would be a leak)")
	}
	if calls := env.rec.snapshot(); len(calls) != 0 {
		t.Fatalf("broker refused but the server saw %d tools/call — a fallback UNAUTHENTICATED call leaked", len(calls))
	}
}

// TestE2E_Phase148_DevstackAttacher_BindingOverAddConnection proves the
// devstack twin of cmd/harbor's runtime-add attacher threads the
// `oauth_provider` binding + `meta_annotations` from the AttachRequest all
// the way to the wire (§17.6 twin discipline): a runtime-added connection
// injects the per-identity bearer exactly like a boot-time one. Also covers
// the twin's fail-loud leg: an unknown provider name fails the attach,
// naming the registered providers — never a silent unauthenticated attach.
func TestE2E_Phase148_DevstackAttacher_BindingOverAddConnection(t *testing.T) {
	t.Parallel()
	broker := newP142Broker(t)
	hs, rec := p148MCPServer(t)
	prov, bus := newP148ProviderAndBus(t, broker, hs.URL)

	cat := tools.NewCatalog()
	mcpReg := mcpdrv.NewRegistry()
	sysID := identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}
	attacher := serve.NewMCPConnectionAttacher(cat, mcpReg, bus, nil, sysID,
		map[string]auth.OAuthProvider{p142Provider: prov}, nil)
	t.Cleanup(func() { _ = attacher.Close(context.Background()) })

	// Fail-loud leg first: an unknown provider name must abort the attach,
	// naming the registered providers.
	err := attacher.Attach(context.Background(), agentcfgprotocol.AttachRequest{
		Identity:      sysID,
		AgentID:       "agent-p148", // the (tenant, agent) reconcile-view owner the attacher now requires
		Name:          "badbind",
		Transport:     agentcfg.MCPTransportHTTP,
		URL:           hs.URL,
		OAuthProvider: "not-declared",
	})
	if err == nil {
		t.Fatal("attach with unknown oauth_provider succeeded — a silent unauthenticated attach")
	}
	if !strings.Contains(err.Error(), p142Provider) {
		t.Fatalf("attach error must list registered providers, got %q", err.Error())
	}

	// Happy path: the runtime add carries the binding + annotations.
	err = attacher.Attach(context.Background(), agentcfgprotocol.AttachRequest{
		Identity:        sysID,
		AgentID:         "agent-p148", // the (tenant, agent) reconcile-view owner the attacher now requires
		Name:            "added",
		Transport:       agentcfg.MCPTransportHTTP,
		URL:             hs.URL,
		OAuthProvider:   p142Provider,
		MetaAnnotations: map[string]string{"deployment": "prod"},
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	echo, ok := cat.Resolve("added_echo")
	if !ok {
		t.Fatal("added_echo not registered after runtime add")
	}

	id := identity.Identity{TenantID: "tenant-add", UserID: "erin", SessionID: "s1"}
	if _, err := echo.Invoke(p148Ctx(t, id, "agent-add"), json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("server saw %d tools/call, want 1", len(calls))
	}
	c := calls[0]
	if c.authz != "Bearer brokered-tenant-add-erin" {
		t.Fatalf("runtime-added connection did not inject the per-identity bearer: Authorization = %q", c.authz)
	}
	if c.meta["deployment"] != "prod" || c.meta["agent_id"] != "agent-add" {
		t.Fatalf("runtime-added connection dropped annotations/provenance: %+v", c.meta)
	}
}

// TestE2E_Phase148_ConsentRequired_TypedErrAuthRequired proves a
// consent_required refusal surfaces the typed *auth.ErrAuthRequired
// (errors.As-reachable) so the runtime parks the run on the unified
// pause/resume primitive — with no wire call.
func TestE2E_Phase148_ConsentRequired_TypedErrAuthRequired(t *testing.T) {
	t.Parallel()
	env := newP148Env(t)
	env.broker.setPosture("consent")
	id := identity.Identity{TenantID: "tenant-1", UserID: "dave", SessionID: "s1"}

	_, err := env.echo.Invoke(p148Ctx(t, id, "agent-x"), json.RawMessage(`{"text":"hi"}`))
	if err == nil {
		t.Fatal("want error on consent_required, got nil")
	}
	var authErr *auth.ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("want errors.As *auth.ErrAuthRequired, got %v", err)
	}
	if calls := env.rec.snapshot(); len(calls) != 0 {
		t.Fatalf("consent_required but server saw %d tools/call", len(calls))
	}
}
