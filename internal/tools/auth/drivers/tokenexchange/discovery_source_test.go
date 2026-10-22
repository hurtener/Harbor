package tokenexchange_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"

	// This test composes only the real MCP and exchange drivers under test.
	mcpdriver "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

type discoverySourceAuthority struct {
	pair           agentcfg.SignedOAuthMCPPair
	active, usable atomic.Bool
}

func (*discoverySourceAuthority) ResolveAgent(context.Context, identity.Identity, string) (bool, error) {
	return false, errors.New("source-addressed discovery must not select a default agent")
}

func (a *discoverySourceAuthority) Active(_ context.Context, q identity.Quadruple, agent string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	if q.TenantID != "tenant-signed" || agent != a.pair.OwnerAgentID || scope != agentcfg.ConfigScopeAgent {
		return agentcfg.Revision{}, false, errors.New("wrong owning configuration slot")
	}
	pairs := agentcfg.SignedOAuthMCPPairs{a.pair.ProviderName: a.pair}
	return agentcfg.Revision{Payload: agentcfg.ConfigPayload{SignedOAuthMCPPairs: &pairs}}, a.active.Load(), nil
}

func (a *discoverySourceAuthority) AuthorizeSignedCapabilityUse(_ context.Context, _, _, _ string, preparation bool) error {
	if preparation || !a.usable.Load() {
		return errors.New("fixture capability use denied")
	}
	return nil
}

// The real Protocol edge, source authorizer, registry, pair-private MCP
// provider, exchange driver and SDK server reproduce the production failure.
// No preparation marker or boot invoking-agent attribution is used by the
// source-addressed admin requests. The broker sees the live caller triple.
func TestSignedCapability_SourceAddressedDiscovery(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "discovery-fixture", Version: "v0"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "existing", InputSchema: map[string]any{"type": "object"}}, nil)
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var requests atomic.Int64
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer opaque-token" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	binding := signedCapabilityBindingFixture(front.URL)
	owner := auth.Owner{Tenant: binding.TenantID, Agent: binding.AgentID}
	const logical = "paired-source"
	physical := mcpdriver.PhysicalServerName(logical, owner)
	authority := &discoverySourceAuthority{pair: agentcfg.SignedOAuthMCPPair{
		ProviderName: binding.ProviderName, OwnerAgentID: owner.Agent,
		OwnerUserID: binding.UserID, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: logical},
	}}
	authority.active.Store(true)
	authority.usable.Store(true)
	binding.UseAuthorizer = authority
	broker := newJWTBroker(t)
	broker.responseAudience, broker.responseResource = binding.Audience, binding.Resource
	var subjects sync.Map
	broker.tokenFn = func(form map[string][]string) string {
		subject := decodeSubject(form["subject_token"][0])
		if subject.TenantID != owner.Tenant || subject.UserID != "live-reader" || subject.SessionID == "" {
			t.Errorf("wrong live exchange subject: %+v", subject)
		}
		subjects.Store(subject.SessionID, true)
		return "opaque-token"
	}
	deps, _, _ := mkDeps(t)
	oauth, err := tokenexchange.New(auth.ProviderConfig{
		Name: binding.ProviderName, CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		TokenURL: broker.tokenURL(), Audience: binding.Audience, ResourceIndicator: binding.Resource,
		AllowedDownstreamHosts: []string{"127.0.0.1"}, SignedCapability: &binding,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	p, err := mcpdriver.New(mcpdriver.Config{
		Name: physical, URL: front.URL, TransportMode: mcpdriver.TransportStreamableHTTP,
		Bus: deps.Bus, OAuthProvider: oauth, OwnOAuthProvider: true,
		DefaultIdentity: identity.Identity{TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	caller := identity.Identity{TenantID: owner.Tenant, UserID: "live-reader", SessionID: "live-session"}
	baseCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ctxFor := func(id identity.Identity, reach string) context.Context {
		ctx, err := identity.WithVerified(baseCtx, id)
		if err != nil {
			t.Fatal(err)
		}
		return protocolauth.WithAgentReach(protocolauth.WithScopes(ctx, []protocolauth.Scope{protocolauth.ScopeAdmin}), []string{reach})
	}
	ctx := tools.WithInvokingAgent(ctxFor(caller, owner.Agent), "unrelated-boot-agent")
	if err := p.Connect(tools.WithEffectiveAgentConfig(ctx, owner.Agent)); err != nil {
		t.Fatal(err)
	}
	initial, err := p.Discover(tools.WithEffectiveAgentConfig(ctx, owner.Agent))
	if err != nil {
		t.Fatal(err)
	}
	reg := mcpdriver.NewRegistry()
	if err := reg.Register(ctx, mcpdriver.ServerRegistration{Provider: p, Owner: owner, LogicalName: logical,
		Transport: "streamable-http", InitialState: mcpdriver.ServerStateOnline}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RecordDiscovery(physical, initial); err != nil {
		t.Fatal(err)
	}
	accessor, err := mcpconsole.NewRegistryAccessor(reg, mcpconsole.WithSourceAuthorizer(mcpconsole.NewSourceAuthorizer(reg, authority)))
	if err != nil {
		t.Fatal(err)
	}
	surface, err := protocol.NewMCPSurface(protocol.MCPDeps{MCP: accessor, OAuth: mcpconsole.NewNoOAuthAccessor(), Redactor: deps.Redactor, Bus: deps.Bus, AgentResolver: authority})
	if err != nil {
		t.Fatal(err)
	}
	call := func(ctx context.Context, probe bool) error {
		if probe {
			v, err := surface.Dispatch(ctx, methods.MethodMCPServersProbe, &types.MCPServerProbeRequest{Name: physical})
			if err == nil && !v.(*types.MCPServerProbeResponse).OK {
				return errors.New(v.(*types.MCPServerProbeResponse).Error)
			}
			return err
		}
		_, err := surface.Dispatch(ctx, methods.MethodMCPServersRefreshDiscovery, &types.MCPServerRefreshDiscoveryRequest{Name: physical})
		return err
	}
	if _, err := p.Discover(ctx); err == nil || !strings.Contains(err.Error(), "signed capability effective agent mismatch") {
		t.Fatalf("unseated discovery must reproduce the live refusal: %v", err)
	}
	// New App-only metadata starts absent in the retained registry catalog.
	tool := physical + "_metadata_list"
	if _, found := reg.ResolveAppTool(physical, tool); found {
		t.Fatal("new metadata already present")
	}
	server.AddTool(&mcpsdk.Tool{Name: "metadata_list", InputSchema: map[string]any{"type": "object"},
		Meta: mcpsdk.Meta{"ui": map[string]any{"visibility": []string{"app"}}}}, nil)
	for _, probe := range []bool{false, true} {
		if err := call(ctx, probe); err != nil {
			t.Fatalf("discovery probe=%t: %v", probe, err)
		}
	}
	if _, found := reg.ResolveAppTool(physical, tool); !found {
		t.Fatal("refresh did not publish new App-only metadata")
	}
	if got := decodeSubject(broker.formValue("subject_token")); got != (subjectTriple{TenantID: caller.TenantID, UserID: caller.UserID, SessionID: caller.SessionID}) {
		t.Fatalf("exchange used wrong live caller: %+v", got)
	}
	if _, present := tools.EffectiveAgentConfigFrom(ctx); present {
		t.Fatal("discovery mutated the caller context")
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"different-agent", tools.WithEffectiveAgentConfig(ctx, "another-agent")},
		{"reach-denied", ctxFor(caller, "another-agent")},
		{"foreign-tenant", ctxFor(identity.Identity{TenantID: "foreign-tenant", UserID: caller.UserID, SessionID: caller.SessionID}, owner.Agent)},
		{"no-admin", protocolauth.WithScopes(ctx, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, exchanges := requests.Load(), broker.callCount()
			for _, probe := range []bool{false, true} {
				if err := call(tc.ctx, probe); err == nil {
					t.Fatalf("denied request admitted probe=%t", probe)
				}
			}
			if requests.Load() != before || broker.callCount() != exchanges {
				t.Fatal("denied request reached source/broker")
			}
		})
	}
	for _, tc := range []struct {
		name string
		flag *atomic.Bool
	}{{"current-config-removed", &authority.active}, {"current-grant-revoked", &authority.usable}} {
		t.Run(tc.name, func(t *testing.T) {
			tc.flag.Store(false)
			defer tc.flag.Store(true)
			before, exchanges := requests.Load(), broker.callCount()
			for _, probe := range []bool{false, true} {
				if err := call(ctx, probe); err == nil {
					t.Fatalf("revoked current authority admitted probe=%t", probe)
				}
			}
			if requests.Load() != before || broker.callCount() != exchanges {
				t.Fatal("revoked authority reached source/broker")
			}
		})
	}
	// One provider concurrently serves distinct verified sessions; each request
	// carries its own live subject and source selection without context mutation.
	var wg sync.WaitGroup
	for i := range 128 {
		id := caller
		id.SessionID = fmt.Sprintf("session-%d", i)
		c := ctxFor(id, owner.Agent)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := call(c, true); err != nil {
				t.Error(err)
			}
			if _, present := tools.EffectiveAgentConfigFrom(c); present {
				t.Error("concurrent caller context mutated")
			}
		}()
	}
	wg.Wait()
	for i := range 128 {
		if _, ok := subjects.Load(fmt.Sprintf("session-%d", i)); !ok {
			t.Errorf("missing live exchange subject session-%d", i)
		}
	}
}
