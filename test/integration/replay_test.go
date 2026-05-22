// Phase 06 integration test — events replay-from-cursor end-to-end
// against real audit + events + telemetry/eventbus drivers (no mocks
// at the seam, per AGENTS.md §17).
//
// What this proves end-to-end:
//
//   - Logger.Error → eventbus adapter → inmem bus → ring buffer → Replay.
//   - The audit redactor walks the runtime.error payload before it
//     hits the ring; replayed events therefore carry the redacted
//     RedactedMap shape, not the raw fields the logger handed in.
//   - Identity propagation through every layer: the triple flows
//     from ctx into Logger, from Logger's BusEmitter into the bus's
//     Identity quadruple, into the ring, and back out via Replay's
//     filter.
//   - Failure mode: Replay on an empty-triple non-admin filter is
//     rejected with ErrIdentityScopeRequired (the same rule
//     Subscribe enforces; Phase 06 reuses Phase 05's filter helper).
//   - Failure mode: a cursor older than the ring tail surfaces as
//     ErrCursorTooOld with the (oldest, requested) detail wrapped.
package integration_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/telemetry/eventbus"
)

// TestE2E_Phase06_Replay_EndToEnd is the canonical wave-3 cross-
// subsystem aliveness test for replay. It wires the same
// Logger.Error → bus path the wave-2 test uses, drains a cursor,
// publishes more events, then calls Replay and asserts the strictly-
// newer events arrive with the same redaction shape and gap-free
// Sequence ordering as the live stream.
func TestE2E_Phase06_Replay_EndToEnd(t *testing.T) {
	ctx := identityCtx(t)
	red := auditpatterns.New()

	cfg := replayConfig(64)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	bus, err := events.Open(ctx, cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("inmem driver does not satisfy events.Replayer")
	}

	logger, err := telemetry.New(cfg.Telemetry, red,
		telemetry.WithBusEmitter(eventbus.New(bus)))
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	id := identity.MustFrom(ctx)
	filter := events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{events.EventTypeRuntimeError},
	}

	// Drain stream 1: subscribe, emit 4 errors, capture cursor.
	sub1, err := bus.Subscribe(ctx, filter)
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	for i := range 4 {
		logger.Error(ctx, "wave3 first batch", slog.Int("iter", i))
	}
	got1 := mustDrain(t, sub1, 4, 2*time.Second)
	cursor := events.Cursor{SessionID: id.SessionID, Sequence: got1[len(got1)-1].Sequence}
	sub1.Cancel()

	// Drain the cancel notice / any tail. We don't care what's there;
	// just don't block the test on a stale buffer.
	drainAndDiscard(sub1, 200*time.Millisecond)

	// More events arrive while "disconnected".
	for i := 4; i < 10; i++ {
		logger.Error(ctx, "wave3 second batch", slog.Int("iter", i))
	}

	// Replay from the cursor — expect strictly newer events, in
	// Sequence order, with the redacted RedactedMap payload.
	out, err := rp.Replay(ctx, cursor, filter)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(out) != 6 {
		t.Fatalf("Replay returned %d events, want 6", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].Sequence <= out[i-1].Sequence {
			t.Errorf("non-monotonic replay seq: out[%d]=%d out[%d]=%d",
				i-1, out[i-1].Sequence, i, out[i].Sequence)
		}
	}
	for i, ev := range out {
		if ev.Identity.TenantID != id.TenantID {
			t.Errorf("replay[%d] cross-tenant: tenant=%q want=%q", i, ev.Identity.TenantID, id.TenantID)
		}
		if _, ok := ev.Payload.(events.RedactedMap); !ok {
			t.Errorf("replay[%d] payload type=%T, want RedactedMap (audit redactor must have run)", i, ev.Payload)
		}
	}

	// Failure mode 1: empty-triple non-admin filter ⇒ ErrIdentityScopeRequired.
	_, err = rp.Replay(ctx, cursor, events.Filter{})
	if !errors.Is(err, events.ErrIdentityScopeRequired) {
		t.Errorf("empty filter: err=%v, want ErrIdentityScopeRequired", err)
	}
}

// TestE2E_Phase06_Replay_RingOverrun pins the documented loss
// semantics: when the ring overruns, a cursor older than the tail
// returns ErrCursorTooOld with detail. This is the seam Phase 57's
// durable log will plug into.
func TestE2E_Phase06_Replay_RingOverrun(t *testing.T) {
	ctx := identityCtx(t)
	red := auditpatterns.New()

	// Tiny ring so we can overrun cheaply.
	cfg := replayConfig(8)
	bus, err := events.Open(ctx, cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("inmem driver does not satisfy events.Replayer")
	}

	logger, err := telemetry.New(cfg.Telemetry, red,
		telemetry.WithBusEmitter(eventbus.New(bus)))
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	id := identity.MustFrom(ctx)
	filter := events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{events.EventTypeRuntimeError},
	}

	// Capture an early cursor by draining one event.
	sub, err := bus.Subscribe(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	logger.Error(ctx, "first")
	got := mustDrain(t, sub, 1, time.Second)
	earlyCursor := events.Cursor{SessionID: id.SessionID, Sequence: got[0].Sequence}

	// Keep the subscriber draining so we don't backpressure the bus.
	drainStop := make(chan struct{})
	go func() {
		for {
			select {
			case <-drainStop:
				return
			case _, ok := <-sub.Events():
				if !ok {
					return
				}
			}
		}
	}()

	// Overrun the ring (cap=8, publish 50).
	for range 50 {
		logger.Error(ctx, "overflow")
	}

	out, err := rp.Replay(ctx, earlyCursor, filter)
	if !errors.Is(err, events.ErrCursorTooOld) {
		t.Fatalf("err=%v, want ErrCursorTooOld", err)
	}
	if out != nil {
		t.Errorf("returned %d events on too-old cursor; want nil", len(out))
	}
	if !strings.Contains(err.Error(), "oldest=") || !strings.Contains(err.Error(), "requested=") {
		t.Errorf("ErrCursorTooOld message missing detail: %v", err)
	}

	close(drainStop)
	sub.Cancel()
}

// --- helpers ---

func replayConfig(ringSize int) *config.Config {
	c := wave2Config()
	c.Events.ReplayBufferSize = ringSize
	return c
}

// mustDrain reads exactly n events from sub or fails the test. Helper
// distinct from wave2_test.go's mustReceive to avoid coupling test
// failure messages between the two files.
func mustDrain(t *testing.T, sub events.Subscription, n int, timeout time.Duration) []events.Event {
	t.Helper()
	out := make([]events.Event, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription channel closed at event %d (want %d)", len(out), n)
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("drain timed out at %d/%d events after %v", len(out), n, timeout)
		}
	}
	return out
}

// drainAndDiscard reads from sub for at most timeout, discarding
// anything received. Used to flush any tail buffered after Cancel.
func drainAndDiscard(sub events.Subscription, timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-sub.Events():
			if !ok {
				return
			}
		case <-deadline:
			return
		}
	}
}
