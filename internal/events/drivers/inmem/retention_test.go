package inmem_test

import (
	"context"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
)

// ringCfg is defaultCfg with a bounded replay ring so retention-horizon
// eviction is exercised.
func ringCfg(capacity int) config.EventsConfig {
	c := defaultCfg()
	c.ReplayBufferSize = capacity
	return c
}

func TestOldestRetainedAt_EmptyRing_Absent(t *testing.T) {
	bus, err := inmem.New(ringCfg(4), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	rr, ok := bus.(events.RetentionReporter)
	if !ok {
		t.Fatal("inmem bus does not implement events.RetentionReporter")
	}
	_, present, err := rr.OldestRetainedAt(context.Background())
	if err != nil {
		t.Fatalf("OldestRetainedAt: %v", err)
	}
	if present {
		t.Fatal("empty ring reported a horizon; want absent")
	}
}

func TestOldestRetainedAt_ReflectsRingHead_AndAdvancesOnEvict(t *testing.T) {
	bus, err := inmem.New(ringCfg(3), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rr := bus.(events.RetentionReporter)

	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	pub := func(i int) {
		ev := mkEvent(1)
		ev.OccurredAt = base.Add(time.Duration(i) * time.Minute)
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	// Fill the ring with events at t+0, t+1, t+2 — head is t+0.
	pub(0)
	pub(1)
	pub(2)
	oldest, present, err := rr.OldestRetainedAt(context.Background())
	if err != nil || !present {
		t.Fatalf("OldestRetainedAt: present=%v err=%v", present, err)
	}
	if !oldest.Equal(base) {
		t.Fatalf("horizon = %v, want ring head %v", oldest, base)
	}

	// Two more publishes evict the two oldest; head advances to t+2.
	pub(3)
	pub(4)
	oldest, present, err = rr.OldestRetainedAt(context.Background())
	if err != nil || !present {
		t.Fatalf("OldestRetainedAt post-evict: present=%v err=%v", present, err)
	}
	want := base.Add(2 * time.Minute)
	if !oldest.Equal(want) {
		t.Fatalf("post-evict horizon = %v, want %v (ring head advanced)", oldest, want)
	}
}

// TestOldestRetainedAt_ConcurrentPublishAndRead extends the driver's
// D-025 concurrent-reuse contract to the retention seam: N concurrent
// publishers (which evict the ring) racing N concurrent horizon readers
// must never race the ring (run under -race) and always return a
// coherent snapshot.
func TestOldestRetainedAt_ConcurrentPublishAndRead(t *testing.T) {
	bus, err := inmem.New(ringCfg(16), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rr := bus.(events.RetentionReporter)

	const workers = 120
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			ev := mkEvent(1)
			ev.OccurredAt = base.Add(time.Duration(i) * time.Second)
			_ = bus.Publish(context.Background(), ev)
		}(i)
		go func() {
			defer wg.Done()
			// The read must never race the ring; the value is best-effort
			// under concurrent eviction so we only assert no-panic / no-race.
			_, _, _ = rr.OldestRetainedAt(context.Background())
		}()
	}
	wg.Wait()

	// After the storm, a final read is coherent and present.
	if _, present, err := rr.OldestRetainedAt(context.Background()); err != nil || !present {
		t.Fatalf("post-storm OldestRetainedAt: present=%v err=%v", present, err)
	}
}

func TestOldestRetainedAt_AfterCloseErrors(t *testing.T) {
	bus, err := inmem.New(ringCfg(2), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	rr := bus.(events.RetentionReporter)
	_ = bus.Close(context.Background())
	if _, _, err := rr.OldestRetainedAt(context.Background()); err == nil {
		t.Fatal("OldestRetainedAt after Close returned nil error; want ErrBusClosed")
	}
}
