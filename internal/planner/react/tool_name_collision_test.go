package react

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// longServerID is a source id long enough that the OLD 64-byte head
// truncation bit BEFORE the `_<verb>` suffix started. Injected catalog keys
// are `<sourceID>_<tool>`, so every tool of such a source shared its first
// 64 bytes and collapsed onto one declaration.
const longServerID = "tenant-acme-production_agent-customer-support-bot_github-mcp-srv"

// typicalServerID is the shape an operator reaches for when catalog keys
// have to be globally unique: a high-entropy uniquifier plus a human label.
const typicalServerID = "sadsdwdqdqwawdwwqsasadwdas_githubmcp"

// collisionCatalog is a minimal planner.ToolCatalogView holding one
// source's worth of prefix-joined tools.
type collisionCatalog struct{ list []tools.Tool }

func (c *collisionCatalog) Resolve(name string) (tools.Tool, bool) {
	for _, t := range c.list {
		if t.Name == name {
			return t, true
		}
	}
	return tools.Tool{}, false
}

func (c *collisionCatalog) List() []tools.Tool {
	out := make([]tools.Tool, len(c.list))
	copy(out, c.list)
	return out
}

// githubVerbs is a representative tool set for one mid-sized MCP server.
// `get_pull_request_review_comments` is the longest at 32 bytes — the width
// maxToolNameBytes is calibrated against.
var githubVerbs = []string{
	"create_issue", "list_issues", "close_issue", "create_pull_request",
	"list_pull_requests", "merge_pull_request", "get_file_contents",
	"create_branch", "list_commits", "search_code", "get_issue",
	"add_issue_comment", "list_branches", "create_repository", "fork_repository",
	"get_pull_request_files", "get_pull_request_diff", "update_issue",
	"search_issues", "search_repositories", "list_workflow_runs",
	"get_workflow_run", "rerun_workflow", "list_releases", "create_release",
	"get_commit", "list_tags", "delete_branch", "list_collaborators",
	"get_pull_request_review_comments",
}

// oneSourcesTools builds prefix-joined catalog entries for a single source
// id, mirroring what a tool source injects into the flat catalog.
func oneSourcesTools(sourceID string, verbs []string) []tools.Tool {
	out := make([]tools.Tool, 0, len(verbs))
	for _, v := range verbs {
		out = append(out, tools.Tool{
			Name:        sourceID + "_" + v,
			Description: "GitHub MCP tool: " + v,
			ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"}}}`),
		})
	}
	return out
}

// catalogDeclarations drops the reserved planner controls, leaving the
// operator-catalog half of a per-turn declaration slice.
func catalogDeclarations(decls []llm.ToolDeclaration) []llm.ToolDeclaration {
	reserved := make(map[string]struct{})
	for _, r := range reservedPlannerControlDeclarations() {
		reserved[r.Name] = struct{}{}
	}
	out := make([]llm.ToolDeclaration, 0, len(decls))
	for _, d := range decls {
		if _, isReserved := reserved[d.Name]; isReserved {
			continue
		}
		out = append(out, d)
	}
	return out
}

// TestBuildToolDeclarations_LongSourceIDDoesNotCollapse is the regression
// guard for the silent tool-drop.
//
// Before the fix, sanitizeToolName head-truncated every
// `<longServerID>_<verb>` catalog key to the SAME 64 bytes, so the
// sanitized-key dedup dropped all but the first with a bare `continue`:
// 30 catalog tools produced 1 declaration and nothing anywhere said so.
// The `<available_tools>` prompt section meanwhile still listed all 30 (it
// dedups on the raw name), so the model was TOLD about thirty tools and
// could call exactly one.
func TestBuildToolDeclarations_LongSourceIDDoesNotCollapse(t *testing.T) {
	catalog := oneSourcesTools(longServerID, githubVerbs)
	rc := planner.RunContext{Catalog: &collisionCatalog{list: catalog}}

	decls := catalogDeclarations(buildToolDeclarations(rc, nil))

	names := make(map[string]int, len(decls))
	for _, d := range decls {
		names[d.Name]++
		if len(d.Name) > 64 {
			t.Errorf("declaration %q is %d bytes — a provider rejects >64", d.Name, len(d.Name))
		}
	}

	t.Logf("source id len=%d: catalog tools in=%d, declarations out=%d, distinct names=%d",
		len(longServerID), len(catalog), len(decls), len(names))

	if len(decls) != len(catalog) {
		t.Errorf("catalog declarations = %d, want %d — %d tool(s) collapsed",
			len(decls), len(catalog), len(catalog)-len(decls))
	}
	for n, c := range names {
		if c > 1 {
			t.Errorf("declaration name %q emitted %d times — declarations must be distinct", n, c)
		}
	}

	// Round-trip: every declared name resolves back to its real catalog key.
	rcp := &planner.RunContext{Catalog: &collisionCatalog{list: catalog}}
	for _, tool := range catalog {
		declared := sanitizeToolName(tool.Name)
		if got := resolveDeclaredToolName(rcp, declared); got != tool.Name {
			t.Errorf("resolveDeclaredToolName(%q) = %q, want %q", declared, got, tool.Name)
		}
	}
}

// TestSanitizeToolName_KeepsTheVerbVisible pins the half of the shortening
// that matters for model comprehension: an over-budget name gives up its
// HEAD (the source id every sibling tool repeats), never its tail (the verb
// that says what the tool DOES). This is the property maxToolNameBytes is
// calibrated against — if the budget is ever tightened, this test is what
// fails first.
func TestSanitizeToolName_KeepsTheVerbVisible(t *testing.T) {
	for _, id := range []string{longServerID, typicalServerID} {
		for _, verb := range githubVerbs {
			full := id + "_" + verb
			got := sanitizeToolName(full)
			if len(got) > maxToolNameBytes {
				t.Errorf("sanitizeToolName(%q) is %d bytes, want <= %d", full, len(got), maxToolNameBytes)
			}
			if !strings.Contains(got, verb) {
				t.Errorf("sanitizeToolName(%q) = %q — the verb %q is not visible to the model", full, got, verb)
			}
		}
	}
}

// TestSanitizeToolName_LongNamesStayDistinct pins the transform directly:
// names sharing an over-budget prefix must not collapse onto one
// provider-safe form.
func TestSanitizeToolName_LongNamesStayDistinct(t *testing.T) {
	seen := make(map[string]string)
	for _, v := range githubVerbs {
		full := longServerID + "_" + v
		s := sanitizeToolName(full)
		if prev, dup := seen[s]; dup {
			t.Errorf("sanitizeToolName collapsed %q and %q onto %q", prev, full, s)
		}
		seen[s] = full
	}
	if len(seen) != len(githubVerbs) {
		t.Errorf("distinct sanitized forms = %d, want %d", len(seen), len(githubVerbs))
	}
}

// TestSanitizeToolName_ShortNamesUnchanged guards the property that makes
// the tighter budget safe to adopt: a catalog whose keys are already short
// pays nothing. Only over-budget names are transformed.
func TestSanitizeToolName_ShortNamesUnchanged(t *testing.T) {
	for _, verb := range githubVerbs {
		full := "github_" + verb
		if len(full) > maxToolNameBytes {
			continue
		}
		if got := sanitizeToolName(full); got != full {
			t.Errorf("sanitizeToolName(%q) = %q — an in-budget name must pass through unchanged", full, got)
		}
	}
}

// TestSanitizeToolName_Deterministic guards the property
// resolveDeclaredToolName depends on: the transform stores no inverse and
// reads nothing but its argument, so it returns the same output for the
// same input on every call. A catalog- or order-dependent name (an assigned
// short alias, say) would break trajectory replay the first time the
// catalog grew mid-run — discovered tools are appended per turn, and a
// replayed historical tool_call must still match the current declaration.
func TestSanitizeToolName_Deterministic(t *testing.T) {
	inputs := []string{
		"clock.now",
		longServerID + "_create_issue",
		longServerID + "_list_issues",
		typicalServerID + "_get_me",
	}
	for _, in := range inputs {
		first := sanitizeToolName(in)
		for range 64 {
			if got := sanitizeToolName(in); got != first {
				t.Fatalf("sanitizeToolName(%q) not deterministic: %q then %q", in, first, got)
			}
		}
	}
}

// TestRenderAvailableTools_MatchesDeclaredNames closes the leak that made
// the shortening unsafe on its own: the `<available_tools>` prompt section
// used to render the RAW catalog key while `req.Tools[]` declared the
// sanitized one, so the model was shown a name it could not call. Both
// surfaces now go through the same transform.
func TestRenderAvailableTools_MatchesDeclaredNames(t *testing.T) {
	catalog := append(oneSourcesTools(typicalServerID, githubVerbs),
		tools.Tool{Name: "clock.now", Description: "current time", ArgsSchema: json.RawMessage(`{"type":"object"}`)})
	rc := planner.RunContext{Catalog: &collisionCatalog{list: catalog}}

	section := renderAvailableToolsSection(rc, 3)
	for _, d := range catalogDeclarations(buildToolDeclarations(rc, nil)) {
		if !strings.Contains(section, "- "+d.Name) {
			t.Errorf("<available_tools> does not list declared name %q — the model is shown a name it cannot call", d.Name)
		}
	}
	// The raw dotted key must not appear: it is not callable.
	if strings.Contains(section, "clock.now") {
		t.Error("<available_tools> still renders the raw key 'clock.now', which is declared as 'clock_now'")
	}
}

// TestBuildToolDeclarations_ResidualCollisionIsLoud covers the collision
// the shortening CANNOT remove: the disallowed-character mapping is
// many-to-one, so `clock.now` and `clock/now` both sanitize to `clock_now`
// regardless of length. The collider is still dropped — declaring one
// function name twice makes provider-side dispatch ambiguous — but the drop
// MUST be announced. Silence is the forbidden outcome (§13).
func TestBuildToolDeclarations_ResidualCollisionIsLoud(t *testing.T) {
	catalog := []tools.Tool{
		{Name: "clock.now", ArgsSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "clock/now", ArgsSchema: json.RawMessage(`{"type":"object"}`)},
	}
	if sanitizeToolName(catalog[0].Name) != sanitizeToolName(catalog[1].Name) {
		t.Fatalf("fixture no longer collides: %q vs %q",
			sanitizeToolName(catalog[0].Name), sanitizeToolName(catalog[1].Name))
	}

	var emitted []planner.ToolDeclarationCollisionPayload
	rc := planner.RunContext{
		Catalog: &collisionCatalog{list: catalog},
		Emit: func(e events.Event) {
			if e.Type != planner.EventTypePlannerToolDeclarationCollision {
				return
			}
			p, ok := e.Payload.(planner.ToolDeclarationCollisionPayload)
			if !ok {
				t.Errorf("payload = %T, want ToolDeclarationCollisionPayload", e.Payload)
				return
			}
			emitted = append(emitted, p)
		},
	}

	decls := buildToolDeclarations(rc, nil)

	if len(emitted) != 1 {
		t.Fatalf("collision events = %d, want 1 — the drop was silent", len(emitted))
	}
	got := emitted[0]
	if got.DroppedTool != "clock/now" {
		t.Errorf("DroppedTool = %q, want %q", got.DroppedTool, "clock/now")
	}
	if got.DeclaredTool != "clock.now" {
		t.Errorf("DeclaredTool = %q, want %q", got.DeclaredTool, "clock.now")
	}
	if got.DeclaredName != "clock_now" {
		t.Errorf("DeclaredName = %q, want %q", got.DeclaredName, "clock_now")
	}

	n := 0
	for _, d := range decls {
		if d.Name == "clock_now" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("clock_now declared %d times, want 1", n)
	}
}

// TestBuildToolDeclarations_CollisionWithReservedControlIsLoud covers the
// other collision source: an operator tool whose name maps onto a reserved
// planner control. The control wins (the planner cannot function without
// it) and the operator's tool is dropped — loudly.
func TestBuildToolDeclarations_CollisionWithReservedControlIsLoud(t *testing.T) {
	catalog := []tools.Tool{
		{Name: "_spawn.task", ArgsSchema: json.RawMessage(`{"type":"object"}`)},
	}
	if sanitizeToolName("_spawn.task") != SpawnTaskToolName {
		t.Fatalf("fixture no longer collides with %q: got %q", SpawnTaskToolName, sanitizeToolName("_spawn.task"))
	}
	var emitted []planner.ToolDeclarationCollisionPayload
	rc := planner.RunContext{
		Catalog: &collisionCatalog{list: catalog},
		Emit: func(e events.Event) {
			if e.Type == planner.EventTypePlannerToolDeclarationCollision {
				p, ok := e.Payload.(planner.ToolDeclarationCollisionPayload)
				if !ok {
					t.Errorf("payload = %T, want ToolDeclarationCollisionPayload", e.Payload)
					return
				}
				emitted = append(emitted, p)
			}
		},
	}
	buildToolDeclarations(rc, nil)
	if len(emitted) != 1 {
		t.Fatalf("collision events = %d, want 1 — the operator's tool vanished silently", len(emitted))
	}
	if emitted[0].DroppedTool != "_spawn.task" || emitted[0].DeclaredTool != SpawnTaskToolName {
		t.Errorf("payload = {declared:%q dropped:%q}, want {declared:%q dropped:%q}",
			emitted[0].DeclaredTool, emitted[0].DroppedTool, SpawnTaskToolName, "_spawn.task")
	}
}

// TestBuildToolDeclarations_BenignRediscoveryStaysQuiet guards the other
// direction. Discovery routinely re-surfaces a tool already in the
// always-loaded set; skipping the duplicate is the intended behaviour and
// must NOT emit a collision. A diagnostic that fires on the normal path is
// noise, and noise is how a real signal gets ignored.
func TestBuildToolDeclarations_BenignRediscoveryStaysQuiet(t *testing.T) {
	catalog := oneSourcesTools(longServerID, githubVerbs)
	n := 0
	rc := planner.RunContext{
		Catalog: &collisionCatalog{list: catalog},
		Emit: func(e events.Event) {
			if e.Type == planner.EventTypePlannerToolDeclarationCollision {
				n++
			}
		},
	}
	names := make([]string, 0, len(catalog))
	for _, tool := range catalog {
		names = append(names, tool.Name)
	}
	decls := catalogDeclarations(buildToolDeclarations(rc, names))

	if n != 0 {
		t.Errorf("collision events = %d on a benign re-discovery, want 0", n)
	}
	if len(decls) != len(catalog) {
		t.Errorf("declarations = %d, want %d (re-discovery must not duplicate)", len(decls), len(catalog))
	}
}

// TestBuildToolDeclarations_DeclaredNameCostPerTurn records what the
// model-visible naming actually costs, and pins the direction the budget
// exists to enforce: a long catalog key must NOT cost more per turn than a
// short one, because the declared form is bounded independently of the key.
//
// The name is paid twice per tool per turn — once in the `req.Tools[]`
// declaration and once in the `<available_tools>` prompt section.
func TestBuildToolDeclarations_DeclaredNameCostPerTurn(t *testing.T) {
	cases := []struct{ label, id string }{
		{"short key", "github"},
		{"typical key", typicalServerID},
		{"long key", longServerID},
	}
	var perTurn []int
	for _, c := range cases {
		catalog := oneSourcesTools(c.id, githubVerbs)
		rc := planner.RunContext{Catalog: &collisionCatalog{list: catalog}}
		decls := catalogDeclarations(buildToolDeclarations(rc, nil))

		rawKeyBytes, declNameBytes := 0, 0
		for _, tl := range catalog {
			rawKeyBytes += len(tl.Name)
		}
		for _, d := range decls {
			declNameBytes += len(d.Name)
		}
		total := declNameBytes * 2
		perTurn = append(perTurn, total)

		t.Logf("%-12s keyLen=%2d tools=%d rawKeyBytes=%4d declNameBytes=%4d declaredNameBytesPerTurn=%4d",
			c.label, len(c.id), len(catalog), rawKeyBytes, declNameBytes, total)
	}
	// A long key must cost no more than a typical one: both are bounded by
	// maxToolNameBytes. Without the bound the long key cost strictly more.
	if perTurn[2] > perTurn[1] {
		t.Errorf("long key costs %d bytes/turn vs typical %d — the declared name is not bounded",
			perTurn[2], perTurn[1])
	}
}
