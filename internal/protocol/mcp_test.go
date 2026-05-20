package protocol_test

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// stubMCP is a deterministic protocol.MCPAccessor for the MCPSurface
// unit tests — no `mcp` driver, no wire.
type stubMCP struct {
	servers   map[string]protocol.MCPServerRow
	notFound  bool
	missingID bool
}

func mcpNotFoundErr() error  { return stderrors.New("mcp: server not found: x") }
func mcpMissingIDErr() error { return stderrors.New("mcp: identity missing from ctx") }

func (s *stubMCP) ListServers(_ context.Context, _ protocol.MCPListFilter) ([]protocol.MCPServerRow, string, error) {
	if s.missingID {
		return nil, "", mcpMissingIDErr()
	}
	out := make([]protocol.MCPServerRow, 0, len(s.servers))
	for _, v := range s.servers {
		out = append(out, v)
	}
	return out, "", nil
}

func (s *stubMCP) GetServer(_ context.Context, name string) (protocol.MCPServerRow, error) {
	if s.notFound {
		return protocol.MCPServerRow{}, mcpNotFoundErr()
	}
	v, ok := s.servers[name]
	if !ok {
		return protocol.MCPServerRow{}, mcpNotFoundErr()
	}
	return v, nil
}

func (s *stubMCP) ListResources(_ context.Context, name string) ([]protocol.MCPResourceRow, error) {
	if s.notFound {
		return nil, mcpNotFoundErr()
	}
	return []protocol.MCPResourceRow{{URI: "repo://x", Name: "x"}}, nil
}

func (s *stubMCP) ListPrompts(_ context.Context, name string) ([]protocol.MCPPromptRow, error) {
	if s.notFound {
		return nil, mcpNotFoundErr()
	}
	return []protocol.MCPPromptRow{{Name: "pr"}}, nil
}

func (s *stubMCP) RefreshDiscovery(_ context.Context, name string) (protocol.MCPDiscoveryRow, error) {
	if s.notFound {
		return protocol.MCPDiscoveryRow{}, mcpNotFoundErr()
	}
	return protocol.MCPDiscoveryRow{DiscoveryID: name + "-disc-1", ToolCount: 1}, nil
}

func (s *stubMCP) Probe(_ context.Context, name string) (protocol.MCPProbeRow, error) {
	if s.notFound {
		return protocol.MCPProbeRow{}, mcpNotFoundErr()
	}
	return protocol.MCPProbeRow{OK: true, LatencyMs: 12}, nil
}

func (s *stubMCP) Health(_ context.Context, name string) (protocol.MCPHealthRow, error) {
	if s.notFound {
		return protocol.MCPHealthRow{}, mcpNotFoundErr()
	}
	return protocol.MCPHealthRow{
		HandshakeLatencyBuckets: []protocol.MCPHealthBucketRow{{StartMs: 1, LatencyMs: 12}},
		ReconnectHistory:        []protocol.MCPReconnectRow{},
	}, nil
}

func (s *stubMCP) SetRawHTMLTrust(_ context.Context, name string, trusted bool) (bool, error) {
	if s.notFound {
		return false, mcpNotFoundErr()
	}
	v := s.servers[name]
	prev := v.RawHTMLTrusted
	v.RawHTMLTrusted = trusted
	s.servers[name] = v
	return prev, nil
}

// stubOAuth is a deterministic protocol.MCPOAuthAccessor.
type stubOAuth struct {
	bindings []protocol.MCPBindingRow
}

func (s *stubOAuth) ListBindings(_ context.Context, _ string) ([]protocol.MCPBindingRow, error) {
	return s.bindings, nil
}

func (s *stubOAuth) InitiateBinding(_ context.Context, server, _ string) (string, string, error) {
	return "https://auth.example.com/authorize?server=" + server, "state-" + server, nil
}

func (s *stubOAuth) RevokeBinding(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

func newMCPBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func newMCPSurface(t *testing.T) (*protocol.MCPSurface, events.EventBus) {
	t.Helper()
	bus := newMCPBus(t)
	s, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP: &stubMCP{servers: map[string]protocol.MCPServerRow{
			"github-server": {
				Name: "github-server", Transport: "http+sse", State: "online",
				ToolCount: 3, OAuthBindingCount: 2, PolicyTimeoutMs: 30000, PolicyMaxRetries: 3,
			},
		}},
		OAuth: &stubOAuth{bindings: []protocol.MCPBindingRow{
			{PrincipalID: "u-1", BindingScope: "user", Scopes: []string{"repo"}},
		}},
		Redactor: patterns.New(),
		Bus:      bus,
		Clock:    func() time.Time { return time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewMCPSurface: %v", err)
	}
	return s, bus
}

func validScope() types.IdentityScope {
	return types.IdentityScope{Tenant: "t-1", User: "u-1", Session: "s-1"}
}

// assertCode asserts err is a *protoerrors.Error with the given Code.
func assertCode(t *testing.T, err error, want protoerrors.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error with code %q, got nil", want)
	}
	var perr *protoerrors.Error
	if !stderrors.As(err, &perr) {
		t.Fatalf("want *protoerrors.Error, got %T: %v", err, err)
	}
	if perr.Code != want {
		t.Fatalf("want code %q, got %q (%v)", want, perr.Code, err)
	}
}

func TestMCPSurface_NewMCPSurface_FailsClosed(t *testing.T) {
	cases := []struct {
		name string
		mut  func(d *protocol.MCPDeps)
	}{
		{"nil MCP", func(d *protocol.MCPDeps) { d.MCP = nil }},
		{"nil OAuth", func(d *protocol.MCPDeps) { d.OAuth = nil }},
		{"nil Redactor", func(d *protocol.MCPDeps) { d.Redactor = nil }},
		{"nil Bus", func(d *protocol.MCPDeps) { d.Bus = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := protocol.MCPDeps{
				MCP: &stubMCP{}, OAuth: &stubOAuth{}, Redactor: patterns.New(), Bus: newMCPBus(t),
			}
			tc.mut(&d)
			if _, err := protocol.NewMCPSurface(d); !stderrors.Is(err, protocol.ErrMCPMisconfigured) {
				t.Fatalf("want ErrMCPMisconfigured, got %v", err)
			}
		})
	}
}

func TestMCPSurface_List_Happy(t *testing.T) {
	s, _ := newMCPSurface(t)
	resp, err := s.Dispatch(context.Background(), methods.MethodMCPServersList,
		&types.MCPServersListRequest{Identity: validScope()})
	if err != nil {
		t.Fatalf("Dispatch list: %v", err)
	}
	lr, ok := resp.(*types.MCPServersListResponse)
	if !ok {
		t.Fatalf("want *MCPServersListResponse, got %T", resp)
	}
	if len(lr.Servers) != 1 || lr.Servers[0].Name != "github-server" {
		t.Fatalf("list shape wrong: %+v", lr.Servers)
	}
}

func TestMCPSurface_List_FailsClosed_MissingIdentity(t *testing.T) {
	s, _ := newMCPSurface(t)
	_, err := s.Dispatch(context.Background(), methods.MethodMCPServersList,
		&types.MCPServersListRequest{})
	assertCode(t, err, protoerrors.CodeIdentityRequired)
}

func TestMCPSurface_Get_NotFound(t *testing.T) {
	s, _ := newMCPSurface(t)
	_, err := s.Dispatch(context.Background(), methods.MethodMCPServersGet,
		&types.MCPServerGetRequest{Identity: validScope(), Name: "nope"})
	assertCode(t, err, protoerrors.CodeNotFound)
}

func TestMCPSurface_Get_Happy(t *testing.T) {
	s, _ := newMCPSurface(t)
	resp, err := s.Dispatch(context.Background(), methods.MethodMCPServersGet,
		&types.MCPServerGetRequest{Identity: validScope(), Name: "github-server"})
	if err != nil {
		t.Fatalf("Dispatch get: %v", err)
	}
	gr := resp.(*types.MCPServerGetResponse)
	if gr.Server.Name != "github-server" {
		t.Fatalf("get shape wrong: %+v", gr)
	}
	if len(gr.BindingsSummary) != 1 || gr.BindingsSummary[0].BindingScope != "user" {
		t.Fatalf("bindings summary wrong: %+v", gr.BindingsSummary)
	}
}

func TestMCPSurface_Resources_Prompts_Health_Policy_Bindings(t *testing.T) {
	s, _ := newMCPSurface(t)
	if _, err := s.Dispatch(context.Background(), methods.MethodMCPServersResources,
		&types.MCPServerResourcesRequest{Identity: validScope(), Name: "github-server"}); err != nil {
		t.Fatalf("resources: %v", err)
	}
	if _, err := s.Dispatch(context.Background(), methods.MethodMCPServersPrompts,
		&types.MCPServerPromptsRequest{Identity: validScope(), Name: "github-server"}); err != nil {
		t.Fatalf("prompts: %v", err)
	}
	if _, err := s.Dispatch(context.Background(), methods.MethodMCPServersHealth,
		&types.MCPServerHealthRequest{Identity: validScope(), Name: "github-server"}); err != nil {
		t.Fatalf("health: %v", err)
	}
	if _, err := s.Dispatch(context.Background(), methods.MethodMCPServersPolicy,
		&types.MCPServerPolicyRequest{Identity: validScope(), Name: "github-server"}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := s.Dispatch(context.Background(), methods.MethodMCPServersBindingsList,
		&types.MCPServerBindingsListRequest{Identity: validScope(), Name: "github-server"}); err != nil {
		t.Fatalf("bindings.list: %v", err)
	}
}

func TestMCPSurface_RefreshDiscovery_RequiresAdmin(t *testing.T) {
	s, _ := newMCPSurface(t)
	// Without admin scope → scope_mismatch.
	_, err := s.Dispatch(context.Background(), methods.MethodMCPServersRefreshDiscovery,
		&types.MCPServerRefreshDiscoveryRequest{Identity: validScope(), Name: "github-server"})
	assertCode(t, err, protoerrors.CodeScopeMismatch)

	// With admin scope → 200.
	adminCtx := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	resp, err := s.Dispatch(adminCtx, methods.MethodMCPServersRefreshDiscovery,
		&types.MCPServerRefreshDiscoveryRequest{Identity: validScope(), Name: "github-server"})
	if err != nil {
		t.Fatalf("refresh_discovery as admin: %v", err)
	}
	if resp.(*types.MCPServerRefreshDiscoveryResponse).DiscoveryID == "" {
		t.Fatalf("want a discovery id")
	}
}

func TestMCPSurface_Probe_RequiresAdmin(t *testing.T) {
	s, _ := newMCPSurface(t)
	_, err := s.Dispatch(context.Background(), methods.MethodMCPServersProbe,
		&types.MCPServerProbeRequest{Identity: validScope(), Name: "github-server"})
	assertCode(t, err, protoerrors.CodeScopeMismatch)
}

func TestMCPSurface_RefreshBinding_RequiresAdmin(t *testing.T) {
	s, _ := newMCPSurface(t)
	_, err := s.Dispatch(context.Background(), methods.MethodMCPServersRefreshBinding,
		&types.MCPServerRefreshBindingRequest{Identity: validScope(), Name: "github-server"})
	assertCode(t, err, protoerrors.CodeScopeMismatch)

	adminCtx := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	resp, err := s.Dispatch(adminCtx, methods.MethodMCPServersRefreshBinding,
		&types.MCPServerRefreshBindingRequest{Identity: validScope(), Name: "github-server"})
	if err != nil {
		t.Fatalf("refresh_binding as admin: %v", err)
	}
	rb := resp.(*types.MCPServerRefreshBindingResponse)
	if rb.AuthorizeURL == "" || rb.State == "" {
		t.Fatalf("refresh_binding shape wrong: %+v", rb)
	}
}

func TestMCPSurface_RevokeBinding_RequiresAdmin(t *testing.T) {
	s, _ := newMCPSurface(t)
	_, err := s.Dispatch(context.Background(), methods.MethodMCPServersRevokeBinding,
		&types.MCPServerRevokeBindingRequest{Identity: validScope(), Name: "github-server"})
	assertCode(t, err, protoerrors.CodeScopeMismatch)
}

func TestMCPSurface_SetRawHTMLTrust_RequiresAdmin_AndEmitsAudit(t *testing.T) {
	s, bus := newMCPSurface(t)

	// Without admin → scope_mismatch.
	_, err := s.Dispatch(context.Background(), methods.MethodMCPServersSetRawHTMLTrust,
		&types.MCPServerSetRawHTMLTrustRequest{Identity: validScope(), Name: "github-server", Trusted: true})
	assertCode(t, err, protoerrors.CodeScopeMismatch)

	// Subscribe to the audit event before the admin call.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  "t-1",
		User:    "u-1",
		Session: "s-1",
		Types:   []events.EventType{events.EventTypeMCPRawHTMLTrustToggled},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	adminCtx := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	resp, err := s.Dispatch(adminCtx, methods.MethodMCPServersSetRawHTMLTrust,
		&types.MCPServerSetRawHTMLTrustRequest{Identity: validScope(), Name: "github-server", Trusted: true})
	if err != nil {
		t.Fatalf("set_raw_html_trust as admin: %v", err)
	}
	tr := resp.(*types.MCPServerSetRawHTMLTrustResponse)
	if !tr.Trusted || tr.Name != "github-server" {
		t.Fatalf("set_raw_html_trust shape wrong: %+v", tr)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeMCPRawHTMLTrustToggled {
			t.Fatalf("want raw_html_trust_toggled event, got %s", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for raw_html_trust_toggled event")
	}
}

func TestMCPSurface_Dispatch_UnknownMethod(t *testing.T) {
	s, _ := newMCPSurface(t)
	_, err := s.Dispatch(context.Background(), methods.MethodStart, &types.MCPServersListRequest{})
	assertCode(t, err, protoerrors.CodeUnknownMethod)
}

func TestMCPSurface_Dispatch_InvalidRequestType(t *testing.T) {
	s, _ := newMCPSurface(t)
	_, err := s.Dispatch(context.Background(), methods.MethodMCPServersList, nil)
	assertCode(t, err, protoerrors.CodeInvalidRequest)
}
