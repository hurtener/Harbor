package projection_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"sync"
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

// loadingCatalog registers a mix of forms / sources / boot-loading modes for
// the D-281 loading-mode override projection tests:
//   - srvA_tool1: TOOL-form, srvA, boot Always
//   - srvA_tool2: TOOL-form, srvA, boot Deferred
//   - srvA_res1:  RESOURCE-form, srvA, boot Deferred (the Form-scoping case)
//   - srvB_tool1: TOOL-form, srvB, boot Always
//   - local_tool: in-process (no Source), boot Always
func loadingCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	reg := func(tool tools.Tool) {
		t.Helper()
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tool,
			Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", tool.Name, err)
		}
	}
	reg(tools.Tool{Name: "srvA_tool1", Source: "srvA", Transport: tools.TransportMCP, Loading: tools.LoadingAlways})
	reg(tools.Tool{Name: "srvA_tool2", Source: "srvA", Transport: tools.TransportMCP, Loading: tools.LoadingDeferred})
	reg(tools.Tool{Name: "srvA_res1", Source: "srvA", Transport: tools.TransportMCP, Loading: tools.LoadingDeferred, Form: tools.ToolFormResource})
	reg(tools.Tool{Name: "srvB_tool1", Source: "srvB", Transport: tools.TransportMCP, Loading: tools.LoadingAlways})
	reg(tools.Tool{Name: "local_tool", Loading: tools.LoadingAlways})
	return cat
}

func setLoadingExposure(t *testing.T, reg agentcfg.Registry, server, tool map[string]string) {
	t.Helper()
	if _, err := reg.SetRevision(t.Context(), projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{ServerLoadingModes: server, ToolLoadingModes: tool},
	}); err != nil {
		t.Fatalf("set loading exposure: %v", err)
	}
}

// resolveLoading returns the Loading mode Resolve(name) reports, failing the
// test if the name does not resolve.
func resolveLoading(t *testing.T, v tools.PlannerCatalogView, name string) tools.LoadingMode {
	t.Helper()
	tl, ok := v.Resolve(name)
	if !ok {
		t.Fatalf("Resolve(%q) not found", name)
	}
	return tl.Loading
}

// TestActivePlannerCatalogView_LoadingOverride_Precedence pins the FULL D-281
// precedence order — tool_loading_modes > server_loading_modes (tool-form
// only) > boot tools.entries[].loading_mode > driver default — including the
// interaction rows the plan calls out explicitly.
func TestActivePlannerCatalogView_LoadingOverride_Precedence(t *testing.T) {
	cases := []struct {
		name         string
		server, tool map[string]string
		wantListName string // name expected present in the prompt-time List()
		wantAbsent   string // name expected absent from the prompt-time List()
		wantResolve  map[string]tools.LoadingMode
	}{
		{
			name:         "no overrides: boot-effective unchanged",
			wantListName: "srvA_tool1",
			wantAbsent:   "srvA_tool2",
			wantResolve:  map[string]tools.LoadingMode{"srvA_tool1": tools.LoadingAlways, "srvA_tool2": tools.LoadingDeferred},
		},
		{
			name:         "server-level always promotes a boot-deferred TOOL-form descriptor",
			server:       map[string]string{"srvA": "always"},
			wantListName: "srvA_tool2",
			wantResolve:  map[string]tools.LoadingMode{"srvA_tool2": tools.LoadingAlways},
		},
		{
			name:        "server-level always does NOT promote a RESOURCE-form descriptor (Form-scoping)",
			server:      map[string]string{"srvA": "always"},
			wantAbsent:  "srvA_res1",
			wantResolve: map[string]tools.LoadingMode{"srvA_res1": tools.LoadingDeferred},
		},
		{
			name:        "per-tool deferred demotes a boot-always TOOL-form descriptor",
			tool:        map[string]string{"srvA_tool1": "deferred"},
			wantAbsent:  "srvA_tool1",
			wantResolve: map[string]tools.LoadingMode{"srvA_tool1": tools.LoadingDeferred},
		},
		{
			name:        "per-tool wins over a conflicting per-server override",
			server:      map[string]string{"srvA": "always"},
			tool:        map[string]string{"srvA_tool1": "deferred"},
			wantAbsent:  "srvA_tool1",
			wantResolve: map[string]tools.LoadingMode{"srvA_tool1": tools.LoadingDeferred, "srvA_tool2": tools.LoadingAlways},
		},
		{
			name:         "server override on a different source is inert",
			server:       map[string]string{"srvB": "deferred"},
			wantListName: "srvA_tool1",
			wantResolve:  map[string]tools.LoadingMode{"srvB_tool1": tools.LoadingDeferred},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cat := loadingCatalog(t)
			reg := newRegistry(t)
			setLoadingExposure(t, reg, tc.server, tc.tool)
			v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
			if err != nil {
				t.Fatalf("projection: %v", err)
			}
			got := viewNames(v)
			if tc.wantListName != "" && !hasName(got, tc.wantListName) {
				t.Errorf("List() = %v, want %q present", got, tc.wantListName)
			}
			if tc.wantAbsent != "" && hasName(got, tc.wantAbsent) {
				t.Errorf("List() = %v, want %q absent", got, tc.wantAbsent)
			}
			for name, want := range tc.wantResolve {
				if got := resolveLoading(t, v, name); got != want {
					t.Errorf("Resolve(%q).Loading = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// TestActivePlannerCatalogView_LoadingOverride_ToolStaysDiscoverable proves
// the full AC scenario end to end: a deferred override hides the tool from
// List() (the prompt) while Resolve() still returns it (tool_search /
// dispatch stay callable — D-167's two-turn cycle).
func TestActivePlannerCatalogView_LoadingOverride_ToolStaysDiscoverable(t *testing.T) {
	ctx := context.Background()
	cat := loadingCatalog(t)
	reg := newRegistry(t)
	setLoadingExposure(t, reg, nil, map[string]string{"srvA_tool1": "deferred"})
	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if hasName(viewNames(v), "srvA_tool1") {
		t.Fatal("deferred-overridden tool must be absent from the prompt-time List()")
	}
	tl, ok := v.Resolve("srvA_tool1")
	if !ok {
		t.Fatal("deferred-overridden tool must still resolve (discovery-callable)")
	}
	if tl.Loading != tools.LoadingDeferred {
		t.Fatalf("Resolve(srvA_tool1).Loading = %q, want deferred", tl.Loading)
	}
}

// TestActivePlannerCatalogView_LoadingOverride_RemovalRestoresBoot proves
// flip-back is desired-state REMOVAL (a fresh revision with no loading maps),
// not a counter-override — the next projection returns to the boot-effective
// mode.
func TestActivePlannerCatalogView_LoadingOverride_RemovalRestoresBoot(t *testing.T) {
	ctx := context.Background()
	cat := loadingCatalog(t)
	reg := newRegistry(t)
	setLoadingExposure(t, reg, nil, map[string]string{"srvA_tool1": "deferred"})
	setLoadingExposure(t, reg, nil, nil) // desired-state replace: remove the override

	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if !hasName(viewNames(v), "srvA_tool1") {
		t.Fatal("removing the override must restore the boot-effective (always) mode")
	}
}

// TestActivePlannerCatalogView_LoadingOverride_ExclusionStaysStrongerThanDefer
// pins the ExclusionView-vs-LoadingOverrideView split side by side: a
// DISABLED tool is absent from BOTH List and Resolve, while a
// DEFERRED-overridden tool is absent from List but still resolves.
func TestActivePlannerCatalogView_LoadingOverride_ExclusionStaysStrongerThanDefer(t *testing.T) {
	ctx := context.Background()
	cat := loadingCatalog(t)
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{
			DisabledTools:    []string{"srvB_tool1"},
			ToolLoadingModes: map[string]string{"srvA_tool1": "deferred"},
		},
	}); err != nil {
		t.Fatalf("set revision: %v", err)
	}
	v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	got := viewNames(v)
	if hasName(got, "srvA_tool1") {
		t.Error("deferred tool must be absent from List()")
	}
	if hasName(got, "srvB_tool1") {
		t.Error("disabled tool must be absent from List()")
	}
	if _, ok := v.Resolve("srvA_tool1"); !ok {
		t.Error("deferred tool must still resolve (defer removes prompt PRESENCE only)")
	}
	if _, ok := v.Resolve("srvB_tool1"); ok {
		t.Error("disabled tool must NOT resolve (disable removes CAPABILITY)")
	}
}

// TestActivePlannerCatalogView_LoadingOverride_InFlightSnapshotUnaffected
// proves a view built BEFORE an admin flip keeps its resolved snapshot (the
// concurrent-reuse contract, D-025/D-234 item 3: next-turn only).
func TestActivePlannerCatalogView_LoadingOverride_InFlightSnapshotUnaffected(t *testing.T) {
	ctx := context.Background()
	cat := loadingCatalog(t)
	reg := newRegistry(t)

	inFlight, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection (pre-flip): %v", err)
	}
	if hasName(viewNames(inFlight), "srvA_tool2") {
		t.Fatal("precondition: srvA_tool2 boots deferred and should be absent pre-flip")
	}

	// Flip AFTER the in-flight view was built.
	setLoadingExposure(t, reg, map[string]string{"srvA": "always"}, nil)

	// The in-flight view's OWN snapshot must be unaffected by the later flip.
	if hasName(viewNames(inFlight), "srvA_tool2") {
		t.Fatal("in-flight view must keep its resolved snapshot — next-turn only")
	}

	// A FRESH projection call sees the new state.
	next, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
	if err != nil {
		t.Fatalf("projection (post-flip): %v", err)
	}
	if !hasName(viewNames(next), "srvA_tool2") {
		t.Fatal("a fresh projection call must observe the admin flip")
	}
}

// TestActivePlannerCatalogView_LoadingOverride_ConcurrentFlips runs N=100
// concurrent projection calls against ONE shared registry + catalog while a
// separate goroutine repeatedly flips the admin loading-mode overrides —
// asserting every returned view is internally consistent (List() presence
// agrees with Resolve().Loading for the SAME name, never a torn read), the
// goroutine count returns to baseline after teardown (no leak), and the run
// under -race is clean (D-025 concurrent-reuse contract).
func TestActivePlannerCatalogView_LoadingOverride_ConcurrentFlips(t *testing.T) {
	ctx := context.Background()
	cat := loadingCatalog(t)
	reg := newRegistry(t)

	runtime.Gosched()
	baseline := runtime.NumGoroutine()

	stop := make(chan struct{})
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		flip := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			if flip {
				setLoadingExposure(t, reg, map[string]string{"srvA": "always"}, nil)
			} else {
				setLoadingExposure(t, reg, nil, map[string]string{"srvA_tool1": "deferred"})
			}
			flip = !flip
		}
	}()

	const n = 100
	var readersWG sync.WaitGroup
	readersWG.Add(n)
	for range n {
		go func() {
			defer readersWG.Done()
			v, err := projection.ActivePlannerCatalogView(ctx, reg, nil, projAgent, projID(), cat, baseFilter())
			if err != nil {
				t.Errorf("projection: %v", err)
				return
			}
			listed := map[string]tools.LoadingMode{}
			for _, tl := range v.List() {
				listed[tl.Name] = tl.Loading
			}
			// Internal consistency: whatever List() reports for a name must
			// match what Resolve() reports for that SAME name, within this
			// one view instance — never a torn snapshot.
			for name, mode := range listed {
				rtl, ok := v.Resolve(name)
				if !ok {
					t.Errorf("List() included %q but Resolve() did not find it", name)
					continue
				}
				if rtl.Loading != mode {
					t.Errorf("torn snapshot: List()[%q].Loading=%q but Resolve(%q).Loading=%q", name, mode, name, rtl.Loading)
				}
			}
		}()
	}
	readersWG.Wait()
	close(stop)
	writerWG.Wait()

	// Goroutine baseline restored — no leak from the concurrent stress.
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.Gosched()
		if runtime.NumGoroutine() <= baseline+2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, got)
	}
}

// TestEffectiveLoadingMode_NilRegistryOrEmptyAgentID_ReturnsBootUnchanged
// pins the tools.describe byte-compatible "no agent_id" fallback path.
func TestEffectiveLoadingMode_NilRegistryOrEmptyAgentID_ReturnsBootUnchanged(t *testing.T) {
	ctx := context.Background()
	tool := tools.Tool{Name: "srvA_tool1", Source: "srvA"}
	mode, err := projection.EffectiveLoadingMode(ctx, nil, projAgent, projID(), tool, tools.LoadingAlways)
	if err != nil || mode != tools.LoadingAlways {
		t.Fatalf("nil registry: mode=%q err=%v, want (always, nil)", mode, err)
	}
	reg := newRegistry(t)
	mode, err = projection.EffectiveLoadingMode(ctx, reg, "", projID(), tool, tools.LoadingAlways)
	if err != nil || mode != tools.LoadingAlways {
		t.Fatalf("empty agentID: mode=%q err=%v, want (always, nil)", mode, err)
	}
}

// TestEffectiveLoadingMode_MirrorsProjectionPrecedence proves the
// tools.describe read surface applies the SAME precedence the run-start
// projection applies (the one-canonical-implementation invariant, D-281).
func TestEffectiveLoadingMode_MirrorsProjectionPrecedence(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	setLoadingExposure(t, reg,
		map[string]string{"srvA": "always"},
		map[string]string{"srvA_tool1": "deferred"},
	)
	resource := tools.Tool{Name: "srvA_res1", Source: "srvA", Form: tools.ToolFormResource}
	mode, err := projection.EffectiveLoadingMode(ctx, reg, projAgent, projID(), resource, tools.LoadingDeferred)
	if err != nil {
		t.Fatalf("describe resource: %v", err)
	}
	if mode != tools.LoadingDeferred {
		t.Fatalf("server override must not flip a RESOURCE-form descriptor: got %q", mode)
	}
	toolForm := tools.Tool{Name: "srvA_tool2", Source: "srvA"}
	mode, err = projection.EffectiveLoadingMode(ctx, reg, projAgent, projID(), toolForm, tools.LoadingDeferred)
	if err != nil {
		t.Fatalf("describe tool-form: %v", err)
	}
	if mode != tools.LoadingAlways {
		t.Fatalf("server override must promote a TOOL-form descriptor: got %q", mode)
	}
	overridden := tools.Tool{Name: "srvA_tool1", Source: "srvA"}
	mode, err = projection.EffectiveLoadingMode(ctx, reg, projAgent, projID(), overridden, tools.LoadingAlways)
	if err != nil {
		t.Fatalf("describe per-tool override: %v", err)
	}
	if mode != tools.LoadingDeferred {
		t.Fatalf("per-tool override must win over the server override: got %q", mode)
	}
}

// TestLoadingResolverAdapter_SatisfiesEffectiveLoadingMode proves the
// production adapter (wired at the cmd/harbor + devstack boot boundary)
// delegates to EffectiveLoadingMode faithfully.
func TestLoadingResolverAdapter_SatisfiesEffectiveLoadingMode(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	setLoadingExposure(t, reg, nil, map[string]string{"srvA_tool1": "deferred"})
	adapter := projection.LoadingResolverAdapter{Registry: reg}
	mode, err := adapter.EffectiveLoading(ctx, identity.Identity{TenantID: projTenant, UserID: projUser, SessionID: projSess},
		projAgent, tools.Tool{Name: "srvA_tool1", Source: "srvA"}, tools.LoadingAlways)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if mode != tools.LoadingDeferred {
		t.Fatalf("adapter mode = %q, want deferred", mode)
	}
}
