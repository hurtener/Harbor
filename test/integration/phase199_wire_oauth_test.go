package integration_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/env"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/remote"
	_ "github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
)

// phase199_wire_oauth_test.go drives the DEV-GATED wire-carried OAuth-provider
// descriptor (HA-32 / D-340) end to end with REAL drivers + REAL fixtures (the
// same §17.8 spec-derived RFC-8693 coordinator + token-broker transcript
// phase-169 uses). It proves, with the opt-in ON:
//   - set_oauth_provider carrying a WIRE binding (token_url + remote{}) installs a
//     provider whose Token() exchange dials the WIRE token_url and yields the
//     fixture bearer for the identity-stamped caller;
//   - an add_mcp_connection inline wire binding DERIVES allowed_downstream_hosts
//     from the connection's own URL (never a wire field);
//   - a wire token_url resolving to a private address is REFUSED by the SSRF
//     backstop (the opt-in does NOT relax the private-dial refusal — D-338 is
//     independent);
// and with the opt-in OFF the wire descriptor is rejected (the D-303 posture).

const (
	p199KEKEnv       = "HARBOR_TEST_P199_KEK"
	p199CoordTokEnv  = "HARBOR_TEST_P199_COORD_TOKEN"
	p199FixtureToken = "downstream-bearer-p199-xyz789"
)

type p199Fixtures struct {
	coordinator  *httptest.Server
	tokenBroker  *httptest.Server
	coordHits    atomic.Int64
	exchangeHits atomic.Int64
}

func newP199Fixtures(t *testing.T) *p199Fixtures {
	t.Helper()
	f := &p199Fixtures{}
	// credsource/remote: an authenticated GET returning the org client credential.
	f.coordinator = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-coord-service-token" {
			http.Error(w, "missing service token", http.StatusUnauthorized)
			return
		}
		f.coordHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"format_version": 1, "client_id": "org-client-id", "client_secret": "org-client-secret", "expires_in": 3600,
		})
	}))
	t.Cleanup(f.coordinator.Close)
	// RFC 8693 token-exchange endpoint (spec shape — a wrong grant is refused).
	f.tokenBroker = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" ||
			r.Form.Get("subject_token") == "" || r.Form.Get("subject_token_type") == "" {
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
			"access_token": p199FixtureToken, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"token_type": "Bearer", "expires_in": 3600, "scope": "mail.read",
		})
	}))
	t.Cleanup(f.tokenBroker.Close)
	return f
}

// p199OnlineAttacher is a ConnectionAttacher that always reports online (the
// wire-binding derive under test happens in the agent-config handler, before the
// attach).
type p199OnlineAttacher struct{}

func (p199OnlineAttacher) Attach(context.Context, agentcfgprotocol.AttachRequest) error { return nil }

type p199Harness struct {
	svc *agentcfgprotocol.Service
	set toolauth.ProviderSet
	bus events.EventBus
}

func newP199Harness(t *testing.T, allowWire bool) *p199Harness {
	t.Helper()
	t.Setenv(p199KEKEnv, hex.EncodeToString(make([]byte, toolauth.KEKSizeBytes)))
	t.Setenv(p199CoordTokEnv, "test-coord-service-token")

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

	// One dummy broker seeds the shared crypto chain (KEK → sealer → token store)
	// a wire provider shares; a wire descriptor never resolves it.
	toolsCfg := config.ToolsConfig{
		OAuthTokenKEKEnv: p199KEKEnv,
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "seed", TokenURL: "https://broker/token", CredentialURL: "https://c/x",
			AllowedDownstreamHosts: []string{"x"}, AuthTokenEnv: p199CoordTokEnv,
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

	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithProviderInstaller(installer),
		agentcfgprotocol.WithConnectionAttacher(p199OnlineAttacher{}),
		agentcfgprotocol.WithCoordinator(pauseresume.New()),
		agentcfgprotocol.WithAllowWireOAuthDescriptor(allowWire),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &p199Harness{svc: svc, set: set, bus: bus}
}

func p199Scope() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: "tenant-A", User: "u", Session: "s"}
}

func wireDesc(name, tokenURL, coordURL string) prototypes.AgentConfigOAuthProviderDescriptor {
	return prototypes.AgentConfigOAuthProviderDescriptor{
		Name: name, Driver: "tokenexchange", CredentialSource: "remote",
		TokenURL: tokenURL, Audience: "https://graph.microsoft.com", Scopes: []string{"mail.read"},
		Remote: &prototypes.AgentConfigOAuthRemoteDescriptor{URL: coordURL, AuthTokenEnv: p199CoordTokEnv},
	}
}

// TestE2E_Phase199_WireInstallExchange — opt-in ON: set_oauth_provider installs a
// WIRE provider whose Token() exchange dials the WIRE token_url.
func TestE2E_Phase199_WireInstallExchange(t *testing.T) {
	f := newP199Fixtures(t)
	h := newP199Harness(t, true)
	ctx := context.Background()
	const agentID = "agent-199"

	if _, err := h.svc.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: p199Scope(), AgentID: agentID,
		Provider: wireDesc("wp", f.tokenBroker.URL, f.coordinator.URL),
	}); err != nil {
		t.Fatalf("SetOAuthProvider (wire): %v", err)
	}
	prov, ok := h.set.Get("wp")
	if !ok {
		t.Fatal("wire provider not installed")
	}
	idCtx, err := identity.With(ctx, identity.Identity{TenantID: "tenant-A", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	tok, err := prov.Token(idCtx, tools.ToolSourceID("wp"))
	if err != nil {
		t.Fatalf("wire Token exchange: %v", err)
	}
	if tok.AccessToken != p199FixtureToken {
		t.Fatalf("exchanged bearer = %q, want the fixture bearer", tok.AccessToken)
	}
	if f.coordHits.Load() == 0 || f.exchangeHits.Load() == 0 {
		t.Fatalf("expected the WIRE coordinator PULL + WIRE broker exchange to fire (coord=%d exchange=%d)", f.coordHits.Load(), f.exchangeHits.Load())
	}
}

// TestE2E_Phase199_InlineDerivesDownstreamHost — opt-in ON: an add_mcp_connection
// inline wire binding DERIVES allowed_downstream_hosts from the connection URL.
func TestE2E_Phase199_InlineDerivesDownstreamHost(t *testing.T) {
	f := newP199Fixtures(t)
	h := newP199Harness(t, true)
	ctx := context.Background()

	resp, err := h.svc.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: p199Scope(), AgentID: "agent-199",
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{
			Name: "srv", Transport: "http", URL: "https://graph.microsoft.com:8443/mcp",
			OAuth: func() *prototypes.AgentConfigOAuthProviderDescriptor {
				d := wireDesc("srv-oauth", f.tokenBroker.URL, f.coordinator.URL)
				return &d
			}(),
		},
	})
	if err != nil {
		t.Fatalf("AddMCPConnection (inline wire): %v", err)
	}
	if resp.State != "online" {
		t.Fatalf("want online, got %q (%s)", resp.State, resp.Reason)
	}
	prov, ok := h.set.Get("srv-oauth")
	if !ok {
		t.Fatal("inline wire provider not installed")
	}
	want := config.NormalizeDownstreamHost("https://graph.microsoft.com:8443/mcp")
	got := prov.AllowedDownstreamHosts()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("allowed_downstream_hosts must be DERIVED as %q from the connection url, got %v", want, got)
	}
}

// TestE2E_Phase199_PrivateTokenURLRefused — opt-in ON does NOT relax the SSRF
// backstop: a wire token_url resolving to a private address is refused at exchange.
func TestE2E_Phase199_PrivateTokenURLRefused(t *testing.T) {
	f := newP199Fixtures(t)
	h := newP199Harness(t, true)
	ctx := context.Background()

	if _, err := h.svc.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: p199Scope(), AgentID: "agent-199",
		Provider: wireDesc("wp-priv", "http://10.255.255.1:8443/token", f.coordinator.URL),
	}); err != nil {
		t.Fatalf("SetOAuthProvider install (build does not dial): %v", err)
	}
	prov, ok := h.set.Get("wp-priv")
	if !ok {
		t.Fatal("provider not installed")
	}
	idCtx, err := identity.With(ctx, identity.Identity{TenantID: "tenant-A", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := prov.Token(idCtx, tools.ToolSourceID("wp-priv")); !errors.Is(err, toolauth.ErrExchangeFailed) {
		t.Fatalf("a private wire token_url must be refused by the SSRF backstop (ErrExchangeFailed), got %v", err)
	}
}

// TestE2E_Phase199_OptInOffPreservesD303 — the default posture: a wire descriptor
// is rejected when the opt-in is off (the zero-URL name-only binding stands).
func TestE2E_Phase199_OptInOffPreservesD303(t *testing.T) {
	f := newP199Fixtures(t)
	h := newP199Harness(t, false)
	ctx := context.Background()

	_, err := h.svc.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: p199Scope(), AgentID: "agent-199",
		Provider: wireDesc("wp", f.tokenBroker.URL, f.coordinator.URL),
	})
	if !errors.Is(err, agentcfgprotocol.ErrWireDescriptorNotAllowed) {
		t.Fatalf("with the opt-in off a wire descriptor must be rejected, got %v", err)
	}
	if _, ok := h.set.Get("wp"); ok {
		t.Fatal("no provider should be installed with the opt-in off")
	}
}
