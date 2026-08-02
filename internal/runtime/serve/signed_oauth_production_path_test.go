package serve

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/remote"
	_ "github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

type productionSignedCapabilityKeySet struct {
	key crypto.PublicKey
}

func (s productionSignedCapabilityKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != "kid" {
		return nil, "", agentcfg.ErrSignedCapabilityAuthority
	}
	return s.key, jwt.SigningMethodRS256.Alg(), nil
}

func TestRegisterOAuthMCPCapability_ProductionPathAuthenticatesInitializeAndDiscovery(t *testing.T) {
	t.Setenv("HARBOR_SIGNED_PRODUCTION_KEK", "0202020202020202020202020202020202020202020202020202020202020202")
	t.Setenv("HARBOR_SIGNED_PRODUCTION_BROKER_AUTH", "fixture-broker-auth")

	const (
		downstreamBearer = "fixture-downstream-bearer"
		providerName     = "signed-provider"
		connectionName   = "signed"
		agentID          = "agent"
	)
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}

	catalog := tools.NewCatalog()
	var mcpRequests atomic.Int64
	var catalogVisibleDuringPrepare atomic.Bool
	var requestMu sync.Mutex
	methods := make(map[string]int)
	authHeaders := make(map[string][]string)

	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "signed-production", Version: "v0"}, nil)
	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	})
	mcpServer.AddResource(&mcpsdk.Resource{URI: "mem://fixture", Name: "fixture", MIMEType: "text/plain"},
		func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			return &mcpsdk.ReadResourceResult{}, nil
		})
	mcpServer.AddPrompt(&mcpsdk.Prompt{Name: "fixture"},
		func(context.Context, *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
			return &mcpsdk.GetPromptResult{}, nil
		})
	mcpHandler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mcpServer }, nil)
	authenticatedMCP := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
		var message struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &message)
		mcpRequests.Add(1)
		requestMu.Lock()
		methods[message.Method]++
		authHeaders[message.Method] = append(authHeaders[message.Method], req.Header.Get("Authorization"))
		requestMu.Unlock()
		if _, ok := catalog.Resolve(connectionName + "_echo"); ok {
			catalogVisibleDuringPrepare.Store(true)
		}
		if req.Header.Get("Authorization") != "Bearer "+downstreamBearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, req)
	})
	tlsMCP := httptest.NewTLSServer(authenticatedMCP)
	t.Cleanup(tlsMCP.Close)
	oldDefaultTransport := http.DefaultTransport
	http.DefaultTransport = tlsMCP.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldDefaultTransport })

	var exchangeMu sync.Mutex
	var exchangeForms []url.Values
	var emptyToken atomic.Bool
	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)
	brokerMux.HandleFunc("/credential", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer fixture-broker-auth" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"format_version": 1, "client_id": "fixture-client", "client_secret": "fixture-secret", "expires_in": 300,
		})
	})
	brokerMux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			http.Error(w, "parse form", http.StatusBadRequest)
			return
		}
		exchangeMu.Lock()
		exchangeForms = append(exchangeForms, maps.Clone(req.Form))
		exchangeMu.Unlock()
		accessToken := downstreamBearer
		if emptyToken.Load() {
			accessToken = ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": accessToken, "token_type": "Bearer", "expires_in": 300,
			"scope": "read", "audience": "capability-audience", "resource": tlsMCP.URL,
		})
	})

	redactor := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, redactor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	registry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{
		OAuthTokenKEKEnv: "HARBOR_SIGNED_PRODUCTION_KEK",
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker", TokenURL: brokerServer.URL + "/token", CredentialURL: brokerServer.URL + "/credential",
			AuthTokenEnv: "HARBOR_SIGNED_PRODUCTION_BROKER_AUTH", ScopeCeiling: []string{"read"},
			AllowedDownstreamHosts: []string{"boot-ceiling.invalid"},
		}},
	}, toolauth.BuildDeps{
		State: store, Bus: bus, Redactor: redactor, Coordinator: pauseresume.New(pauseresume.WithBus(bus)),
	})
	if err != nil {
		t.Fatal(err)
	}
	providerSet := toolauth.NewProviderSet(nil)
	mcpRegistry := mcpdrv.NewRegistry()
	attacher := NewMCPConnectionAttacher(catalog, mcpRegistry, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)), id, nil, providerSet, nil)
	t.Cleanup(func() { _ = attacher.Close(context.Background()) })
	installer := NewOAuthProviderInstaller(builder, providerSet, false, nil)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	canonicalURL, sink, err := agentcfg.CanonicalOAuthMCPURL(tlsMCP.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	connection := agentcfg.SignedOAuthMCPConnectionDescriptor{Name: connectionName, URL: canonicalURL}
	claims := agentcfg.SignedOAuthMCPAuthorityClaims{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, AgentID: agentID,
		Broker: "broker", ProviderName: providerName, CapabilityRevision: "capability-revision-7",
		URLDigest: agentcfg.OAuthMCPURLDigest(canonicalURL), SinkDigest: agentcfg.OAuthMCPURLDigest(sink),
		Audience: "capability-audience", Scopes: []string{"read"}, Connection: connection,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "issuer", ID: "jti-production", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "kid"
	envelope, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	req := prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		AgentID:  agentID, ProviderName: providerName, Broker: "broker", Audience: claims.Audience,
		Scopes: []string{"read"}, Connection: prototypes.SignedOAuthMCPConnectionDescriptor{Name: connectionName, URL: canonicalURL},
		AuthorityEnvelope: envelope,
	}
	service, err := agentcfgprotocol.NewService(registry,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionPreparer(attacher),
		agentcfgprotocol.WithConnectionDetacher(attacher),
		agentcfgprotocol.WithProviderInstaller(installer),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(store),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {
				Broker: "broker", Issuer: "issuer", Keys: productionSignedCapabilityKeySet{key: &key.PublicKey},
				ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: 10 * time.Minute,
			},
		}),
		agentcfgprotocol.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	verifiedCtx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	wrongIdentity := req
	wrongIdentity.Identity.User = "other-user"
	wrongIdentityClaims := claims
	wrongIdentityClaims.UserID = "other-user"
	wrongIdentityClaims.ID = "jti-wrong-identity"
	wrongIdentityToken := jwt.NewWithClaims(jwt.SigningMethodRS256, wrongIdentityClaims)
	wrongIdentityToken.Header["kid"] = "kid"
	wrongIdentity.AuthorityEnvelope, err = wrongIdentityToken.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterOAuthMCPCapability(verifiedCtx, wrongIdentity); err == nil {
		t.Fatal("signed identity different from the verified caller unexpectedly prepared MCP")
	}
	wrongAgent := req
	wrongAgent.AgentID = "other-agent"
	if _, err := service.RegisterOAuthMCPCapability(verifiedCtx, wrongAgent); err == nil {
		t.Fatal("wrong agent unexpectedly passed signed binding verification")
	}
	if got := mcpRequests.Load(); got != 0 {
		t.Fatalf("wrong identity/agent reached MCP: requests=%d", got)
	}
	exchangeMu.Lock()
	exchangesBeforeEmptyToken := len(exchangeForms)
	exchangeMu.Unlock()
	if exchangesBeforeEmptyToken != 0 {
		t.Fatalf("wrong identity/agent reached credential exchange: exchanges=%d", exchangesBeforeEmptyToken)
	}

	emptyToken.Store(true)
	if _, err := service.RegisterOAuthMCPCapability(verifiedCtx, req); err == nil {
		t.Fatal("empty downstream token unexpectedly prepared an MCP connection")
	}
	if got := mcpRequests.Load(); got != 0 {
		t.Fatalf("empty token reached MCP: requests=%d", got)
	}
	if _, ok := catalog.Resolve(connectionName + "_echo"); ok {
		t.Fatal("catalog published after failed private preparation")
	}

	emptyToken.Store(false)
	response, err := service.RegisterOAuthMCPCapability(verifiedCtx, req)
	if err != nil {
		t.Fatalf("production registration: %v", err)
	}
	if response.ProviderName != providerName || response.ConnectionName != connectionName {
		t.Fatalf("registration response = %+v", response)
	}
	if catalogVisibleDuringPrepare.Load() {
		t.Fatal("catalog became visible before authenticated preparation completed")
	}
	if _, ok := catalog.Resolve(connectionName + "_echo"); !ok {
		t.Fatal("catalog did not publish after authenticated preparation succeeded")
	}
	if _, ok := providerSet.Get(providerName); ok {
		t.Fatal("signed pair-private provider leaked into the shared ProviderSet")
	}

	requestMu.Lock()
	for _, method := range []string{"initialize", "tools/list", "resources/list", "prompts/list"} {
		if methods[method] == 0 {
			requestMu.Unlock()
			t.Fatalf("authenticated production preparation omitted %s; methods=%v", method, methods)
		}
		for _, header := range authHeaders[method] {
			if header != "Bearer "+downstreamBearer {
				requestMu.Unlock()
				t.Fatalf("%s authorization = %q, want exact downstream bearer", method, header)
			}
		}
	}
	requestMu.Unlock()

	exchangeMu.Lock()
	forms := append([]url.Values(nil), exchangeForms...)
	exchangeMu.Unlock()
	if len(forms) != 2 {
		t.Fatalf("token exchanges = %d, want failed-empty attempt plus successful retry", len(forms))
	}
	form := forms[len(forms)-1]
	if form.Get("audience") != claims.Audience || form.Get("resource") != sink {
		t.Fatalf("exchange destination = audience %q resource %q, want %q / %q", form.Get("audience"), form.Get("resource"), claims.Audience, sink)
	}
	if form.Get("client_id") != "fixture-client" || form.Get("client_secret") != "fixture-secret" {
		t.Fatal("exchange omitted the boot-pulled organization credential")
	}
	var subject struct {
		TenantID  string `json:"tenant_id"`
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
	}
	decodeExchangeField(t, form.Get("subject_token"), &subject)
	if subject.TenantID != id.TenantID || subject.UserID != id.UserID || subject.SessionID != id.SessionID {
		t.Fatalf("subject token lost verified identity: %+v", subject)
	}
	var actor toolauth.SignedCapabilityExchangeBinding
	decodeExchangeField(t, form.Get("actor_token"), &actor)
	expectedBinding := agentcfg.SignedOAuthMCPBinding{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, AgentID: agentID,
		Broker: "broker", ProviderName: providerName, CapabilityRevision: claims.CapabilityRevision,
		URLDigest: claims.URLDigest, SinkDigest: claims.SinkDigest, Audience: claims.Audience,
		Scopes: []string{"read"}, Connection: connection,
	}
	if actor.TenantID != id.TenantID || actor.UserID != id.UserID || actor.SessionID != id.SessionID ||
		actor.AgentID != agentID || actor.ProviderName != providerName || actor.CapabilityRevision != claims.CapabilityRevision ||
		actor.URLDigest != claims.URLDigest || actor.SinkDigest != claims.SinkDigest || actor.Audience != claims.Audience ||
		actor.Resource != sink || actor.PairFingerprint != agentcfg.SignedOAuthMCPPairFingerprint(expectedBinding) {
		t.Fatalf("actor binding lost exact signed authority: %+v", actor)
	}
}

func decodeExchangeField(t *testing.T, encoded string, out any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode exchange field: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal exchange field: %v", err)
	}
}
