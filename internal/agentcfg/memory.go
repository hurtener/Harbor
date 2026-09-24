package agentcfg

import "strconv"

// MemorySection overrides the YAML working-input budget for subsequent runs.
// Zero selects the effective model's safe input target; nil inherits YAML.
// This is not an output, spend, or persisted-evidence byte limit.
type MemorySection struct {
	BudgetTokens int `json:"budget_tokens"`
}

// MemoryDiff distinguishes inheritance (empty) from automatic budgeting ("0").
type MemoryDiff struct {
	BudgetTokensChanged bool
	BudgetTokensFrom    string
	BudgetTokensTo      string
}

// Changed reports whether the budget or its inheritance policy changed.
func (d MemoryDiff) Changed() bool { return d.BudgetTokensChanged }

// DiffMemory compares the explicit budgets, preserving section presence.
func DiffMemory(from, to ConfigPayload) MemoryDiff {
	f, t := "", ""
	if from.Memory != nil {
		f = strconv.Itoa(from.Memory.BudgetTokens)
	}
	if to.Memory != nil {
		t = strconv.Itoa(to.Memory.BudgetTokens)
	}
	return MemoryDiff{BudgetTokensChanged: f != t, BudgetTokensFrom: f, BudgetTokensTo: t}
}
