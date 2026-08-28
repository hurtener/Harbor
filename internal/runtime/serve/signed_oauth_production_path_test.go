package serve

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	protocolmethods "github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
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
		func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{
				{URI: req.Params.URI, MIMEType: "text/plain", Text: "signed-resource"},
			}}, nil
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
	var credentialRequests atomic.Int64
	var emptyToken atomic.Bool
	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)
	brokerMux.HandleFunc("/credential", func(w http.ResponseWriter, req *http.Request) {
		credentialRequests.Add(1)
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
	dsn := filepath.Join(t.TempDir(), "signed-publisher-runtime.sqlite")
	store, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	registry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	providerBuilderConfig := config.ToolsConfig{
		OAuthTokenKEKEnv: "HARBOR_SIGNED_PRODUCTION_KEK",
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker", TokenURL: brokerServer.URL + "/token", CredentialURL: brokerServer.URL + "/credential",
			AuthTokenEnv: "HARBOR_SIGNED_PRODUCTION_BROKER_AUTH", ScopeCeiling: []string{"read"},
			AllowedDownstreamHosts: []string{"boot-ceiling.invalid"},
		}},
	}
	builder, err := toolauth.NewProviderBuilder(context.Background(), providerBuilderConfig, toolauth.BuildDeps{
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
	authorities := map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
		"broker": {
			Broker: "broker", Issuer: "issuer", Keys: productionSignedCapabilityKeySet{key: &key.PublicKey},
			ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: 10 * time.Minute,
		},
	}
	service, err := agentcfgprotocol.NewService(registry,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionPreparer(attacher),
		agentcfgprotocol.WithConnectionDetacher(attacher),
		agentcfgprotocol.WithProviderInstaller(installer),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(store),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(authorities),
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
	operationStore, err := agentcfg.NewSignedOAuthMCPOperationStore(store)
	if err != nil {
		t.Fatal(err)
	}
	operations, _, err := operationStore.ScanTenantPage(context.Background(), id.TenantID, 16, "")
	if err != nil {
		t.Fatalf("scan rejected requests: %v", err)
	}
	if len(operations) != 0 {
		t.Fatalf("rejected identity/agent persisted operations: %+v", operations)
	}
	if revisions, listErr := registry.ListRevisions(context.Background(), identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent, 16); listErr != nil || len(revisions) != 0 {
		t.Fatalf("rejected identity/agent mutated revision history: revisions=%+v err=%v", revisions, listErr)
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, fenceErr := fences.Load(context.Background(), id.TenantID, agentID); fenceErr == nil {
		t.Fatal("rejected identity/agent persisted an activation fence")
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
	} else {
		t.Logf("failed preparation: %v", err)
	}
	if got := mcpRequests.Load(); got != 0 {
		t.Fatalf("empty token reached MCP: requests=%d", got)
	}
	if _, ok := catalog.Resolve(connectionName + "_echo"); ok {
		t.Fatal("catalog published after failed private preparation")
	}
	failedPreparation, err := operationStore.Load(context.Background(), agentcfg.SignedOAuthMCPReplayKey{
		TenantID: id.TenantID, TrustAnchorName: "broker", Issuer: claims.Issuer,
		KeyID: "kid", JTI: claims.ID,
	})
	if err != nil {
		t.Fatalf("load failed preparation operation: %v", err)
	}
	if failedPreparation.Phase != agentcfg.SignedOAuthMCPPhaseRevisionCommitted || failedPreparation.PublisherEpoch == "" {
		t.Fatalf("failed preparation receipt = phase %q epoch %q, want revision_committed with publisher epoch", failedPreparation.Phase, failedPreparation.PublisherEpoch)
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

	dispatchCtx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	dispatchCtx = protocolauth.WithAgentReach(dispatchCtx, []string{agentID})
	runtimeATool, ok := catalog.Resolve(connectionName + "_echo")
	if !ok {
		t.Fatal("runtime A tool is not published")
	}
	artifactStore, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = artifactStore.Close(context.Background()) })
	toolContext, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{
		State: store, Store: artifactStore, Bus: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	appsAccessor, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry: mcpRegistry, Catalog: catalog, Store: artifactStore, Bus: bus,
		ToolContext: toolContext, AgentConfig: registry, AgentID: agentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewAgentResolverAdapter(registry, agentID)
	reach := protocolauth.NewAgentReachAuthorizer()
	mcpRegAccessor, err := mcpconsole.NewRegistryAccessor(mcpRegistry,
		mcpconsole.WithSourceAuthorizer(mcpconsole.NewSourceAuthorizer(mcpRegistry, registry, reach)))
	if err != nil {
		t.Fatal(err)
	}
	mcpSurface, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP: mcpRegAccessor, OAuth: mcpconsole.NewNoOAuthAccessor(), Redactor: redactor, Bus: bus,
		AgentResolver: resolver, AgentReach: reach,
	})
	if err != nil {
		t.Fatal(err)
	}
	appsSurface, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: appsAccessor, Invoker: appsAccessor, ToolContext: appsAccessor,
		AgentResolver: resolver,
		AgentReach:    reach,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestMu.Lock()
	resourcesListBefore := methods["resources/list"]
	requestMu.Unlock()
	listResp, err := mcpSurface.Dispatch(dispatchCtx, protocolmethods.MethodMCPServersResources, &prototypes.MCPServerResourcesRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		AgentID:  agentID, Name: connectionName,
	})
	if err != nil {
		t.Fatalf("MCP signed resources dispatch: %v", err)
	}
	resources := listResp.(*prototypes.MCPServerResourcesResponse).Resources
	if len(resources) != 1 || resources[0].URI != "mem://fixture" {
		t.Fatalf("MCP signed resources = %+v, want mem://fixture", resources)
	}
	requestMu.Lock()
	resourcesListAfter := methods["resources/list"]
	resourcesListAuth := append([]string(nil), authHeaders["resources/list"]...)
	requestMu.Unlock()
	if len(resourcesListAuth) == 0 {
		t.Fatal("signed resources/list made no authenticated downstream request")
	}
	latestResourcesAuth := resourcesListAuth[len(resourcesListAuth)-1]
	if resourcesListAfter != resourcesListBefore+1 || latestResourcesAuth != "Bearer "+downstreamBearer {
		t.Fatalf("signed resources/list = calls %d->%d auth %q", resourcesListBefore, resourcesListAfter, latestResourcesAuth)
	}
	_, err = mcpSurface.Dispatch(dispatchCtx, protocolmethods.MethodMCPServersResources, &prototypes.MCPServerResourcesRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		AgentID:  "other-agent", Name: connectionName,
	})
	var denied *protoerrors.Error
	if !stderrors.As(err, &denied) || denied.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("cross-agent resources denial = %v, want scope_mismatch", err)
	}
	requestMu.Lock()
	resourcesListAfterDenial := methods["resources/list"]
	requestMu.Unlock()
	if resourcesListAfterDenial != resourcesListAfter {
		t.Fatalf("cross-agent denial reached resources/list: calls %d->%d", resourcesListAfter, resourcesListAfterDenial)
	}
	readResp, err := appsSurface.Dispatch(dispatchCtx, protocolmethods.MethodMCPReadResource, &prototypes.ReadMCPResourceRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		AgentID:  agentID, ServerID: connectionName, ResourceURI: "mem://fixture",
	})
	if err != nil {
		t.Fatalf("Apps signed resource dispatch: %v", err)
	}
	if got := readResp.(*prototypes.ReadMCPResourceResponse).Content; got != "signed-resource" {
		t.Fatalf("Apps signed resource content = %q, want signed-resource", got)
	}
	if _, err := appsSurface.Dispatch(dispatchCtx, protocolmethods.MethodMCPAppsCallTool, &prototypes.MCPAppCallToolRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		AgentID:  agentID, Tool: connectionName + "_echo", Arguments: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Apps signed tool dispatch: %v", err)
	}
	requestMu.Lock()
	for _, method := range []string{"resources/read", "tools/call"} {
		if methods[method] == 0 {
			requestMu.Unlock()
			t.Fatalf("Apps signed dispatch omitted %s; methods=%v", method, methods)
		}
		for _, header := range authHeaders[method] {
			if header != "Bearer "+downstreamBearer {
				requestMu.Unlock()
				t.Fatalf("Apps %s authorization = %q, want exact downstream bearer", method, header)
			}
		}
	}
	requestMu.Unlock()
	// The remaining half of this fixture invokes saved descriptors directly to
	// prove stale publisher epochs become inert. Model the run loop for those
	// direct calls; the Apps calls above deliberately received no pre-stamped
	// effective-agent capability and therefore prove the Protocol surface adds
	// it only after reach + tenant resolution.
	dispatchCtx = tools.WithEffectiveAgentConfig(dispatchCtx, agentID)
	operationKey := agentcfg.SignedOAuthMCPReplayKey{
		TenantID: id.TenantID, TrustAnchorName: "broker", Issuer: claims.Issuer,
		KeyID: "kid", JTI: claims.ID,
	}
	runtimeAOperation, err := operationStore.Load(context.Background(), operationKey)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeAOperation.Phase != agentcfg.SignedOAuthMCPPhasePublished || runtimeAOperation.PublisherEpoch == "" {
		t.Fatalf("runtime A operation = phase %q epoch %q", runtimeAOperation.Phase, runtimeAOperation.PublisherEpoch)
	}

	// Runtime B opens an independent SQLite handle and local provider/catalog
	// graph. Its reconciliation CAS-takes the durable publisher epoch; the
	// already-cached runtime-A token and descriptor must become inert without a
	// broker call or downstream request.
	secondStore, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondStore.Close(context.Background()) })
	secondRegistry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: secondStore, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondRegistry.Close(context.Background()) })
	secondBuilder, err := toolauth.NewProviderBuilder(context.Background(), providerBuilderConfig, toolauth.BuildDeps{
		State: secondStore, Bus: bus, Redactor: redactor, Coordinator: pauseresume.New(pauseresume.WithBus(bus)),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondCatalog := tools.NewCatalog()
	secondProviderSet := toolauth.NewProviderSet(nil)
	secondMCPRegistry := mcpdrv.NewRegistry()
	secondAttacher := NewMCPConnectionAttacher(secondCatalog, secondMCPRegistry, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)), id, nil, secondProviderSet, nil)
	t.Cleanup(func() { _ = secondAttacher.Close(context.Background()) })
	secondInstaller := NewOAuthProviderInstaller(secondBuilder, secondProviderSet, false, nil)
	secondReconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(secondRegistry, secondStore, secondAttacher, secondAttacher, secondInstaller)
	if err != nil {
		t.Fatal(err)
	}
	quad := identity.Quadruple{Identity: id}
	if err := secondReconciler.ReconcileSignedOAuthMCPCapability(dispatchCtx, quad, agentID); err != nil {
		t.Fatalf("runtime B reconcile/takeover: %v", err)
	}
	runtimeBOperation, err := operationStore.Load(context.Background(), operationKey)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeBOperation.PublisherEpoch == "" || runtimeBOperation.PublisherEpoch == runtimeAOperation.PublisherEpoch {
		t.Fatalf("runtime B did not take over publisher epoch: A=%q B=%q", runtimeAOperation.PublisherEpoch, runtimeBOperation.PublisherEpoch)
	}
	runtimeBTool, ok := secondCatalog.Resolve(connectionName + "_echo")
	if !ok {
		t.Fatal("runtime B tool is not published after reconcile")
	}
	if _, err := runtimeBTool.Invoke(dispatchCtx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("runtime B dispatch: %v", err)
	}
	exchangeMu.Lock()
	beforeStaleExchange := len(exchangeForms)
	exchangeMu.Unlock()
	beforeStaleCredential := credentialRequests.Load()
	beforeStaleDownstream := mcpRequests.Load()
	if _, err := runtimeATool.Invoke(dispatchCtx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("runtime A cached descriptor remained usable after runtime B publisher takeover")
	}
	exchangeMu.Lock()
	afterStaleExchange := len(exchangeForms)
	exchangeMu.Unlock()
	if afterStaleExchange != beforeStaleExchange || credentialRequests.Load() != beforeStaleCredential || mcpRequests.Load() != beforeStaleDownstream {
		t.Fatalf("stale runtime A reached credential/downstream planes: exchange %d->%d credential %d->%d downstream %d->%d",
			beforeStaleExchange, afterStaleExchange, beforeStaleCredential, credentialRequests.Load(), beforeStaleDownstream, mcpRequests.Load())
	}

	// Runtime C has no local provider or MCP handle. Durable removal admission
	// still fences runtime B immediately; teardown succeeds from the empty local
	// graph, and both cached descriptors stay network-inert until their owning
	// runtimes reconcile local cleanup.
	thirdStore, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thirdStore.Close(context.Background()) })
	thirdRegistry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: thirdStore, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thirdRegistry.Close(context.Background()) })
	thirdBuilder, err := toolauth.NewProviderBuilder(context.Background(), providerBuilderConfig, toolauth.BuildDeps{
		State: thirdStore, Bus: bus, Redactor: redactor, Coordinator: pauseresume.New(pauseresume.WithBus(bus)),
	})
	if err != nil {
		t.Fatal(err)
	}
	thirdCatalog := tools.NewCatalog()
	thirdProviderSet := toolauth.NewProviderSet(nil)
	thirdMCPRegistry := mcpdrv.NewRegistry()
	thirdAttacher := NewMCPConnectionAttacher(thirdCatalog, thirdMCPRegistry, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)), id, nil, thirdProviderSet, nil)
	t.Cleanup(func() { _ = thirdAttacher.Close(context.Background()) })
	thirdInstaller := NewOAuthProviderInstaller(thirdBuilder, thirdProviderSet, false, nil)
	thirdService, err := agentcfgprotocol.NewService(thirdRegistry,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionPreparer(thirdAttacher),
		agentcfgprotocol.WithConnectionDetacher(thirdAttacher),
		agentcfgprotocol.WithProviderInstaller(thirdInstaller),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(thirdStore),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(authorities),
		agentcfgprotocol.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := thirdService.RemoveOAuthMCPCapability(verifiedCtx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: req.Identity, AgentID: agentID, ExpectedContentHash: response.Revision.ContentHash,
	})
	if err != nil {
		t.Fatalf("empty-runtime production removal: %v", err)
	}
	if removed.OperationPhase != string(agentcfg.SignedOAuthMCPPhaseRemoved) {
		t.Fatalf("empty-runtime removal phase = %q, want removed", removed.OperationPhase)
	}
	if _, ok := thirdCatalog.Resolve(connectionName + "_echo"); ok {
		t.Fatal("empty remover unexpectedly acquired a local catalog handle")
	}
	exchangeMu.Lock()
	beforeRemovedExchange := len(exchangeForms)
	exchangeMu.Unlock()
	beforeRemovedCredential := credentialRequests.Load()
	beforeRemovedDownstream := mcpRequests.Load()
	for runtimeName, descriptor := range map[string]tools.ToolDescriptor{"runtime A": runtimeATool, "runtime B": runtimeBTool} {
		if _, err := descriptor.Invoke(dispatchCtx, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("%s cached descriptor remained usable after durable removal", runtimeName)
		}
	}
	exchangeMu.Lock()
	afterRemovedExchange := len(exchangeForms)
	exchangeMu.Unlock()
	if afterRemovedExchange != beforeRemovedExchange || credentialRequests.Load() != beforeRemovedCredential || mcpRequests.Load() != beforeRemovedDownstream {
		t.Fatalf("removed cached descriptors reached credential/downstream planes: exchange %d->%d credential %d->%d downstream %d->%d",
			beforeRemovedExchange, afterRemovedExchange, beforeRemovedCredential, credentialRequests.Load(), beforeRemovedDownstream, mcpRequests.Load())
	}
	if err := secondReconciler.ReconcileSignedOAuthMCPCapability(dispatchCtx, quad, agentID); err != nil {
		t.Fatalf("runtime B terminal cleanup reconcile: %v", err)
	}
	if _, ok := secondCatalog.Resolve(connectionName + "_echo"); ok {
		t.Fatal("runtime B terminal reconcile left catalog dispatch visible")
	}
	if _, _, ok := secondMCPRegistry.RegistrationIdentity(connectionName); ok {
		t.Fatal("runtime B terminal reconcile left matching publisher handle registered")
	}
}

// TestRegisterUserOAuthMCPCapability_ProductionPathAuthenticatesPerUserAndIsolatesCatalog
// is the user-tier counterpart to the operator production-path test above.
// The downstream MCP endpoint refuses initialize/tools/list/resources/list
// without a bearer. Each user registration must therefore complete the real
// broker exchange with that user's identity, discover a non-empty catalog, and
// publish only into that user's owner-derived source namespace. The shared
// catalog is an internal materialization; the planner projection and every
// identity-aware registry read must hide the other user's source.
func TestRegisterUserOAuthMCPCapability_ProductionPathAuthenticatesPerUserAndIsolatesCatalog(t *testing.T) {
	t.Setenv("HARBOR_SIGNED_USER_PRODUCTION_KEK", "0404040404040404040404040404040404040404040404040404040404040404")
	t.Setenv("HARBOR_SIGNED_USER_PRODUCTION_BROKER_AUTH", "fixture-user-broker-auth")

	const (
		agentID      = "agent"
		logicalName  = "shared-user-connection"
		providerName = "shared-user-provider"
		bearerUserA  = "fixture-user-a-bearer"
		bearerUserB  = "fixture-user-b-bearer"
	)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	idA := identity.Identity{TenantID: "tenant", UserID: "user-a", SessionID: "session-a"}
	idB := identity.Identity{TenantID: "tenant", UserID: "user-b", SessionID: "session-b"}
	bearerByUser := map[string]string{idA.UserID: bearerUserA, idB.UserID: bearerUserB}

	catalog := tools.NewCatalog()
	mcpRegistry := mcpdrv.NewRegistry()
	var mcpMu sync.Mutex
	acceptedHeaders := make(map[string][]string)
	unauthorizedRequests := 0

	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "signed-user-production", Version: "v0"}, nil)
	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	})
	mcpServer.AddResource(&mcpsdk.Resource{URI: "mem://fixture", Name: "fixture", MIMEType: "text/plain"},
		func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "signed-user-resource"}}}, nil
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
		authorization := req.Header.Get("Authorization")
		allowed := authorization == "Bearer "+bearerUserA || authorization == "Bearer "+bearerUserB
		if !allowed {
			mcpMu.Lock()
			unauthorizedRequests++
			mcpMu.Unlock()
			http.Error(w, "bearer required", http.StatusUnauthorized)
			return
		}
		mcpMu.Lock()
		acceptedHeaders[message.Method] = append(acceptedHeaders[message.Method], authorization)
		mcpMu.Unlock()
		mcpHandler.ServeHTTP(w, req)
	})
	tlsMCP := httptest.NewTLSServer(authenticatedMCP)
	t.Cleanup(tlsMCP.Close)
	oldDefaultTransport := http.DefaultTransport
	http.DefaultTransport = tlsMCP.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldDefaultTransport })

	// Prove the fixture really has an authentication boundary before Harbor is
	// involved. The production attach path must never take this branch.
	unauthenticatedRequest, err := http.NewRequest(http.MethodPost, tlsMCP.URL+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
	if err != nil {
		t.Fatal(err)
	}
	unauthenticatedResponse, err := tlsMCP.Client().Do(unauthenticatedRequest)
	if err != nil {
		t.Fatalf("unauthenticated MCP request: %v", err)
	}
	_, _ = io.Copy(io.Discard, unauthenticatedResponse.Body)
	_ = unauthenticatedResponse.Body.Close()
	if unauthenticatedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated MCP status = %d, want %d", unauthenticatedResponse.StatusCode, http.StatusUnauthorized)
	}

	var exchangeMu sync.Mutex
	type exchangeObservation struct {
		subjectUser string
		actorUser   string
		bearer      string
	}
	var exchanges []exchangeObservation
	var exchangeError string
	decodeField := func(encoded string, out any) error {
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, out)
	}
	var credentialRequests atomic.Int64
	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)
	brokerMux.HandleFunc("/credential", func(w http.ResponseWriter, req *http.Request) {
		credentialRequests.Add(1)
		if req.Header.Get("Authorization") != "Bearer fixture-user-broker-auth" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"format_version": 1, "client_id": "fixture-user-client", "client_secret": "fixture-user-secret", "expires_in": 300,
		})
	})
	brokerMux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			http.Error(w, "parse form", http.StatusBadRequest)
			return
		}
		var subject struct {
			TenantID  string `json:"tenant_id"`
			UserID    string `json:"user_id"`
			SessionID string `json:"session_id"`
		}
		if err := decodeField(req.Form.Get("subject_token"), &subject); err != nil {
			exchangeMu.Lock()
			exchangeError = "subject token decode failed"
			exchangeMu.Unlock()
			http.Error(w, "invalid subject", http.StatusBadRequest)
			return
		}
		var actor struct {
			TenantID  string `json:"tenant_id"`
			UserID    string `json:"user_id"`
			SessionID string `json:"session_id"`
			AgentID   string `json:"agent_id"`
		}
		if err := decodeField(req.Form.Get("actor_token"), &actor); err != nil {
			exchangeMu.Lock()
			exchangeError = "actor token decode failed"
			exchangeMu.Unlock()
			http.Error(w, "invalid actor", http.StatusBadRequest)
			return
		}
		bearer, ok := bearerByUser[subject.UserID]
		if !ok || subject.TenantID != idA.TenantID || subject.SessionID == "" || actor.TenantID != subject.TenantID || actor.UserID != subject.UserID || actor.SessionID != subject.SessionID || actor.AgentID != agentID {
			exchangeMu.Lock()
			exchangeError = "exchange subject/actor mismatch"
			exchangeMu.Unlock()
			http.Error(w, "subject mismatch", http.StatusForbidden)
			return
		}
		exchangeMu.Lock()
		exchanges = append(exchanges, exchangeObservation{subjectUser: subject.UserID, actorUser: actor.UserID, bearer: bearer})
		exchangeMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": bearer, "token_type": "Bearer", "expires_in": 300,
			"scope": "read", "audience": "user-capability-audience", "resource": tlsMCP.URL,
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
	store, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "signed-user-production.sqlite")})
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
		OAuthTokenKEKEnv: "HARBOR_SIGNED_USER_PRODUCTION_KEK",
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker", TokenURL: brokerServer.URL + "/token", CredentialURL: brokerServer.URL + "/credential",
			AuthTokenEnv: "HARBOR_SIGNED_USER_PRODUCTION_BROKER_AUTH", ScopeCeiling: []string{"read"},
			AllowedDownstreamHosts: []string{"boot-ceiling.invalid"},
		}},
	}, toolauth.BuildDeps{State: store, Bus: bus, Redactor: redactor, Coordinator: pauseresume.New(pauseresume.WithBus(bus))})
	if err != nil {
		t.Fatal(err)
	}
	providerSet := toolauth.NewProviderSet(nil)
	attacher := NewMCPConnectionAttacher(catalog, mcpRegistry, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)), idA, nil, providerSet, nil)
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
	connection := agentcfg.SignedOAuthMCPConnectionDescriptor{Name: logicalName, URL: canonicalURL}
	makeRequest := func(id identity.Identity, jti string) prototypes.AgentConfigUserRegisterOAuthMCPCapabilityRequest {
		claims := agentcfg.SignedOAuthMCPAuthorityClaims{
			TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, AgentID: agentID,
			Broker: "broker", ProviderName: providerName, CapabilityRevision: "user-capability-revision",
			URLDigest: agentcfg.OAuthMCPURLDigest(canonicalURL), SinkDigest: agentcfg.OAuthMCPURLDigest(sink),
			Audience: "user-capability-audience", Scopes: []string{"read"}, Connection: connection,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: "issuer", ID: jti, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
			},
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "kid"
		envelope, err := token.SignedString(key)
		if err != nil {
			t.Fatalf("sign %s authority: %v", id.UserID, err)
		}
		return prototypes.AgentConfigUserRegisterOAuthMCPCapabilityRequest{
			Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
			AgentID:  agentID, ProviderName: providerName, Broker: "broker", Audience: claims.Audience,
			Scopes: []string{"read"}, Connection: prototypes.SignedOAuthMCPConnectionDescriptor{Name: logicalName, URL: canonicalURL},
			AuthorityEnvelope: envelope,
		}
	}
	userContext := func(id identity.Identity) context.Context {
		ctx, err := identity.WithVerified(context.Background(), id)
		if err != nil {
			t.Fatalf("verified %s: %v", id.UserID, err)
		}
		ctx = protocolauth.WithScopes(ctx, []protocolauth.Scope{protocolauth.ScopeAgentConfigUser})
		return protocolauth.WithAgentReach(ctx, []string{agentID})
	}
	authorities := map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
		"broker": {Broker: "broker", Issuer: "issuer", Keys: productionSignedCapabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: 10 * time.Minute},
	}
	service, err := agentcfgprotocol.NewService(registry,
		agentcfgprotocol.WithBus(bus), agentcfgprotocol.WithConnectionPreparer(attacher), agentcfgprotocol.WithConnectionDetacher(attacher),
		agentcfgprotocol.WithProviderInstaller(installer), agentcfgprotocol.WithSignedOAuthMCPOperationState(store),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(authorities), agentcfgprotocol.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterUserOAuthMCPCapability(userContext(idA), makeRequest(idA, "jti-user-a")); err != nil {
		t.Fatalf("register user A: %v", err)
	}
	if _, err := service.RegisterUserOAuthMCPCapability(userContext(idB), makeRequest(idB, "jti-user-b")); err != nil {
		t.Fatalf("register user B: %v", err)
	}

	ownerA := toolauth.Owner{Tenant: idA.TenantID, Agent: agentID, User: idA.UserID}
	ownerB := toolauth.Owner{Tenant: idB.TenantID, Agent: agentID, User: idB.UserID}
	physicalA := mcpdrv.PhysicalServerName(logicalName, ownerA)
	physicalB := mcpdrv.PhysicalServerName(logicalName, ownerB)
	if physicalA == physicalB || physicalA == logicalName || physicalB == logicalName {
		t.Fatalf("user source namespace collapsed: A=%q B=%q", physicalA, physicalB)
	}
	if _, ok := catalog.Resolve(physicalA + "_echo"); !ok {
		t.Fatal("user A discovered tool was not published")
	}
	if _, ok := catalog.Resolve(physicalB + "_echo"); !ok {
		t.Fatal("user B discovered tool was not published")
	}
	if _, ok := catalog.Resolve(logicalName + "_echo"); ok {
		t.Fatal("logical user source leaked as a shared catalog key")
	}

	ctxA := userContext(idA)
	ctxB := userContext(idB)
	viewsA, _, err := mcpRegistry.ListServers(ctxA, mcpdrv.ListFilter{})
	if err != nil || len(viewsA) != 1 || viewsA[0].Name != physicalA {
		t.Fatalf("user A MCP view = %+v err=%v, want only %q", viewsA, err, physicalA)
	}
	viewsB, _, err := mcpRegistry.ListServers(ctxB, mcpdrv.ListFilter{})
	if err != nil || len(viewsB) != 1 || viewsB[0].Name != physicalB {
		t.Fatalf("user B MCP view = %+v err=%v, want only %q", viewsB, err, physicalB)
	}
	if _, err := mcpRegistry.GetServer(ctxB, physicalA); !stderrors.Is(err, mcpdrv.ErrServerNotFound) {
		t.Fatalf("user B get of user A source = %v, want ErrServerNotFound", err)
	}
	if _, err := mcpRegistry.ListResources(ctxB, physicalA); !stderrors.Is(err, mcpdrv.ErrServerNotFound) {
		t.Fatalf("user B resources of user A source = %v, want ErrServerNotFound", err)
	}
	if _, _, err := mcpRegistry.ReadResource(ctxB, physicalA, "mem://fixture"); !stderrors.Is(err, mcpdrv.ErrServerNotFound) {
		t.Fatalf("user B read of user A source = %v, want ErrServerNotFound", err)
	}

	viewA, err := projection.ActivePlannerCatalogView(ctxA, registry, nil, agentID, identity.Quadruple{Identity: idA}, catalog, tools.CatalogFilter{}, mcpRegistry)
	if err != nil {
		t.Fatalf("user A planner projection: %v", err)
	}
	viewB, err := projection.ActivePlannerCatalogView(ctxB, registry, nil, agentID, identity.Quadruple{Identity: idB}, catalog, tools.CatalogFilter{}, mcpRegistry)
	if err != nil {
		t.Fatalf("user B planner projection: %v", err)
	}
	if _, ok := viewA.Resolve(physicalA + "_echo"); !ok {
		t.Fatal("user A planner view omitted its own tool")
	}
	if _, ok := viewA.Resolve(physicalB + "_echo"); ok {
		t.Fatal("user A planner view exposed user B's tool")
	}
	if _, ok := viewB.Resolve(physicalB + "_echo"); !ok {
		t.Fatal("user B planner view omitted its own tool")
	}
	if _, ok := viewB.Resolve(physicalA + "_echo"); ok {
		t.Fatal("user B planner view exposed user A's tool")
	}

	mcpMu.Lock()
	unauthorized := unauthorizedRequests
	accepted := make(map[string][]string, len(acceptedHeaders))
	for method, headers := range acceptedHeaders {
		accepted[method] = append([]string(nil), headers...)
	}
	mcpMu.Unlock()
	if unauthorized != 1 {
		t.Fatalf("unauthorized MCP requests = %d, want exactly the direct no-bearer probe", unauthorized)
	}
	for _, method := range []string{"initialize", "tools/list", "resources/list"} {
		headers := accepted[method]
		if len(headers) < 2 {
			t.Fatalf("authenticated %s calls = %v, want both users", method, headers)
		}
		seenA, seenB := false, false
		for _, header := range headers {
			switch header {
			case "Bearer " + bearerUserA:
				seenA = true
			case "Bearer " + bearerUserB:
				seenB = true
			default:
				t.Fatalf("%s authorization = %q, want one of the two user bearers", method, header)
			}
		}
		if !seenA || !seenB {
			t.Fatalf("%s did not carry distinct user bearers: %v", method, headers)
		}
	}
	exchangeMu.Lock()
	observedExchanges := append([]exchangeObservation(nil), exchanges...)
	exchangeFailure := exchangeError
	exchangeMu.Unlock()
	if exchangeFailure != "" {
		t.Fatal(exchangeFailure)
	}
	seenExchangeA, seenExchangeB := false, false
	for _, exchange := range observedExchanges {
		if exchange.subjectUser != exchange.actorUser || exchange.bearer != bearerByUser[exchange.subjectUser] {
			t.Fatalf("broker exchange crossed identity boundary: %+v", exchange)
		}
		seenExchangeA = seenExchangeA || exchange.subjectUser == idA.UserID
		seenExchangeB = seenExchangeB || exchange.subjectUser == idB.UserID
	}
	if !seenExchangeA || !seenExchangeB {
		t.Fatalf("broker exchanges = %+v, want one for each user", observedExchanges)
	}
	if credentialRequests.Load() == 0 {
		t.Fatal("broker credential source was never called")
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
