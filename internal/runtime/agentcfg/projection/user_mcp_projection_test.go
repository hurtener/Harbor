package projection_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

type testSourceOwners map[tools.ToolSourceID]toolauth.Owner

func (m testSourceOwners) OwnerOfSource(source tools.ToolSourceID) (toolauth.Owner, bool) {
	owner, ok := m[source]
	return owner, ok
}

type testPhysicalSourceResolver struct {
	owners  map[tools.ToolSourceID]toolauth.Owner
	logical map[tools.ToolSourceID]string
}

func (r testPhysicalSourceResolver) OwnerOfSource(source tools.ToolSourceID) (toolauth.Owner, bool) {
	owner, ok := r.owners[source]
	return owner, ok
}

func (r testPhysicalSourceResolver) LogicalNameOfSource(source tools.ToolSourceID) (string, bool) {
	logical, ok := r.logical[source]
	return logical, ok
}

func TestActivePlannerCatalogView_UserMCPDesiredPairsAreIdentityScoped(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	idA := identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: "user-a", SessionID: "session-a"}}
	idB := identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: "user-b", SessionID: "session-b"}}

	for _, tc := range []struct {
		id       identity.Quadruple
		user     string
		provider string
		source   string
	}{
		{id: idA, user: "user-a", provider: "provider-a", source: "srvA"},
		{id: idB, user: "user-b", provider: "provider-b", source: "srvB"},
	} {
		pair := &agentcfg.SignedOAuthMCPPair{
			ProviderName:           tc.provider,
			Broker:                 "broker",
			Audience:               "audience",
			Scopes:                 []string{"read"},
			CapabilityRevision:     "capability",
			URLDigest:              agentcfg.OAuthMCPURLDigest("https://example.test/mcp"),
			Sink:                   "https://example.test",
			SinkDigest:             agentcfg.OAuthMCPURLDigest("https://example.test"),
			Connection:             agentcfg.SignedOAuthMCPConnectionDescriptor{Name: tc.source, URL: "https://example.test/mcp"},
			AuthorityOperationKind: "operation-" + tc.user,
			OwnerAgentID:           projAgent,
			OwnerUserID:            tc.user,
			OwnerSessionID:         tc.id.SessionID,
		}
		payload := agentcfg.ConfigPayload{SignedOAuthMCPPair: pair}
		if _, err := reg.SetRevision(agentcfg.WithSignedOAuthMCPFenceOperation(ctx, pair.AuthorityOperationKind), tc.id, projAgent, agentcfg.ConfigScopeUser, payload, agentcfg.SetOptions{}); err != nil {
			t.Fatalf("set %s user pair: %v", tc.user, err)
		}
	}

	owners := testSourceOwners{
		"srvA": {Tenant: projTenant, Agent: projAgent, User: "user-a"},
		"srvB": {Tenant: projTenant, Agent: projAgent, User: "user-b"},
	}
	for _, tc := range []struct {
		name string
		id   identity.Quadruple
		want string
		hide string
	}{
		{name: "user A", id: idA, want: "srvA_alpha", hide: "srvB_gamma"},
		{name: "user B", id: idB, want: "srvB_gamma", hide: "srvA_alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, tc.id, cat, tools.CatalogFilter{
				TenantID: tc.id.TenantID, UserID: tc.id.UserID, SessionID: tc.id.SessionID,
			}, owners)
			if err != nil {
				t.Fatalf("projection: %v", err)
			}
			if _, ok := view.Resolve(tc.want); !ok {
				t.Fatalf("desired source tool %q is hidden: %v", tc.want, viewNames(view))
			}
			if _, ok := view.Resolve(tc.hide); ok {
				t.Fatalf("foreign user source tool %q is visible: %v", tc.hide, viewNames(view))
			}
			if !hasName(viewNames(view), "local_tool") {
				t.Fatalf("operator/boot tool disappeared from user view: %v", viewNames(view))
			}
		})
	}
}

func TestActivePlannerCatalogView_UserExposureUsesLogicalNamesForPhysicalSources(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	cat := tools.NewCatalog()
	const (
		logical   = "shared"
		physicalA = "shared~u-a"
		physicalB = "shared~u-b"
	)
	for _, source := range []string{physicalA, physicalB} {
		for _, suffix := range []string{"_echo", "_beta"} {
			name := source + suffix
			if err := cat.Register(tools.ToolDescriptor{
				Tool: tools.Tool{Name: name, Source: tools.ToolSourceID(source)},
				Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
					return tools.ToolResult{Value: "ok"}, nil
				},
			}); err != nil {
				t.Fatalf("register %s: %v", name, err)
			}
		}
	}
	idA := identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: "user-a", SessionID: "session-a"}}
	idB := identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: "user-b", SessionID: "session-b"}}
	for _, tc := range []struct {
		id        identity.Quadruple
		user      string
		session   string
		operation string
		exposure  *agentcfg.ToolExposure
	}{
		{id: idA, user: "user-a", session: "session-a", operation: "op-a", exposure: &agentcfg.ToolExposure{DisabledTools: []string{logical + "_echo"}}},
		{id: idB, user: "user-b", session: "session-b", operation: "op-b"},
	} {
		pair := &agentcfg.SignedOAuthMCPPair{
			ProviderName: "provider", Broker: "broker", Audience: "audience", Scopes: []string{"read"},
			CapabilityRevision: "capability", URLDigest: agentcfg.OAuthMCPURLDigest("https://example.test/mcp"),
			Sink: "https://example.test", SinkDigest: agentcfg.OAuthMCPURLDigest("https://example.test"),
			Connection:             agentcfg.SignedOAuthMCPConnectionDescriptor{Name: logical, URL: "https://example.test/mcp"},
			AuthorityOperationKind: tc.operation, OwnerAgentID: projAgent, OwnerUserID: tc.user, OwnerSessionID: tc.session,
		}
		payload := agentcfg.ConfigPayload{SignedOAuthMCPPair: pair, ToolExposure: tc.exposure}
		if _, err := reg.SetRevision(agentcfg.WithSignedOAuthMCPFenceOperation(ctx, tc.operation), tc.id, projAgent, agentcfg.ConfigScopeUser, payload, agentcfg.SetOptions{}); err != nil {
			t.Fatalf("set %s user pair: %v", tc.user, err)
		}
	}
	resolver := testPhysicalSourceResolver{
		owners: map[tools.ToolSourceID]toolauth.Owner{
			physicalA: {Tenant: projTenant, Agent: projAgent, User: idA.UserID},
			physicalB: {Tenant: projTenant, Agent: projAgent, User: idB.UserID},
		},
		logical: map[tools.ToolSourceID]string{physicalA: logical, physicalB: logical},
	}
	viewA, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, idA, cat, tools.CatalogFilter{TenantID: projTenant, UserID: idA.UserID, SessionID: idA.SessionID}, resolver)
	if err != nil {
		t.Fatalf("projection A: %v", err)
	}
	if _, ok := viewA.Resolve(physicalA + "_echo"); ok {
		t.Fatal("user A logical disable did not hide its physical echo tool")
	}
	if _, ok := viewA.Resolve(physicalA + "_beta"); !ok {
		t.Fatal("user A logical disable hid its sibling tool")
	}
	if _, ok := viewA.Resolve(physicalB + "_beta"); ok {
		t.Fatal("user A saw user B's physical source")
	}
	viewB, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, idB, cat, tools.CatalogFilter{TenantID: projTenant, UserID: idB.UserID, SessionID: idB.SessionID}, resolver)
	if err != nil {
		t.Fatalf("projection B: %v", err)
	}
	if _, ok := viewB.Resolve(physicalB + "_echo"); !ok {
		t.Fatal("user B's source was hidden by user A's logical policy")
	}
	if _, ok := viewB.Resolve(physicalA + "_beta"); ok {
		t.Fatal("user B saw user A's physical source")
	}
}
