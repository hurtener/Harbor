package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

func TestNewStreamableTransportStandaloneSSEPolicy(t *testing.T) {
	provider := &stubOAuthProvider{token: "identity-token"}
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "connection oauth", cfg: Config{URL: "https://mcp.example.test", OAuthProvider: provider}, want: true},
		{name: "pair private owned oauth", cfg: Config{URL: "https://mcp.example.test", OAuthProvider: provider, OwnOAuthProvider: true}, want: true},
		{name: "per entry oauth", cfg: Config{URL: "https://mcp.example.test", ToolOAuthProviders: map[string]auth.OAuthProvider{"echo": provider}}, want: true},
		{name: "unbound", cfg: Config{URL: "https://mcp.example.test"}, want: false},
		{name: "static header", cfg: Config{URL: "https://mcp.example.test", Headers: map[string]string{"Authorization": "Bearer static"}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport, ok := newStreamableTransport(tt.cfg).(*mcpsdk.StreamableClientTransport)
			if !ok {
				t.Fatalf("transport type = %T, want *mcp.StreamableClientTransport", newStreamableTransport(tt.cfg))
			}
			if transport.DisableStandaloneSSE != tt.want {
				t.Fatalf("DisableStandaloneSSE = %v, want %v", transport.DisableStandaloneSSE, tt.want)
			}
		})
	}
}

// rejectingStandaloneSSEServer mirrors the ordinary shared/per-entry Soundings
// boundary relevant to this regression: initialize and discovery are public,
// protected calls need the identity-scoped bearer, and a bearerless
// connection-level GET is denied. The regression asserts that unsafe GET is
// absent; it does not depend on go-sdk surfacing the GET's non-SSE 401 as a
// Connect failure. DELETE is denied by the same rule and recorded separately so
// teardown behavior is observable without weakening the resource server.
type rejectingStandaloneSSEServer struct {
	inner http.Handler

	mu          sync.Mutex
	getCount    int
	deleteCount int
	toolCalls   int
	publicRPCs  int
	publicAuth  int
}

func (s *rejectingStandaloneSSEServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		s.getCount++
		s.mu.Unlock()
		http.Error(w, "bearer required", http.StatusUnauthorized)
		return
	case http.MethodDelete:
		s.mu.Lock()
		s.deleteCount++
		s.mu.Unlock()
		http.Error(w, "bearer required", http.StatusUnauthorized)
		return
	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		r.ContentLength = int64(len(body))
		var envelope struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &envelope); err == nil {
			switch envelope.Method {
			case "initialize", "notifications/initialized", "tools/list":
				s.mu.Lock()
				s.publicRPCs++
				if r.Header.Get("Authorization") != "" {
					s.publicAuth++
				}
				s.mu.Unlock()
			case "tools/call":
				s.mu.Lock()
				s.toolCalls++
				s.mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer brokered-tenant-a-user-a" {
					http.Error(w, "bearer required", http.StatusUnauthorized)
					return
				}
			}
		}
	}
	s.inner.ServeHTTP(w, r)
}

func (s *rejectingStandaloneSSEServer) counts() (gets, deletes, calls, publicRPCs, publicAuth int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCount, s.deleteCount, s.toolCalls, s.publicRPCs, s.publicAuth
}

func TestStreamableOAuth_PublicInitializeProtectedCall_NoStandaloneSSE(t *testing.T) {
	tests := []struct {
		name string
		bind func(*Config)
	}{
		{name: "connection oauth", bind: func(cfg *Config) { cfg.OAuthProvider = identityBearerProvider{} }},
		{name: "per entry oauth", bind: func(cfg *Config) {
			cfg.ToolOAuthProviders = map[string]auth.OAuthProvider{"echo": identityBearerProvider{}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sdkServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "oauth-sse-regression", Version: "v0"}, nil)
			mcpsdk.AddTool(sdkServer, &mcpsdk.Tool{Name: "echo", Description: "echo"},
				func(_ context.Context, _ *mcpsdk.CallToolRequest, in struct {
					Text string `json:"text"`
				}) (*mcpsdk.CallToolResult, any, error) {
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Text}}}, nil, nil
				})
			front := &rejectingStandaloneSSEServer{inner: mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return sdkServer }, nil)}
			httpServer := httptest.NewServer(front)
			t.Cleanup(httpServer.Close)

			cfg := Config{
				Name:            "protected",
				TransportMode:   TransportStreamableHTTP,
				URL:             httpServer.URL,
				Bus:             mkScopeTestBus(t),
				DefaultIdentity: identity.Identity{TenantID: "system", UserID: "system", SessionID: "system"},
			}
			tt.bind(&cfg)
			provider, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := provider.Connect(context.Background()); err != nil {
				t.Fatalf("public initialize/discovery failed: %v", err)
			}
			descriptors, err := provider.Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			var invoke func(context.Context, json.RawMessage) (tools.ToolResult, error)
			for _, descriptor := range descriptors {
				if descriptor.Tool.Name == "protected_echo" {
					invoke = descriptor.Invoke
					break
				}
			}
			if invoke == nil {
				t.Fatal("protected_echo was not discovered")
			}
			callCtx, err := identity.With(context.Background(), identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"})
			if err != nil {
				t.Fatalf("identity.With: %v", err)
			}
			if _, err := invoke(callCtx, json.RawMessage(`{"text":"ok"}`)); err != nil {
				t.Fatalf("protected tool invocation: %v", err)
			}
			gets, _, calls, publicRPCs, publicAuth := front.counts()
			if gets != 0 {
				t.Fatalf("standalone SSE GET count = %d, want 0 for identity-scoped OAuth", gets)
			}
			if calls != 1 {
				t.Fatalf("protected tools/call count = %d, want 1", calls)
			}
			if publicRPCs < 2 || publicAuth != 0 {
				t.Fatalf("public RPCs = %d, authenticated public RPCs = %d; want initialize/discovery public", publicRPCs, publicAuth)
			}

			// The SDK still issues a connection-level DELETE during close.
			// Soundings correctly rejects that bearerless request; the SDK treats the
			// rejection as completed teardown and does not fail the successful RPC.
			if err := provider.Close(context.Background()); err != nil {
				t.Fatalf("Close after rejected bearerless DELETE: %v", err)
			}
			_, deletes, _, _, _ := front.counts()
			if deletes != 1 {
				t.Fatalf("bearerless close DELETE count = %d, want 1", deletes)
			}
		})
	}
}
