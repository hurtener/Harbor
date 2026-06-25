package projection_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/skills"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

const (
	projTenant = "tenant-proj"
	projUser   = "user-proj"
	projSess   = "sess-proj"
	projAgent  = "agent-proj"
)

func projID() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: projUser, SessionID: projSess}}
}

func newRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return reg
}

func views(names ...string) []skills.SkillView {
	out := make([]skills.SkillView, 0, len(names))
	for _, n := range names {
		out = append(out, skills.SkillView{Name: n})
	}
	return out
}

func names(vs []skills.SkillView) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFilterSkillViewsByMembership_KeepsMembersDropsRest is the pure
// set-intersection contract.
func TestFilterSkillViewsByMembership_KeepsMembersDropsRest(t *testing.T) {
	got := names(projection.FilterSkillViewsByMembership(views("a", "b", "c"), []string{"a", "c", "z"}))
	if !eq(got, []string{"a", "c"}) {
		t.Fatalf("got %v, want [a c]", got)
	}
	// Empty membership keeps nothing (the rollback-to-empty case).
	if got := projection.FilterSkillViewsByMembership(views("a", "b"), nil); len(got) != 0 {
		t.Fatalf("empty membership kept %v, want none", names(got))
	}
}

// TestActiveSkillViews_NilRegistryOrEmptyAgentID_PassThrough proves the
// backward-compatible ungated path: with no registry or no agent id the
// views are returned unchanged.
func TestActiveSkillViews_NilRegistryOrEmptyAgentID_PassThrough(t *testing.T) {
	in := views("a", "b")
	got, err := projection.ActiveSkillViews(context.Background(), nil, nil, projAgent, projID(), in)
	if err != nil {
		t.Fatalf("nil registry: %v", err)
	}
	if !eq(names(got), []string{"a", "b"}) {
		t.Fatalf("nil registry filtered: %v", names(got))
	}
	reg := newRegistry(t)
	got, err = projection.ActiveSkillViews(context.Background(), reg, nil, "", projID(), in)
	if err != nil {
		t.Fatalf("empty agent id: %v", err)
	}
	if !eq(names(got), []string{"a", "b"}) {
		t.Fatalf("empty agent id filtered: %v", names(got))
	}
}

// TestActiveSkillViews_NoActiveRevision_PassThrough proves an agent with no
// config revision sees the unfiltered directory (backward compatible).
func TestActiveSkillViews_NoActiveRevision_PassThrough(t *testing.T) {
	reg := newRegistry(t)
	got, err := projection.ActiveSkillViews(context.Background(), reg, nil, projAgent, projID(), views("a", "b"))
	if err != nil {
		t.Fatalf("no active revision: %v", err)
	}
	if !eq(names(got), []string{"a", "b"}) {
		t.Fatalf("no active revision filtered: %v", names(got))
	}
}

// TestActiveSkillViews_ActiveRevisionFilters proves a real active revision's
// skills membership narrows the directory views — through the REAL registry.
func TestActiveSkillViews_ActiveRevisionFilters(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a", "c"}},
	}); err != nil {
		t.Fatalf("set revision: %v", err)
	}
	got, err := projection.ActiveSkillViews(ctx, reg, nil, projAgent, projID(), views("a", "b", "c"))
	if err != nil {
		t.Fatalf("active filter: %v", err)
	}
	if !eq(names(got), []string{"a", "c"}) {
		t.Fatalf("active revision projection = %v, want [a c]", names(got))
	}
}

// TestActiveSkillViews_AdminPinnedMissingBody_FailsLoud is the wave-end
// regression for the silent-drop drift (audit W2): an admin-pinned membership
// name whose body is absent from the directory view must fail LOUD per the
// 92c plan (never the §13 silent-degradation shape), e.g. a rollback onto a
// since-hard-deleted skill.
func TestActiveSkillViews_AdminPinnedMissingBody_FailsLoud(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a", "ghost"}},
	}); err != nil {
		t.Fatalf("set revision: %v", err)
	}
	// views lacks "ghost" (its body was hard-deleted) → loud, not silent.
	_, err := projection.ActiveSkillViews(ctx, reg, nil, projAgent, projID(), views("a", "b"))
	if !errors.Is(err, projection.ErrSkillBodyMissing) {
		t.Fatalf("err = %v, want ErrSkillBodyMissing", err)
	}
}

// TestActiveSkillViews_PersonalSkillMissingBody_Silent proves the exemption:
// a SESSION-personal name absent from the view is NOT loud (a safe-subset add
// that may legitimately not be in the directory) — only ADMIN-pinned names
// fail loud.
func TestActiveSkillViews_PersonalSkillMissingBody_Silent(t *testing.T) {
	reg := newRegistry(t)
	ov := newOverlay(t)
	ctx := context.Background()
	// Admin pins only "a" (present in views); the session adds a personal
	// skill "p" whose body is NOT in views.
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
	}); err != nil {
		t.Fatalf("set revision: %v", err)
	}
	if _, err := ov.AddPersonalSkill(ctx, projID(), projAgent, "p"); err != nil {
		t.Fatalf("add personal skill: %v", err)
	}
	got, err := projection.ActiveSkillViews(ctx, reg, ov, projAgent, projID(), views("a", "b"))
	if err != nil {
		t.Fatalf("a missing PERSONAL skill must not fail loud: %v", err)
	}
	if !eq(names(got), []string{"a"}) {
		t.Fatalf("projection = %v, want [a] (personal 'p' absent from views is silently kept-if-present)", names(got))
	}
}

// TestActiveSkillViews_RollbackChangesProjection proves the next-turn effect
// of a rollback: rollback repoints the active revision, and the projection
// reflects the rolled-back membership — the §13 consumer behaviour over the
// real registry.
func TestActiveSkillViews_RollbackChangesProjection(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	r1, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
	})
	if err != nil {
		t.Fatalf("set r1: %v", err)
	}
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a", "b", "c"}},
	}); err != nil {
		t.Fatalf("set r2: %v", err)
	}
	// Active now is r2 → projection keeps a,b,c.
	got, err := projection.ActiveSkillViews(ctx, reg, nil, projAgent, projID(), views("a", "b", "c"))
	if err != nil {
		t.Fatalf("active r2: %v", err)
	}
	if !eq(names(got), []string{"a", "b", "c"}) {
		t.Fatalf("r2 projection = %v, want [a b c]", names(got))
	}
	// Roll back to r1 → projection narrows to just a (next-turn effect).
	if _, err := reg.Rollback(ctx, projID(), projAgent, r1.RevisionID, agentcfg.ConfigScopeAgent); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, err = projection.ActiveSkillViews(ctx, reg, nil, projAgent, projID(), views("a", "b", "c"))
	if err != nil {
		t.Fatalf("active after rollback: %v", err)
	}
	if !eq(names(got), []string{"a"}) {
		t.Fatalf("post-rollback projection = %v, want [a]", names(got))
	}
}

// --- tool-exposure projection (ActivePlannerCatalogView) ---

func toolCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	reg := func(name string, source tools.ToolSourceID) {
		t.Helper()
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
			Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	reg("srvA_alpha", "srvA")
	reg("srvA_beta", "srvA")
	reg("srvB_gamma", "srvB")
	if err := cat.Register(tools.ToolDescriptor{
		Tool:   tools.Tool{Name: "local_tool"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil },
	}); err != nil {
		t.Fatalf("register local_tool: %v", err)
	}
	return cat
}

func viewNames(v tools.PlannerCatalogView) []string {
	out := make([]string, 0)
	for _, tl := range v.List() {
		out = append(out, tl.Name)
	}
	return out
}

func hasName(vs []string, want string) bool {
	for _, n := range vs {
		if n == want {
			return true
		}
	}
	return false
}

func baseFilter() tools.CatalogFilter {
	return tools.CatalogFilter{TenantID: projTenant, UserID: projUser, SessionID: projSess}
}

// TestActivePlannerCatalogView_NoConfig_PassThrough proves the
// backward-compatible ungated path: a nil registry, an empty agent id, or no
// active revision returns the unfiltered NewPlannerView.
func TestActivePlannerCatalogView_NoConfig_PassThrough(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	// nil registry
	v, err := projection.ActivePlannerCatalogView(ctx, nil, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("nil registry: %v", err)
	}
	if len(v.List()) != 4 {
		t.Fatalf("nil registry filtered the catalog: %v", viewNames(v))
	}
	// registry present, no active revision
	reg := newRegistry(t)
	v, err = projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("no active revision: %v", err)
	}
	if len(v.List()) != 4 {
		t.Fatalf("no active revision filtered the catalog: %v", viewNames(v))
	}
}

// TestActivePlannerCatalogView_PausedServerExcluded proves a paused server's
// tools are absent from the run's view while other tools remain — through the
// REAL registry + REAL catalog.
func TestActivePlannerCatalogView_PausedServerExcluded(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srvA"}},
	}); err != nil {
		t.Fatalf("set revision: %v", err)
	}
	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	if hasName(got, "srvA_alpha") || hasName(got, "srvA_beta") {
		t.Fatalf("paused server srvA tools still visible: %v", got)
	}
	if !hasName(got, "srvB_gamma") || !hasName(got, "local_tool") {
		t.Fatalf("non-paused tools missing: %v", got)
	}
	if _, ok := v.Resolve("srvA_alpha"); ok {
		t.Fatal("Resolve(srvA_alpha) succeeded against a paused server")
	}
}

// TestActivePlannerCatalogView_DisabledToolExcluded proves an individually
// disabled tool is excluded while siblings remain.
func TestActivePlannerCatalogView_DisabledToolExcluded(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"srvA_alpha"}},
	}); err != nil {
		t.Fatalf("set revision: %v", err)
	}
	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	if hasName(got, "srvA_alpha") {
		t.Fatalf("disabled tool still visible: %v", got)
	}
	if !hasName(got, "srvA_beta") {
		t.Fatalf("sibling srvA_beta wrongly hidden: %v", got)
	}
}

// TestActivePlannerCatalogView_ResumeRestores proves resume (a new revision
// clearing the paused set) restores the tools next-turn — no re-dial.
func TestActivePlannerCatalogView_ResumeRestores(t *testing.T) {
	ctx := context.Background()
	cat := toolCatalog(t)
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srvA"}},
	}); err != nil {
		t.Fatalf("set paused: %v", err)
	}
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{}, // resume all
	}); err != nil {
		t.Fatalf("set resumed: %v", err)
	}
	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if len(v.List()) != 4 {
		t.Fatalf("resume did not restore tools: %v", viewNames(v))
	}
}
