package serve

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestSignedOAuthMCPReconcile_AgentPairPreparesAsOwnerUnderEndUserRun drives
// the production attacher, provider builder and token-exchange driver against
// a broker that, like a governing platform, refuses an ordinary mint whose
// subject differs from the signed pair's actor. A run-start reconcile of an
// agent-tier pair triggered by an END USER's run must prepare the private
// connection as the frozen pair owner (subject == actor), so the attach
// succeeds. A live planner call by that end user must still exchange as the
// end user, receive the governed refusal, and retry with a bound operation.
func TestSignedOAuthMCPReconcile_AgentPairPreparesAsOwnerUnderEndUserRun(t *testing.T) {
	t.Setenv("HARBOR_SIGNED_OWNER_KEK", "0303030303030303030303030303030303030303030303030303030303030303")
	t.Setenv("HARBOR_SIGNED_OWNER_BROKER_AUTH", "fixture-owner-broker-auth")
	const (
		providerName   = "owner-provider"
		connectionName = "owner-signed"
		agentID        = "agent"
		bearer         = "fixture-owner-bearer"
	)
	now := time.Now().UTC()
	owner := identity.Identity{TenantID: "tenant", UserID: "pair-owner", SessionID: "owner-session"}
	endUser := identity.Identity{TenantID: "tenant", UserID: "end-user", SessionID: "end-session"}
	physicalName := mcpdrv.PhysicalServerName(connectionName, toolauth.Owner{Tenant: owner.TenantID, Agent: agentID})

	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "owner-fixture", Version: "v0"}, nil)
	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}, func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	})
	mcpHandler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mcpServer }, nil)
	tlsMCP := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/mcp" || req.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, req)
	}))
	t.Cleanup(tlsMCP.Close)
	oldDefaultTransport := http.DefaultTransport
	http.DefaultTransport = tlsMCP.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldDefaultTransport })

	type subjectClaims struct {
		TenantID  string `json:"tenant_id"`
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
	}
	type exchange struct {
		subject   subjectClaims
		actor     subjectClaims
		operation string
		refused   bool
	}
	var (
		exchangeMu sync.Mutex
		exchanges  []exchange
	)
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
		var ex exchange
		if !decodeOwnerExchangeField(form.Get("subject_token"), &ex.subject) || !decodeOwnerExchangeField(form.Get("actor_token"), &ex.actor) {
			http.Error(w, "invalid subject or actor", http.StatusBadRequest)
			return
		}
		ex.operation = form.Get("app_operation")
		response := map[string]any{"access_token": bearer, "token_type": "Bearer", "expires_in": 300,
			"scope": "read", "audience": "owner-audience", "resource": tlsMCP.URL}
		switch {
		case ex.operation != "":
			sum := sha256.Sum256([]byte(ex.operation))
			response["expires_in"], response["app_operation_sha256"] = 30, hex.EncodeToString(sum[:])
			delete(response, "scope")
		case ex.subject != ex.actor:
			ex.refused = true
		}
		exchangeMu.Lock()
		exchanges = append(exchanges, ex)
		exchangeMu.Unlock()
		if ex.refused {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"access_denied","reason":"governed_app_operation_required"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	})
	snapshot := func() []exchange {
		exchangeMu.Lock()
		defer exchangeMu.Unlock()
		return append([]exchange(nil), exchanges...)
	}

	redactor := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, redactor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	dsn := filepath.Join(t.TempDir(), "signed-owner-identity.sqlite")
	builderConfig := config.ToolsConfig{
		OAuthTokenKEKEnv: "HARBOR_SIGNED_OWNER_KEK",
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker", TokenURL: brokerServer.URL + "/token", CredentialURL: brokerServer.URL + "/credential",
			AuthTokenEnv: "HARBOR_SIGNED_OWNER_BROKER_AUTH", ScopeCeiling: []string{"read"},
			AllowedDownstreamHosts: []string{"boot-ceiling.invalid"},
		}},
	}
	type runtimeGraph struct {
		registry   agentcfg.Registry
		store      state.StateStore
		catalog    tools.ToolCatalog
		attacher   *MCPConnectionAttacher
		installer  *OAuthProviderInstaller
		reconciler *agentcfgprotocol.SignedOAuthMCPReconciler
	}
	openRuntime := func() runtimeGraph {
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
		builder, err := toolauth.NewProviderBuilder(context.Background(), builderConfig, toolauth.BuildDeps{
			State: store, Bus: bus, Redactor: redactor, Coordinator: pauseresume.New(pauseresume.WithBus(bus)),
		})
		if err != nil {
			t.Fatal(err)
		}
		catalog := tools.NewCatalog()
		providerSet := toolauth.NewProviderSet(nil)
		attacher := NewMCPConnectionAttacher(catalog, mcpdrv.NewRegistry(), bus,
			slog.New(slog.NewTextHandler(io.Discard, nil)), owner, nil, providerSet, nil)
		t.Cleanup(func() { _ = attacher.Close(context.Background()) })
		installer := NewOAuthProviderInstaller(builder, providerSet, false, nil)
		reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(registry, store, attacher, attacher, installer)
		if err != nil {
			t.Fatal(err)
		}
		return runtimeGraph{registry: registry, store: store, catalog: catalog, attacher: attacher, installer: installer, reconciler: reconciler}
	}

	// Runtime A: the owner registers the agent-tier pair (the initial push).
	first := openRuntime()
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
		TenantID: owner.TenantID, UserID: owner.UserID, SessionID: owner.SessionID, AgentID: agentID,
		Broker: "broker", ProviderName: providerName, CapabilityRevision: "capability-revision-1",
		URLDigest: agentcfg.OAuthMCPURLDigest(canonicalURL), SinkDigest: agentcfg.OAuthMCPURLDigest(sink),
		Audience: "owner-audience", Scopes: []string{"read"}, Connection: connection,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "issuer", ID: "jti-owner", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "kid"
	envelope, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentcfgprotocol.NewService(first.registry,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionPreparer(first.attacher),
		agentcfgprotocol.WithConnectionDetacher(first.attacher),
		agentcfgprotocol.WithProviderInstaller(first.installer),
		agentcfgprotocol.WithSignedOAuthMCPOperationState(first.store),
		agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			"broker": {Broker: "broker", Issuer: "issuer", Keys: productionSignedCapabilityKeySet{key: &key.PublicKey},
				ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: 10 * time.Minute},
		}),
		agentcfgprotocol.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx, err := identity.WithVerified(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterOAuthMCPCapability(ownerCtx, prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest{
		Identity: prototypes.IdentityScope{Tenant: owner.TenantID, User: owner.UserID, Session: owner.SessionID},
		AgentID:  agentID, ProviderName: providerName, Broker: "broker", Audience: claims.Audience, Scopes: []string{"read"},
		Connection:        prototypes.SignedOAuthMCPConnectionDescriptor{Name: connectionName, URL: canonicalURL},
		AuthorityEnvelope: envelope,
	}); err != nil {
		t.Fatalf("owner registration: %v", err)
	}
	registered := len(snapshot())

	// Runtime B has no local handle. Its run-start reconcile runs in an END
	// USER's run context, exactly as the run loop builds it.
	second := openRuntime()
	endUserQuad := identity.Quadruple{Identity: endUser, RunID: "end-user-run"}
	runCtx, err := identity.WithVerified(context.Background(), endUser)
	if err != nil {
		t.Fatal(err)
	}
	if runCtx, err = identity.WithRun(runCtx, endUser, endUserQuad.RunID); err != nil {
		t.Fatal(err)
	}
	if err := second.reconciler.ReconcileSignedOAuthMCPCapability(runCtx, endUserQuad, agentID); err != nil {
		t.Fatalf("agent-tier reconcile under end-user run: %v", err)
	}
	preparation := snapshot()[registered:]
	if len(preparation) == 0 {
		t.Fatal("reconcile made no preparation exchange")
	}
	for _, ex := range preparation {
		if ex.refused || ex.operation != "" || ex.subject.UserID != owner.UserID || ex.subject.SessionID != owner.SessionID || ex.subject != ex.actor {
			t.Fatalf("preparation exchange = %+v, want subject == frozen owner actor", ex)
		}
	}
	tool, ok := second.catalog.Resolve(physicalName + "_echo")
	if !ok {
		t.Fatal("agent-tier pair did not attach under the end-user run")
	}

	// A live planner call keeps the end user's identity: the ordinary mint is
	// refused as governed and the trusted invocation retries bound.
	beforeLive := len(snapshot())
	callCtx := appoperation.WithAgentInvocation(tools.WithEffectiveAgentConfig(runCtx, agentID), agentID)
	if _, err := tool.Invoke(callCtx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("live end-user call: %v", err)
	}
	live := snapshot()[beforeLive:]
	if len(live) != 2 || !live[0].refused || live[0].operation != "" || live[1].refused || live[1].operation == "" {
		t.Fatalf("live exchanges = %+v, want governed refusal then bound operation", live)
	}
	for _, ex := range live {
		if ex.subject.UserID != endUser.UserID || ex.subject.SessionID != endUser.SessionID {
			t.Fatalf("live exchange subject = %+v, want the end user", ex.subject)
		}
	}
}

func decodeOwnerExchangeField(encoded string, out any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && json.Unmarshal(raw, out) == nil
}
