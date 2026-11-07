package protocol

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
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

func TestSignedAttachBackoff_OwnerSuccessResetsSiblingWindows(t *testing.T) {
	b := newSignedAttachBackoff()
	owner := signedAttachBackoffKey{scope: agentcfg.ConfigScopeAgent, tenant: "t", agent: "a", provider: "p1"}
	sibling := owner
	sibling.provider = "p2"
	other := owner
	other.agent = "b"
	for _, key := range []signedAttachBackoffKey{owner, other} {
		b.fail(key, "fp")
	}
	b.ownerPrepared(sibling)
	if err := b.admit(owner, "fp"); err != nil {
		t.Fatalf("same-owner window survived a successful sibling preparation: %v", err)
	}
	if err := b.admit(other, "fp"); !errors.Is(err, ErrSignedCapabilityAttachSuppressed) {
		t.Fatalf("another owner's window was reset: %v", err)
	}
}

func TestSignedReconcileCursorKey_AgentTierIgnoresTriggeringRun(t *testing.T) {
	a := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "end-a", SessionID: "s-a"}}
	b := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "end-b", SessionID: "s-b"}}
	if signedReconcileCursorKey(agentcfg.ConfigScopeAgent, a, "agent") != signedReconcileCursorKey(agentcfg.ConfigScopeAgent, b, "agent") {
		t.Fatal("agent-tier scan cursor keyed by the triggering end user")
	}
	if signedReconcileCursorKey(agentcfg.ConfigScopeUser, a, "agent") == signedReconcileCursorKey(agentcfg.ConfigScopeUser, b, "agent") {
		t.Fatal("user-tier scan cursors of different users collided")
	}
	a2 := a
	a2.SessionID = "s-a2"
	if signedReconcileCursorKey(agentcfg.ConfigScopeUser, a, "agent") != signedReconcileCursorKey(agentcfg.ConfigScopeUser, a2, "agent") {
		t.Fatal("user-tier scan cursor restarted for a new session")
	}
}
