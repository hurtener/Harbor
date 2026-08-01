package react

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// sanitizeStubCatalog is a minimal planner.ToolCatalogView for the
// name-sanitization tests.
type sanitizeStubCatalog struct{ list []tools.Tool }

func mustResolveDeclaredToolName(t *testing.T, rc *planner.RunContext, name string) string {
	t.Helper()
	got, ok := resolveDeclaredToolName(rc, name)
	if !ok {
		t.Fatalf("resolveDeclaredToolName(%q) rejected a declared name", name)
	}
	return got
}

func (c *sanitizeStubCatalog) Resolve(name string) (tools.Tool, bool) {
	for _, t := range c.list {
		if t.Name == name {
			return t, true
		}
	}
	return tools.Tool{}, false
}

func (c *sanitizeStubCatalog) List() []tools.Tool {
	out := make([]tools.Tool, len(c.list))
	copy(out, c.list)
	return out
}

// TestSanitizeToolName pins the provider-safe transform. OpenAI-compatible
// native tool-calling rejects a function name that is not
// ^[a-zA-Z0-9_-]{1,64}$ with a 400, so Harbor's dotted convention must be
// sanitized before it reaches req.Tools.
func TestSanitizeToolName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"clock.now", "clock_now"},
		{"inventory.check", "inventory_check"},
		{"text.echo", "text_echo"},
		{"mcptest_echo", "mcptest_echo"},       // already valid → identity
		{"_spawn_task", "_spawn_task"},         // reserved control → identity
		{"a.b-c_d", "a_b-c_d"},                 // mixed
		{"weather lookup!", "weather_lookup_"}, // space + punctuation
		{"valid-name", "valid-name"},
	}
	for _, c := range cases {
		if got := sanitizeToolName(c.in); got != c.want {
			t.Errorf("sanitizeToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Over-budget names are shortened to exactly maxToolNameBytes. The
	// budget is deliberately BELOW the provider's 64-byte ceiling: a tool
	// name is paid on every turn, twice per tool, so the model-visible form
	// is bounded independently of how long the catalog key is.
	long := strings.Repeat("a", 100)
	if got := sanitizeToolName(long); len(got) != maxToolNameBytes {
		t.Errorf("sanitizeToolName(len=100) len = %d, want %d", len(got), maxToolNameBytes)
	}
	if maxToolNameBytes > 64 {
		t.Fatalf("maxToolNameBytes = %d exceeds the provider ceiling of 64", maxToolNameBytes)
	}
	// Shortening retains the TAIL, not the head: sibling tools of one
	// source share a long prefix, so the tail is what distinguishes them.
	if got := sanitizeToolName(strings.Repeat("x", 80) + "_verb"); !strings.Contains(got, "_verb_") {
		t.Errorf("sanitizeToolName dropped the tail: got %q, want it to retain %q", got, "_verb")
	}
}

// TestBuildToolDeclarations_SanitizesDottedCatalogNames is the regression
// guard for the live bug: a dotted catalog tool was declared to the LLM
// verbatim, so a real provider 400'd. The declaration MUST carry the
// sanitized name (the scripted-LLM tests missed this because mocks bypass
// provider name-validation — CLAUDE.md §17.8).
func TestBuildToolDeclarations_SanitizesDottedCatalogNames(t *testing.T) {
	rc := planner.RunContext{
		Catalog: &sanitizeStubCatalog{list: []tools.Tool{
			{Name: "inventory.check", ArgsSchema: json.RawMessage(`{"type":"object"}`)},
		}},
	}
	decls := buildToolDeclarations(rc, nil)
	var found bool
	for _, d := range decls {
		if strings.Contains(d.Name, ".") {
			t.Errorf("declaration name %q contains a dot — a real provider rejects it", d.Name)
		}
		if d.Name == "inventory_check" {
			found = true
		}
		if d.Name == "inventory.check" {
			t.Errorf("declaration still carries the raw dotted name %q", d.Name)
		}
	}
	if !found {
		t.Fatalf("no sanitized 'inventory_check' declaration emitted; got %d decls", len(decls))
	}
}

// TestProjectResponse_ResolvesSanitizedNameToCatalog closes the loop: when
// the provider returns the sanitized name, the projector resolves it back
// to the real (dotted) catalog name so dispatch hits the registered tool.
func TestProjectResponse_ResolvesSanitizedNameToCatalog(t *testing.T) {
	rc := &planner.RunContext{
		Catalog: &sanitizeStubCatalog{list: []tools.Tool{
			{Name: "inventory.check", ArgsSchema: json.RawMessage(`{"type":"object"}`)},
		}},
	}
	resp := llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{
		ID:   "c1",
		Name: "inventory_check", // the sanitized name the LLM saw + returned
		Args: json.RawMessage(`{"sku":"SKU-42"}`),
	}}}
	dec, err := projectResponse(resp, rc, true)
	if err != nil {
		t.Fatalf("projectResponse: %v", err)
	}
	ct, ok := dec.(planner.CallTool)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallTool", dec)
	}
	if ct.Tool != "inventory.check" {
		t.Errorf("CallTool.Tool = %q, want the resolved catalog name %q", ct.Tool, "inventory.check")
	}
}

func TestProjectResponse_RejectsRawCatalogKeyBeforeDispatch(t *testing.T) {
	rc := &planner.RunContext{
		Catalog: &sanitizeStubCatalog{list: []tools.Tool{{Name: "clock.now"}}},
	}

	declared := llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{
		ID: "declared", Name: "clock_now", Args: json.RawMessage(`{}`),
	}}}
	dec, err := projectResponse(declared, rc, true)
	if err != nil {
		t.Fatalf("declared name rejected: %v", err)
	}
	call, ok := dec.(planner.CallTool)
	if !ok || call.Tool != "clock.now" {
		t.Fatalf("declared name projected to %#v, want CallTool{Tool:clock.now}", dec)
	}

	// Positive precondition for the adversarial shape: the raw key really is
	// present in the catalog and would dispatch if it escaped the projector.
	if _, ok := rc.Catalog.Resolve("clock.now"); !ok {
		t.Fatal("fixture catalog does not resolve raw key clock.now")
	}
	raw := llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{
		ID: "raw", Name: "clock.now", Args: json.RawMessage(`{}`),
	}}}
	if dec, err := projectResponse(raw, rc, true); dec != nil || !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("raw catalog key projected to (%#v, %v), want nil + ErrInvalidDecision", dec, err)
	}
}

// TestResolveDeclaredToolName covers the strict namespace boundary: declared
// names resolve, while raw catalog keys and unknown names are refused before
// they can reach catalog dispatch.
func TestResolveDeclaredToolName(t *testing.T) {
	rc := &planner.RunContext{
		Catalog: &sanitizeStubCatalog{list: []tools.Tool{
			{Name: "clock.now"},
			{Name: "mcptest_echo"},
		}},
	}
	cases := []struct{ in, want string }{
		{"clock_now", "clock.now"},       // sanitize-match → dotted real
		{"mcptest_echo", "mcptest_echo"}, // exact match
	}
	for _, c := range cases {
		if got := mustResolveDeclaredToolName(t, rc, c.in); got != c.want {
			t.Errorf("resolveDeclaredToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, refused := range []string{"clock.now", "unknown_tool"} {
		if got, ok := resolveDeclaredToolName(rc, refused); ok {
			t.Errorf("resolveDeclaredToolName(%q) = %q, true; raw or unknown model vocabulary must be refused", refused, got)
		}
	}
}
