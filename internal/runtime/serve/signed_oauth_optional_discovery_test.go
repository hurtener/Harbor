package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/remote"
	_ "github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

type allowSignedCapabilityUse struct{}

func (allowSignedCapabilityUse) AuthorizeSignedCapabilityUse(context.Context, string, string, string, bool) error {
	return nil
}

type signedDiscoveryFault string

const (
	signedDiscoveryResourcesUnauthorized signedDiscoveryFault = "resources-unauthorized"
	signedDiscoveryPromptsTransport      signedDiscoveryFault = "prompts-transport"
	signedDiscoveryOptionalAbsent        signedDiscoveryFault = "optional-absent"
)

func TestMCPConnectionAttacher_SignedPrivateOptionalDiscoveryErrors(t *testing.T) {
	t.Setenv("HARBOR_SIGNED_DISCOVERY_KEK", "0303030303030303030303030303030303030303030303030303030303030303")
	t.Setenv("HARBOR_SIGNED_DISCOVERY_BROKER_AUTH", "fixture-broker-auth")

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

	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)
	brokerMux.HandleFunc("/credential", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Connection", "close")
		if req.Header.Get("Authorization") != "Bearer fixture-broker-auth" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"format_version": 1, "client_id": "fixture-client", "client_secret": "fixture-secret", "expires_in": 300,
		})
	})
	brokerMux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Connection", "close")
		if err := req.ParseForm(); err != nil {
			http.Error(w, "parse form", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fixture-discovery-bearer", "token_type": "Bearer", "expires_in": 300,
			"scope": "read", "audience": req.Form.Get("audience"), "resource": req.Form.Get("resource"),
		})
	})
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{
		OAuthTokenKEKEnv: "HARBOR_SIGNED_DISCOVERY_KEK",
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker", TokenURL: brokerServer.URL + "/token", CredentialURL: brokerServer.URL + "/credential",
			AuthTokenEnv: "HARBOR_SIGNED_DISCOVERY_BROKER_AUTH", ScopeCeiling: []string{"read"},
			AllowedDownstreamHosts: []string{"boot-ceiling.invalid"},
		}},
	}, toolauth.BuildDeps{
		State: store, Bus: bus, Redactor: redactor, Coordinator: pauseresume.New(pauseresume.WithBus(bus)),
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name             string
		fault            signedDiscoveryFault
		wantErr          bool
		wantAuthRequired bool
		wantMethods      []string
		forbiddenMethods []string
	}{
		{
			name: "resources 401 fails preparation", fault: signedDiscoveryResourcesUnauthorized, wantErr: true, wantAuthRequired: true,
			wantMethods: []string{"initialize", "tools/list", "resources/list"}, forbiddenMethods: []string{"prompts/list"},
		},
		{
			name: "prompts transport failure fails preparation", fault: signedDiscoveryPromptsTransport, wantErr: true,
			wantMethods: []string{"initialize", "tools/list", "resources/list", "prompts/list"},
		},
		{
			name: "canonical method not found remains optional", fault: signedDiscoveryOptionalAbsent,
			wantMethods: []string{"initialize", "tools/list", "resources/list", "prompts/list"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baselineGoroutines := runtime.NumGoroutine()
			const (
				bearer = "Bearer fixture-discovery-bearer"
				agent  = "agent"
			)
			id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}
			catalog := tools.NewCatalog()
			registry := mcpdrv.NewRegistry()
			providerSet := toolauth.NewProviderSet(nil)
			connectionName := "signed-" + string(tc.fault)
			fingerprint := "fingerprint-" + string(tc.fault)

			mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: connectionName, Version: "v0"}, nil)
			mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
				Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object", "additionalProperties": false},
			}, func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, any, error) {
				return &mcpsdk.CallToolResult{}, nil, nil
			})
			mcpServer.AddResource(&mcpsdk.Resource{URI: "mem://fixture", Name: "fixture", MIMEType: "text/plain"},
				func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
					return &mcpsdk.ReadResourceResult{}, nil
				})
			mcpServer.AddPrompt(&mcpsdk.Prompt{Name: "fixture"},
				func(context.Context, *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
					return &mcpsdk.GetPromptResult{}, nil
				})
			inner := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mcpServer }, nil)
			var requestMu sync.Mutex
			seen := make(map[string][]string)
			front := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, readErr := io.ReadAll(req.Body)
				if readErr != nil {
					http.Error(w, "read request", http.StatusBadRequest)
					return
				}
				_ = req.Body.Close()
				req.Body = io.NopCloser(bytes.NewReader(body))
				var message struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				_ = json.Unmarshal(body, &message)
				requestMu.Lock()
				seen[message.Method] = append(seen[message.Method], req.Header.Get("Authorization"))
				requestMu.Unlock()
				if req.Header.Get("Authorization") != bearer {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				switch {
				case tc.fault == signedDiscoveryResourcesUnauthorized && message.Method == "resources/list":
					w.Header().Set("WWW-Authenticate", `Bearer realm="fixture"`)
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				case tc.fault == signedDiscoveryPromptsTransport && message.Method == "prompts/list":
					http.Error(w, "upstream unavailable", http.StatusBadGateway)
					return
				case tc.fault == signedDiscoveryOptionalAbsent && (message.Method == "resources/list" || message.Method == "prompts/list"):
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0", "id": message.ID,
						"error": map[string]any{"code": -32601, "message": "method not found"},
					})
					return
				}
				inner.ServeHTTP(w, req)
			})
			tlsMCP := httptest.NewTLSServer(front)
			oldDefaultTransport := http.DefaultTransport
			http.DefaultTransport = tlsMCP.Client().Transport
			t.Cleanup(func() {
				http.DefaultTransport = oldDefaultTransport
				tlsMCP.Client().CloseIdleConnections()
				tlsMCP.CloseClientConnections()
				tlsMCP.Close()
			})

			canonicalURL, sink, err := agentcfg.CanonicalOAuthMCPURL(tlsMCP.URL + "/mcp")
			if err != nil {
				t.Fatal(err)
			}
			binding := toolauth.SignedCapabilityExchangeBinding{
				TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, AgentID: agent,
				ProviderName: connectionName + "-provider", CapabilityRevision: "revision", PairFingerprint: fingerprint,
				URLDigest: agentcfg.OAuthMCPURLDigest(canonicalURL), SinkDigest: agentcfg.OAuthMCPURLDigest(sink),
				Audience: "audience", Resource: sink,
				AuthorityOperationKind: "operation", PublisherEpoch: "publisher", UseAuthorizer: allowSignedCapabilityUse{},
			}
			provider, err := builder.BuildSignedCapability(context.Background(), "broker", binding, []string{"read"})
			if err != nil {
				t.Fatal(err)
			}
			attacher := NewMCPConnectionAttacher(catalog, registry, bus,
				slog.New(slog.NewTextHandler(io.Discard, nil)), id, nil, providerSet, nil)
			ctx, err := identity.WithVerified(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			ctx = tools.WithInvokingAgent(ctx, agent)
			prepared, prepareErr := attacher.PrepareConnection(ctx, agentcfgprotocol.AttachRequest{
				Identity: id, AgentID: agent, Name: connectionName, Transport: agentcfg.MCPTransportHTTP,
				URL: canonicalURL, OAuthProvider: binding.ProviderName, OAuthProviderOverride: provider,
				OwnOAuthProvider: true, DescriptorFingerprint: fingerprint,
			})
			if tc.wantErr {
				if prepareErr == nil {
					t.Fatal("optional discovery failure unexpectedly prepared a connection")
				}
				if !errors.Is(prepareErr, mcpdrv.ErrTransportFailed) {
					t.Fatalf("optional discovery failure lost transport classification: %v", prepareErr)
				}
				if tc.wantAuthRequired && !errors.Is(prepareErr, agentcfgprotocol.ErrAuthRequired) {
					t.Fatalf("resources 401 lost auth-required classification: %v", prepareErr)
				}
				if prepared != nil {
					t.Fatal("failed preparation returned a prepared connection")
				}
				assertSignedDiscoveryUnpublished(t, catalog, registry, providerSet, connectionName, binding.ProviderName)
				if _, err := provider.Token(ctx, tools.ToolSourceID(binding.ProviderName)); !errors.Is(err, toolauth.ErrProviderClosed) {
					t.Fatalf("failed preparation left private token cache/worker usable: %v", err)
				}
			} else {
				if prepareErr != nil {
					t.Fatalf("method-not-found preparation: %v", prepareErr)
				}
				assertSignedDiscoveryUnpublished(t, catalog, registry, providerSet, connectionName, binding.ProviderName)
				if err := prepared.Activate(ctx); err != nil {
					t.Fatalf("activate method-not-found preparation: %v", err)
				}
				if _, ok := catalog.Resolve(connectionName + "_echo"); !ok {
					t.Fatal("method-not-found preparation did not publish tool after activation")
				}
				if _, _, ok := registry.RegistrationIdentity(connectionName); !ok {
					t.Fatal("method-not-found preparation did not publish registry entry after activation")
				}
				if err := attacher.DetachExactConnection(ctx, id.TenantID, agent, connectionName, fingerprint); err != nil {
					t.Fatalf("exact detach: %v", err)
				}
				if _, err := provider.Token(ctx, tools.ToolSourceID(binding.ProviderName)); !errors.Is(err, toolauth.ErrProviderClosed) {
					t.Fatalf("exact detach left private token cache/worker usable: %v", err)
				}
			}
			if err := attacher.Close(context.Background()); err != nil {
				t.Fatalf("attacher close: %v", err)
			}

			requestMu.Lock()
			seenSnapshot := make(map[string][]string, len(seen))
			for method, headers := range seen {
				seenSnapshot[method] = append([]string(nil), headers...)
			}
			requestMu.Unlock()
			for method, headers := range seenSnapshot {
				for _, header := range headers {
					if header != bearer {
						t.Fatalf("%s authorization = %q, want exact private bearer", method, header)
					}
				}
			}
			for _, method := range tc.wantMethods {
				if len(seenSnapshot[method]) == 0 {
					t.Fatalf("required signed discovery method %s was not called", method)
				}
			}
			for _, method := range tc.forbiddenMethods {
				if len(seenSnapshot[method]) != 0 {
					t.Fatalf("discovery continued to %s after the injected failure", method)
				}
			}

			// Tear down the fixture and idle HTTP transport before sampling the
			// process-global goroutine count. Cleanup remains registered for fatal
			// paths; both operations are safe to repeat.
			http.DefaultTransport = oldDefaultTransport
			tlsMCP.Client().CloseIdleConnections()
			tlsMCP.CloseClientConnections()
			tlsMCP.Close()
			assertNoPersistentGoroutineGrowth(t, baselineGoroutines)
		})
	}
}

func assertSignedDiscoveryUnpublished(t *testing.T, catalog tools.ToolCatalog, registry *mcpdrv.Registry, providerSet toolauth.ProviderSet, connectionName, providerName string) {
	t.Helper()
	if got := catalog.List(tools.CatalogFilter{}); len(got) != 0 {
		t.Fatalf("failed/private preparation leaked %d tool(s) into the catalog", len(got))
	}
	if _, _, ok := registry.RegistrationIdentity(connectionName); ok {
		t.Fatal("failed/private preparation leaked into the MCP registry")
	}
	if _, ok := providerSet.Get(providerName); ok {
		t.Fatal("pair-private provider leaked into the shared ProviderSet")
	}
}

func assertNoPersistentGoroutineGrowth(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("private preparation leaked workers: baseline=%d current=%d", baseline, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
