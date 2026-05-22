package flow_test

import (
	"context"
	"sync"

	"github.com/hurtener/Harbor/internal/events"
)

// recordingBus is a minimal events.EventBus that captures every
// Publish in memory.
type recordingBus struct {
	events []events.Event
	mu     sync.Mutex
	closed bool
}

func newRecordingBus() *recordingBus {
	return &recordingBus{}
}

func (b *recordingBus) Publish(ctx context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return events.ErrBusClosed
	}
	b.events = append(b.events, ev)
	return nil
}

func (b *recordingBus) Subscribe(ctx context.Context, f events.Filter) (events.Subscription, error) {
	return nil, events.ErrBusClosed
}

func (b *recordingBus) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (b *recordingBus) Count(t events.EventType) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.events {
		if e.Type == t {
			n++
		}
	}
	return n
}

func (b *recordingBus) All() []events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]events.Event, len(b.events))
	copy(out, b.events)
	return out
}

func withBusOnCtx(ctx context.Context, bus events.EventBus) context.Context {
	return events.WithBus(ctx, bus)
}
