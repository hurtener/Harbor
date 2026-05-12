package governance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/llm"
)

func TestMaxTokens_LatentPermitsAllValues(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	e := governance.NewMaxTokensEnforcer(bus, governance.Config{})
	ctx := ctxWith(t, "T", "U", "S", "R")
	huge := 1_000_000
	req := llm.CompleteRequest{Model: "m", MaxTokens: &huge}
	if err := e.PreCall(ctx, req); err != nil {
		t.Errorf("PreCall under latent default returned: %v", err)
	}
}

func TestMaxTokens_BlocksOverCap(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}
	e := governance.NewMaxTokensEnforcer(bus, cfg)
	ctx := ctxWith(t, "T", "U", "S", "R")

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "T", User: "U", Session: "S",
		Types: []events.EventType{governance.EventTypeMaxTokensExceeded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Under cap: permits.
	under := 50
	if err := e.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &under}); err != nil {
		t.Errorf("under cap PreCall: %v", err)
	}
	// At cap: permits (≤ cap).
	at := 100
	if err := e.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &at}); err != nil {
		t.Errorf("at cap PreCall: %v", err)
	}
	// Over cap: fail loud.
	over := 200
	err = e.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &over})
	if !errors.Is(err, governance.ErrMaxTokensExceeded) {
		t.Fatalf("over cap PreCall: want ErrMaxTokensExceeded, got %v", err)
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(governance.MaxTokensExceededPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Requested != 200 || p.Cap != 100 {
			t.Errorf("payload = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not observe maxtokens_exceeded event within 2s")
	}
}

func TestMaxTokens_NilOrZeroRequestedPermits(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}
	e := governance.NewMaxTokensEnforcer(bus, cfg)
	ctx := ctxWith(t, "T", "U", "S", "R")
	// nil MaxTokens — permits.
	if err := e.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Errorf("nil MaxTokens PreCall: %v", err)
	}
	// Zero MaxTokens — permits.
	zero := 0
	if err := e.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &zero}); err != nil {
		t.Errorf("zero MaxTokens PreCall: %v", err)
	}
}

func TestMaxTokens_FailsClosedOnMissingIdentity(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}
	e := governance.NewMaxTokensEnforcer(bus, cfg)
	over := 200
	err := e.PreCall(context.Background(), llm.CompleteRequest{Model: "m", MaxTokens: &over})
	if !errors.Is(err, governance.ErrIdentityRequired) {
		t.Errorf("missing identity: want ErrIdentityRequired, got %v", err)
	}
}

func TestMaxTokens_PostCallNoop(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	e := governance.NewMaxTokensEnforcer(bus, governance.Config{})
	ctx := ctxWith(t, "T", "U", "S", "R")
	if err := e.PostCall(ctx, llm.CompleteRequest{}, llm.CompleteResponse{}, nil); err != nil {
		t.Errorf("PostCall: %v", err)
	}
}
