package protocol

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

func TestSignedAttachBackoff_BoundedAndFingerprintScoped(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	b := newSignedAttachBackoff()
	b.now = func() time.Time { return now }
	for i := range signedAttachBackoffEntries * 3 {
		b.fail(signedAttachBackoffKey{scope: agentcfg.ConfigScopeAgent, tenant: "t", agent: fmt.Sprint(i), provider: "p"}, "fp")
		if b.len() > signedAttachBackoffEntries {
			t.Fatalf("entries = %d, exceeds bound %d", b.len(), signedAttachBackoffEntries)
		}
	}
	key := signedAttachBackoffKey{scope: agentcfg.ConfigScopeAgent, tenant: "t", agent: "fresh", provider: "p"}
	b.fail(key, "fp-1")
	if err := b.admit(key, "fp-1"); !errors.Is(err, ErrSignedCapabilityAttachSuppressed) {
		t.Fatalf("same fingerprint admit = %v, want suppression", err)
	}
	// Any change to the revision, receipt or phase retries immediately.
	if err := b.admit(key, "fp-2"); err != nil {
		t.Fatalf("changed fingerprint admit = %v, want immediate retry", err)
	}
	if got := signedAttachBackoffDelay(1000); got != signedAttachBackoffMax {
		t.Fatalf("delay cap = %v", got)
	}
}

func TestSignedReconcileContinuations_Bounded(t *testing.T) {
	c := newSignedReconcileContinuations()
	for i := range signedReconcileContinuationEntries * 2 {
		c.set(fmt.Sprint(i), "cursor")
	}
	if len(c.entries) > signedReconcileContinuationEntries {
		t.Fatalf("continuations = %d, exceeds bound", len(c.entries))
	}
	c.set("done", "cursor")
	c.set("done", "")
	if _, ok := c.entries["done"]; ok {
		t.Fatal("completed scan cursor retained")
	}
}
