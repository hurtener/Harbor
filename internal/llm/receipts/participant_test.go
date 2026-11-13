package receipts

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// A participant's turn is billed to the participant: the receipt must name
// the writer, never the partition owner, and still delivers.
func TestOutbox_ParticipantReceiptNamesWriter(t *testing.T) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	delivery := &recordingDelivery{}
	o, err := New(Config{Store: store, Delivery: delivery, BaseBackoff: time.Millisecond, MaxBackoff: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	receipt := testReceipt()
	partition := identity.Identity{TenantID: receipt.TenantID, UserID: "partition-owner", SessionID: receipt.SessionID}
	ctx, err := identity.WithRun(context.Background(), partition, receipt.LogicalRunID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: receipt.UserID})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Enqueue(ctx, receipt); err != nil {
		t.Fatalf("participant receipt naming the writer: %v", err)
	}
	owned := receipt
	owned.ReceiptID, owned.UserID = receipt.ReceiptID+"-owner", "partition-owner"
	if err := o.Enqueue(ctx, owned); err == nil {
		t.Fatal("a participant's turn must not enqueue a receipt billed to the partition owner")
	}
	if stats, err := o.Replay(ctx); err != nil || stats.Acknowledged != 1 {
		t.Fatalf("replay = %+v, %v", stats, err)
	}
}
