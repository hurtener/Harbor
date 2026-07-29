// `meta_annotations` honour `_meta` path nesting — the cross-subsystem seam
// (RFC §6.4 + §3.4 + §4.2).
//
// Two mechanisms write into the SAME outbound MCP `_meta` map on the same call:
// the operator's `meta_annotations` and the per-user credential-injection
// `meta_key`. This test proves they now agree about what a dotted key means, by
// observing what a REAL MCP server actually receives on the wire.
//
// Real drivers at every boundary (CLAUDE.md §17.3): the RFC-8693 broker fixture
// driving the real `tokenexchange` provider, the real in-mem event bus with the
// real `audit/drivers/patterns` redactor, the real in-mem StateStore behind the
// real TokenStore, and an httptest MCP fixture built on the OFFICIAL go-sdk
// streamable-HTTP handler (§17.8 — the `_meta` shape is the SDK's, not a
// hand-authored interpretation of it).
//
// Failure modes covered (§17.3 requires ≥1; three are provided): an over-deep
// annotation path refused at the door; a colliding declaration refused at the
// door; a legacy colliding pair failing the CALL with ErrMetaPathCollision and
// no wire request.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	stateInmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// metaNestCapture is one recorded server-side observation of the `_meta` map.
// It is decoded as map[string]any — NOT map[string]string — precisely because
// the shape under test is nested; a map[string]string decode would silently
// drop every nested node and make the test unable to tell nested from flat.
type metaNestCapture struct {
	meta map[string]any
}

type metaNestRecorder struct {
	inner http.Handler
	mu    sync.Mutex
	calls []metaNestCapture
}

func (r *metaNestRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err == nil {
			r.record(body)
		}
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
	}
	r.inner.ServeHTTP(w, req)
}

func (r *metaNestRecorder) record(body []byte) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method != "tools/call" {
		return
	}
	r.mu.Lock()
	r.calls = append(r.calls, metaNestCapture{meta: msg.Params.Meta})
	r.mu.Unlock()
}

func (r *metaNestRecorder) snapshot() []metaNestCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]metaNestCapture, len(r.calls))
	copy(out, r.calls)
	return out
}

func metaNestMCPServer(t *testing.T) (*httptest.Server, *metaNestRecorder) {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-meta-nesting-fixture", Version: "v0"}, nil)
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
	rec := &metaNestRecorder{inner: mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)}
	hs := httptest.NewServer(rec)
	t.Cleanup(hs.Close)
	return hs, rec
}

// metaNestBus builds the real in-mem event bus behind the real pattern
// redactor.
func metaNestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
	}, patternsAudit.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// metaNestInjectionProvider builds the REAL tokenexchange provider against the
// RFC-8693 broker fixture, allow-listing the fixture MCP host.
func metaNestInjectionProvider(t *testing.T, broker *p142Broker, bus events.EventBus, host string) auth.OAuthProvider {
	t.Helper()
	red := patternsAudit.New()
	rawState, err := stateInmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = rawState.Close(context.Background()) })
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*11 + 5)
	}
	sealer, err := auth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	store, err := auth.NewTokenStore(rawState, sealer)
	if err != nil {
		t.Fatalf("token store: %v", err)
	}
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:                   p142Provider,
		CredentialSource:       credsource.Static(p142BrokerClient, p142BrokerSecret),
		Scopes:                 []string{"Mail.Read"},
		TokenURL:               broker.tokenURL(),
		Extra:                  map[string]string{"audience": p142Audience},
		AllowedDownstreamHosts: []string{host},
	}, auth.FactoryDeps{
		Store:       store,
		Bus:         bus,
		Redactor:    red,
		Coordinator: pauseresume.New(pauseresume.WithBus(bus)),
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	return prov
}

// metaNestEnv wires the fixture server + a driver Provider carrying both a
// nested annotation and a credential injection into the SAME `_meta` namespace.
type metaNestEnv struct {
	rec  *metaNestRecorder
	echo tools.ToolDescriptor
}

func newMetaNestEnv(t *testing.T, annotations map[string]string, withInjection bool) *metaNestEnv {
	t.Helper()
	hs, rec := metaNestMCPServer(t)
	bus := metaNestBus(t)

	cfg := mcpdrv.Config{
		Name:            "vendorsrv",
		TransportMode:   mcpdrv.TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		MetaAnnotations: annotations,
	}
	if withInjection {
		broker := newP142Broker(t)
		cfg.Injection = &mcpdrv.CredentialInjection{
			Provider: metaNestInjectionProvider(t, broker, bus, hs.URL),
			Form:     mcpdrv.InjectionFormMeta,
			MetaKey:  []string{"vendor", "api_key"},
		}
	}
	p, err := mcpdrv.New(cfg)
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
		if d.Tool.Name == "vendorsrv_echo" {
			echo = d
		}
	}
	if echo.Invoke == nil {
		t.Fatal("echo descriptor not discovered")
	}
	return &metaNestEnv{rec: rec, echo: echo}
}

func metaNestCtx(t *testing.T, id identity.Identity, agentID string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return tools.WithInvokingAgent(ctx, agentID)
}

// metaNestNode reads a nested `_meta` node, failing the test if the path is not
// a chain of JSON objects — a flat literal key would fail here, which is the
// whole point.
func metaNestNode(t *testing.T, meta map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := meta
	for i, seg := range path {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			t.Fatalf("_meta.%s is %#v, want a nested object", strings.Join(path[:i+1], "."), cur[seg])
		}
		cur = next
	}
	return cur
}

// TestE2E_MCPMetaNesting_AnnotationAndCredentialShareOneNamespace is the
// headline seam: a receiver-style server reading ONE nested namespace receives
// the injected per-user credential AND its non-secret companion annotation,
// which was impossible before — the companion cannot ride `injection` (one
// mapping per connection, and `account_id` is correctly refused by the
// credential-name predicate) and it cannot ride `Headers` (documented secret,
// never persisted).
func TestE2E_MCPMetaNesting_AnnotationAndCredentialShareOneNamespace(t *testing.T) {
	t.Parallel()
	env := newMetaNestEnv(t, map[string]string{
		"vendor.account_id": "acct-42",
		"vendor.region":     "eu-west",
		"deployment":        "prod",
	}, true)

	ctx := metaNestCtx(t, identity.Identity{TenantID: "tenant-1", UserID: "alice", SessionID: "s1"}, "agent-77")
	if _, err := env.echo.Invoke(ctx, json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	calls := env.rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("server saw %d tools/call, want 1", len(calls))
	}
	meta := calls[0].meta

	// The dotted annotation keys are GONE as literals...
	for _, flat := range []string{"vendor.account_id", "vendor.region"} {
		if _, present := meta[flat]; present {
			t.Fatalf("a dotted annotation reached the wire as a literal flat key %q: %#v", flat, meta)
		}
	}
	// ...and present as one nested namespace, alongside the injected credential.
	vendor := metaNestNode(t, meta, "vendor")
	if vendor["account_id"] != "acct-42" || vendor["region"] != "eu-west" {
		t.Fatalf("annotation leaves missing from the nested namespace: %#v", vendor)
	}
	if vendor["api_key"] != "brokered-tenant-1-alice" {
		t.Fatalf("the injected credential is missing or wrong: %#v", vendor)
	}

	// The flat annotation and the identity stamps are untouched.
	if meta["deployment"] != "prod" {
		t.Fatalf("flat annotation changed shape: %#v", meta)
	}
	if meta["tenant"] != "tenant-1" || meta["user"] != "alice" || meta["session"] != "s1" {
		t.Fatalf("_meta triple wrong: %#v", meta)
	}
	if meta["agent_id"] != "agent-77" {
		t.Fatalf("_meta agent_id = %#v, want agent-77", meta["agent_id"])
	}
}

// TestE2E_MCPMetaNesting_IdentityPropagatesAcrossTenants proves the nested
// namespace is per-call and the isolation triple is per-identity: two tenants
// on ONE shared connection see their OWN triple and their OWN per-user
// credential inside the shared namespace, with no cross-talk.
func TestE2E_MCPMetaNesting_IdentityPropagatesAcrossTenants(t *testing.T) {
	t.Parallel()
	env := newMetaNestEnv(t, map[string]string{"vendor.account_id": "acct-42"}, true)

	ids := []identity.Identity{
		{TenantID: "t-a", UserID: "amy", SessionID: "s"},
		{TenantID: "t-b", UserID: "bob", SessionID: "s"},
	}
	for _, id := range ids {
		if _, err := env.echo.Invoke(metaNestCtx(t, id, "agent-x"), json.RawMessage(`{"text":"hi"}`)); err != nil {
			t.Fatalf("Invoke(%s): %v", id.UserID, err)
		}
	}

	calls := env.rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("server saw %d calls, want 2", len(calls))
	}
	wantCred := map[string]string{"amy": "brokered-t-a-amy", "bob": "brokered-t-b-bob"}
	wantTenant := map[string]string{"amy": "t-a", "bob": "t-b"}
	for _, c := range calls {
		user, _ := c.meta["user"].(string)
		exp, known := wantCred[user]
		if !known {
			t.Fatalf("unexpected user in _meta: %#v", c.meta)
		}
		if c.meta["tenant"] != wantTenant[user] {
			t.Fatalf("user %s carried tenant %#v, want %q (cross-tenant bleed)", user, c.meta["tenant"], wantTenant[user])
		}
		vendor := metaNestNode(t, c.meta, "vendor")
		if vendor["api_key"] != exp {
			t.Fatalf("user %s: nested credential %#v, want %q (per-user bleed inside the shared namespace)", user, vendor["api_key"], exp)
		}
		if vendor["account_id"] != "acct-42" {
			t.Fatalf("user %s: the annotation sibling was wiped by the credential write: %#v", user, vendor)
		}
	}
}

// TestE2E_MCPMetaNesting_ConcurrentIdentitiesNoCrossTalk is the §17.3
// concurrency stress across the seam: N concurrent distinct identities on ONE
// shared connection, each asserting its own triple and its own credential
// inside the shared nested namespace.
func TestE2E_MCPMetaNesting_ConcurrentIdentitiesNoCrossTalk(t *testing.T) {
	t.Parallel()
	const n = 16
	env := newMetaNestEnv(t, map[string]string{"vendor.account_id": "acct-42", "fleet": "west"}, true)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i),
				UserID:    fmt.Sprintf("u-%d", i),
				SessionID: "s",
			}
			<-start
			if _, err := env.echo.Invoke(metaNestCtx(t, id, "agent-x"), json.RawMessage(`{"text":"hi"}`)); err != nil {
				t.Errorf("Invoke(%s): %v", id.UserID, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	calls := env.rec.snapshot()
	if len(calls) != n {
		t.Fatalf("server saw %d calls, want %d", len(calls), n)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		user, _ := c.meta["user"].(string)
		tenant, _ := c.meta["tenant"].(string)
		if user == "" || strings.TrimPrefix(user, "u-") != strings.TrimPrefix(tenant, "t-") {
			t.Fatalf("identity bleed under concurrency: %#v", c.meta)
		}
		if seen[user] {
			t.Fatalf("duplicate identity %q — a call was attributed twice", user)
		}
		seen[user] = true
		vendor := metaNestNode(t, c.meta, "vendor")
		wantCred := "brokered-" + tenant + "-" + user
		if vendor["api_key"] != wantCred {
			t.Fatalf("user %s: nested credential %#v, want %q", user, vendor["api_key"], wantCred)
		}
		if vendor["account_id"] != "acct-42" || c.meta["fleet"] != "west" {
			t.Fatalf("annotations drifted under concurrency: %#v", c.meta)
		}
	}
}

// TestE2E_MCPMetaNesting_DoorsRefuseMalformedPaths is failure mode 1 + 2: the
// over-deep path and the colliding declaration are refused at the doors,
// BEFORE anything dials.
func TestE2E_MCPMetaNesting_DoorsRefuseMalformedPaths(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat("a.", config.MaxMCPMetaKeyDepth) + "leaf"

	cases := []struct {
		name        string
		annotations map[string]string
		injection   *config.MCPCredentialInjectionConfig
		wantText    string
	}{
		{"over-deep annotation path", map[string]string{deep: "x"}, nil, "exceeding the cap"},
		{"reserved first segment", map[string]string{"tenant.foo": "x"}, nil, "reserved"},
		{"colliding annotation paths", map[string]string{"vendor": "a", "vendor.id": "b"}, nil, "collide"},
		{
			"flat annotation colliding with the injection _meta path",
			map[string]string{"vendor": "a"},
			&config.MCPCredentialInjectionConfig{Provider: "vendor-broker", Form: config.MCPInjectionFormMeta, MetaKey: "vendor.api_key"},
			"collide",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var closers []func(context.Context) error
			// The attach door — the shared boot + runtime-set path every
			// connection passes through before any dial.
			err := mcpdrv.Attach(context.Background(), config.MCPServerConfig{
				Name:            "vendorsrv",
				TransportMode:   "streamable_http",
				URL:             "https://mcp.invalid/rpc",
				MetaAnnotations: tc.annotations,
				Injection:       tc.injection,
			}, mcpdrv.AttachDeps{
				Catalog:         tools.NewCatalog(),
				Registry:        mcpdrv.NewRegistry(),
				Bus:             metaNestBus(t),
				DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
				Closers:         &closers,
			})
			if err == nil {
				t.Fatalf("the attach door accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err=%q missing %q", err.Error(), tc.wantText)
			}
		})
	}
}

// TestE2E_MCPMetaNesting_LegacyCollisionFailsTheCall is failure mode 3: a
// connection whose PERSISTED declaration carries a colliding annotation pair
// (possible — nothing rejected one before the path rules shipped) fails the
// CALL loud with ErrMetaPathCollision, and issues NO wire request. No silent
// winner, and no order-dependent result either.
func TestE2E_MCPMetaNesting_LegacyCollisionFailsTheCall(t *testing.T) {
	t.Parallel()
	// Constructed through the driver directly, bypassing the doors, exactly as
	// a revision persisted before the rules shipped would rehydrate.
	env := newMetaNestEnv(t, map[string]string{"vendor": "flat", "vendor.id": "nested"}, false)

	before := len(env.rec.snapshot())
	ctx := metaNestCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, "agent-x")
	_, err := env.echo.Invoke(ctx, json.RawMessage(`{"text":"hi"}`))
	if err == nil {
		t.Fatal("a colliding legacy annotation pair produced a successful call — one annotation was silently discarded")
	}
	if !errors.Is(err, mcpdrv.ErrMetaPathCollision) {
		t.Fatalf("want ErrMetaPathCollision, got %v", err)
	}
	if after := len(env.rec.snapshot()); after != before {
		t.Fatalf("a wire request was issued despite the collision (%d -> %d)", before, after)
	}
}
