package receipts

import (
	"context"
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

type recordingDelivery struct {
	mu       sync.Mutex
	receipts []llm.AttemptUsageReceipt
	err      error
	calls    int
}

type scriptedBatchDelivery struct {
	mu      sync.Mutex
	calls   [][]llm.AttemptUsageReceipt
	results []struct {
		acks []DeliveryAck
		err  error
	}
}

type blockingBatchDelivery struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (d *blockingBatchDelivery) Deliver(context.Context, llm.AttemptUsageReceipt) error {
	return errors.New("single delivery must not be used")
}

func (d *blockingBatchDelivery) DeliverBatch(ctx context.Context, batch []llm.AttemptUsageReceipt) ([]DeliveryAck, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	select {
	case d.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.release:
	}
	acks := make([]DeliveryAck, len(batch))
	for i := range batch {
		acks[i] = DeliveryAck{ReceiptID: batch[i].ReceiptID, CanonicalBodyHash: batch[i].CanonicalBodyHash}
	}
	return acks, nil
}

func (d *scriptedBatchDelivery) Deliver(context.Context, llm.AttemptUsageReceipt) error {
	return errors.New("single delivery must not be used")
}

func (d *scriptedBatchDelivery) DeliverBatch(_ context.Context, batch []llm.AttemptUsageReceipt) ([]DeliveryAck, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, append([]llm.AttemptUsageReceipt(nil), batch...))
	if len(d.results) == 0 {
		return nil, errors.New("unexpected batch")
	}
	result := d.results[0]
	d.results = d.results[1:]
	return append([]DeliveryAck(nil), result.acks...), result.err
}

func (d *recordingDelivery) Deliver(_ context.Context, receipt llm.AttemptUsageReceipt) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil {
		return d.err
	}
	d.receipts = append(d.receipts, receipt)
	return nil
}

func (d *recordingDelivery) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func receiptContext(t *testing.T, receipt llm.AttemptUsageReceipt) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: receipt.TenantID, UserID: receipt.UserID, SessionID: receipt.SessionID}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, id, receipt.LogicalRunID)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func testReceipt() llm.AttemptUsageReceipt {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	receipt := llm.AttemptUsageReceipt{
		ReceiptID: "grant-1/call-1/nonce-1/0/0/0/1", GrantID: "grant-1", LogicalCallID: "call-1", AttemptNonce: "nonce-1", OrganizationID: "org-a", RuntimeID: "runtime-1",
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", LogicalRunID: "run-a", Provider: "openai",
		ProviderModelID: "model-fast", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, RouteID: "route-a", CredentialAssetGeneration: 1,
		PolicyGeneration: 7, AttemptNumber: 1, Currency: "USD", Status: "success", StartedAt: now, CompletedAt: now.Add(time.Millisecond),
		IdempotencyKey: "grant-1/call-1/nonce-1/0/0/0/1", TotalTokens: 5,
	}
	hash, _ := llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	receipt.CanonicalBodyHash = hash
	return receipt
}

func openStores(t *testing.T) []struct {
	name  string
	store state.StateStore
} {
	t.Helper()
	inMemory, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fileStore, err := sqlite.New(config.StateConfig{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	return []struct {
		name  string
		store state.StateStore
	}{{"inmem", inMemory}, {"sqlite", fileStore}}
}

func TestOutbox_EnqueueReplayAckAndResponseLossIdempotency(t *testing.T) {
	for _, tc := range openStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { _ = tc.store.Close(context.Background()) })
			delivery := &recordingDelivery{}
			o, err := New(Config{Store: tc.store, Delivery: delivery, BaseBackoff: time.Millisecond, MaxBackoff: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			receipt := testReceipt()
			ctx := receiptContext(t, receipt)
			if err := o.Enqueue(ctx, receipt); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if err := o.Enqueue(ctx, receipt); err != nil {
				t.Fatalf("duplicate Enqueue: %v", err)
			}
			stats, err := o.Replay(ctx)
			if err != nil {
				t.Fatalf("Replay: %v", err)
			}
			if stats.Acknowledged != 1 || len(delivery.receipts) != 1 {
				t.Fatalf("stats=%+v delivered=%d", stats, len(delivery.receipts))
			}
			stats, err = o.Replay(ctx)
			if err != nil {
				t.Fatalf("Replay after ACK: %v", err)
			}
			if stats.Acknowledged != 0 {
				t.Fatalf("acked receipt replayed: %+v", stats)
			}
		})
	}
}

func TestOutbox_BackoffAndCircuitBreakerBoundReplayStorm(t *testing.T) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	delivery := &recordingDelivery{err: errors.New("coordinator unavailable")}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	o, err := New(Config{Store: store, Delivery: delivery, BaseBackoff: time.Second, MaxBackoff: time.Minute, CircuitFailures: 2, CircuitOpenFor: time.Minute, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt()
	ctx := receiptContext(t, receipt)
	if err := o.Enqueue(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Replay(ctx); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("first replay = %v, want ErrDeliveryFailed", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := o.Replay(ctx); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("second replay = %v, want ErrDeliveryFailed", err)
	}
	if _, err := o.Replay(ctx); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("third replay = %v, want ErrCircuitOpen", err)
	}
}

func TestOutbox_RejectsReceiptWithChangedCanonicalBody(t *testing.T) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	o, err := New(Config{Store: store, Delivery: &recordingDelivery{}})
	if err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt()
	ctx := receiptContext(t, receipt)
	if err := o.Enqueue(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.TotalTokens++
	receipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err := o.Enqueue(ctx, receipt); !errors.Is(err, llm.ErrUsageReceiptUnavailable) {
		t.Fatalf("changed receipt = %v, want ErrUsageReceiptUnavailable", err)
	}
}

func TestOutbox_BatchPartialAckAndResponseLossReplayOnlyUnackedFacts(t *testing.T) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	first := receiptVariant(0)
	second := receiptVariant(1)
	delivery := &scriptedBatchDelivery{results: []struct {
		acks []DeliveryAck
		err  error
	}{
		{acks: []DeliveryAck{{ReceiptID: first.ReceiptID, CanonicalBodyHash: first.CanonicalBodyHash}}},
		{err: errors.New("response lost")},
		{acks: []DeliveryAck{{ReceiptID: second.ReceiptID, CanonicalBodyHash: second.CanonicalBodyHash}}},
	}}
	o, err := New(Config{Store: store, Delivery: delivery, MaxBatch: 2, BaseBackoff: time.Second, MaxBackoff: time.Second, CircuitFailures: 10, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []llm.AttemptUsageReceipt{first, second} {
		if err := o.Enqueue(receiptContext(t, receipt), receipt); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := o.Replay(context.Background())
	if !errors.Is(err, ErrDeliveryFailed) || stats.Acknowledged != 1 || stats.Failed != 1 {
		t.Fatalf("partial replay stats=%+v err=%v", stats, err)
	}
	now = now.Add(2 * time.Second)
	if _, err := o.Replay(context.Background()); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("response-loss replay=%v", err)
	}
	now = now.Add(2 * time.Second)
	stats, err = o.Replay(context.Background())
	if err != nil || stats.Acknowledged != 1 {
		t.Fatalf("final replay stats=%+v err=%v", stats, err)
	}
	if len(delivery.calls) != 3 {
		t.Fatalf("batch calls=%d, want 3", len(delivery.calls))
	}
	for i := 1; i < len(delivery.calls); i++ {
		if len(delivery.calls[i]) != 1 || delivery.calls[i][0].ReceiptID != second.ReceiptID {
			t.Fatalf("call %d replayed acknowledged fact: %#v", i, delivery.calls[i])
		}
	}
}

func TestOutbox_BatchAckHashMismatchRetainsWholeBatch(t *testing.T) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	receipt := testReceipt()
	delivery := &scriptedBatchDelivery{results: []struct {
		acks []DeliveryAck
		err  error
	}{
		{acks: []DeliveryAck{{ReceiptID: receipt.ReceiptID, CanonicalBodyHash: "wrong"}}},
		{acks: []DeliveryAck{{ReceiptID: receipt.ReceiptID, CanonicalBodyHash: receipt.CanonicalBodyHash}}},
	}}
	o, err := New(Config{Store: store, Delivery: delivery, BaseBackoff: time.Second, MaxBackoff: time.Second, CircuitFailures: 10, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Enqueue(receiptContext(t, receipt), receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Replay(context.Background()); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("mismatched ACK=%v", err)
	}
	now = now.Add(2 * time.Second)
	stats, err := o.Replay(context.Background())
	if err != nil || stats.Acknowledged != 1 || len(delivery.calls) != 2 {
		t.Fatalf("retained replay stats=%+v calls=%d err=%v", stats, len(delivery.calls), err)
	}
}

func TestOutbox_ConcurrentReplayDoesNotDoubleDeliver(t *testing.T) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	delivery := &blockingBatchDelivery{started: make(chan struct{}, 1), release: make(chan struct{})}
	o, err := New(Config{Store: store, Delivery: delivery})
	if err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt()
	if err := o.Enqueue(receiptContext(t, receipt), receipt); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, replayErr := o.Replay(context.Background())
		firstDone <- replayErr
	}()
	select {
	case <-delivery.started:
	case <-time.After(time.Second):
		t.Fatal("first replay did not reach delivery")
	}
	stats, err := o.Replay(context.Background())
	if err != nil || stats != (ReplayStats{}) {
		t.Fatalf("concurrent replay stats=%+v err=%v", stats, err)
	}
	close(delivery.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first replay: %v", err)
	}
	delivery.mu.Lock()
	calls := delivery.calls
	delivery.mu.Unlock()
	if calls != 1 {
		t.Fatalf("delivery calls=%d, want 1", calls)
	}
}
