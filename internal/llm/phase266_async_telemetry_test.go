package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// phase266BlockingBatchStore is a failure-injection decorator around the real
// in-memory StateStore. It models slow durable persistence without replacing
// any production behavior: all records still pass through the real driver's
// SaveBatchIf implementation once the test releases the gate.
type phase266BlockingBatchStore struct {
	state.StateStore
	armed     atomic.Bool
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	freeOnce  sync.Once
}

func (s *phase266BlockingBatchStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	if s.armed.Load() {
		s.enterOnce.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

func (s *phase266BlockingBatchStore) free() {
	s.freeOnce.Do(func() { close(s.release) })
}

func phase266TelemetryConfig() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Hour,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
		LegacyWritersDrained:     true,
	}
}

func phase266TelemetryID() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "phase266-llm-tenant",
			UserID:    "phase266-llm-user",
			SessionID: "phase266-llm-session",
		},
		RunID: "phase266-llm-run",
	}
}

func phase266TelemetryFilter(id identity.Quadruple) events.Filter {
	return events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Run:     id.RunID,
	}
}

func phase266TelemetryBus(t *testing.T, store state.StateStore) (events.EventBus, events.Replayer) {
	t.Helper()
	bus, err := durable.New(context.Background(), phase266TelemetryConfig(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("durable bus does not implement events.Replayer")
	}
	return bus, rp
}

func phase266ReplayEventually(t *testing.T, rp events.Replayer, id identity.Quadruple, want int) []events.Event {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, phase266TelemetryFilter(id))
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		if len(got) >= want {
			return got
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("Replay returned %d events after 2s, want at least %d", len(got), want)
		}
	}
}

// TestPhase266_AsyncCostTelemetry_DoesNotWaitForSlowDurableStore is expected
// to fail on the pre-Phase-266 implementation: emitCostRecorded currently
// calls EventBus.Publish inline. Once the async telemetry lane lands, the
// caller must return while the real durable write remains blocked, and the
// eventual replay must contain the cost event exactly once.
func TestPhase266_AsyncCostTelemetry_DoesNotWaitForSlowDurableStore(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close(context.Background()) })
	store := &phase266BlockingBatchStore{
		StateStore: inner,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	bus, rp := phase266TelemetryBus(t, store)
	id := phase266TelemetryID()
	store.armed.Store(true)

	callerDone := make(chan struct{})
	go func() {
		emitCostRecorded(context.Background(), bus, id, "phase266-model", Cost{TotalCost: 0.01}, Usage{TotalTokens: 4}, 4096)
		close(callerDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		store.free()
		t.Fatal("cost telemetry did not reach the blocked durable write")
	}
	select {
	case <-callerDone:
	case <-time.After(250 * time.Millisecond):
		store.free()
		<-callerDone
		t.Fatal("async cost telemetry caller remained blocked by durable persistence")
	}

	store.free()
	phase266ReplayEventually(t, rp, id, 1)
}

// TestPhase266_TerminalPublishCannotOvertakeQueuedTelemetry starts a later
// synchronous publication marker only after the earlier cost telemetry has
// entered the durable write. The terminal barrier must wait for that earlier
// queued item. The external integration gate uses the real task.completed
// payload; this package keeps the marker on the core events surface to avoid
// creating an llm -> tasks test import cycle.
func TestPhase266_TerminalPublishCannotOvertakeQueuedTelemetry(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close(context.Background()) })
	store := &phase266BlockingBatchStore{
		StateStore: inner,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	bus, rp := phase266TelemetryBus(t, store)
	id := phase266TelemetryID()
	store.armed.Store(true)

	telemetryDone := make(chan struct{})
	go func() {
		emitCostRecorded(context.Background(), bus, id, "phase266-model", Cost{TotalCost: 0.01}, Usage{TotalTokens: 4}, 4096)
		close(telemetryDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		store.free()
		t.Fatal("cost telemetry did not reach the blocked durable write")
	}

	terminalDone := make(chan error, 1)
	go func() {
		terminalDone <- bus.Publish(context.Background(), events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: 1},
		})
	}()
	select {
	case err := <-terminalDone:
		store.free()
		<-telemetryDone
		if err == nil {
			t.Fatal("terminal Publish completed before earlier queued telemetry was released")
		}
		t.Fatalf("terminal Publish failed before barrier release: %v", err)
	case <-time.After(250 * time.Millisecond):
		// The terminal call is correctly waiting behind the earlier durable
		// publication. Release it below and assert exact replay order.
	}

	store.free()
	select {
	case <-telemetryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cost telemetry did not finish after durable release")
	}
	select {
	case err := <-terminalDone:
		if err != nil {
			t.Fatalf("terminal Publish after barrier release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal Publish did not finish after durable release")
	}

	got := phase266ReplayEventually(t, rp, id, 2)
	if len(got) != 2 {
		t.Fatalf("replay returned %d events, want exactly 2: %#v", len(got), got)
	}
	if got[0].Type != EventTypeCostRecorded || got[0].Sequence != 1 {
		t.Errorf("first replay event = (%d,%q), want (1,%q)", got[0].Sequence, got[0].Type, EventTypeCostRecorded)
	}
	if got[1].Type != events.EventTypeRuntimeWarning || got[1].Sequence != 2 {
		t.Errorf("second replay event = (%d,%q), want (2,%q)", got[1].Sequence, got[1].Type, events.EventTypeRuntimeWarning)
	}
}

// TestPhase266_CancelledLiveChunkDoesNotCreateDurableReplay pins the honest
// cancellation boundary shared with Console live_resume_seq: a callback that
// runs after its driver context is cancelled cannot create a durable chunk or
// a synthetic terminal record. Durable task/session state remains the only
// source of truth for completion.
func TestPhase266_CancelledLiveChunkDoesNotCreateDurableReplay(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close(context.Background()) })
	bus, rp := phase266TelemetryBus(t, inner)
	id := phase266TelemetryID()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	publishChunk := NewChunkPublisherContext(ctx, bus, id, "phase266-task", nil)
	publishChunk("late-delta", false, "content")

	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, phase266TelemetryFilter(id))
	if err != nil {
		t.Fatalf("Replay after cancelled chunk: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cancelled live callback created durable replay rows: %#v", got)
	}
}
