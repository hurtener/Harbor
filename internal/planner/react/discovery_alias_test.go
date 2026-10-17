package react

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"

	// A minimal real search index keeps this discovery seam independent of runtime assembly.
	"github.com/hurtener/Harbor/internal/tools/drivers/searchcache"
)

type legacyDiscoveryAliasView struct {
	tools.PlannerView
	legacy, canonical string
}

func (v legacyDiscoveryAliasView) Resolve(name string) (tools.Tool, bool) {
	if name == v.legacy {
		name = v.canonical
	}
	return v.PlannerView.Resolve(name)
}

type discoveryAliasClient struct {
	inspect func(llm.CompleteRequest) error
	alias   string
}

func (c discoveryAliasClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if err := c.inspect(req); err != nil {
		return llm.CompleteResponse{}, err
	}
	if c.alias == "" {
		return llm.CompleteResponse{Content: "done"}, nil
	}
	return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "discovered-call", Name: c.alias, Args: json.RawMessage(`{}`)}}}, nil
}
func (discoveryAliasClient) Close(context.Context) error { return nil }

func discoveryAliasCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	index, err := searchcache.New(searchcache.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Error(err)
		}
	})
	cat := tools.NewCatalog(tools.WithSearchCache(index))
	if err := builtin.RegisterWith(builtin.RegistryContext{Catalog: cat}, []string{"tool_search", "tool_get"}); err != nil {
		t.Fatal(err)
	}
	return cat
}
func registerDiscoveryAlias(t *testing.T, cat tools.ToolCatalog, tool tools.Tool, calls *atomic.Int64) {
	t.Helper()
	if err := cat.Register(tools.ToolDescriptor{Tool: tool, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		calls.Add(1)
		return tools.ToolResult{}, fmt.Errorf("discovery must not execute domain handlers")
	}}); err != nil {
		t.Fatal(err)
	}
}
func searchAliasTrajectory(t *testing.T, cat tools.ToolCatalog, q identity.Quadruple, query string) *planner.Trajectory {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	search, ok := cat.Resolve("tool_search")
	if !ok {
		t.Fatal("missing real search")
	}
	args, err := json.Marshal(builtin.ToolSearchArgs{Query: query, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	result, err := search.Invoke(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := json.Marshal(result.Value)
	if err != nil {
		t.Fatal(err)
	}
	return &planner.Trajectory{Steps: []planner.Step{{Action: planner.CallTool{Tool: "tool_search"}, LLMObservation: json.RawMessage(observation)}}}
}
func discoveryAliasIdentity() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "discovery-run"}
}
func discoveryAliasView(cat tools.ToolCatalog, q identity.Quadruple) tools.PlannerView {
	return tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID, LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred}})
}

func TestDiscoveryAliases_SearchHarvestNextDeclaration(t *testing.T) {
	for _, source := range []string{"short", "source-" + strings.Repeat("a", 32)} {
		for _, verb := range []string{"reporting_view", "reporting_authoring_create_v1"} {
			for _, override := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/override=%t", source, verb, override), func(t *testing.T) {
					cat := discoveryAliasCatalog(t)
					q := discoveryAliasIdentity()
					var calls atomic.Int64
					tool := tools.Tool{Name: source + "_" + verb, Description: "aliasregression current schema", ArgsSchema: json.RawMessage(`{"type":"object","properties":{"current":{"type":"boolean"}}}`), Loading: tools.LoadingDeferred, Source: tools.ToolSourceID(source)}
					if override {
						tool.Loading = tools.LoadingAlways
					}
					registerDiscoveryAlias(t, cat, tool, &calls)
					view := tools.NewLoadingOverrideView(discoveryAliasView(cat, q), map[string]tools.LoadingMode{tool.Name: tools.LoadingDeferred}, nil)
					alias := tools.ModelVisibleToolName(tool.Name)
					if _, found := declarationNamed(buildToolDeclarations(planner.RunContext{Catalog: view}, nil), alias); found {
						t.Fatal("tool loaded before discovery")
					}
					tr := searchAliasTrajectory(t, cat, q, "aliasregression")
					names := deriveDiscoveredFromTrajectory(tr)
					if len(names) != 1 || names[0] != alias {
						t.Fatalf("actual search harvest=%v, want %s", names, alias)
					}
					client := discoveryAliasClient{alias: alias, inspect: func(req llm.CompleteRequest) error {
						decl, ok := declarationNamed(req.Tools, alias)
						if !ok {
							return fmt.Errorf("search alias absent from next declaration: %s", alias)
						}
						if decl.Description != tool.Description || !strings.Contains(string(decl.Schema), "current") {
							return fmt.Errorf("current schema missing: %#v", decl)
						}
						return nil
					}}
					decision, err := New(client).Next(t.Context(), planner.RunContext{Quadruple: q, Goal: "inspect discovery", Catalog: view, Trajectory: tr})
					if err != nil {
						t.Fatal(err)
					}
					call, ok := decision.(planner.CallTool)
					if !ok || call.Tool != tool.Name {
						t.Fatalf("dispatch decision=%#v, want canonical %s", decision, tool.Name)
					}
					if calls.Load() != 0 {
						t.Fatal("discovery executed a domain handler")
					}
					// A different run starts with the same immutable catalog and no discovery.
					if _, found := declarationNamed(buildToolDeclarations(planner.RunContext{Catalog: view}, nil), alias); found {
						t.Fatal("discovery leaked into another run")
					}
				})
			}
		}
	}
}

func TestDiscoveryAliases_CurrentVisibilityAndSchema(t *testing.T) {
	for _, mode := range []string{"schema", "scope", "disabled", "paused", "filtered", "removed"} {
		t.Run(mode, func(t *testing.T) {
			cat := discoveryAliasCatalog(t)
			q := discoveryAliasIdentity()
			var calls atomic.Int64
			tool := tools.Tool{Name: "source_" + strings.Repeat("a", 40) + "_read_document", Description: "visibilityregression OLD-SCHEMA", ArgsSchema: json.RawMessage(`{"type":"object"}`), Loading: tools.LoadingDeferred, Source: "source"}
			registerDiscoveryAlias(t, cat, tool, &calls)
			tr := searchAliasTrajectory(t, cat, q, "visibilityregression")
			if len(deriveDiscoveredFromTrajectory(tr)) != 1 {
				t.Fatal("real search did not discover fixture")
			}
			tool.Description = "CURRENT-SCHEMA"
			if mode == "scope" {
				tool.AuthScopes = []string{"private"}
			}
			prior, _ := cat.Resolve(tool.Name)
			prior.Tool = tool
			replacer, ok := cat.(tools.CatalogReplacer)
			if !ok {
				t.Fatal("catalog replacement unavailable")
			}
			if err := replacer.Replace([]tools.ToolDescriptor{prior}); err != nil {
				t.Fatal(err)
			}
			var view tools.PlannerCatalogView = tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID})
			switch mode {
			case "disabled":
				view = tools.NewExclusionView(view, nil, []string{tool.Name})
			case "paused":
				view = tools.NewExclusionView(view, []string{"source"}, nil)
			case "filtered":
				view = tools.NewPlannerView(cat, tools.CatalogFilter{NameRegex: regexp.MustCompile(`^tool_`)})
			case "removed":
				view = tools.NewPlannerView(tools.NewCatalog(), tools.CatalogFilter{})
			}
			alias := tools.ModelVisibleToolName(tool.Name)
			client := discoveryAliasClient{inspect: func(req llm.CompleteRequest) error {
				decl, found := declarationNamed(req.Tools, alias)
				if found != (mode == "schema") {
					return fmt.Errorf("mode=%s declared=%t", mode, found)
				}
				if found && decl.Description != "CURRENT-SCHEMA" {
					return fmt.Errorf("stale schema: %s", decl.Description)
				}
				return nil
			}}
			if _, err := New(client).Next(t.Context(), planner.RunContext{Quadruple: q, Goal: "check current authority", Catalog: view, Trajectory: tr}); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("discovery dispatched a handler")
			}
		})
	}
}

func TestDiscoveryAliases_CollisionOwnerAndCanonicalCompatibility(t *testing.T) {
	cat := discoveryAliasCatalog(t)
	q := discoveryAliasIdentity()
	var calls atomic.Int64
	for _, name := range []string{"clock.now", "clock_now", "_spawn.task"} {
		registerDiscoveryAlias(t, cat, tools.Tool{Name: name, Description: "collisionregression " + name, ArgsSchema: json.RawMessage(`{"type":"object"}`), Loading: tools.LoadingDeferred}, &calls)
	}
	tr := searchAliasTrajectory(t, cat, q, "collisionregression")
	names := deriveDiscoveredFromTrajectory(tr)
	if len(names) != 1 || names[0] != "clock_now" {
		t.Fatalf("search taught a loser/reserved name: %v", names)
	}
	view := tools.NewPlannerView(cat, tools.CatalogFilter{})
	for _, discovered := range [][]string{names, {"clock.now"}, {"clock_now"}, {"clock.now", "clock_now", "_spawn.task", "_spawn_task", "missing"}} {
		rc := planner.RunContext{Catalog: view, DiscoveredTools: discovered}
		projected, p := projectModelTools(rc, discovered)
		owner, ok := p.ResolveDeclared("clock_now")
		if !ok || owner != "clock.now" {
			t.Fatalf("discovery=%v retargeted alias to %q", discovered, owner)
		}
		for _, tool := range projected {
			if tool.Name == "clock_now" || tool.Name == "_spawn.task" {
				t.Fatalf("forbidden collider projected: %s", tool.Name)
			}
		}
	}
	// Retained successful calls carry canonical names, including shortened keys.
	canonical := "source_" + strings.Repeat("z", 50) + "_read"
	registerDiscoveryAlias(t, cat, tools.Tool{Name: canonical, ArgsSchema: json.RawMessage(`{"type":"object"}`), Loading: tools.LoadingDeferred}, &calls)
	retained, err := planner.RetainStep(planner.Step{Action: planner.CallTool{Tool: canonical}, LLMObservation: "done"}, "old-run", 0)
	if err != nil {
		t.Fatal(err)
	}
	discovered := deriveDiscoveredFromTrajectory(&planner.Trajectory{Steps: []planner.Step{retained}})
	if _, found := declarationNamed(buildToolDeclarations(planner.RunContext{Catalog: view}, discovered), tools.ModelVisibleToolName(canonical)); !found {
		t.Fatal("canonical history compatibility lost")
	}
	// The raw provider-safe collider must never steal a shortened alias.
	alias := tools.ModelVisibleToolName(canonical)
	registerDiscoveryAlias(t, cat, tools.Tool{Name: alias, ArgsSchema: json.RawMessage(`{"type":"object"}`), Loading: tools.LoadingDeferred}, &calls)
	_, projectedNames := projectModelTools(planner.RunContext{Catalog: view}, []string{alias})
	if owner, found := projectedNames.ResolveDeclared(alias); !found || owner != canonical {
		t.Fatalf("shortened alias retargeted to raw collider %q", owner)
	}
	// Owner views also admit canonical names written before source namespacing.
	legacyView := legacyDiscoveryAliasView{PlannerView: view, legacy: "legacy_read", canonical: canonical}
	if _, found := declarationNamed(buildToolDeclarations(planner.RunContext{Catalog: legacyView}, []string{"legacy_read"}), alias); !found {
		t.Fatal("legacy canonical translation was rejected")
	}
	if calls.Load() != 0 {
		t.Fatal("discovery invoked domain tools")
	}
}

func TestDiscoveryAliases_ConcurrentRunIsolation(t *testing.T) {
	cat := discoveryAliasCatalog(t)
	var calls atomic.Int64
	const count = 128
	names := make([]string, count)
	for i := range count {
		names[i] = fmt.Sprintf("source_%04d_%s_read", i, strings.Repeat("a", 40))
		registerDiscoveryAlias(t, cat, tools.Tool{Name: names[i], ArgsSchema: json.RawMessage(`{"type":"object"}`), Loading: tools.LoadingDeferred, AuthScopes: []string{fmt.Sprintf("scope-%d", i)}}, &calls)
	}
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			view := tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: fmt.Sprintf("tenant-%d", i), UserID: "user", SessionID: "session", GrantedScopes: []string{fmt.Sprintf("scope-%d", i)}})
			discovered := make([]string, count)
			for j, name := range names {
				discovered[j] = tools.ModelVisibleToolName(name)
			}
			projected, _ := projectModelTools(planner.RunContext{Catalog: view}, discovered)
			found := 0
			for _, tool := range projected {
				if strings.HasPrefix(tool.Name, "source_") {
					found++
					if tool.Name != names[i] {
						t.Errorf("run %d saw %s", i, tool.Name)
					}
				}
			}
			if found != 1 {
				t.Errorf("run %d own declarations=%d", i, found)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 0 {
		t.Fatal("discovery executed a handler")
	}
}

// legacyDeferredAliasCatalog models existing custom schema views whose List
// exposes eager schemas while Resolve also admits canonical deferred keys.
type legacyDeferredAliasCatalog struct {
	eager, deferred []tools.Tool
}

func (v legacyDeferredAliasCatalog) List() []tools.Tool { return append([]tools.Tool(nil), v.eager...) }
func (v legacyDeferredAliasCatalog) Resolve(name string) (tools.Tool, bool) {
	for _, group := range [][]tools.Tool{v.eager, v.deferred} {
		for _, tool := range group {
			if tool.Name == name {
				return tool, true
			}
		}
	}
	return tools.Tool{}, false
}

func TestDiscoveryAliases_LegacyWrappedCanonicalDiscovery(t *testing.T) {
	target := tools.Tool{Name: "source_" + strings.Repeat("a", 40) + "_read_document", Description: "legacy permitted deferred schema", Loading: tools.LoadingDeferred, Source: "legacy-source", ArgsSchema: json.RawMessage(`{"type":"object"}`)}
	winner := tools.Tool{Name: "clock.now", Description: "eager collision owner", ArgsSchema: json.RawMessage(`{"type":"object"}`)}
	loser := tools.Tool{Name: "clock_now", Description: "forbidden raw collider", Loading: tools.LoadingDeferred, ArgsSchema: json.RawMessage(`{"type":"object"}`)}
	legacy := legacyDeferredAliasCatalog{eager: []tools.Tool{winner}, deferred: []tools.Tool{target, loser}}
	emptyExclusion := tools.NewExclusionView(legacy, nil, nil)
	loading := tools.NewLoadingOverrideView(legacy, nil, nil)
	cases := []struct {
		name  string
		view  tools.PlannerCatalogView
		allow bool
	}{
		{"direct", legacy, true},
		{"exclusion", emptyExclusion, true},
		{"loading", loading, true},
		{"nested", tools.NewLoadingOverrideView(emptyExclusion, nil, nil), true},
		{"disabled", tools.NewExclusionView(loading, nil, []string{target.Name}), false},
		{"paused", tools.NewLoadingOverrideView(tools.NewExclusionView(legacy, []string{"legacy-source"}, nil), nil, nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if discovery, ok := tc.view.(interface{ DiscoveryTools() ([]tools.Tool, bool) }); ok {
				if _, complete := discovery.DiscoveryTools(); complete {
					t.Fatal("wrapper claimed the legacy eager List was complete")
				}
			}
			rc := planner.RunContext{Catalog: tc.view}
			declarations := buildToolDeclarations(rc, []string{target.Name, loser.Name})
			_, found := declarationNamed(declarations, tools.ModelVisibleToolName(target.Name))
			if found != tc.allow {
				t.Fatalf("canonical deferred discovery=%t, want %t", found, tc.allow)
			}
			decl, found := declarationNamed(declarations, "clock_now")
			if !found || decl.Description != winner.Description {
				t.Fatalf("raw fallback changed collision owner: %#v", decl)
			}
		})
	}
	// A complete production snapshot still rejects a raw residual-collision loser.
	cat := tools.NewCatalog()
	var calls atomic.Int64
	for _, tool := range []tools.Tool{winner, {Name: "clock/now", Description: "dropped deferred schema", Loading: tools.LoadingDeferred, ArgsSchema: json.RawMessage(`{"type":"object"}`)}} {
		registerDiscoveryAlias(t, cat, tool, &calls)
	}
	complete := tools.NewExclusionView(tools.NewPlannerView(cat, tools.CatalogFilter{}), nil, nil)
	_, projection := projectModelTools(planner.RunContext{Catalog: complete}, []string{"clock/now"})
	if _, found := projection.DeclaredName("clock/now"); found {
		t.Fatal("complete snapshot admitted a raw collision loser")
	}
	if calls.Load() != 0 {
		t.Fatal("canonical discovery invoked a handler")
	}
}
