package materializer

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
)

// listKindCountingStore makes the durable projection's global maintenance
// scans observable without changing the StateStore behavior. The production
// symptom is a ListKind(kindHead) query, but counting all ListKind calls also
// catches the adjacent fence snapshot scans performed by one Page call.
type listKindCountingStore struct {
	state.StateStore
	listKinds atomic.Int64
}

type scriptedProjectionSource struct {
	mu    sync.Mutex
	pages []events.ProjectionPage
	calls int
	wm    uint64
}

func (s *scriptedProjectionSource) Page(ctx context.Context, after uint64, _ int) (events.ProjectionPage, error) {
	if err := ctx.Err(); err != nil {
		return events.ProjectionPage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.pages) {
		return events.ProjectionPage{Next: after, Watermark: s.wm, Quality: events.ProjectionCurrent}, nil
	}
	page := s.pages[s.calls]
	s.calls++
	page.Events = append([]events.Event(nil), page.Events...)
	return page, nil
}

func (s *scriptedProjectionSource) Watermark(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wm, nil
}

func (*scriptedProjectionSource) Watch(context.Context, chan<- uint64) (events.ProjectionWatch, error) {
	return events.ProjectionWatchFunc(func() {}), nil
}

func (s *listKindCountingStore) ListKind(ctx context.Context, scope state.ListScope, prefix string) ([]state.StateRecord, error) {
	if strings.HasPrefix(prefix, "events.durable.") || strings.Contains(prefix, "events/session-fence/") {
		s.listKinds.Add(1)
	}
	return s.StateStore.ListKind(ctx, scope, prefix)
}

// TestMaterialize_DurableExcludedTailDoesNotRescanWhileIdle covers the
// browser-independent production shape: a durable source has a canonical
// event followed by a persisted bus-internal notice. The current page's
// Watermark is therefore ahead of Next. The materializer must checkpoint the
// examined watermark only after applying the page, then remain idle without
// repeating the durable driver's three global ListKind scans. A later
// canonical event must still wake and project, and durable replay must retain
// the complete canonical sequence around the excluded notice.
func TestMaterialize_DurableExcludedTailDoesNotRescanWhileIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	baseStore, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	store := &listKindCountingStore{StateStore: baseStore}
	bus, err := durable.New(ctx, config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
		LegacyWritersDrained:     true,
	}, auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()

	turnStore, err := sqlite.New(sqlite.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("turns sqlite.New: %v", err)
	}
	proj, err := turns.New(turnStore)
	if err != nil {
		_ = turnStore.Close(context.Background())
		t.Fatalf("turns.New: %v", err)
	}
	defer func() { _ = proj.Close(context.Background()) }()

	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}
	m, err := New(src, proj, WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("materializer.New: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(ctx) }()

	id := identity.Identity{TenantID: "tenant-idle", UserID: "user-idle", SessionID: "session-idle"}
	canonical := func() events.Event {
		return events.Event{
			Type:       events.EventTypeRuntimeError,
			Identity:   identity.Quadruple{Identity: id, RunID: "run-idle"},
			OccurredAt: time.Now().UTC(),
			Payload:    events.SubscriptionIdleClosedPayload{SubscriberID: 1},
		}
	}
	if err := bus.Publish(ctx, canonical()); err != nil {
		t.Fatalf("publish canonical: %v", err)
	}
	// Direct publication of an internal notice is supported for the event
	// registry and is intentionally persisted by the durable driver. The bus
	// generated form is transient; this fixture pins the historical tail that
	// made the polling loop repeatedly revisit the same source range.
	if err := bus.Publish(ctx, events.Event{
		Type:       events.EventTypeBusDropped,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: time.Now().UTC(),
		Payload:    events.BusDroppedPayload{FromSeq: 1, ToSeq: 1, DroppedCount: 1},
	}); err != nil {
		t.Fatalf("publish internal notice: %v", err)
	}

	if !eventually(t, func() bool { return m.Cursor() >= 2 }) {
		t.Fatal("materializer did not advance across the excluded durable tail")
	}
	store.listKinds.Store(0)
	// The short interval makes the old regression deterministic: before the
	// fix every tick saw watermark 2 > cursor 1 and issued a fresh Page.
	time.Sleep(50 * time.Millisecond)
	if got := store.listKinds.Load(); got != 0 {
		t.Fatalf("idle durable projection issued %d global ListKind scans", got)
	}

	if err := bus.Publish(ctx, canonical()); err != nil {
		t.Fatalf("publish post-idle canonical: %v", err)
	}
	if !eventually(t, func() bool { return m.Cursor() >= 3 }) {
		t.Fatal("materializer did not wake and advance after a later canonical event")
	}

	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("durable bus does not implement Replayer")
	}
	got, err := rp.Replay(ctx, events.Cursor{SessionID: id.SessionID}, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 3 || got[0].Sequence != 1 || got[1].Sequence != 2 || got[2].Sequence != 3 {
		t.Fatalf("Replay sequences = %v, want lossless [1 2 3] including persisted notice", replaySequences(got))
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("materializer Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("materializer Run did not stop after cancellation")
	}
}

func TestMaterialize_EmptyCatchingUpDoesNotPromoteWatermark(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()

	later := spawnEv(h.id, "run-after-fence", "task-after-fence", tasks.KindForeground, "")
	later.Sequence = 3
	src := &scriptedProjectionSource{
		wm: 3,
		pages: []events.ProjectionPage{
			{Next: 0, Watermark: 3, Quality: events.ProjectionCatchingUp},
			{Events: []events.Event{later}, Next: 3, Watermark: 3, Quality: events.ProjectionCurrent},
		},
	}
	m, err := New(src, h.proj, WithPageLimit(1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("first Materialize: %v", err)
	}
	if first.Cursor != 0 || first.Quality != events.ProjectionCatchingUp || first.EventsApplied != 0 {
		t.Fatalf("first result = %+v; want unchanged cursor 0 and CatchingUp", first)
	}

	second, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("second Materialize: %v", err)
	}
	if second.Cursor != 3 || second.EventsApplied != 1 {
		t.Fatalf("second result = %+v; want later canonical event applied at cursor 3", second)
	}
	row := mustGetRow(t, h, "task-after-fence")
	if row.RunID != "run-after-fence" || row.Status != turns.StatusPending {
		t.Fatalf("later turn = run %q status %q; want run-after-fence, pending", row.RunID, row.Status)
	}

	third, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("third Materialize: %v", err)
	}
	if third.EventsApplied != 0 || third.Cursor != 3 {
		t.Fatalf("third result = %+v; later canonical event applied more than once", third)
	}
}

func replaySequences(evs []events.Event) []uint64 {
	seqs := make([]uint64, len(evs))
	for i, ev := range evs {
		seqs[i] = ev.Sequence
	}
	return seqs
}
