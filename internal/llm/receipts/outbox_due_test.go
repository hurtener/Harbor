package receipts

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

type countingStateStore struct {
	state.StateStore
	mu      sync.Mutex
	bounded int
	loads   int
	due     int
}

type countingPendingSource struct {
	mu    sync.Mutex
	calls int
}

func (s *countingPendingSource) PendingReceipts(context.Context, int) ([]llm.AttemptUsageReceipt, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return nil, nil
}

func (s *countingPendingSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type signalingFailDelivery struct {
	calls chan struct{}
}

func (d *signalingFailDelivery) Deliver(context.Context, llm.AttemptUsageReceipt) error {
	select {
	case d.calls <- struct{}{}:
	default:
	}
	return errors.New("coordinator unavailable")
}

func (s *countingStateStore) ListKindBounded(ctx context.Context, scope state.ListScope, prefix string, limit int) ([]state.StateRecord, error) {
	s.mu.Lock()
	s.bounded++
	s.mu.Unlock()
	return s.StateStore.ListKindBounded(ctx, scope, prefix, limit)
}

func (s *countingStateStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	s.mu.Lock()
	s.loads++
	if q == identity.InternalCoordinationQuadruple() && kind == dueKind {
		s.due++
	}
	s.mu.Unlock()
	return s.StateStore.Load(ctx, q, kind)
}

func (s *countingStateStore) counters() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bounded
}

func (s *countingStateStore) resetCounters() {
	s.mu.Lock()
	s.bounded, s.loads, s.due = 0, 0, 0
	s.mu.Unlock()
}

func (s *countingStateStore) dueLoads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.due
}

func receiptVariant(n int) llm.AttemptUsageReceipt {
	r := testReceipt()
	r.LogicalCallID = "call-" + string(rune('a'+n))
	r.AttemptNonce = "nonce-" + string(rune('a'+n))
	r.ReceiptID = "grant-1/" + r.LogicalCallID + "/" + r.AttemptNonce + "/0/0/0/1"
	r.IdempotencyKey = r.ReceiptID
	r.LogicalRunID = "run-" + string(rune('a'+n))
	r.SessionID = "session-" + string(rune('a'+n))
	r.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(r)
	return r
}

func TestOutboxReplayUsesDurableDueIndexAndHonorsBatch(t *testing.T) {
	base, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(context.Background())
	store := &countingStateStore{StateStore: base}
	delivery := &recordingDelivery{}
	o, err := New(Config{Store: store, Delivery: delivery, MaxBatch: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		r := receiptVariant(i)
		if err := o.Enqueue(receiptContext(t, r), r); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	store.resetCounters()
	stats, err := o.Replay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bounded := store.counters()
	if bounded != 0 {
		t.Fatalf("Replay performed %d global prefix scans", bounded)
	}
	if stats.Acknowledged != 2 || len(delivery.receipts) != 2 {
		t.Fatalf("stats=%+v delivered=%d, want exactly one batch", stats, len(delivery.receipts))
	}
	stats, err = o.Replay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Acknowledged != 2 {
		t.Fatalf("second batch stats=%+v", stats)
	}
	stats, err = o.Replay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Acknowledged != 1 {
		t.Fatalf("final batch stats=%+v", stats)
	}
}

func TestOutboxDeferredReceiptDoesNotSpin(t *testing.T) {
	base, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(context.Background())
	store := &countingStateStore{StateStore: base}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	delivery := &recordingDelivery{err: errors.New("down")}
	o, err := New(Config{Store: store, Delivery: delivery, BaseBackoff: time.Hour, MaxBackoff: time.Hour, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	r := testReceipt()
	if err := o.Enqueue(receiptContext(t, r), r); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Replay(context.Background()); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("first replay=%v", err)
	}
	store.resetCounters()
	stats, err := o.Replay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bounded := store.counters()
	if bounded != 0 || stats.Deferred != 1 || stats.Failed != 0 {
		t.Fatalf("stats=%+v bounded=%d, want deferred without retry scan", stats, bounded)
	}
}

func TestOutboxRunIdleReconciliationDoesNotGrowQueries(t *testing.T) {
	base, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(context.Background())
	store := &countingStateStore{StateStore: base}
	o, err := New(Config{Store: store, Delivery: &recordingDelivery{}, ReconcileInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	boundedBefore := store.counters()
	time.Sleep(50 * time.Millisecond)
	boundedAfter := store.counters()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run=%v", err)
	}
	if boundedBefore == 0 || boundedAfter != boundedBefore {
		t.Fatalf("idle reconciliation prefix scans grew: before=%d after=%d", boundedBefore, boundedAfter)
	}
}

func TestOutboxRunDueRetriesDoNotAccelerateReconciliation(t *testing.T) {
	base, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(context.Background())
	store := &countingStateStore{StateStore: base}
	pending := &countingPendingSource{}
	delivery := &signalingFailDelivery{calls: make(chan struct{}, 16)}
	o, err := New(Config{
		Store: store, Delivery: delivery, PendingSource: pending,
		BaseBackoff: 10 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
		CircuitFailures: 1000, ReconcileInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt()
	if err := o.Enqueue(receiptContext(t, receipt), receipt); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	for attempt := 0; attempt < 4; attempt++ {
		select {
		case <-delivery.calls:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatalf("delivery attempt %d did not occur", attempt+1)
		}
	}
	if got := pending.callCount(); got != 1 {
		cancel()
		t.Fatalf("pending-source calls=%d, want startup reconciliation only", got)
	}
	if got := store.counters(); got != 1 {
		cancel()
		t.Fatalf("maintenance prefix scans=%d, want startup reconciliation only", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run=%v", err)
	}
}

func TestOutboxRunCircuitOpenSleepsWithoutDueIndexSpin(t *testing.T) {
	base, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(context.Background())
	store := &countingStateStore{StateStore: base}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	delivery := &recordingDelivery{err: errors.New("coordinator unavailable")}
	o, err := New(Config{
		Store: store, Delivery: delivery, BaseBackoff: time.Millisecond,
		MaxBackoff: time.Millisecond, CircuitFailures: 1, CircuitOpenFor: time.Second,
		ReconcileInterval: time.Hour, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt()
	if err := o.Enqueue(receiptContext(t, receipt), receipt); err != nil {
		t.Fatal(err)
	}
	store.resetCounters()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	time.Sleep(40 * time.Millisecond)
	first := store.dueLoads()
	calls := delivery.callCount()
	time.Sleep(80 * time.Millisecond)
	second := store.dueLoads()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run=%v", err)
	}
	if calls != 1 {
		t.Fatalf("delivery calls=%d, want one before circuit sleep", calls)
	}
	if first == 0 || first > 4 || second != first {
		t.Fatalf("due-index loads grew while circuit open: first=%d second=%d", first, second)
	}
}

func TestOutboxReconcileDoesNotRequeueAcknowledgedReceipts(t *testing.T) {
	base, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(context.Background())
	store := &countingStateStore{StateStore: base}
	o, err := New(Config{Store: store, Delivery: &recordingDelivery{}})
	if err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt()
	ctx := receiptContext(t, receipt)
	if err := o.Enqueue(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	stats, err := o.Replay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Acknowledged != 1 {
		t.Fatalf("ack stats=%+v", stats)
	}
	_, before, err := o.loadIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Entries) != 0 {
		t.Fatalf("acknowledged receipt remained due before reconcile: %+v", before.Entries)
	}
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	_, after, err := o.loadIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Entries) != 0 {
		t.Fatalf("reconcile requeued acknowledged receipt: %+v", after.Entries)
	}
}

func TestOutboxPrepareRefusesLegacyOverflowWithoutHidingLaterPending(t *testing.T) {
	tests := map[string]func(*testing.T) state.StateStore{
		"inmem": func(t *testing.T) state.StateStore {
			t.Helper()
			store, err := inmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
		"sqlite": func(t *testing.T) state.StateStore {
			t.Helper()
			store, err := sqlite.New(config.StateConfig{DSN: ":memory:"})
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}
	for name, open := range tests {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			t.Cleanup(func() { _ = store.Close(context.Background()) })
			for i := range 3 {
				receipt := receiptVariant(i)
				status := "acked"
				if i == 2 {
					status = "pending"
				}
				body, err := json.Marshal(storedReceipt{Receipt: receipt, Status: status})
				if err != nil {
					t.Fatal(err)
				}
				kind := receiptKind(receipt.ReceiptID)
				next := state.NewInternalRecord(state.NewEventID(), identity.Quadruple{Identity: identity.Identity{TenantID: receipt.TenantID, UserID: receipt.UserID, SessionID: receipt.SessionID}, RunID: receipt.LogicalRunID}, kind, body)
				if err := store.SaveIf(context.Background(), []state.SlotExpectation{state.InternalSlotExpectation(next.Identity, kind, "")}, next); err != nil {
					t.Fatal(err)
				}
			}
			o, err := New(Config{Store: store, Delivery: &recordingDelivery{}, MaxBatch: 2})
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Prepare(context.Background()); !errors.Is(err, ErrReconcileOverflow) {
				t.Fatalf("Prepare=%v, want ErrReconcileOverflow", err)
			}
			if _, idx, err := o.loadIndex(context.Background()); !errors.Is(err, state.ErrNotFound) && (err != nil || len(idx.Entries) != 0) {
				t.Fatalf("overflow partially adopted legacy page: index=%+v err=%v", idx, err)
			}
		})
	}
}
