package flow

import (
	"github.com/hurtener/Harbor/internal/events"
)

// Phase 26a flow event types.
const (
	// EventTypeFlowBudgetExceeded — emitted by invokeFlow when
	// any axis of the per-call Budget accumulator fires
	// (deadline / hop_budget / cost_cap). Carries the flow name
	// + the triggering axis. SafePayload.
	EventTypeFlowBudgetExceeded events.EventType = "flow.budget_exceeded"
)

func init() {
	events.RegisterEventType(EventTypeFlowBudgetExceeded)
}

// BudgetExceededPayload is the typed payload for
// EventTypeFlowBudgetExceeded.
//
// Axis names: "deadline", "hop_budget", "cost_cap".
type BudgetExceededPayload struct {
	events.SafeSealed
	FlowName string
	Axis     string
}
