package llm

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

type phase266AdmissionBus struct {
	asyncErr  error
	asyncCall atomic.Int64
	publish   atomic.Int64
	signal    *events.AsyncAdmissionSignal
}

func (b *phase266AdmissionBus) Publish(context.Context, events.Event) error {
	b.publish.Add(1)
	return nil
}

func (b *phase266AdmissionBus) PublishAsync(context.Context, events.Event) error {
	b.asyncCall.Add(1)
	return b.asyncErr
}

func (*phase266AdmissionBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, nil
}

func (*phase266AdmissionBus) Close(context.Context) error { return nil }

func (b *phase266AdmissionBus) ObserveAsyncAdmissionFailure(ctx context.Context, eventType events.EventType, err error) {
	b.signal.Observe(ctx, eventType, err)
}

func (b *phase266AdmissionBus) AsyncAdmissionFailures() int64 {
	return b.signal.Total()
}

var _ events.EventBus = (*phase266AdmissionBus)(nil)
var _ events.AsyncPublisher = (*phase266AdmissionBus)(nil)
var _ events.AsyncAdmissionFailureObserver = (*phase266AdmissionBus)(nil)
var _ events.AsyncAdmissionCounter = (*phase266AdmissionBus)(nil)

func TestEmitCostRecorded_AdmissionFailureIsObservableAndPrompt(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "queue_full", err: events.ErrAsyncQueueFull},
		{name: "bus_closed", err: events.ErrBusClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := &phase266AdmissionBus{
				asyncErr: tc.err,
				signal:   events.NewAsyncAdmissionSignal(nil, time.Hour),
			}
			started := time.Now()
			emitCostRecorded(context.Background(), bus, phase266TelemetryID(), "phase266-model", Cost{TotalCost: 0.01}, Usage{TotalTokens: 4}, 4096)
			if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
				t.Fatalf("emitCostRecorded blocked for %s after %v", elapsed, tc.err)
			}
			if got := bus.AsyncAdmissionFailures(); got != 1 {
				t.Fatalf("AsyncAdmissionFailures = %d, want 1", got)
			}
			if got := bus.asyncCall.Load(); got != 1 {
				t.Fatalf("PublishAsync calls = %d, want 1", got)
			}
			if got := bus.publish.Load(); got != 0 {
				t.Fatalf("synchronous Publish calls = %d, want 0", got)
			}
		})
	}
}
