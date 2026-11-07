package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

const (
	signedOwnerBearer   = "fixture-owner-bearer"
	signedOwnerAudience = "owner-audience"
)

type signedOwnerSubject struct {
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
}

type signedOwnerExchange struct {
	subject   signedOwnerSubject
	actor     signedOwnerSubject
	operation string
	refused   bool
}

// signedOwnerFixture is a production-path graph: real attacher, provider
// builder and token-exchange driver against a broker that, like a governing
// platform, refuses an ordinary mint whose subject differs from the signed
// pair's actor, and an MCP endpoint that requires the exchanged bearer.
type signedOwnerFixture struct {
	t        *testing.T
	now      time.Time
	owner    identity.Identity
	endUser  identity.Identity
	key      *rsa.PrivateKey
	mcpURL   string
	dsn      string
	bus      events.EventBus
	builder  config.ToolsConfig
	redactor *auditpatterns.Driver

	exchangeMu sync.Mutex
	exchanges  []signedOwnerExchange

	// stall, when armed, blocks every request to the /stall/ endpoint until
	// it is closed (or the request ends).
	stallMu sync.Mutex
	stall   chan struct{}
	stalled chan struct{}
}

type signedOwnerRuntime struct {
	registry    agentcfg.Registry
	store       state.StateStore
	catalog     tools.ToolCatalog
	mcpRegistry *mcpdrv.Registry
	attacher    *MCPConnectionAttacher
	installer   *OAuthProviderInstaller
	reconciler  *agentcfgprotocol.SignedOAuthMCPReconciler
	service     *agentcfgprotocol.Service
}

func newSignedOwnerFixture(t *testing.T) *signedOwnerFixture {
	t.Helper()
	t.Setenv("HARBOR_SIGNED_OWNER_KEK", "0303030303030303030303030303030303030303030303030303030303030303")
	t.Setenv("HARBOR_SIGNED_OWNER_BROKER_AUTH", "fixture-owner-broker-auth")
	f := &signedOwnerFixture{
		t:       t,
		now:     time.Now().UTC(),
		owner:   identity.Identity{TenantID: "tenant", UserID: "pair-owner", SessionID: "owner-session"},
		endUser: identity.Identity{TenantID: "tenant", UserID: "end-user", SessionID: "end-session"},
		dsn:     filepath.Join(t.TempDir(), "signed-owner-identity.sqlite"),
		stalled: make(chan struct{}, 8),
	}
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "owner-fixture", Version: "v0"}, nil)
	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}, func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	})
	mcpHandler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mcpServer }, nil)
	tlsMCP := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Only requests (POST) stall; a session-termination DELETE answers, so
		// a cancelled preparation can tear its transport down.
		if strings.HasPrefix(req.URL.Path, "/stall/") && req.Method == http.MethodPost {
			f.stallMu.Lock()
			stall := f.stall
			f.stallMu.Unlock()
			if stall != nil {
				select {
				case f.stalled <- struct{}{}:
				default:
				}
				select {
				case <-stall:
				case <-req.Context().Done():
					return
				}
			}
		}
		if !strings.HasSuffix(req.URL.Path, "/mcp") || req.Header.Get("Authorization") != "Bearer "+signedOwnerBearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, req)
	}))
	t.Cleanup(tlsMCP.Close)
	f.mcpURL = tlsMCP.URL
	oldDefaultTransport := http.DefaultTransport
	http.DefaultTransport = tlsMCP.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldDefaultTransport })

	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)
	brokerMux.HandleFunc("/credential", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer fixture-owner-broker-auth" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"format_version": 2, "tenant_id": req.Header.Get("X-Harbor-Credential-Tenant"), "client_id": "fixture-client", "client_secret": "fixture-secret", "expires_in": 300,
		})
	})
	brokerMux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			http.Error(w, "parse form", http.StatusBadRequest)
			return
		}
		form := maps.Clone(req.Form)
		var ex signedOwnerExchange
		if !decodeOwnerExchangeField(form.Get("subject_token"), &ex.subject) || !decodeOwnerExchangeField(form.Get("actor_token"), &ex.actor) {
			http.Error(w, "invalid subject or actor", http.StatusBadRequest)
			return
		}
		ex.operation = form.Get("app_operation")
		response := map[string]any{"access_token": signedOwnerBearer, "token_type": "Bearer", "expires_in": 300,
			"scope": "read", "audience": signedOwnerAudience, "resource": tlsMCP.URL}
		switch {
		case ex.operation != "":
			sum := sha256.Sum256([]byte(ex.operation))
			response["expires_in"], response["app_operation_sha256"] = 30, hex.EncodeToString(sum[:])
			delete(response, "scope")
		case ex.subject != ex.actor:
			ex.refused = true
		}
		f.exchangeMu.Lock()
		f.exchanges = append(f.exchanges, ex)
		f.exchangeMu.Unlock()
		if ex.refused {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"access_denied","reason":"governed_app_operation_required"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	})

	f.redactor = auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, f.redactor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	f.bus = bus
	f.builder = config.ToolsConfig{
		OAuthTokenKEKEnv: "HARBOR_SIGNED_OWNER_KEK",
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker", TokenURL: brokerServer.URL + "/token", CredentialURL: brokerServer.URL + "/credential",
			AuthTokenEnv: "HARBOR_SIGNED_OWNER_BROKER_AUTH", ScopeCeiling: []string{"read"},
			AllowedDownstreamHosts: []string{"boot-ceiling.invalid"},
		}},
	}
	f.key, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *signedOwnerFixture) snapshot() []signedOwnerExchange {
	f.exchangeMu.Lock()
	defer f.exchangeMu.Unlock()
	return append([]signedOwnerExchange(nil), f.exchanges...)
}

func (f *signedOwnerFixture) armStall() chan struct{} {
	release := make(chan struct{})
	f.stallMu.Lock()
	f.stall = release
	f.stallMu.Unlock()
	return release
}

// openRuntime opens an independent runtime graph over the shared database.
// wrap optionally decorates its state store (fault injection).
func (f *signedOwnerFixture) openRuntime(wrap func(state.StateStore) state.StateStore) signedOwnerRuntime {
	t := f.t
	t.Helper()
	base, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: f.dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close(context.Background()) })
	store := base
	if wrap != nil {
		store = wrap(base)
	}
	registry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: store, Bus: f.bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	builder, err := toolauth.NewProviderBuilder(context.Background(), f.builder, toolauth.BuildDeps{
		State: store, Bus: f.bus, Redactor: f.redactor, Coordinator: pauseresume.New(pauseresume.WithBus(f.bus)),
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog := tools.NewCatalog()
	providerSet := toolauth.NewProviderSet(nil)
	mcpRegistry := mcpdrv.NewRegistry()
	attacher := NewMCPConnectionAttacher(catalog, mcpRegistry, f.bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)), f.owner, nil, providerSet, nil)
	t.Cleanup(func() { _ = attacher.Close(context.Background()) })
	installer := NewOAuthProviderInstaller(builder, providerSet, false, nil)
	reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(registry, store, attacher, attacher, installer)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentcfgprotocol.NewService(registry,
		agentcfgprotocol.WithBus(f.bus),
		agentcfgprotocol.WithConnectionPreparer(attacher),
		agentcfgprotocol.WithConnectionDetacher(attacher),
		agentcfgprotocol.WithProviderInstaller(installer),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(store),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: productionSignedCapabilityKeySet{key: &f.key.PublicKey},
				ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: 10 * time.Minute},
		}),
		agentcfgprotocol.WithClock(func() time.Time { return f.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	return signedOwnerRuntime{registry: registry, store: store, catalog: catalog, mcpRegistry: mcpRegistry,
		attacher: attacher, installer: installer, reconciler: reconciler, service: service}
}

// register pushes one agent-tier pair as the owner.
func (f *signedOwnerFixture) register(rt signedOwnerRuntime, agentID, providerName, connectionName, path, jti string) error {
	t := f.t
	t.Helper()
	canonicalURL, sink, err := agentcfg.CanonicalOAuthMCPURL(f.mcpURL + path)
	if err != nil {
		t.Fatal(err)
	}
	connection := agentcfg.SignedOAuthMCPConnectionDescriptor{Name: connectionName, URL: canonicalURL}
	claims := agentcfg.SignedOAuthMCPAuthorityClaims{
		TenantID: f.owner.TenantID, UserID: f.owner.UserID, SessionID: f.owner.SessionID, AgentID: agentID,
		Broker: "broker", ProviderName: providerName, CapabilityRevision: "capability-revision-1",
		URLDigest: agentcfg.OAuthMCPURLDigest(canonicalURL), SinkDigest: agentcfg.OAuthMCPURLDigest(sink),
		Audience: signedOwnerAudience, Scopes: []string{"read"}, Connection: connection,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "issuer", ID: jti, IssuedAt: jwt.NewNumericDate(f.now), ExpiresAt: jwt.NewNumericDate(f.now.Add(10 * time.Minute)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "kid"
	envelope, err := token.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx, err := identity.WithVerified(context.Background(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.service.RegisterOAuthMCPCapability(ownerCtx, prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest{
		Identity: prototypes.IdentityScope{Tenant: f.owner.TenantID, User: f.owner.UserID, Session: f.owner.SessionID},
		AgentID:  agentID, ProviderName: providerName, Broker: "broker", Audience: claims.Audience, Scopes: []string{"read"},
		Connection:        prototypes.SignedOAuthMCPConnectionDescriptor{Name: connectionName, URL: canonicalURL},
		AuthorityEnvelope: envelope,
	})
	return err
}

// endUserRun builds the run context the run loop builds for an end user.
func (f *signedOwnerFixture) endUserRun() (context.Context, identity.Quadruple) {
	t := f.t
	t.Helper()
	q := identity.Quadruple{Identity: f.endUser, RunID: "end-user-run"}
	ctx, err := identity.WithVerified(context.Background(), f.endUser)
	if err != nil {
		t.Fatal(err)
	}
	if ctx, err = identity.WithRun(ctx, f.endUser, q.RunID); err != nil {
		t.Fatal(err)
	}
	return ctx, q
}

// TestSignedOAuthMCPReconcile_AgentPairPreparesAsOwnerUnderEndUserRun: a
// run-start reconcile of an agent-tier pair triggered by an END USER's run
// must prepare the private connection as the frozen pair owner (subject ==
// actor), so the attach succeeds. A live planner call by that end user must
// still exchange as the end user, receive the governed refusal, and retry
// with a bound operation.
func TestSignedOAuthMCPReconcile_AgentPairPreparesAsOwnerUnderEndUserRun(t *testing.T) {
	const agentID, connectionName = "agent", "owner-signed"
	f := newSignedOwnerFixture(t)
	first := f.openRuntime(nil)
	if err := f.register(first, agentID, "owner-provider", connectionName, "/mcp", "jti-owner"); err != nil {
		t.Fatalf("owner registration: %v", err)
	}
	registered := len(f.snapshot())

	// Runtime B has no local handle; its run-start reconcile runs in an END
	// USER's run context.
	second := f.openRuntime(nil)
	runCtx, endUserQuad := f.endUserRun()
	if err := second.reconciler.ReconcileSignedOAuthMCPCapability(runCtx, endUserQuad, agentID); err != nil {
		t.Fatalf("agent-tier reconcile under end-user run: %v", err)
	}
	preparation := f.snapshot()[registered:]
	if len(preparation) == 0 {
		t.Fatal("reconcile made no preparation exchange")
	}
	for _, ex := range preparation {
		if ex.refused || ex.operation != "" || ex.subject.UserID != f.owner.UserID || ex.subject.SessionID != f.owner.SessionID || ex.subject != ex.actor {
			t.Fatalf("preparation exchange = %+v, want subject == frozen owner actor", ex)
		}
	}
	physicalName := mcpdrv.PhysicalServerName(connectionName, toolauth.Owner{Tenant: f.owner.TenantID, Agent: agentID})
	tool, ok := second.catalog.Resolve(physicalName + "_echo")
	if !ok {
		t.Fatal("agent-tier pair did not attach under the end-user run")
	}

	beforeLive := len(f.snapshot())
	callCtx := appoperation.WithAgentInvocation(tools.WithEffectiveAgentConfig(runCtx, agentID), agentID)
	if _, err := tool.Invoke(callCtx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("live end-user call: %v", err)
	}
	live := f.snapshot()[beforeLive:]
	if len(live) != 2 || !live[0].refused || live[0].operation != "" || live[1].refused || live[1].operation == "" {
		t.Fatalf("live exchanges = %+v, want governed refusal then bound operation", live)
	}
	for _, ex := range live {
		if ex.subject.UserID != f.endUser.UserID || ex.subject.SessionID != f.endUser.SessionID {
			t.Fatalf("live exchange subject = %+v, want the end user", ex.subject)
		}
	}
}

// fenceCommitFailingStore fails the activation fence's transition to
// committed, the window in which a published pair is live while the logical
// Active read still returns the prior revision.
type fenceCommitFailingStore struct {
	state.StateStore
	fail atomic.Bool
}

func (s *fenceCommitFailingStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if s.fail.Load() && strings.HasPrefix(next.Kind, "agentcfg.signed_oauth_mcp.activation_fence") &&
		bytes.Contains(next.Bytes, []byte(`"phase":"`+string(agentcfg.SignedOAuthMCPFenceCommitted)+`"`)) {
		return errors.New("injected activation fence commit failure")
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

// TestSignedOAuthMCPReconcile_GenericLegNeverDetachesPendingFencePair: a
// signed pair whose publication succeeded but whose fence commit failed is
// live while the logical revision still hides it. The generic run-start
// detach leg (which now runs even after a signed failure) must not tear it
// down.
func TestSignedOAuthMCPReconcile_GenericLegNeverDetachesPendingFencePair(t *testing.T) {
	const agentID, connectionName = "agent", "pending-fence"
	f := newSignedOwnerFixture(t)
	var faulty *fenceCommitFailingStore
	rt := f.openRuntime(func(base state.StateStore) state.StateStore {
		faulty = &fenceCommitFailingStore{StateStore: base}
		return faulty
	})
	faulty.fail.Store(true)
	if err := f.register(rt, agentID, "pending-provider", connectionName, "/mcp", "jti-pending"); err == nil {
		t.Fatal("fixture: fence commit failure did not surface")
	}
	ownerQuad := identity.Quadruple{Identity: f.owner}
	if active, set, err := rt.registry.Active(context.Background(), ownerQuad, agentID, agentcfg.ConfigScopeAgent); err != nil {
		t.Fatal(err)
	} else if pairs, _ := active.Payload.EffectiveSignedOAuthMCPPairs(); set && len(pairs) != 0 {
		t.Fatalf("fixture: logical Active already exposes the pair (%d)", len(pairs))
	}
	physicalName := mcpdrv.PhysicalServerName(connectionName, toolauth.Owner{Tenant: f.owner.TenantID, Agent: agentID})
	if _, ok := rt.catalog.Resolve(physicalName + "_echo"); !ok {
		t.Fatal("fixture: publication did not land before the fence failure")
	}

	runCtx, endUserQuad := f.endUserRun()
	detacher := NewMCPConnectionDetacher(rt.catalog, rt.mcpRegistry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	detached, _, err := projection.ReconcileConnections(runCtx, rt.registry, agentID, endUserQuad, detacher, nil, nil)
	if err != nil {
		t.Fatalf("generic reconcile: %v", err)
	}
	if detached != 0 {
		t.Fatalf("generic leg detached %d connections", detached)
	}
	if _, ok := rt.catalog.Resolve(physicalName + "_echo"); !ok {
		t.Fatal("generic leg tore down a published signed pair hidden by its pending fence")
	}
	// Defense in depth: even a direct generic deregister is refused.
	if err := detacher.Detach(runCtx, connectionName, toolauth.Owner{Tenant: f.owner.TenantID, Agent: agentID}); !errors.Is(err, mcpdrv.ErrExactTeardownRequired) {
		t.Fatalf("direct generic detach = %v, want exact-teardown refusal", err)
	}
	if _, ok := rt.catalog.Resolve(physicalName + "_echo"); !ok {
		t.Fatal("direct generic detach tore down the signed pair")
	}

	// The signed reconciler still converges the fence once the store heals.
	faulty.fail.Store(false)
	if err := rt.reconciler.ReconcileSignedOAuthMCPCapability(runCtx, endUserQuad, agentID); err != nil {
		t.Fatalf("signed reconcile after heal: %v", err)
	}
	if active, set, err := rt.registry.Active(context.Background(), ownerQuad, agentID, agentcfg.ConfigScopeAgent); err != nil || !set {
		t.Fatalf("active after heal = %v %v", set, err)
	} else if pairs, _ := active.Payload.EffectiveSignedOAuthMCPPairs(); len(pairs) != 1 {
		t.Fatalf("healed active pairs = %d, want 1", len(pairs))
	}
}

// TestSignedOAuthMCPReconcile_StalledOwnerDoesNotBlockOtherOwner: with the
// production attacher, an owner whose downstream preparation stalls must not
// hold up a different owner's reconcile, and a same-name waiter honours its
// own cancellation.
func TestSignedOAuthMCPReconcile_StalledOwnerDoesNotBlockOtherOwner(t *testing.T) {
	const stalledAgent, healthyAgent = "agent-stalled", "agent-healthy"
	f := newSignedOwnerFixture(t)
	first := f.openRuntime(nil)
	if err := f.register(first, stalledAgent, "stalled-provider", "stalled", "/stall/mcp", "jti-stalled"); err != nil {
		t.Fatalf("register stalled: %v", err)
	}
	if err := f.register(first, healthyAgent, "healthy-provider", "healthy", "/mcp", "jti-healthy"); err != nil {
		t.Fatalf("register healthy: %v", err)
	}

	second := f.openRuntime(nil)
	release := f.armStall()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	runCtx, endUserQuad := f.endUserRun()
	stalledDone := make(chan error, 1)
	go func() {
		stalledDone <- second.reconciler.ReconcileSignedOAuthMCPCapability(runCtx, endUserQuad, stalledAgent)
	}()
	select {
	case <-f.stalled:
	case <-time.After(20 * time.Second):
		t.Fatal("stalled owner's preparation never reached the downstream")
	}

	healthyDone := make(chan error, 1)
	go func() {
		healthyDone <- second.reconciler.ReconcileSignedOAuthMCPCapability(runCtx, endUserQuad, healthyAgent)
	}()
	select {
	case err := <-healthyDone:
		if err != nil {
			t.Fatalf("healthy owner reconcile: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("healthy owner queued behind a stalled owner's preparation")
	}
	healthyName := mcpdrv.PhysicalServerName("healthy", toolauth.Owner{Tenant: f.owner.TenantID, Agent: healthyAgent})
	if _, ok := second.catalog.Resolve(healthyName + "_echo"); !ok {
		t.Fatal("healthy owner did not attach")
	}

	// A waiter for the SAME stalled connection honours its own deadline.
	stalledOwner := toolauth.Owner{Tenant: f.owner.TenantID, Agent: stalledAgent}
	waitCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := second.attacher.PrepareConnection(waitCtx, agentcfgprotocol.AttachRequest{
		Identity: f.owner, AgentID: stalledAgent, Owner: stalledOwner, Name: "stalled",
		Transport: agentcfg.MCPTransportHTTP, URL: f.mcpURL + "/stall/mcp",
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-name waiter = %v, want its own deadline", err)
	}

	close(release)
	released = true
	select {
	case err := <-stalledDone:
		if err != nil {
			t.Fatalf("stalled owner reconcile after release: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("stalled owner never finished after release")
	}
	if n := second.attacher.gates.len(); n != 0 {
		t.Fatalf("attach gates retained = %d, want 0", n)
	}
}

func decodeOwnerExchangeField(encoded string, out any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && json.Unmarshal(raw, out) == nil
}

// TestMCPConnectionAttacher_CloseJoinsInFlightPreparation: Close must cancel
// and join an in-flight preparation before draining closers, and a prepared
// connection activated after Close must be refused rather than published.
func TestMCPConnectionAttacher_CloseJoinsInFlightPreparation(t *testing.T) {
	f := newSignedOwnerFixture(t)
	owner := toolauth.Owner{Tenant: f.owner.TenantID, Agent: "agent"}
	request := func(name, path string) agentcfgprotocol.AttachRequest {
		return agentcfgprotocol.AttachRequest{Identity: f.owner, AgentID: owner.Agent, Owner: owner, Name: name,
			Transport: agentcfg.MCPTransportHTTP, URL: f.mcpURL + path,
			Headers: map[string]string{"Authorization": "Bearer " + signedOwnerBearer}}
	}
	ownerCtx, err := identity.With(context.Background(), f.owner)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("in-flight preparation", func(t *testing.T) {
		rt := f.openRuntime(nil)
		release := f.armStall()
		defer close(release)
		prepDone := make(chan error, 1)
		go func() {
			prepared, err := rt.attacher.PrepareConnection(ownerCtx, request("stalled", "/stall/mcp"))
			if err == nil {
				err = prepared.Activate(ownerCtx)
			}
			prepDone <- err
		}()
		select {
		case <-f.stalled:
		case <-time.After(20 * time.Second):
			t.Fatal("preparation never reached the downstream")
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := rt.attacher.Close(closeCtx); err != nil {
			t.Fatalf("close: %v", err)
		}
		select {
		case err := <-prepDone:
			if err == nil {
				t.Fatal("preparation raced Close and published")
			}
		default:
			t.Fatal("Close returned before the in-flight preparation completed")
		}
		rt.attacher.mu.Lock()
		undrained := len(rt.attacher.closers)
		rt.attacher.mu.Unlock()
		if undrained != 0 {
			t.Fatalf("undrained closers = %d", undrained)
		}
		if _, err := rt.attacher.PrepareConnection(ownerCtx, request("late", "/mcp")); !errors.Is(err, ErrAttacherClosed) {
			t.Fatalf("preparation after Close = %v, want ErrAttacherClosed", err)
		}
	})

	t.Run("activation after close", func(t *testing.T) {
		rt := f.openRuntime(nil)
		prepared, err := rt.attacher.PrepareConnection(ownerCtx, request("prepared", "/mcp"))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		closed := make(chan error, 1)
		go func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			closed <- rt.attacher.Close(closeCtx)
		}()
		deadline := time.Now().Add(20 * time.Second)
		for !rt.attacher.lifecycle.isClosed() {
			if time.Now().After(deadline) {
				t.Fatal("Close never began")
			}
			time.Sleep(time.Millisecond)
		}
		if err := prepared.Activate(ownerCtx); !errors.Is(err, ErrAttacherClosed) {
			t.Fatalf("activation after Close = %v, want ErrAttacherClosed", err)
		}
		if err := <-closed; err != nil {
			t.Fatalf("close: %v", err)
		}
		name := mcpdrv.PhysicalServerName("prepared", owner)
		if _, ok := rt.catalog.Resolve(name + "_echo"); ok {
			t.Fatal("connection published after Close")
		}
		rt.attacher.mu.Lock()
		undrained := len(rt.attacher.closers)
		rt.attacher.mu.Unlock()
		if undrained != 0 {
			t.Fatalf("undrained closers = %d", undrained)
		}
	})
}
