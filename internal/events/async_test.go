package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type legacyPublicationBus struct {
	published []Event
}

func (b *legacyPublicationBus) Publish(_ context.Context, ev Event) error {
	b.published = append(b.published, ev)
	return nil
}

func (*legacyPublicationBus) Subscribe(context.Context, Filter) (Subscription, error) {
	return nil, nil
}

func (*legacyPublicationBus) Close(context.Context) error { return nil }

type asyncPublicationBus struct {
	*legacyPublicationBus
	async    []Event
	flushes  int
	flushErr error
}

func (b *asyncPublicationBus) PublishAsync(_ context.Context, ev Event) error {
	b.async = append(b.async, ev)
	return nil
}

func (b *asyncPublicationBus) Flush(context.Context) error {
	b.flushes++
	return b.flushErr
}

var (
	_ EventBus       = (*legacyPublicationBus)(nil)
	_ AsyncPublisher = (*asyncPublicationBus)(nil)
	_ Flusher        = (*asyncPublicationBus)(nil)
)

func TestPublishAsync_SelectsCapabilityAndPreservesEvent(t *testing.T) {
	want := Event{Type: EventTypeRuntimeWarning}
	bus := &asyncPublicationBus{legacyPublicationBus: &legacyPublicationBus{}}

	if err := PublishAsync(context.Background(), bus, want); err != nil {
		t.Fatalf("PublishAsync: %v", err)
	}
	if len(bus.async) != 1 || bus.async[0].Type != want.Type {
		t.Fatalf("async publications = %#v, want one %q event", bus.async, want.Type)
	}
	if len(bus.published) != 0 {
		t.Fatalf("synchronous Publish called %d times, want 0", len(bus.published))
	}
}

func TestPublishAsync_LegacyFallbackUsesSynchronousPublish(t *testing.T) {
	want := Event{Type: EventTypeRuntimeWarning}
	bus := &legacyPublicationBus{}

	if err := PublishAsync(context.Background(), bus, want); err != nil {
		t.Fatalf("PublishAsync legacy fallback: %v", err)
	}
	if len(bus.published) != 1 || bus.published[0].Type != want.Type {
		t.Fatalf("legacy publications = %#v, want one %q event", bus.published, want.Type)
	}
}

func TestFlush_SelectsCapabilityAndPropagatesFailure(t *testing.T) {
	wantErr := errors.New("flush failed")
	bus := &asyncPublicationBus{
		legacyPublicationBus: &legacyPublicationBus{},
		flushErr:             wantErr,
	}

	if err := Flush(context.Background(), bus); !errors.Is(err, wantErr) {
		t.Fatalf("Flush error = %v, want %v", err, wantErr)
	}
	if bus.flushes != 1 {
		t.Fatalf("Flush calls = %d, want 1", bus.flushes)
	}
}

func TestFlush_LegacyBusIsNoOp(t *testing.T) {
	if err := Flush(context.Background(), &legacyPublicationBus{}); err != nil {
		t.Fatalf("Flush legacy bus: %v", err)
	}
}

type observedAsyncBus struct {
	*asyncPublicationBus
	err    error
	signal *AsyncAdmissionSignal
}

func (b *observedAsyncBus) PublishAsync(context.Context, Event) error { return b.err }

func (b *observedAsyncBus) ObserveAsyncAdmissionFailure(ctx context.Context, eventType EventType, err error) {
	b.signal.Observe(ctx, eventType, err)
}

func (b *observedAsyncBus) AsyncAdmissionFailures() int64 { return b.signal.Total() }

var _ AsyncAdmissionFailureObserver = (*observedAsyncBus)(nil)

func TestPublishAsyncObserved_SignalsAdmissionFailureWithoutBlocking(t *testing.T) {
	var logs bytes.Buffer
	signal := NewAsyncAdmissionSignal(slog.New(slog.NewTextHandler(&logs, nil)), time.Hour)
	bus := &observedAsyncBus{
		asyncPublicationBus: &asyncPublicationBus{legacyPublicationBus: &legacyPublicationBus{}},
		err:                 ErrAsyncQueueFull,
		signal:              signal,
	}
	started := time.Now()
	PublishAsyncObserved(context.Background(), bus, Event{
		Type:    EventTypeRuntimeWarning,
		Payload: RuntimeErrorPayload{Message: "secret payload must not be logged"},
	})
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("PublishAsyncObserved blocked for %s", elapsed)
	}
	if got := bus.AsyncAdmissionFailures(); got != 1 {
		t.Fatalf("AsyncAdmissionFailures = %d, want 1", got)
	}
	if !strings.Contains(logs.String(), "reason=queue_full") {
		t.Fatalf("admission warning missing queue_full reason: %q", logs.String())
	}
	if strings.Contains(logs.String(), "secret payload") {
		t.Fatalf("admission warning leaked event payload: %q", logs.String())
	}
}

func TestAsyncAdmissionSignal_CountsReasonsAndRateLimitsLogs(t *testing.T) {
	var logs bytes.Buffer
	signal := NewAsyncAdmissionSignal(slog.New(slog.NewTextHandler(&logs, nil)), time.Hour)
	ev := Event{Type: EventTypeRuntimeError}
	signal.Observe(context.Background(), ev.Type, ErrAsyncQueueFull)
	signal.Observe(context.Background(), ev.Type, fmt.Errorf("wrapped: %w", ErrAsyncQueueFull))
	signal.Observe(context.Background(), ev.Type, ErrBusClosed)
	signal.Observe(context.Background(), ev.Type, context.Canceled)

	if got := signal.Total(); got != 3 {
		t.Fatalf("Total = %d, want 3 recognized admission failures", got)
	}
	if got := signal.QueueFull(); got != 2 {
		t.Fatalf("QueueFull = %d, want 2", got)
	}
	if got := signal.Closed(); got != 1 {
		t.Fatalf("Closed = %d, want 1", got)
	}
	if got := strings.Count(logs.String(), "asynchronous publication admission failed"); got != 1 {
		t.Fatalf("rate-limited warning count = %d, want 1: %q", got, logs.String())
	}
	if strings.Contains(logs.String(), "do not log me") {
		t.Fatalf("admission warning leaked event payload: %q", logs.String())
	}
}
