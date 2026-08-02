package protocol_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
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
func (capabilityInstaller) PrepareSignedCapabilityProvider(context.Context, string, string, string, string, string, string, []string) (agentcfgprotocol.PreparedOAuthProvider, error) {
	return capabilityPreparedProvider{}, nil
}

type capabilityPreparer struct {
	mu          sync.Mutex
	activations int
	detaches    int
}

// capabilityLandedThenErroredRegistry models the production-shaped ambiguous
// acknowledgement: the immutable revision and active pointer commit, but the
// caller receives an error. The D-401 recovery path must exact-reread the
// candidate and continue with THAT revision, never the stale predecessor.
type capabilityLandedThenErroredRegistry struct {
	agentcfg.Registry
	err  error
	once sync.Once
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

func (p *capabilityPreparer) PrepareConnection(context.Context, agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	return capabilityPreparedConnection{parent: p}, nil
}

type capabilityPreparedConnection struct{ parent *capabilityPreparer }

func (p capabilityPreparedConnection) Activate(context.Context) error {
	p.parent.mu.Lock()
	p.parent.activations++
	p.parent.mu.Unlock()
	return nil
}
func (capabilityPreparedConnection) Close(context.Context) error { return nil }

func (p *capabilityPreparer) DetachConnection(context.Context, string, string, string) error {
	p.mu.Lock()
	p.detaches++
	p.mu.Unlock()
	return nil
}

func signedCapabilityService(t *testing.T, now time.Time) (*agentcfgprotocol.Service, *rsa.PrivateKey, state.StateStore, *capabilityPreparer) {
	t.Helper()
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
	return svc, key, st, preparer
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
	again, err := svc.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID})
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

func signedCapabilityRequest(t *testing.T, key *rsa.PrivateKey, now time.Time, jti, audience string) prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest {
	return signedCapabilityRequestFor(t, key, now, prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s"}, testAgentID, jti, audience, "cap")
}

func signedCapabilityRequestFor(t *testing.T, key *rsa.PrivateKey, now time.Time, scope prototypes.IdentityScope, agentID, jti, audience, connectionName string) prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest {
	t.Helper()
	canonical, _, err := agentcfg.CanonicalOAuthMCPURL("https://example.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	claims := agentcfg.SignedOAuthMCPAuthorityClaims{
		TenantID: scope.Tenant, AgentID: agentID, Broker: "broker", ProviderName: "provider", CapabilityRevision: "v1",
		URLDigest: agentcfg.OAuthMCPURLDigest(canonical), Audience: audience, Scopes: []string{"read"},
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
	second := newService(secondRegistry, secondStore, &capabilityPreparer{})
	// The Postgres test intentionally reuses the database across local reruns
	// and CI packages. Its JTI therefore must be unique per test invocation:
	// a fixed value would (correctly) collide with the prior run's durable
	// removed tombstone and turn test residue into a false failure.
	req := signedCapabilityRequest(t, key, now, fmt.Sprintf("jti-postgres-two-runtime-%d", time.Now().UnixNano()), "aud-postgres")
	registered, err := first.RegisterOAuthMCPCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("first runtime registration: %v", err)
	}
	replayed, err := second.RegisterOAuthMCPCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("second runtime exact replay: %v", err)
	}
	if replayed.Revision.RevisionID != registered.Revision.RevisionID {
		t.Fatalf("second runtime revision = %q, want %q", replayed.Revision.RevisionID, registered.Revision.RevisionID)
	}
	removed, err := second.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ExpectedContentHash: registered.Revision.ContentHash})
	if err != nil {
		t.Fatalf("second runtime removal: %v", err)
	}
	if removed.OperationPhase != string(agentcfg.SignedOAuthMCPPhaseRemoved) {
		t.Fatalf("removal phase = %q, want removed", removed.OperationPhase)
	}
	if _, err := first.RemoveOAuthMCPCapability(context.Background(), prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID}); err != nil {
		t.Fatalf("first runtime terminal removal replay: %v", err)
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
