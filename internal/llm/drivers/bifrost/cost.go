package bifrost

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// emitCostRecorded publishes the `llm.cost.recorded` event after a
// successful Complete. This is an OBSERVABILITY emit only: the governance
// accumulator does NOT subscribe against it — cost accounting is in-band
// synchronous (the accumulator folds each call's cost, and every
// intermediate retry / downgrade attempt via the per-call attempt-cost
// tap, in its PostCall), so the next ceiling check sees the latest total
// without a bus-delivery race. The event drives dashboards and replay
// tooling; it is not the accounting path (per the V1 scoping: latent
// governance, opt-in via config).
//
// Best-effort — never blocks the request path on the bus. A nil bus
// (e.g. tests that construct the Driver directly) is a no-op. Cost
// emit fires even when `cost.TotalCost == 0` because some providers
// don't report cost at all (the validation report's OpenRouter route
// recorded 6/6 cost values; the test stubs may not — the accumulator
// can still record token usage in that case).
//
// The payload is `events.SafePayload` (composes `events.SafeSealed`):
// cost figures and token counts are operator-visible, not secret-
// shaped.
func emitCostRecorded(ctx context.Context, bus events.EventBus, id identity.Quadruple, model string, cost llm.Cost, usage llm.Usage, contextWindow int) {
	if bus == nil {
		return
	}
	now := time.Now()
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort cost-telemetry emit — must not block the LLM call path.
		Type:       llm.EventTypeCostRecorded,
		Identity:   id,
		OccurredAt: now,
		Payload: llm.CostRecordedPayload{
			Identity:            id,
			Model:               model,
			Cost:                cost,
			Usage:               usage,
			ContextWindowTokens: contextWindow,
			OccurredAt:          now,
		},
	})
}
