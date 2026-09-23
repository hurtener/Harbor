package projection

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
)

// ActiveMemoryBudget freezes the agent's working-input target for one run.
// An absent section inherits YAML; explicit zero uses automatic model sizing.
// The value is copied, so later revisions cannot mutate an in-flight run.
func ActiveMemoryBudget(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, yamlBudget int, compactorAvailable bool) (int, error) {
	if reg == nil || agentID == "" {
		return yamlBudget, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return 0, err
	}
	if !ok || rev.Payload.Memory == nil {
		return yamlBudget, nil
	}
	if !compactorAvailable || rev.Payload.Memory.BudgetTokens < 0 {
		return 0, fmt.Errorf("memory budget requires a configured compactor and nonnegative budget_tokens")
	}
	return rev.Payload.Memory.BudgetTokens, nil
}
