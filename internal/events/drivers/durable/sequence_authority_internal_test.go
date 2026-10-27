package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

type unknownCommitStore struct{ state.StateStore }

func (s *unknownCommitStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	if err := s.StateStore.SaveBatchIf(ctx, expectations, writes); err != nil {
		return err
	}
	return fmt.Errorf("ambiguous commit acknowledgement: %w", errors.Join(state.ErrCommitOutcomeUnknown, context.Canceled))
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
	b.store = wrapped.StateStore
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish after definite CAS exhaustion: %v", err)
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
	b.store = wrapped.StateStore
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish after definite cancellation: %v", err)
	}
}

func TestSequenceAuthority_UnknownCommitThatLandedReconcilesToSuccess(t *testing.T) {
	b := newAuthorityTestBus(t)
	inner := b.store
	b.store = &unknownCommitStore{StateStore: inner}
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish with landed-but-ambiguous commit = %v, want reconciled success", err)
	}
	if b.PersistenceHealthy() != nil || b.pendingCommit != nil {
		t.Fatal("landed ambiguous commit left a pending reconciliation")
	}
	b.store = inner
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish after reconciled commit: %v", err)
	}
	if wm, err := b.Watermark(context.Background()); err != nil || wm != 2 {
		t.Fatalf("Watermark = %d, %v; want 2 (no sequence reuse)", wm, err)
	}
}

func TestSequenceAuthority_UnknownCommitThatDidNotLandIsRetryable(t *testing.T) {
	b := newAuthorityTestBus(t)
	inner := b.store
	b.store = &reconcileStore{StateStore: inner, apply: false}
	err := b.Publish(context.Background(), authorityTestEvent())
	if !errors.Is(err, errCommitNotApplied) || errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("Publish = %v, want definite errCommitNotApplied", err)
	}
	if b.pendingCommit != nil {
		t.Fatal("absent ambiguous commit left a pending reconciliation")
	}
	b.store = inner
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("retry after reconciled-absent commit: %v", err)
	}
	if wm, err := b.Watermark(context.Background()); err != nil || wm != 1 {
		t.Fatalf("Watermark = %d, %v; want 1", wm, err)
	}
}

func TestSequenceAuthority_ReconcileFailureThenRecoveryClears(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(fmt.Sprintf("applied=%v", applied), func(t *testing.T) {
			clock := &steppedClock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
			store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			opened, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store, WithClock(clock))
			if err != nil {
				t.Fatal(err)
			}
			b := opened.(*bus)
			t.Cleanup(func() { _ = b.Close(context.Background()) })
			wrapped := &reconcileStore{StateStore: store, apply: applied}
			wrapped.failLoads.Store(true)
			b.store = wrapped
			if err := b.Publish(context.Background(), authorityTestEvent()); !errors.Is(err, state.ErrCommitOutcomeUnknown) {
				t.Fatalf("Publish = %v, want unresolved ErrCommitOutcomeUnknown", err)
			}
			if b.pendingCommit == nil {
				t.Fatal("unresolved reconciliation was not recorded as pending")
			}
			if err := b.Publish(context.Background(), authorityTestEvent()); !errors.Is(err, errCommitReconcilePending) {
				t.Fatalf("Publish while pending = %v, want errCommitReconcilePending", err)
			}
			clock.advance(persistenceUnhealthyAfter)
			if err := b.PersistenceHealthy(); err != nil {
				t.Fatalf("PersistenceHealthy at the bound = %v, want healthy", err)
			}
			clock.advance(time.Second)
			if err := b.PersistenceHealthy(); !errors.Is(err, errCommitReconcilePending) {
				t.Fatalf("PersistenceHealthy past the bound = %v, want errCommitReconcilePending", err)
			}

			wrapped.failLoads.Store(false)
			wrapped.ambiguous.Store(false)
			if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
				t.Fatalf("Publish after store recovery: %v", err)
			}
			if b.pendingCommit != nil || b.PersistenceHealthy() != nil {
				t.Fatal("recovered reconciliation did not clear the pending state")
			}
			want := uint64(1)
			if applied {
				want = 2
			}
			if wm, err := b.Watermark(context.Background()); err != nil || wm != want {
				t.Fatalf("Watermark = %d, %v; want %d", wm, err, want)
			}
		})
	}
}

// reconcileStore reports every SaveBatchIf as ambiguous (optionally applying
// it first) and can fail Loads to simulate an unreachable store during
// reconciliation.
type reconcileStore struct {
	state.StateStore
	apply     bool
	failLoads atomic.Bool
	ambiguous atomic.Bool
	init      sync.Once
}

func (s *reconcileStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	s.init.Do(func() { s.ambiguous.Store(true) })
	if !s.ambiguous.Load() {
		return s.StateStore.SaveBatchIf(ctx, expectations, writes)
	}
	if s.apply {
		if err := s.StateStore.SaveBatchIf(ctx, expectations, writes); err != nil {
			return err
		}
	}
	return fmt.Errorf("ambiguous commit acknowledgement: %w", errors.Join(state.ErrCommitOutcomeUnknown, errors.New("connection reset")))
}

func (s *reconcileStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if s.failLoads.Load() && strings.HasPrefix(kind, kindEntryPrefix) {
		return state.StateRecord{}, errors.New("store unreachable")
	}
	return s.StateStore.Load(ctx, q, kind)
}

type steppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
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
	if err := store.Save(context.Background(), state.NewInternalRecord(
		state.NewEventID(), sequenceAuthorityIdentity, kindSequenceAuthority, payload,
	)); err != nil {
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

func TestSequenceAuthority_SentinelIdentityDeleteScopeCannotEraseAuthority(t *testing.T) {
	b := newAuthorityTestBus(t)
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.DeleteScope(context.Background(), sequenceAuthorityIdentity.Identity); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.loadSequenceAuthority(context.Background()); err != nil {
		t.Fatalf("authority after sentinel DeleteScope: %v", err)
	}
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatal(err)
	}
	wm, err := b.Watermark(context.Background())
	if err != nil || wm != 2 {
		t.Fatalf("Watermark after sentinel erasure = %d, %v; want 2", wm, err)
	}
}

func TestSequenceAuthority_LegacyWriterDrainBarrier(t *testing.T) {
	empty, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := metadataCfg()
	cfg.LegacyWritersDrained = false
	if _, err := New(context.Background(), cfg, auditpatterns.New(), empty); err == nil || !strings.Contains(err.Error(), "events.legacy_writers_drained=true") {
		t.Fatalf("authority-absent boot without acknowledgement = %v", err)
	}
	if _, err := empty.Load(context.Background(), sequenceAuthorityIdentity, kindSequenceAuthority); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("failed admission published authority: %v", err)
	}
	cfg.LegacyWritersDrained = true
	fresh, err := New(context.Background(), cfg, auditpatterns.New(), empty)
	if err != nil {
		t.Fatalf("acknowledged empty-store boot: %v", err)
	}
	_ = fresh.Close(context.Background())
	cfg.LegacyWritersDrained = false
	restarted, err := New(context.Background(), cfg, auditpatterns.New(), empty)
	if err != nil {
		t.Fatalf("already-authority restart required acknowledgement: %v", err)
	}
	_ = restarted.Close(context.Background())

	legacy, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyHistory(t, legacy, 1, time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC))
	if _, err := New(context.Background(), cfg, auditpatterns.New(), legacy); err == nil || !strings.Contains(err.Error(), "events.legacy_writers_drained=true") {
		t.Fatalf("legacy boot without acknowledgement = %v", err)
	}
	cfg.LegacyWritersDrained = true
	adopted, err := New(context.Background(), cfg, auditpatterns.New(), legacy)
	if err != nil {
		t.Fatalf("legacy boot after acknowledgement: %v", err)
	}
	_ = adopted.Close(context.Background())
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
