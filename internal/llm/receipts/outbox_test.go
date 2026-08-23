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
