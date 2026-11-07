package protocol

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

func TestSignedAttachBackoff_BoundedAndFingerprintScoped(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	b := newSignedAttachBackoff()
	b.now = func() time.Time { return now }
	for i := range signedAttachBackoffEntries * 3 {
		b.fail(signedAttachBackoffKey{scope: agentcfg.ConfigScopeAgent, tenant: "t", agent: fmt.Sprint(i), provider: "p"}, "fp", "broker", false)
		if b.len() > signedAttachBackoffEntries {
			t.Fatalf("entries = %d, exceeds bound %d", b.len(), signedAttachBackoffEntries)
		}
	}
	key := signedAttachBackoffKey{scope: agentcfg.ConfigScopeAgent, tenant: "t", agent: "fresh", provider: "p"}
	b.fail(key, "fp-1", "broker", false)
	if err := b.admit(key, "fp-1"); !errors.Is(err, ErrSignedCapabilityAttachSuppressed) {
		t.Fatalf("same fingerprint admit = %v, want suppression", err)
	}
	// Any change to the revision, receipt or phase retries immediately.
	if err := b.admit(key, "fp-2"); err != nil {
		t.Fatalf("changed fingerprint admit = %v, want immediate retry", err)
	}
	// The literal operator-facing cap, not the constant compared to itself.
	if got := signedAttachBackoffDelay(1000); got != 5*time.Minute {
		t.Fatalf("delay cap = %v, want 5m", got)
	}
	if got := signedAttachBackoffDelay(1); got != 30*time.Second {
		t.Fatalf("first delay = %v, want 30s", got)
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

func TestSignedAttachBackoff_BrokerRecoveryResetsOnlySharedOutages(t *testing.T) {
	b := newSignedAttachBackoff()
	refused := signedAttachBackoffKey{scope: agentcfg.ConfigScopeAgent, tenant: "t", agent: "a", provider: "refused"}
	outage := refused
	outage.provider = "outage"
	otherBroker := refused
	otherBroker.provider = "other-broker"
	otherOwner := outage
	otherOwner.agent = "b"
	succeeded := refused
	succeeded.provider = "healthy"
	b.fail(refused, "fp", "broker", false)
	b.fail(outage, "fp", "broker", true)
	b.fail(otherBroker, "fp", "broker-2", true)
	b.fail(otherOwner, "fp", "broker", true)
	b.brokerRecovered(succeeded, "broker")
	if err := b.admit(outage, "fp"); err != nil {
		t.Fatalf("same-broker outage window survived broker recovery: %v", err)
	}
	for name, key := range map[string]signedAttachBackoffKey{"pair-specific refusal": refused, "other broker": otherBroker, "other owner": otherOwner} {
		if err := b.admit(key, "fp"); !errors.Is(err, ErrSignedCapabilityAttachSuppressed) {
			t.Fatalf("%s window was reset by another pair's success: %v", name, err)
		}
	}
}

func TestSignedSharedExchangeOutage_Classification(t *testing.T) {
	if !signedSharedExchangeOutage(fmt.Errorf("connect: %w", fmt.Errorf("%w: broker status 503", toolauth.ErrExchangeFailed))) {
		t.Fatal("broker server-side failure not classified as shared outage")
	}
	if signedSharedExchangeOutage(fmt.Errorf("%w: %w: status 403", toolauth.ErrExchangeFailed, toolauth.ErrCredentialRejected)) {
		t.Fatal("pair refusal classified as shared outage")
	}
	if signedSharedExchangeOutage(errors.New("downstream 401")) {
		t.Fatal("downstream failure classified as shared outage")
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
