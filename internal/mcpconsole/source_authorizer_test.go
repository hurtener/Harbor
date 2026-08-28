package mcpconsole_test

import (
	"context"
	"errors"
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

// mutableUserConfig is deliberately only the narrow durable read seam. It
// lets this test leave the physical registry entry stale while dropping the
// sole ConfigScopeUser pair that authorizes it.
type mutableUserConfig struct {
	mu     sync.RWMutex
	active bool
	pair   agentcfg.SignedOAuthMCPPair
	pairs  map[string]agentcfg.SignedOAuthMCPPair
}

func (r *mutableUserConfig) Active(_ context.Context, _ identity.Quadruple, agentID string, _ agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.active {
		return agentcfg.Revision{}, false, nil
	}
	pair := r.pair
	if configured, ok := r.pairs[agentID]; ok {
		pair = configured
	}
	pairs := agentcfg.SignedOAuthMCPPairs{pair.ProviderName: pair}
	return agentcfg.Revision{Payload: agentcfg.ConfigPayload{SignedOAuthMCPPairs: &pairs}}, true, nil
}

func (r *mutableUserConfig) setActive(active bool) {
	r.mu.Lock()
	r.active = active
	r.mu.Unlock()
}

// TestSourceAuthorizer_AllNamedSurfacesUseCurrentUserRevision proves that a
// physical source left in the live cache after a durable pair removal cannot
// be enumerated, read, refreshed, probed, inspected, or mutated through any
// named MCP Connections method. The same helper is used by list/get and the
// named control/read paths; owner filtering alone would fail this test.
func TestSourceAuthorizer_AllNamedSurfacesUseCurrentUserRevision(t *testing.T) {
	const logical = "personal-connection"
	owner := auth.Owner{Tenant: "tenant-1", Agent: "agent-1", User: "user-a"}
	foreign := identity.Identity{TenantID: owner.Tenant, UserID: "user-b", SessionID: "session-b"}
	caller := identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "session-a"}
	newSession := identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "session-a-new"}

	reg := mcp.NewRegistry()
	physical := mcp.PhysicalServerName(logical, owner)
	if err := reg.Register(context.Background(), mcp.ServerRegistration{
		Provider:     &stubProvider{id: tools.ToolSourceID(physical)},
		Transport:    "streamable-http",
		LogicalName:  logical,
		Owner:        owner,
		InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("register stale-source fixture: %v", err)
	}

	config := &mutableUserConfig{active: true, pair: agentcfg.SignedOAuthMCPPair{
		ProviderName: "provider",
		OwnerAgentID: owner.Agent,
		OwnerUserID:  owner.User,
		Connection:   agentcfg.SignedOAuthMCPConnectionDescriptor{Name: logical},
	}}
	authorizer := mcpconsole.NewSourceAuthorizer(reg, config)
	accessor, err := mcpconsole.NewRegistryAccessor(reg, mcpconsole.WithSourceAuthorizer(authorizer))
	if err != nil {
		t.Fatalf("NewRegistryAccessor: %v", err)
	}

	callerCtx := func(id identity.Identity) context.Context {
		ctx, err := identity.With(context.Background(), id)
		if err != nil {
			t.Fatalf("identity.With(%+v): %v", id, err)
		}
		return protocolauth.WithAgentReach(ctx, []string{owner.Agent})
	}

	// A new session for the same user still sees the durable pair. Session is
	// audit/run context, never the personal attachment key.
	if visible, err := authorizer.Visible(callerCtx(newSession), tools.ToolSourceID(physical), owner.Agent); err != nil || !visible {
		t.Fatalf("same-user new-session visibility = (%t, %v), want (true, nil)", visible, err)
	}
	if rows, _, err := accessor.ListServers(callerCtx(caller), protocol.MCPListFilter{}); err != nil || len(rows) != 1 || rows[0].Name != physical {
		t.Fatalf("active user list = (%v, %v), want the personal source", rows, err)
	}
	if _, err := accessor.GetServer(callerCtx(caller), physical); err != nil {
		t.Fatalf("active user get: %v", err)
	}

	// Simulate the durable revision dropping the pair while the process-local
	// provider remains registered. Every named surface must now converge on
	// the same not-found answer before it reaches the provider or cache state.
	config.setActive(false)
	tests := []struct {
		name string
		call func() error
	}{
		{"list", func() error {
			rows, _, err := accessor.ListServers(callerCtx(caller), protocol.MCPListFilter{})
			if err != nil {
				return err
			}
			if len(rows) != 0 {
				return errors.New("stale personal source was enumerated")
			}
			return mcp.ErrServerNotFound
		}},
		{"get", func() error { _, err := accessor.GetServer(callerCtx(caller), physical); return err }},
		{"resources", func() error { _, err := accessor.ListResources(callerCtx(caller), physical); return err }},
		{"prompts", func() error { _, err := accessor.ListPrompts(callerCtx(caller), physical); return err }},
		{"refresh", func() error { _, err := accessor.RefreshDiscovery(callerCtx(caller), physical); return err }},
		{"probe", func() error { _, err := accessor.Probe(callerCtx(caller), physical); return err }},
		{"health", func() error { _, err := accessor.Health(callerCtx(caller), physical); return err }},
		{"raw-html-trust", func() error { _, err := accessor.SetRawHTMLTrust(callerCtx(caller), physical, true); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("stale personal source was admitted")
			}
			if !errors.Is(err, protocol.ErrAccessorNotFound) && !errors.Is(err, mcp.ErrServerNotFound) {
				t.Fatalf("stale-source error = %v, want not-found", err)
			}
		})
	}

	// A different user is denied even before the durable pair lookup. The
	// physical source stays in the cache throughout the assertion.
	if visible, err := authorizer.Visible(callerCtx(foreign), tools.ToolSourceID(physical), owner.Agent); err != nil || visible {
		t.Fatalf("foreign visibility = (%t, %v), want (false, nil)", visible, err)
	}
	if rows, _, err := accessor.ListServers(callerCtx(foreign), protocol.MCPListFilter{}); err != nil || len(rows) != 0 {
		t.Fatalf("foreign list = (%v, %v), want no personal source", rows, err)
	}
	if _, err := accessor.GetServer(callerCtx(foreign), physical); err == nil {
		t.Fatal("foreign user read stale personal source")
	}
}

// TestSourceAuthorizer_ReachBoundsPersonalSources proves that a verified
// user's personal ConfigScopeUser revision does not widen the signed agent
// reach. Even when the durable revision names the same logical connection on
// two agents, a token whose reach contains only agent-a cannot enumerate or
// address agent-b's physical source.
func TestSourceAuthorizer_ReachBoundsPersonalSources(t *testing.T) {
	const logical = "shared-connection"
	ownerA := auth.Owner{Tenant: "tenant-1", Agent: "agent-a", User: "user-a"}
	ownerB := auth.Owner{Tenant: "tenant-1", Agent: "agent-b", User: "user-a"}
	id := identity.Identity{TenantID: ownerA.Tenant, UserID: ownerA.User, SessionID: "session-a"}
	reg := mcp.NewRegistry()
	for _, owner := range []auth.Owner{ownerA, ownerB} {
		physical := mcp.PhysicalServerName(logical, owner)
		if err := reg.Register(context.Background(), mcp.ServerRegistration{
			Provider:     &stubProvider{id: tools.ToolSourceID(physical)},
			Transport:    "streamable-http",
			LogicalName:  logical,
			Owner:        owner,
			InitialState: mcp.ServerStateOnline,
		}); err != nil {
			t.Fatalf("register %s: %v", owner.Agent, err)
		}
	}
	config := &mutableUserConfig{
		active: true,
		pairs: map[string]agentcfg.SignedOAuthMCPPair{
			ownerA.Agent: {ProviderName: "provider-a", OwnerAgentID: ownerA.Agent, OwnerUserID: ownerA.User, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: logical}},
			ownerB.Agent: {ProviderName: "provider-b", OwnerAgentID: ownerB.Agent, OwnerUserID: ownerB.User, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: logical}},
		},
	}
	// The default SourceAuthorizer uses the verified reach set from ctx.
	accessor, err := mcpconsole.NewRegistryAccessor(reg, mcpconsole.WithSourceAuthorizer(mcpconsole.NewSourceAuthorizer(reg, config)))
	if err != nil {
		t.Fatalf("NewRegistryAccessor: %v", err)
	}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx = protocolauth.WithAgentReach(ctx, []string{ownerA.Agent})

	physicalA := mcp.PhysicalServerName(logical, ownerA)
	physicalB := mcp.PhysicalServerName(logical, ownerB)
	rows, _, err := accessor.ListServers(ctx, protocol.MCPListFilter{})
	if err != nil {
		t.Fatalf("reach-bounded list: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != physicalA {
		t.Fatalf("reach-bounded list = %v, want only %q", rows, physicalA)
	}
	if _, err := accessor.GetServer(ctx, physicalB); err == nil {
		t.Fatal("agent-b personal source was readable with reach limited to agent-a")
	}
	visibleB, visibleErr := mcpconsole.NewSourceAuthorizer(reg, config).Visible(ctx, tools.ToolSourceID(physicalB), "")
	t.Logf("physical A=%q B=%q direct B visibility=(%t,%v)", physicalA, physicalB, visibleB, visibleErr)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"resources", func() error { _, err := accessor.ListResources(ctx, physicalB); return err }},
		{"prompts", func() error { _, err := accessor.ListPrompts(ctx, physicalB); return err }},
		{"refresh", func() error { _, err := accessor.RefreshDiscovery(ctx, physicalB); return err }},
		{"probe", func() error { _, err := accessor.Probe(ctx, physicalB); return err }},
		{"health", func() error { _, err := accessor.Health(ctx, physicalB); return err }},
		{"raw-html-trust", func() error { _, err := accessor.SetRawHTMLTrust(ctx, physicalB, true); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			t.Logf("%s error=%v", tc.name, err)
			if err == nil {
				t.Fatal("agent-b source was admitted outside the verified reach")
			}
			if !errors.Is(err, protocol.ErrAccessorNotFound) && !errors.Is(err, mcp.ErrServerNotFound) {
				t.Fatalf("reach denial = %v, want not-found", err)
			}
		})
	}
}
