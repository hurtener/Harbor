package governance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

// failingStateStore is a state.StateStore stub that always errors on
// Load. Wave 7b audit FAIL #2: pins the production code's no-silent-
// permit promise — when StateStore.Load fails at PreCall, the
// accumulator + rate-limiter MUST return wrapped ErrStateUnavailable
// rather than permitting silently.
type failingStateStore struct{}

var errStateProbe = errors.New("io: simulated state read failure")

func (failingStateStore) Save(_ context.Context, _ state.StateRecord) error {
	return errStateProbe
}

func (failingStateStore) Load(_ context.Context, _ identity.Quadruple, _ string) (state.StateRecord, error) {
	return state.StateRecord{}, errStateProbe
}

func (failingStateStore) LoadByEventID(_ context.Context, _ state.EventID) (state.StateRecord, error) {
	return state.StateRecord{}, errStateProbe
}

func (failingStateStore) Delete(_ context.Context, _ identity.Quadruple, _ string) error {
	return errStateProbe
}

func (failingStateStore) Close(_ context.Context) error { return nil }

func openBusForStateFailTest(t *testing.T) (events.EventBus, func()) {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	return bus, func() { _ = bus.Close(context.Background()) }
}

// TestCostAccumulator_PreCall_FailsLoudOnStateReadError pins the
// no-silent-permit invariant: when the StateStore.Load fails on
// PreCall lookup, the accumulator MUST return a wrapped
// ErrStateUnavailable rather than permit. AGENTS.md §13 forbids
// silent degradation.
func TestCostAccumulator_PreCall_FailsLoudOnStateReadError(t *testing.T) {
	t.Parallel()
	bus, cleanup := openBusForStateFailTest(t)
	defer cleanup()

	cfg := governance.Config{
		DefaultTier: "tier",
		IdentityTiers: map[string]governance.TierConfig{
			"tier": {BudgetCeilingUSD: 1.0},
		},
	}
	acc, err := governance.NewCostAccumulator(failingStateStore{}, bus, cfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer func() { _ = acc.Close(context.Background()) }()

	ctx := ctxWith(t, "T", "U", "S", "R")
	err = acc.PreCall(ctx, llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, governance.ErrStateUnavailable) {
		t.Errorf("PreCall on failing state: got %v, want ErrStateUnavailable", err)
	}
}

// TestRateLimiter_PreCall_FailsLoudOnStateReadError pins the same
// invariant for the rate-limiter's bucket-state lookup path.
func TestRateLimiter_PreCall_FailsLoudOnStateReadError(t *testing.T) {
	t.Parallel()
	bus, cleanup := openBusForStateFailTest(t)
	defer cleanup()

	cfg := governance.Config{
		DefaultTier: "tier",
		IdentityTiers: map[string]governance.TierConfig{
			"tier": {
				RateLimit: governance.RateLimitConfig{
					Capacity:       100,
					RefillTokens:   10,
					RefillInterval: time.Second,
				},
			},
		},
	}
	rl, err := governance.NewRateLimiter(failingStateStore{}, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer func() { _ = rl.Close(context.Background()) }()

	ctx := ctxWith(t, "T", "U", "S", "R")
	err = rl.PreCall(ctx, llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, governance.ErrStateUnavailable) {
		t.Errorf("PreCall on failing state: got %v, want ErrStateUnavailable", err)
	}
}
