package protocol_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

type capabilityKeySet struct{ key crypto.PublicKey }

func (s capabilityKeySet) KeyByID(string) (crypto.PublicKey, string, error) {
	return s.key, jwt.SigningMethodRS256.Alg(), nil
}

type capabilityPreparedProvider struct{}

func (capabilityPreparedProvider) Binding() toolauth.OAuthProvider { return nil }
func (capabilityPreparedProvider) Publish(context.Context) error   { return nil }
func (capabilityPreparedProvider) Commit(context.Context)          {}
func (capabilityPreparedProvider) Rollback(context.Context) error  { return nil }
func (capabilityPreparedProvider) Close(context.Context) error     { return nil }

type capabilityInstaller struct{}

func (capabilityInstaller) InstallProvider(context.Context, string, string, agentcfg.OAuthProviderDescriptor) error {
	return nil
}
func (capabilityInstaller) UninstallProvider(context.Context, string, string, string) error {
	return nil
}
func (capabilityInstaller) PrepareSignedCapabilityProvider(context.Context, string, toolauth.SignedCapabilityExchangeBinding, []string) (agentcfgprotocol.PreparedOAuthProvider, error) {
	return capabilityPreparedProvider{}, nil
}

type reconcileTrackingProvider struct {
	mu        sync.Mutex
	resources int
	closes    int
	commits   int
}

func (*reconcileTrackingProvider) Binding() toolauth.OAuthProvider { return nil }
func (*reconcileTrackingProvider) Publish(context.Context) error   { return nil }
func (p *reconcileTrackingProvider) Commit(context.Context) {
	p.mu.Lock()
	p.commits++
	p.mu.Unlock()
}
func (*reconcileTrackingProvider) Rollback(context.Context) error { return nil }
func (p *reconcileTrackingProvider) Close(context.Context) error {
	p.mu.Lock()
	p.closes++
	p.resources = 0
	p.mu.Unlock()
	return nil
}

type reconcileTrackingInstaller struct{ provider *reconcileTrackingProvider }

func (reconcileTrackingInstaller) InstallProvider(context.Context, string, string, agentcfg.OAuthProviderDescriptor) error {
	return nil
}
func (reconcileTrackingInstaller) UninstallProvider(context.Context, string, string, string) error {
	return nil
}
func (i reconcileTrackingInstaller) PrepareSignedCapabilityProvider(context.Context, string, toolauth.SignedCapabilityExchangeBinding, []string) (agentcfgprotocol.PreparedOAuthProvider, error) {
	i.provider.mu.Lock()
	i.provider.resources++
	i.provider.mu.Unlock()
	return i.provider, nil
}

var (
	errCapabilityConnectionClose = errors.New("injected prepared connection close failure")
	errCapabilityProviderClose   = errors.New("injected prepared provider close failure")
)

type cleanupCapabilityProvider struct {
	mu               sync.Mutex
	closeCalls       int
	resources        int
	observedDeadline bool
}

func (*cleanupCapabilityProvider) Binding() toolauth.OAuthProvider { return nil }
func (*cleanupCapabilityProvider) Publish(context.Context) error   { return nil }
func (*cleanupCapabilityProvider) Commit(context.Context)          {}
func (*cleanupCapabilityProvider) Rollback(context.Context) error  { return nil }
func (p *cleanupCapabilityProvider) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeCalls++
	if _, ok := ctx.Deadline(); ok {
		p.observedDeadline = true
	}
	p.resources = 0
	return errCapabilityProviderClose
}

type cleanupCapabilityInstaller struct{ provider *cleanupCapabilityProvider }

func (cleanupCapabilityInstaller) InstallProvider(context.Context, string, string, agentcfg.OAuthProviderDescriptor) error {
	return nil
}
func (cleanupCapabilityInstaller) UninstallProvider(context.Context, string, string, string) error {
	return nil
}
func (i cleanupCapabilityInstaller) PrepareSignedCapabilityProvider(context.Context, string, toolauth.SignedCapabilityExchangeBinding, []string) (agentcfgprotocol.PreparedOAuthProvider, error) {
	i.provider.mu.Lock()
	i.provider.resources = 1
	i.provider.mu.Unlock()
	return i.provider, nil
}

type cleanupCapabilityPreparer struct {
	mu               sync.Mutex
	closeCalls       int
	activations      int
	workers          int
	observedDeadline bool
}

func (p *cleanupCapabilityPreparer) PrepareConnection(context.Context, agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	p.mu.Lock()
	p.workers = 1
	p.mu.Unlock()
	return &cleanupCapabilityConnection{parent: p}, nil
}
func (*cleanupCapabilityPreparer) DetachConnection(context.Context, string, string, string) error {
	return nil
}
func (*cleanupCapabilityPreparer) DetachExactConnection(context.Context, string, string, string, string) error {
	return nil
}
func (*cleanupCapabilityPreparer) BeginExactConnectionTeardown(string, string, string, string) (agentcfgprotocol.ExactConnectionTeardownFence, error) {
	return noopExactTeardownFence{}, nil
}

type noopExactTeardownFence struct{}

func (noopExactTeardownFence) Seal()                        {}
func (noopExactTeardownFence) Cancel(context.Context) error { return nil }

type cleanupCapabilityConnection struct{ parent *cleanupCapabilityPreparer }

func (c *cleanupCapabilityConnection) Activate(context.Context) error {
	c.parent.mu.Lock()
	c.parent.activations++
	c.parent.mu.Unlock()
	return nil
}
func (c *cleanupCapabilityConnection) ActivateIf(ctx context.Context, prove func(context.Context) error) error {
	if err := prove(ctx); err != nil {
		return err
	}
	return c.Activate(ctx)
}
func (c *cleanupCapabilityConnection) ActivateUnder(ctx context.Context, admit func(context.Context, func() error) error) error {
	return admit(ctx, func() error { return c.Activate(ctx) })
}
func (c *cleanupCapabilityConnection) Close(ctx context.Context) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()
	c.parent.closeCalls++
	if _, ok := ctx.Deadline(); ok {
		c.parent.observedDeadline = true
	}
	c.parent.workers = 0
	return errCapabilityConnectionClose
}

type capabilityPreparer struct {
	mu                 sync.Mutex
	activations        int
	detaches           int
	preparedCloses     int
	live               map[string]string
	failActivate       error
	failDetach         error
	detachEntered      chan struct{}
	detachRelease      <-chan struct{}
	prepareBlockTenant string
	prepareEntered     chan struct{}
	prepareRelease     <-chan struct{}
	publishEntered     chan struct{}
	publishRelease     <-chan struct{}
	fenceCancelErrs    []error
	fenceCancels       int
	fenceSeals         int
}

// capabilityLandedThenErroredRegistry models the production-shaped ambiguous
// acknowledgement: the immutable revision and active pointer commit, but the
// caller receives an error. The signed-capability recovery path must exact-reread the
// candidate and continue with THAT revision, never the stale predecessor.
type capabilityLandedThenErroredRegistry struct {
	agentcfg.Registry
	err  error
	once sync.Once
}

type capabilityOperationPhaseStore struct {
	state.StateStore
	mu      sync.Mutex
	phase   agentcfg.SignedOAuthMCPOperationPhase
	entered chan<- struct{}
	release <-chan struct{}
	failErr error
}

func (s *capabilityOperationPhaseStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	s.mu.Lock()
	matches := strings.Contains(string(next.Bytes), `"phase":"`+string(s.phase)+`"`)
	entered, release, failErr := s.entered, s.release, s.failErr
	if matches {
		s.failErr = nil
	}
	s.mu.Unlock()
	if matches && entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if matches && release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if matches && failErr != nil {
		return failErr
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

type capabilityRemovalFailingRegistry struct {
	agentcfg.Registry
	mu      sync.Mutex
	failErr error
}

func (r *capabilityRemovalFailingRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	r.mu.Lock()
	err := r.failErr
	if payload.SignedOAuthMCPPair == nil && opts.ExpectedContentHash != "" {
		r.failErr = nil
	} else {
		err = nil
	}
	r.mu.Unlock()
	if err != nil {
		return agentcfg.Revision{}, err
	}
	return r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
}

func (r *capabilityLandedThenErroredRegistry) PhysicalActive(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	physical, ok := r.Registry.(interface {
		PhysicalActive(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
	})
	if !ok {
		return agentcfg.Revision{}, false, errors.New("registry has no physical active recovery seam")
	}
	return physical.PhysicalActive(ctx, id, agentID, scope)
}

var (
	errCapabilityPointerWrite = errors.New("injected capability active-pointer write failure")
	errCapabilityOrphanDelete = errors.New("injected capability orphan cleanup failure")
)

// capabilityOrphanFaultStore leaves the exact residue that must never be
// mistaken for authority: the immutable candidate lands, its active-pointer
// SaveIf fails, and the compensating revision delete fails too.
type capabilityOrphanFaultStore struct{ state.StateStore }

func (s *capabilityOrphanFaultStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if next.Kind == agentcfg.ActiveSlotKind {
		return errCapabilityPointerWrite
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func (s *capabilityOrphanFaultStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	if strings.HasPrefix(kind, "agentcfg.revision.") {
		return errCapabilityOrphanDelete
	}
	return s.StateStore.Delete(ctx, id, kind)
}

func (r *capabilityLandedThenErroredRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	rev, err := r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
	if err != nil {
		return rev, err
	}
	injected := false
	r.once.Do(func() { injected = true })
	if injected {
		return rev, r.err
	}
	return rev, nil
}

func (p *capabilityPreparer) PrepareConnection(ctx context.Context, req agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	p.mu.Lock()
	block := p.prepareBlockTenant == req.Identity.TenantID
	entered, release := p.prepareEntered, p.prepareRelease
	p.mu.Unlock()
	if block && entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block && release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return capabilityPreparedConnection{parent: p, req: req}, nil
}

type capabilityPreparedConnection struct {
	parent *capabilityPreparer
	req    agentcfgprotocol.AttachRequest
}

func (p capabilityPreparedConnection) Activate(context.Context) error {
	p.parent.mu.Lock()
	if p.parent.failActivate != nil {
		err := p.parent.failActivate
		p.parent.mu.Unlock()
		return err
	}
	p.parent.activations++
	if p.parent.live == nil {
		p.parent.live = make(map[string]string)
	}
	p.parent.live[p.req.Identity.TenantID+"/"+p.req.AgentID+"/"+p.req.Name] = p.req.DescriptorFingerprint
	p.parent.mu.Unlock()
	return nil
}

func (p capabilityPreparedConnection) ActivateIf(ctx context.Context, prove func(context.Context) error) error {
	if err := prove(ctx); err != nil {
		return err
	}
	return p.Activate(ctx)
}
func (p capabilityPreparedConnection) ActivateUnder(ctx context.Context, admit func(context.Context, func() error) error) error {
	return admit(ctx, func() error {
		p.parent.mu.Lock()
		entered, release := p.parent.publishEntered, p.parent.publishRelease
		p.parent.mu.Unlock()
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return p.Activate(ctx)
	})
}

func TestSignedOAuthMCPReconciler_HistoricalPublishedPairCannotReattach(t *testing.T) {
	now := time.Now().UTC()
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-historical", "aud-historical")); err != nil {
		t.Fatalf("register: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, ok, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !ok || active.Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("active pair: ok=%v err=%v rev=%+v", ok, err, active)
	}
	pair := active.Payload.SignedOAuthMCPPair
	removedPayload := active.Payload
	removedPayload.SignedOAuthMCPPair = nil
	removeCtx := agentcfg.WithSignedOAuthMCPFenceOperation(context.Background(), pair.AuthorityOperationKind)
	if _, err := reg.SetRevision(removeCtx, q, testAgentID, agentcfg.ConfigScopeAgent, removedPayload, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("land removal revision before receipt checkpoint: %v", err)
	}
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	preparer.mu.Lock()
	activations, live := preparer.activations, len(preparer.live)
	preparer.mu.Unlock()
	if activations != 1 || live != 0 {
		t.Fatalf("historical published pair was dispatched: activations=%d live=%d", activations, live)
	}
	ops, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	op, err := ops.LoadForPair(context.Background(), q.TenantID, pair)
	if err != nil || op.Phase != agentcfg.SignedOAuthMCPPhaseRemoved {
		t.Fatalf("removal recovery phase=%q err=%v", op.Phase, err)
	}
}

func TestSignedOAuthMCPReconciler_RemovalDuringPrepareCannotRepublish(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	registered, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-stale-reconcile", "aud-stale-reconcile"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("active pair before reconcile: set=%t active=%+v err=%v", set, active, err)
	}
	pair := active.Payload.SignedOAuthMCPPair

	prepareEntered := make(chan struct{}, 1)
	prepareRelease := make(chan struct{})
	preparer.mu.Lock()
	delete(preparer.live, "t/"+testAgentID+"/"+pair.Connection.Name) // simulate process-local loss before restart reconciliation
	preparer.prepareBlockTenant = "t"
	preparer.prepareEntered = prepareEntered
	preparer.prepareRelease = prepareRelease
	preparer.mu.Unlock()
	trackingProvider := &reconcileTrackingProvider{}
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, reconcileTrackingInstaller{provider: trackingProvider})
	if err != nil {
		t.Fatal(err)
	}
	reconcileErr := make(chan error, 1)
	go func() {
		reconcileErr <- reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID)
	}()
	select {
	case <-prepareEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile did not pause after its initial active read")
	}

	otherSubject := prototypes.IdentityScope{Tenant: "t", User: "other-user", Session: "other-session"}
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequestFor(t, key, now, otherSubject, testAgentID, "jti-same-tenant-replacement", "aud-replacement", "cap-replacement")); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityPairExists) && !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		close(prepareRelease)
		t.Fatalf("same-tenant replacement during stale reconcile = %v, want pair-exists/pending refusal", err)
	}
	otherTenant := prototypes.IdentityScope{Tenant: "other-tenant", User: "other-user", Session: "other-session"}
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequestFor(t, key, now, otherTenant, testAgentID, "jti-other-tenant", "aud-other-tenant", "cap-other-tenant")); err != nil {
		close(prepareRelease)
		t.Fatalf("cross-tenant registration was not isolated from stale reconcile: %v", err)
	}
	removed, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash,
	})
	if err != nil || removed.OperationPhase != string(agentcfg.SignedOAuthMCPPhaseRemoved) {
		close(prepareRelease)
		t.Fatalf("remove while reconcile paused: phase=%q err=%v", removed.OperationPhase, err)
	}
	close(prepareRelease)
	if err := <-reconcileErr; !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		t.Fatalf("stale reconcile after completed removal = %v, want pending refusal", err)
	}

	if current, currentSet, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent); err != nil || !currentSet || current.Payload.SignedOAuthMCPPair != nil {
		t.Fatalf("removed desired state was resurrected: set=%t current=%+v err=%v", currentSet, current, err)
	}
	ops, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	op, err := ops.LoadForPair(context.Background(), q.TenantID, pair)
	if err != nil || op.Phase != agentcfg.SignedOAuthMCPPhaseRemoved {
		t.Fatalf("pair lifetime after stale reconcile: phase=%q err=%v", op.Phase, err)
	}
	preparer.mu.Lock()
	activations, preparedCloses := preparer.activations, preparer.preparedCloses
	_, staleLive := preparer.live["t/"+testAgentID+"/"+pair.Connection.Name]
	_, isolatedLive := preparer.live["other-tenant/"+testAgentID+"/cap-other-tenant"]
	preparer.mu.Unlock()
	if activations != 2 || preparedCloses != 1 || staleLive || !isolatedLive {
		t.Fatalf("stale publication cleanup: activations=%d closes=%d stale_live=%t isolated_live=%t", activations, preparedCloses, staleLive, isolatedLive)
	}
	trackingProvider.mu.Lock()
	resources, closes, commits := trackingProvider.resources, trackingProvider.closes, trackingProvider.commits
	trackingProvider.mu.Unlock()
	if resources != 0 || closes != 1 || commits != 0 {
		t.Fatalf("stale private provider cleanup: resources=%d closes=%d commits=%d", resources, closes, commits)
	}

	restarted, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, reconcileTrackingInstaller{provider: trackingProvider})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
		t.Fatalf("restart after completed removal: %v", err)
	}
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	if preparer.activations != activations {
		t.Fatalf("restart resurrected removed pair: activations=%d want %d", preparer.activations, activations)
	}
}

func TestSignedOAuthMCPReconciler_TwoRegistriesRemovalCannotCrossPublicationFence(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	first, key, _, st, _ := signedCapabilityServiceWithRegistry(t, now)
	registered, err := first.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-two-registry-fence", "aud-two-registry-fence"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	secondRegistry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = secondRegistry.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	publishRelease := make(chan struct{})
	secondPreparer := &capabilityPreparer{publishEntered: make(chan struct{}, 1), publishRelease: publishRelease}
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(secondRegistry, st, secondPreparer, secondPreparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID)
	}()
	select {
	case <-secondPreparer.publishEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second registry did not enter durable fenced publication")
	}
	removeDone := make(chan error, 1)
	go func() {
		_, removeErr := first.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
			Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash,
		})
		removeDone <- removeErr
	}()
	select {
	case err := <-removeDone:
		t.Fatalf("durable removal crossed publication fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(publishRelease)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("fenced reconcile publication: %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("removal after publication release: %v", err)
	}
	// The other runtime converges its process-local registry from the durable
	// removed receipt; no shared-memory detacher is assumed across runtimes.
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
		t.Fatalf("second-runtime post-removal reconcile: %v", err)
	}
	secondPreparer.mu.Lock()
	live := len(secondPreparer.live)
	secondPreparer.mu.Unlock()
	if live != 0 {
		t.Fatalf("second registry remained dispatchable after completed removal: live=%d", live)
	}
}

func TestSignedOAuthMCPReconciler_RemovalAdmittedWithActivePairResumesForward(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	registered, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-admitted-restart", "aud-admitted-restart"))
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("active pair: set=%t active=%+v err=%v", set, active, err)
	}
	operations, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	op, err := operations.LoadForPair(context.Background(), q.TenantID, active.Payload.SignedOAuthMCPPair)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operations.Advance(context.Background(), op, agentcfg.SignedOAuthMCPPhaseRemovalAdmitted, active.RevisionID); err != nil {
		t.Fatal(err)
	}
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
		t.Fatalf("resume admitted removal: %v", err)
	}
	latest, err := operations.LoadForPair(context.Background(), q.TenantID, active.Payload.SignedOAuthMCPPair)
	if err != nil || latest.Phase != agentcfg.SignedOAuthMCPPhaseRemoved {
		t.Fatalf("operation after reconcile = phase=%q err=%v, want removed", latest.Phase, err)
	}
	current, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || current.ContentHash == registered.Revision.ContentHash || current.Payload.SignedOAuthMCPPair != nil {
		t.Fatalf("desired removal after reconcile: set=%t current=%+v err=%v", set, current, err)
	}
	preparer.mu.Lock()
	live := len(preparer.live)
	preparer.mu.Unlock()
	if live != 0 {
		t.Fatalf("admitted restart left dispatchable state: live=%d", live)
	}
}

func TestSignedOAuthMCPReconciler_ExpiredIncompleteNeutralizesCandidate(t *testing.T) {
	authorityNow := time.Now().UTC().Add(-2 * time.Hour)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, authorityNow)
	preparer.mu.Lock()
	preparer.failActivate = errors.New("injected activation failure")
	preparer.mu.Unlock()
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, authorityNow, "jti-expired-incomplete", "aud-expired")); err == nil {
		t.Fatal("registration activation fault did not fail")
	}
	history, err := reg.ListRevisions(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID, agentcfg.ConfigScopeAgent, 0)
	if err != nil || len(history) != 1 || history[0].Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("candidate history: %+v err=%v", history, err)
	}
	pair := history[0].Payload.SignedOAuthMCPPair
	preparer.mu.Lock()
	preparer.failActivate = nil
	preparer.mu.Unlock()
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
		t.Fatalf("expire reconcile: %v", err)
	}
	if active, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent); err != nil || set {
		t.Fatalf("expired candidate remained active: set=%v rev=%+v err=%v", set, active, err)
	}
	ops, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	op, err := ops.LoadForPair(context.Background(), q.TenantID, pair)
	if err != nil || op.Phase != agentcfg.SignedOAuthMCPPhaseExpiredIncomplete {
		t.Fatalf("expired receipt phase=%q err=%v", op.Phase, err)
	}
	preparer.mu.Lock()
	deactivations := preparer.activations
	preparer.mu.Unlock()
	if deactivations != 0 {
		t.Fatalf("expired candidate was published: activations=%d", deactivations)
	}
}

func TestSignedOAuthMCPReconciler_ExpiredIncompleteRestoresBootLifecycle(t *testing.T) {
	authorityNow := time.Now().UTC().Add(-2 * time.Hour)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, authorityNow)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	boot, err := reg.SetRevision(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed boot lifecycle: %v", err)
	}
	preparer.mu.Lock()
	preparer.failActivate = errors.New("injected activation failure")
	preparer.mu.Unlock()
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, authorityNow, "jti-expired-boot", "aud-expired-boot")); err == nil {
		t.Fatal("registration activation fault did not fail")
	}
	preparer.mu.Lock()
	preparer.failActivate = nil
	preparer.mu.Unlock()
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
		t.Fatalf("expire reconcile: %v", err)
	}
	active, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.RevisionID != boot.RevisionID || active.ContentHash != boot.ContentHash {
		t.Fatalf("boot lifecycle was not restored exactly: set=%t active=%+v boot=%+v err=%v", set, active, boot, err)
	}
	if active.Payload.SignedOAuthMCPPair != nil || active.Payload.OAuthProviders != nil {
		t.Fatalf("expired candidate authority survived restoration: %+v", active.Payload)
	}
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	if preparer.activations != 0 || len(preparer.live) != 0 {
		t.Fatalf("expired candidate was republished: activations=%d live=%d", preparer.activations, len(preparer.live))
	}
}

func TestSignedOAuthMCPReconciler_CorruptExpiryAdmittedScanFailsBeforeSideEffects(t *testing.T) {
	now := time.Now().UTC()
	_, _, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	prior, err := reg.SetRevision(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	binding := agentcfg.SignedOAuthMCPBinding{TenantID: "t", UserID: "u", SessionID: "s", AgentID: testAgentID, Broker: "broker", ProviderName: "provider", CapabilityRevision: "v1", URLDigest: "url", SinkDigest: "sink", Audience: "aud", Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: "cap", URL: "https://example.test/mcp"}}
	key := agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: "corrupt-expiry-admitted"}
	ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(st)
	op, _, err := ops.Claim(context.Background(), key, binding, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	kind, _ := ops.Kind(key)
	corrupt := op
	corrupt.Phase = agentcfg.SignedOAuthMCPPhaseExpiryAdmitted
	corrupt.EventID = ""
	encoded, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	controlQ := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "__agentcfg__", SessionID: "__signed_oauth_mcp__"}}
	nextID := state.NewEventID()
	if err := st.SaveIf(context.Background(), []state.SlotExpectation{{Identity: controlQ, Kind: kind, ExpectedEventID: op.EventID}}, state.StateRecord{ID: nextID, Identity: controlQ, Kind: kind, Bytes: encoded}); err != nil {
		t.Fatal(err)
	}
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); !errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
		t.Fatalf("corrupt expiry_admitted reconcile = %v, want replay refusal", err)
	}
	preparer.mu.Lock()
	detaches, activations := preparer.detaches, preparer.activations
	preparer.mu.Unlock()
	if detaches != 0 || activations != 0 {
		t.Fatalf("corrupt scan caused side effects: detaches=%d activations=%d", detaches, activations)
	}
	active, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.RevisionID != prior.RevisionID || active.ContentHash != prior.ContentHash {
		t.Fatalf("corrupt scan mutated registry: active=%+v prior=%+v set=%t err=%v", active, prior, set, err)
	}
}
func (c capabilityPreparedConnection) Close(context.Context) error {
	c.parent.mu.Lock()
	c.parent.preparedCloses++
	c.parent.mu.Unlock()
	return nil
}

func (p *capabilityPreparer) DetachConnection(_ context.Context, tenant, agentID, name string) error {
	p.mu.Lock()
	if p.failDetach != nil {
		err := p.failDetach
		p.mu.Unlock()
		return err
	}
	p.detaches++
	delete(p.live, tenant+"/"+agentID+"/"+name)
	p.mu.Unlock()
	return nil
}

func (p *capabilityPreparer) DetachExactConnection(ctx context.Context, tenant, agentID, name, fingerprint string) error {
	p.mu.Lock()
	if p.failDetach != nil {
		err := p.failDetach
		p.mu.Unlock()
		return err
	}
	entered, release := p.detachEntered, p.detachRelease
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := tenant + "/" + agentID + "/" + name
	if live, ok := p.live[key]; ok && live != fingerprint {
		// Another runtime owns the durable current epoch. This process-local
		// handle is stale and must remain untouched; durable removal already
		// makes it inert.
		return nil
	}
	p.detaches++
	delete(p.live, key)
	return nil
}

type capabilityExactTeardownFence struct{ parent *capabilityPreparer }

func (f *capabilityExactTeardownFence) Seal() {
	f.parent.mu.Lock()
	f.parent.fenceSeals++
	f.parent.mu.Unlock()
}

func (f *capabilityExactTeardownFence) Cancel(context.Context) error {
	f.parent.mu.Lock()
	defer f.parent.mu.Unlock()
	f.parent.fenceCancels++
	if len(f.parent.fenceCancelErrs) == 0 {
		return nil
	}
	err := f.parent.fenceCancelErrs[0]
	f.parent.fenceCancelErrs = f.parent.fenceCancelErrs[1:]
	return err
}

func (p *capabilityPreparer) BeginExactConnectionTeardown(string, string, string, string) (agentcfgprotocol.ExactConnectionTeardownFence, error) {
	return &capabilityExactTeardownFence{parent: p}, nil
}

func (p *capabilityPreparer) ConnectionMatches(owner toolauth.Owner, name, fingerprint string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live[owner.Tenant+"/"+owner.Agent+"/"+name] == fingerprint
}

func signedCapabilityService(t *testing.T, now time.Time) (*agentcfgprotocol.Service, *rsa.PrivateKey, state.StateStore, *capabilityPreparer) {
	svc, key, _, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	return svc, key, st, preparer
}

func signedCapabilityServiceWithRegistry(t *testing.T, now time.Time) (*agentcfgprotocol.Service, *rsa.PrivateKey, agentcfg.Registry, state.StateStore, *capabilityPreparer) {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	return signedCapabilityServiceWithStore(t, now, st)
}

func signedCapabilityServiceWithStore(t *testing.T, now time.Time, st state.StateStore) (*agentcfgprotocol.Service, *rsa.PrivateKey, agentcfg.Registry, state.StateStore, *capabilityPreparer) {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	preparer := &capabilityPreparer{}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithClock(func() time.Time { return now }),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithConnectionDetacher(preparer),
		agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return svc, key, reg, st, preparer
}

func signedCapabilityServiceForExisting(t *testing.T, now time.Time, reg agentcfg.Registry, st state.StateStore, preparer *capabilityPreparer, key *rsa.PrivateKey) *agentcfgprotocol.Service {
	t.Helper()
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithClock(func() time.Time { return now }),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithConnectionDetacher(preparer),
		agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestRegisterOAuthMCPCapability_StableJTIRecoversClaimedBeforeFenceAndPreservesPrior(t *testing.T) {
	oldNow := time.Now().UTC().Add(-3 * time.Hour)
	_, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, oldNow)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	prior, err := reg.SetRevision(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	req := signedCapabilityRequest(t, key, oldNow, "stable-claimed", "aud-stable")
	canonical, sink, _ := agentcfg.CanonicalOAuthMCPURL(req.Connection.URL)
	binding := agentcfg.SignedOAuthMCPBinding{TenantID: "t", UserID: "u", SessionID: "s", AgentID: testAgentID, Broker: req.Broker, ProviderName: req.ProviderName, CapabilityRevision: "v1", URLDigest: agentcfg.OAuthMCPURLDigest(canonical), SinkDigest: agentcfg.OAuthMCPURLDigest(sink), Audience: req.Audience, Scopes: req.Scopes, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: req.Connection.Name, URL: canonical}}
	ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(st)
	operationKey := agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: "stable-claimed"}
	if _, claimed, err := ops.Claim(context.Background(), operationKey, binding, oldNow.Add(30*time.Minute)); err != nil || !claimed {
		t.Fatalf("seed claimed receipt = claimed=%t err=%v", claimed, err)
	}
	renewNow := oldNow.Add(2 * time.Hour)
	renewedReq := signedCapabilityRequest(t, key, renewNow, "stable-claimed", "aud-stable")
	renewedSvc := signedCapabilityServiceForExisting(t, renewNow, reg, st, preparer, key)
	registered, err := renewedSvc.RegisterOAuthMCPCapability(context.Background(), renewedReq)
	if err != nil {
		t.Fatalf("stable-JTI claimed recovery: %v", err)
	}
	if registered.Revision.ParentRevisionID != prior.RevisionID {
		t.Fatalf("renewed candidate parent = %q, want preserved prior %q", registered.Revision.ParentRevisionID, prior.RevisionID)
	}
	preserved, err := reg.Get(context.Background(), q, testAgentID, prior.RevisionID, agentcfg.ConfigScopeAgent)
	if err != nil || preserved.ContentHash != prior.ContentHash {
		t.Fatalf("unrelated prior revision changed: preserved=%+v prior=%+v err=%v", preserved, prior, err)
	}
	op, err := ops.Load(context.Background(), operationKey)
	if err != nil || op.Phase != agentcfg.SignedOAuthMCPPhasePublished || op.AuthorityGeneration != 2 || op.ExpiredAttemptCount != 1 {
		t.Fatalf("stable-JTI operation = %+v err=%v", op, err)
	}
}

func TestRegisterOAuthMCPCapability_StableJTIClaimedBeforeFenceReplacesOlderAbortedFence(t *testing.T) {
	oldNow := time.Now().UTC().Add(-3 * time.Hour)
	_, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, oldNow)
	req := signedCapabilityRequest(t, key, oldNow, "stable-after-older-aborted", "aud-stable-aborted")
	canonical, sink, _ := agentcfg.CanonicalOAuthMCPURL(req.Connection.URL)
	binding := agentcfg.SignedOAuthMCPBinding{TenantID: "t", UserID: "u", SessionID: "s", AgentID: testAgentID, Broker: req.Broker, ProviderName: req.ProviderName, CapabilityRevision: "v1", URLDigest: agentcfg.OAuthMCPURLDigest(canonical), SinkDigest: agentcfg.OAuthMCPURLDigest(sink), Audience: req.Audience, Scopes: req.Scopes, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: req.Connection.Name, URL: canonical}}
	ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(st)
	fences, _ := agentcfg.NewSignedOAuthMCPActivationFenceStore(st)
	olderKey := agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: "older-aborted-operation"}
	older, _, err := ops.Claim(context.Background(), olderKey, binding, oldNow.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	olderKind, _ := ops.Kind(olderKey)
	olderFence, err := fences.Begin(context.Background(), "t", testAgentID, olderKind, older.Fingerprint, "older-candidate-hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fences.Advance(context.Background(), olderFence, agentcfg.SignedOAuthMCPFenceAborted, ""); err != nil {
		t.Fatal(err)
	}
	currentKey := agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: "stable-after-older-aborted"}
	if _, claimed, err := ops.Claim(context.Background(), currentKey, binding, oldNow.Add(30*time.Minute)); err != nil || !claimed {
		t.Fatalf("seed current expired claim = claimed=%t err=%v", claimed, err)
	}
	renewNow := oldNow.Add(2 * time.Hour)
	renewedSvc := signedCapabilityServiceForExisting(t, renewNow, reg, st, preparer, key)
	renewedReq := signedCapabilityRequest(t, key, renewNow, currentKey.JTI, req.Audience)
	first, err := renewedSvc.RegisterOAuthMCPCapability(context.Background(), renewedReq)
	if err != nil {
		t.Fatalf("stable JTI behind older aborted fence: %v", err)
	}
	second, err := renewedSvc.RegisterOAuthMCPCapability(context.Background(), renewedReq)
	if err != nil || second.Revision.RevisionID != first.Revision.RevisionID {
		t.Fatalf("exact stable-JTI retry = revisions %q/%q err=%v", first.Revision.RevisionID, second.Revision.RevisionID, err)
	}
	olderAfter, err := ops.Load(context.Background(), olderKey)
	if err != nil || olderAfter.EventID != older.EventID || olderAfter.Phase != older.Phase || olderAfter.ExpiresAt != older.ExpiresAt {
		t.Fatalf("older operation receipt changed: before=%+v after=%+v err=%v", older, olderAfter, err)
	}
	current, err := ops.Load(context.Background(), currentKey)
	if err != nil || current.Phase != agentcfg.SignedOAuthMCPPhasePublished || current.AuthorityGeneration != 2 {
		t.Fatalf("current stable-JTI operation = %+v err=%v", current, err)
	}
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	if preparer.activations != 1 || len(preparer.live) != 1 {
		t.Fatalf("stable-JTI publication = activations=%d live=%d", preparer.activations, len(preparer.live))
	}
}

func TestRegisterOAuthMCPCapability_StableJTIRecoversExpiredRevisionCommittedOnce(t *testing.T) {
	oldNow := time.Now().UTC().Add(-3 * time.Hour)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, oldNow)
	preparer.mu.Lock()
	preparer.failActivate = errors.New("injected first-generation activation failure")
	preparer.mu.Unlock()
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, oldNow, "stable-committed", "aud-stable-committed")); err == nil {
		t.Fatal("first generation unexpectedly published")
	}
	preparer.mu.Lock()
	preparer.failActivate = nil
	preparer.mu.Unlock()
	renewNow := oldNow.Add(2 * time.Hour)
	renewedSvc := signedCapabilityServiceForExisting(t, renewNow, reg, st, preparer, key)
	renewedReq := signedCapabilityRequest(t, key, renewNow, "stable-committed", "aud-stable-committed")
	first, err := renewedSvc.RegisterOAuthMCPCapability(context.Background(), renewedReq)
	if err != nil {
		t.Fatalf("stable-JTI committed recovery: %v", err)
	}
	second, err := renewedSvc.RegisterOAuthMCPCapability(context.Background(), renewedReq)
	if err != nil || second.Revision.RevisionID != first.Revision.RevisionID {
		t.Fatalf("repeated stable-JTI convergence = revision %q/%q err=%v", first.Revision.RevisionID, second.Revision.RevisionID, err)
	}
	ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(st)
	op, err := ops.Load(context.Background(), agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: "stable-committed"})
	if err != nil || op.Phase != agentcfg.SignedOAuthMCPPhasePublished || op.AuthorityGeneration != 2 || op.LastExpiredRevisionID == "" {
		t.Fatalf("stable-JTI committed operation = %+v err=%v", op, err)
	}
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	if preparer.activations != 1 || len(preparer.live) != 1 {
		t.Fatalf("publication convergence = activations=%d live=%d, want 1/1", preparer.activations, len(preparer.live))
	}
}

func TestRegisterOAuthMCPCapability_ExpiredClaimedMatchingCandidateWithoutFenceFailsClosed(t *testing.T) {
	oldNow := time.Now().UTC().Add(-3 * time.Hour)
	_, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, oldNow)
	req := signedCapabilityRequest(t, key, oldNow, "stable-no-fence", "aud-no-fence")
	canonical, sink, _ := agentcfg.CanonicalOAuthMCPURL(req.Connection.URL)
	binding := agentcfg.SignedOAuthMCPBinding{TenantID: "t", UserID: "u", SessionID: "s", AgentID: testAgentID, Broker: req.Broker, ProviderName: req.ProviderName, CapabilityRevision: "v1", URLDigest: agentcfg.OAuthMCPURLDigest(canonical), SinkDigest: agentcfg.OAuthMCPURLDigest(sink), Audience: req.Audience, Scopes: req.Scopes, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: req.Connection.Name, URL: canonical}}
	operationKey := agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: "stable-no-fence"}
	ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(st)
	op, _, err := ops.Claim(context.Background(), operationKey, binding, oldNow.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	kind, _ := ops.Kind(operationKey)
	pair := &agentcfg.SignedOAuthMCPPair{ProviderName: binding.ProviderName, Broker: binding.Broker, Audience: binding.Audience, Scopes: binding.Scopes, CapabilityRevision: binding.CapabilityRevision, URLDigest: binding.URLDigest, Sink: sink, SinkDigest: binding.SinkDigest, Connection: binding.Connection, AuthorityIssuer: "issuer", AuthorityKeyID: "kid", AuthorityJTIHash: signedJTIHashForTest(operationKey.JTI), AuthorityOperationKind: kind, OwnerAgentID: binding.AgentID, OwnerUserID: binding.UserID, OwnerSessionID: binding.SessionID}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	candidate, err := reg.SetRevision(agentcfg.WithSignedOAuthMCPFenceOperation(context.Background(), kind), q, testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{SignedOAuthMCPPair: pair}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	renewNow := oldNow.Add(2 * time.Hour)
	renewedSvc := signedCapabilityServiceForExisting(t, renewNow, reg, st, preparer, key)
	if _, err := renewedSvc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, renewNow, "stable-no-fence", "aud-no-fence")); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		t.Fatalf("matching candidate without fence = %v, want pending refusal", err)
	}
	physical := reg.(interface {
		PhysicalActive(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
	})
	active, set, err := physical.PhysicalActive(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.RevisionID != candidate.RevisionID || active.ContentHash != candidate.ContentHash {
		t.Fatalf("unfenced candidate was mutated: active=%+v set=%t err=%v", active, set, err)
	}
	latest, err := ops.Load(context.Background(), operationKey)
	if err != nil || latest.Phase != agentcfg.SignedOAuthMCPPhaseExpiryAdmitted || latest.ExpiryCandidateRevisionID != candidate.RevisionID || latest.EventID == op.EventID {
		t.Fatalf("expiry admission receipt = %+v err=%v", latest, err)
	}
}

func signedJTIHashForTest(jti string) string {
	sum := sha256.Sum256([]byte(jti))
	return hex.EncodeToString(sum[:])
}

func TestSignedOAuthMCPReconciler_Restart_ReattachesFrozenOwnerForLaterSubject(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-reconcile-restart", "aud-reconcile")); err != nil {
		t.Fatalf("register: %v", err)
	}
	preparer.mu.Lock()
	preparer.live = make(map[string]string) // process-local MCP catalog is lost on restart.
	preparer.mu.Unlock()
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatalf("NewSignedOAuthMCPReconciler: %v", err)
	}
	invoker := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "later-user", SessionID: "later-session"}}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), invoker, testAgentID); err != nil {
		t.Fatalf("reconcile restart: %v", err)
	}
	preparer.mu.Lock()
	activations := preparer.activations
	preparer.mu.Unlock()
	if activations != 2 {
		t.Fatalf("activations = %d, want register plus one restart reattach", activations)
	}
	owner := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, set, err := reg.Active(context.Background(), owner, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set {
		t.Fatalf("frozen owner active pair = set:%v err:%v, want owner slot", set, err)
	}
	if active.Payload.SignedOAuthMCPPair == nil || active.Payload.SignedOAuthMCPPair.OwnerUserID != owner.UserID || active.Payload.SignedOAuthMCPPair.OwnerSessionID != owner.SessionID {
		t.Fatalf("reattached pair owner = %+v, want frozen owner %s/%s", active.Payload.SignedOAuthMCPPair, owner.UserID, owner.SessionID)
	}
	// An exact tenant/agent scan must not turn a same-named foreign slot into a
	// dispatch target.
	foreign := identity.Quadruple{Identity: identity.Identity{TenantID: "foreign", UserID: "u", SessionID: "s"}}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), foreign, testAgentID); err != nil {
		t.Fatalf("foreign empty slot must be a no-op, got %v", err)
	}
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	if preparer.activations != activations {
		t.Fatalf("foreign reconcile dispatched a local pair")
	}
}

func TestSignedOAuthMCPReconciler_SQLiteRestart_ReattachesPublishedPair(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	dsn := filepath.Join(t.TempDir(), "signed-capability.sqlite")
	firstStore, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	first, key, firstRegistry, _, _ := signedCapabilityServiceWithStore(t, now, firstStore)
	if _, err := first.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-sqlite-restart", "aud-sqlite")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := firstRegistry.Close(context.Background()); err != nil {
		t.Fatalf("close first registry: %v", err)
	}
	if err := firstStore.Close(context.Background()); err != nil {
		t.Fatalf("close first state: %v", err)
	}
	secondStore, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	_, _, secondRegistry, _, secondPreparer := signedCapabilityServiceWithStore(t, now, secondStore)
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(secondRegistry, secondStore, secondPreparer, secondPreparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	secondPreparer.mu.Lock()
	defer secondPreparer.mu.Unlock()
	if secondPreparer.activations != 1 {
		t.Fatalf("restart activations = %d, want 1", secondPreparer.activations)
	}
}

func TestRegisterOAuthMCPCapability_SQLiteTwoHandleStableJTIExpiryRecovery(t *testing.T) {
	for _, phase := range []agentcfg.SignedOAuthMCPOperationPhase{agentcfg.SignedOAuthMCPPhaseClaimed, agentcfg.SignedOAuthMCPPhaseRevisionCommitted} {
		t.Run(string(phase), func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "stable-jti.sqlite")
			firstStore, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			secondStore, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			firstBus, _ := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
			secondBus, _ := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
			firstRegistry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: firstStore, Bus: firstBus})
			if err != nil {
				t.Fatal(err)
			}
			secondRegistry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: secondStore, Bus: secondBus})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = secondRegistry.Close(context.Background())
				_ = firstRegistry.Close(context.Background())
				_ = secondBus.Close(context.Background())
				_ = firstBus.Close(context.Background())
				_ = secondStore.Close(context.Background())
				_ = firstStore.Close(context.Background())
			})
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			oldNow := time.Now().UTC().Add(-3 * time.Hour)
			renewNow := oldNow.Add(2 * time.Hour)
			jti := "sqlite-stable-" + string(phase)
			firstPreparer := &capabilityPreparer{}
			first := signedCapabilityServiceForExisting(t, oldNow, firstRegistry, firstStore, firstPreparer, key)
			if phase == agentcfg.SignedOAuthMCPPhaseClaimed {
				req := signedCapabilityRequest(t, key, oldNow, jti, "aud-sqlite-stable")
				canonical, sink, _ := agentcfg.CanonicalOAuthMCPURL(req.Connection.URL)
				binding := agentcfg.SignedOAuthMCPBinding{TenantID: "t", UserID: "u", SessionID: "s", AgentID: testAgentID, Broker: req.Broker, ProviderName: req.ProviderName, CapabilityRevision: "v1", URLDigest: agentcfg.OAuthMCPURLDigest(canonical), SinkDigest: agentcfg.OAuthMCPURLDigest(sink), Audience: req.Audience, Scopes: req.Scopes, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: req.Connection.Name, URL: canonical}}
				ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(firstStore)
				_, claimed, claimErr := ops.Claim(context.Background(), agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: jti}, binding, oldNow.Add(30*time.Minute))
				if claimErr != nil || !claimed {
					t.Fatalf("seed SQLite claim = claimed=%t err=%v", claimed, claimErr)
				}
			} else {
				firstPreparer.failActivate = errors.New("injected first-handle activation failure")
				if _, err := first.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, oldNow, jti, "aud-sqlite-stable")); err == nil {
					t.Fatal("first SQLite handle unexpectedly published")
				}
			}

			secondPreparer := &capabilityPreparer{}
			second := signedCapabilityServiceForExisting(t, renewNow, secondRegistry, secondStore, secondPreparer, key)
			if _, err := second.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, renewNow, jti, "aud-sqlite-stable")); err != nil {
				t.Fatalf("second SQLite handle stable-JTI recovery: %v", err)
			}
			ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(secondStore)
			op, err := ops.Load(context.Background(), agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: jti})
			if err != nil || op.Phase != agentcfg.SignedOAuthMCPPhasePublished || op.AuthorityGeneration != 2 || op.ExpiredAttemptCount != 1 {
				t.Fatalf("SQLite stable-JTI operation = %+v err=%v", op, err)
			}
			secondPreparer.mu.Lock()
			defer secondPreparer.mu.Unlock()
			if secondPreparer.activations != 1 || len(secondPreparer.live) != 1 {
				t.Fatalf("SQLite stable-JTI publication = activations=%d live=%d", secondPreparer.activations, len(secondPreparer.live))
			}
		})
	}
}

func TestSignedOAuthMCPReconciler_RecoversRemovalAfterDetachFault(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	registered, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-reconcile-remove", "aud-remove"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	preparer.mu.Lock()
	preparer.failDetach = errors.New("injected detach fault")
	preparer.mu.Unlock()
	_, err = svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash})
	if err == nil {
		t.Fatal("remove without detach fault error")
	}
	ops, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	history, err := reg.ListRevisions(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent, 0)
	var pair *agentcfg.SignedOAuthMCPPair
	for _, revision := range history {
		if revision.Payload.SignedOAuthMCPPair != nil {
			pair = revision.Payload.SignedOAuthMCPPair
			break
		}
	}
	if err != nil || pair == nil {
		t.Fatalf("pair history after removal fault: %+v err=%v", history, err)
	}
	assertRemovalPhase := func(want agentcfg.SignedOAuthMCPOperationPhase) {
		t.Helper()
		op, loadErr := ops.LoadForPair(context.Background(), q.TenantID, pair)
		if loadErr != nil || op.Phase != want {
			t.Fatalf("removal phase = %q err=%v, want %q", op.Phase, loadErr, want)
		}
	}
	assertRemovalPhase(agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted)
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatalf("NewSignedOAuthMCPReconciler: %v", err)
	}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err == nil {
		t.Fatal("persistent close fault became a reconciler teardown receipt")
	}
	assertRemovalPhase(agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted)
	if _, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash}); err == nil {
		t.Fatal("persistent close fault became a Service retry teardown receipt")
	}
	assertRemovalPhase(agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted)
	preparer.mu.Lock()
	preparer.failDetach = nil
	preparer.mu.Unlock()
	for range 3 {
		if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	removed, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash})
	if err != nil {
		t.Fatalf("terminal removal retry: %v", err)
	}
	if removed.OperationPhase != string(agentcfg.SignedOAuthMCPPhaseRemoved) {
		t.Fatalf("phase = %q, want removed", removed.OperationPhase)
	}
}

func TestSignedOAuthMCPReconciler_ConcurrentReuseN128_CancellationDoesNotLeak(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-reconcile-concurrent", "aud-concurrent")); err != nil {
		t.Fatalf("register: %v", err)
	}
	preparer.mu.Lock()
	preparer.live = make(map[string]string)
	preparer.mu.Unlock()
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, preparer, preparer, capabilityInstaller{})
	if err != nil {
		t.Fatalf("NewSignedOAuthMCPReconciler: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for i := range 128 {
		wg.Add(1)
		go func(cancelled bool) {
			defer wg.Done()
			callCtx := context.Background()
			if cancelled {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(callCtx)
				cancel()
			}
			err := reconciler.ReconcileSignedOAuthMCPCapability(callCtx, q, testAgentID)
			if err != nil && !errors.Is(err, context.Canceled) {
				errs <- err
			}
		}(i%7 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("reconcile: %v", err)
	}
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	if preparer.activations != 2 {
		t.Fatalf("activations = %d, want one recovery attach", preparer.activations)
	}
}

func TestRemoveOAuthMCPCapability_ContinuesPairLifetimeReceipt(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, _, preparer := signedCapabilityService(t, now)
	registered, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-remove", "aud-1"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	removed, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got, want := removed.OperationPhase, string(agentcfg.SignedOAuthMCPPhaseRemoved); got != want {
		t.Fatalf("phase = %q, want %q", got, want)
	}
	// The terminal retry must use immutable history rather than requiring the
	// now-removed pair to remain in desired state.
	again, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash})
	if err != nil {
		t.Fatalf("terminal retry: %v", err)
	}
	if again.Revision.RevisionID != removed.Revision.RevisionID {
		t.Fatalf("terminal retry revision = %q, want %q", again.Revision.RevisionID, removed.Revision.RevisionID)
	}
	preparer.mu.Lock()
	detaches := preparer.detaches
	preparer.mu.Unlock()
	if detaches != 1 {
		t.Fatalf("detaches = %d, want exactly one", detaches)
	}
}

func TestRemoveOAuthMCPCapability_DelayedTargetCannotRemoveReplacement(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, _, preparer := signedCapabilityServiceWithRegistry(t, now)
	first, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-remove-a", "aud-a"))
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	preparer.mu.Lock()
	firstLiveFingerprint := preparer.live["t/"+testAgentID+"/cap"]
	preparer.mu.Unlock()
	removed, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash,
	})
	if err != nil {
		t.Fatalf("remove A: %v", err)
	}
	second, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-remove-b", "aud-b"))
	if err != nil {
		t.Fatalf("register B: %v", err)
	}
	preparer.mu.Lock()
	secondLiveFingerprint := preparer.live["t/"+testAgentID+"/cap"]
	preparer.mu.Unlock()
	if firstLiveFingerprint == "" || secondLiveFingerprint == "" || firstLiveFingerprint == secondLiveFingerprint {
		t.Fatalf("attachment generations were not distinct: A=%q B=%q", firstLiveFingerprint, secondLiveFingerprint)
	}

	delayed, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash,
	})
	if err != nil {
		t.Fatalf("delayed A retry: %v", err)
	}
	if delayed.Revision.RevisionID != removed.Revision.RevisionID {
		t.Fatalf("delayed retry revision = %q, want A removal %q", delayed.Revision.RevisionID, removed.Revision.RevisionID)
	}
	if _, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: scope(), AgentID: testAgentID,
	}); !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("hash-less delayed removal = %v, want revision conflict", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.RevisionID != second.Revision.RevisionID || active.Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("replacement active = (%+v, %t, %v), want B %q", active, set, err, second.Revision.RevisionID)
	}
	preparer.mu.Lock()
	detaches := preparer.detaches
	live := len(preparer.live)
	preparer.mu.Unlock()
	if detaches != 1 || live != 1 {
		t.Fatalf("delayed removal touched replacement: detaches=%d live=%d", detaches, live)
	}
}

func TestRegisterOAuthMCPCapability_FreshJTIMustNotAdoptMatchingActivePair(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, _, preparer := signedCapabilityService(t, now)
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-owner-a", "aud-same")); err != nil {
		t.Fatalf("register owner A: %v", err)
	}
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-owner-b", "aud-same")); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityPairExists) {
		t.Fatalf("fresh JTI matching active binding = %v, want pair exists", err)
	}
	preparer.mu.Lock()
	activations := preparer.activations
	preparer.mu.Unlock()
	if activations != 1 {
		t.Fatalf("fresh JTI adopted active pair: activations=%d", activations)
	}
}

func TestRegisterOAuthMCPCapability_BlocksReplacementUntilRemovalTerminal(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, _, preparer := signedCapabilityService(t, now)
	first, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-pending-a", "aud-a"))
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	preparer.mu.Lock()
	preparer.failDetach = errors.New("injected detach failure")
	preparer.mu.Unlock()
	if _, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash,
	}); err == nil {
		t.Fatal("remove A unexpectedly passed detach failure")
	}
	replacement := signedCapabilityRequest(t, key, now, "jti-pending-b", "aud-b")
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), replacement); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		t.Fatalf("replacement during A removal = %v, want pending", err)
	}
	preparer.mu.Lock()
	preparer.failDetach = nil
	preparer.mu.Unlock()
	if _, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash,
	}); err != nil {
		t.Fatalf("finish A removal: %v", err)
	}
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), replacement); err != nil {
		t.Fatalf("same claimed replacement retry after terminal A removal: %v", err)
	}
}

func TestRegisterOAuthMCPCapability_ConcurrentRemovalBlocksReplacement(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	second, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithClock(func() time.Time { return now }),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithConnectionDetacher(preparer),
		agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-race-a", "aud-a"))
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	preparer.mu.Lock()
	preparer.detachEntered = entered
	preparer.detachRelease = release
	preparer.mu.Unlock()
	removeErr := make(chan error, 1)
	go func() {
		_, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
			Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash,
		})
		removeErr <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("removal did not reach exact teardown")
	}
	replacement := signedCapabilityRequest(t, key, now, "jti-race-b", "aud-b")
	if _, err := second.RegisterOAuthMCPCapability(context.Background(), replacement); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		close(release)
		t.Fatalf("concurrent replacement = %v, want pending", err)
	}
	close(release)
	if err := <-removeErr; err != nil {
		t.Fatalf("remove A: %v", err)
	}
	if _, err := second.RegisterOAuthMCPCapability(context.Background(), replacement); err != nil {
		t.Fatalf("same claimed replacement retry after concurrent removal: %v", err)
	}
}

func TestRegisterOAuthMCPCapability_CrossSessionServiceCannotReplaceDuringRemoval(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	firstService, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	secondService, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithClock(func() time.Time { return now }),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithConnectionDetacher(preparer),
		agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstService.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-cross-session-a", "aud-a"))
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	preparer.mu.Lock()
	preparer.detachEntered = entered
	preparer.detachRelease = release
	preparer.mu.Unlock()
	removeErr := make(chan error, 1)
	go func() {
		_, removeErrValue := firstService.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
			Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash,
		})
		removeErr <- removeErrValue
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("removal did not reach exact teardown")
	}
	otherSubject := prototypes.IdentityScope{Tenant: "t", User: "other-user", Session: "other-session"}
	replacement := signedCapabilityRequestFor(t, key, now, otherSubject, testAgentID, "jti-cross-session-b", "aud-b", "cap")
	if _, err := secondService.RegisterOAuthMCPCapability(context.Background(), replacement); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		close(release)
		t.Fatalf("cross-session replacement during removal = %v, want pending", err)
	}
	close(release)
	if err := <-removeErr; err != nil {
		t.Fatalf("remove A: %v", err)
	}
	if _, err := secondService.RegisterOAuthMCPCapability(context.Background(), replacement); err != nil {
		t.Fatalf("cross-session replacement after terminal removal: %v", err)
	}
}

func TestRegisterOAuthMCPCapability_CrossServiceRemovalAdmissionBlocksReplacement(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	baseService, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	first, err := baseService.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-admitted-a", "aud-a"))
	if err != nil {
		t.Fatalf("register A: %v", err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	operationState := &capabilityOperationPhaseStore{StateStore: st, phase: agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, entered: entered, release: release}
	newService := func(registry agentcfg.Registry, operationStore state.StateStore) *agentcfgprotocol.Service {
		t.Helper()
		svc, serviceErr := agentcfgprotocol.NewService(registry,
			agentcfgprotocol.WithClock(func() time.Time { return now }),
			agentcfgprotocol.WithConnectionPreparer(preparer),
			agentcfgprotocol.WithConnectionDetacher(preparer),
			agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
			agentcfgprotocol.WithSignedOAuthMCPOperationState(operationStore),
			agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
				"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
			}),
		)
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		return svc
	}
	removalService := newService(reg, operationState)
	replacementService := newService(reg, st)

	removeDone := make(chan error, 1)
	go func() {
		_, removeErr := removalService.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
			Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash,
		})
		removeDone <- removeErr
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("removal did not pause after pair absence and durable admission")
	}

	otherSubject := prototypes.IdentityScope{Tenant: "t", User: "other-user", Session: "other-session"}
	replacement := signedCapabilityRequestFor(t, key, now, otherSubject, testAgentID, "jti-admitted-b", "aud-b", "cap")
	if _, err := replacementService.RegisterOAuthMCPCapability(context.Background(), replacement); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		close(release)
		t.Fatalf("replacement during removal admission = %v, want pending", err)
	}
	close(release)
	if err := <-removeDone; err != nil {
		t.Fatalf("finish admitted removal: %v", err)
	}
	if _, err := replacementService.RegisterOAuthMCPCapability(context.Background(), replacement); err != nil {
		t.Fatalf("replacement after terminal removal: %v", err)
	}
}

func TestRemoveOAuthMCPCapability_PairAbsentCheckpointFailureDirectRetryCompletes(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	baseService, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	first, err := baseService.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-checkpoint-retry", "aud-a"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	checkpointErr := errors.New("injected removal receipt checkpoint failure")
	operationState := &capabilityOperationPhaseStore{StateStore: st, phase: agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted, failErr: checkpointErr}
	removalService, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithClock(func() time.Time { return now }),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithConnectionDetacher(preparer),
		agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(operationState),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash}
	if _, err := removalService.RemoveOAuthMCPCapability(context.Background(), request); !errors.Is(err, checkpointErr) {
		t.Fatalf("first removal = %v, want checkpoint failure", err)
	}
	active, set, err := reg.Active(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.Payload.SignedOAuthMCPPair != nil {
		t.Fatalf("pair absence did not survive checkpoint failure: set=%t active=%+v err=%v", set, active, err)
	}
	removed, err := removalService.RemoveOAuthMCPCapability(context.Background(), request)
	if err != nil || removed.OperationPhase != string(agentcfg.SignedOAuthMCPPhaseRemoved) {
		t.Fatalf("direct retry = phase=%q err=%v, want removed", removed.OperationPhase, err)
	}
	preparer.mu.Lock()
	live := len(preparer.live)
	preparer.mu.Unlock()
	if live != 0 {
		t.Fatalf("direct retry left dispatchable state: live=%d", live)
	}
}

func TestRemoveOAuthMCPCapability_RemovalAdmittedCarriesNewerSamePairSiblings(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, st, _ := signedCapabilityServiceWithRegistry(t, now)
	registered, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-admitted-sibling", "aud-a"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("active pair: set=%t active=%+v err=%v", set, active, err)
	}
	operations, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	op, err := operations.LoadForPair(context.Background(), q.TenantID, active.Payload.SignedOAuthMCPPair)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operations.Advance(context.Background(), op, agentcfg.SignedOAuthMCPPhaseRemovalAdmitted, active.RevisionID); err != nil {
		t.Fatal(err)
	}
	siblingPayload := active.Payload
	siblingPayload.ExtraSystemBlocks = &agentcfg.ExtraSystemBlocks{Blocks: []agentcfg.NamedBlock{{Name: "operator", Body: "preserve me"}}}
	sibling, err := reg.SetRevision(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent, siblingPayload, agentcfg.SetOptions{ExpectedContentHash: active.ContentHash})
	if err != nil {
		t.Fatalf("same-pair sibling writer: %v", err)
	}
	removed, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash,
	})
	if err != nil || removed.OperationPhase != string(agentcfg.SignedOAuthMCPPhaseRemoved) {
		t.Fatalf("remove after same-pair sibling = phase=%q err=%v", removed.OperationPhase, err)
	}
	current, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || current.Payload.SignedOAuthMCPPair != nil || current.ParentRevisionID != sibling.RevisionID {
		t.Fatalf("removed sibling revision: set=%t current=%+v sibling=%+v err=%v", set, current, sibling, err)
	}
	blocks := current.Payload.ExtraSystemBlockList()
	if len(blocks) != 1 || blocks[0].Name != "operator" || blocks[0].Body != "preserve me" {
		t.Fatalf("same-pair sibling content was not preserved: %+v", blocks)
	}
}

func TestRemoveOAuthMCPCapability_DefinitiveCASFailureRollsBackAdmissionAndSurfacesFenceCleanup(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	baseService, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
	first, err := baseService.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-remove-rollback", "aud-a"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	writeErr := errors.New("injected desired-state CAS refusal")
	cancelErr := errors.New("injected exact-fence cleanup refusal")
	preparer.mu.Lock()
	preparer.fenceCancelErrs = []error{cancelErr, nil}
	preparer.mu.Unlock()
	failingRegistry := &capabilityRemovalFailingRegistry{Registry: reg, failErr: writeErr}
	removalService, err := agentcfgprotocol.NewService(failingRegistry,
		agentcfgprotocol.WithClock(func() time.Time { return now }),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithConnectionDetacher(preparer),
		agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: first.Revision.ContentHash}
	if _, err := removalService.RemoveOAuthMCPCapability(context.Background(), request); !errors.Is(err, writeErr) || !errors.Is(err, cancelErr) {
		t.Fatalf("failed removal = %v, want desired CAS and cleanup errors", err)
	}
	active, set, err := reg.Active(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.ContentHash != first.Revision.ContentHash || active.Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("desired pair changed after definitive refusal: set=%t active=%+v err=%v", set, active, err)
	}
	operations, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	op, err := operations.LoadForPair(context.Background(), "t", active.Payload.SignedOAuthMCPPair)
	if err != nil || op.Phase != agentcfg.SignedOAuthMCPPhasePublished {
		t.Fatalf("operation after definitive refusal = phase=%q err=%v, want published", op.Phase, err)
	}
	if _, err := removalService.RemoveOAuthMCPCapability(context.Background(), request); err != nil {
		t.Fatalf("retry after released cleanup fence: %v", err)
	}
	preparer.mu.Lock()
	cancels, seals := preparer.fenceCancels, preparer.fenceSeals
	preparer.mu.Unlock()
	if cancels != 1 || seals != 1 {
		t.Fatalf("fence receipts after failure+retry: cancels=%d seals=%d, want 1/1", cancels, seals)
	}
}

func TestRegisterOAuthMCPCapability_PublishedReplayAcceptsCarriedPairSiblingRevision(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, reg, _, preparer := signedCapabilityServiceWithRegistry(t, now)
	req := signedCapabilityRequest(t, key, now, "jti-sibling-replay", "aud-sibling")
	registered, err := svc.RegisterOAuthMCPCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, set, err := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set {
		t.Fatalf("active: set=%t err=%v", set, err)
	}
	model := "sibling-model"
	payload := active.Payload
	payload.LLMParams = &agentcfg.LLMParams{Model: &model}
	sibling, err := reg.SetRevision(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{ExpectedContentHash: registered.Revision.ContentHash})
	if err != nil {
		t.Fatalf("generic sibling edit: %v", err)
	}
	replayed, err := svc.RegisterOAuthMCPCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("published replay after sibling edit: %v", err)
	}
	if replayed.Revision.RevisionID != sibling.RevisionID {
		t.Fatalf("replay revision = %q, want current carried pair %q", replayed.Revision.RevisionID, sibling.RevisionID)
	}
	preparer.mu.Lock()
	activations := preparer.activations
	preparer.mu.Unlock()
	if activations != 1 {
		t.Fatalf("published replay activations = %d, want one", activations)
	}
	if _, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: scope(), AgentID: testAgentID, ExpectedContentHash: replayed.Revision.ContentHash,
	}); err != nil {
		t.Fatalf("remove carried pair revision: %v", err)
	}
}

func TestRegisterOAuthMCPCapability_RevisionCASRefusalAllocatesNoCredentialResources(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &cleanupCapabilityProvider{}
	preparer := &cleanupCapabilityPreparer{}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithClock(func() time.Time { return now }),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithConnectionDetacher(preparer),
		agentcfgprotocol.WithProviderInstaller(cleanupCapabilityInstaller{provider: provider}),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	req := signedCapabilityRequest(t, key, now, "jti-cleanup-errors", "aud-cleanup")
	req.ExpectedContentHash = strings.Repeat("a", 64)
	_, err = svc.RegisterOAuthMCPCapability(context.Background(), req)
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("registration error = %v, want revision conflict", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if active, set, activeErr := reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent); activeErr != nil || set {
		t.Fatalf("active residue = (%+v, %t, %v), want none", active, set, activeErr)
	}
	if history, historyErr := reg.ListRevisions(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent, 0); historyErr != nil || len(history) != 0 {
		t.Fatalf("revision residue = (%+v, %v), want none", history, historyErr)
	}
	preparer.mu.Lock()
	connectionCloseCalls, activations, workers, connectionDeadline := preparer.closeCalls, preparer.activations, preparer.workers, preparer.observedDeadline
	preparer.mu.Unlock()
	provider.mu.Lock()
	providerCloseCalls, resources, providerDeadline := provider.closeCalls, provider.resources, provider.observedDeadline
	provider.mu.Unlock()
	if connectionCloseCalls != 0 || activations != 0 || workers != 0 || connectionDeadline {
		t.Fatalf("connection cleanup state: closes=%d activations=%d workers=%d deadline=%t", connectionCloseCalls, activations, workers, connectionDeadline)
	}
	if providerCloseCalls != 0 || resources != 0 || providerDeadline {
		t.Fatalf("provider cleanup state: closes=%d resources=%d deadline=%t", providerCloseCalls, resources, providerDeadline)
	}
}

func signedCapabilityRequest(t *testing.T, key *rsa.PrivateKey, now time.Time, jti, audience string) prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest {
	return signedCapabilityRequestFor(t, key, now, prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s"}, testAgentID, jti, audience, "cap")
}

func signedCapabilityRequestFor(t *testing.T, key *rsa.PrivateKey, now time.Time, scope prototypes.IdentityScope, agentID, jti, audience, connectionName string) prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest {
	t.Helper()
	canonical, sink, err := agentcfg.CanonicalOAuthMCPURL("https://example.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	claims := agentcfg.SignedOAuthMCPAuthorityClaims{
		TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session, AgentID: agentID, Broker: "broker", ProviderName: "provider", CapabilityRevision: "v1",
		URLDigest: agentcfg.OAuthMCPURLDigest(canonical), SinkDigest: agentcfg.OAuthMCPURLDigest(sink), Audience: audience, Scopes: []string{"read"},
		Connection:       agentcfg.SignedOAuthMCPConnectionDescriptor{Name: connectionName, URL: canonical},
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: jti, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(30 * time.Minute))},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "kid"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest{
		Identity: scope, AgentID: agentID, ProviderName: "provider", Broker: "broker", Audience: audience, Scopes: []string{"read"},
		Connection: prototypes.SignedOAuthMCPConnectionDescriptor{Name: connectionName, URL: canonical}, AuthorityEnvelope: raw,
	}
}

func TestRegisterOAuthMCPCapability_DurableReplayResumesPublishedOperation(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, _, preparer := signedCapabilityService(t, now)
	req := signedCapabilityRequest(t, key, now, "jti-1", "aud-1")
	first, err := svc.RegisterOAuthMCPCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	second, err := svc.RegisterOAuthMCPCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	if first.Revision.RevisionID != second.Revision.RevisionID {
		t.Fatalf("replay revision = %q, want %q", second.Revision.RevisionID, first.Revision.RevisionID)
	}
	preparer.mu.Lock()
	activations := preparer.activations
	preparer.mu.Unlock()
	if activations != 1 {
		t.Fatalf("activations = %d, want one", activations)
	}
	changed := signedCapabilityRequest(t, key, now, "jti-1", "aud-2")
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), changed); !errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
		t.Fatalf("different binding with same JTI = %v, want replay refusal", err)
	}
}

func TestRegisterOAuthMCPCapability_CommittedRevisionThenError_RecoversExactCandidate(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	preparer := &capabilityPreparer{}
	svc, err := agentcfgprotocol.NewService(&capabilityLandedThenErroredRegistry{Registry: reg, err: errors.New("injected acknowledgement loss")},
		agentcfgprotocol.WithClock(func() time.Time { return now }),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithConnectionDetacher(preparer),
		agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := svc.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, "jti-committed-error", "aud-1"))
	if err != nil {
		t.Fatalf("registration must recover committed revision: %v", err)
	}
	if registered.Revision.RevisionID == "" {
		t.Fatal("response omitted the exact recovered candidate revision")
	}
	active, set, err := reg.Active(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set {
		t.Fatalf("active = (%+v, %v, %v), want committed candidate", active, set, err)
	}
	if active.RevisionID != registered.Revision.RevisionID || active.Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("active candidate = %+v, want recovered signed pair %q", active, registered.Revision.RevisionID)
	}
}

func TestRegisterOAuthMCPCapability_PointerAndCompensationFailure_DoesNotPublishMatchingOrphan(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	base, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	faults := &capabilityOrphanFaultStore{StateStore: base}
	svc, key, reg, st, preparer := signedCapabilityServiceWithStore(t, now, faults)
	req := signedCapabilityRequest(t, key, now, "jti-matching-orphan", "aud-orphan")
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), req); !errors.Is(err, errCapabilityPointerWrite) || !errors.Is(err, errCapabilityOrphanDelete) {
		t.Fatalf("registration error = %v, want pointer and compensation failures", err)
	}

	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	history, err := reg.ListRevisions(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent, 0)
	if err != nil || len(history) != 1 || history[0].Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("matching orphan history = %+v, err=%v", history, err)
	}
	physical := reg.(interface {
		PhysicalActive(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
	})
	if revision, set, err := physical.PhysicalActive(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent); err != nil || set {
		t.Fatalf("physical active = (%+v, %t, %v), want absent despite matching history", revision, set, err)
	}
	ops, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	op, err := ops.LoadForPair(context.Background(), q.TenantID, history[0].Payload.SignedOAuthMCPPair)
	if err != nil || op.Phase != agentcfg.SignedOAuthMCPPhaseClaimed || op.RevisionID != "" {
		t.Fatalf("operation advanced on orphan: phase=%q revision=%q err=%v", op.Phase, op.RevisionID, err)
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(st)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fences.Load(context.Background(), q.TenantID, testAgentID)
	if err != nil || fence.Phase != agentcfg.SignedOAuthMCPFencePending || fence.CandidateRevisionID != "" {
		t.Fatalf("fence committed orphan: phase=%q revision=%q err=%v", fence.Phase, fence.CandidateRevisionID, err)
	}
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	if preparer.activations != 0 || len(preparer.live) != 0 {
		t.Fatalf("matching orphan was published: activations=%d live=%d", preparer.activations, len(preparer.live))
	}
}

// TestRegisterOAuthMCPCapability_PostgresTwoIndependentRuntimes exercises the
// actual multi-node StateStore seam. The second Service has its own Registry,
// EventBus, and database handle, so a successful replay/removal cannot depend
// on either process-local lock or the first runtime's in-memory receipt.
func TestRegisterOAuthMCPCapability_PostgresTwoIndependentRuntimes(t *testing.T) {
	dsn := os.Getenv("HARBOR_PG_DSN")
	if dsn == "" {
		t.Skip("HARBOR_PG_DSN not set; skipping Phase 233b real Postgres two-runtime recovery")
	}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	firstStore, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		_ = firstStore.Close(context.Background())
		t.Fatal(err)
	}
	firstBus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	secondBus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	firstRegistry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: firstStore, Bus: firstBus})
	if err != nil {
		t.Fatal(err)
	}
	secondRegistry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: secondStore, Bus: secondBus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = secondRegistry.Close(context.Background())
		_ = firstRegistry.Close(context.Background())
		_ = secondBus.Close(context.Background())
		_ = firstBus.Close(context.Background())
		_ = secondStore.Close(context.Background())
		_ = firstStore.Close(context.Background())
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	newService := func(reg agentcfg.Registry, st state.StateStore, prep *capabilityPreparer) *agentcfgprotocol.Service {
		t.Helper()
		svc, newErr := agentcfgprotocol.NewService(reg,
			agentcfgprotocol.WithClock(func() time.Time { return now }),
			agentcfgprotocol.WithConnectionPreparer(prep),
			agentcfgprotocol.WithConnectionDetacher(prep),
			agentcfgprotocol.WithProviderInstaller(capabilityInstaller{}),
			agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
			agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
				"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
			}),
		)
		if newErr != nil {
			t.Fatal(newErr)
		}
		return svc
	}
	first := newService(firstRegistry, firstStore, &capabilityPreparer{})
	secondPreparer := &capabilityPreparer{}
	second := newService(secondRegistry, secondStore, secondPreparer)
	// The Postgres test intentionally reuses the database across local reruns
	// and CI packages. Its JTI therefore must be unique per test invocation:
	// a fixed value would (correctly) collide with the prior run's durable
	// removed tombstone and turn test residue into a false failure.
	req := signedCapabilityRequest(t, key, now, fmt.Sprintf("jti-postgres-two-runtime-%d", time.Now().UnixNano()), "aud-postgres")
	registered, err := first.RegisterOAuthMCPCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("first runtime registration: %v", err)
	}
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(secondRegistry, secondStore, secondPreparer, secondPreparer, capabilityInstaller{})
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(context.Background(), q, testAgentID); err != nil {
		t.Fatalf("second runtime reconcile: %v", err)
	}
	secondPreparer.mu.Lock()
	activations := secondPreparer.activations
	secondPreparer.mu.Unlock()
	if activations != 1 {
		t.Fatalf("second runtime activations = %d, want restart attach", activations)
	}
	removed, err := second.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash})
	if err != nil {
		t.Fatalf("second runtime removal: %v", err)
	}
	if removed.OperationPhase != string(agentcfg.SignedOAuthMCPPhaseRemoved) {
		t.Fatalf("removal phase = %q, want removed", removed.OperationPhase)
	}
	if _, err := first.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash}); err != nil {
		t.Fatalf("first runtime terminal removal replay: %v", err)
	}
	oldNow := time.Now().UTC().Add(-3 * time.Hour)
	now = oldNow
	stableJTI := fmt.Sprintf("jti-postgres-stable-%d", time.Now().UnixNano())
	stableFirstPreparer := &capabilityPreparer{failActivate: errors.New("injected first-runtime stable-JTI activation failure")}
	stableFirst := newService(firstRegistry, firstStore, stableFirstPreparer)
	if _, err := stableFirst.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, oldNow, stableJTI, "aud-postgres-stable")); err == nil {
		t.Fatal("first Postgres runtime unexpectedly published stable-JTI generation")
	}
	now = oldNow.Add(2 * time.Hour)
	if _, err := second.RegisterOAuthMCPCapability(context.Background(), signedCapabilityRequest(t, key, now, stableJTI, "aud-postgres-stable")); err != nil {
		t.Fatalf("second Postgres runtime stable-JTI recovery: %v", err)
	}
	ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(secondStore)
	stable, err := ops.Load(context.Background(), agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: stableJTI})
	if err != nil || stable.Phase != agentcfg.SignedOAuthMCPPhasePublished || stable.AuthorityGeneration != 2 || stable.ExpiredAttemptCount != 1 {
		t.Fatalf("Postgres stable-JTI operation = %+v err=%v", stable, err)
	}
}

func TestRegisterOAuthMCPCapability_ConcurrentReplaySharesOnePublication(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, _, preparer := signedCapabilityService(t, now)
	req := signedCapabilityRequest(t, key, now, "jti-concurrent", "aud-1")
	const callers = 128
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.RegisterOAuthMCPCapability(context.Background(), req)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent registration: %v", err)
		}
	}
	preparer.mu.Lock()
	activations := preparer.activations
	preparer.mu.Unlock()
	if activations != 1 {
		t.Fatalf("activations = %d, want one publication", activations)
	}
}

// TestRegisterOAuthMCPCapability_ConcurrentMixedIdentityN128 shares one
// compiled verifier, registry, provider preparer, and operation store across
// 128 identities. Each success must remain bound to its own tenant/agent and
// a cancelled caller must not perturb a neighbour's durable operation.
func TestRegisterOAuthMCPCapability_ConcurrentMixedIdentityN128(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, key, _, _ := signedCapabilityService(t, now)
	const callers = 128
	type outcome struct {
		request   prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest
		response  prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse
		err       error
		cancelled bool
	}
	start := make(chan struct{})
	results := make(chan outcome, callers)
	var wg sync.WaitGroup
	for i := range callers {
		request := signedCapabilityRequestFor(t, key, now,
			prototypes.IdentityScope{Tenant: fmt.Sprintf("tenant-%02d", i%8), User: fmt.Sprintf("user-%02d", i%16), Session: fmt.Sprintf("session-%02d", i%32)},
			fmt.Sprintf("agent-%03d", i), fmt.Sprintf("jti-mixed-%03d", i), fmt.Sprintf("aud-%03d", i), fmt.Sprintf("cap-%03d", i))
		cancelled := i%8 == 0
		wg.Add(1)
		go func(req prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest, cancelled bool) {
			defer wg.Done()
			<-start
			ctx := context.Background()
			if cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			response, err := svc.RegisterOAuthMCPCapability(ctx, req)
			results <- outcome{request: req, response: response, err: err, cancelled: cancelled}
		}(request, cancelled)
	}
	close(start)
	wg.Wait()
	close(results)
	for result := range results {
		if result.cancelled {
			if !errors.Is(result.err, context.Canceled) {
				t.Fatalf("cancelled %s/%s = %v, want context cancellation", result.request.Identity.Tenant, result.request.AgentID, result.err)
			}
			continue
		}
		if result.err != nil {
			t.Fatalf("registration %s/%s: %v", result.request.Identity.Tenant, result.request.AgentID, result.err)
		}
		if result.response.Revision.RevisionID == "" {
			t.Fatalf("registration %s/%s omitted revision", result.request.Identity.Tenant, result.request.AgentID)
		}
		got, err := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: result.request.Identity, AgentID: result.request.AgentID})
		if err != nil || !got.Set || got.Revision.RevisionID != result.response.Revision.RevisionID {
			t.Fatalf("readback %s/%s = (%+v, %v), want own revision %q", result.request.Identity.Tenant, result.request.AgentID, got, err, result.response.Revision.RevisionID)
		}
	}
}
