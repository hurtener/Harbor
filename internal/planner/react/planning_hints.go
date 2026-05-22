package react

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hurtener/Harbor/internal/planner"
)

// renderPlanningHints renders the `<planning_constraints>` section
// body from a [planner.PlanningHints] (Phase 83c — D-145). It returns
// the empty string when `h` is nil or carries no content, so the
// prompt builder omits the optional section entirely (brief 13 §2.1
// design property: optional sections are omitted, never emitted as
// empty tag pairs).
//
// Every field is rendered only when non-empty — a partial PlanningHints
// produces a partial section with no blank lines for the absent
// fields. The render is a pure function of `h`: no mutation, no
// package state, safe under the D-025 concurrent-reuse contract.
//
// The returned string is the FULL section including the
// `<planning_constraints>` / `</planning_constraints>` tag pair, so
// the caller appends it verbatim to the section list.
func renderPlanningHints(h *planner.PlanningHints) string {
	if h == nil {
		return ""
	}

	var lines []string

	if c := strings.TrimSpace(h.Constraints); c != "" {
		lines = append(lines, "Constraints: "+oneLine(c))
	}
	if order := nonEmpty(h.PreferredOrder); len(order) > 0 {
		lines = append(lines, "Preferred tool order: "+strings.Join(order, " -> "))
	}
	if groups := renderParallelGroups(h.ParallelGroups); len(groups) > 0 {
		lines = append(lines, "Parallel groups (tools within a group may run concurrently):")
		lines = append(lines, groups...)
	}
	if disallow := nonEmpty(h.DisallowTools); len(disallow) > 0 {
		lines = append(lines, "Disallowed tools (do NOT call): "+strings.Join(disallow, ", "))
	}
	if prefer := nonEmpty(h.PreferredTools); len(prefer) > 0 {
		lines = append(lines, "Preferred tools: "+strings.Join(prefer, ", "))
	}
	if budget := renderBudgetHints(h.Budget); len(budget) > 0 {
		lines = append(lines, budget...)
	}

	if len(lines) == 0 {
		return ""
	}
	return "<planning_constraints>\n" + strings.Join(lines, "\n") + "\n</planning_constraints>"
}

// renderParallelGroups renders each non-empty parallel group as an
// indented bullet. Empty groups (and empty tool names within a group)
// are dropped. The result is empty (len 0) when no group has content;
// the caller len-checks before emitting the section header.
func renderParallelGroups(groups [][]string) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		members := nonEmpty(g)
		if len(members) == 0 {
			continue
		}
		out = append(out, "  - "+strings.Join(members, ", "))
	}
	return out
}

// renderBudgetHints renders the advisory budget caps. Each cap is a
// pointer; nil pointers are skipped so a partial BudgetHints renders
// only the dimensions it pins. Returns nil when no cap is set.
func renderBudgetHints(b *planner.BudgetHints) []string {
	if b == nil {
		return nil
	}
	var out []string
	if b.MaxSteps != nil {
		out = append(out, "Budget — max steps: "+strconv.Itoa(*b.MaxSteps))
	}
	if b.MaxCostUSD != nil {
		out = append(out, "Budget — max cost: "+
			fmt.Sprintf("$%s USD", strconv.FormatFloat(*b.MaxCostUSD, 'f', -1, 64)))
	}
	if b.MaxLatencyMS != nil {
		out = append(out, "Budget — max latency: "+
			strconv.FormatInt(*b.MaxLatencyMS, 10)+" ms")
	}
	return out
}

// nonEmpty returns a copy of `in` with empty / whitespace-only entries
// dropped and the rest trimmed. Returns nil when nothing survives —
// so a caller can `len()`-check the result to decide whether to emit
// a line at all.
func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}
