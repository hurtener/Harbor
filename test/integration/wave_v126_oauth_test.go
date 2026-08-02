package integration_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/state"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// testE2EWaveV126SignedOAuth composes the actual signed-capability service
// with durable SQLite and its restart reconciler. The small prepared transport
// is a strict unpublished publication seam: no credential or token URL is
// accepted by it, and every assertion is made against the production service's
// signed receipt, active revision, and exact teardown calls.
func testE2EWaveV126SignedOAuth(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate authority key: %v", err)
	}
	dsn := filepath.Join(t.TempDir(), "wave-v126-signed-capability.db")
	const agentID = "wave-v126-capability-agent"
	scope := prototypes.IdentityScope{Tenant: "wave-v126-capability", User: "operator", Session: "control"}

	type runtime struct {
		store    state.StateStore
		bus      interface{ Close(context.Context) error }
		registry agentcfg.Registry
		service  *agentcfgprotocol.Service
		prep     *waveV126CapabilityPreparer
	}
	open := func() *runtime {
		store, openErr := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if openErr != nil {
			t.Fatalf("open signed-capability SQLite: %v", openErr)
		}
		bus, openErr := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
		if openErr != nil {
			_ = store.Close(ctx)
			t.Fatalf("open signed-capability bus: %v", openErr)
		}
		registry, openErr := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
		if openErr != nil {
			_ = bus.Close(ctx)
			_ = store.Close(ctx)
			t.Fatalf("open signed-capability registry: %v", openErr)
		}
		prep := &waveV126CapabilityPreparer{live: make(map[string]string)}
		service, openErr := agentcfgprotocol.NewService(registry,
			agentcfgprotocol.WithClock(func() time.Time { return now }),
			agentcfgprotocol.WithConnectionPreparer(prep),
			agentcfgprotocol.WithConnectionDetacher(prep),
			agentcfgprotocol.WithProviderInstaller(waveV126CapabilityInstaller{}),
			agentcfgprotocol.WithSignedOAuthMCPOperationState(store),
			agentcfgprotocol.WithRunSnapshotGate(runsnapshot.NewGate()),
			agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
				"broker": {Broker: "broker", Issuer: "issuer", Keys: waveV126KeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
			}),
		)
		if openErr != nil {
			t.Fatalf("open signed-capability service: %v", openErr)
		}
		return &runtime{store: store, bus: bus, registry: registry, service: service, prep: prep}
	}
	closeRuntime := func(r *runtime) { _ = r.registry.Close(ctx); _ = r.bus.Close(ctx); _ = r.store.Close(ctx) }

	first := open()
	request := waveV126SignedRequest(t, key, now, scope, agentID, "v126-jti-register", "capability")
	registered, err := first.service.RegisterOAuthMCPCapability(ctx, request)
	if err != nil {
		t.Fatalf("signed registration: %v", err)
	}
	if registered.Revision.ContentHash == "" || registered.ProviderName != "provider" || registered.ConnectionName != "capability" {
		t.Fatalf("closed registration response=%+v", registered)
	}
	if _, err := first.service.RegisterOAuthMCPCapability(ctx, waveV126SignedRequest(t, key, now, scope, agentID, "v126-jti-denied", "denied", "write")); err == nil {
		t.Fatal("scope above authority ceiling was accepted")
	}
	closeRuntime(first)

	second := open()
	defer func() {
		if second != nil {
			closeRuntime(second)
		}
	}()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}}
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(second.registry, second.store, second.prep, second.prep, waveV126CapabilityInstaller{})
	if err != nil {
		t.Fatalf("create restart reconciler: %v", err)
	}
	if err := reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, agentID); err != nil {
		t.Fatalf("restart reconcile: %v", err)
	}
	if !second.prep.matches(scope.Tenant, agentID, "capability") {
		t.Fatal("restart did not reattach the exact signed pair")
	}
	removed, err := second.service.RemoveOAuthMCPCapability(ctx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope, AgentID: agentID, ExpectedContentHash: registered.Revision.ContentHash})
	if err != nil || removed.OperationPhase != string(agentcfg.SignedOAuthMCPPhaseRemoved) {
		t.Fatalf("paired removal=(%+v,%v)", removed, err)
	}
	if second.prep.matches(scope.Tenant, agentID, "capability") {
		t.Fatal("paired removal left a dispatchable connection")
	}

	// D-401 cleanup is separate durable work. Persist a cleanup fault, destroy
	// the process, and prove the restarted service resumes only the frozen pair.
	cleanup, err := second.service.RegisterOAuthMCPCapability(ctx, waveV126SignedRequest(t, key, now, scope, agentID, "v126-jti-cleanup", "cleanup"))
	if err != nil {
		t.Fatalf("signed registration for retirement cleanup: %v", err)
	}
	second.prep.failExact = 1
	retirement := prototypes.AgentConfigRetireRequest{Identity: scope, AgentID: agentID, OperationID: "wave-v126-signed-cleanup", ExpectedContentHash: cleanup.Revision.ContentHash}
	if _, err := second.service.Retire(ctx, retirement); err == nil {
		t.Fatal("signed retirement cleanup fault did not fail loud")
	}
	closeRuntime(second)
	second = nil
	third := open()
	defer closeRuntime(third)
	completed, err := third.service.Retire(ctx, retirement)
	if err != nil || !completed.Status.Completed {
		t.Fatalf("signed cleanup restart retry=(%+v,%v)", completed, err)
	}
	if _, _, err := third.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("signed cleanup retirement did not retain terminal tombstone: %v", err)
	}
}

type waveV126KeySet struct{ key crypto.PublicKey }

func (s waveV126KeySet) KeyByID(string) (crypto.PublicKey, string, error) {
	return s.key, jwt.SigningMethodRS256.Alg(), nil
}

type waveV126CapabilityInstaller struct{}

func (waveV126CapabilityInstaller) InstallProvider(context.Context, string, string, agentcfg.OAuthProviderDescriptor) error {
	return nil
}
func (waveV126CapabilityInstaller) UninstallProvider(context.Context, string, string, string) error {
	return nil
}
func (waveV126CapabilityInstaller) PrepareSignedCapabilityProvider(context.Context, string, toolauth.SignedCapabilityExchangeBinding, []string) (agentcfgprotocol.PreparedOAuthProvider, error) {
	return waveV126PreparedProvider{}, nil
}

type waveV126PreparedProvider struct{}

func (waveV126PreparedProvider) Binding() toolauth.OAuthProvider { return nil }
func (waveV126PreparedProvider) Publish(context.Context) error   { return nil }
func (waveV126PreparedProvider) Commit(context.Context)          {}
func (waveV126PreparedProvider) Rollback(context.Context) error  { return nil }
func (waveV126PreparedProvider) Close(context.Context) error     { return nil }

type waveV126CapabilityPreparer struct {
	mu        sync.Mutex
	live      map[string]string
	failExact int
}

func (p *waveV126CapabilityPreparer) PrepareConnection(_ context.Context, req agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	return waveV126PreparedConnection{parent: p, request: req}, nil
}
func (p *waveV126CapabilityPreparer) DetachConnection(_ context.Context, tenant, agentID, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.live, tenant+"/"+agentID+"/"+name)
	return nil
}
func (p *waveV126CapabilityPreparer) DetachExactConnection(_ context.Context, tenant, agentID, name, fingerprint string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failExact > 0 {
		p.failExact--
		return fmt.Errorf("injected exact teardown failure")
	}
	key := tenant + "/" + agentID + "/" + name
	if live, ok := p.live[key]; ok && live != fingerprint {
		// A different local generation owns this name. Exact cleanup preserves
		// it; the durable publisher receipt independently fences the stale pair.
		return nil
	}
	delete(p.live, key)
	return nil
}
func (*waveV126CapabilityPreparer) BeginExactConnectionTeardown(string, string, string, string) (agentcfgprotocol.ExactConnectionTeardownFence, error) {
	return waveV126TeardownFence{}, nil
}
func (p *waveV126CapabilityPreparer) ConnectionMatches(owner toolauth.Owner, name, fingerprint string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live[owner.Tenant+"/"+owner.Agent+"/"+name] == fingerprint
}
func (p *waveV126CapabilityPreparer) matches(tenant, agent, name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.live[tenant+"/"+agent+"/"+name]
	return ok
}

type waveV126TeardownFence struct{}

func (waveV126TeardownFence) Seal()                        {}
func (waveV126TeardownFence) Cancel(context.Context) error { return nil }

type waveV126PreparedConnection struct {
	parent  *waveV126CapabilityPreparer
	request agentcfgprotocol.AttachRequest
}

func (c waveV126PreparedConnection) Activate(context.Context) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()
	c.parent.live[c.request.Identity.TenantID+"/"+c.request.AgentID+"/"+c.request.Name] = c.request.DescriptorFingerprint
	return nil
}
func (c waveV126PreparedConnection) ActivateIf(ctx context.Context, prove func(context.Context) error) error {
	if err := prove(ctx); err != nil {
		return err
	}
	return c.Activate(ctx)
}
func (c waveV126PreparedConnection) ActivateUnder(ctx context.Context, admit func(context.Context, func() error) error) error {
	return admit(ctx, func() error { return c.Activate(ctx) })
}
func (waveV126PreparedConnection) Close(context.Context) error { return nil }

func waveV126SignedRequest(t *testing.T, key *rsa.PrivateKey, now time.Time, scope prototypes.IdentityScope, agentID, jti, connection string, scopes ...string) prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []string{"read"}
	}
	canonical, sink, err := agentcfg.CanonicalOAuthMCPURL("https://example.test/mcp")
	if err != nil {
		t.Fatalf("canonical capability URL: %v", err)
	}
	claims := agentcfg.SignedOAuthMCPAuthorityClaims{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session, AgentID: agentID, Broker: "broker", ProviderName: "provider", CapabilityRevision: "v1", URLDigest: agentcfg.OAuthMCPURLDigest(canonical), SinkDigest: agentcfg.OAuthMCPURLDigest(sink), Audience: "audience", Scopes: scopes, Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: connection, URL: canonical}, RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", ID: jti, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(30 * time.Minute))}}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "kid"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign authority: %v", err)
	}
	return prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest{Identity: scope, AgentID: agentID, ProviderName: "provider", Broker: "broker", Audience: "audience", Scopes: scopes, Connection: prototypes.SignedOAuthMCPConnectionDescriptor{Name: connection, URL: canonical}, AuthorityEnvelope: raw}
}
