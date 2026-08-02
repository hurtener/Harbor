package agentcfg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestSignedOAuthMCPOperationStore_ClaimsTenantScopedReplayAndTransitions(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	ops, err := NewSignedOAuthMCPOperationStore(store)
	if err != nil {
		t.Fatal(err)
	}
	key := SignedOAuthMCPReplayKey{TenantID: "tenant-a", TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "jti"}
	binding := SignedOAuthMCPBinding{TenantID: "tenant-a", AgentID: "agent", Broker: "broker", ProviderName: "provider", CapabilityRevision: "1", URLDigest: "digest", Audience: "aud"}
	op, claimed, err := ops.Claim(context.Background(), key, binding, time.Now().Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("first claim = (%v, %v), want claimed", err, claimed)
	}
	resumed, claimed, err := ops.Claim(context.Background(), key, binding, time.Now().Add(time.Hour))
	if err != nil || claimed || resumed.EventID != op.EventID {
		t.Fatalf("exact replay = (%+v, %v, %v), want resume", resumed, claimed, err)
	}
	if _, _, err := ops.Claim(context.Background(), key, SignedOAuthMCPBinding{TenantID: "tenant-a", AgentID: "other"}, time.Now().Add(time.Hour)); !errors.Is(err, ErrSignedCapabilityReplay) {
		t.Fatalf("different fingerprint err = %v, want ErrSignedCapabilityReplay", err)
	}
	next, err := ops.Advance(context.Background(), op, SignedOAuthMCPPhaseRevisionCommitted, "revision")
	if err != nil || next.Phase != SignedOAuthMCPPhaseRevisionCommitted {
		t.Fatalf("advance = (%+v, %v)", next, err)
	}
	if _, err := ops.Advance(context.Background(), op, SignedOAuthMCPPhasePublished, "revision"); !errors.Is(err, ErrSignedCapabilityTransition) {
		t.Fatalf("stale/invalid advance err = %v, want ErrSignedCapabilityTransition", err)
	}
}

func TestSignedOAuthMCPOperationStore_TenantSeparatesIdenticalJTI(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	ops, _ := NewSignedOAuthMCPOperationStore(store)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		_, claimed, err := ops.Claim(context.Background(), SignedOAuthMCPReplayKey{TenantID: tenant, TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "same"}, SignedOAuthMCPBinding{TenantID: tenant, AgentID: "agent"}, time.Now().Add(time.Hour))
		if err != nil || !claimed {
			t.Fatalf("tenant %q claim = (%v, %v)", tenant, claimed, err)
		}
	}
}

func TestSignedOAuthMCPOperationStore_PublisherEpochCASAndRemovalFenceUse(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	ops, err := NewSignedOAuthMCPOperationStore(store)
	if err != nil {
		t.Fatal(err)
	}
	key := SignedOAuthMCPReplayKey{TenantID: "tenant-a", TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "publisher-jti"}
	binding := SignedOAuthMCPBinding{TenantID: "tenant-a", UserID: "user", SessionID: "session", AgentID: "agent", Broker: "broker", ProviderName: "provider", CapabilityRevision: "1", URLDigest: "url", SinkDigest: "sink", Audience: "aud"}
	claimed, _, err := ops.Claim(context.Background(), key, binding, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	committed, err := ops.Advance(context.Background(), claimed, SignedOAuthMCPPhaseRevisionCommitted, "revision")
	if err != nil {
		t.Fatal(err)
	}
	first, err := ops.AcquirePublisher(context.Background(), committed)
	if err != nil || first.PublisherEpoch == "" {
		t.Fatalf("first publisher = %+v err=%v", first, err)
	}
	if _, err := ops.AcquirePublisher(context.Background(), committed); !errors.Is(err, ErrSignedCapabilityReplay) {
		t.Fatalf("stale publisher CAS = %v, want replay refusal", err)
	}
	kind, err := ops.Kind(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.AuthorizeSignedCapabilityUse(context.Background(), key.TenantID, kind, first.PublisherEpoch, false); !errors.Is(err, ErrSignedCapabilityPending) {
		t.Fatalf("normal use before published = %v, want pending", err)
	}
	if err := ops.AuthorizeSignedCapabilityUse(context.Background(), key.TenantID, kind, first.PublisherEpoch, true); err != nil {
		t.Fatalf("private preparation after revision commit: %v", err)
	}
	published, err := ops.Advance(context.Background(), first, SignedOAuthMCPPhasePublished, "revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.AuthorizeSignedCapabilityUse(context.Background(), key.TenantID, kind, first.PublisherEpoch, false); err != nil {
		t.Fatalf("published use: %v", err)
	}
	second, err := ops.AcquirePublisher(context.Background(), published)
	if err != nil || second.PublisherEpoch == first.PublisherEpoch {
		t.Fatalf("takeover = %+v err=%v", second, err)
	}
	if err := ops.AuthorizeSignedCapabilityUse(context.Background(), key.TenantID, kind, first.PublisherEpoch, false); !errors.Is(err, ErrSignedCapabilityPending) {
		t.Fatalf("old publisher after takeover = %v, want pending", err)
	}
	removing, err := ops.Advance(context.Background(), second, SignedOAuthMCPPhaseRemovalAdmitted, "revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.AuthorizeSignedCapabilityUse(context.Background(), key.TenantID, kind, second.PublisherEpoch, false); !errors.Is(err, ErrSignedCapabilityPending) {
		t.Fatalf("current publisher after removal admission = %v, want pending", err)
	}
	if err := ops.AuthorizeSignedCapabilityUse(context.Background(), key.TenantID, kind, removing.PublisherEpoch, true); !errors.Is(err, ErrSignedCapabilityPending) {
		t.Fatalf("preparation marker widened removal = %v, want pending", err)
	}
}

func TestSignedOAuthMCPActivationFenceStore_TerminalFenceYieldsToNextOperation(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	fences, err := NewSignedOAuthMCPActivationFenceStore(store)
	if err != nil {
		t.Fatal(err)
	}
	first, err := fences.Begin(context.Background(), "tenant-a", "agent", "operation-one", "fingerprint-one", "candidate-one", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fences.Advance(context.Background(), first, SignedOAuthMCPFenceCommitted, "revision-one"); err != nil {
		t.Fatal(err)
	}
	second, err := fences.Begin(context.Background(), "tenant-a", "agent", "operation-two", "fingerprint-two", "candidate-two", "revision-one")
	if err != nil {
		t.Fatalf("terminal fence must yield to the next operation: %v", err)
	}
	if second.Phase != SignedOAuthMCPFencePending || second.OperationKind != "operation-two" {
		t.Fatalf("replacement fence = %+v, want pending operation-two", second)
	}
	if _, err := fences.Begin(context.Background(), "tenant-a", "agent", "foreign", "foreign", "foreign", "revision-one"); !errors.Is(err, ErrSignedCapabilityPending) {
		t.Fatalf("foreign pending operation = %v, want ErrSignedCapabilityPending", err)
	}
}
