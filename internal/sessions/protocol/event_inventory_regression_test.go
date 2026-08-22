package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

const inventoryEntryStem = "events.durable.entry/"

// inventoryCountingStore holds durable event-head reads until the request is
// cancelled. It makes the real StateStore acquisition boundary observable:
// the test can prove that the eight projector workers are joined before the
// cancelled sessions.list returns, and that no event payload entry is loaded.
type inventoryCountingStore struct {
	state.StateStore

	loads      atomic.Int64
	entryLoads atomic.Int64
	active     atomic.Int64
	peak       atomic.Int64
	postReturn atomic.Int64
	returned   atomic.Bool
	block      atomic.Bool

	started    chan struct{}
	cancelSeen chan struct{}
	release    <-chan struct{}
}

func (s *inventoryCountingStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	s.loads.Add(1)
	if strings.HasPrefix(kind, inventoryEntryStem) {
		s.entryLoads.Add(1)
	}
	if !s.block.Load() {
		return s.StateStore.Load(ctx, id, kind)
	}

	if s.returned.Load() {
		s.postReturn.Add(1)
	}
	active := s.active.Add(1)
	for {
		peak := s.peak.Load()
		if active <= peak || s.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	defer s.active.Add(-1)
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		select {
		case s.cancelSeen <- struct{}{}:
		default:
		}
		<-s.release
		return state.StateRecord{}, ctx.Err()
	case <-s.release:
		return s.StateStore.Load(ctx, id, kind)
	}
}

func (s *inventoryCountingStore) reset() {
	s.loads.Store(0)
	s.entryLoads.Store(0)
	s.active.Store(0)
	s.peak.Store(0)
	s.postReturn.Store(0)
	s.returned.Store(false)
}

func TestService_SessionsList_DurableInventoryBoundsLoadsAndJoinsCancellation(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	store := &inventoryCountingStore{
		StateStore: inner,
		started:    make(chan struct{}, maxCounterEnrichmentWorkers+16),
		cancelSeen: make(chan struct{}, maxCounterEnrichmentWorkers+16),
		release:    release,
	}
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	cfg := config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}
	bus, err := durable.New(context.Background(), cfg, passRedactor{}, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	const sessionCount = maxCounterEnrichmentWorkers + 24
	base := time.Date(2026, 8, 22, 4, 0, 0, 0, time.UTC)
	snapshots := make([]sessions.SessionSnapshot, 0, sessionCount)
	for i := range sessionCount {
		sessionID := fmt.Sprintf("inventory-%03d", i)
		id := identity.Identity{TenantID: "tenant-fleet", UserID: "user-fleet", SessionID: sessionID}
		if err := bus.Publish(context.Background(), events.Event{
			Type:       events.EventTypeRuntimeWarning,
			Identity:   identity.Quadruple{Identity: id},
			OccurredAt: base.Add(time.Duration(i) * time.Second),
			Payload: events.BusDroppedPayload{
				FromSeq: 1, ToSeq: 1, DroppedCount: 0, SubscriberID: uint64(i),
			},
		}); err != nil {
			t.Fatalf("publish inventory event %d: %v", i, err)
		}
		snapshots = append(snapshots, sessions.SessionSnapshot{Session: sessions.Session{
			ID:       sessionID,
			Identity: id,
			OpenedAt: base,
			LastSeen: base.Add(time.Duration(i) * time.Second),
		}})
	}
	store.reset()
	store.block.Store(true)

	reg := newTaskRegistry(t, bus)
	enricher := newEnricher(t, bus, reg, pauseresume.New())
	projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	baseline := settledGoroutineCount()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, listErr := svc.List(ctx, prototypes.SessionsListRequest{
			Identity: prototypes.IdentityScope{Tenant: "tenant-fleet", User: "user-fleet", Session: "request-session"},
			// Cost sorting forces the full inventory/counter path. A lifecycle
			// sort would page before enrichment and would not exercise all
			// candidate sessions.
			Sort:  prototypes.SessionSortCostDesc,
			Limit: sessionCount,
		}, false)
		done <- listErr
	}()

	for range maxCounterEnrichmentWorkers {
		select {
		case <-store.started:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for durable event-store loads")
		}
	}
	if got := store.peak.Load(); got != int64(maxCounterEnrichmentWorkers) {
		t.Fatalf("peak durable StateStore Load acquisitions = %d, want worker bound %d", got, maxCounterEnrichmentWorkers)
	}
	cancel()
	for range maxCounterEnrichmentWorkers {
		select {
		case <-store.cancelSeen:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for durable loads to observe cancellation")
		}
	}
	assertListStillJoining(t, done)
	releaseOnce.Do(func() { close(release) })
	assertCanceledListResult(t, done)
	store.returned.Store(true)

	if got := store.active.Load(); got != 0 {
		t.Fatalf("active durable StateStore Load calls after canceled return = %d, want 0", got)
	}
	if got := store.loads.Load(); got > int64(maxCounterEnrichmentWorkers) {
		t.Fatalf("durable StateStore Load calls = %d, want <= %d", got, maxCounterEnrichmentWorkers)
	}
	if got := store.entryLoads.Load(); got != 0 {
		t.Fatalf("durable event payload entry loads = %d, want 0 (metadata-only counters)", got)
	}
	if got := store.postReturn.Load(); got != 0 {
		t.Fatalf("durable StateStore Load calls after sessions.list returned = %d, want 0", got)
	}
	if leaked := settledGoroutineCount() - baseline; leaked > 2 {
		t.Fatalf("goroutine leak after canceled durable inventory = %d above baseline", leaked)
	}
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("request context error = %v, want context.Canceled", err)
	}
}
