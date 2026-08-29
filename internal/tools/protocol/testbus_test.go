package protocol_test

import (
	"context"
	"sync"

	"github.com/hurtener/Harbor/internal/events"
)

// capturingBus is a minimal events.EventBus used by the admin-audit
// tests to count published events. It is concurrency-safe so the
// concurrent-reuse test can share one instance.
type capturingBus struct {
	mu     sync.Mutex
	events []events.Event
}

func newCapturingBus() *capturingBus { return &capturingBus{} }

func (b *capturingBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	return nil
}

func (b *capturingBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, nil
}

func (b *capturingBus) Close(context.Context) error { return nil }

func (b *capturingBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}
