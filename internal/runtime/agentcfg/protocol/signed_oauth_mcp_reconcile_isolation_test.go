package protocol_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// identityRecordingPreparer records the working identity every private
// preparation runs under and can fail or block chosen connections. All other
// behaviour is the shared capabilityPreparer fixture.
type identityRecordingPreparer struct {
	*capabilityPreparer
	mu         sync.Mutex
	identities map[string][]identity.Identity
	fail       map[string]error
	blockAgent string
	entered    chan string
	release    chan struct{}
}

func newIdentityRecordingPreparer() *identityRecordingPreparer {
	return &identityRecordingPreparer{capabilityPreparer: &capabilityPreparer{}, identities: map[string][]identity.Identity{}, fail: map[string]error{}}
}

func (p *identityRecordingPreparer) PrepareConnection(ctx context.Context, req agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	id, _ := identity.From(ctx)
	p.mu.Lock()
	p.identities[req.Name] = append(p.identities[req.Name], id)
	failErr := p.fail[req.Name]
	block := p.blockAgent != "" && req.AgentID == p.blockAgent
	entered, release := p.entered, p.release
	p.mu.Unlock()
	if block {
		entered <- req.AgentID
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failErr != nil {
		return nil, failErr
	}
	return p.capabilityPreparer.PrepareConnection(ctx, req)
}

func (p *identityRecordingPreparer) seen(name string) []identity.Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]identity.Identity(nil), p.identities[name]...)
}

func (p *identityRecordingPreparer) setFail(name string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.fail, name)
		return
	}
	p.fail[name] = err
}

func (p *identityRecordingPreparer) activationCount() int {
	p.capabilityPreparer.mu.Lock()
	defer p.capabilityPreparer.mu.Unlock()
	return p.capabilityPreparer.activations
}

func endUserRunContext(t *testing.T, id identity.Identity, runID string) (context.Context, identity.Quadruple) {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, id, runID)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, identity.Quadruple{Identity: id, RunID: runID}
}

func registerTwoAgentPairs(t *testing.T, now time.Time) func() (*agentcfgprotocol.SignedOAuthMCPReconciler, *identityRecordingPreparer) {
	t.Helper()
	svc, key, reg, st, _ := signedCapabilityServiceWithRegistry(t, now)
	first, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-isolation-first", "aud-first"))
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	canonical, sink, err := agentcfg.CanonicalOAuthMCPURL("https://second.example.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	second := signedCapabilityRequestNamed(t, key, now, scope(), testAgentID, "jti-isolation-second", "aud-second", "second-provider",
		prototypes.SignedOAuthMCPConnectionDescriptor{Name: "second", URL: canonical}, sink)
	second.ExpectedContentHash = first.Revision.ContentHash
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), second); err != nil {
		t.Fatalf("register second: %v", err)
	}
	restart := func() (*agentcfgprotocol.SignedOAuthMCPReconciler, *identityRecordingPreparer) {
		preparer := newIdentityRecordingPreparer()
		reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
		if err != nil {
			t.Fatal(err)
		}
		return reconciler, preparer
	}
	return restart
}

// An agent-tier pair is prepared as its frozen owner even when an end user's
// run triggers reconciliation; one failing pair never blocks its sibling, and
// the failing pair's retry is bounded by its window.
func TestSignedOAuthMCPReconcile_OwnerIdentityIsolationAndBackoff(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	restart := registerTwoAgentPairs(t, now)
	reconciler, preparer := restart()
	clock := now
	var clockMu sync.Mutex
	agentcfgprotocol.SetSignedOAuthMCPReconcilerClockForTest(reconciler, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	})
	advance := func(d time.Duration) {
		clockMu.Lock()
		clock = clock.Add(d)
		clockMu.Unlock()
	}
	owner := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, q := endUserRunContext(t, identity.Identity{TenantID: "t", UserID: "end-user", SessionID: "end-session"}, "run-1")

	brokerRefused := errors.New("injected preparation refusal")
	preparer.setFail("cap", brokerRefused)
	err := reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, testAgentID)
	if !errors.Is(err, brokerRefused) {
		t.Fatalf("first reconcile = %v, want the failing pair's error", err)
	}
	if got := preparer.activationCount(); got != 1 {
		t.Fatalf("activations = %d, want the healthy sibling attached despite the failing pair", got)
	}
	for _, name := range []string{"cap", "second"} {
		seen := preparer.seen(name)
		if len(seen) != 1 || seen[0] != owner {
			t.Fatalf("%s prepared under %+v, want exactly the frozen owner %+v", name, seen, owner)
		}
	}

	// Inside the window: no preparation, a typed suppression, sibling stays.
	err = reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, testAgentID)
	if !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityAttachSuppressed) || errors.Is(err, brokerRefused) {
		t.Fatalf("second reconcile = %v, want suppression only", err)
	}
	if got := len(preparer.seen("cap")); got != 1 {
		t.Fatalf("suppressed pair prepared %d times, want 1", got)
	}
	if got := len(preparer.seen("second")); got != 1 || preparer.activationCount() != 1 {
		t.Fatalf("attached sibling re-prepared: prepares=%d activations=%d", got, preparer.activationCount())
	}

	// After the window: exactly one retry, and the window doubles.
	advance(31 * time.Second)
	if err := reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, testAgentID); !errors.Is(err, brokerRefused) {
		t.Fatalf("post-window reconcile = %v, want a real retry", err)
	}
	advance(31 * time.Second)
	if err := reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, testAgentID); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityAttachSuppressed) {
		t.Fatalf("doubled window reconcile = %v, want suppression", err)
	}
	if got := len(preparer.seen("cap")); got != 2 {
		t.Fatalf("failing pair prepared %d times, want 2 bounded attempts", got)
	}

	// Healing: once the window lapses the pair attaches and the entry clears.
	preparer.setFail("cap", nil)
	advance(time.Minute)
	if err := reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, testAgentID); err != nil {
		t.Fatalf("healed reconcile: %v", err)
	}
	if got := preparer.activationCount(); got != 2 {
		t.Fatalf("activations = %d, want both pairs attached", got)
	}
	for _, id := range preparer.seen("cap") {
		if id != owner {
			t.Fatalf("retry prepared under %+v, want owner", id)
		}
	}
	if gates, backoff := agentcfgprotocol.SignedOAuthMCPReconcilerSizesForTest(reconciler); gates != 0 || backoff != 0 {
		t.Fatalf("retained gates=%d backoff=%d, want both released", gates, backoff)
	}
}

// A user-tier pair keeps the acting user (its owner) as the preparation
// identity, including a later session of that user.
func TestSignedOAuthMCPReconcile_UserPairPreparesAsActingUser(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, _ := signedCapabilityServiceWithRegistry(t, now)
	idA := identity.Identity{TenantID: "t", UserID: "user-a", SessionID: "session-a"}
	req := signedCapabilityRequestFor(t, key, now, prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID}, testAgentID, "jti-user-acting", "aud", "user-cap")
	if _, err := svc.RegisterUserOAuthMCPCapability(verifiedUserCapabilityContext(t, idA, true, true), userRegisterRequest(req)); err != nil {
		t.Fatalf("register user pair: %v", err)
	}
	preparer := newIdentityRecordingPreparer()
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	idA2 := identity.Identity{TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "session-a2"}
	if err := reconciler.ReconcileSignedOAuthMCPCapabilityForScope(verifiedUserCapabilityContext(t, idA2, true, true), identity.Quadruple{Identity: idA2, RunID: "run-a2"}, testAgentID, agentcfg.ConfigScopeUser); err != nil {
		t.Fatalf("user reconcile: %v", err)
	}
	seen := preparer.seen("user-cap")
	if len(seen) != 1 || seen[0] != idA2 {
		t.Fatalf("user pair prepared under %+v, want the acting user %+v", seen, idA2)
	}
}

// Reconciles of different owners proceed concurrently; the same owner stays
// serialized, and every gate is released afterwards.
func TestSignedOAuthMCPReconcile_PerOwnerSerialization(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, _ := signedCapabilityServiceWithRegistry(t, now)
	const otherAgent = "agent-y"
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-gate-x", "aud-x")); err != nil {
		t.Fatalf("register x: %v", err)
	}
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequestFor(t, key, now, scope(), otherAgent, "jti-gate-y", "aud-y", "cap-y")); err != nil {
		t.Fatalf("register y: %v", err)
	}
	preparer := newIdentityRecordingPreparer()
	preparer.blockAgent = testAgentID
	preparer.entered = make(chan string, 1)
	preparer.release = make(chan struct{})
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, q := endUserRunContext(t, identity.Identity{TenantID: "t", UserID: "end-user", SessionID: "end-session"}, "run-gate")

	blocked := make(chan error, 1)
	go func() { blocked <- reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, testAgentID) }()
	select {
	case <-preparer.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("agent x preparation never started")
	}

	// A different owner is not queued behind the blocked one.
	otherDone := make(chan error, 1)
	go func() { otherDone <- reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, otherAgent) }()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatalf("other owner reconcile: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("other owner queued behind a blocked owner")
	}
	if seen := preparer.seen("cap-y"); len(seen) != 1 {
		t.Fatalf("other owner prepares = %d, want 1", len(seen))
	}

	// The same owner waits for the in-flight pass.
	sameCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := reconciler.ReconcileSignedOAuthMCPCapability(sameCtx, q, testAgentID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent same-owner reconcile = %v, want it serialized behind the in-flight pass", err)
	}
	if got := len(preparer.seen("cap")); got != 1 {
		t.Fatalf("same owner prepared %d times concurrently, want 1", got)
	}

	close(preparer.release)
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("blocked owner reconcile: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked owner never finished")
	}
	if gates, _ := agentcfgprotocol.SignedOAuthMCPReconcilerSizesForTest(reconciler); gates != 0 {
		t.Fatalf("retained gates = %d, want 0", gates)
	}
}
