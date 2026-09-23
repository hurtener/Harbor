package types

// AgentConfigMemory sets the working-input compaction target, not model output
// or storage size. Values must be nonnegative; zero requests automatic sizing.
type AgentConfigMemory struct {
	BudgetTokens int `json:"budget_tokens"`
}

// AgentConfigMemoryDiff compares explicit budget values. Empty strings mean
// inheritance from YAML; "0" means automatic sizing, not disabled compaction.
type AgentConfigMemoryDiff struct {
	BudgetTokensChanged bool   `json:"budget_tokens_changed"`
	BudgetTokensFrom    string `json:"budget_tokens_from"`
	BudgetTokensTo      string `json:"budget_tokens_to"`
}
