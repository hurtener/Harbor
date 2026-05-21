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

// busBenchConfig is the inmem event-bus config the fan-out
// benchmark runs against. SubscriberBufferSize is sized generously
// so the benchmark measures fan-out cost, not drop-oldest churn;
// MaxSubscribersPerSession caps the sweep at the operational
// default (brief 06 §"Filter expressions": "Cardinality of
// subscribers is bounded per session ... default ~16").
func busBenchConfig() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     1024,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}
}

func busBenchEvent(id identity.Quadruple) events.Event {
	return events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: id,
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: 1},
	}
}

// BenchmarkBusFanOut measures event-bus publish latency as a
// function of subscriber count — the master-plan's "bus fan-out
// (subscribers vs latency)" axis. It sweeps {1, 8, 16} subscribers
// (capped at the default MaxSubscribersPerSession) and reports
// `ns/op` per Publish for each. brief 06 §"Fan-out" says Publish is
// O(1) with non-blocking fan-out sends; this benchmark is the
// empirical check that publish latency stays near-flat as
// subscribers grow.
//
// Real components on every seam: a real `audit` redactor (the
// `patterns` driver) and the real `inmem` EventBus driver, both
// resolved through their §4.4 factories — no mocks (CLAUDE.md §13).
func BenchmarkBusFanOut(b *testing.B) {
	for _, subs := range []int{1, 8, 16} {
		subs := subs
		b.Run(fmt.Sprintf("subscribers=%d", subs), func(b *testing.B) {
			red, err := audit.Open(context.Background(), config.AuditConfig{})
			if err != nil {
				b.Fatalf("audit.Open: %v", err)
			}
			bus, err := events.Open(context.Background(), busBenchConfig(), red)
			if err != nil {
				b.Fatalf("events.Open: %v", err)
			}
			b.Cleanup(func() { _ = bus.Close(context.Background()) })

			id := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  "bench-tenant",
					UserID:    "bench-user",
					SessionID: "bench-session",
				},
			}

			// Establish `subs` subscribers on the session and drain
			// their channels concurrently so the bus never blocks on
			// a full subscriber buffer — the benchmark measures the
			// fan-out send cost, not back-pressure stalls.
			for s := 0; s < subs; s++ {
				sub, err := bus.Subscribe(context.Background(), events.Filter{
					Tenant:  id.TenantID,
					User:    id.UserID,
					Session: id.SessionID,
				})
				if err != nil {
					b.Fatalf("Subscribe: %v", err)
				}
				ch := sub.Events()
				go func() {
					for range ch {
					}
				}()
				b.Cleanup(sub.Cancel)
			}

			ev := busBenchEvent(id)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := bus.Publish(context.Background(), ev); err != nil {
					b.Fatalf("Publish: %v", err)
				}
			}
		})
	}
}

// BenchmarkBusFanOutCrossTenant measures publish latency when
// subscribers span multiple tenants — the server-side filter
// (events.Filter.Matches) is evaluated per subscriber before
// fan-out (brief 06 §"Filter expressions"). Each publish targets
// one tenant; the bus must filter the non-matching subscribers out.
// This confirms identity-scoped filtering cost stays bounded.
func BenchmarkBusFanOutCrossTenant(b *testing.B) {
	const tenants = 8

	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		b.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), busBenchConfig(), red)
	if err != nil {
		b.Fatalf("events.Open: %v", err)
	}
	b.Cleanup(func() { _ = bus.Close(context.Background()) })

	ids := make([]identity.Quadruple, tenants)
	for t := 0; t < tenants; t++ {
		id := identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", t),
				UserID:    "bench-user",
				SessionID: "bench-session",
			},
		}
		ids[t] = id
		sub, err := bus.Subscribe(context.Background(), events.Filter{
			Tenant:  id.TenantID,
			User:    id.UserID,
			Session: id.SessionID,
		})
		if err != nil {
			b.Fatalf("Subscribe: %v", err)
		}
		ch := sub.Events()
		go func() {
			for range ch {
			}
		}()
		b.Cleanup(sub.Cancel)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := ids[i%tenants]
		if err := bus.Publish(context.Background(), busBenchEvent(id)); err != nil {
			b.Fatalf("Publish: %v", err)
		}
	}
}
