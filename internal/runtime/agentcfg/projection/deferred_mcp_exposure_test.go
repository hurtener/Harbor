package projection_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// The production run uses the default always-only prompt filter. Capability
// exclusions and legacy resume resolution must also cover hidden deferred tools.
func TestDeferredMCPExposure_AllTiersAndLoadingModes(t *testing.T) {
	for _, scope := range []string{"agent", "user"} {
		for _, loading := range []string{"always", "deferred", "demoted"} {
			for _, tier := range []string{"admin", "user", "session", "none"} {
				for _, restriction := range []string{"server", "tool"} {
					t.Run(scope+"/"+loading+"/"+tier+"/"+restriction, func(t *testing.T) {
						ctx := context.Background()
						reg := newRegistry(t)
						overlay := newOverlay(t)
						cat := tools.NewCatalog()
						owner := auth.Owner{Tenant: projID().TenantID, Agent: projAgent}
						if scope == "user" {
							owner.User = projID().UserID
						}
						source := tools.ToolSourceID(mcp.PhysicalServerName("shared", owner))
						physical := string(source) + "_echo"
						mode := tools.LoadingAlways
						if loading == "deferred" {
							mode = tools.LoadingDeferred
						}
						invoke := func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }
						if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: physical, Source: source, Loading: mode}, Invoke: invoke}); err != nil {
							t.Fatal(err)
						}
						if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "control", Loading: tools.LoadingAlways}, Invoke: invoke}); err != nil {
							t.Fatal(err)
						}
						resolver := testPhysicalSourceResolver{owners: map[tools.ToolSourceID]auth.Owner{source: owner}, logical: map[tools.ToolSourceID]string{source: "shared"}}
						admin := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "shared", Transport: agentcfg.MCPTransportHTTP, URL: "https://example.test/mcp"}}}, ToolExposure: &agentcfg.ToolExposure{}}
						if loading == "demoted" {
							admin.ToolExposure.ToolLoadingModes = map[string]string{"shared_echo": "deferred"}
						}
						restricted := &agentcfg.ToolExposure{}
						if restriction == "server" {
							restricted.PausedServers = []string{"shared"}
						} else {
							restricted.DisabledTools = []string{"shared_echo"}
						}
						user := agentcfg.ConfigPayload{}
						if scope == "user" {
							user.SignedOAuthMCPPair = &agentcfg.SignedOAuthMCPPair{ProviderName: "provider", Broker: "broker", Audience: "audience", Scopes: []string{"read"}, CapabilityRevision: "capability", URLDigest: agentcfg.OAuthMCPURLDigest("https://example.test/mcp"), Sink: "https://example.test", SinkDigest: agentcfg.OAuthMCPURLDigest("https://example.test"), Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: "shared", URL: "https://example.test/mcp"}, AuthorityOperationKind: "operation", OwnerAgentID: projAgent, OwnerUserID: projID().UserID, OwnerSessionID: projID().SessionID}
						}
						switch tier {
						case "admin":
							admin.ToolExposure.PausedServers = restricted.PausedServers
							admin.ToolExposure.DisabledTools = restricted.DisabledTools
						case "user":
							user.ToolExposure = restricted
						case "session":
							if _, err := overlay.SetSourceDisables(ctx, projID(), projAgent, restricted.PausedServers, restricted.DisabledTools); err != nil {
								t.Fatal(err)
							}
						}
						if scope == "user" || tier == "user" {
							userCtx := ctx
							if user.SignedOAuthMCPPair != nil {
								userCtx = agentcfg.WithSignedOAuthMCPFenceOperation(ctx, "operation")
							}
							if _, err := reg.SetRevision(userCtx, projID(), projAgent, agentcfg.ConfigScopeUser, user, agentcfg.SetOptions{}); err != nil {
								t.Fatal(err)
							}
						}
						if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, admin, agentcfg.SetOptions{}); err != nil {
							t.Fatal(err)
						}
						view, err := projection.ActivePlannerCatalogView(ctx, reg, overlay, projAgent, projID(), cat, tools.CatalogFilter{}, resolver)
						if err != nil {
							t.Fatal(err)
						}
						for _, name := range []string{physical, "shared_echo"} {
							got, ok := view.Resolve(name)
							if scope == "user" && name == "shared_echo" {
								if ok {
									t.Fatal("personal source gained legacy alias")
								}
								continue
							}
							if tier != "none" {
								if ok {
									t.Errorf("restricted %s resolved: %+v", name, got)
								}
								continue
							}
							if !ok || got.Name != physical {
								t.Fatalf("legitimate resume %s: %+v %v", name, got, ok)
							}
							wantMode := tools.LoadingAlways
							if loading != "always" {
								wantMode = tools.LoadingDeferred
							}
							if got.Loading != wantMode {
								t.Errorf("effective loading=%s want %s", got.Loading, wantMode)
							}
						}
						names := viewNames(view)
						if !hasName(names, "control") {
							t.Fatalf("always control hidden: %v", names)
						}
						wantPrompt := tier == "none" && loading == "always"
						if hasName(names, physical) != wantPrompt {
							t.Fatalf("prompt list %v: target presence want %v", names, wantPrompt)
						}
					})
				}
			}
		}
	}
}
