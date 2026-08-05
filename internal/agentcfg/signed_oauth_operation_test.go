package agentcfg

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func TestSignedOAuthMCPOperationStore_ExpiryAdmissionRenewalIsExactAndCASBound(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	ops, _ := NewSignedOAuthMCPOperationStore(store)
	key := SignedOAuthMCPReplayKey{TenantID: "tenant-a", TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "stable-jti"}
	binding := SignedOAuthMCPBinding{TenantID: "tenant-a", UserID: "registrar", SessionID: "session", AgentID: "agent", Broker: "broker", ProviderName: "provider", CapabilityRevision: "1", URLDigest: "url", SinkDigest: "sink", Audience: "aud", Scopes: []string{"read"}, Connection: SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://example.test/mcp"}}
	expiry := time.Now().UTC().Add(-time.Hour)
	claimed, _, err := ops.Claim(context.Background(), key, binding, expiry)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := ops.AdmitExpiry(context.Background(), claimed, "", time.Now().UTC())
	if err != nil || admitted.Phase != SignedOAuthMCPPhaseExpiryAdmitted || admitted.ExpiryFromPhase != SignedOAuthMCPPhaseClaimed || admitted.ExpiredAttemptCount != 1 {
		t.Fatalf("admit expiry = %+v err=%v", admitted, err)
	}
	if _, err := ops.RenewAuthority(context.Background(), admitted, key, binding, time.Now().Add(time.Hour)); !errors.Is(err, ErrSignedCapabilityReplay) {
		t.Fatalf("renew before compensation = %v, want replay refusal", err)
	}
	completed, err := ops.CompleteExpiry(context.Background(), admitted)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*SignedOAuthMCPBinding){
		"scope":      func(b *SignedOAuthMCPBinding) { b.Scopes = []string{"read", "write"} },
		"sink":       func(b *SignedOAuthMCPBinding) { b.SinkDigest = "other-sink" },
		"audience":   func(b *SignedOAuthMCPBinding) { b.Audience = "other-audience" },
		"registrar":  func(b *SignedOAuthMCPBinding) { b.UserID = "other-user" },
		"connection": func(b *SignedOAuthMCPBinding) { b.Connection.Name = "other-server" },
	}
	for name, mutate := range mutations {
		changed := binding
		mutate(&changed)
		if _, err := ops.RenewAuthority(context.Background(), completed, key, changed, time.Now().Add(time.Hour)); !errors.Is(err, ErrSignedCapabilityReplay) {
			t.Fatalf("%s binding renewal = %v, want replay refusal", name, err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			defer wg.Done()
			_, renewErr := ops.RenewAuthority(context.Background(), completed, key, binding, time.Now().Add(time.Hour))
			results <- renewErr
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for renewErr := range results {
		if renewErr == nil {
			winners++
		} else if !errors.Is(renewErr, ErrSignedCapabilityReplay) {
			t.Fatalf("concurrent renewal error = %v", renewErr)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent renewal winners = %d, want 1", winners)
	}
	renewed, err := ops.Load(context.Background(), key)
	if err != nil || renewed.Phase != SignedOAuthMCPPhaseClaimed || renewed.AuthorityGeneration != 2 || renewed.LastExpiredAt.IsZero() {
		t.Fatalf("renewed receipt = %+v err=%v", renewed, err)
	}
	if _, err := ops.RenewAuthority(context.Background(), renewed, key, binding, time.Now().Add(2*time.Hour)); !errors.Is(err, ErrSignedCapabilityReplay) {
		t.Fatalf("nonterminal claimed reopen = %v, want replay refusal", err)
	}
	committed, err := ops.Advance(context.Background(), renewed, SignedOAuthMCPPhaseRevisionCommitted, "revision-2")
	if err != nil {
		t.Fatal(err)
	}
	published, err := ops.Advance(context.Background(), committed, SignedOAuthMCPPhasePublished, "revision-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.RenewAuthority(context.Background(), published, key, binding, time.Now().Add(3*time.Hour)); !errors.Is(err, ErrSignedCapabilityReplay) {
		t.Fatalf("published reopen = %v, want replay refusal", err)
	}
	removal, _ := ops.Advance(context.Background(), published, SignedOAuthMCPPhaseRemovalAdmitted, "revision-2")
	removalRevision, _ := ops.Advance(context.Background(), removal, SignedOAuthMCPPhaseRemovalRevisionCommitted, "revision-3")
	unpublished, _ := ops.Advance(context.Background(), removalRevision, SignedOAuthMCPPhaseCatalogUnpublished, "revision-3")
	teardown, _ := ops.Advance(context.Background(), unpublished, SignedOAuthMCPPhaseTeardownReceipted, "revision-3")
	removed, err := ops.Advance(context.Background(), teardown, SignedOAuthMCPPhaseRemoved, "revision-3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.RenewAuthority(context.Background(), removed, key, binding, time.Now().Add(4*time.Hour)); !errors.Is(err, ErrSignedCapabilityReplay) {
		t.Fatalf("removed reopen = %v, want replay refusal", err)
	}
}

func TestSignedOAuthMCPOperationStore_SQLiteTwoHandleRenewalRaceHasOneWinner(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "stable-jti-renewal.sqlite")
	leftStore, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	rightStore, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rightStore.Close(context.Background())
		_ = leftStore.Close(context.Background())
	})
	left, _ := NewSignedOAuthMCPOperationStore(leftStore)
	right, _ := NewSignedOAuthMCPOperationStore(rightStore)
	key := SignedOAuthMCPReplayKey{TenantID: "tenant-a", TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "sqlite-stable-jti"}
	binding := SignedOAuthMCPBinding{TenantID: "tenant-a", UserID: "registrar", SessionID: "session", AgentID: "agent", Broker: "broker", ProviderName: "provider", CapabilityRevision: "1", URLDigest: "url", SinkDigest: "sink", Audience: "aud", Connection: SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://example.test/mcp"}}
	claimed, _, err := left.Claim(context.Background(), key, binding, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := left.AdmitExpiry(context.Background(), claimed, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	completed, err := left.CompleteExpiry(context.Background(), admitted)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, ops := range []*SignedOAuthMCPOperationStore{left, right} {
		wg.Add(1)
		go func(operations *SignedOAuthMCPOperationStore) {
			defer wg.Done()
			<-start
			_, renewErr := operations.RenewAuthority(context.Background(), completed, key, binding, time.Now().Add(time.Hour))
			results <- renewErr
		}(ops)
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for renewErr := range results {
		if renewErr == nil {
			winners++
		} else if !errors.Is(renewErr, ErrSignedCapabilityReplay) {
			t.Fatalf("SQLite renewal race error = %v", renewErr)
		}
	}
	if winners != 1 {
		t.Fatalf("SQLite renewal race winners = %d, want 1", winners)
	}
	latest, err := right.Load(context.Background(), key)
	if err != nil || latest.Phase != SignedOAuthMCPPhaseClaimed || latest.AuthorityGeneration != 2 {
		t.Fatalf("SQLite renewed receipt = %+v err=%v", latest, err)
	}
}

func TestSignedOAuthMCPActivationFenceStore_ReopensOnlyExactRenewedGeneration(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	fences, _ := NewSignedOAuthMCPActivationFenceStore(store)
	ops, _ := NewSignedOAuthMCPOperationStore(store)
	key := SignedOAuthMCPReplayKey{TenantID: "tenant-a", TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "reopen-jti"}
	binding := SignedOAuthMCPBinding{TenantID: "tenant-a", UserID: "registrar", SessionID: "session", AgentID: "agent", Broker: "broker", ProviderName: "provider", CapabilityRevision: "1", URLDigest: "url", SinkDigest: "sink", Audience: "aud", Connection: SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://example.test/mcp"}}
	claimed, _, _ := ops.Claim(context.Background(), key, binding, time.Now().Add(-time.Hour))
	kind, _ := ops.Kind(key)
	pending, err := fences.Begin(context.Background(), binding.TenantID, binding.AgentID, kind, claimed.Fingerprint, "candidate-hash", "prior-revision")
	if err != nil {
		t.Fatal(err)
	}
	aborted, err := fences.Advance(context.Background(), pending, SignedOAuthMCPFenceAborted, "candidate-revision")
	if err != nil {
		t.Fatal(err)
	}
	admitted, _ := ops.AdmitExpiry(context.Background(), claimed, "candidate-revision", time.Now())
	completed, _ := ops.CompleteExpiry(context.Background(), admitted)
	renewed, _ := ops.RenewAuthority(context.Background(), completed, key, binding, time.Now().Add(time.Hour))
	forged := renewed
	forged.AuthorityGeneration = 1
	if _, err := fences.ReopenForRenewedAuthority(context.Background(), aborted, forged, "next-hash", "prior-revision"); !errors.Is(err, ErrSignedCapabilityTransition) {
		t.Fatalf("forged generation reopen = %v, want transition refusal", err)
	}
	reopened, err := fences.ReopenForRenewedAuthority(context.Background(), aborted, renewed, "next-hash", "prior-revision")
	if err != nil || reopened.Phase != SignedOAuthMCPFencePending || reopened.CandidateContentHash != "next-hash" || reopened.CandidateRevisionID != "" {
		t.Fatalf("exact reopen = %+v err=%v", reopened, err)
	}
}

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

func TestSignedOAuthMCPActivationFenceStore_PairScopedOperationsCoexist(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	fences, err := NewSignedOAuthMCPActivationFenceStore(store)
	if err != nil {
		t.Fatal(err)
	}
	left, err := fences.BeginForOperation(context.Background(), "tenant", "agent", "operation-left", "fingerprint-left", "candidate-left", "prior")
	if err != nil {
		t.Fatal(err)
	}
	right, err := fences.BeginForOperation(context.Background(), "tenant", "agent", "operation-right", "fingerprint-right", "candidate-right", "prior")
	if err != nil {
		t.Fatalf("independent pair fence: %v", err)
	}
	if left.StateKind == right.StateKind || left.EventID == right.EventID {
		t.Fatalf("pair fences share durable slot: left=%+v right=%+v", left, right)
	}
	if _, err := fences.Advance(context.Background(), left, SignedOAuthMCPFenceCommitted, "revision-left"); err != nil {
		t.Fatal(err)
	}
	stillPending, err := fences.LoadForOperation(context.Background(), "tenant", "agent", "operation-right")
	if err != nil || stillPending.Phase != SignedOAuthMCPFencePending || stillPending.EventID != right.EventID {
		t.Fatalf("right fence changed with left: %+v err=%v", stillPending, err)
	}
	ops, err := NewSignedOAuthMCPOperationStore(store)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := ops.ScanTenantPage(context.Background(), "tenant", 10, "")
	if err != nil || len(page) != 0 {
		t.Fatalf("pair fences leaked into operation scan: %+v err=%v", page, err)
	}
}

func TestSignedOAuthMCPRetirementResource_HashOnlyRejectsTamperAndForeignBinding(t *testing.T) {
	binding := SignedOAuthMCPBinding{
		TenantID: "tenant-a", UserID: "private-user", SessionID: "private-session", AgentID: "agent",
		Broker: "broker", ProviderName: "provider", CapabilityRevision: "revision", URLDigest: "url-digest", SinkDigest: "sink-digest", Audience: "audience",
		Connection: SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://secret.example/path"},
	}
	op := SignedOAuthMCPOperation{
		ReplayKey: SignedOAuthMCPReplayKey{TenantID: binding.TenantID, TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "private-jti"},
		Binding:   binding, Fingerprint: SignedOAuthMCPPairFingerprint(binding), Phase: SignedOAuthMCPPhasePublished,
	}
	resource, err := SignedOAuthMCPRetirementResource(op)
	if err != nil {
		t.Fatal(err)
	}
	if len(resource) != 129 || resource[64] != '.' || strings.Contains(resource, "private") || strings.Contains(resource, "secret") || strings.Contains(resource, "jti") {
		t.Fatalf("resource is not hash-only: %q", resource)
	}
	terminal := op
	terminal.Phase = SignedOAuthMCPPhaseRemoved
	if !SignedOAuthMCPRetirementResourceMatches(resource, terminal) {
		t.Fatal("same exact operation must remain matchable after terminal convergence")
	}
	tampered := op
	tampered.Fingerprint = strings.Repeat("0", 64)
	if SignedOAuthMCPRetirementResourceMatches(resource, tampered) {
		t.Fatal("tampered fingerprint matched frozen resource")
	}
	foreign := op
	foreign.ReplayKey.TenantID = "tenant-b"
	foreign.Binding.TenantID = "tenant-b"
	foreign.Fingerprint = SignedOAuthMCPPairFingerprint(foreign.Binding)
	if SignedOAuthMCPRetirementResourceMatches(resource, foreign) {
		t.Fatal("foreign tenant operation matched frozen resource")
	}
}
