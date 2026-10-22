package mcpconsole_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// This fixture exercises the actual official SDK resources/read response through
// the connected Provider, current registry/source authorizer, accessor and final
// Protocol DTO. It performs one resource read, never a tool/model invocation.
func TestAppResourceMetadata_ActualAdmittedProtocolRead(t *testing.T) {
	for _, mode := range []string{"ordinary", "admission", "absent", "malformed", "withdrawn_during_read", "foreign_user", "foreign_tenant", "wrong_agent", "non_app"} {
		t.Run(mode, func(t *testing.T) {
			const uri = "ui://sample/app.html"
			owner := auth.Owner{Tenant: "t-1", User: "u-1", Agent: appsAdmissionAgentID}
			physical := mcp.PhysicalServerName("sample-app", owner)
			config := &mutableUserConfig{active: true, pair: agentcfg.SignedOAuthMCPPair{ProviderName: "sample", OwnerAgentID: owner.Agent, OwnerUserID: owner.User, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: "sample-app"}}}
			reads := new(atomic.Int64)
			srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "resource-metadata-fixture", Version: "1"}, nil)
			read := func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				reads.Add(1)
				if mode == "withdrawn_during_read" {
					config.deactivate()
				}
				meta := mcpsdk.Meta{"ui": map[string]any{"csp": map[string]any{"resourceDomains": []any{"https://assets.example.com"}, "connectDomains": []any{"https://private.example.com"}}, "permissions": map[string]any{"camera": true}}, "secret": "hidden"}
				if mode == "absent" {
					meta = nil
				}
				if mode == "malformed" || mode == "non_app" {
					meta = mcpsdk.Meta{"ui": map[string]any{"csp": map[string]any{"resourceDomains": "malformed"}}}
				}
				return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: mcp.ResourceMIMEType, Text: `<img src="https://not-declared.example.com/a">`, Meta: meta}}}, nil
			}
			srv.AddResource(&mcpsdk.Resource{URI: uri, Name: "app", MIMEType: mcp.ResourceMIMEType}, read)
			srv.AddResource(&mcpsdk.Resource{URI: "repo://sample/file", Name: "file"}, read)
			hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
			t.Cleanup(hs.Close)
			bus := newAppsBus(t)
			p, err := mcp.New(mcp.Config{Name: physical, TransportMode: mcp.TransportStreamableHTTP, URL: hs.URL, Bus: bus, DefaultIdentity: identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "s-1"}, DefaultPolicy: tools.DefaultPolicy()})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := p.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close(context.Background()) })
			reg := mcp.NewRegistry()
			descs, err := p.Discover(ctx)
			if err != nil {
				t.Fatal(err)
			}
			swap, err := reg.StageRegistration(mcp.ServerRegistration{Provider: p, Owner: owner, LogicalName: "sample-app", Transport: string(mcp.TransportStreamableHTTP), URLOrCommand: hs.URL, InitialState: mcp.ServerStateOnline}, descs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := swap.Publish(ctx, nil); err != nil {
				t.Fatal(err)
			}
			acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{Registry: reg, Catalog: tools.NewCatalog(), Store: newAppsStore(t), Bus: bus, ToolContext: newAppsToolCtx(t), SourceAuthorizer: mcpconsole.NewSourceAuthorizer(reg, config)})
			if err != nil {
				t.Fatal(err)
			}
			surface, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: acc, Invoker: acc, ToolContext: acc, AgentResolver: &admissionAgentResolver{}, AgentReach: allowAdmissionReach{}, RenderAdmissionAuthority: &admissionAuthoritySeam{auth: newAdmissionAuthority(t)}, RenderAdmissionGate: &realAdmissionGate{acc: acc}})
			if err != nil {
				t.Fatal(err)
			}
			id := identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "s-1"}
			if mode == "foreign_user" {
				id.UserID = "another"
			}
			if mode == "foreign_tenant" {
				id.TenantID = "another"
			}
			verified, err := identity.WithVerified(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			verified = protocolauth.WithAgentReach(verified, []string{owner.Agent})
			req := &types.ReadMCPResourceRequest{Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, ServerID: physical, ResourceURI: uri, RequestRenderAdmission: mode == "admission"}
			if mode == "wrong_agent" {
				req.AgentID = "another"
			}
			if mode == "non_app" {
				req.ResourceURI = "repo://sample/file"
			}
			result, err := surface.Dispatch(verified, methods.MethodMCPReadResource, req)
			denied := mode == "malformed" || mode == "withdrawn_during_read" || mode == "foreign_user" || mode == "foreign_tenant" || mode == "wrong_agent"
			if denied {
				if err == nil || result != nil {
					t.Fatalf("denied request disclosed result: %+v %v", result, err)
				}
				if (mode == "foreign_user" || mode == "foreign_tenant" || mode == "wrong_agent") && reads.Load() != 0 {
					t.Fatal("cross-scope request reached provider")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response := result.(*types.ReadMCPResourceResponse)
			wire, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			// Loopback HTTP fixture has no public HTTPS provider origin. It cannot
			// self-assert an origin through HTML, _meta, headers or endpoint paths.
			if response.ProviderOrigin != "" {
				t.Fatal("private fixture endpoint disclosed")
			}
			if mode == "absent" || mode == "non_app" {
				if response.Meta != nil {
					t.Fatal("invented metadata")
				}
			} else {
				if response.Meta == nil || len(response.Meta.UI.CSP.ResourceDomains) != 1 || response.Meta.UI.CSP.ResourceDomains[0] != "https://assets.example.com" {
					t.Fatalf("standard metadata lost: %s", wire)
				}
				for _, field := range []string{"secret", "permissions", "connectDomains", "private.example.com", "not-declared.example.com\"]"} {
					if strings.Contains(string(wire), field) {
						t.Fatalf("unrecognized metadata projected: %s", wire)
					}
				}
			}
			if reads.Load() != 1 {
				t.Fatalf("reads=%d; expected single resources/read", reads.Load())
			}
			if mode == "admission" {
				if response.RenderAdmission == nil || response.RenderAdmission.Availability != types.RenderAdmissionAvailable {
					t.Fatal("valid admitted read lost admission")
				}
			} else if response.RenderAdmission != nil {
				t.Fatal("ordinary read minted authority")
			}
		})
	}
}

// The existing agent resolver's post-I/O decision is independent of source
// ownership: boot sources may still not disclose an App after agent retirement.
type resourceReadAgent struct{ retired atomic.Bool }

func (*resourceReadAgent) EffectiveAgentID(requested string) (string, error) {
	if requested == "" {
		return appsAdmissionAgentID, nil
	}
	return requested, nil
}
func (r *resourceReadAgent) ResolveAgent(context.Context, identity.Identity, string) (bool, error) {
	if r.retired.Load() {
		return false, protocol.ErrAgentRetired
	}
	return true, nil
}

type withdrawingResourceReader struct{ agent *resourceReadAgent }

func (r withdrawingResourceReader) ReadResource(context.Context, string, string) (protocol.MCPResourceContent, error) {
	r.agent.retired.Store(true)
	return protocol.MCPResourceContent{Inline: []byte("document"), ProviderOrigin: "https://api.example.com", ResourceDomains: []string{"https://assets.example.com"}}, nil
}
func TestAppResourceMetadata_FinalAgentFence(t *testing.T) {
	agent := new(resourceReadAgent)
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{Registry: newAppsRegistry(t, []byte("app")), Catalog: tools.NewCatalog(), Store: newAppsStore(t), Bus: newAppsBus(t), ToolContext: newAppsToolCtx(t)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: withdrawingResourceReader{agent}, Invoker: acc, ToolContext: acc, AgentResolver: agent, AgentReach: allowAdmissionReach{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{Identity: admissionID(), ServerID: "sample", ResourceURI: "ui://sample/app"})
	if err == nil || result != nil {
		t.Fatal("retired agent released resource metadata")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Fatalf("wrong refusal: %v", err)
	}
}
