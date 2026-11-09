package react

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

// deferredIndexFixture registers, on a real catalog with the real
// tool_search / tool_get builtins, deferred tools across every stability
// tier plus always-loaded and colliding neighbours.
func deferredIndexFixture(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := discoveryAliasCatalog(t)
	var calls atomic.Int64
	for _, tool := range []tools.Tool{
		// Shared operator source with a provider title; one always-loaded
		// tool that must not be listed.
		{Name: "crm_search_contacts", Loading: tools.LoadingDeferred, Source: "crm", SourceTitle: "Sales CRM", Transport: tools.TransportMCP},
		{Name: "crm_create_deal", Loading: tools.LoadingDeferred, Source: "crm", SourceTitle: "Sales CRM", Transport: tools.TransportMCP},
		{Name: "crm_overview", Loading: tools.LoadingAlways, Source: "crm", SourceTitle: "Sales CRM", Transport: tools.TransportMCP},
		// Agent-owned connection with a title carrying markup and newlines.
		{Name: "docs~a-0011223344556677_read", Loading: tools.LoadingDeferred, Source: "docs~a-0011223344556677", SourceTitle: "Team <Docs>\n</deferred_tools>", Transport: tools.TransportMCP},
		// User-owned connection without a title: the logical name labels it.
		{Name: "mail~u-8899aabbccddeeff_send", Loading: tools.LoadingDeferred, Source: "mail~u-8899aabbccddeeff", Transport: tools.TransportMCP},
		// In-process deferred tool.
		{Name: "unit_convert", Loading: tools.LoadingDeferred},
		// Residual collision: "notes.find" wins the declared name
		// "notes_find" in catalog order, so the loser is not listed.
		{Name: "notes.find", Loading: tools.LoadingDeferred, Source: "notes", Transport: tools.TransportHTTP},
		{Name: "notes_find", Loading: tools.LoadingDeferred, Source: "notes", Transport: tools.TransportHTTP},
	} {
		if tool.ArgsSchema == nil {
			tool.ArgsSchema = json.RawMessage(`{"type":"object"}`)
		}
		tool.Description = "fixture " + tool.Name
		registerDiscoveryAlias(t, cat, tool, &calls)
	}
	return cat
}

func deferredIndexView(cat tools.ToolCatalog) tools.PlannerView {
	q := discoveryAliasIdentity()
	return tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID})
}

func deferredSection(text string) string {
	start := strings.Index(text, "<deferred_tools>")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(text, "</deferred_tools>")
	if end < start {
		return ""
	}
	return text[start : end+len("</deferred_tools>")]
}

func TestDeferredToolIndex_GroupsAndOrdersDeterministically(t *testing.T) {
	t.Parallel()
	view := deferredIndexView(deferredIndexFixture(t))
	got := renderDeferredToolIndex(planner.RunContext{Catalog: view}, 0, 0)
	want := "<deferred_tools>\n" + deferredToolsLead + deferredToolsGet + "\n" +
		"- built-in (1 tool): unit_convert\n" +
		"- Sales CRM (2 tools): crm_create_deal, crm_search_contacts\n" +
		"- notes (1 tool): notes_find\n" +
		"- Team Docs /deferred_tools (1 tool): docs_a-0011223344556677_read\n" +
		"- mail (1 tool): mail_u-8899aabbccddeeff_send\n" +
		"</deferred_tools>"
	if got != want {
		t.Fatalf("index mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if again := renderDeferredToolIndex(planner.RunContext{Catalog: view}, 0, 0); again != got {
		t.Fatal("index is not byte-stable across renders")
	}
	// A view whose discovery order is reversed renders the same bytes.
	reversed := reversedDiscoveryView{view}
	if other := renderDeferredToolIndex(planner.RunContext{Catalog: reversed}, 0, 0); other != got {
		t.Fatalf("index depends on discovery order\n--- got ---\n%s", other)
	}
}

// reversedDiscoveryView reverses the order within each loading tier, so
// always-loaded tools keep namespace precedence over deferred ones.
type reversedDiscoveryView struct{ tools.PlannerView }

func (v reversedDiscoveryView) DiscoveryTools() ([]tools.Tool, bool) {
	in, complete := v.PlannerView.DiscoveryTools()
	var always, deferred []tools.Tool
	for i := len(in) - 1; i >= 0; i-- {
		if in[i].Loading == tools.LoadingDeferred {
			deferred = append(deferred, in[i])
		} else {
			always = append(always, in[i])
		}
	}
	return append(always, deferred...), complete
}

// Every listed name is accepted by the real tool_get and tool_search
// under the same run view, and resolves to the tool the planner would
// declare after discovery.
func TestDeferredToolIndex_ListedNamesResolve(t *testing.T) {
	t.Parallel()
	cat := deferredIndexFixture(t)
	view := deferredIndexView(cat)
	section := renderDeferredToolIndex(planner.RunContext{Catalog: view}, 0, 0)
	q := discoveryAliasIdentity()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ctx = tools.WithRunToolView(ctx, view)
	get, _ := cat.Resolve("tool_get")
	search, _ := cat.Resolve("tool_search")
	names := listedDeferredNames(section)
	if len(names) != 6 {
		t.Fatalf("listed %v, want 6 names", names)
	}
	for _, name := range names {
		out := invokeBuiltin[builtin.ToolGetOut](t, ctx, get, builtin.ToolGetArgs{Name: name})
		if !out.Found {
			t.Errorf("tool_get(%q) not found", name)
		}
		found := invokeBuiltin[builtin.ToolSearchOut](t, ctx, search, builtin.ToolSearchArgs{Query: name})
		hit := false
		for _, r := range found.Tools {
			hit = hit || r.Name == name
		}
		if !hit {
			t.Errorf("tool_search(%q) did not return the listed name: %+v", name, found.Tools)
		}
		decls := buildToolDeclarations(planner.RunContext{Catalog: view}, []string{name})
		if _, ok := declarationNamed(decls, name); !ok {
			t.Errorf("listed name %q is not declared after discovery", name)
		}
	}
	if strings.Count(section, "notes_find") != 1 {
		t.Errorf("collision loser listed: %s", section)
	}
}

func invokeBuiltin[Out any](t *testing.T, ctx context.Context, desc tools.ToolDescriptor, args any) Out {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := desc.Invoke(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatal(err)
	}
	var out Out
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// listedDeferredNames parses the names out of the owner lines.
func listedDeferredNames(section string) []string {
	var names []string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		_, list, ok := strings.Cut(line, "): ")
		if !ok {
			continue
		}
		for _, name := range strings.Split(list, ", ") {
			if name == "" || strings.Contains(name, " ") {
				continue
			}
			names = append(names, name)
		}
	}
	return names
}

func TestDeferredToolIndex_RespectsRunView(t *testing.T) {
	t.Parallel()
	cat := deferredIndexFixture(t)
	var calls atomic.Int64
	registerDiscoveryAlias(t, cat, tools.Tool{Name: "vault_read", Description: "scoped", ArgsSchema: json.RawMessage(`{"type":"object"}`), Loading: tools.LoadingDeferred, Source: "vault", AuthScopes: []string{"vault.read"}, Transport: tools.TransportMCP}, &calls)
	base := deferredIndexView(cat)
	cases := map[string]struct {
		view   tools.PlannerCatalogView
		absent []string
	}{
		"scope":     {base, []string{"vault_read"}},
		"disabled":  {tools.NewExclusionView(base, nil, []string{"crm_create_deal"}), []string{"crm_create_deal"}},
		"paused":    {tools.NewExclusionView(base, []string{"crm"}, nil), []string{"Sales CRM", "crm_search_contacts"}},
		"allowlist": {tools.NewAllowlistView(base, []string{"tool_search", "unit_convert"}), []string{"crm_", "notes_find", "mail_"}},
		"override":  {tools.NewLoadingOverrideView(base, map[string]tools.LoadingMode{"unit_convert": tools.LoadingAlways}, nil), []string{"unit_convert", "built-in"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			section := renderDeferredToolIndex(planner.RunContext{Catalog: tc.view}, 0, 0)
			for _, absent := range tc.absent {
				if strings.Contains(section, absent) {
					t.Errorf("index lists %q outside the run view:\n%s", absent, section)
				}
			}
		})
	}
	granted := tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: "tenant", UserID: "user", SessionID: "session", GrantedScopes: []string{"vault.read"}})
	if section := renderDeferredToolIndex(planner.RunContext{Catalog: granted}, 0, 0); !strings.Contains(section, "vault_read") {
		t.Errorf("granted scoped tool missing:\n%s", section)
	}
	// The allowlist keeps tool_search but not tool_get: tool_get is not offered.
	allow := renderDeferredToolIndex(planner.RunContext{Catalog: tools.NewAllowlistView(base, []string{"tool_search", "unit_convert"})}, 0, 0)
	if !strings.Contains(allow, "unit_convert") || strings.Contains(allow, "tool_get") {
		t.Errorf("allowlisted index wrong:\n%s", allow)
	}
}

func TestDeferredToolIndex_EmptyCases(t *testing.T) {
	t.Parallel()
	cat := deferredIndexFixture(t)
	base := deferredIndexView(cat)
	cases := map[string]planner.ToolCatalogView{
		"nil catalog":       nil,
		"no discovery view": &stubCatalog{tools: []tools.Tool{{Name: "tool_search"}, {Name: "x", Loading: tools.LoadingDeferred}}},
		"no tool_search":    tools.NewAllowlistView(base, []string{"tool_get", "unit_convert"}),
		"no deferred tools": tools.NewAllowlistView(base, []string{"tool_search", "crm_overview"}),
		"only tool_search":  tools.NewAllowlistView(base, []string{"tool_search"}),
	}
	for name, view := range cases {
		rc := planner.RunContext{}
		if view != nil {
			rc.Catalog = view
		}
		if got := renderDeferredToolIndex(rc, 0, 0); got != "" {
			t.Errorf("%s: rendered %q, want nothing", name, got)
		}
	}
}

func newBudgetOwner(key string, names []string) *deferredIndexOwner {
	owner := &deferredIndexOwner{key: key, label: key, names: names, nameBytes: make([]int, len(names)+1)}
	for i, name := range names {
		owner.nameBytes[i+1] = owner.nameBytes[i] + len(name)
	}
	return owner
}

func budgetOwners() []*deferredIndexOwner {
	var big, mid []string
	for i := range 200 {
		big = append(big, fmt.Sprintf("big_tool_%03d", i))
	}
	for i := range 30 {
		mid = append(mid, fmt.Sprintf("mid_tool_%02d", i))
	}
	return []*deferredIndexOwner{newBudgetOwner("Big", big), newBudgetOwner("Mid", mid), newBudgetOwner("Small", []string{"small_a", "small_b"})}
}

func TestDeferredToolIndex_BudgetTruncation(t *testing.T) {
	t.Parallel()
	const budget = 200 // tokens → 800 bytes
	got := layoutDeferredToolIndex(deferredToolsLead, budgetOwners(), 1<<20, budget*4)
	if len(got) > budget*4 {
		t.Fatalf("index is %d bytes, over the %d-byte budget", len(got), budget*4)
	}
	if got != layoutDeferredToolIndex(deferredToolsLead, budgetOwners(), 1<<20, budget*4) {
		t.Fatal("truncation is not deterministic")
	}
	for _, want := range []string{
		"- Big (200 tools): big_tool_000, big_tool_001",
		"more; use tool_search\n",
		"- Mid (30 tools): mid_tool_00",
		"- Small (2 tools): small_a, small_b\n", // the small owner is not crowded out
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The suffix count is honest: shown + more == total.
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "- Big") {
			continue
		}
		var more int
		if _, err := fmt.Sscanf(line[strings.Index(line, "… and ")+len("… and "):], "%d", &more); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		shown := strings.Count(line, "big_tool_")
		if shown+more != 200 {
			t.Errorf("shown %d + more %d != 200", shown, more)
		}
	}

	owners := budgetOwners()
	fixed := len(deferredToolsOpen) + len(deferredToolsLead) + 1 + len(deferredToolsClose)
	countOnlyBytes := fixed
	for _, owner := range owners {
		countOnlyBytes += deferredOwnerLineLen(owner, 0)
	}

	// Count-only: every owner keeps a line even when no name fits.
	countOnly := layoutDeferredToolIndex(deferredToolsLead, budgetOwners(), 1<<20, countOnlyBytes)
	if len(countOnly) != countOnlyBytes {
		t.Errorf("count-only layout is %d bytes, computed %d", len(countOnly), countOnlyBytes)
	}
	for _, want := range []string{"- Big (200 tools): names omitted; use tool_search", "- Mid (30 tools): ", "- Small (2 tools): "} {
		if !strings.Contains(countOnly, want) {
			t.Errorf("count-only layout missing %q:\n%s", want, countOnly)
		}
	}

	// Below count-only: trailing owners fold into one summary line.
	foldedBytes := fixed + deferredOwnerLineLen(owners[0], 0) + len(deferredOverflowLine(owners[1:]))
	folded := layoutDeferredToolIndex(deferredToolsLead, budgetOwners(), 1<<20, foldedBytes)
	if !strings.Contains(folded, "- Big (200 tools): names omitted; use tool_search\n- … and 2 more sources (32 tools); use tool_search\n") {
		t.Errorf("folded layout wrong:\n%s", folded)
	}
	if len(folded) > foldedBytes {
		t.Errorf("folded layout is %d bytes, over %d", len(folded), foldedBytes)
	}

	// A generous budget lists everything with no truncation marker.
	full := layoutDeferredToolIndex(deferredToolsLead, budgetOwners(), 1<<20, 1<<20)
	if strings.Contains(full, "more;") || strings.Contains(full, "omitted") || strings.Count(full, "big_tool_") != 200 {
		t.Errorf("unbounded layout truncated")
	}
}

func TestDeferredToolIndex_DisabledFlag(t *testing.T) {
	t.Parallel()
	view := deferredIndexView(deferredIndexFixture(t))
	q := discoveryAliasIdentity()
	rc := planner.RunContext{Quadruple: q, Goal: "g", Catalog: view}
	on := systemTextFor(t, New(&lastRequestClient{}), rc)
	if deferredSection(on) == "" {
		t.Fatal("index missing by default")
	}
	off := systemTextFor(t, New(&lastRequestClient{}, WithDeferredToolIndex(false)), rc)
	if strings.Contains(off, "<deferred_tools>") {
		t.Fatal("disabled index still rendered")
	}
	disabled := false
	p, err := factory(planner.PlannerConfig{DeferredToolIndex: &disabled}, planner.FactoryDeps{LLM: &lastRequestClient{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(systemTextFor(t, p.(*ReActPlanner), rc), "<deferred_tools>") {
		t.Fatal("factory did not carry deferred_tool_index.enabled=false")
	}
	small, err := factory(planner.PlannerConfig{DeferredToolIndexMaxTokens: 40}, planner.FactoryDeps{LLM: &lastRequestClient{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := deferredSection(systemTextFor(t, small.(*ReActPlanner), rc)); !strings.Contains(got, "- … and 5 more sources (6 tools); use tool_search\n") {
		t.Fatalf("factory did not carry max_tokens:\n%s", got)
	}
}

func systemTextFor(t *testing.T, p *ReActPlanner, rc planner.RunContext) string {
	t.Helper()
	client := p.client.(*lastRequestClient)
	if _, err := p.Next(t.Context(), rc); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	return systemText(t, *client.req)
}

// Two turns of one run: the index sits after <available_tools>, outside
// the cached spine, and does not change when nothing changed or when a
// deferred tool is discovered between the turns.
func TestNext_DeferredToolIndexStableAcrossTurns(t *testing.T) {
	t.Parallel()
	cat := deferredIndexFixture(t)
	view := deferredIndexView(cat)
	q := discoveryAliasIdentity()
	ctx := promptCacheCtx(t, q, "agent-1")
	turn := func(tr *planner.Trajectory) llm.CompleteRequest {
		return nextRequest(t, ctx, planner.RunContext{Quadruple: q, Goal: "g", Catalog: view, Trajectory: tr})
	}
	first, second := turn(nil), turn(nil)
	s1, s2 := systemText(t, first), systemText(t, second)
	if s1 != s2 {
		t.Fatal("system message changed between identical turns")
	}
	index := deferredSection(s1)
	if index == "" {
		t.Fatal("index missing")
	}
	if strings.Index(s1, "<deferred_tools>") < strings.Index(s1, "</available_tools>") || strings.Index(s1, "<deferred_tools>") > strings.Index(s1, "<current_date>") {
		t.Fatal("index is not between <available_tools> and <current_date>")
	}
	if strings.Contains(systemSpine(first.Messages), "<deferred_tools>") {
		t.Fatal("index leaked into the cached system spine")
	}

	// Discover a deferred tool with the real search, then take turn 3.
	third := turn(searchAliasTrajectory(t, cat, q, "crm_create_deal"))
	if _, ok := declarationNamed(third.Tools, "crm_create_deal"); !ok {
		t.Fatal("discovered tool not declared on the next turn")
	}
	s3 := systemText(t, third)
	if deferredSection(s3) != index {
		t.Fatalf("index changed after discovery\n--- before ---\n%s\n--- after ---\n%s", index, deferredSection(s3))
	}
	if systemSpine(first.Messages) != systemSpine(third.Messages) || first.PromptCacheScope != third.PromptCacheScope {
		t.Fatal("cached prefix changed across the run's turns")
	}
}

// The per-source cap bounds each line independently: a large source is
// truncated at its own budget while a small one is listed in full, and the
// overall bound is not reached.
func TestDeferredToolIndex_PerSourceBudget(t *testing.T) {
	t.Parallel()
	const perSource = 100 // tokens → 400 bytes
	got := layoutDeferredToolIndex(deferredToolsLead, budgetOwners(), perSource*4, 1<<20)
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "- ") && len(line)+1 > perSource*4 {
			t.Errorf("line is %d bytes, over the %d-byte per-source budget: %q", len(line)+1, perSource*4, line)
		}
	}
	for _, want := range []string{"- Big (200 tools): big_tool_000, ", "more; use tool_search\n- Mid (30 tools): mid_tool_00", "- Small (2 tools): small_a, small_b\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	big := strings.Count(got, "big_tool_")
	if big == 0 || big == 200 {
		t.Errorf("big source shows %d names, want a per-source truncation", big)
	}
	// The overall bound still applies on top of the per-source caps.
	tight := layoutDeferredToolIndex(deferredToolsLead, budgetOwners(), perSource*4, 600)
	if len(tight) > 600 || !strings.Contains(tight, "- Small (2 tools): ") {
		t.Errorf("overall bound not applied (%d bytes):\n%s", len(tight), tight)
	}
}

// Measures the per-name cost under the repo's chars/4 estimator for 200
// bare names and 200 names declared under a runtime-added connection's
// owner-scoped source prefix, and pins that the default per-source budget
// lists both sets of 200 names in full.
func TestDeferredToolIndex_DefaultBudgetMeasurement(t *testing.T) {
	t.Parallel()
	verbs := []string{"search_contacts", "create_deal", "update_record", "list_accounts", "get_invoice"}
	measure := func(prefix string) (string, int) {
		names := make([]string, 0, 200)
		for i := range 200 {
			names = append(names, tools.ModelVisibleToolName(fmt.Sprintf("%s%s_%d", prefix, verbs[i%len(verbs)], i)))
		}
		sort.Strings(names)
		owner := newBudgetOwner("Sales CRM", names)
		full := deferredOwnerLineLen(owner, len(names))
		line := layoutDeferredToolIndex(deferredToolsLead, []*deferredIndexOwner{owner}, DefaultDeferredToolIndexMaxTokensPerSource*4, DefaultDeferredToolIndexMaxTokens*4)
		return line, full
	}
	bare, bareBytes := measure("")
	if strings.Contains(bare, "more; use tool_search") {
		t.Fatalf("default per-source budget does not list 200 bare names (%d bytes, %d tokens)", bareBytes, bareBytes/4+1)
	}
	prefixed, prefixedBytes := measure("crm~u-0123456789abcdef_")
	if strings.Contains(prefixed, "more; use tool_search") || strings.Count(prefixed, "crm_u-") != 200 {
		t.Fatalf("default per-source budget lists %d of 200 owner-prefixed names (%d bytes, %d tokens)", strings.Count(prefixed, "crm_u-"), prefixedBytes, prefixedBytes/4+1)
	}
	t.Logf("200 bare names: %d bytes = %d tokens (chars/4), %.1f tokens/name", bareBytes, bareBytes/4+1, float64(bareBytes)/4/200)
	t.Logf("200 owner-prefixed names: %d bytes = %d tokens (chars/4), %.1f tokens/name; listed in full at default: %t",
		prefixedBytes, prefixedBytes/4+1, float64(prefixedBytes)/4/200, !strings.Contains(prefixed, "more; use tool_search"))
	t.Logf("owner-prefixed names listed at the default per-source budget: %d of 200", strings.Count(prefixed, "crm_u-"))
}
