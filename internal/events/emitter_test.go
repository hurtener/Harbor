package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// emitterCaptureBus is a minimal EventBus double that records every
// published event (optionally failing). The behavioural contract under
// test is the EMITTER's (stamping + Warn-on-failure), not a bus
// driver's — the driver contract has its own conformance suite.
type emitterCaptureBus struct {
	mu     sync.Mutex
	events []Event
	fail   error
}

func (b *emitterCaptureBus) Publish(_ context.Context, ev Event) error {
	if b.fail != nil {
		return b.fail
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	return nil
}

func (b *emitterCaptureBus) PublishLive(ctx context.Context, ev Event) error {
	return b.Publish(ctx, ev)
}

func (b *emitterCaptureBus) Subscribe(context.Context, Filter) (Subscription, error) {
	return nil, errors.New("emitterCaptureBus: Subscribe unsupported")
}

func (b *emitterCaptureBus) Close(context.Context) error { return nil }

func (b *emitterCaptureBus) captured() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, len(b.events))
	copy(out, b.events)
	return out
}

func emitterTestQuad() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"},
		RunID:    "run-1",
	}
}

// TestIdentityStampingEmitter_StampsMissingIdentity — an event with
// an empty Identity gets the run's quadruple stamped before publish.
func TestIdentityStampingEmitter_StampsMissingIdentity(t *testing.T) {
	bus := &emitterCaptureBus{}
	q := emitterTestQuad()
	emit := IdentityStampingEmitter(bus, q, slog.Default())

	emit(Event{Type: EventType("planner.decision")})

	got := bus.captured()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	if got[0].Identity != q {
		t.Errorf("Identity = %+v, want stamped %+v", got[0].Identity, q)
	}
}

// TestIdentityStampingEmitter_PreservesPresetIdentity — a planner
// that already scoped its event wins; the emitter never overwrites.
func TestIdentityStampingEmitter_PreservesPresetIdentity(t *testing.T) {
	bus := &emitterCaptureBus{}
	preset := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"},
		RunID:    "run-preset",
	}
	emit := IdentityStampingEmitter(bus, emitterTestQuad(), slog.Default())

	emit(Event{Type: EventType("planner.decision"), Identity: preset})

	got := bus.captured()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	if got[0].Identity != preset {
		t.Errorf("Identity = %+v, want preserved %+v", got[0].Identity, preset)
	}
}

// TestIdentityStampingEmitter_WarnsOnPublishFailure — brief 06 §5:
// bus publish failures are surfaced loudly, never swallowed.
func TestIdentityStampingEmitter_WarnsOnPublishFailure(t *testing.T) {
	bus := &emitterCaptureBus{fail: errors.New("bus closed")}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	emit := IdentityStampingEmitter(bus, emitterTestQuad(), logger)

	emit(Event{Type: EventType("planner.decision")})

	if !strings.Contains(buf.String(), "emitter publish failed") {
		t.Errorf("expected loud Warn on publish failure; log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "bus closed") {
		t.Errorf("Warn should carry the publish error; log: %s", buf.String())
	}
}

// TestIdentityStampingEmitter_NilLoggerDefaults — a nil logger must
// not panic the failure path (it defaults to slog.Default()).
func TestIdentityStampingEmitter_NilLoggerDefaults(t *testing.T) {
	bus := &emitterCaptureBus{fail: errors.New("boom")}
	emit := IdentityStampingEmitter(bus, emitterTestQuad(), nil)
	emit(Event{Type: EventType("planner.decision")}) // must not panic
}

// TestIdentityStampingEmitter_ConcurrentRuns_NoIdentityBleed is the
// D-025 / §6 stress gate the phase plan mandates: N≥100 concurrent
// per-run emitter closures share ONE bus; every delivered event must
// carry exactly its own run's quadruple (no cross-run identity bleed)
// and the goroutine baseline must be restored after all runs return.
func TestIdentityStampingEmitter_ConcurrentRuns_NoIdentityBleed(t *testing.T) {
	bus := &emitterCaptureBus{}
	const runs = 128
	const perRun = 8

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("tenant-%d", n),
					UserID:    fmt.Sprintf("user-%d", n),
					SessionID: fmt.Sprintf("sess-%d", n),
				},
				RunID: fmt.Sprintf("run-%d", n),
			}
			emit := IdentityStampingEmitter(bus, q, slog.Default())
			for range perRun {
				emit(Event{
					Type:  EventType("planner.decision"),
					Extra: map[string]string{"run": q.RunID},
				})
			}
		}(i)
	}
	wg.Wait()

	got := bus.captured()
	if len(got) != runs*perRun {
		t.Fatalf("delivered %d events, want %d", len(got), runs*perRun)
	}
	for _, ev := range got {
		wantRun := ev.Extra["run"]
		if ev.Identity.RunID != wantRun {
			t.Fatalf("cross-run identity bleed: event tagged %q carries RunID %q", wantRun, ev.Identity.RunID)
		}
		wantSuffix := strings.TrimPrefix(wantRun, "run-")
		if ev.Identity.TenantID != "tenant-"+wantSuffix {
			t.Fatalf("cross-run identity bleed: event tagged %q carries TenantID %q", wantRun, ev.Identity.TenantID)
		}
	}

	// Goroutine baseline restored (the emitter spawns none; this
	// guards a future regression that makes it async).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Errorf("goroutine baseline not restored: %d > %d", n, baseline)
	}
}
