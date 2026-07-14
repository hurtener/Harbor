package integration_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/env"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/remote"
	_ "github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"

	"github.com/hurtener/Harbor/internal/tools"
)

// phase169_oauth_provider_install_test.go drives the Protocol-installed,
// ZERO-URL broker-pull OAuth provider end to end with REAL drivers (statestore
// agent-config registry, inmem state/events, patterns redactor) and REAL
// fixture endpoints: a credential coordinator (the `remote` credential-source
// PULL) + an RFC-8693 token-exchange endpoint (the broker). It proves:
//   - set_oauth_provider installs the provider owner-tagged, and its Token()
//     exchange yields the fixture bearer for the IDENTITY-STAMPED caller (the
//     subject_token encodes the triple) — the whole point, on the wire;
//   - remove_oauth_provider uninstalls + CLOSES the provider, so the bound
//     provider's next call fails LOUD (never an unauthenticated dial);
//   - the run-start provider reconcile is OWNER-SCOPED: a tenant-B run never
//     uninstalls a tenant-A provider (FAIL 6, closed by owner-scoping);
//   - §17.8: the token endpoint enforces the RFC-8693 grant/subject-token-type
//     shape (a wrong grant_type is refused), so a wrong-field mutation FAILS.

const (
	p169KEKEnv       = "HARBOR_TEST_P169_KEK"
	p169BrokerTokEnv = "HARBOR_TEST_P169_BROKER_TOKEN"
	// p169FixtureBearer is the access token the fixture broker mints on a
	// well-formed RFC-8693 exchange. Recognisable so the redaction/round-trip
	// assertions can find (or fail to find) it.
	p169FixtureBearer = "downstream-bearer-p169-abc123"
)

// p169Fixtures stands up the two loopback HTTP endpoints an installed
// broker-pull provider depends on. Provenance (§17.8): the token endpoint's
// request shape is RFC 8693 §2.1 (grant_type + subject_token + subject_token_type)
// and its response is RFC 8693 §2.2.1 (access_token + token_type + issued_token_type);
// the credential endpoint is Harbor's credsource/remote transcript
// (format_version + client_id + client_secret).
type p169Fixtures struct {
	coordinator  *httptest.Server
	tokenBroker  *httptest.Server
	coordHits    atomic.Int64
	exchangeHits atomic.Int64
	// lastGrant records the grant_type the last exchange saw (for the §17.8
	// spec-shape assertion).
	lastGrant atomic.Value
}

func newP169Fixtures(t *testing.T) *p169Fixtures {
	t.Helper()
	f := &p169Fixtures{}
	f.coordinator = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// credsource/remote: an authenticated GET returning the org client
		// credential. Assert the runtime presented its broker service token.
		if got := r.Header.Get("Authorization"); got != "Bearer test-broker-service-token" {
			http.Error(w, "missing service token", http.StatusUnauthorized)
			return
		}
		f.coordHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"format_version": 1,
			"client_id":      "org-client-id",
			"client_secret":  "org-client-secret",
			"expires_in":     3600,
		})
	}))
	t.Cleanup(f.coordinator.Close)

	f.tokenBroker = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		grant := r.Form.Get("grant_type")
		f.lastGrant.Store(grant)
		// §17.8 — enforce the RFC-8693 shape. A wrong grant_type / missing
		// subject_token is refused (the fixture is spec-derived, not a rubber
		// stamp that accepts any request).
		if grant != "urn:ietf:params:oauth:grant-type:token-exchange" {
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		if r.Form.Get("subject_token") == "" || r.Form.Get("subject_token_type") == "" {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_id") != "org-client-id" || r.Form.Get("client_secret") != "org-client-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		f.exchangeHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      p169FixtureBearer,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"token_type":        "Bearer",
			"expires_in":        3600,
			"scope":             "mail.read",
		})
	}))
	t.Cleanup(f.tokenBroker.Close)
	return f
}

// p169Harness wires the REAL install path: a ProviderBuilder over a broker that
// references the fixtures, a ProviderSet, the serve installer, and a real
// agent-config Service over a statestore registry.
type p169Harness struct {
	svc       *agentcfgprotocol.Service
	registry  agentcfg.Registry
	set       toolauth.ProviderSet
	installer *serve.OAuthProviderInstaller
	bus       events.EventBus
}

func newP169Harness(t *testing.T, f *p169Fixtures) *p169Harness {
	t.Helper()
	t.Setenv(p169KEKEnv, hex.EncodeToString(make([]byte, toolauth.KEKSizeBytes)))
	t.Setenv(p169BrokerTokEnv, "test-broker-service-token")

	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })

	toolsCfg := config.ToolsConfig{
		OAuthTokenKEKEnv: p169KEKEnv,
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name:                   "m365-broker",
			TokenURL:               f.tokenBroker.URL,
			CredentialURL:          f.coordinator.URL,
			AllowedDownstreamHosts: []string{"graph.microsoft.com"},
			AuthTokenEnv:           p169BrokerTokEnv,
			Audience:               "https://graph.microsoft.com",
			ScopeCeiling:           []string{"mail.read", "mail.send"},
		}},
	}
	builder, err := toolauth.NewProviderBuilder(context.Background(), toolsCfg, toolauth.BuildDeps{
		State: st, Bus: bus, Redactor: auditpatterns.New(), Coordinator: pauseresume.New(),
	})
	if err != nil {
		t.Fatalf("NewProviderBuilder: %v", err)
	}
	set := toolauth.NewProviderSet(nil)
	installer := serve.NewOAuthProviderInstaller(builder, set)
	if installer == nil {
		t.Fatal("installer is nil")
	}

	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithProviderInstaller(installer),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &p169Harness{svc: svc, registry: reg, set: set, installer: installer, bus: bus}
}

func p169Scope(tenant string) prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: tenant, User: "u", Session: "s"}
}

// TestE2E_Phase169_InstallExchangeRemove drives the full install → identity-
// stamped exchange → uninstall-fails-loud path with real drivers + fixtures.
func TestE2E_Phase169_InstallExchangeRemove(t *testing.T) {
	f := newP169Fixtures(t)
	h := newP169Harness(t, f)
	ctx := context.Background()
	const agentID = "agent-169"

	// 1) Install the ZERO-URL provider.
	resp, err := h.svc.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: p169Scope("tenant-A"), AgentID: agentID,
		Provider: prototypes.AgentConfigOAuthProviderDescriptor{
			Name: "m365", Driver: "tokenexchange", CredentialSource: "remote",
			CredentialBroker: "m365-broker", Scopes: []string{"mail.read"},
		},
	})
	if err != nil {
		t.Fatalf("SetOAuthProvider: %v", err)
	}
	if resp.Name != "m365" {
		t.Fatalf("resp name %q", resp.Name)
	}

	// The owner-tagged set carries it (bare-name resolution).
	prov, ok := h.set.Get("m365")
	if !ok {
		t.Fatal("provider not in the set after install")
	}
	if names := h.set.InstalledFor(toolauth.Owner{Tenant: "tenant-A", Agent: agentID}); len(names) != 1 || names[0] != "m365" {
		t.Fatalf("InstalledFor(A): %v", names)
	}

	// 2) The exchange yields the fixture bearer for the identity-stamped caller.
	idCtx, err := identity.With(ctx, identity.Identity{TenantID: "tenant-A", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	tok, err := prov.Token(idCtx, tools.ToolSourceID("m365"))
	if err != nil {
		t.Fatalf("Token exchange: %v", err)
	}
	if tok.AccessToken != p169FixtureBearer {
		t.Fatalf("exchanged bearer = %q, want the fixture bearer", tok.AccessToken)
	}
	if f.coordHits.Load() == 0 || f.exchangeHits.Load() == 0 {
		t.Fatalf("expected the coordinator PULL + the broker exchange to fire (coord=%d exchange=%d)", f.coordHits.Load(), f.exchangeHits.Load())
	}
	// §17.8: the exchange used the RFC-8693 grant.
	if g, _ := f.lastGrant.Load().(string); g != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Fatalf("exchange grant_type = %q, want the RFC-8693 token-exchange URN", g)
	}

	// 3) Uninstall CLOSES the provider — the bound provider's next call fails
	// LOUD (never an unauthenticated dial).
	if _, err := h.svc.RemoveOAuthProvider(ctx, prototypes.AgentConfigRemoveOAuthProviderRequest{
		Identity: p169Scope("tenant-A"), AgentID: agentID, Name: "m365",
	}); err != nil {
		t.Fatalf("RemoveOAuthProvider: %v", err)
	}
	if _, ok := h.set.Get("m365"); ok {
		t.Fatal("provider still resolvable after uninstall")
	}
	if _, err := prov.Token(idCtx, tools.ToolSourceID("m365")); !errors.Is(err, toolauth.ErrProviderClosed) {
		t.Fatalf("a closed provider's next call must fail loud with ErrProviderClosed, got %v", err)
	}
}

// TestE2E_Phase169_ReconcileTenantBNeverUninstallsTenantAProvider proves the
// owner-scoped reconcile confines the uninstall to the owning owner (FAIL 6).
func TestE2E_Phase169_ReconcileTenantBNeverUninstallsTenantAProvider(t *testing.T) {
	f := newP169Fixtures(t)
	h := newP169Harness(t, f)
	ctx := context.Background()
	const agentA = "agent-A"
	const agentB = "agent-B"

	// Tenant A installs a provider.
	if _, err := h.svc.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: p169Scope("tenant-A"), AgentID: agentA,
		Provider: prototypes.AgentConfigOAuthProviderDescriptor{
			Name: "provider-A", Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "m365-broker",
		},
	}); err != nil {
		t.Fatalf("install A: %v", err)
	}

	// A tenant-B run-start reconcile (B declares no providers) must NEVER touch
	// A's install — the reconcile view is owner-scoped.
	changed, err := projection.ReconcileOAuthProviders(ctx, h.registry, agentB,
		identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-B", UserID: "u", SessionID: "s"}},
		h.installer)
	if err != nil {
		t.Fatalf("reconcile B: %v", err)
	}
	if changed != 0 {
		t.Fatalf("tenant-B reconcile changed %d providers — it must never touch tenant-A's install", changed)
	}
	if _, ok := h.set.Get("provider-A"); !ok {
		t.Fatal("tenant-B reconcile CLOSED tenant-A's provider — cross-tenant outage (FAIL 6)")
	}
	if names := h.set.InstalledFor(toolauth.Owner{Tenant: "tenant-B", Agent: agentB}); len(names) != 0 {
		t.Fatalf("InstalledFor(B) should be empty, got %v", names)
	}
}

// TestE2E_Phase169_ConcurrentInstallUninstall runs N≥100 concurrent
// install/uninstall/get across two tenants against the shared set under -race.
func TestE2E_Phase169_ConcurrentInstallUninstall(t *testing.T) {
	f := newP169Fixtures(t)
	h := newP169Harness(t, f)
	ctx := context.Background()

	var wg sync.WaitGroup
	const workers = 100
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := "tenant-A"
			agentID := "agent-A"
			if i%2 == 1 {
				tenant, agentID = "tenant-B", "agent-B"
			}
			name := "prov-" + tenant
			_, _ = h.svc.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
				Identity: p169Scope(tenant), AgentID: agentID,
				Provider: prototypes.AgentConfigOAuthProviderDescriptor{
					Name: name, Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "m365-broker",
				},
			})
			_, _ = h.set.Get(name)
			_, _ = h.svc.RemoveOAuthProvider(ctx, prototypes.AgentConfigRemoveOAuthProviderRequest{
				Identity: p169Scope(tenant), AgentID: agentID, Name: name,
			})
		}(i)
	}
	wg.Wait()
}

// TestE2E_Phase169_UnknownBrokerFailsLoud proves an install naming an unknown
// broker is refused (nothing installed, a loud client error).
func TestE2E_Phase169_UnknownBrokerFailsLoud(t *testing.T) {
	f := newP169Fixtures(t)
	h := newP169Harness(t, f)
	_, err := h.svc.SetOAuthProvider(context.Background(), prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: p169Scope("tenant-A"), AgentID: "agent-169",
		Provider: prototypes.AgentConfigOAuthProviderDescriptor{
			Name: "x", Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "no-such-broker",
		},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "broker") {
		t.Fatalf("unknown broker must fail loud naming the broker, got %v", err)
	}
	if _, ok := h.set.Get("x"); ok {
		t.Fatal("a failed install must leave nothing in the set")
	}
}
