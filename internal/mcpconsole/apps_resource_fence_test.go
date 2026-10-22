package mcpconsole_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/mcpconsole/admission"
	"github.com/hurtener/Harbor/internal/protocol"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

type resourceFenceProvider struct {
	*stubProvider
	reads atomic.Int64
}

func (p *resourceFenceProvider) ReadResourceWithMetadata(context.Context, string) (mcp.ResourceRead, error) {
	p.reads.Add(1)
	return mcp.ResourceRead{Content: []byte("original-document"), MIMEType: mcp.ResourceMIMEType, ResourceDomains: []string{"https://assets.example.com"}}, nil
}

type resourceFenceAgent struct {
	calls        int
	afterResolve func()
	retired      bool
}

func (*resourceFenceAgent) EffectiveAgentID(requested string) (string, error) {
	if requested == "" {
		return appsAdmissionAgentID, nil
	}
	return requested, nil
}
func (r *resourceFenceAgent) ResolveAgent(context.Context, identity.Identity, string) (bool, error) {
	r.calls++
	if r.calls == 2 && r.afterResolve != nil {
		r.afterResolve()
	}
	if r.retired {
		return false, protocol.ErrAgentRetired
	}
	return true, nil
}

type resourceFenceAuthority struct {
	*admissionAuthoritySeam
	afterMint func()
}

func (r *resourceFenceAuthority) Mint(ctx context.Context, tuple admission.RenderTuple) (admission.Token, error) {
	token, err := r.admissionAuthoritySeam.Mint(ctx, tuple)
	if err == nil && r.afterMint != nil {
		r.afterMint()
	}
	return token, err
}

type resourceFenceGate struct {
	*realAdmissionGate
	wrongGeneration bool
}

func (g resourceFenceGate) AuthorizeRender(ctx context.Context, source, uri string) (string, error) {
	generation, err := g.realAdmissionGate.AuthorizeRender(ctx, source, uri)
	if err == nil && g.wrongGeneration {
		return "different-admitted-generation", nil
	}
	return generation, err
}

// The same real registry/accessor/sealed-admission path as the review reproducer.
// Mutations happen after the accessor returned, including after token sealing.
func TestAppResourceReadFence_FinalAdmissionSnapshot(t *testing.T) {
	for _, scenario := range []string{"current", "ordinary_current", "replacement_on_resolve", "same_digest_on_resolve", "ordinary_same_digest", "replacement_on_mint", "rediscovery_on_mint", "withdrawal_on_mint", "withdrawal_on_resolve", "retired_on_mint", "wrong_admitted_generation"} {
		t.Run(scenario, func(t *testing.T) {
			owner := auth.Owner{Tenant: "t-1", User: "u-1", Agent: appsAdmissionAgentID}
			const logical = "resource-app"
			source := mcp.PhysicalServerName(logical, owner)
			reg := mcp.NewRegistry()
			config := &mutableUserConfig{active: true, pair: agentcfg.SignedOAuthMCPPair{ProviderName: "sample", OwnerAgentID: owner.Agent, OwnerUserID: owner.User, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: logical}}}
			provider := &resourceFenceProvider{stubProvider: &stubProvider{id: tools.ToolSourceID(source)}}
			descs := []tools.ToolDescriptor{{Tool: tools.Tool{Name: "original", Source: tools.ToolSourceID(source)}}}
			register := func(registration mcp.ServerRegistration, descs []tools.ToolDescriptor) {
				t.Helper()
				registration.Owner, registration.LogicalName = owner, logical
				registration.Transport, registration.URLOrCommand = string(mcp.TransportStreamableHTTP), "https://provider.example.com/private/mcp"
				if err := reg.Register(context.Background(), registration); err != nil {
					t.Fatal(err)
				}

				if err := reg.RecordDiscovery(source, descs); err != nil {
					t.Fatal(err)
				}
			}
			register(mcp.ServerRegistration{Provider: provider}, descs)
			originalGeneration, _ := reg.CurrentGeneration(source)
			acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{Registry: reg, Catalog: tools.NewCatalog(), Store: newAppsStore(t), Bus: newAppsBus(t), ToolContext: newAppsToolCtx(t), SourceAuthorizer: mcpconsole.NewSourceAuthorizer(reg, config)})
			if err != nil {
				t.Fatal(err)
			}
			agent := new(resourceFenceAgent)
			authority := &resourceFenceAuthority{admissionAuthoritySeam: &admissionAuthoritySeam{auth: newAdmissionAuthority(t)}}
			replace := func(sameDigest bool) {
				if err := reg.Deregister(context.Background(), source, owner); err != nil {
					t.Fatal(err)
				}
				replacement := descs
				if !sameDigest {
					replacement = []tools.ToolDescriptor{{Tool: tools.Tool{Name: "replacement", Source: tools.ToolSourceID(source)}}}
				}
				register(mcp.ServerRegistration{Provider: &stubProvider{id: tools.ToolSourceID(source), resourceBody: []byte("replacement-document")}}, replacement)
				now, _ := reg.CurrentGeneration(source)
				if (now == originalGeneration) != sameDigest {
					t.Fatal("replacement fixture did not establish intended digest equality")
				}
			}
			switch scenario {
			case "replacement_on_resolve":
				agent.afterResolve = func() { replace(false) }
			case "same_digest_on_resolve", "ordinary_same_digest":
				agent.afterResolve = func() { replace(true) }
			case "replacement_on_mint":
				authority.afterMint = func() { replace(false) }
			case "rediscovery_on_mint":
				authority.afterMint = func() {
					if err := reg.RecordDiscovery(source, []tools.ToolDescriptor{{Tool: tools.Tool{Name: "updated", Source: tools.ToolSourceID(source)}}}); err != nil {
						t.Fatal(err)
					}
				}
			case "withdrawal_on_mint":
				authority.afterMint = func() { config.deactivate() }
			case "withdrawal_on_resolve":
				agent.afterResolve = func() { config.deactivate() }
			case "retired_on_mint":
				authority.afterMint = func() { agent.retired = true }
			}
			gate := resourceFenceGate{realAdmissionGate: &realAdmissionGate{acc: acc}, wrongGeneration: scenario == "wrong_admitted_generation"}
			surface, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: acc, Invoker: acc, ToolContext: acc, AgentResolver: agent, AgentReach: allowAdmissionReach{}, RenderAdmissionAuthority: authority, RenderAdmissionGate: gate})
			if err != nil {
				t.Fatal(err)
			}
			ordinary := strings.HasPrefix(scenario, "ordinary_")
			verified := protocolauth.WithAgentReach(admissionVerifiedCtx(t), []string{owner.Agent})
			result, err := surface.Dispatch(verified, methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{Identity: admissionID(), ServerID: source, ResourceURI: "ui://sample/app", RequestRenderAdmission: !ordinary})
			if provider.reads.Load() != 1 {
				t.Fatalf("resource re-read count %d", provider.reads.Load())
			}
			if scenario != "current" && scenario != "ordinary_current" {
				if err == nil || result != nil {
					t.Fatalf("changed snapshot disclosed resource/admission: %+v %v", result, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response := result.(*types.ReadMCPResourceResponse)
			if response.Content != "original-document" || response.ProviderOrigin != "https://provider.example.com" || response.Meta == nil || len(response.Meta.UI.CSP.ResourceDomains) != 1 || response.Meta.UI.CSP.ResourceDomains[0] != "https://assets.example.com" {
				t.Fatalf("current read projection lost: %+v", response)
			}
			if ordinary {
				if response.RenderAdmission != nil {
					t.Fatal("ordinary read minted admission")
				}
				return
			}
			if response.RenderAdmission == nil || response.RenderAdmission.Availability != types.RenderAdmissionAvailable {
				t.Fatal("current read lost admission")
			}
			if _, err := authority.Verify(context.Background(), admission.RenderTuple{Identity: identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "s-1"}, AgentID: owner.Agent, ServerID: source, ResourceURI: "ui://sample/app", DescriptorFingerprint: originalGeneration}, response.RenderAdmission.Token); err != nil {
				t.Fatal(err)
			}
		})
	}
}
