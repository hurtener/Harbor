package protocol

import (
	"slices"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestMemoryBudgetCapabilityRequiresBothConsumers(t *testing.T) {
	for _, agentConfig := range []bool{false, true} {
		for _, compactor := range []bool{false, true} {
			caps := wiredCapabilitiesFor(false, agentConfig, false, false, false, false, false, false, false, compactor)
			if got := slices.Contains(caps, types.CapAgentConfigMemory); got != (agentConfig && compactor) {
				t.Fatalf("agent_config=%v compactor=%v: %v", agentConfig, compactor, caps)
			}
		}
	}
}
