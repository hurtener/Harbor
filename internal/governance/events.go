package governance

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Phase 36a/36b governance event types. Registered via init() so the
// canonical events registry stays the single source of truth (AGENTS.md
// §17.6's "wiring gap" lesson — register at declaration time, publish at
// use time).
//
// All payloads are SafePayload (compose events.SafeSealed): identity,
// model, ceilings, and totals are operator-visible metadata, not
// secret-shaped. Cost values are USD floats — same wire shape as
// `llm.CostRecordedPayload`.
const (
	// EventTypeBudgetExceeded — emitted from `CostAccumulator.PreCall`
	// when the per-identity total cost meets or exceeds the configured
	// tier ceiling.
	EventTypeBudgetExceeded events.EventType = "governance.budget_exceeded"

	// EventTypeRateLimited — emitted from `RateLimiter.PreCall` when
	// the token bucket for the request's (identity, model) underflows
	// the requested drain.
	EventTypeRateLimited events.EventType = "governance.rate_limited"

	// EventTypeMaxTokensExceeded — emitted from
	// `MaxTokensEnforcer.PreCall` when `req.MaxTokens` exceeds the
	// resolved tier's cap.
	EventTypeMaxTokensExceeded events.EventType = "governance.maxtokens_exceeded"
)

func init() {
	for _, t := range []events.EventType{
		EventTypeBudgetExceeded,
		EventTypeRateLimited,
		EventTypeMaxTokensExceeded,
	} {
		events.RegisterEventType(t)
	}
}

// BudgetExceededPayload is the typed payload for EventTypeBudgetExceeded.
// SafePayload — identity + cost figures + tier name are operator-visible.
//
// `TotalCost` is the per-identity sum at the time of the block (i.e. the
// pre-call running total; the rejected call did not contribute).
// `Ceiling` is the operator-configured cap at the time of the emit.
// `Model` carries the request's model identifier; `Tier` is the resolved
// tier name.
type BudgetExceededPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Tier       string
	Model      string
	TotalCost  float64
	Ceiling    float64
	Currency   string
	OccurredAt time.Time
}

// RateLimitedPayload is the typed payload for EventTypeRateLimited.
// `Requested` is the number of tokens the drain attempted to remove;
// `Available` is the bucket's level at the moment of the block.
type RateLimitedPayload struct {
	events.SafeSealed
	Identity     identity.Quadruple
	Tier         string
	Model        string
	Requested    int
	Available    int
	Capacity     int
	RefillTokens int
	RefillEvery  time.Duration
	OccurredAt   time.Time
}

// MaxTokensExceededPayload is the typed payload for
// EventTypeMaxTokensExceeded. `Requested` is the value on
// `CompleteRequest.MaxTokens`; `Cap` is the tier's configured ceiling.
type MaxTokensExceededPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Tier       string
	Model      string
	Requested  int
	Cap        int
	OccurredAt time.Time
}
