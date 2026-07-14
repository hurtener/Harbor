package protocol

import (
	"time"

	"github.com/hurtener/Harbor/internal/protocol/projectioncheck"
	"github.com/hurtener/Harbor/internal/runtime/flow"
)

// init self-registers the flows projection surface into the
// projection-completeness gate (§4.4). The gate proves the flows
// Budget aggregate never sums a wire field the projector leaves zero — the
// exact `tokens_used`-always-zero bug this phase closed.
func init() {
	projectioncheck.Register(projectioncheck.ProjectionContract{
		Surface: "flows",
		// Probe runs the PRODUCTION budgetConsumption aggregate over a
		// fully-populated run record — one NodeState (requests_used),
		// non-zero CostUSD (cost_usd_used), non-zero Tokens (tokens_used) —
		// and returns the projected FlowBudgetConsumption the gate reflects.
		Probe: func() any {
			since := time.Unix(0, 0).UTC()
			runs := []flow.RunRecord{{
				StartedAt:  since.Add(time.Second),
				NodeStates: []flow.NodeRunRecord{{NodeID: "n0", Status: "succeeded"}},
				CostUSD:    1.5,
				Tokens:     4096,
			}}
			return budgetConsumption(runs, since)
		},
		// The three budget-consumption axes the Flows Budget meter renders.
		OperatedFields: []string{"requests_used", "cost_usd_used", "tokens_used"},
		ProdWiringTest: "TestBudgetConsumption_SumsTokensUsed",
	})
}
