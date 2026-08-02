package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/remote"
	_ "github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

type signedCapabilityProviderAdapter struct{ provider toolauth.OAuthProvider }

func (*signedCapabilityProviderAdapter) SourceID() tools.ToolSourceID { return "provider" }
func (*signedCapabilityProviderAdapter) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return nil, nil
}
func (*signedCapabilityProviderAdapter) DisplayModes() []string { return nil }
func (*signedCapabilityProviderAdapter) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", nil
}
func (p *signedCapabilityProviderAdapter) Close(ctx context.Context) error {
	return p.provider.Close(ctx)
}

func TestBuildSignedCapability_ProductionBuilder_BindsExchangeAndCloseKillsCache(t *testing.T) {
	t.Setenv("HARBOR_SIGNED_BUILDER_KEK", "0101010101010101010101010101010101010101010101010101010101010101")
	t.Setenv("HARBOR_SIGNED_BUILDER_AUTH", "fixture-broker-auth")

	var mu sync.Mutex
	var exchanges int
	var recorded url.Values
	responseAudience := "capability-audience"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/credential", func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("Authorization"); got != "Bearer fixture-broker-auth" {
			t.Errorf("credential authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"format_version": 1, "client_id": "fixture-client", "client_secret": "fixture-secret", "expires_in": 300,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			t.Errorf("parse exchange form: %v", err)
		}
		mu.Lock()
		exchanges++
		recorded = maps.Clone(req.Form)
		audience := responseAudience
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fixture-downstream-token", "token_type": "Bearer", "expires_in": 300,
			"scope": "read", "audience": audience, "resource": "https://mcp.example.test:8443",
		})
	})

	redactor := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32, IdleTimeout: time.Minute, DropWindow: time.Second}, redactor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{
		OAuthTokenKEKEnv: "HARBOR_SIGNED_BUILDER_KEK",
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker", TokenURL: server.URL + "/token", CredentialURL: server.URL + "/credential",
			AuthTokenEnv: "HARBOR_SIGNED_BUILDER_AUTH", Audience: "boot-ceiling", ScopeCeiling: []string{"read"},
			AllowedDownstreamHosts: []string{"boot.example.test"},
		}},
	}, toolauth.BuildDeps{State: store, Bus: bus, Redactor: redactor, Coordinator: pauseresume.New(pauseresume.WithBus(bus))})
	if err != nil {
		t.Fatal(err)
	}
	canonicalURL, sink, err := agentcfg.CanonicalOAuthMCPURL("https://mcp.example.test:8443/mcp")
	if err != nil {
		t.Fatal(err)
	}
	binding := toolauth.SignedCapabilityExchangeBinding{
		TenantID: "tenant", UserID: "user", SessionID: "session", AgentID: "agent", ProviderName: "provider",
		CapabilityRevision: "cap-rev-7", PairFingerprint: "pair-fingerprint", URLDigest: agentcfg.OAuthMCPURLDigest(canonicalURL),
		SinkDigest: agentcfg.OAuthMCPURLDigest(sink), Audience: "capability-audience", Resource: sink,
	}
	provider, err := builder.BuildSignedCapability(context.Background(), "broker", binding, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	ctx = tools.WithInvokingAgent(ctx, "agent")
	first, err := provider.Token(ctx, tools.ToolSourceID("provider"))
	if err != nil || first.AccessToken != "fixture-downstream-token" {
		t.Fatalf("first token: %+v err=%v", first, err)
	}
	if _, err := provider.Token(ctx, tools.ToolSourceID("provider")); err != nil {
		t.Fatalf("cache hit: %v", err)
	}
	mu.Lock()
	gotExchanges, form := exchanges, maps.Clone(recorded)
	mu.Unlock()
	if gotExchanges != 1 {
		t.Fatalf("exchange count = %d, want one plus a cache hit", gotExchanges)
	}
	if form.Get("audience") != binding.Audience || form.Get("resource") != binding.Resource {
		t.Fatalf("destination form = audience %q resource %q", form.Get("audience"), form.Get("resource"))
	}
	if form.Get("client_id") != "fixture-client" || form.Get("client_secret") != "fixture-secret" {
		t.Fatalf("broker credential was not supplied by the boot-pinned source")
	}
	decode := func(field string, out any) {
		t.Helper()
		raw, decErr := base64.RawURLEncoding.DecodeString(form.Get(field))
		if decErr != nil {
			t.Fatalf("decode %s: %v", field, decErr)
		}
		if decErr := json.Unmarshal(raw, out); decErr != nil {
			t.Fatalf("unmarshal %s: %v", field, decErr)
		}
	}
	var subject struct {
		TenantID  string `json:"tenant_id"`
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
	}
	decode("subject_token", &subject)
	if subject.TenantID != binding.TenantID || subject.UserID != binding.UserID || subject.SessionID != binding.SessionID {
		t.Fatalf("subject token = %+v", subject)
	}
	var actor toolauth.SignedCapabilityExchangeBinding
	decode("actor_token", &actor)
	if actor != binding {
		t.Fatalf("actor binding = %+v, want %+v", actor, binding)
	}
	if actor.AgentID != "agent" || actor.CapabilityRevision != "cap-rev-7" || actor.URLDigest != agentcfg.OAuthMCPURLDigest(canonicalURL) {
		t.Fatalf("actor omitted exact agent/revision/normalized URL digest: %+v", actor)
	}
	registry := mcpdrv.NewRegistry()
	owner := toolauth.Owner{Tenant: "tenant", Agent: "agent"}
	if err := registry.Register(ctx, mcpdrv.ServerRegistration{
		Provider: &signedCapabilityProviderAdapter{provider: provider}, Transport: "streamable-http",
		Owner: owner, DescriptorFingerprint: "descriptor-fingerprint",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DeregisterExact(context.Background(), "provider", owner, "descriptor-fingerprint", func() int { return 0 }); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Token(ctx, tools.ToolSourceID("provider")); !errors.Is(err, toolauth.ErrProviderClosed) {
		t.Fatalf("exact MCP teardown left private cached credential usable: %v", err)
	}
	mu.Lock()
	responseAudience = "wrong-audience"
	mu.Unlock()
	mismatchBinding := binding
	mismatchBinding.ProviderName = "provider-mismatch"
	mismatchBinding.PairFingerprint = "pair-fingerprint-mismatch"
	mismatch, err := builder.BuildSignedCapability(context.Background(), "broker", mismatchBinding, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mismatch.Close(context.Background()) })
	if _, err := mismatch.Token(ctx, tools.ToolSourceID("provider-mismatch")); !errors.Is(err, toolauth.ErrExchangeFailed) {
		t.Fatalf("mismatched returned audience was accepted: %v", err)
	}
}
