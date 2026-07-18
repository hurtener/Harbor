package llm

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// TestEmitCostRecorded_CacheTokensRoundTrip asserts the two cache-token
// counts ride the embedded Usage through emitCostRecorded → Publish → the
// subscriber unchanged. emitCostRecorded needs no code change to forward
// them (it copies resp.Usage verbatim); this test pins that they survive
// the round trip so a future refactor cannot silently drop them.
func TestEmitCostRecorded_CacheTokensRoundTrip(t *testing.T) {
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         100,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-cache-rt")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	quad := identityQuad(ctx)

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{EventTypeCostRecorded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	usage := Usage{
		PromptTokens:     1000,
		CompletionTokens: 200,
		TotalTokens:      1200,
		CacheReadTokens:  800,
		CacheWriteTokens: 150,
	}
	emitCostRecorded(ctx, bus, quad, "m", Cost{TotalCost: 0.01}, usage, 4096)

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(CostRecordedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want CostRecordedPayload", ev.Payload)
		}
		if p.Usage.CacheReadTokens != 800 {
			t.Errorf("Usage.CacheReadTokens = %d want 800", p.Usage.CacheReadTokens)
		}
		if p.Usage.CacheWriteTokens != 150 {
			t.Errorf("Usage.CacheWriteTokens = %d want 150", p.Usage.CacheWriteTokens)
		}
		// The base counts are untouched by the cache split.
		if p.Usage.PromptTokens != 1000 || p.Usage.TotalTokens != 1200 {
			t.Errorf("base Usage altered: %+v", p.Usage)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no llm.cost.recorded event observed within 2s")
	}
}
