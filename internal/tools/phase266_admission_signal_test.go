package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
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

func TestCatalogLifecycle_FailedToolAdmissionFailureIsObservableAndPrompt(t *testing.T) {
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
			cat := NewCatalog(WithCatalogBus(bus))
			if err := cat.Register(ToolDescriptor{
				Tool: Tool{Name: "phase266-failing-tool", Transport: TransportInProcess},
				Invoke: func(context.Context, json.RawMessage) (ToolResult, error) {
					return ToolResult{}, context.Canceled
				},
			}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			desc, ok := cat.Resolve("phase266-failing-tool")
			if !ok {
				t.Fatal("Resolve: tool missing")
			}
			id := identity.Identity{TenantID: "phase266-tools-tenant", UserID: "phase266-tools-user", SessionID: "phase266-tools-session"}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				t.Fatalf("identity.With: %v", err)
			}
			ctx, err = identity.WithRun(ctx, id, "phase266-tools-run")
			if err != nil {
				t.Fatalf("identity.WithRun: %v", err)
			}

			started := time.Now()
			_, invokeErr := desc.Invoke(ctx, json.RawMessage(`{}`))
			if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
				t.Fatalf("failed tool invocation blocked for %s after %v", elapsed, tc.err)
			}
			if !errors.Is(invokeErr, context.Canceled) {
				t.Fatalf("Invoke error = %v, want context.Canceled", invokeErr)
			}
			if got := bus.AsyncAdmissionFailures(); got != 2 {
				t.Fatalf("AsyncAdmissionFailures = %d, want 2 (invoked + failed)", got)
			}
			if got := bus.asyncCall.Load(); got != 2 {
				t.Fatalf("PublishAsync calls = %d, want 2", got)
			}
			if got := bus.publish.Load(); got != 0 {
				t.Fatalf("synchronous Publish calls = %d, want 0", got)
			}
		})
	}
}
