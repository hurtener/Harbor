package agentcfg_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

func TestMemoryBudgetPresenceAndClone(t *testing.T) {
	p := agentcfg.ConfigPayload{Memory: &agentcfg.MemorySection{BudgetTokens: 64000}}
	clone := agentcfg.NormalizePayload(p)
	p.Memory.BudgetTokens = 1
	if clone.Memory == nil || clone.Memory.BudgetTokens != 64000 {
		t.Fatal("normalization aliased the budget")
	}
	auto := agentcfg.ConfigPayload{Memory: &agentcfg.MemorySection{}}
	if agentcfg.NormalizePayload(auto).Memory == nil {
		t.Fatal("automatic budget was dropped")
	}
	d := agentcfg.DiffMemory(agentcfg.ConfigPayload{}, auto)
	if !d.Changed() || d.BudgetTokensFrom != "" || d.BudgetTokensTo != "0" {
		t.Fatalf("inherit/automatic diff: %+v", d)
	}
	if agentcfg.DiffMemory(clone, clone).Changed() {
		t.Fatal("identical budget changed")
	}
}
