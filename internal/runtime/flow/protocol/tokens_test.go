package protocol

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/flow"
)

// TestBudgetConsumption_SumsTokensUsed is the flows prod-wiring test named
// by the projection-completeness contract (Half B): the PRODUCTION
// budgetConsumption aggregate sums the per-run Tokens into
// FlowBudgetConsumption.TokensUsed. A regression that drops the sum (the
// original "0 tokens used" bug) fails here, and the gate's Half-A probe
// flags the zero field (D-313).
func TestBudgetConsumption_SumsTokensUsed(t *testing.T) {
	since := time.Unix(0, 0).UTC()
	runs := []flow.RunRecord{
		{
			StartedAt:  since.Add(time.Second),
			NodeStates: []flow.NodeRunRecord{{NodeID: "a"}},
			CostUSD:    1.0,
			Tokens:     100,
		},
		{
			StartedAt:  since.Add(2 * time.Second),
			NodeStates: []flow.NodeRunRecord{{NodeID: "a"}, {NodeID: "b"}},
			CostUSD:    2.0,
			Tokens:     50,
		},
		{
			// Older than `since` — excluded from the window.
			StartedAt: since.Add(-time.Hour),
			Tokens:    9999,
		},
	}
	c := budgetConsumption(runs, since)
	if c.TokensUsed != 150 {
		t.Fatalf("TokensUsed = %d, want 150 (summed over the two in-window runs)", c.TokensUsed)
	}
	if c.CostUSDUsed != 3.0 {
		t.Fatalf("CostUSDUsed = %v, want 3.0 (symmetry check)", c.CostUSDUsed)
	}
	if c.RequestsUsed != 3 {
		t.Fatalf("RequestsUsed = %d, want 3", c.RequestsUsed)
	}
}
