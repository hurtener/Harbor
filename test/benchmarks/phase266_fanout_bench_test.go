package benchmarks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
)

func phase266FanoutConfig(maxSubscribers int) config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: maxSubscribers + 1,
		SubscriberBufferSize:     1,
		IdleTimeout:              time.Hour,
		DropWindow:               time.Second,
		ReplayBufferSize:         0,
	}
}

// BenchmarkPhase266FanOutIdentityIsolation measures the candidate indexed
// fan-out path with unrelated subscribers at 1K and 10K scale. Every
// subscriber has a distinct identity; each publish targets only the final
// identity. A flat scan can appear correct while still degrading linearly,
// so this benchmark is kept separate from the existing 1/8/16 same-session
// fan-out benchmark and reports the unrelated subscriber cardinality.
//
// The real audit redactor and real in-memory EventBus are used. No fake bus or
// reimplemented filter is involved; the non-target channels are checked after
// the timed loop to catch identity-isolation regressions as well.
func BenchmarkPhase266FanOutIdentityIsolation(b *testing.B) {
	for _, subscriberCount := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("subscribers=%d", subscriberCount), func(b *testing.B) {
			red, err := audit.Open(context.Background(), config.AuditConfig{})
			if err != nil {
				b.Fatalf("audit.Open: %v", err)
			}
			bus, err := events.Open(context.Background(), phase266FanoutConfig(subscriberCount), red)
			if err != nil {
				b.Fatalf("events.Open: %v", err)
			}
			b.Cleanup(func() { _ = bus.Close(context.Background()) })

			target := identity.Quadruple{Identity: identity.Identity{
				TenantID:  "phase266-target-tenant",
				UserID:    "phase266-target-user",
				SessionID: "phase266-target-session",
			}}
			type channelRef struct {
				id identity.Quadruple
				ch <-chan events.Event
			}
			others := make([]channelRef, 0, subscriberCount-1)
			var targetCh <-chan events.Event
			for i := range subscriberCount {
				id := identity.Quadruple{Identity: identity.Identity{
					TenantID:  fmt.Sprintf("phase266-tenant-%d", i),
					UserID:    fmt.Sprintf("phase266-user-%d", i),
					SessionID: fmt.Sprintf("phase266-session-%d", i),
				}}
				if i == subscriberCount-1 {
					id = target
				}
				sub, err := bus.Subscribe(context.Background(), events.Filter{
					Tenant:  id.TenantID,
					User:    id.UserID,
					Session: id.SessionID,
				})
				if err != nil {
					b.Fatalf("Subscribe(%d): %v", i, err)
				}
				b.Cleanup(sub.Cancel)
				if i == subscriberCount-1 {
					targetCh = sub.Events()
				} else {
					others = append(others, channelRef{id: id, ch: sub.Events()})
				}
			}

			ev := events.Event{
				Type:     events.EventTypeRuntimeWarning,
				Identity: target,
				Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: 1},
			}
			b.ReportMetric(float64(subscriberCount), "subscribers")
			b.ResetTimer()
			for range b.N {
				if err := bus.Publish(context.Background(), ev); err != nil {
					b.Fatalf("Publish: %v", err)
				}
				select {
				case got := <-targetCh:
					if got.Identity != target {
						b.Fatalf("target subscriber received foreign identity: %+v", got.Identity)
					}
				case <-time.After(time.Second):
					b.Fatal("target subscriber did not receive its event")
				}
			}
			b.StopTimer()

			for _, other := range others {
				select {
				case leaked := <-other.ch:
					b.Fatalf("subscriber %+v received target event: %+v", other.id, leaked)
				default:
				}
			}
		})
	}
}
