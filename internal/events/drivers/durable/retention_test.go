package durable_test

import (
	"context"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
)

func publishAt(t *testing.T, bus events.EventBus, id identity.Quadruple, at time.Time) {
	t.Helper()
	ev := events.Event{
		Type:       events.EventTypeRuntimeWarning,
		Identity:   id,
		OccurredAt: at,
		Payload:    runtimeWarn("retention-probe"),
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func TestDurable_OldestRetainedAt_EmptyLog_Absent(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	rr, ok := bus.(events.RetentionReporter)
	if !ok {
		t.Fatal("durable bus does not implement events.RetentionReporter")
	}
	_, present, err := rr.OldestRetainedAt(context.Background())
	if err != nil {
		t.Fatalf("OldestRetainedAt: %v", err)
	}
	if present {
		t.Fatal("empty durable log reported a horizon; want absent")
	}
}

func TestDurable_OldestRetainedAt_ReflectsPersistedHead(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	rr := bus.(events.RetentionReporter)

	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	id := quad("t1", "u1", "s1")
	// Persist three events across two sessions; the oldest is the head.
	publishAt(t, bus, id, base)
	publishAt(t, bus, quad("t1", "u1", "s2"), base.Add(time.Hour))
	publishAt(t, bus, id, base.Add(2*time.Hour))

	oldest, present, err := rr.OldestRetainedAt(context.Background())
	if err != nil || !present {
		t.Fatalf("OldestRetainedAt: present=%v err=%v", present, err)
	}
	if !oldest.Equal(base) {
		t.Fatalf("horizon = %v, want persisted head %v", oldest, base)
	}
}

// TestDurable_OldestRetainedAt_ConcurrentPublishAndRead extends the
// driver's D-025 stress to the retention seam: N concurrent persisting
// publishers racing N horizon readers must never race the horizon field
// (run under -race) and the horizon stays the earliest OccurredAt.
func TestDurable_OldestRetainedAt_ConcurrentPublishAndRead(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)
	rr := bus.(events.RetentionReporter)

	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	id := quad("t1", "u1", "s1")
	// Publish the earliest event first so the horizon floor is fixed.
	publishAt(t, bus, id, base)

	const workers = 120
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for i := 1; i <= workers; i++ {
		go func(i int) {
			defer wg.Done()
			publishAt(t, bus, id, base.Add(time.Duration(i)*time.Second))
		}(i)
		go func() {
			defer wg.Done()
			_, _, _ = rr.OldestRetainedAt(context.Background())
		}()
	}
	wg.Wait()

	oldest, present, err := rr.OldestRetainedAt(context.Background())
	if err != nil || !present {
		t.Fatalf("post-storm OldestRetainedAt: present=%v err=%v", present, err)
	}
	if !oldest.Equal(base) {
		t.Fatalf("horizon = %v, want the earliest %v (untrimmed floor)", oldest, base)
	}
}

// The untrimmed durable log means the horizon survives a restart: a fresh
// bus over the SAME store recovers the oldest-retained head from the
// persisted log at boot.
func TestDurable_OldestRetainedAt_RecoveredAcrossRestart(t *testing.T) {
	store := newInmemStore(t)
	base := time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC)
	id := quad("t1", "u1", "s1")

	// First bus: persist two events, then close (store survives).
	bus1, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New #1: %v", err)
	}
	publishAt(t, bus1, id, base)
	publishAt(t, bus1, id, base.Add(time.Hour))
	if err := bus1.Close(context.Background()); err != nil {
		t.Fatalf("Close #1: %v", err)
	}

	// Second bus over the same store: recovers the horizon at boot.
	bus2, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New #2: %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close(context.Background()) })

	oldest, present, err := bus2.(events.RetentionReporter).OldestRetainedAt(context.Background())
	if err != nil || !present {
		t.Fatalf("post-restart OldestRetainedAt: present=%v err=%v", present, err)
	}
	if !oldest.Equal(base) {
		t.Fatalf("post-restart horizon = %v, want recovered head %v", oldest, base)
	}
}
