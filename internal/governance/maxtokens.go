package governance

import (
	"context"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
)

// MaxTokensEnforcer is the Phase 36b Subsystem that gates `req.MaxTokens`
// against the resolved identity tier's cap. Stateless — no StateStore
// dependency. Fail-loud per master plan line 420 + RFC §6.15 line 1122.
//
// Latent default: with `TierConfig.MaxTokens == 0` for the resolved tier,
// PreCall is a permit no-op. Same with an unresolved tier.
//
// Concurrency: the enforcer holds no mutable state beyond `Config`; D-025
// is trivially satisfied. Operators may share one enforcer across the
// entire runtime.
type MaxTokensEnforcer struct {
	bus    events.EventBus
	clock  Clock
	cfg    Config
	closed atomic.Bool
}

// NewMaxTokensEnforcer constructs the enforcer. The bus is required for
// `governance.maxtokens_exceeded` emit. Nil bus rejected.
func NewMaxTokensEnforcer(bus events.EventBus, cfg Config) *MaxTokensEnforcer {
	if bus == nil {
		panic("governance.NewMaxTokensEnforcer: nil bus")
	}
	return &MaxTokensEnforcer{cfg: cfg, bus: bus, clock: cfg.clock()}
}

// PreCall checks `req.MaxTokens` against the tier cap. Permits if either
// is unset / zero.
func (e *MaxTokensEnforcer) PreCall(ctx context.Context, req llm.CompleteRequest) error {
	if e.closed.Load() {
		return ErrClosed
	}
	if req.MaxTokens == nil || *req.MaxTokens <= 0 {
		return nil
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}
	tier, ok := e.cfg.tierConfig(id)
	if !ok || tier.MaxTokens <= 0 {
		return nil
	}
	if *req.MaxTokens <= tier.MaxTokens {
		return nil
	}
	quad, qErr := quadrupleFromCtx(ctx)
	if qErr != nil {
		// Should not happen — identityFromCtx already validated.
		return qErr
	}
	tierName := e.cfg.resolveTier(id)
	now := e.clock.Now()
	_ = e.bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort event emit; publish failure must not fail the MaxTokens check
		Type:       EventTypeMaxTokensExceeded,
		Identity:   quad,
		OccurredAt: now,
		Payload: MaxTokensExceededPayload{
			Identity:   quad,
			Tier:       tierName,
			Model:      req.Model,
			Requested:  *req.MaxTokens,
			Cap:        tier.MaxTokens,
			OccurredAt: now,
		},
	})
	return errorWith(ErrMaxTokensExceeded,
		"identity=%s/%s/%s model=%q requested=%d cap=%d",
		id.TenantID, id.UserID, id.SessionID, req.Model, *req.MaxTokens, tier.MaxTokens)
}

// PostCall is a no-op. MaxTokens enforcement is a pre-call gate only.
func (e *MaxTokensEnforcer) PostCall(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error {
	return nil
}

// Close marks the enforcer closed.
func (e *MaxTokensEnforcer) Close() error {
	e.closed.Store(true)
	return nil
}

var _ Subsystem = (*MaxTokensEnforcer)(nil)
