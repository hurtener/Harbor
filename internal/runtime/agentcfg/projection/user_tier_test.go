package projection_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools"
)

// setUserDisables writes a USER-scope (ConfigScopeUser) narrow-only disable
// revision for id via the real registry — the durable user tier 126c projects.
func setUserDisables(t *testing.T, reg agentcfg.Registry, id identity.Quadruple, toolNames []string) {
	t.Helper()
	if _, err := reg.SetRevision(context.Background(), id, projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{DisabledTools: toolNames},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set user disables: %v", err)
	}
}

// TestPlannerCatalog_UserDisableExcludesAdminExposedTool proves the durable
// user-scope disable set alone removes an admin-exposed tool from the run view.
func TestPlannerCatalog_UserDisableExcludesAdminExposedTool(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	setUserDisables(t, reg, projID(), []string{"srvA_alpha"})

	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	if hasName(got, "srvA_alpha") {
		t.Fatalf("user-disabled tool still visible: %v", got)
	}
	if !hasName(got, "srvA_beta") || !hasName(got, "srvB_gamma") || !hasName(got, "local_tool") {
		t.Fatalf("non-disabled tools missing: %v", got)
	}
}

// TestPlannerCatalog_AdminSurvivesEmptyUser proves the admin baseline is
// independent of the user tier — an empty user revision leaves the admin
// exclusion intact.
func TestPlannerCatalog_AdminSurvivesEmptyUser(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srvA"}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	setUserDisables(t, reg, projID(), nil) // empty user revision

	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	if hasName(got, "srvA_alpha") || hasName(got, "srvA_beta") {
		t.Fatalf("admin-paused server resurfaced under an empty user revision: %v", got)
	}
}

// TestPlannerCatalog_UserCannotReEnableAdminDisabled proves the union is
// narrow-only: a user revision can never re-enable an admin-disabled tool
// (there is no enable field, and union never subtracts).
func TestPlannerCatalog_UserCannotReEnableAdminDisabled(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"srvA_alpha"}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	// The user revision disables a DIFFERENT tool; it has no way to express
	// "enable srvA_alpha" (no enable field), so the admin disable stands.
	setUserDisables(t, reg, projID(), []string{"srvB_gamma"})

	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	if hasName(got, "srvA_alpha") {
		t.Fatalf("user tier re-enabled an admin-disabled tool: %v", got)
	}
	if hasName(got, "srvB_gamma") {
		t.Fatalf("user-disabled tool still visible: %v", got)
	}
}

// TestPlannerCatalog_ThreeSetUnion proves all three tiers fold into the one
// grow-only exclusion set: admin pauses srvA, user disables srvB_gamma, the
// session disables local_tool — all three are excluded.
func TestPlannerCatalog_ThreeSetUnion(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	ov := newOverlay(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srvA"}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	setUserDisables(t, reg, projID(), []string{"srvB_gamma"})
	if _, err := ov.SetSourceDisables(ctx, projID(), projAgent, nil, []string{"local_tool"}); err != nil {
		t.Fatalf("set session: %v", err)
	}

	v, err := projection.ActivePlannerCatalogView(ctx, reg, ov, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	for _, excluded := range []string{"srvA_alpha", "srvA_beta", "srvB_gamma", "local_tool"} {
		if hasName(got, excluded) {
			t.Fatalf("three-set union missed %q: %v", excluded, got)
		}
	}
	if len(got) != 0 {
		t.Fatalf("expected all tools excluded by the union, got %v", got)
	}
}

// TestPlannerCatalog_AgentIDNotIsolation proves the user read isolates by
// (tenant, user), NOT by agent_id: two distinct users sharing ONE agent_id get
// independent projections — user A's disable set never reaches user B's run.
func TestPlannerCatalog_AgentIDNotIsolation(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)

	userA := projID()
	userB := identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: "user-B", SessionID: "sess-B"}}

	// user A disables srvA_alpha for the SAME agent.
	setUserDisables(t, reg, userA, []string{"srvA_alpha"})

	// user A's run excludes it.
	va, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, userA, cat, baseFilter())
	if err != nil {
		t.Fatalf("projection A: %v", err)
	}
	if hasName(viewNames(va), "srvA_alpha") {
		t.Fatalf("user A's disable did not apply to user A: %v", viewNames(va))
	}

	// user B (same agent_id) is UNAFFECTED — A's disable never crosses users.
	filterB := tools.CatalogFilter{TenantID: projTenant, UserID: "user-B", SessionID: "sess-B"}
	vb, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, userB, cat, filterB)
	if err != nil {
		t.Fatalf("projection B: %v", err)
	}
	if !hasName(viewNames(vb), "srvA_alpha") {
		t.Fatalf("cross-user bleed: user A's disable reached user B's run (agent_id wrongly used as an isolation filter): %v", viewNames(vb))
	}
}

// TestPlannerCatalog_UserReadError_FailsLoud proves a registry read error on
// the user tier fails the run loudly — never a silent skip to the unfiltered
// view. userScopeErrRegistry (from promptlayers_test.go) errors only on the
// ConfigScopeUser read.
func TestPlannerCatalog_UserReadError_FailsLoud(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	sentinel := errSentinel("user read exploded")
	reg := &userScopeErrRegistry{err: sentinel}
	_, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("user read error must fail the projection loudly, got %v", err)
	}
}
