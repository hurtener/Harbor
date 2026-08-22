package durable

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type authorityConflictStore struct {
	state.StateStore
	calls  atomic.Int64
	cancel context.CancelFunc
}

func (s *authorityConflictStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	if len(writes) == 3 {
		s.calls.Add(1)
		if s.cancel != nil {
			s.cancel()
		}
		return state.ErrConditionFailed
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

func TestSequenceAuthority_ConflictRetryIsBounded(t *testing.T) {
	b := newAuthorityTestBus(t)
	wrapped := &authorityConflictStore{StateStore: b.store}
	b.store = wrapped
	err := b.Publish(context.Background(), authorityTestEvent())
	if !errors.Is(err, state.ErrConditionFailed) {
		t.Fatalf("Publish = %v, want ErrConditionFailed", err)
	}
	if got := wrapped.calls.Load(); got != publishCASMaxAttempts {
		t.Fatalf("batch attempts = %d, want %d", got, publishCASMaxAttempts)
	}
}

func TestSequenceAuthority_ConflictRetryHonorsCancellation(t *testing.T) {
	b := newAuthorityTestBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	wrapped := &authorityConflictStore{StateStore: b.store, cancel: cancel}
	b.store = wrapped
	err := b.Publish(ctx, authorityTestEvent())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish = %v, want context.Canceled", err)
	}
	if got := wrapped.calls.Load(); got != 1 {
		t.Fatalf("batch attempts after cancellation = %d, want 1", got)
	}
}

func TestSequenceAuthority_RestartNeverLowersPersistedAuthority(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(sequenceAuthorityRecord{Sequence: 50})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), state.StateRecord{
		ID: state.NewEventID(), Identity: sequenceAuthorityIdentity,
		Kind: kindSequenceAuthority, Bytes: payload,
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatal(err)
	}
	b := opened.(*bus)
	defer func() { _ = b.Close(context.Background()) }()
	if b.nextSeq != 50 {
		t.Fatalf("recovered nextSeq = %d, want authority floor 50", b.nextSeq)
	}
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatal(err)
	}
	wm, err := b.Watermark(context.Background())
	if err != nil || wm != 51 {
		t.Fatalf("Watermark = %d, %v; want 51", wm, err)
	}
}

func newAuthorityTestBus(t *testing.T) *bus {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatal(err)
	}
	b := opened.(*bus)
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

func authorityTestEvent() events.Event {
	return events.Event{
		Type: events.EventTypeRuntimeWarning,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID: "tenant", UserID: "user", SessionID: "session",
		}},
		Payload: events.BusDroppedPayload{FromSeq: 1, ToSeq: 1, SubscriberID: 1},
	}
}
