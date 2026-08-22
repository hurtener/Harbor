package durable

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type countingStore struct {
	state.StateStore
	loads     atomic.Int64
	listKinds atomic.Int64
}

var errInjectedHeadSave = errors.New("injected durable head save failure")

type headFailureStore struct {
	state.StateStore
	failHead atomic.Bool
}

func (s *headFailureStore) Save(ctx context.Context, rec state.StateRecord) error {
	if rec.Kind == kindHead && s.failHead.CompareAndSwap(true, false) {
		return errInjectedHeadSave
	}
	return s.StateStore.Save(ctx, rec)
}

func metadataCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}
}

func (s *countingStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	s.loads.Add(1)
	return s.StateStore.Load(ctx, id, kind)
}

func (s *countingStore) ListKind(ctx context.Context, scope state.ListScope, prefix string) ([]state.StateRecord, error) {
	s.listKinds.Add(1)
	return s.StateStore.ListKind(ctx, scope, prefix)
}

func (s *countingStore) resetCounters() {
	s.loads.Store(0)
	s.listKinds.Store(0)
}

func metadataBus(t *testing.T, store state.StateStore) events.EventBus {
	t.Helper()
	bus, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func metadataIdentity() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
}

func seedLegacyHistory(t *testing.T, store state.StateStore, n int, at time.Time) {
	t.Helper()
	id := metadataIdentity()
	seqs := make([]uint64, 0, n)
	for i := 0; i < n; i++ {
		typ := events.EventTypeRuntimeWarning
		if i == n-1 {
			typ = events.EventTypeRuntimeError
		}
		seq := uint64(i + 1)
		ev := events.Event{
			Type:       typ,
			Identity:   id,
			OccurredAt: at.Add(time.Duration(i) * time.Millisecond),
			Sequence:   seq,
			Payload: events.BusDroppedPayload{
				FromSeq: 1, ToSeq: 1, DroppedCount: 0, SubscriberID: 0,
			},
		}
		bytes, err := encodeEvent(ev)
		if err != nil {
			t.Fatalf("encodeEvent(%d): %v", i, err)
		}
		if err := store.Save(context.Background(), state.StateRecord{
			ID: state.NewEventID(), Identity: id, Kind: kindEntryPrefix + seqToken(seq), Bytes: bytes,
		}); err != nil {
			t.Fatalf("seed entry(%d): %v", i, err)
		}
		seqs = append(seqs, seq)
	}
	headBytes, err := encodeHead(headRecord{Sequences: seqs})
	if err != nil {
		t.Fatalf("encode legacy head: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{
		ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headBytes,
	}); err != nil {
		t.Fatalf("seed head: %v", err)
	}
}

func TestDurable_MetadataIndex_BoundsPayloadLoadsForSparseAndZeroMatches(t *testing.T) {
	base := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	store := &countingStore{StateStore: inner}
	seedLegacyHistory(t, store, 25000, base)
	bus := metadataBus(t, store)
	store.resetCounters()

	filter := events.EventListQuery{
		Filter: eventsWireFilter(metadataIdentity(), []string{string(events.EventTypeRuntimeError)}),
		Limit:  200,
	}
	hr := bus.(events.HistoryReplayer)
	page, err := hr.ListWindow(context.Background(), filter)
	if err != nil {
		t.Fatalf("sparse ListWindow: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Type != events.EventTypeRuntimeError {
		t.Fatalf("sparse page = %d events, want one runtime.error", len(page.Events))
	}
	if got := store.loads.Load(); got > 2 {
		t.Fatalf("sparse ListWindow loaded %d StateStore records, want head + one payload", got)
	}

	store.resetCounters()
	zero := filter
	zero.Filter.EventTypes = []string{string(events.EventTypeGovernanceBudgetExceeded)}
	page, err = hr.ListWindow(context.Background(), zero)
	if err != nil {
		t.Fatalf("zero ListWindow: %v", err)
	}
	if len(page.Events) != 0 || page.HasMore {
		t.Fatalf("zero page = %+v, want empty exact page", page)
	}
	if got := store.loads.Load(); got > 1 {
		t.Fatalf("zero ListWindow loaded %d StateStore records, want head only", got)
	}
}

func TestDurable_MetadataIndex_RestartIsIdempotentAndMalformedRowsFailLoudly(t *testing.T) {
	base := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	store := &countingStore{StateStore: inner}
	seedLegacyHistory(t, store, 8, base)
	first := metadataBus(t, store)
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	store.resetCounters()
	second, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("restart with indexed head: %v", err)
	}
	if got := store.loads.Load(); got > 2 {
		t.Fatalf("restart loaded %d payload records, want head + retention seed only", got)
	}
	if got := store.listKinds.Load(); got != 1 {
		t.Fatalf("restart ListKind calls = %d, want one recovery scan", got)
	}
	_ = second.Close(context.Background())

	// An unknown type is a malformed projection and must refuse boot rather
	// than letting an unrelated event ledger satisfy the index contract.
	rec, err := store.Load(context.Background(), metadataIdentity(), kindHead)
	if err != nil {
		t.Fatalf("load head: %v", err)
	}
	head, err := decodeHead(rec.Bytes)
	if err != nil {
		t.Fatalf("decode head: %v", err)
	}
	head.Metadata[0].Type = events.EventType("events.unknown.test")
	bytes, err := encodeHead(head)
	if err != nil {
		t.Fatalf("encode malformed head: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: metadataIdentity(), Kind: kindHead, Bytes: bytes}); err != nil {
		t.Fatalf("save malformed head: %v", err)
	}
	if _, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store); err == nil {
		t.Fatal("New accepted metadata with an unknown event type")
	}
}

func TestDurable_MetadataIndex_ValidButStalePayloadFailsOnRead(t *testing.T) {
	base := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	store := &countingStore{StateStore: inner}
	seedLegacyHistory(t, store, 1, base)
	bus := metadataBus(t, store)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rec, err := store.Load(context.Background(), metadataIdentity(), kindHead)
	if err != nil {
		t.Fatalf("load head: %v", err)
	}
	head, err := decodeHead(rec.Bytes)
	if err != nil {
		t.Fatalf("decode head: %v", err)
	}
	head.Metadata[0].Type = events.EventTypeGovernanceRateLimited // valid, but not the payload type
	bytes, err := encodeHead(head)
	if err != nil {
		t.Fatalf("encode stale head: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: metadataIdentity(), Kind: kindHead, Bytes: bytes}); err != nil {
		t.Fatalf("save stale head: %v", err)
	}
	readBus, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("New with structurally valid stale metadata: %v", err)
	}
	t.Cleanup(func() { _ = readBus.Close(context.Background()) })
	_, err = readBus.(events.HistoryReplayer).ListWindow(context.Background(), events.EventListQuery{
		Filter: eventsWireFilter(metadataIdentity(), nil), Limit: 1,
	})
	if err == nil {
		t.Fatal("ListWindow accepted metadata that disagrees with its payload")
	}
}

func TestDurable_HeadSaveFailure_PoisonsRetryAndRestartMakesOmissionExplicit(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	store := &headFailureStore{StateStore: inner}
	bus, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("initial durable.New: %v", err)
	}
	id := metadataIdentity()
	publish := func(marker string) error {
		return bus.Publish(context.Background(), events.Event{
			Type:       events.EventTypeRuntimeWarning,
			Identity:   id,
			OccurredAt: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
			Payload: events.BusDroppedPayload{
				FromSeq: 1, ToSeq: 1, SubscriberID: 1,
			},
			Extra: map[string]string{"marker": marker},
		})
	}
	if err := publish("committed-before-fault"); err != nil {
		t.Fatalf("publish before fault: %v", err)
	}
	store.failHead.Store(true)
	if err := publish("entry-only-fault"); !errors.Is(err, errInjectedHeadSave) {
		t.Fatalf("faulted publish error = %v, want injected head-save failure", err)
	}

	// The entry write succeeded before the head write was rejected. It is
	// intentionally orphaned: the index never exposes it as a committed
	// event, and recovery must not infer publication from an entry alone.
	// This is the concrete two-write atomicity boundary: the failed event may
	// be lost, but no caller-visible Publish success can be omitted; a
	// transaction/atomic journal would be needed for all-or-nothing failure
	// semantics across both records.
	if _, err := inner.Load(context.Background(), id, kindEntryPrefix+seqToken(2)); err != nil {
		t.Fatalf("faulted entry was not persisted before head failure: %v", err)
	}
	if err := publish("retry-before-restart"); err == nil {
		t.Fatal("retry on poisoned bus succeeded; ambiguous persistence must require restart")
	}
	page, err := bus.(events.HistoryReplayer).ListWindow(context.Background(), events.EventListQuery{
		Filter: eventsWireFilter(id, nil), Limit: 10,
	})
	if err != nil {
		t.Fatalf("read before restart: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Extra["marker"] != "committed-before-fault" {
		t.Fatalf("pre-restart history = %+v, want only the successfully committed event", page.Events)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("close poisoned bus: %v", err)
	}

	restarted, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("restart after ambiguous head write: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	if err := restarted.Publish(context.Background(), events.Event{
		Type:       events.EventTypeRuntimeWarning,
		Identity:   id,
		OccurredAt: time.Date(2026, 8, 22, 0, 0, 1, 0, time.UTC),
		Payload:    events.BusDroppedPayload{FromSeq: 2, ToSeq: 2, SubscriberID: 2},
		Extra:      map[string]string{"marker": "committed-after-restart"},
	}); err != nil {
		t.Fatalf("publish after restart: %v", err)
	}
	page, err = restarted.(events.HistoryReplayer).ListWindow(context.Background(), events.EventListQuery{
		Filter: eventsWireFilter(id, nil), Limit: 10,
	})
	if err != nil {
		t.Fatalf("read after restart: %v", err)
	}
	if len(page.Events) != 2 || page.Events[0].Extra["marker"] != "committed-before-fault" || page.Events[1].Extra["marker"] != "committed-after-restart" {
		t.Fatalf("post-restart history = %+v, want committed event plus explicit retry", page.Events)
	}
}

func eventsWireFilter(id identity.Quadruple, types []string) prototypes.EventFilter {
	return prototypes.EventFilter{TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID}, SessionIDs: []string{id.SessionID}, EventTypes: types}
}
