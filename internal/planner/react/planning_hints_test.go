package react

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// ptr is a tiny generic pointer helper for the optional BudgetHints
// fields.
func ptr[T any](v T) *T { return &v }

// TestRenderPlanningHints_NilAndEmptyOmitSection asserts a nil hints
// pointer and an all-empty PlanningHints both render the empty string
// — so the prompt builder omits the <planning_constraints> section.
func TestRenderPlanningHints_NilAndEmptyOmitSection(t *testing.T) {
	t.Parallel()
	if got := renderPlanningHints(nil); got != "" {
		t.Errorf("renderPlanningHints(nil) = %q, want empty", got)
	}
	if got := renderPlanningHints(&planner.PlanningHints{}); got != "" {
		t.Errorf("renderPlanningHints(empty) = %q, want empty", got)
	}
	// A PlanningHints whose slices are all-whitespace also renders
	// nothing — nonEmpty drops blank entries.
	blank := &planner.PlanningHints{
		Constraints:    "   ",
		PreferredOrder: []string{"", "  "},
		DisallowTools:  []string{""},
	}
	if got := renderPlanningHints(blank); got != "" {
		t.Errorf("renderPlanningHints(blank) = %q, want empty", got)
	}
}

// TestRenderPlanningHints_FullHints asserts every field renders into
// the section with the expected tag pair.
func TestRenderPlanningHints_FullHints(t *testing.T) {
	t.Parallel()
	h := &planner.PlanningHints{
		Constraints:    "stay within the finance domain",
		PreferredOrder: []string{"search", "summarise"},
		ParallelGroups: [][]string{{"fetch_a", "fetch_b"}, {"fetch_c"}},
		DisallowTools:  []string{"delete_account"},
		PreferredTools: []string{"read_ledger"},
		Budget: &planner.BudgetHints{
			MaxSteps:     ptr(8),
			MaxCostUSD:   ptr(1.5),
			MaxLatencyMS: ptr[int64](30000),
		},
	}
	got := renderPlanningHints(h)

	if !strings.HasPrefix(got, "<planning_constraints>\n") ||
		!strings.HasSuffix(got, "\n</planning_constraints>") {
		t.Fatalf("section not wrapped in tag pair:\n%s", got)
	}
	wantSubstrings := []string{
		"Constraints: stay within the finance domain",
		"Preferred tool order: search -> summarise",
		"Parallel groups",
		"  - fetch_a, fetch_b",
		"  - fetch_c",
		"Disallowed tools (do NOT call): delete_account",
		"Preferred tools: read_ledger",
		"Budget — max steps: 8",
		"Budget — max cost: $1.5 USD",
		"Budget — max latency: 30000 ms",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("rendered section missing %q\n--- full ---\n%s", s, got)
		}
	}
}

// TestRenderPlanningHints_PartialOmitsAbsentFields asserts a partial
// PlanningHints renders only its set fields — no blank lines for the
// absent ones.
func TestRenderPlanningHints_PartialOmitsAbsentFields(t *testing.T) {
	t.Parallel()
	h := &planner.PlanningHints{
		DisallowTools: []string{"dangerous_tool"},
	}
	got := renderPlanningHints(h)
	if !strings.Contains(got, "Disallowed tools (do NOT call): dangerous_tool") {
		t.Fatalf("missing disallow line:\n%s", got)
	}
	for _, absent := range []string{"Constraints:", "Preferred tool order:", "Parallel groups", "Budget —"} {
		if strings.Contains(got, absent) {
			t.Errorf("partial render leaked absent field %q:\n%s", absent, got)
		}
	}
	// No empty lines inside the body.
	body := strings.TrimPrefix(got, "<planning_constraints>\n")
	body = strings.TrimSuffix(body, "\n</planning_constraints>")
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			t.Errorf("partial render produced an empty line:\n%s", got)
		}
	}
}

// TestRenderPlanningHints_PartialBudget asserts a BudgetHints with
// only some pointers set renders only those dimensions.
func TestRenderPlanningHints_PartialBudget(t *testing.T) {
	t.Parallel()
	h := &planner.PlanningHints{
		Budget: &planner.BudgetHints{MaxSteps: ptr(5)},
	}
	got := renderPlanningHints(h)
	if !strings.Contains(got, "Budget — max steps: 5") {
		t.Fatalf("missing max-steps line:\n%s", got)
	}
	if strings.Contains(got, "max cost") || strings.Contains(got, "max latency") {
		t.Errorf("partial budget leaked an unset dimension:\n%s", got)
	}
}

// TestRenderPlanningConstraints_BridgesRunContext asserts the prompt-
// side wrapper reads RunContext.PlanningHints.
func TestRenderPlanningConstraints_BridgesRunContext(t *testing.T) {
	t.Parallel()
	rcNil := planner.RunContext{}
	if got := renderPlanningConstraints(rcNil); got != "" {
		t.Errorf("nil PlanningHints: got %q, want empty", got)
	}
	rc := planner.RunContext{
		PlanningHints: &planner.PlanningHints{Constraints: "be terse"},
	}
	if got := renderPlanningConstraints(rc); !strings.Contains(got, "be terse") {
		t.Errorf("PlanningHints not bridged: %q", got)
	}
}
