package projection_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// newOverlay builds a real StateStore-backed session-overlay store for the
// projection composition tests.
func newOverlay(t *testing.T) sessionoverlay.Store {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	ov, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("overlay store: %v", err)
	}
	return ov
}

// TestActivePlannerCatalogView_SessionDisable_NarrowsWithinAdminAllowed
// proves a session disable of an ADMIN-ALLOWED source narrows the view
// (the safe subset's narrow-only enable/disable).
func TestActivePlannerCatalogView_SessionDisable_NarrowsWithinAdminAllowed(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	ov := newOverlay(t)
	// Admin exposes everything (no pause). Session disables srvB.
	if _, err := ov.SetSourceDisables(ctx, projID(), projAgent, []string{"srvB"}, nil); err != nil {
		t.Fatalf("session disable: %v", err)
	}
	v, err := projection.ActivePlannerCatalogView(ctx, reg, ov, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	if hasName(got, "srvB_gamma") {
		t.Fatalf("session-disabled srvB still visible: %v", got)
	}
	if !hasName(got, "srvA_alpha") || !hasName(got, "local_tool") {
		t.Fatalf("non-disabled tools missing: %v", got)
	}
}

// TestActivePlannerCatalogView_NarrowOnly_AdminPauseSurvivesSessionEmpty is
// the #1 invariant: a session edit can NEVER widen. Admin pauses srvA; the
// session sets an EMPTY disable set (the closest a session can get to
// "enable srvA"). The admin pause MUST survive — the union keeps it.
func TestActivePlannerCatalogView_NarrowOnly_AdminPauseSurvivesSessionEmpty(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	ov := newOverlay(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srvA"}},
	}); err != nil {
		t.Fatalf("admin pause: %v", err)
	}
	// Session tries to "un-disable" by disabling something else / nothing of srvA.
	if _, err := ov.SetSourceDisables(ctx, projID(), projAgent, []string{"srvB"}, nil); err != nil {
		t.Fatalf("session disable: %v", err)
	}
	v, err := projection.ActivePlannerCatalogView(ctx, reg, ov, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	// Admin pause of srvA is untouched by the session edit (NARROW-ONLY).
	if hasName(got, "srvA_alpha") || hasName(got, "srvA_beta") {
		t.Fatalf("ADMIN-PAUSED srvA re-enabled by a session edit — narrow-only VIOLATED: %v", got)
	}
	// And the session's own disable of srvB ALSO applied (the union narrows further).
	if hasName(got, "srvB_gamma") {
		t.Fatalf("session disable of srvB did not apply: %v", got)
	}
	if !hasName(got, "local_tool") {
		t.Fatalf("local_tool wrongly hidden: %v", got)
	}
}

// TestActivePlannerCatalogView_NarrowOnly_SessionCannotEnableAdminDisabledTool
// proves a session disabling tools never re-enables an admin-disabled tool —
// the union keeps the admin disable regardless of the session set.
func TestActivePlannerCatalogView_NarrowOnly_SessionCannotEnableAdminDisabledTool(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	ov := newOverlay(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"srvA_alpha"}},
	}); err != nil {
		t.Fatalf("admin disable: %v", err)
	}
	// Session disable set names a DIFFERENT tool (it has no enable verb).
	if _, err := ov.SetSourceDisables(ctx, projID(), projAgent, nil, []string{"srvA_beta"}); err != nil {
		t.Fatalf("session disable: %v", err)
	}
	v, err := projection.ActivePlannerCatalogView(ctx, reg, ov, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	if hasName(got, "srvA_alpha") {
		t.Fatalf("ADMIN-DISABLED srvA_alpha re-enabled by a session edit — narrow-only VIOLATED: %v", got)
	}
	if hasName(got, "srvA_beta") {
		t.Fatalf("session disable of srvA_beta did not apply: %v", got)
	}
}

// TestApplyPromptLayers_SessionUserComposesAboveAdminBase proves the session
// user layer composes ABOVE the admin base (lower-trust position) and that
// the admin base remains the spine — base-unwritable-by-session.
func TestApplyPromptLayers_SessionUserComposesAboveAdminBase(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	ov := newOverlay(t)
	adminBase := "OPERATOR BASE — guardrails"
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: &adminBase},
	}); err != nil {
		t.Fatalf("admin base: %v", err)
	}
	if _, err := ov.SetUserPrompt(ctx, projID(), projAgent, "session refinement"); err != nil {
		t.Fatalf("session user: %v", err)
	}
	got, err := projection.ApplyPromptLayers(ctx, reg, ov, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got == nil || got.BasePromptLayer == nil || *got.BasePromptLayer != adminBase {
		t.Fatalf("admin base not the spine: %+v", got)
	}
	if got.UserPromptLayer == nil || *got.UserPromptLayer != "session refinement" {
		t.Fatalf("session user layer not composed: %+v", got.UserPromptLayer)
	}
}

// TestApplyPromptLayers_AdminAndSessionUserLayersJoin proves the admin user
// layer and the session user layer BOTH land in the lower-trust user block
// (admin first, then session), never overwriting the operator base.
func TestApplyPromptLayers_AdminAndSessionUserLayersJoin(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	ov := newOverlay(t)
	base := "BASE"
	adminUser := "admin user layer"
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: &base, User: &adminUser},
	}); err != nil {
		t.Fatalf("admin layers: %v", err)
	}
	if _, err := ov.SetUserPrompt(ctx, projID(), projAgent, "session user layer"); err != nil {
		t.Fatalf("session user: %v", err)
	}
	got, err := projection.ApplyPromptLayers(ctx, reg, ov, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if *got.BasePromptLayer != "BASE" {
		t.Fatalf("base changed: %q", *got.BasePromptLayer)
	}
	want := "admin user layer\n\nsession user layer"
	if got.UserPromptLayer == nil || *got.UserPromptLayer != want {
		t.Fatalf("user layer join = %q, want %q", deref(got.UserPromptLayer), want)
	}
}

// TestApplyPromptLayers_SessionUserOnly_NoAdminBase proves a session user
// layer alone (no admin config) composes as the user layer with no base — the
// session never gains a base by writing only a user layer.
func TestApplyPromptLayers_SessionUserOnly_NoAdminBase(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	ov := newOverlay(t)
	if _, err := ov.SetUserPrompt(ctx, projID(), projAgent, "only session"); err != nil {
		t.Fatalf("session user: %v", err)
	}
	got, err := projection.ApplyPromptLayers(ctx, reg, ov, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got == nil || got.UserPromptLayer == nil || *got.UserPromptLayer != "only session" {
		t.Fatalf("session user layer missing: %+v", got)
	}
	if got.BasePromptLayer != nil {
		t.Fatalf("session user-only must NOT set a base: %q", *got.BasePromptLayer)
	}
}

// TestActiveSkillViews_PersonalSkillSurvivesAdminMembershipFilter proves a
// session personal skill is added back above the admin membership filter (the
// safe-subset personal-skills capability).
func TestActiveSkillViews_PersonalSkillSurvivesAdminMembershipFilter(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	ov := newOverlay(t)
	// Admin pins membership {a}. Session adds personal skill "p".
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
	}); err != nil {
		t.Fatalf("admin membership: %v", err)
	}
	if _, err := ov.AddPersonalSkill(ctx, projID(), projAgent, "p"); err != nil {
		t.Fatalf("add personal: %v", err)
	}
	got, err := projection.ActiveSkillViews(ctx, reg, ov, projAgent, projID(), views("a", "b", "p"))
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	gn := names(got)
	if !hasName(gn, "a") || !hasName(gn, "p") {
		t.Fatalf("admin member 'a' + personal 'p' expected: %v", gn)
	}
	if hasName(gn, "b") {
		t.Fatalf("non-member 'b' leaked through: %v", gn)
	}
}

// TestActiveSkillViews_DurableUserSkillSurvivesAdminMembershipFilter is the
// run-start consumer of the durable ScopeUser rung (D-345): a durable
// user-scope personal skill (recorded in the caller's ConfigScopeUser
// membership) survives the admin membership filter, exactly like the session's
// ephemeral personal skill. Its BODY already resolved into `views` because the
// SkillStore returns ScopeUser rows across the user's sessions; this asserts
// the projection keeps it visible when the admin pins a membership.
func TestActiveSkillViews_DurableUserSkillSurvivesAdminMembershipFilter(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	ov := newOverlay(t)
	// Admin pins membership {a}. The user has a durable user-scope skill "u"
	// (ConfigScopeUser membership) and a session ephemeral skill "p".
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
	}); err != nil {
		t.Fatalf("admin membership: %v", err)
	}
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"u"}},
	}); err != nil {
		t.Fatalf("durable user membership: %v", err)
	}
	if _, err := ov.AddPersonalSkill(ctx, projID(), projAgent, "p"); err != nil {
		t.Fatalf("add personal: %v", err)
	}
	got, err := projection.ActiveSkillViews(ctx, reg, ov, projAgent, projID(), views("a", "b", "p", "u"))
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	gn := names(got)
	if !hasName(gn, "a") || !hasName(gn, "p") || !hasName(gn, "u") {
		t.Fatalf("admin member 'a' + session personal 'p' + durable user 'u' expected: %v", gn)
	}
	if hasName(gn, "b") {
		t.Fatalf("non-member 'b' leaked through: %v", gn)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
