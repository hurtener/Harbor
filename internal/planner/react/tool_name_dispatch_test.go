package react

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

type mutableProjectionCatalog struct {
	mu    sync.RWMutex
	list  []tools.Tool
	lists int
}

func (c *mutableProjectionCatalog) Replace(list []tools.Tool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append([]tools.Tool(nil), list...)
}

func (c *mutableProjectionCatalog) Resolve(name string) (tools.Tool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, tool := range c.list {
		if tool.Name == name {
			return tool, true
		}
	}
	return tools.Tool{}, false
}

func (c *mutableProjectionCatalog) List() []tools.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	return append([]tools.Tool(nil), c.list...)
}

func (c *mutableProjectionCatalog) ListCalls() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lists
}

type projectionMutationLLM struct {
	completeStarted chan struct{}
	mutationDone    chan struct{}
	t               *testing.T
}

func (c *projectionMutationLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.t.Helper()
	decl, ok := declarationNamed(req.Tools, "clock_now")
	if !ok || decl.Description != "clock.now" {
		c.t.Fatalf("request declaration = %#v, want clock_now owned by clock.now", decl)
	}
	close(c.completeStarted)
	<-c.mutationDone
	return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{
		ID: "call-clock", Name: "clock_now", Args: json.RawMessage(`{}`),
	}}}, nil
}

func (*projectionMutationLLM) Close(context.Context) error { return nil }

// TestReActPlanner_TurnProjectionSurvivesConcurrentCatalogMutation proves the
// model response is interpreted through the exact immutable projection sent
// in its request. While Complete is in flight, the catalog replaces
// `clock.now` with the colliding raw key `clock_now`. Rebuilding after
// Complete would silently retarget the returned declared name to the new tool.
func TestReActPlanner_TurnProjectionSurvivesConcurrentCatalogMutation(t *testing.T) {
	initial := tools.Tool{Name: "clock.now", Description: "clock.now", ArgsSchema: json.RawMessage(`{"type":"object"}`)}
	replacement := tools.Tool{Name: "clock_now", Description: "clock_now", ArgsSchema: json.RawMessage(`{"type":"object"}`)}
	catalog := &mutableProjectionCatalog{list: []tools.Tool{initial}}
	started := make(chan struct{})
	mutated := make(chan struct{})
	client := &projectionMutationLLM{completeStarted: started, mutationDone: mutated, t: t}

	go func() {
		<-started
		catalog.Replace([]tools.Tool{replacement})
		close(mutated)
	}()

	decision, err := New(client).Next(context.Background(), planner.RunContext{
		Quadruple: identity.Quadruple{
			Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"},
			RunID:    "frozen-tool-projection",
		},
		Goal:    "read the clock",
		Catalog: catalog,
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	call, ok := decision.(planner.CallTool)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallTool", decision)
	}
	if call.Tool != initial.Name {
		t.Fatalf("CallTool.Tool = %q, want request-time owner %q; catalog mutation retargeted the declared name", call.Tool, initial.Name)
	}
	if got := catalog.ListCalls(); got != 1 {
		t.Fatalf("catalog List calls = %d, want exactly one immutable provider-turn snapshot", got)
	}
}

// collidingPair is the catalog shape the whole file probes, and it is
// reachable with HARBOR'S OWN naming conventions rather than a contrived
// fixture: built-in tools are dotted (`clock.now`) and injected tool-source
// keys are `<sourceID>_<tool>`, so an MCP server registered as `clock` that
// exposes a `now` tool lands on the catalog key `clock_now`. Both sanitize
// to the single provider-safe function name `clock_now`.
//
// Descriptions are the raw catalog names on purpose: a declaration carries
// the SANITIZED name, so the description is the only thing in a
// llm.ToolDeclaration that still identifies which catalog tool it came from.
func collidingPair(dottedFirst bool) []tools.Tool {
	dotted := tools.Tool{
		Name:        "clock.now",
		Description: "clock.now",
		ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"tz":{"type":"string"}}}`),
	}
	underscored := tools.Tool{
		Name:        "clock_now",
		Description: "clock_now",
		ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"format":{"type":"string"}}}`),
	}
	if dottedFirst {
		return []tools.Tool{dotted, underscored}
	}
	return []tools.Tool{underscored, dotted}
}

// collectCollisions returns a RunContext over the given catalog plus the
// slice its Emit closure appends every collision payload to.
func collectCollisions(catalog []tools.Tool) (*planner.RunContext, *[]planner.ToolDeclarationCollisionPayload) {
	var got []planner.ToolDeclarationCollisionPayload
	rc := &planner.RunContext{
		Catalog: &collisionCatalog{list: catalog},
		Emit: func(e events.Event) {
			if e.Type != planner.EventTypePlannerToolDeclarationCollision {
				return
			}
			if p, ok := e.Payload.(planner.ToolDeclarationCollisionPayload); ok {
				got = append(got, p)
			}
		},
	}
	return rc, &got
}

// declarationNamed returns the declaration emitted under the given
// provider-safe function name.
func declarationNamed(decls []llm.ToolDeclaration, name string) (llm.ToolDeclaration, bool) {
	for _, d := range decls {
		if d.Name == name {
			return d, true
		}
	}
	return llm.ToolDeclaration{}, false
}

// TestResolveDeclaredToolName_DroppedColliderIsNeverDispatched is the
// regression guard for the residual-collision DISPATCH defect.
//
// A dropped collider whose catalog name happens to BE the provider-safe
// string used to win the resolver's exact-match branch, so the model was
// shown tool A's description and schema under a name that executed tool B:
//
//	declaration `clock_now` carries clock.now's description and schema
//	collision announces declared="clock.now" dropped="clock_now"
//	the model returns "clock_now" -> resolveDeclaredToolName -> "clock_now"
//	dispatch executes the DROPPED tool, with args shaped for the DECLARED one
//
// Nothing failed, nothing logged: the run silently executed a different
// tool than the one the model read about. That is CLAUDE.md §13 silent
// degradation on an execution path. The declared tool is the ONLY one the
// model can reach, so the provider-safe name must resolve to it.
func TestResolveDeclaredToolName_DroppedColliderIsNeverDispatched(t *testing.T) {
	catalog := collidingPair(true)
	rc, collisions := collectCollisions(catalog)

	decls := buildToolDeclarations(*rc, nil)

	// Preconditions: exactly one declaration under the colliding name, it
	// belongs to `clock.now`, and the drop was announced naming `clock_now`.
	decl, ok := declarationNamed(decls, "clock_now")
	if !ok {
		t.Fatalf("no declaration named %q emitted", "clock_now")
	}
	if decl.Description != "clock.now" {
		t.Fatalf("declaration %q carries description %q, want %q",
			decl.Name, decl.Description, "clock.now")
	}
	if len(*collisions) != 1 {
		t.Fatalf("collision events = %d, want 1", len(*collisions))
	}
	got := (*collisions)[0]
	if got.DeclaredTool != "clock.now" || got.DroppedTool != "clock_now" {
		t.Fatalf("collision = {declared:%q dropped:%q}, want {declared:%q dropped:%q}",
			got.DeclaredTool, got.DroppedTool, "clock.now", "clock_now")
	}
	// The SHADOW shape: the dropped tool's catalog name IS the provider-safe
	// function name. It is what makes the payload easy to misread ("declared
	// name clock_now, so clock_now is what runs") and it is the exact case
	// the exact-match branch got wrong. The payload's semantics are pinned
	// here: DeclaredTool is reachable, DroppedTool is not, under a name that
	// spells DroppedTool.
	if got.DroppedTool != got.DeclaredName {
		t.Fatalf("fixture no longer exercises the shadow shape: dropped=%q declaredName=%q",
			got.DroppedTool, got.DeclaredName)
	}

	// The defect: the model returns the only name it was given.
	if got := mustResolveDeclaredToolName(t, rc, "clock_now"); got != "clock.now" {
		t.Errorf("resolveDeclaredToolName(%q) = %q, want %q — the DROPPED tool is being dispatched "+
			"under the DECLARED tool's schema", "clock_now", got, "clock.now")
	}

	// End-to-end through the projector, which is where it reaches dispatch.
	resp := llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{
		ID:   "c1",
		Name: "clock_now",
		Args: json.RawMessage(`{"tz":"UTC"}`),
	}}}
	dec, err := projectResponse(resp, rc, true)
	if err != nil {
		t.Fatalf("projectResponse: %v", err)
	}
	ct, okDec := dec.(planner.CallTool)
	if !okDec {
		t.Fatalf("decision = %T, want planner.CallTool", dec)
	}
	if ct.Tool != "clock.now" {
		t.Errorf("CallTool.Tool = %q, want %q — the projector dispatches the dropped collider", ct.Tool, "clock.now")
	}
}

// TestResolveDeclaredToolName_OrderIndependent pins the property that made
// the defect so hard to see: the SAME two catalog tools dispatched correctly
// in one registration order and incorrectly in the other, because the
// resolver's exact-match branch happened to agree with the declaration in
// one order only. Resolution now re-derives the forward transform in the
// same precedence buildToolDeclarations applies, so whichever tool KEEPS the
// declaration is the one the name resolves to — in both orders.
func TestResolveDeclaredToolName_OrderIndependent(t *testing.T) {
	for _, dottedFirst := range []bool{true, false} {
		catalog := collidingPair(dottedFirst)
		rc, collisions := collectCollisions(catalog)
		decls := buildToolDeclarations(*rc, nil)

		decl, ok := declarationNamed(decls, "clock_now")
		if !ok {
			t.Fatalf("dottedFirst=%v: no declaration named %q", dottedFirst, "clock_now")
		}
		if len(*collisions) != 1 {
			t.Fatalf("dottedFirst=%v: collision events = %d, want 1", dottedFirst, len(*collisions))
		}
		// The declaration's description names its owning catalog tool.
		want := decl.Description
		if got := mustResolveDeclaredToolName(t, rc, "clock_now"); got != want {
			t.Errorf("dottedFirst=%v: resolveDeclaredToolName(%q) = %q, want %q (the declared tool)",
				dottedFirst, "clock_now", got, want)
		}
		// And the announced DroppedTool is never reachable under it.
		if got := mustResolveDeclaredToolName(t, rc, "clock_now"); got == (*collisions)[0].DroppedTool {
			t.Errorf("dottedFirst=%v: the announced dropped tool %q is what dispatches",
				dottedFirst, got)
		}
	}
}

// TestResolveDeclaredToolName_MirrorsEveryDeclaration is the lockstep guard
// between the two halves of the round trip: for EVERY declaration
// buildToolDeclarations emits, resolving that declaration's name must return
// the catalog tool the declaration was built from. It runs over a catalog
// that mixes every shape the transform has to survive at once — dotted
// built-ins, a residual collider, over-budget source-joined keys, an
// already-provider-safe key, and a discovered tool absent from the
// always-loaded view.
//
// This is the guard that would have caught the defect generically rather
// than one hand-picked pair at a time.
func TestResolveDeclaredToolName_MirrorsEveryDeclaration(t *testing.T) {
	catalog := []tools.Tool{
		{Name: "clock.now", Description: "clock.now"},
		{Name: "clock_now", Description: "clock_now"},
		{Name: "clock/now", Description: "clock/now"},
		{Name: "inventory.check", Description: "inventory.check"},
		{Name: "mcptest_echo", Description: "mcptest_echo"},
		{Name: "_spawn.task", Description: "_spawn.task"},
	}
	for _, v := range githubVerbs {
		catalog = append(catalog, tools.Tool{
			Name:        longServerID + "_" + v,
			Description: longServerID + "_" + v,
		})
	}
	// A discovered tool that is NOT in the always-loaded view: it reaches
	// req.Tools only through the discovered arm, so its declaration name has
	// to resolve through the discovered arm too.
	discoveredOnly := tools.Tool{Name: typicalServerID + "_search_code", Description: typicalServerID + "_search_code"}
	full := append(append([]tools.Tool{}, catalog...), discoveredOnly)

	rc := &planner.RunContext{
		// List() is the always-loaded view; Resolve() sees the full catalog.
		Catalog:         &splitCatalog{listed: catalog, all: full},
		DiscoveredTools: []string{discoveredOnly.Name},
	}
	decls := buildToolDeclarations(*rc, rc.DiscoveredTools)

	reserved := make(map[string]struct{})
	for _, r := range reservedPlannerControlDeclarations() {
		reserved[r.Name] = struct{}{}
	}
	checked := 0
	for _, d := range decls {
		if _, isReserved := reserved[d.Name]; isReserved {
			// A reserved control name must never resolve to a catalog tool:
			// the operator tool that collided with it was DROPPED.
			if got, ok := resolveDeclaredToolName(rc, d.Name); ok {
				t.Errorf("resolveDeclaredToolName(%q) = %q, true — a reserved planner control resolved to a catalog tool",
					d.Name, got)
			}
			continue
		}
		checked++
		if got := mustResolveDeclaredToolName(t, rc, d.Name); got != d.Description {
			t.Errorf("resolveDeclaredToolName(%q) = %q, want %q — resolution disagrees with the declaration",
				d.Name, got, d.Description)
		}
	}
	if checked < len(catalog)-2 {
		t.Fatalf("only %d catalog declarations checked, want >= %d — the fixture stopped exercising the path",
			checked, len(catalog)-2)
	}
}

// splitCatalog separates the always-loaded view (List) from the full catalog
// (Resolve), which is the real shape: List() is identity- and
// LoadingMode-filtered, while Resolve() reaches every registered tool.
type splitCatalog struct {
	listed []tools.Tool
	all    []tools.Tool
}

func (c *splitCatalog) Resolve(name string) (tools.Tool, bool) {
	for _, t := range c.all {
		if t.Name == name {
			return t, true
		}
	}
	return tools.Tool{}, false
}

func (c *splitCatalog) List() []tools.Tool {
	out := make([]tools.Tool, len(c.listed))
	copy(out, c.listed)
	return out
}

// TestResolveDeclaredToolName_ReservedControlNeverResolvesToCatalogTool
// closes the reserved-control half of the same defect. An operator tool
// named `_spawn.task` sanitizes onto the reserved control `_spawn_task` and
// is DROPPED (the control wins — the planner cannot function without it), so
// the reserved name must not resolve to it. The projector's reserved-name
// switch intercepts these before resolution today; the guard exists because
// "unreachable by construction" is exactly the reasoning this defect
// punished once already.
func TestResolveDeclaredToolName_ReservedControlNeverResolvesToCatalogTool(t *testing.T) {
	catalog := []tools.Tool{
		{Name: "_spawn.task", Description: "_spawn.task"},
		{Name: "_await.task", Description: "_await.task"},
		{Name: "_finish", Description: "_finish"},
	}
	rc := &planner.RunContext{Catalog: &collisionCatalog{list: catalog}}
	for _, name := range []string{
		SpawnTaskToolName, AwaitTaskToolName, FinishToolName, TaskStatusToolName,
		CancelTaskToolName, SteerTaskToolName, PauseTaskToolName, ResumeTaskToolName,
		TaskProgressToolName,
	} {
		if got, ok := resolveDeclaredToolName(rc, name); ok {
			t.Errorf("resolveDeclaredToolName(%q) = %q, true — a reserved planner control resolved to a catalog tool", name, got)
		}
	}
}

func TestReservedPlannerControls_MatchSharedModelNamespace(t *testing.T) {
	want := tools.ReservedModelToolNames()
	declarations := reservedPlannerControlDeclarations()
	actual := []string{FinishToolName} // intercepted but intentionally not declared
	for _, declaration := range declarations {
		actual = append(actual, declaration.Name)
	}
	if len(actual) != len(want) {
		t.Fatalf("reserved controls = %d, shared namespace = %d", len(actual), len(want))
	}
	for i, name := range actual {
		if name != want[i] {
			t.Errorf("reserved control[%d] = %q, shared namespace = %q", i, name, want[i])
		}
	}
}

// TestResolveDeclaredToolName_DiscoveredToolIsDispatchable is the guard for
// the second defect the probe surfaced. `Catalog.List()` is the ALWAYS-loaded
// view (`tools.PlannerView.List` applies a CatalogFilter that defaults to
// LoadingAlways); a deferred tool reaches the model only after the
// tool_search discovery cycle appends it to `rc.DiscoveredTools`, and
// buildToolDeclarations declares it from that arm under its SANITIZED name.
//
// Resolution scanned List() alone, so a discovered tool whose name is dotted
// or over-budget was declared to the model and then failed on dispatch with
// "unknown tool" — every deferred built-in (`clock.now`) and every deferred
// tool of a long-id source. That failure is loud rather than silent, but the
// tool was unusable, and the discovery cycle exists precisely to make it
// usable.
func TestResolveDeclaredToolName_DiscoveredToolIsDispatchable(t *testing.T) {
	always := []tools.Tool{{Name: "mcptest_echo", Description: "mcptest_echo"}}
	deferredDotted := tools.Tool{Name: "clock.now", Description: "clock.now"}
	deferredLong := tools.Tool{
		Name:        longServerID + "_create_issue",
		Description: longServerID + "_create_issue",
	}
	rc := &planner.RunContext{
		Catalog: &splitCatalog{
			listed: always,
			all:    append(append([]tools.Tool{}, always...), deferredDotted, deferredLong),
		},
		DiscoveredTools: []string{deferredDotted.Name, deferredLong.Name},
	}

	decls := buildToolDeclarations(*rc, rc.DiscoveredTools)
	for _, want := range []tools.Tool{deferredDotted, deferredLong} {
		declared := sanitizeToolName(want.Name)
		if _, ok := declarationNamed(decls, declared); !ok {
			t.Fatalf("no declaration named %q — the fixture stopped exercising the discovered arm", declared)
		}
		if got := mustResolveDeclaredToolName(t, rc, declared); got != want.Name {
			t.Errorf("resolveDeclaredToolName(%q) = %q, want %q — a DISCOVERED tool is declared but undispatchable",
				declared, got, want.Name)
		}
	}

	// A discovered name the catalog no longer resolves must NOT be invented or
	// passed through as an alternate raw vocabulary.
	rc.DiscoveredTools = append(rc.DiscoveredTools, "ghost.tool")
	if got, ok := resolveDeclaredToolName(rc, "ghost_tool"); ok {
		t.Errorf("resolveDeclaredToolName(%q) = %q, true; want refusal", "ghost_tool", got)
	}
}

// TestResolveDeclaredToolName_ConcurrentReuse pins the concurrent-reuse
// contract over the resolution path (CLAUDE.md §5 / §11): resolution reads
// only its arguments and the per-run RunContext, holds no state of its own,
// and is safe to drive from many goroutines against ONE shared catalog view.
// This is the property that makes the stateless re-derivation preferable to
// a recorded declared→catalog map.
func TestResolveDeclaredToolName_ConcurrentReuse(t *testing.T) {
	const n = 128
	catalog := append(collidingPair(true), oneSourcesTools(longServerID, githubVerbs)...)
	shared := &collisionCatalog{list: catalog}

	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine gets its OWN RunContext over the shared view —
			// the per-run scope the contract requires.
			rc := &planner.RunContext{Catalog: shared}
			if got := mustResolveDeclaredToolName(t, rc, "clock_now"); got != "clock.now" {
				errs <- "goroutine " + strconv.Itoa(i) + ": clock_now resolved to " + got
			}
			tool := catalog[2+(i%len(githubVerbs))]
			if got := mustResolveDeclaredToolName(t, rc, sanitizeToolName(tool.Name)); got != tool.Name {
				errs <- "goroutine " + strconv.Itoa(i) + ": " + tool.Name + " resolved to " + got
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestRenderAvailableTools_DropsTheSameColliderAsTheDeclarations closes the
// third instance of the same shape, arriving through the PROMPT rather than
// the projector.
//
// `<available_tools>` deduped on the RAW catalog name and then rendered each
// entry through sanitizeToolName, while buildToolDeclarations dedups on the
// SANITIZED name and drops the collider. On a residual collision the section
// therefore listed the provider-safe name TWICE — one bullet per colliding
// catalog tool, each with its own description — against a single declaration.
// The model read the DROPPED tool's description under a name that dispatches
// to the tool that kept the declaration: tool B's prose, tool A's code.
//
// The section must drop exactly what the declarations drop.
func TestRenderAvailableTools_DropsTheSameColliderAsTheDeclarations(t *testing.T) {
	for _, dottedFirst := range []bool{true, false} {
		catalog := collidingPair(dottedFirst)
		rc := planner.RunContext{Catalog: &collisionCatalog{list: catalog}}

		section := renderAvailableToolsSection(rc, 3)
		if n := strings.Count(section, "- clock_now"); n != 1 {
			t.Errorf("dottedFirst=%v: <available_tools> lists %q %d times, want 1 — the model reads two tools under one callable name",
				dottedFirst, "clock_now", n)
		}

		// And the surviving bullet must describe the tool the name actually
		// dispatches to.
		decls := buildToolDeclarations(rc, nil)
		decl, ok := declarationNamed(decls, "clock_now")
		if !ok {
			t.Fatalf("dottedFirst=%v: no declaration named %q", dottedFirst, "clock_now")
		}
		dropped := "clock_now"
		if decl.Description == "clock_now" {
			dropped = "clock.now"
		}
		if strings.Contains(section, "- clock_now: "+dropped) {
			t.Errorf("dottedFirst=%v: <available_tools> describes the DROPPED tool %q under the declared name",
				dottedFirst, dropped)
		}
	}
}

// sectionToolNames extracts the model-visible names `<available_tools>`
// renders, one per `- <name>[: desc]` bullet.
func sectionToolNames(section string) []string {
	var out []string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		name := strings.TrimPrefix(line, "- ")
		if i := strings.Index(name, ": "); i >= 0 {
			name = name[:i]
		}
		out = append(out, name)
	}
	return out
}

// TestRenderAvailableTools_SetMatchesDeclarations is the enforcement the
// `renderToolNameDesc` godoc's "the quick reference and the declaration can
// never disagree" claim never had. Both surfaces are derived from the same
// two arms (`Catalog.List()` + `rc.DiscoveredTools`), so the SET of names
// they produce must be equal — not merely the transform they each apply.
//
// The claim was false in two ways, both of which showed the model a tool it
// could not call, or could call only as something else:
//
//   - the section deduped on the RAW catalog key while the declarations
//     deduped on the SANITIZED one, so a residual collision listed one
//     callable name twice against one declaration; and
//   - the declarations seed their dedup with the reserved planner controls
//     (which always win their name) while the section did not, so an
//     operator tool named `_spawn.task` was DROPPED from the declarations
//     and still listed as `_spawn_task` — the reserved control's name
//     carrying the operator tool's description.
//
// A comment asserting an invariant over two independently-keyed dedups is
// the class of claim this guard replaces.
func TestRenderAvailableTools_SetMatchesDeclarations(t *testing.T) {
	catalog := []tools.Tool{
		{Name: "clock.now", Description: "clock.now"},
		{Name: "clock_now", Description: "clock_now"},
		{Name: "clock/now", Description: "clock/now"},
		{Name: "_spawn.task", Description: "_spawn.task"},
		{Name: "_await.task", Description: "_await.task"},
		{Name: "mcptest_echo", Description: "mcptest_echo"},
	}
	catalog = append(catalog, oneSourcesTools(longServerID, githubVerbs)...)
	discovered := tools.Tool{Name: typicalServerID + "_search_code", Description: typicalServerID + "_search_code"}
	rc := planner.RunContext{
		Catalog: &splitCatalog{
			listed: catalog,
			all:    append(append([]tools.Tool{}, catalog...), discovered),
		},
		DiscoveredTools: []string{discovered.Name},
	}

	inSection := make(map[string]int)
	for _, n := range sectionToolNames(renderAvailableToolsSection(rc, 3)) {
		inSection[n]++
	}
	inDecls := make(map[string]int)
	for _, d := range catalogDeclarations(buildToolDeclarations(rc, rc.DiscoveredTools)) {
		inDecls[d.Name]++
	}

	if len(inSection) == 0 || len(inDecls) == 0 {
		t.Fatalf("fixture produced nothing: section=%d decls=%d", len(inSection), len(inDecls))
	}
	for n, c := range inSection {
		if c != 1 {
			t.Errorf("<available_tools> lists %q %d times — one callable name, one bullet", n, c)
		}
		if _, ok := inDecls[n]; !ok {
			t.Errorf("<available_tools> lists %q but no declaration carries that name — the model is told about a tool it cannot call", n)
		}
	}
	for n := range inDecls {
		if _, ok := inSection[n]; !ok {
			t.Errorf("declaration %q is absent from <available_tools> — the two surfaces disagree", n)
		}
	}
}

// adversarialToolNames is the corpus the tail-first shortening's round trip
// was verified against, kept here so the resolution change is measured
// against the same bar rather than a friendlier one: empty, the 43/44/45
// boundary widths either side of maxToolNameBytes, a 500-byte name,
// multi-byte UTF-8, NUL and control bytes, all-underscore, and uppercase.
func adversarialToolNames() []string {
	return []string{
		"",
		"a",
		strings.Repeat("a", maxToolNameBytes-1), // 43 — in budget
		strings.Repeat("b", maxToolNameBytes),   // 44 — exactly at budget
		strings.Repeat("c", maxToolNameBytes+1), // 45 — one over
		strings.Repeat("d", 500),
		"héllo.wörld",
		"日本語.tool",
		"tool\x00with\x01control\x1fbytes",
		strings.Repeat("_", maxToolNameBytes+6),
		"UPPER.Case-Name",
		"clock.now",
		"clock/now", // collides with clock.now
		"clock_now", // collides with clock.now
		"_spawn.task",
		"trailing.",
		".leading",
		"a..b",
	}
}

// TestResolveDeclaredToolName_AdversarialRoundTrip re-runs the shortening's
// round-trip sweep against the NEW resolution path. Every declaration
// buildToolDeclarations emits over the adversarial corpus must resolve back
// to the catalog tool it was built from, and every collider the corpus
// contains must be announced and stay unreachable.
func TestResolveDeclaredToolName_AdversarialRoundTrip(t *testing.T) {
	var catalog []tools.Tool
	for _, n := range adversarialToolNames() {
		catalog = append(catalog, tools.Tool{Name: n, Description: n})
	}
	rc, collisions := collectCollisions(catalog)
	decls := buildToolDeclarations(*rc, nil)

	reserved := make(map[string]struct{})
	for _, r := range reservedPlannerControlDeclarations() {
		reserved[r.Name] = struct{}{}
	}
	for _, d := range decls {
		if _, isReserved := reserved[d.Name]; isReserved {
			continue
		}
		if len(d.Name) > 64 {
			t.Errorf("declaration %q is %d bytes — a provider rejects >64", d.Name, len(d.Name))
		}
		if got := mustResolveDeclaredToolName(t, rc, d.Name); got != d.Description {
			t.Errorf("resolveDeclaredToolName(%q) = %q, want %q", d.Name, got, d.Description)
		}
	}

	// The corpus contains three colliders (`clock/now` + `clock_now` onto
	// `clock.now`, and `_spawn.task` onto the reserved control). Each must be
	// announced, and none may be reachable under the name it lost.
	if len(*collisions) != 3 {
		t.Fatalf("collision events = %d, want 3 — the corpus stopped colliding", len(*collisions))
	}
	for _, c := range *collisions {
		if got, ok := resolveDeclaredToolName(rc, c.DeclaredName); ok && got == c.DroppedTool {
			t.Errorf("declared name %q dispatches the announced dropped tool %q", c.DeclaredName, got)
		}
	}
}

// TestSanitizeToolNameTo_TotalOverEveryBudget closes the panic NIT. The
// budget is a constant in production, but `sanitizeToolNameTo` takes it as a
// parameter, and `shortenToolName` used to compute a NEGATIVE retained-tail
// width for any budget below toolNameDigestBytes+1 — a slice-bounds panic
// one future caller away. CLAUDE.md §5 allows panic only for
// impossible-by-construction cases, and a parameter is not that.
func TestSanitizeToolNameTo_TotalOverEveryBudget(t *testing.T) {
	inputs := []string{
		"",
		"a",
		"clock.now",
		strings.Repeat("x", 500),
		longServerID + "_get_pull_request_review_comments",
		"héllo.wörld",          // multi-byte UTF-8
		"tool\x00with\x01ctrl", // NUL + control bytes
		"________",
		"UPPER.Case-Name",
	}
	for budget := -4; budget <= 64; budget++ {
		for _, in := range inputs {
			got := sanitizeToolNameTo(in, budget)
			if budget >= 0 && len(got) > budget && len(in) > budget {
				t.Errorf("sanitizeToolNameTo(%q, %d) = %q (%d bytes) — over budget", in, budget, got, len(got))
			}
			for _, r := range got {
				if !tools.IsModelToolNameRune(r) {
					t.Errorf("sanitizeToolNameTo(%q, %d) = %q — contains a provider-rejected rune %q",
						in, budget, got, r)
				}
			}
		}
	}
}

// TestSanitizeToolNameTo_StaysDiscriminatingUnderTinyBudgets pins that the
// total-function fix did not turn a tiny budget into a name COLLAPSE: even
// where no tail survives, distinct inputs keep distinct forms, because what
// remains is the digest of the whole sanitized string.
func TestSanitizeToolNameTo_StaysDiscriminatingUnderTinyBudgets(t *testing.T) {
	for budget := 4; budget <= toolNameDigestBytes+2; budget++ {
		seen := make(map[string]string, len(githubVerbs))
		for _, v := range githubVerbs {
			full := longServerID + "_" + v
			got := sanitizeToolNameTo(full, budget)
			if prev, dup := seen[got]; dup {
				t.Errorf("budget=%d: %q and %q both map onto %q", budget, prev, full, got)
			}
			seen[got] = full
		}
	}
}
