package mcpconsole_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// The reader asserts the durable slot used by agent-owned source admission.
type agentScopeReader struct{ active bool }

func (r agentScopeReader) Active(_ context.Context, q identity.Quadruple, agent string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	if scope != agentcfg.ConfigScopeAgent || q.TenantID != "tenant-a" || agent != "base" {
		return agentcfg.Revision{}, false, fmt.Errorf("wrong durable slot: %s %s %v", q.TenantID, agent, scope)
	}
	pairs := agentcfg.SignedOAuthMCPPairs{"provider": {ProviderName: "provider", OwnerAgentID: "base", Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: "same"}}}
	return agentcfg.Revision{Payload: agentcfg.ConfigPayload{SignedOAuthMCPPairs: &pairs}}, r.active, nil
}

func TestSourceAuthorizer_AgentScopeConcurrentIsolationAndRevocation(t *testing.T) {
	reg := mcp.NewRegistry()
	owner := auth.Owner{Tenant: "tenant-a", Agent: "base"}
	source := tools.ToolSourceID(mcp.PhysicalServerName("same", owner))
	for _, entry := range []struct {
		source  tools.ToolSourceID
		owner   auth.Owner
		logical string
	}{{source, owner, "same"}, {"boot", auth.Owner{}, "boot"}} {
		if err := reg.Register(context.Background(), mcp.ServerRegistration{Transport: "streamable-http", Provider: &stubProvider{id: entry.source}, Owner: entry.owner, LogicalName: entry.logical}); err != nil {
			t.Fatal(err)
		}
	}
	authorizer := mcpconsole.NewSourceAuthorizer(reg, agentScopeReader{active: true})
	accessor, err := mcpconsole.NewRegistryAccessor(reg, mcpconsole.WithSourceAuthorizer(authorizer))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := "tenant-a"
			if i%2 != 0 {
				tenant = "tenant-b"
			}
			ctx, e := identity.With(context.Background(), identity.Identity{TenantID: tenant, UserID: fmt.Sprintf("user-%d", i), SessionID: fmt.Sprintf("session-%d", i)})
			if e != nil {
				t.Error(e)
				return
			}
			ctx = protocolauth.WithAgentReach(ctx, []string{"base", "derived"})
			visible, e := authorizer.Visible(ctx, source, "base")
			if e != nil || visible != (tenant == owner.Tenant) {
				t.Errorf("source scope: %v %v", visible, e)
			}
			if visible, e := authorizer.Visible(ctx, source, "derived"); e != nil || visible {
				t.Errorf("derived leaked base: %v %v", visible, e)
			}
			rows, _, e := accessor.ListServers(ctx, protocol.MCPListFilter{})
			want := 1
			if tenant == owner.Tenant {
				want = 2
			}
			if e != nil || len(rows) != want {
				t.Errorf("catalog count: %d want %d err %v", len(rows), want, e)
			}
			revoked := mcpconsole.NewSourceAuthorizer(reg, agentScopeReader{})
			if visible, e := revoked.Visible(ctx, source, "base"); e != nil || visible {
				t.Errorf("revoked source: %v %v", visible, e)
			}
		}(i)
	}
	wg.Wait()
}
